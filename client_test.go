package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestSiloRequestPacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api, _ := NewSiloAPI("http://silo", "", "")
		start := time.Now()
		for attempt := 0; attempt < 4; attempt++ {
			if err := api.waitRequest(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if elapsed := time.Since(start); elapsed != 300*time.Millisecond {
			t.Fatalf("pacing elapsed=%s", elapsed)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		if err := api.waitRequest(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("pacing cancellation: %v", err)
		}
	})
}

type testTransport func(*http.Request) (*http.Response, error)

func (transport testTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestSiloRetryTiming(t *testing.T) {
	for _, test := range []struct {
		name, method, retryAfter string
		cancel, transportFailure bool
		wantCalls                int
		wantDelay                time.Duration
	}{
		{"seconds", "PUT", "2", false, false, 2, 2 * time.Second},
		{"date", "DELETE", "date", false, false, 2, 3 * time.Second},
		{"fallback", "POST", "invalid", false, false, 2, time.Second},
		{"cancellation", "PUT", "10", true, false, 1, 50 * time.Millisecond},
		{"transport uncertainty", "POST", "", false, true, 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api, _ := NewSiloAPI("http://silo", "", "")
				calls := 0
				start := time.Now()
				api.HTTP.Transport = testTransport(func(request *http.Request) (*http.Response, error) {
					calls++
					if test.transportFailure {
						return nil, errors.New("connection lost")
					}
					code := 204
					if calls == 1 {
						code = 429
					}
					retryAfter := test.retryAfter
					if retryAfter == "date" {
						retryAfter = start.Add(3 * time.Second).Format(http.TimeFormat)
					}
					return &http.Response{StatusCode: code, Header: http.Header{"Retry-After": []string{retryAfter}}, Body: io.NopCloser(strings.NewReader(""))}, nil
				})
				ctx := t.Context()
				if test.cancel {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
					defer cancel()
				}
				_, err := api.Request(ctx, test.method, "/items", nil, nil, "")
				if calls != test.wantCalls || time.Since(start) != test.wantDelay || (err != nil) != (test.cancel || test.transportFailure) {
					t.Fatalf("calls=%d delay=%s err=%v", calls, time.Since(start), err)
				}
			})
		})
	}
}

func TestSiloWriteRetry(t *testing.T) {
	for _, test := range []struct {
		name, retryAfter  string
		status, wantCalls int
		silo, recover     bool
	}{
		{"throttle then success", "0", 429, 2, true, true},
		{"bounded throttle", "0", 429, 3, true, false},
		{"long delay", "61", 429, 1, true, false},
		{"overflow delay", "9223372036854775807", 429, 1, true, false},
		{"ambiguous server failure", "0", 503, 1, true, false},
		{"other API unchanged", "0", 429, 1, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				body, _ := io.ReadAll(request.Body)
				if string(body) != `{"position":4}` || request.Header.Get("If-Match") != `"version"` || request.Header.Get("Authorization") != "Bearer secret" {
					t.Error("retry changed request")
				}
				if test.recover && calls == 2 {
					writer.WriteHeader(204)
					return
				}
				writer.Header().Set("Retry-After", test.retryAfter)
				writer.WriteHeader(test.status)
			}))
			defer server.Close()
			api, _ := NewAPI(server.URL, "secret", "")
			api.silo = test.silo
			_, err := api.Request(t.Context(), "PUT", "/items/1", map[string]int{"position": 4}, nil, `"version"`)
			if (err == nil) != test.recover || calls != test.wantCalls {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestSnapshotRejectsIncompleteResponses(t *testing.T) {
	for _, payload := range []string{`{}`, `{"results":null}`, `{"results":[],"pagination":{"total":1,"limit":100,"offset":0,"next":null}}`, `{"results":[{"item":{"media_type":"movie","source":"unknown","media_id":"10"}}],"pagination":{"total":1,"limit":100,"offset":0,"next":null}}`, `not json`} {
		t.Run(payload, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("ETag", `"snapshot"`)
				fmt.Fprint(writer, payload)
			}))
			defer server.Close()
			api, _ := NewAPI(server.URL, "secret", "")
			if _, err := api.Snapshot(context.Background(), "1", "movie"); err == nil {
				t.Fatal("accepted incomplete source")
			}
		})
	}
}

func TestSnapshotAcceptsExplicitEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/prefix/api/v1/lists/1/items/" {
			t.Error(request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing auth")
		}
		writer.Header().Set("ETag", `"snapshot"`)
		fmt.Fprint(writer, `{"results":[],"pagination":{"total":0,"limit":100,"offset":0,"next":null}}`)
	}))
	defer server.Close()
	api, _ := NewAPI(server.URL+"/prefix", "secret", "")
	if _, err := api.Snapshot(context.Background(), "1", "movie"); err != nil {
		t.Fatal(err)
	}
}

