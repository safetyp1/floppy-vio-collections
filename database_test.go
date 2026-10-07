package main

import (
	"context"
	"strings"
	"testing"
)

func TestProviderQueryIsTypedAndParameterized(t *testing.T) {
	for _, source := range []string{"tmdb", "imdb", "tvdb"} {
		identifier := "987654321"
		if source == "imdb" {
			identifier = "tt987654321"
		}
		query, err := providerQuery(Entry{"tv", source, identifier}, "series")
		if err != nil || !strings.Contains(query, "item."+source+"_id = $2") || strings.Contains(query, identifier) {
			t.Fatalf("provider query not parameterized: %v", err)
		}
		if !strings.Contains(query, "file.media_folder_id = $3") || !strings.Contains(query, "file.missing_since IS NULL") {
			t.Fatal("lookup lost library or file availability restriction")
		}
		if strings.Contains(query, " OR ") || strings.Contains(query, "LEFT JOIN") || !strings.Contains(query, "episode.series_id = item.content_id") {
			t.Fatal("series lookup must use a direct episode join")
		}
		if strings.Contains(query, "ORDER BY file.id") || !strings.Contains(query, "LIMIT 1") {
			t.Fatal("playable lookup must find one indexed match without imposing global file order")
		}
		movieQuery, err := providerQuery(Entry{"movie", source, identifier}, "movie")
		if err != nil || strings.Contains(movieQuery, "public.episodes") || !strings.Contains(movieQuery, "file.content_id = item.content_id") {
			t.Fatal("movie lookup must use a direct file match")
		}
	}
	for _, entry := range []Entry{{"movie", "tmdb", "10' OR true"}, {"movie", "bad", "10"}, {"tv", "tmdb", "10"}} {
		if _, err := providerQuery(entry, "movie"); err == nil {
			t.Fatal("accepted invalid typed ID")
		}
	}
}

func TestEmptyDatabaseLookupDoesNotConnect(t *testing.T) {
	matcher := &DatabaseMatcher{}
	items, err := matcher.Lookup(context.Background(), "1", "movie", nil)
	if err != nil || len(items) != 0 {
		t.Fatalf("empty source: %v", err)
	}
}
