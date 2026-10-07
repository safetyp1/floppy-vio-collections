package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"time"
)

type Service struct {
	Lookup func(context.Context, string, string, []Entry) ([]CatalogEntry, error)
	Silo   *API
	Floppy *API
	State  *State
	DryRun bool
	Radarr *Arr
	Sonarr *Arr
	Vio    *Vio
	Output io.Writer
}

func digest(value any) string {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func sameLibraries(first, second []string) bool {
	first, second = slices.Clone(first), slices.Clone(second)
	slices.Sort(first)
	slices.Sort(second)
	return slices.Equal(first, second)
}

func (service *Service) cachedDetail(ctx context.Context, itemID, libraryID string) (CatalogEntry, error) {
	key := "detail:" + digest([]string{service.Silo.Base, service.Silo.Profile, libraryID, itemID})
	var cache CachedDetail
	found, err := service.State.Get(key, &cache)
	if err != nil {
		return CatalogEntry{}, err
	}
	if found && time.Since(cache.Fetched) < 6*time.Hour && (cache.Item.TMDB != "" || cache.Item.IMDB != "" || cache.Item.TVDB != "") {
		return cache.Item, nil
	}
	item, err := service.Silo.Detail(ctx, itemID, libraryID)
	if err != nil {
		return item, err
	}
	if item.ID != itemID {
		return item, errors.New("catalog detail identity mismatch")
	}
	if err := service.State.Put(key, CachedDetail{item, time.Now().UTC()}); err != nil {
		return item, err
	}
	return item, nil
}

func validateCollection(collection Collection, binding Binding) error {
	if collection.ID == "" || collection.CollectionType != "manual" || collection.ManagementMode != "manual" || !sameLibraries(collection.LibraryIDs, binding.LibraryIDs) {
		return errors.New("collection ownership/type/library configuration changed; refusing mutations")
	}
	return nil
}

func (service *Service) ensureCollection(ctx context.Context, binding Binding, saved *BindingState) error {
	if saved.CollectionID != "" {
		collection, err := service.Silo.Collection(ctx, saved.CollectionID)
		if err != nil {
			return err
		}
		if collection.ID != saved.CollectionID {
			return errors.New("collection identity mismatch")
		}
		return validateCollection(collection, binding)
	}
	if binding.CollectionID != "" {
		collection, err := service.Silo.Collection(ctx, binding.CollectionID)
		if err != nil {
			return err
		}
		if collection.ID != binding.CollectionID {
			return errors.New("collection identity mismatch")
		}
		if err := validateCollection(collection, binding); err != nil {
			return err
		}
		saved.CollectionID = binding.CollectionID
		if !service.DryRun {
			return service.State.Put("binding:"+binding.Key, saved)
		}
		return nil
	}
	slug := "floppy-sync-" + saved.Fingerprint[:20]
	var response struct {
		Items []Collection `json:"items"`
	}
	_, err := service.Silo.Request(ctx, "GET", "/api/v2/admin/collections?library_id="+url.QueryEscape(binding.LibraryIDs[0]), nil, &response, "")
	if err != nil {
		return err
	}
	if response.Items == nil {
		return errors.New("incomplete collection listing")
	}
	matches := []Collection{}
	for _, collection := range response.Items {
		if collection.Slug == slug {
			matches = append(matches, collection)
		}
	}
	if len(matches) > 0 {
		if !saved.Creating || len(matches) != 1 {
			return errors.New("collection slug already exists; explicit adoption required")
		}
		if err := validateCollection(matches[0], binding); err != nil {
			return err
		}
		saved.CollectionID, saved.Creating = matches[0].ID, false
		if !service.DryRun {
			return service.State.Put("binding:"+binding.Key, saved)
		}
		return nil
	}
	if saved.Creating {
		return errors.New("previous creation outcome uncertain; inspect before retrying or explicitly map collection_id")
	}
	if service.DryRun {
		return nil
	}
	saved.Creating = true
	if err := service.State.Put("binding:"+binding.Key, saved); err != nil {
		return err
	}
	var collection Collection
	_, err = service.Silo.Request(ctx, "POST", "/api/v2/admin/collections", map[string]any{
		"library_ids": binding.LibraryIDs, "title": binding.Title, "slug": slug, "collection_type": "manual", "management_mode": "manual",
	}, &collection, "")
	if err != nil {
		return err
	}
	if err := validateCollection(collection, binding); err != nil {
		return err
	}
	saved.CollectionID, saved.Creating = collection.ID, false
	return service.State.Put("binding:"+binding.Key, saved)
}

func (service *Service) Sync(ctx context.Context, binding Binding) (result Changes, syncErr error) {
	stats := SyncStats{}
	defer func() { result.Stats = stats }()
	var saved BindingState
	fingerprint := digest([]any{service.Silo.Base, service.Silo.Profile, service.Floppy.Base, binding.ListID, binding.MediaType, binding.LibraryIDs, binding.Key})
	found, err := service.State.Get("binding:"+binding.Key, &saved)
	if err != nil {
		return Changes{}, err
	}
	if found && saved.Fingerprint != fingerprint {
		return Changes{}, errors.New("binding target changed; explicit migration required")
	}
	if found && binding.CollectionID != "" && saved.CollectionID != "" && binding.CollectionID != saved.CollectionID {
		return Changes{}, errors.New("collection_id conflicts with durable mapping")
	}
	saved.Fingerprint = fingerprint
	if saved.Matches == nil {
		saved.Matches = map[string]string{}
	}
	snapshot, err := service.Floppy.selectedSnapshot(ctx, binding.ListID, binding.MediaType, binding.MaxItems, binding.Selection)
	if err != nil {
		return Changes{}, err
	}
	stats.Items, stats.SourceRead = len(snapshot.Entries), true
	mediaType := binding.MediaType
	if mediaType == "tv" {
		mediaType = "series"
	}
	catalog := []CatalogEntry{}
	wanted := map[string]bool{}
	for _, entry := range snapshot.Entries {
		wanted[entry.Key()] = true
	}
	for _, libraryID := range binding.LibraryIDs {
		if service.Lookup == nil {
			return Changes{}, errors.New("read-only database matcher required")
		}
		items, err := service.Lookup(ctx, libraryID, mediaType, snapshot.Entries)
		if err != nil {
			return Changes{}, err
		}
		for _, item := range items {
			candidate := false
			for source, externalID := range map[string]string{"tmdb": item.TMDB, "imdb": item.IMDB, "tvdb": item.TVDB} {
				if wanted[(Entry{binding.MediaType, source, externalID}).Key()] {
					candidate = true
				}
			}
			if !candidate {
				continue
			}
			fresh, available, err := service.Silo.Available(ctx, item, libraryID)
			if err != nil {
				return Changes{}, fmt.Errorf("match revalidation: %w", err)
			}
			if available {
				catalog = append(catalog, fresh)
			}
		}
	}
	matched, err := Plan(snapshot.Entries, catalog, nil, nil)
	if err != nil {
		return Changes{}, err
	}
	stats.Matched, stats.Missing, stats.MatchesKnown = stats.Items-matched.Pending, matched.Pending, true
	if err := service.ensureCollection(ctx, binding, &saved); err != nil {
		return Changes{}, err
	}
	current := []string{}
	if saved.CollectionID != "" {
		current, _, err = service.Silo.Order(ctx, saved.CollectionID)
		if err != nil {
			return Changes{}, err
		}
	}
	changes, err := Plan(snapshot.Entries, catalog, current, saved.Matches)
	if err != nil {
		return changes, err
	}
	if saved.SourceDigest != snapshot.Digest {
		changes.DeferredRemovals = len(changes.Remove)
		changes.Desired = append(changes.Desired, changes.Remove...)
		changes.Remove = nil
	}
	stats.Deferred = changes.DeferredRemovals
	if service.DryRun {
		stats.Added, stats.Removed, stats.PreviewKnown = len(changes.Add), len(changes.Remove), true
		return changes, service.acquireMissing(ctx, changes.Missing, &stats.Acquisition)
	}
	latest, err := service.Floppy.selectedSnapshot(ctx, binding.ListID, binding.MediaType, binding.MaxItems, binding.Selection)
	if err != nil {
		return Changes{}, err
	}
	if latest.Digest != snapshot.Digest || !slices.Equal(latest.Entries, snapshot.Entries) {
		return Changes{}, errors.New("source changed during inventory; deferring reconciliation")
	}
	saved.SourceDigest = snapshot.Digest
	for key, itemID := range changes.Matches {
		saved.Matches[key] = itemID
	}
	if err := service.State.Put("binding:"+binding.Key, saved); err != nil {
		return Changes{}, err
	}
	if len(changes.Desired) > 200 {
		return Changes{}, errors.New("deferred removals would exceed collection limit; retry next cycle")
	}
	for position, itemID := range changes.Desired {
		if !slices.Contains(changes.Add, itemID) {
			continue
		}
		if _, err := service.Silo.Request(ctx, "PUT", collectionPath(saved.CollectionID)+"/items/"+url.PathEscape(itemID), map[string]int{"position": position}, nil, ""); err != nil {
			return Changes{}, fmt.Errorf("add %s not confirmed; remote outcome may be uncertain: %w", itemID, err)
		}
		stats.Added++
	}
	for _, itemID := range changes.Remove {
		if _, err := service.Silo.Request(ctx, "DELETE", collectionPath(saved.CollectionID)+"/items/"+url.PathEscape(itemID), nil, nil, ""); err != nil {
			return Changes{}, fmt.Errorf("remove %s not confirmed; remote outcome may be uncertain: %w", itemID, err)
		}
		stats.Removed++
	}
	for attempt := 0; attempt < 3; attempt++ {
		observed, etag, err := service.Silo.Order(ctx, saved.CollectionID)
		if err != nil {
			return Changes{}, err
		}
		if slices.Equal(observed, changes.Desired) {
			saved.Matches, saved.LastSuccess = changes.Matches, time.Now().UTC()
			if err := service.State.Put("binding:"+binding.Key, saved); err != nil {
				return changes, err
			}
			return changes, service.acquireMissing(ctx, changes.Missing, &stats.Acquisition)
		}
		if !sameLibraries(observed, changes.Desired) {
			return Changes{}, errors.New("membership changed concurrently; deferring reconciliation")
		}
		_, err = service.Silo.Request(ctx, "PUT", collectionPath(saved.CollectionID)+"/items/order", map[string]any{"ordered_ids": changes.Desired}, nil, etag)
		if err != nil {
			var status *APIError
			if !errors.As(err, &status) || status.Status != 412 {
				return Changes{}, err
			}
		}
	}
	return Changes{}, errors.New("order did not converge within retry limit")
}

func (service *Service) acquireMissing(ctx context.Context, entries []Entry, stats *AcquisitionStats) error {
	if service.Vio != nil {
		return service.Vio.acquireMissing(ctx, entries, stats, service.DryRun)
	}
	for _, entry := range entries {
		arr := service.Radarr
		if entry.MediaType == "tv" {
			arr = service.Sonarr
		}
		if arr == nil {
			stats.Disabled++
			continue
		}
		if service.DryRun {
			stats.Requested++
			continue
		}
		if err := arr.acquire(ctx, entry, stats); err != nil {
			stats.Failures = append(stats.Failures, fmt.Errorf("acquisition for %s: %w", entry.Key(), err))
		}
	}
	return errors.Join(stats.Failures...)
}

func (service *Service) Run(ctx context.Context, bindings []Binding) error {
	started := time.Now()
	output := service.output()
	mode := "APPLY"
	if service.DryRun {
		mode = "DRY RUN | No remote changes"
	}
	logLine(output, "Sync started | %s | %d lists", mode, len(bindings))
	results := make([]listResult, 0, len(bindings))
	failed, skipped := 0, 0
	for index, binding := range bindings {
		if ctx.Err() != nil {
			results = append(results, listResult{Binding: binding, Skipped: true})
			skipped++
			continue
		}
		fmt.Fprintln(output)
		logLine(output, "[%d/%d] %s | %s | key=%s", index+1, len(bindings), logText(binding.Title), mediaLabel(binding.MediaType), logText(binding.Key))
		changes, err := service.Sync(ctx, binding)
		result := listResult{Binding: binding, Stats: changes.Stats, Err: err}
		results = append(results, result)
		service.logResult(result)
		if err != nil {
			failed++
		}
	}
	fmt.Fprintln(output)
	logLine(output, "Sync finished | %s | %d lists OK | %d lists with errors | %d not processed", time.Since(started).Round(time.Millisecond), len(results)-failed-skipped, failed, skipped)
	if ctx.Err() != nil {
		logLine(output, "WARNING: Cycle interrupted: %s", logText(ctx.Err().Error()))
	}
	writeSummary(output, results, service.DryRun)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failed > 0 {
		return errors.New("one or more lists failed; inspect per-list status")
	}
	return nil
}
