package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

type Vio struct {
	API   *API
	State *State
}

type vioRequestBody struct {
	MediaType    string `json:"media_type"`
	TMDBID       int    `json:"tmdb_id"`
	TVDBID       int    `json:"tvdb_id,omitempty"`
	IMDBID       string `json:"imdb_id,omitempty"`
	Title        string `json:"title"`
	Year         int    `json:"year,omitempty"`
	Overview     string `json:"overview,omitempty"`
	PosterPath   string `json:"poster_path,omitempty"`
	BackdropPath string `json:"backdrop_path,omitempty"`
}

type vioDetail struct {
	vioRequestBody
	Availability string `json:"availability"`
	Request      *struct {
		ID          string `json:"request_id"`
		Requestable *bool  `json:"requestable"`
	} `json:"request"`
}

func (detail vioDetail) requestedOrAvailable() bool {
	return detail.Availability == "available" || (detail.Request != nil && detail.Request.ID != "")
}

func (vio *Vio) detail(ctx context.Context, kind string, tmdbID int) (vioDetail, error) {
	var detail vioDetail
	_, err := vio.API.Request(ctx, http.MethodGet, "/api/v2/requests/detail/"+kind+"/"+strconv.Itoa(tmdbID), nil, &detail, "")
	if err != nil {
		return detail, err
	}
	if detail.MediaType != kind || detail.TMDBID != tmdbID || strings.TrimSpace(detail.Title) == "" ||
		(detail.Availability != "available" && detail.Availability != "missing") || detail.Request == nil || detail.Request.Requestable == nil {
		return detail, errors.New("incomplete or mismatched Vio request detail")
	}
	return detail, nil
}

func (vio *Vio) acquire(ctx context.Context, entry Entry, stats *AcquisitionStats) error {
	if !validIdentity(entry) || entry.Source != "tmdb" || (entry.MediaType != "movie" && entry.MediaType != "tv") {
		return errors.New("Vio acquisition requires an exact TMDB movie or TV identity; no title-based fallback")
	}
	tmdbID, err := strconv.Atoi(entry.MediaID)
	if err != nil || tmdbID <= 0 {
		return errors.New("invalid TMDB acquisition ID")
	}
	kind := entry.MediaType
	if kind == "tv" {
		kind = "series"
	}
	key := "vio-acquisition:" + digest([]any{vio.API.Base, vio.API.Profile, kind, tmdbID})
	var saved AcquisitionState
	if _, err := vio.State.Get(key, &saved); err != nil {
		return err
	}
	if saved.Status == "submitted" {
		stats.Previous++
		return nil
	}
	if saved.Status != "" && saved.Status != "started" {
		return errors.New("invalid Vio acquisition state")
	}
	detail, err := vio.detail(ctx, kind, tmdbID)
	if err != nil {
		return err
	}
	if detail.requestedOrAvailable() {
		if err := vio.State.Put(key, AcquisitionState{Status: "submitted"}); err != nil {
			return err
		}
		stats.Previous++
		return nil
	}
	if saved.Status == "started" {
		return fmt.Errorf("Vio request outcome uncertain; inspect Vio before resetting state key %s", key)
	}
	if !*detail.Request.Requestable {
		return errors.New("Vio reports this title is not requestable")
	}
	if err := vio.State.Put(key, AcquisitionState{Status: "started"}); err != nil {
		return err
	}
	var response struct {
		ID string `json:"id"`
	}
	_, submitErr := vio.API.Request(ctx, http.MethodPost, "/api/v2/requests", detail.vioRequestBody, &response, "")
	if submitErr == nil && response.ID != "" {
		stats.Submitted++
		return vio.State.Put(key, AcquisitionState{Status: "submitted"})
	}
	confirmed, confirmErr := vio.detail(ctx, kind, tmdbID)
	if confirmErr == nil && confirmed.requestedOrAvailable() {
		stats.Submitted++
		return vio.State.Put(key, AcquisitionState{Status: "submitted"})
	}
	var apiErr *APIError
	if errors.As(submitErr, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != http.StatusRequestTimeout && apiErr.Status != http.StatusConflict {
		return errors.Join(submitErr, vio.State.Put(key, AcquisitionState{}))
	}
	return fmt.Errorf("Vio request outcome uncertain; inspect state key %s before retrying: %w", key, errors.Join(submitErr, confirmErr, errors.New("no confirmed request ID")))
}

func (vio *Vio) acquireMissing(ctx context.Context, entries []Entry, stats *AcquisitionStats, dryRun bool) error {
	if dryRun {
		stats.Requested += len(entries)
		return nil
	}
	jobs := make(chan Entry)
	results := make(chan AcquisitionStats, len(entries))
	var workers sync.WaitGroup
	for range min(16, len(entries)) {
		workers.Go(func() {
			for entry := range jobs {
				result := AcquisitionStats{}
				if err := vio.acquire(ctx, entry, &result); err != nil {
					result.Failures = append(result.Failures, fmt.Errorf("acquisition for %s: %w", entry.Key(), err))
				}
				results <- result
			}
		})
	}
	for _, entry := range entries {
		jobs <- entry
	}
	close(jobs)
	workers.Wait()
	close(results)
	for result := range results {
		stats.Submitted += result.Submitted
		stats.Previous += result.Previous
		stats.Failures = append(stats.Failures, result.Failures...)
	}
	return errors.Join(stats.Failures...)
}
