package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVioAcquisition(t *testing.T) {
	for _, kind := range []string{"movie", "tv"} {
		for _, scenario := range []string{"submitted", "existing", "available", "timeout recorded", "conflict recorded", "conflict unresolved", "uncertain", "rejected", "malformed", "wrong identity"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				mediaType := kind
				if kind == "tv" {
					mediaType = "series"
				}
				posts, gets := 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Header.Get("Authorization") != "Bearer token" || request.Header.Get("X-Profile-Id") != "profile" || request.Header.Get("X-Api-Key") != "" {
						t.Error("incorrect Vio authentication")
					}
					if request.Method == http.MethodGet && request.URL.Path == "/api/v2/requests/detail/"+mediaType+"/10" {
						gets++
						identity := 10
						if scenario == "wrong identity" {
							identity = 11
						}
						requestID, availability := "", "missing"
						if scenario == "existing" || ((scenario == "timeout recorded" || scenario == "conflict recorded") && posts > 0) {
							requestID = "7"
						}
						if scenario == "available" {
							availability = "available"
						}
						fmt.Fprintf(writer, `{"media_type":%q,"tmdb_id":%d,"tvdb_id":20,"imdb_id":"tt10","title":"Example","year":2025,"availability":%q,"request":{"request_id":%q,"requestable":true}}`, mediaType, identity, availability, requestID)
						return
					}
					if request.Method != http.MethodPost || request.URL.Path != "/api/v2/requests" {
						t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
						writer.WriteHeader(404)
						return
					}
					posts++
					var body map[string]any
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body["media_type"] != mediaType || body["tmdb_id"] != float64(10) || body["title"] != "Example" || body["imdb_id"] != "tt10" {
						t.Errorf("incorrect request payload: %+v", body)
					}
					if _, exists := body["request"]; exists {
						t.Error("request detail leaked into create payload")
					}
					switch scenario {
					case "timeout recorded", "uncertain":
						writer.WriteHeader(504)
					case "conflict recorded", "conflict unresolved":
						writer.WriteHeader(409)
					case "rejected":
						writer.WriteHeader(403)
					case "malformed":
						fmt.Fprint(writer, `{}`)
					default:
						writer.WriteHeader(201)
						fmt.Fprint(writer, `{"id":"7"}`)
					}
				}))
				defer server.Close()
				statePath := filepath.Join(t.TempDir(), "state.db")
				api, _ := NewSiloAPI(server.URL, "token", "profile")
				for attempt := 0; attempt < 2; attempt++ {
					state, err := OpenState(statePath)
					if err != nil {
						t.Fatal(err)
					}
					vio := &Vio{API: api, State: state}
					stats := AcquisitionStats{}
					err = vio.acquire(context.Background(), Entry{kind, "tmdb", "10"}, &stats)
					state.Close()
					wantError := scenario == "uncertain" || scenario == "conflict unresolved" || scenario == "rejected" || scenario == "malformed" || scenario == "wrong identity"
					if (err != nil) != wantError {
						t.Fatalf("attempt %d: %v", attempt, err)
					}
					if !wantError && stats.Submitted+stats.Previous != 1 {
						t.Fatalf("missing stats: %+v", stats)
					}
				}
				wantPosts := 1
				if scenario == "existing" || scenario == "available" || scenario == "wrong identity" {
					wantPosts = 0
				}
				if scenario == "rejected" {
					wantPosts = 2
				}
				if posts != wantPosts {
					t.Fatalf("posts=%d want=%d (gets=%d)", posts, wantPosts, gets)
				}
			})
		}
	}
}

func TestVioDryRunAndUnsupportedIdentity(t *testing.T) {
	vio := &Vio{}
	stats := AcquisitionStats{}
	entries := []Entry{{"movie", "tmdb", "10"}, {"tv", "tmdb", "20"}}
	service := &Service{Vio: vio, DryRun: true}
	if err := service.acquireMissing(context.Background(), entries, &stats); err != nil || stats.Requested != 2 {
		t.Fatalf("dry run: %+v %v", stats, err)
	}
	if err := vio.acquire(context.Background(), Entry{"tv", "tvdb", "10"}, &AcquisitionStats{}); err == nil || !strings.Contains(err.Error(), "TMDB") {
		t.Fatalf("unsupported identity: %v", err)
	}
}

func TestVioConcurrentMissing(t *testing.T) {
	started := make(chan struct{}, 20)
	release := make(chan struct{})
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			identity, _ := strconv.Atoi(request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:])
			fmt.Fprintf(writer, `{"media_type":"movie","tmdb_id":%d,"title":"Example","availability":"missing","request":{"requestable":true}}`, identity)
			return
		}
		current := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); current > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
		writer.WriteHeader(201)
		fmt.Fprint(writer, `{"id":"7"}`)
	}))
	defer server.Close()
	state, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	api, _ := NewAPI(server.URL, "", "")
	service := &Service{Vio: &Vio{API: api, State: state}}
	entries := make([]Entry, 20)
	for index := range entries {
		entries[index] = Entry{"movie", "tmdb", strconv.Itoa(index + 1)}
	}
	stats := AcquisitionStats{}
	done := make(chan error, 1)
	go func() { done <- service.acquireMissing(context.Background(), entries, &stats) }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for range 16 {
		select {
		case <-started:
		case <-deadline.C:
			close(release)
			<-done
			t.Fatal("requests did not queue concurrently")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stats.Submitted != 20 || peak.Load() != 16 {
		t.Fatalf("submitted=%d peak=%d", stats.Submitted, peak.Load())
	}
	stats = AcquisitionStats{}
	if err := service.acquireMissing(context.Background(), entries, &stats); err != nil || stats.Previous != 20 {
		t.Fatalf("repeat: %+v %v", stats, err)
	}
}

func TestVioLogLabels(t *testing.T) {
	var output bytes.Buffer
	service := &Service{Vio: &Vio{}, Output: &output}
	service.logResult(listResult{Stats: SyncStats{Acquisition: AcquisitionStats{Submitted: 2}}})
	if !strings.Contains(output.String(), "Vio: 2 requests submitted") || strings.Contains(output.String(), "Radarr") {
		t.Fatal(output.String())
	}
}