func TestRedirectNeverForwardsCredentials(t *testing.T) {
	visited := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { visited = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, 302)
	}))
	defer server.Close()
	api, _ := NewAPI(server.URL, "secret", "")
	api.APIKey = "arr-secret"
	_, err := api.Request(context.Background(), "GET", "/test", nil, nil, "")
	if err == nil || visited || strings.Contains(err.Error(), "secret") {
		t.Fatalf("redirect safety: visited=%v err=%v", visited, err)
	}
}

func TestCatalogMissingPaginationIsNotEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { fmt.Fprint(writer, `{"items":[]}`) }))
	defer server.Close()
	api, _ := NewAPI(server.URL, "", "")
	if _, err := api.Catalog(context.Background(), "1", "movie", "Example", api.Detail); err == nil {
		t.Fatal("accepted incomplete inventory")
	}
}

func TestCatalogRejectsUnfilteredLookup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("unfiltered lookup reached server")
	}))
	defer server.Close()
	api, _ := NewAPI(server.URL, "", "")
	if _, err := api.Catalog(context.Background(), "1", "movie", " ", api.Detail); err == nil {
		t.Fatal("accepted unfiltered lookup")
	}
}

func TestWriteIsNotBlindlyRetried(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { calls++; writer.WriteHeader(503) }))
	defer server.Close()
	api, _ := NewAPI(server.URL, "", "")
	_, err := api.Request(context.Background(), "POST", "/create", map[string]string{}, nil, "")
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestFloppyPaginationUsesConfiguredServerAndFiltersKinds(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.URL.Path != "/api/v1/lists/7/items/" {
			t.Error("unexpected path")
		}
		if calls == 1 {
			fmt.Fprint(writer, `{"results":[{"item":{"media_type":"movie","source":"tmdb","media_id":"10"}}],"pagination":{"total":2,"limit":1,"offset":0,"next":"https://untrusted.invalid/steal"}}`)
		} else {
			if request.URL.Query().Get("offset") != "1" {
				t.Error("wrong offset")
			}
			fmt.Fprint(writer, `{"results":[{"item":{"media_type":"tv","source":"tmdb","media_id":"20"}}],"pagination":{"total":2,"limit":1,"offset":1,"next":null}}`)
		}
	}))
	defer server.Close()
	api, _ := NewAPI(server.URL, "secret", "")
	snapshot, err := api.Snapshot(context.Background(), "7", "movie")
	if err != nil || calls != 2 || len(snapshot.Entries) != 1 || snapshot.Entries[0].MediaID != "10" {
		t.Fatalf("calls=%d snapshot=%v err=%v", calls, snapshot, err)
	}
}

func TestSnapshotWindow(t *testing.T) {
	for _, test := range []struct {
		name                               string
		limit, total, wantItems, wantCalls int
		fault                              string
		wantError                          bool
	}{
		{"latest 200", 200, 1432, 200, 2, "", false},
		{"partial page", 125, 1432, 125, 2, "", false},
		{"short list", 200, 150, 150, 2, "", false},
		{"empty", 200, 0, 0, 1, "", false},
		{"default rejects oversized", 0, 1432, 0, 1, "", true},
		{"changing total", 200, 1432, 0, 2, "total", true},
		{"incomplete page", 200, 1432, 0, 2, "short", true},
		{"missing continuation", 200, 1432, 0, 2, "next", true},
		{"duplicate identity", 200, 1432, 0, 2, "duplicate", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				offset, _ := strconv.Atoi(request.URL.Query().Get("offset"))
				total := test.total
				if calls == 2 && test.fault == "total" {
					total++
				}
				rows := []any{}
				for position := offset; position < min(offset+100, total); position++ {
					identifier := position + 1
					if calls == 2 && test.fault == "duplicate" {
						identifier = 1
					}
					rows = append(rows, map[string]any{"item": Entry{"tv", "tmdb", strconv.Itoa(identifier)}})
				}
				if calls == 2 && test.fault == "short" {
					rows = rows[:len(rows)-1]
				}
				var next any
				if offset+len(rows) < total {
					next = "/next"
				}
				if calls == 2 && test.fault == "next" {
					next = nil
				}
				json.NewEncoder(writer).Encode(map[string]any{"results": rows, "pagination": map[string]any{"total": total, "limit": 100, "offset": offset, "next": next}})
			}))
			defer server.Close()
			api, _ := NewAPI(server.URL, "", "")
			snapshot, err := api.LimitedSnapshot(context.Background(), "17", "tv", test.limit)
			if (err != nil) != test.wantError || calls != test.wantCalls || len(snapshot.Entries) != test.wantItems {
				t.Fatalf("items=%d calls=%d err=%v", len(snapshot.Entries), calls, err)
			}
			for position, entry := range snapshot.Entries {
				if entry.MediaID != strconv.Itoa(position+1) {
					t.Fatal("source order changed")
				}
			}
		})
	}
}

