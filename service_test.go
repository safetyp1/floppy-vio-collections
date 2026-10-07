package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestServiceDelayedArrivalRemovalAndFailedSource(t *testing.T) {
	entries := []Entry{{"movie", "tmdb", "10"}, {"movie", "tmdb", "20"}}
	catalog := []CatalogEntry{{ID: "first", Type: "movie", TMDB: "10", Versions: []FileVersion{{ID: "1"}}}, {ID: "second", Type: "movie", TMDB: "20", Versions: []FileVersion{}}}
	order := []string{}
	sourceFailed, inventoryFailed := false, false
	sourceChanged, sourceReads := false, 0
	mutations := 0
	mutationFailureAt := 0
	acquisitionReads, acquisitionWrites := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("ETag", `"test"`)
		path := request.URL.Path
		switch {
		case path == "/api/v3/movie/lookup":
			acquisitionReads++
			if request.URL.Query().Get("term") != "tmdb:20" {
				t.Error("attempted to acquire an available item")
			}
			fmt.Fprint(writer, `[{"tmdbId":20,"title":"Missing movie"}]`)
		case path == "/api/v3/movie":
			if request.Method == "POST" {
				acquisitionWrites++
				fmt.Fprint(writer, `{"id":7}`)
			} else {
				acquisitionReads++
				fmt.Fprint(writer, `[]`)
			}
		case path == "/api/v1/lists/1/items/":
			sourceReads++
			if sourceFailed {
				writer.WriteHeader(401)
				return
			}
			results := []map[string]any{}
			responseEntries := entries
			if sourceChanged && sourceReads%2 == 0 {
				responseEntries = entries[:1]
			}
			for _, entry := range responseEntries {
				results = append(results, map[string]any{"item": map[string]string{"media_type": entry.MediaType, "source": entry.Source, "media_id": entry.MediaID, "title": "Title " + entry.MediaID, "release_datetime": map[string]string{"10": "2020-01-01", "20": "2025-01-01"}[entry.MediaID]}})
			}
			json.NewEncoder(writer).Encode(map[string]any{"results": results, "pagination": map[string]any{"total": len(responseEntries), "limit": 100, "offset": 0, "next": nil}})
		case path == "/api/v2/catalog":
			t.Error("database matching must not search or crawl the catalog")
			if !strings.HasPrefix(request.URL.Query().Get("q"), "Title ") {
				t.Error("catalog request did not use the Floppy title")
			}
			if inventoryFailed {
				fmt.Fprint(writer, `{"items":[]}`)
				return
			}
			json.NewEncoder(writer).Encode(map[string]any{"items": catalog, "page": map[string]any{"has_more": false}})
		case strings.HasPrefix(path, "/api/v2/catalog/items/"):
			for _, item := range catalog {
				if strings.HasSuffix(path, "/"+item.ID) {
					json.NewEncoder(writer).Encode(item)
					return
				}
			}
			writer.WriteHeader(404)
		case path == "/api/v2/admin/collections/managed":
			json.NewEncoder(writer).Encode(Collection{ID: "managed", CollectionType: "manual", ManagementMode: "manual", LibraryIDs: []string{"1"}})
		case strings.HasSuffix(path, "/items/order"):
			if request.Method == "PUT" {
				mutations++
				if request.Header.Get("If-Match") != `"test"` {
					t.Error("missing conditional order write")
				}
				var body struct {
					IDs []string `json:"ordered_ids"`
				}
				json.NewDecoder(request.Body).Decode(&body)
				order = body.IDs
			}
			json.NewEncoder(writer).Encode(map[string]any{"ordered_ids": order})
		case strings.Contains(path, "/items/"):
			mutations++
			if mutations == mutationFailureAt {
				writer.WriteHeader(500)
				return
			}
			itemID := path[strings.LastIndex(path, "/")+1:]
			if request.Method == "PUT" && !slices.Contains(order, itemID) {
				order = append(order, itemID)
			}
			if request.Method == "DELETE" {
				order = slices.DeleteFunc(order, func(value string) bool { return value == itemID })
			}
			writer.WriteHeader(204)
		default:
			t.Errorf("unexpected endpoint %s", path)
			writer.WriteHeader(404)
		}
	}))
	defer server.Close()
	api, _ := NewAPI(server.URL, "secret", "")
	state, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	service := &Service{Silo: api, Floppy: api, State: state, DryRun: true}
	service.Radarr = &Arr{API: api, State: state, Kind: "movie", Config: ArrConfig{QualityProfileID: 1, RootFolderPath: "/movies"}}
	service.Lookup = func(ctx context.Context, library, kind string, source []Entry) ([]CatalogEntry, error) {
		if inventoryFailed {
			return nil, errors.New("database unavailable")
		}
		if len(source) == 0 {
			return []CatalogEntry{}, nil
		}
		return catalog, nil
	}
	binding := Binding{Key: "test", ListID: "1", MediaType: "movie", LibraryIDs: []string{"1"}, Title: "Test", CollectionID: "managed"}
	if changes, err := service.Sync(context.Background(), binding); err != nil || mutations != 0 {
		t.Fatalf("dry run: writes=%d err=%v", mutations, err)
	} else if changes.Stats.Items != 2 || changes.Stats.Matched != 1 || changes.Stats.Added != 1 || changes.Stats.Acquisition.Requested != 1 || changes.Stats.Acquisition.Submitted != 0 {
		t.Fatalf("dry run stats: %+v", changes.Stats)
	}
	if acquisitionReads != 0 || acquisitionWrites != 0 {
		t.Fatal("dry run contacted Radarr")
	}
	service.DryRun = false
	sourceChanged, sourceReads = true, 0
	if _, err := service.Sync(context.Background(), binding); err == nil || mutations != 0 || acquisitionReads != 0 || acquisitionWrites != 0 {
		t.Fatalf("changing source was not deferred: mutations=%d acquisitions=%d err=%v", mutations, acquisitionWrites, err)
	}
	sourceChanged = false
	if changes, err := service.Sync(context.Background(), binding); err != nil || !slices.Equal(order, []string{"first"}) {
		t.Fatalf("initial: %v %v", order, err)
	} else if changes.Stats.Matched != 1 || changes.Stats.Added != 1 || changes.Stats.Acquisition.Submitted != 1 {
		t.Fatalf("initial stats: %+v", changes.Stats)
	}
	if acquisitionWrites != 1 {
		t.Fatal("missing item was not requested")
	}
	if changes, err := service.Sync(context.Background(), binding); err != nil || changes.Stats.Acquisition.Submitted != 0 || changes.Stats.Acquisition.Previous != 1 || changes.Stats.Added != 0 {
		t.Fatalf("repeat sync stats: %+v err=%v", changes.Stats, err)
	}
	catalog[1].Versions = []FileVersion{{ID: "2"}}
	if _, err := service.Sync(context.Background(), binding); err != nil || !slices.Equal(order, []string{"first", "second"}) {
		t.Fatalf("arrival: %v %v", order, err)
	}
	sourceFailed = true
	before := mutations
	if _, err := service.Sync(context.Background(), binding); err == nil || mutations != before {
		t.Fatalf("failed source mutated state: writes=%d err=%v", mutations, err)
	}
	sourceFailed = false
	inventoryFailed = true
	if _, err := service.Sync(context.Background(), binding); err == nil || mutations != before {
		t.Fatalf("failed inventory mutated state: writes=%d err=%v", mutations, err)
	}
	inventoryFailed = false
	entries = entries[1:]
	if changes, err := service.Sync(context.Background(), binding); err != nil || changes.DeferredRemovals != 1 || len(order) != 2 {
		t.Fatalf("removal confirmation: %v %v", order, err)
	}
	if changes, err := service.Sync(context.Background(), binding); err != nil || !slices.Equal(order, []string{"second"}) {
		t.Fatalf("removal: %v %v", order, err)
	} else if changes.Stats.Removed != 1 || changes.Stats.Matched != 1 {
		t.Fatalf("removal stats: %+v", changes.Stats)
	}
	entries = []Entry{}
	if _, err := service.Sync(context.Background(), binding); err != nil || len(order) != 1 {
		t.Fatalf("transient empty: %v %v", order, err)
	}
	if _, err := service.Sync(context.Background(), binding); err != nil || len(order) != 0 {
		t.Fatalf("empty: %v %v", order, err)
	}
	if acquisitionWrites != 1 {
		t.Fatalf("unexpected acquisition writes: %d", acquisitionWrites)
	}
	entries = []Entry{{"movie", "tmdb", "10"}, {"movie", "tmdb", "20"}}
	mutationFailureAt = mutations + 2
	if changes, err := service.Sync(context.Background(), binding); err == nil || changes.Stats.Added != 1 || changes.Stats.Matched != 2 || changes.Stats.Acquisition.Submitted != 0 {
		t.Fatalf("partial add stats: %+v err=%v", changes.Stats, err)
	}
	mutationFailureAt = 0
	if _, err := service.Sync(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	catalog[0].Versions = []FileVersion{}
	service.Radarr = nil
	if changes, err := service.Sync(context.Background(), binding); err != nil || changes.Stats.Matched != 1 || changes.Stats.Missing != 1 || len(changes.Desired) != 2 {
		t.Fatalf("retained member counted as match: %+v err=%v", changes.Stats, err)
	}
	entries = nil
	if _, err := service.Sync(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	mutationFailureAt = mutations + 2
	if changes, err := service.Sync(context.Background(), binding); err == nil || changes.Stats.Removed != 1 || changes.Stats.Matched != 0 {
		t.Fatalf("partial removal stats: %+v err=%v", changes.Stats, err)
	}
	mutationFailureAt = 0
	entries = []Entry{{"movie", "tmdb", "10"}, {"movie", "tmdb", "20"}}
	binding.MaxItems, binding.Selection = 1, "latest_released"
	sourceChanged, sourceReads = true, 0
	before = mutations
	if _, err := service.Sync(context.Background(), binding); err == nil || mutations != before || sourceReads != 2 {
		t.Fatalf("latest release source change: reads=%d writes=%d err=%v", sourceReads, mutations-before, err)
	}
	sourceChanged, sourceReads = false, 0
	if changes, err := service.Sync(context.Background(), binding); err != nil || changes.Stats.Items != 1 || changes.Stats.Matched != 1 || changes.Stats.Missing != 0 || sourceReads != 2 {
		t.Fatalf("latest release selection: stats=%+v reads=%d err=%v", changes.Stats, sourceReads, err)
	}
}

func TestUnknownCreationOutcomeDoesNotCreateAgain(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == "POST" {
			posts++
			writer.WriteHeader(503)
			return
		}
		fmt.Fprint(writer, `{"items":[]}`)
	}))
	defer server.Close()
	api, _ := NewAPI(server.URL, "", "")
	state, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	service := &Service{Silo: api, State: state}
	saved := BindingState{Fingerprint: strings.Repeat("a", 64)}
	binding := Binding{Key: "test", LibraryIDs: []string{"1"}, Title: "Test"}
	if err := service.ensureCollection(context.Background(), binding, &saved); err == nil {
		t.Fatal("expected uncertain creation")
	}
	if err := service.ensureCollection(context.Background(), binding, &saved); err == nil || posts != 1 {
		t.Fatalf("posts=%d err=%v", posts, err)
	}
}

