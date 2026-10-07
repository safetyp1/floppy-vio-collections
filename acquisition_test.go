package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquisitionAddAndSearchOnce(t *testing.T) {
	for _, kind := range []string{"movie", "tv"} {
		t.Run(kind, func(t *testing.T) {
			resource, field, source := "movie", "tmdbId", "tmdb"
			if kind == "tv" {
				resource, field, source = "series", "tvdbId", "tvdb"
			}
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("X-Api-Key") != "secret" || request.Header.Get("Authorization") != "" {
					t.Error("incorrect acquisition authentication")
				}
				switch {
				case request.URL.Path == "/api/v3/"+resource+"/lookup":
					if request.URL.Query().Get("term") != source+":10" && request.URL.Query().Get("term") != "imdb:tt10" {
						t.Error("lookup must use the external ID")
					}
					fmt.Fprintf(writer, `[{"%s":10,"imdbId":"tt10","title":"Example","titleSlug":"example","seasons":[{"seasonNumber":1,"monitored":true}]}]`, field)
				case request.Method == "GET" && request.URL.Path == "/api/v3/"+resource:
					t.Error("acquisition must not enumerate inventory")
					fmt.Fprint(writer, `[]`)
				case request.Method == "POST" && request.URL.Path == "/api/v3/"+resource:
					posts++
					var body map[string]any
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						t.Error(err)
						writer.WriteHeader(400)
						return
					}
					if body["qualityProfileId"] != float64(3) || body["rootFolderPath"] != "/media" || body["monitored"] != true {
						t.Errorf("incorrect add settings: %v", body)
					}
					options, _ := body["addOptions"].(map[string]any)
					if kind == "movie" && options["searchForMovie"] != true {
						t.Error("missing movie search")
					}
					if kind == "tv" && (options["searchForMissingEpisodes"] != true || options["monitor"] != "all") {
						t.Error("missing series search")
					}
					fmt.Fprint(writer, `{"id":7}`)
				default:
					t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
					writer.WriteHeader(404)
				}
			}))
			defer server.Close()
			api, _ := NewAPI(server.URL, "", "")
			api.APIKey = "secret"
			statePath := filepath.Join(t.TempDir(), "state.db")
			state, err := OpenState(statePath)
			if err != nil {
				t.Fatal(err)
			}
			arr := &Arr{API: api, State: state, Kind: kind, Config: ArrConfig{QualityProfileID: 3, RootFolderPath: "/media"}}
			entry := Entry{kind, source, "10"}
			stats := AcquisitionStats{}
			if err := arr.acquire(context.Background(), entry, &stats); err != nil {
				t.Fatal(err)
			}
			if stats.Submitted != 1 || stats.Previous != 0 {
				t.Fatalf("new acquisition stats: %+v", stats)
			}
			state.Close()
			state, err = OpenState(statePath)
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			arr.State = state
			stats = AcquisitionStats{}
			if err := arr.acquire(context.Background(), Entry{kind, "imdb", "tt10"}, &stats); err != nil || posts != 1 {
				t.Fatalf("duplicate acquisition: posts=%d err=%v", posts, err)
			}
			if stats.Submitted != 0 || stats.Previous != 1 {
				t.Fatalf("previous acquisition stats: %+v", stats)
			}
		})
	}
}

