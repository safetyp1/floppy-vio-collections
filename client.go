package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxResponseBytes = 8 << 20

type API struct {
	Base        string
	Token       string
	APIKey      string
	Profile     string
	HTTP        *http.Client
	silo        bool
	paceMu      sync.Mutex
	nextRequest time.Time
}

func NewSiloAPI(base, token, profile string) (*API, error) {
	api, err := NewAPI(base, token, profile)
	if err != nil {
		return nil, err
	}
	api.silo = true
	return api, nil
}

func (api *API) waitRequest(ctx context.Context) error {
	if !api.silo {
		return ctx.Err()
	}
	api.paceMu.Lock()
	defer api.paceMu.Unlock()
	if delay := time.Until(api.nextRequest); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	api.nextRequest = time.Now().Add(100 * time.Millisecond)
	return nil
}

func NewAPI(base, token, profile string) (*API, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("API URL must be an HTTP(S) origin with an optional base path, without credentials or query")
	}
	return &API{Base: strings.TrimRight(base, "/"), Token: token, Profile: profile, HTTP: &http.Client{
		Timeout:       45 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

type APIError struct{ Status int }

func (err *APIError) Error() string { return fmt.Sprintf("API returned HTTP %d", err.Status) }

func (api *API) Request(ctx context.Context, method, path string, input, output any, etag string) (string, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(path, "..") {
		return "", errors.New("invalid API path")
	}
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			return "", errors.New("cannot encode API request")
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := api.waitRequest(ctx); err != nil {
			return "", err
		}
		request, err := http.NewRequestWithContext(ctx, method, api.Base+path, bytes.NewReader(body))
		if err != nil {
			return "", errors.New("cannot construct API request")
		}
		request.Header.Set("Accept", "application/json")
		if api.APIKey != "" {
			request.Header.Set("X-Api-Key", api.APIKey)
		}
		if api.Token != "" {
			request.Header.Set("Authorization", "Bearer "+api.Token)
		}
		if api.Profile != "" {
			request.Header.Set("X-Profile-Id", api.Profile)
		}
		if input != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		if etag != "" {
			request.Header.Set("If-Match", etag)
		}
		response, err := api.HTTP.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", errors.New("API transport failed; response is uncertain")
		}
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
		response.Body.Close()
		if readErr != nil || len(payload) > maxResponseBytes {
			return "", errors.New("incomplete or oversized API response")
		}
		if response.StatusCode == 429 || response.StatusCode == 502 || response.StatusCode == 503 || response.StatusCode == 504 {
			if (method == http.MethodGet || (api.silo && response.StatusCode == http.StatusTooManyRequests)) && attempt < 2 {
				delay := time.Duration(attempt+1) * time.Second
				if value := response.Header.Get("Retry-After"); value != "" {
					if seconds, err := strconv.Atoi(value); err == nil {
						if seconds > 60 {
							return "", &APIError{response.StatusCode}
						}
						seconds = max(seconds, 0)
						delay = time.Duration(seconds) * time.Second
					} else if deadline, err := http.ParseTime(value); err == nil {
						delay = time.Until(deadline)
					}
				}
				if delay < 0 {
					delay = 0
				}
				if delay > time.Minute {
					return "", &APIError{response.StatusCode}
				}
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return "", ctx.Err()
				case <-timer.C:
				}
				continue
			}
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return "", &APIError{response.StatusCode}
		}
		if output != nil {
			if err := json.Unmarshal(payload, output); err != nil {
				return "", errors.New("invalid JSON API response")
			}
		}
		return response.Header.Get("ETag"), nil
	}
	return "", errors.New("API retry limit reached")
}

type Snapshot struct {
	Entries []Entry           `json:"entries"`
	Digest  string            `json:"-"`
	Titles  map[string]string `json:"-"`
}

func (api *API) Snapshot(ctx context.Context, listID, mediaType string) (Snapshot, error) {
	return api.LimitedSnapshot(ctx, listID, mediaType, 0)
}

func (api *API) LimitedSnapshot(ctx context.Context, listID, mediaType string, maxItems int) (Snapshot, error) {
	return api.selectedSnapshot(ctx, listID, mediaType, maxItems, "")
}

