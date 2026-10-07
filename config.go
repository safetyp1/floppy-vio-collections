package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

type Connection struct {
	URL       string `json:"url"`
	TokenFile string `json:"token_file"`
	ProfileID string `json:"profile_id,omitempty"`
}

type Binding struct {
	Key          string   `json:"key"`
	ListID       string   `json:"list_id"`
	MediaType    string   `json:"media_type"`
	LibraryIDs   []string `json:"library_ids"`
	Title        string   `json:"title"`
	CollectionID string   `json:"collection_id,omitempty"`
	MaxItems     int      `json:"max_items,omitempty"`
	Selection    string   `json:"selection,omitempty"`
}

type Config struct {
	AcquisitionMode string     `json:"acquisition_mode,omitempty"`
	Silo            Connection `json:"silo"`
	Floppy          Connection `json:"floppy"`
	Radarr          *ArrConfig `json:"radarr,omitempty"`
	Sonarr          *ArrConfig `json:"sonarr,omitempty"`
	Interval        string     `json:"interval"`
	StatePath       string     `json:"state_path"`
	Lists           []Binding  `json:"lists"`
}

var positiveID = regexp.MustCompile(`^[1-9][0-9]*$`)
var safeKey = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func LoadConfig(path string) (Config, time.Duration, error) {
	var config Config
	file, err := os.Open(path)
	if err != nil {
		return config, 0, errors.New("cannot open config")
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, 0, errors.New("invalid config JSON or unknown field")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return config, 0, errors.New("config must contain one JSON document")
	}
	if config.AcquisitionMode != "" && config.AcquisitionMode != "arr" && config.AcquisitionMode != "vio" {
		return config, 0, errors.New("acquisition_mode must be arr or vio")
	}
	if config.AcquisitionMode == "vio" && (config.Radarr != nil || config.Sonarr != nil) {
		return config, 0, errors.New("remove radarr and sonarr configuration when acquisition_mode is vio")
	}
	for _, acquisition := range []*ArrConfig{config.Radarr, config.Sonarr} {
		if acquisition == nil {
			continue
		}
		if _, err := NewAPI(acquisition.URL, "", ""); err != nil {
			return config, 0, errors.New("radarr/sonarr requires a valid HTTP(S) URL")
		}
		if acquisition.QualityProfileID <= 0 || strings.TrimSpace(acquisition.RootFolderPath) == "" {
			return config, 0, errors.New("radarr/sonarr requires a positive quality_profile_id and root_folder_path")
		}
	}
	if config.Interval == "" {
		config.Interval = "3h"
	}
	interval, err := time.ParseDuration(config.Interval)
	if err != nil || interval < time.Minute || interval > 24*time.Hour {
		return config, 0, errors.New("interval must be between 1m and 24h; recommended 3h to 6h")
	}
	if config.StatePath == "" {
		config.StatePath = "state/sync.db"
	}
	if len(config.Lists) == 0 || len(config.Lists) > 100 {
		return config, 0, errors.New("configure between 1 and 100 explicit lists")
	}
	keys, collections := map[string]bool{}, map[string]bool{}
	for _, binding := range config.Lists {
		if !safeKey.MatchString(binding.Key) || keys[binding.Key] {
			return config, 0, errors.New("list keys must be unique lowercase slugs")
		}
		keys[binding.Key] = true
		if !positiveID.MatchString(binding.ListID) || strings.TrimSpace(binding.Title) == "" || len(binding.LibraryIDs) == 0 {
			return config, 0, errors.New("list requires a numeric Floppy ID, title and library IDs")
		}
		if binding.MediaType != "movie" && binding.MediaType != "tv" {
			return config, 0, errors.New("media_type must be movie or tv")
		}
		if binding.MaxItems < 0 || binding.MaxItems > 200 {
			return config, 0, errors.New("max_items must be between 0 and 200")
		}
		if binding.Selection != "" && (binding.Selection != "latest_released" || binding.MaxItems == 0) {
			return config, 0, errors.New("selection must be latest_released with max_items between 1 and 200")
		}
		libraries := map[string]bool{}
		for _, libraryID := range binding.LibraryIDs {
			if !positiveID.MatchString(libraryID) || libraries[libraryID] {
				return config, 0, errors.New("library IDs must be distinct positive numeric strings")
			}
			libraries[libraryID] = true
		}
		if binding.CollectionID != "" {
			if collections[binding.CollectionID] {
				return config, 0, errors.New("two bindings cannot own one collection")
			}
			collections[binding.CollectionID] = true
		}
	}
	return config, interval, nil
}

func readToken(path, environment string) (string, error) {
	token := os.Getenv(environment)
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", errors.New("cannot read token file")
		}
		token = string(data)
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("provide a token in the configured secret file or environment variable")
	}
	return token, nil
}
