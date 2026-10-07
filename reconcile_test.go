package main

import (
	"reflect"
	"testing"
)

func TestDelayedArrivalAndSourceRemoval(t *testing.T) {
	entries := []Entry{{"movie", "tmdb", "10"}, {"movie", "tmdb", "20"}}
	catalog := []CatalogEntry{{ID: "first", Type: "movie", TMDB: "10"}}
	first, err := Plan(entries, catalog, nil, nil)
	if err != nil || first.Pending != 1 || !reflect.DeepEqual(first.Desired, []string{"first"}) {
		t.Fatalf("first sync: %+v %v", first, err)
	}
	catalog = append(catalog, CatalogEntry{ID: "second", Type: "movie", TMDB: "20"})
	next, err := Plan(entries, catalog, first.Desired, first.Matches)
	if err != nil || !reflect.DeepEqual(next.Add, []string{"second"}) {
		t.Fatalf("unchanged source: %+v %v", next, err)
	}
	removed, err := Plan(entries[1:], catalog, next.Desired, next.Matches)
	if err != nil || !reflect.DeepEqual(removed.Remove, []string{"first"}) {
		t.Fatalf("source removal: %+v %v", removed, err)
	}
}

func TestMovieAndSeriesIDsStaySeparate(t *testing.T) {
	changes, err := Plan([]Entry{{"tv", "tmdb", "10"}}, []CatalogEntry{{ID: "movie", Type: "movie", TMDB: "10"}, {ID: "series", Type: "series", TMDB: "10"}}, nil, nil)
	if err != nil || !reflect.DeepEqual(changes.Desired, []string{"series"}) {
		t.Fatalf("%+v %v", changes, err)
	}
}

func TestAmbiguousMatchAborts(t *testing.T) {
	_, err := Plan([]Entry{{"movie", "tmdb", "10"}}, []CatalogEntry{{ID: "first", Type: "movie", TMDB: "10"}, {ID: "second", Type: "movie", TMDB: "10"}}, []string{"existing"}, nil)
	if err == nil {
		t.Fatal("ambiguous match must abort")
	}
}

func TestExistingUnresolvedMemberSurvives(t *testing.T) {
	entry := Entry{"movie", "tmdb", "10"}
	changes, err := Plan([]Entry{entry}, nil, []string{"existing"}, map[string]string{entry.Key(): "existing"})
	if err != nil || len(changes.Remove) != 0 || changes.Pending != 1 || !reflect.DeepEqual(changes.Missing, []Entry{entry}) {
		t.Fatalf("%+v %v", changes, err)
	}
}

func TestOnlyUnavailableEntriesAreMissing(t *testing.T) {
	available := Entry{"movie", "tmdb", "10"}
	missing := Entry{"movie", "tmdb", "20"}
	changes, err := Plan([]Entry{available, missing}, []CatalogEntry{{ID: "first", Type: "movie", TMDB: "10"}}, nil, nil)
	if err != nil || !reflect.DeepEqual(changes.Missing, []Entry{missing}) {
		t.Fatalf("%+v %v", changes, err)
	}
}

func TestAuthoritativeEmptyClearsMembership(t *testing.T) {
	changes, err := Plan([]Entry{}, nil, []string{"existing"}, nil)
	if err != nil || !reflect.DeepEqual(changes.Remove, []string{"existing"}) {
		t.Fatalf("%+v %v", changes, err)
	}
}
