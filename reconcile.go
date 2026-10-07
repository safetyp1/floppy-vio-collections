package main

import (
	"fmt"
	"slices"
)

type Entry struct {
	MediaType string `json:"media_type"`
	Source    string `json:"source"`
	MediaID   string `json:"media_id"`
}

func (entry Entry) Key() string {
	return entry.MediaType + ":" + entry.Source + ":" + entry.MediaID
}

type CatalogEntry struct {
	ID            string        `json:"content_id"`
	Type          string        `json:"type"`
	TMDB          string        `json:"tmdb_id"`
	IMDB          string        `json:"imdb_id"`
	TVDB          string        `json:"tvdb_id"`
	PlayContentID string        `json:"play_content_id,omitempty"`
	SeriesID      string        `json:"series_id,omitempty"`
	Versions      []FileVersion `json:"versions"`
}

type FileVersion struct {
	ID string `json:"file_id"`
}

type Changes struct {
	Desired          []string
	Add              []string
	Remove           []string
	Pending          int
	Missing          []Entry
	DeferredRemovals int
	Matches          map[string]string
	Stats            SyncStats
}

func Plan(entries []Entry, catalog []CatalogEntry, current []string, previous map[string]string) (Changes, error) {
	changes := Changes{Desired: []string{}, Matches: map[string]string{}}
	index := map[string][]string{}
	for _, item := range catalog {
		kind := item.Type
		if kind == "series" {
			kind = "tv"
		}
		for source, externalID := range map[string]string{"tmdb": item.TMDB, "imdb": item.IMDB, "tvdb": item.TVDB} {
			if externalID == "" {
				continue
			}
			key := (Entry{kind, source, externalID}).Key()
			if !slices.Contains(index[key], item.ID) {
				index[key] = append(index[key], item.ID)
			}
		}
	}
	for _, entry := range entries {
		key := entry.Key()
		candidates := index[key]
		if len(candidates) > 1 {
			return Changes{}, fmt.Errorf("ambiguous catalog identity; no membership changes applied")
		}
		matched := ""
		if len(candidates) == 1 {
			matched = candidates[0]
		} else {
			changes.Pending++
			changes.Missing = append(changes.Missing, entry)
			if slices.Contains(current, previous[key]) {
				matched = previous[key]
			}
		}
		if matched == "" {
			continue
		}
		changes.Matches[key] = matched
		if !slices.Contains(changes.Desired, matched) {
			changes.Desired = append(changes.Desired, matched)
		}
	}
	for _, itemID := range changes.Desired {
		if !slices.Contains(current, itemID) {
			changes.Add = append(changes.Add, itemID)
		}
	}
	for _, itemID := range current {
		if !slices.Contains(changes.Desired, itemID) {
			changes.Remove = append(changes.Remove, itemID)
		}
	}
	return changes, nil
}
