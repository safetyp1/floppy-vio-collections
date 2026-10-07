package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

type ArrConfig struct {
	URL              string `json:"url"`
	APIKeyFile       string `json:"api_key_file,omitempty"`
	QualityProfileID int    `json:"quality_profile_id"`
	RootFolderPath   string `json:"root_folder_path"`
}

type Arr struct {
	API    *API
	Config ArrConfig
	State  *State
	Kind   string
}

type AcquisitionState struct {
	Status string `json:"status"`
}

type AcquisitionStats struct {
	Submitted int
	Previous  int
	Requested int
	Disabled  int
	Failures  []error
}

func NewArr(config *ArrConfig, kind string, state *State) (*Arr, error) {
	if config == nil {
		return nil, nil
	}
	environment := "RADARR_API_KEY"
	if kind == "tv" {
		environment = "SONARR_API_KEY"
	}
	key, err := readToken(config.APIKeyFile, environment)
	if err != nil {
		return nil, fmt.Errorf("%s credential: %w", environment, err)
	}
	api, err := NewAPI(config.URL, "", "")
	if err != nil {
		return nil, err
	}
	api.APIKey = key
	return &Arr{API: api, Config: *config, State: state, Kind: kind}, nil
}

type arrIdentity struct {
	ID        int    `json:"id"`
	TMDB      int    `json:"tmdbId"`
	TVDB      int    `json:"tvdbId"`
	IMDB      string `json:"imdbId"`
	Monitored bool   `json:"monitored"`
	Seasons   []struct {
		Monitored bool `json:"monitored"`
	} `json:"seasons"`
}

type arrResource map[string]json.RawMessage

func (resource arrResource) identity() (arrIdentity, error) {
	var identity arrIdentity
	encoded, err := json.Marshal(resource)
	if err == nil {
		err = json.Unmarshal(encoded, &identity)
	}
	return identity, err
}

func (identity arrIdentity) matches(entry Entry) bool {
	switch entry.Source {
	case "tmdb":
		return strconv.Itoa(identity.TMDB) == entry.MediaID
	case "tvdb":
		return strconv.Itoa(identity.TVDB) == entry.MediaID
	case "imdb":
		return identity.IMDB == entry.MediaID
	}
	return false
}

func (arr *Arr) Acquire(ctx context.Context, entry Entry) error {
	return arr.acquire(ctx, entry, &AcquisitionStats{})
}

func (arr *Arr) acquire(ctx context.Context, entry Entry, stats *AcquisitionStats) error {
	if !validIdentity(entry) || entry.MediaType != arr.Kind || (arr.Kind != "movie" && arr.Kind != "tv") {
		return errors.New("invalid acquisition identity")
	}
	resource := "movie"
	if arr.Kind == "tv" {
		resource = "series"
	} else if entry.Source == "tvdb" {
		return errors.New("Radarr requires a TMDB or IMDb movie identity; TVDB movie lookup is unsupported")
	}
	path := "/api/v3/" + resource
	var results []arrResource
	if _, err := arr.API.Request(ctx, "GET", path+"/lookup?term="+url.QueryEscape(entry.Source+":"+entry.MediaID), nil, &results, ""); err != nil {
		return err
	}
	var match arrResource
	var identity arrIdentity
	for _, candidate := range results {
		parsed, err := candidate.identity()
		if err != nil {
			return errors.New("invalid acquisition lookup response")
		}
		if parsed.matches(entry) {
			if match != nil {
				return errors.New("ambiguous acquisition identity")
			}
			match, identity = candidate, parsed
		}
	}
	canonicalID := identity.TMDB
	if arr.Kind == "tv" {
		canonicalID = identity.TVDB
	}
	if match == nil || canonicalID <= 0 {
		return errors.New("no exact acquisition match; refusing title-based fallback")
	}
	key := "acquisition:" + digest([]any{arr.API.Base, arr.Kind, canonicalID})
	var saved AcquisitionState
	found, err := arr.State.Get(key, &saved)
	if err != nil {
		return err
	}
	if found {
		switch saved.Status {
		case "submitted":
			stats.Previous++
			return nil
		case "started":
			return fmt.Errorf("acquisition outcome uncertain; inspect Sonarr/Radarr before resetting state key %s", key)
		case "":
		default:
			return errors.New("invalid acquisition state")
		}
	}
	writePath := path
	var payload any
	if identity.ID > 0 {
		var existing arrResource
		if _, err := arr.API.Request(ctx, "GET", path+"/"+strconv.Itoa(identity.ID), nil, &existing, ""); err != nil {
			return err
		}
		current, err := existing.identity()
		if err != nil || current.ID != identity.ID || (arr.Kind == "movie" && current.TMDB != canonicalID) || (arr.Kind == "tv" && current.TVDB != canonicalID) {
			return errors.New("acquisition inventory identity mismatch")
		}
		if !current.Monitored {
			return errors.New("existing Sonarr/Radarr item is unmonitored; enable monitoring before searching")
		}
		command := map[string]any{"name": "MoviesSearch", "movieIds": []int{current.ID}}
		if arr.Kind == "tv" {
			monitored := false
			for _, season := range current.Seasons {
				monitored = monitored || season.Monitored
			}
			if !monitored {
				return errors.New("existing Sonarr series has no monitored seasons; enable monitoring before searching")
			}
			command = map[string]any{"name": "SeriesSearch", "seriesId": current.ID}
		}
		writePath, payload = "/api/v3/command", command
	} else {
		encoded, err := json.Marshal(match)
		if err != nil {
			return err
		}
		var addition map[string]any
		if err := json.Unmarshal(encoded, &addition); err != nil {
			return err
		}
		delete(addition, "id")
		delete(addition, "path")
		addition["qualityProfileId"] = arr.Config.QualityProfileID
		addition["rootFolderPath"] = arr.Config.RootFolderPath
		addition["monitored"] = true
		addition["addOptions"] = map[string]any{"searchForMovie": true}
		if arr.Kind == "tv" {
			addition["seasonFolder"] = true
			addition["addOptions"] = map[string]any{"monitor": "all", "searchForMissingEpisodes": true, "searchForCutoffUnmetEpisodes": false}
		}
		payload = addition
	}
	if err := arr.State.Put(key, AcquisitionState{Status: "started"}); err != nil {
		return err
	}
	var response struct {
		ID int `json:"id"`
	}
	_, err = arr.API.Request(ctx, "POST", writePath, payload, &response, "")
	if err != nil {
		var apiError *APIError
		if errors.As(err, &apiError) && apiError.Status >= 400 && apiError.Status < 500 && apiError.Status != 408 {
			return errors.Join(err, arr.State.Put(key, AcquisitionState{}))
		}
		return fmt.Errorf("acquisition outcome uncertain for state key %s: %w", key, err)
	}
	if response.ID <= 0 {
		return fmt.Errorf("missing acquisition response ID; inspect state key %s before retrying", key)
	}
	stats.Submitted++
	if err := arr.State.Put(key, AcquisitionState{Status: "submitted"}); err != nil {
		return err
	}
	return nil
}