func TestSnapshotLatestReleased(t *testing.T) {
	for _, fault := range []string{"", "invalid date", "changing total", "oversized"} {
		t.Run(fault, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				offset, _ := strconv.Atoi(request.URL.Query().Get("offset"))
				total := 205
				if fault == "changing total" && offset == 200 {
					total++
				}
				if fault == "oversized" {
					total = 10001
				}
				rows := []any{}
				for position := offset; position < min(offset+100, total); position++ {
					date, kind := "2020-01-01", "tv"
					switch position {
					case 200:
						date = "2025-01-01T00:00:00Z"
					case 201:
						date = ""
					case 202:
						date = "2999-01-01"
					case 203:
						date, kind = "2025-12-01", "movie"
					case 204:
						date = "2025-01-01"
					}
					if fault == "invalid date" && position == 204 {
						date = "yesterday"
					}
					rows = append(rows, map[string]any{"item": map[string]any{"media_type": kind, "source": "tmdb", "media_id": strconv.Itoa(position + 1), "title": "Title", "release_datetime": date}})
				}
				var next any
				if offset+len(rows) < total {
					next = "/next"
				}
				json.NewEncoder(writer).Encode(map[string]any{"results": rows, "pagination": map[string]any{"total": total, "limit": 100, "offset": offset, "next": next}})
			}))
			defer server.Close()
			api, _ := NewAPI(server.URL, "", "")
			snapshot, err := api.selectedSnapshot(context.Background(), "17", "tv", 200, "latest_released")
			if fault != "" {
				if err == nil {
					t.Fatal("accepted invalid source")
				}
				return
			}
			if err != nil || calls != 3 || len(snapshot.Entries) != 200 {
				t.Fatalf("snapshot=%+v calls=%d err=%v", snapshot, calls, err)
			}
			if snapshot.Entries[0].MediaID != "201" || snapshot.Entries[1].MediaID != "205" {
				t.Fatal("newest entries on final page not selected with stable tie order")
			}
			for _, entry := range snapshot.Entries {
				if entry.MediaID == "202" || entry.MediaID == "203" || entry.MediaID == "204" {
					t.Fatal("selected unknown, future or wrong-type release")
				}
			}
		})
	}
}

func TestSourceSelectionConfigValidation(t *testing.T) {
	for _, test := range []struct {
		limit     int
		selection string
		valid     bool
	}{
		{0, "", true}, {200, "", true}, {200, "latest_released", true},
		{-1, "", false}, {201, "", false}, {0, "latest_released", false}, {200, "newest", false},
	} {
		config := Config{Lists: []Binding{{Key: "test", ListID: "1", MediaType: "tv", LibraryIDs: []string{"1"}, Title: "Test", MaxItems: test.limit, Selection: test.selection}}}
		encoded, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, encoded, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadConfig(path); (err == nil) != test.valid {
			t.Fatalf("limit=%d selection=%s err=%v", test.limit, test.selection, err)
		}
	}
}

func TestSeriesRequiresAnOwnedPlayableEpisode(t *testing.T) {
	episode := CatalogEntry{ID: "episode", Type: "episode", SeriesID: "series", Versions: []FileVersion{}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/series") {
			json.NewEncoder(writer).Encode(CatalogEntry{ID: "series", Type: "series", TMDB: "10"})
			return
		}
		json.NewEncoder(writer).Encode(episode)
	}))
	defer server.Close()
	api, _ := NewAPI(server.URL, "", "")
	series := CatalogEntry{ID: "series", Type: "series", PlayContentID: "episode"}
	if _, available, err := api.Available(context.Background(), series, "1"); err != nil || available {
		t.Fatalf("metadata-only: %v %v", available, err)
	}
	episode.Versions = []FileVersion{{ID: "1"}}
	if _, available, err := api.Available(context.Background(), series, "1"); err != nil || !available {
		t.Fatalf("arrived: %v %v", available, err)
	}
	episode.SeriesID = "other"
	if _, _, err := api.Available(context.Background(), series, "1"); err == nil {
		t.Fatal("accepted another series' episode")
	}
}