func TestAcquisitionRoutingContinuesAfterFailure(t *testing.T) {
	seriesAdded := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v3/movie/lookup":
			writer.WriteHeader(401)
		case "/api/v3/series/lookup":
			fmt.Fprint(writer, `[{"tvdbId":20,"title":"Series"}]`)
		case "/api/v3/series":
			if request.Method == "POST" {
				seriesAdded++
				fmt.Fprint(writer, `{"id":7}`)
			} else {
				fmt.Fprint(writer, `[]`)
			}
		default:
			t.Errorf("unexpected endpoint: %s", request.URL.Path)
			writer.WriteHeader(404)
		}
	}))
	defer server.Close()
	state, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	api, _ := NewAPI(server.URL, "", "")
	service := &Service{
		Radarr: &Arr{API: api, State: state, Kind: "movie"},
		Sonarr: &Arr{API: api, State: state, Kind: "tv"},
	}
	entries := []Entry{{"movie", "tmdb", "10"}, {"tv", "tvdb", "20"}}
	stats := AcquisitionStats{}
	if err := service.acquireMissing(context.Background(), entries, &stats); err == nil || !strings.Contains(err.Error(), "movie:tmdb:10") || seriesAdded != 1 {
		t.Fatalf("seriesAdded=%d err=%v", seriesAdded, err)
	}
	if stats.Submitted != 1 || len(stats.Failures) != 1 {
		t.Fatalf("partial acquisition stats: %+v", stats)
	}
	service.Radarr, service.Sonarr = nil, nil
	stats = AcquisitionStats{}
	if err := service.acquireMissing(context.Background(), entries, &stats); err != nil {
		t.Fatalf("disabled acquisition failed: %v", err)
	}
	if stats.Disabled != 2 || stats.Submitted != 0 {
		t.Fatalf("disabled acquisition stats: %+v", stats)
	}
}

func TestServiceRunReadableSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/lists/1/items/":
			writer.WriteHeader(401)
		case "/api/v1/lists/2/items/":
			fmt.Fprint(writer, `{"results":[],"pagination":{"total":0,"limit":100,"offset":0,"next":null}}`)
		case "/api/v2/admin/collections/managed":
			json.NewEncoder(writer).Encode(Collection{ID: "managed", CollectionType: "manual", ManagementMode: "manual", LibraryIDs: []string{"1"}})
		case "/api/v2/admin/collections/managed/items/order":
			writer.Header().Set("ETag", `"test"`)
			fmt.Fprint(writer, `{"ordered_ids":[]}`)
		default:
			t.Errorf("unexpected request: %s", request.URL.Path)
			writer.WriteHeader(404)
		}
	}))
	defer server.Close()
	state, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	api, _ := NewAPI(server.URL, "", "")
	var output bytes.Buffer
	service := &Service{Silo: api, Floppy: api, State: state, DryRun: true, Output: &output,
		Lookup: func(context.Context, string, string, []Entry) ([]CatalogEntry, error) { return nil, nil },
	}
	bindings := []Binding{
		{Key: "movies", Title: "Movie\nList", ListID: "1", MediaType: "movie", LibraryIDs: []string{"1"}, CollectionID: "managed"},
		{Key: "tv", Title: "TV List", ListID: "2", MediaType: "tv", LibraryIDs: []string{"1"}, CollectionID: "managed"},
	}
	if err := service.Run(context.Background(), bindings); err == nil {
		t.Fatal("expected failed list")
	}
	text := output.String()
	for _, expected := range []string{
		"Sync started | DRY RUN | No remote changes | 2 lists",
		"[1/2] Movie List | Movies | key=movies",
		"[2/2] TV List | TV | key=tv",
		"Source: - items | Matched in Silo: - | Missing: -",
		"Source: 0 items | Matched in Silo: 0 | Missing: 0",
		"1 lists OK | 1 lists with errors | 0 not processed",
		"ERROR:", "Would Add", "Would Remove", "Would Request", "(known counts)",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("missing %q in logs:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "Movie\nList") || strings.Contains(text, `"level"`) {
		t.Fatalf("unsafe or JSON logging: %s", text)
	}
	output.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Run(ctx, bindings); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if text := output.String(); !strings.Contains(text, "2 not processed") || strings.Count(text, "Not processed") != 2 || !strings.Contains(text, "Cycle interrupted") || !strings.Contains(text, "TOTAL") {
		t.Fatalf("missing cancellation summary: %s", text)
	}
}