func TestAcquisitionExistingAndFailureSafety(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		entry      Entry
		lookup     string
		inventory  string
		postStatus int
		wantPosts  int
		wantError  bool
	}{
		{"existing movie", "movie", Entry{"movie", "imdb", "tt10"}, `[{"tmdbId":10,"imdbId":"tt10"}]`, `[{"id":7,"tmdbId":10,"monitored":true}]`, 200, 1, false},
		{"existing series", "tv", Entry{"tv", "tmdb", "10"}, `[{"tvdbId":20,"tmdbId":10}]`, `[{"id":7,"tvdbId":20,"monitored":true,"seasons":[{"monitored":true}]}]`, 200, 1, false},
		{"uncertain command", "movie", Entry{"movie", "tmdb", "10"}, `[{"tmdbId":10}]`, `[{"id":7,"tmdbId":10,"monitored":true}]`, 503, 1, true},
		{"uncertain add", "movie", Entry{"movie", "tmdb", "10"}, `[{"tmdbId":10}]`, `[]`, 503, 1, true},
		{"rejected command retries next cycle", "movie", Entry{"movie", "tmdb", "10"}, `[{"tmdbId":10}]`, `[{"id":7,"tmdbId":10,"monitored":true}]`, 429, 2, true},
		{"unmonitored movie", "movie", Entry{"movie", "tmdb", "10"}, `[{"tmdbId":10}]`, `[{"id":7,"tmdbId":10,"monitored":false}]`, 200, 0, true},
		{"unmonitored seasons", "tv", Entry{"tv", "tvdb", "20"}, `[{"tvdbId":20}]`, `[{"id":7,"tvdbId":20,"monitored":true,"seasons":[{"monitored":false}]}]`, 200, 0, true},
		{"no exact match", "movie", Entry{"movie", "tmdb", "10"}, `[{"tmdbId":11}]`, `[]`, 200, 0, true},
		{"ambiguous lookup", "movie", Entry{"movie", "tmdb", "10"}, `[{"tmdbId":10},{"tmdbId":10}]`, `[]`, 200, 0, true},
		{"incomplete inventory", "movie", Entry{"movie", "tmdb", "10"}, `[{"tmdbId":10}]`, `null`, 200, 0, true},
		{"wrong inventory identity", "movie", Entry{"movie", "tmdb", "10"}, `[{"tmdbId":10}]`, `[{"id":7,"tmdbId":11,"monitored":true}]`, 200, 0, true},
		{"unsupported movie provider", "movie", Entry{"movie", "tvdb", "20"}, `[]`, `[]`, 200, 0, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == "GET" {
					if strings.HasSuffix(request.URL.Path, "/lookup") {
						var results []map[string]any
						if err := json.Unmarshal([]byte(test.lookup), &results); err != nil {
							t.Error(err)
						}
						if test.inventory != `[]` && len(results) == 1 {
							results[0]["id"] = 7
						}
						json.NewEncoder(writer).Encode(results)
					} else {
						resource := "movie"
						if test.kind == "tv" {
							resource = "series"
						}
						if request.URL.Path != "/api/v3/"+resource+"/7" || request.URL.RawQuery != "" {
							t.Error("existing items require a direct detail lookup")
						}
						var items []json.RawMessage
						if err := json.Unmarshal([]byte(test.inventory), &items); err != nil {
							t.Error(err)
						}
						if len(items) == 1 {
							writer.Write(items[0])
						} else {
							fmt.Fprint(writer, `null`)
						}
					}
					return
				}
				posts++
				if test.inventory != `[]` {
					if request.URL.Path != "/api/v3/command" || request.Method != "POST" {
						t.Error("existing item must only receive a search command")
					}
					var command struct {
						Name     string `json:"name"`
						MovieIDs []int  `json:"movieIds"`
						SeriesID int    `json:"seriesId"`
					}
					if err := json.NewDecoder(request.Body).Decode(&command); err != nil {
						t.Error(err)
					}
					if test.kind == "movie" && (command.Name != "MoviesSearch" || len(command.MovieIDs) != 1 || command.MovieIDs[0] != 7) {
						t.Errorf("incorrect movie command: %+v", command)
					}
					if test.kind == "tv" && (command.Name != "SeriesSearch" || command.SeriesID != 7) {
						t.Errorf("incorrect series command: %+v", command)
					}
				}
				writer.WriteHeader(test.postStatus)
				fmt.Fprint(writer, `{"id":9}`)
			}))
			defer server.Close()
			state, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			api, _ := NewAPI(server.URL, "", "")
			arr := &Arr{API: api, State: state, Kind: test.kind, Config: ArrConfig{QualityProfileID: 3, RootFolderPath: "/media"}}
			for attempt := 0; attempt < 2; attempt++ {
				stats := AcquisitionStats{}
				err := arr.acquire(context.Background(), test.entry, &stats)
				if (err != nil) != test.wantError {
					t.Fatalf("attempt %d: %v", attempt, err)
				}
				if test.wantError && stats.Submitted != 0 {
					t.Fatalf("failed acquisition counted as submitted: %+v", stats)
				}
				if !test.wantError && (stats.Submitted != 1-attempt || stats.Previous != attempt) {
					t.Fatalf("attempt %d stats: %+v", attempt, stats)
				}
			}
			if posts != test.wantPosts {
				t.Fatalf("posts=%d want=%d", posts, test.wantPosts)
			}
		})
	}
}

func TestAcquisitionConfigValidation(t *testing.T) {
	for _, test := range []struct {
		name      string
		arr       *ArrConfig
		wantError bool
	}{
		{"disabled", nil, false},
		{"enabled", &ArrConfig{URL: "http://localhost:7878", QualityProfileID: 1, RootFolderPath: "/movies"}, false},
		{"missing profile", &ArrConfig{URL: "http://localhost:7878", RootFolderPath: "/movies"}, true},
		{"missing root", &ArrConfig{URL: "http://localhost:7878", QualityProfileID: 1}, true},
		{"invalid URL", &ArrConfig{URL: "file:///tmp/test", QualityProfileID: 1, RootFolderPath: "/movies"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := Config{Radarr: test.arr, Sonarr: test.arr, Lists: []Binding{{Key: "test", ListID: "1", MediaType: "movie", LibraryIDs: []string{"1"}, Title: "Test"}}}
			encoded, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadConfig(path); (err != nil) != test.wantError {
				t.Fatalf("config validation: %v", err)
			}
		})
	}
}

func TestAcquisitionCredentials(t *testing.T) {
	t.Setenv("RADARR_API_KEY", "radarr-secret")
	t.Setenv("SONARR_API_KEY", "sonarr-secret")
	config := &ArrConfig{URL: "http://localhost:7878"}
	for kind, expected := range map[string]string{"movie": "radarr-secret", "tv": "sonarr-secret"} {
		arr, err := NewArr(config, kind, nil)
		if err != nil || arr.API.APIKey != expected || arr.API.Token != "" {
			t.Fatal("incorrect API key environment selection")
		}
	}
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config.APIKeyFile = path
	arr, err := NewArr(config, "movie", nil)
	if err != nil || arr.API.APIKey != "file-secret" {
		t.Fatal("API key file did not override environment")
	}
}

func TestVioAcquisitionConfig(t *testing.T) {
	for _, test := range []struct {
		mode      string
		arr       *ArrConfig
		wantError bool
	}{
		{"vio", nil, false},
		{"arr", nil, false},
		{"unknown", nil, true},
		{"vio", &ArrConfig{}, true},
	} {
		config := Config{AcquisitionMode: test.mode, Radarr: test.arr, Lists: []Binding{{Key: "test", ListID: "1", MediaType: "movie", LibraryIDs: []string{"1"}, Title: "Test"}}}
		encoded, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(file, encoded, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadConfig(file); (err != nil) != test.wantError {
			t.Fatalf("mode %s: %v", test.mode, err)
		}
	}
}