func (api *API) selectedSnapshot(ctx context.Context, listID, mediaType string, maxItems int, selection string) (Snapshot, error) {
	if maxItems < 0 || maxItems > 200 {
		return Snapshot{}, errors.New("max_items must be between 0 and 200")
	}
	if selection != "" && (selection != "latest_released" || maxItems == 0) {
		return Snapshot{}, errors.New("invalid source selection")
	}
	type releasedEntry struct {
		entry Entry
		title string
		date  string
	}
	released := []releasedEntry{}
	today := time.Now().UTC().Format(time.DateOnly)
	snapshot := Snapshot{Entries: []Entry{}, Titles: map[string]string{}}
	seen := map[string]bool{}
	total, offset := -1, 0
	for {
		var response struct {
			Results []struct {
				Item *struct {
					Entry
					Title           string `json:"title"`
					ReleaseDatetime string `json:"release_datetime"`
				} `json:"item"`
			} `json:"results"`
			Pagination *struct {
				Total  *int            `json:"total"`
				Limit  *int            `json:"limit"`
				Offset *int            `json:"offset"`
				Next   json.RawMessage `json:"next"`
			} `json:"pagination"`
		}
		path := "/api/v1/lists/" + url.PathEscape(listID) + "/items/?limit=100&offset=" + strconv.Itoa(offset)
		if _, err := api.Request(ctx, "GET", path, nil, &response, ""); err != nil {
			return Snapshot{}, fmt.Errorf("reading Floppy list: %w", err)
		}
		page := response.Pagination
		if response.Results == nil || page == nil || page.Total == nil || page.Limit == nil || page.Offset == nil || page.Next == nil {
			return Snapshot{}, errors.New("complete Floppy pagination required")
		}
		if *page.Total < 0 || (maxItems == 0 && *page.Total > 200) || (selection != "" && *page.Total > 10000) || *page.Limit < 1 || *page.Limit > 100 || *page.Offset != offset || (total != -1 && total != *page.Total) {
			return Snapshot{}, errors.New("invalid, oversized or changing Floppy list")
		}
		total = *page.Total
		if len(response.Results) != min(*page.Limit, total-offset) {
			return Snapshot{}, errors.New("incomplete Floppy page")
		}
		for position, row := range response.Results {
			if row.Item == nil {
				return Snapshot{}, errors.New("missing Floppy item")
			}
			entry := row.Item.Entry
			if (entry.MediaType != "movie" && entry.MediaType != "tv") || !validIdentity(entry) || seen[entry.Key()] {
				return Snapshot{}, errors.New("invalid or duplicate Floppy identity")
			}
			seen[entry.Key()] = true
			if entry.MediaType == mediaType && selection == "latest_released" {
				if row.Item.ReleaseDatetime == "" {
					continue
				}
				release, err := time.Parse(time.RFC3339, row.Item.ReleaseDatetime)
				if err != nil {
					release, err = time.Parse(time.DateOnly, row.Item.ReleaseDatetime)
				}
				if err != nil {
					return Snapshot{}, errors.New("invalid Floppy release date")
				}
				date := release.Format(time.DateOnly)
				if date <= today {
					released = append(released, releasedEntry{entry, strings.TrimSpace(row.Item.Title), date})
				}
			} else if entry.MediaType == mediaType && (maxItems == 0 || offset+position < maxItems) {
				snapshot.Entries = append(snapshot.Entries, entry)
				snapshot.Titles[entry.Key()] = strings.TrimSpace(row.Item.Title)
			}
		}
		offset += len(response.Results)
		if offset == total {
			if string(page.Next) != "null" {
				return Snapshot{}, errors.New("unexpected Floppy continuation")
			}
			break
		}
		var next string
		if err := json.Unmarshal(page.Next, &next); err != nil || next == "" || len(response.Results) == 0 {
			return Snapshot{}, errors.New("missing Floppy continuation")
		}
		if selection == "" && maxItems > 0 && offset >= maxItems {
			break
		}
	}
	if selection == "latest_released" {
		slices.SortFunc(released, func(first, second releasedEntry) int {
			if order := strings.Compare(second.date, first.date); order != 0 {
				return order
			}
			return strings.Compare(first.entry.Key(), second.entry.Key())
		})
		for _, item := range released[:min(maxItems, len(released))] {
			snapshot.Entries = append(snapshot.Entries, item.entry)
			snapshot.Titles[item.entry.Key()] = item.title
		}
	}
	snapshot.Digest = digest(snapshot.Entries)
	return snapshot, nil
}

func validSource(source string) bool { return source == "tmdb" || source == "imdb" || source == "tvdb" }

func validIdentity(entry Entry) bool {
	if !validSource(entry.Source) {
		return false
	}
	identifier := entry.MediaID
	if entry.Source == "imdb" {
		if !strings.HasPrefix(identifier, "tt") {
			return false
		}
		identifier = strings.TrimPrefix(identifier, "tt")
	}
	if identifier == "" {
		return false
	}
	for _, character := range identifier {
		if character < '0' || character > '9' {
			return false
		}
	}
	return strings.Trim(identifier, "0") != ""
}

type pageInfo struct {
	HasMore    *bool  `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

func (api *API) Catalog(ctx context.Context, libraryID, mediaType, title string, detail func(context.Context, string, string) (CatalogEntry, error)) ([]CatalogEntry, error) {
	if strings.TrimSpace(title) == "" {
		return nil, errors.New("a Floppy title is required for targeted catalog lookup")
	}
	items := []CatalogEntry{}
	seenCursor := map[string]bool{}
	seenItems := map[string]bool{}
	cursor := ""
	for page := 0; page < 10; page++ {
		query := url.Values{"library_id": {libraryID}, "type": {mediaType}, "q": {title}, "limit": {"100"}, "sort": {"title"}, "skip_total": {"true"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var response struct {
			Items []CatalogEntry `json:"items"`
			Page  *pageInfo      `json:"page"`
		}
		if _, err := api.Request(ctx, "GET", "/api/v2/catalog?"+query.Encode(), nil, &response, ""); err != nil {
			return nil, err
		}
		if response.Items == nil || response.Page == nil || response.Page.HasMore == nil {
			return nil, errors.New("incomplete catalog page")
		}
		for _, card := range response.Items {
			if card.ID == "" || card.Type != mediaType || seenItems[card.ID] {
				return nil, errors.New("invalid or unstable catalog traversal")
			}
			seenItems[card.ID] = true
			item, err := detail(ctx, card.ID, libraryID)
			if err != nil {
				return nil, err
			}
			if item.ID != card.ID || item.Type != mediaType {
				return nil, errors.New("catalog identity changed during traversal")
			}
			item.PlayContentID = card.PlayContentID
			items = append(items, item)
		}
		if !*response.Page.HasMore {
			return items, nil
		}
		cursor = response.Page.NextCursor
		if cursor == "" || seenCursor[cursor] || len(response.Items) == 0 {
			return nil, errors.New("invalid catalog continuation")
		}
		seenCursor[cursor] = true
	}
	return nil, errors.New("catalog page limit exceeded")
}

func (api *API) Detail(ctx context.Context, itemID, libraryID string) (CatalogEntry, error) {
	var item CatalogEntry
	_, err := api.Request(ctx, "GET", "/api/v2/catalog/items/"+url.PathEscape(itemID)+"?library_id="+url.QueryEscape(libraryID), nil, &item, "")
	return item, err
}

func (api *API) Available(ctx context.Context, item CatalogEntry, libraryID string) (CatalogEntry, bool, error) {
	fresh, err := api.Detail(ctx, item.ID, libraryID)
	if err != nil {
		return fresh, false, err
	}
	if fresh.ID != item.ID || fresh.Type != item.Type {
		return fresh, false, errors.New("matched catalog identity changed")
	}
	playable := fresh
	if item.Type == "series" {
		if item.PlayContentID == "" {
			return fresh, false, nil
		}
		playable, err = api.Detail(ctx, item.PlayContentID, libraryID)
		if err != nil {
			return fresh, false, err
		}
		if playable.ID != item.PlayContentID || playable.Type != "episode" || playable.SeriesID != item.ID {
			return fresh, false, errors.New("series playback target changed")
		}
	}
	if playable.Versions == nil {
		return fresh, false, errors.New("complete file versions required")
	}
	for _, version := range playable.Versions {
		if version.ID == "" {
			return fresh, false, errors.New("invalid file version")
		}
	}
	return fresh, len(playable.Versions) > 0, nil
}

type Collection struct {
	ID             string   `json:"id"`
	Slug           string   `json:"slug"`
	Title          string   `json:"title"`
	LibraryIDs     []string `json:"library_ids"`
	CollectionType string   `json:"collection_type"`
	ManagementMode string   `json:"management_mode"`
}

func collectionPath(collectionID string) string {
	return "/api/v2/admin/collections/" + url.PathEscape(collectionID)
}

func (api *API) Collection(ctx context.Context, collectionID string) (Collection, error) {
	var collection Collection
	_, err := api.Request(ctx, "GET", collectionPath(collectionID), nil, &collection, "")
	return collection, err
}

func (api *API) Order(ctx context.Context, collectionID string) ([]string, string, error) {
	var order struct {
		IDs []string `json:"ordered_ids"`
	}
	etag, err := api.Request(ctx, "GET", collectionPath(collectionID)+"/items/order", nil, &order, "")
	if err != nil {
		return nil, "", err
	}
	if order.IDs == nil || len(order.IDs) > 200 || etag == "" {
		return nil, "", errors.New("complete bounded item order and ETag required")
	}
	seen := map[string]bool{}
	for _, itemID := range order.IDs {
		if itemID == "" || seen[itemID] {
			return nil, "", errors.New("invalid item order")
		}
		seen[itemID] = true
	}
	return order.IDs, etag, nil
}
