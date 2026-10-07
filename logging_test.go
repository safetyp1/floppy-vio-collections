package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestReadableSummary(t *testing.T) {
	results := []listResult{
		{Binding: Binding{Title: "Netflix Trending", MediaType: "movie"}, Stats: SyncStats{SourceRead: true, MatchesKnown: true, Items: 30, Matched: 25, Added: 3, Acquisition: AcquisitionStats{Submitted: 5}}},
		{Binding: Binding{Title: "Netflix Trending", MediaType: "tv"}, Stats: SyncStats{SourceRead: true, MatchesKnown: true, Items: 50, Matched: 42, Added: 5, Removed: 2, Acquisition: AcquisitionStats{Submitted: 6, Previous: 2}}},
		{Binding: Binding{Title: "Disney Popular", MediaType: "tv"}, Stats: SyncStats{SourceRead: true, MatchesKnown: true, Items: 20, Matched: 18, Removed: 1, Acquisition: AcquisitionStats{Submitted: 1, Failures: []error{errors.New("no exact acquisition match")}}}, Err: errors.New("acquisition failed")},
		{Binding: Binding{Title: "Weekend Watchlist", MediaType: "movie"}, Stats: SyncStats{SourceRead: true, MatchesKnown: true, Items: 15, Matched: 15, Added: 2}},
	}
	var output bytes.Buffer
	writeSummary(&output, results, false)
	text := output.String()
	var rows []string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && (fields[0] == "TV" || fields[0] == "Movies" || fields[0] == "TOTAL") {
			rows = append(rows, strings.Join(fields, " "))
		}
	}
	expected := []string{
		"TV Netflix Trending 50 42 5 2 6 0 OK",
		"TV Disney Popular 20 18 0 1 1 1 Error",
		"Movies Netflix Trending 30 25 3 0 5 0 OK",
		"Movies Weekend Watchlist 15 15 2 0 0 0 OK",
		"TOTAL 115 100 10 3 12 1",
	}
	if strings.Join(rows, "\n") != strings.Join(expected, "\n") {
		t.Fatalf("unexpected summary:\n%s", text)
	}
	t.Log(text)
}

func TestReadableDryRunAndNotes(t *testing.T) {
	var output bytes.Buffer
	service := &Service{DryRun: true, Output: &output}
	result := listResult{
		Binding: Binding{Title: "Preview", MediaType: "tv"},
		Stats:   SyncStats{SourceRead: true, MatchesKnown: true, PreviewKnown: true, Items: 10, Matched: 4, Missing: 6, Added: 2, Deferred: 3, Acquisition: AcquisitionStats{Requested: 6}},
	}
	service.logResult(result)
	writeSummary(&output, []listResult{result}, true)
	text := output.String()
	for _, expected := range []string{"Matched in Silo: 4", "2 would add | 0 would remove", "6 would request (not checked with Arr)", "3 removals deferred", "Would Add", "Would Remove", "Would Request", "Preview"} {
		if !strings.Contains(text, expected) {
			t.Errorf("missing %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "searches submitted") || strings.Contains(text, "Searched") {
		t.Fatalf("dry run implies searches performed: %s", text)
	}
	output.Reset()
	service.DryRun = false
	result.Stats.Acquisition = AcquisitionStats{Disabled: 6}
	service.logResult(result)
	if !strings.Contains(output.String(), "Acquisition disabled for 6 missing items") {
		t.Fatal(output.String())
	}
	output.Reset()
	result.Stats.Acquisition = AcquisitionStats{Submitted: 1, Previous: 2, Failures: []error{errors.New("tv:tmdb:10: acquisition outcome uncertain\ninspect state")}}
	result.Err = errors.New("acquisition failed")
	service.logResult(result)
	if text := output.String(); !strings.Contains(text, "1 searches submitted | 2 previously requested") || !strings.Contains(text, "ERROR: tv:tmdb:10: acquisition outcome uncertain inspect state") {
		t.Fatalf("missing acquisition outcomes: %s", text)
	}
}

func TestLogTextKeepsUntrustedValuesOnOneLine(t *testing.T) {
	if got := logText("  Title\n\tInjected\r\x1b[31m\u202eText  "); got != "Title Injected [31m Text" {
		t.Fatalf("unexpected sanitized log text: %q", got)
	}
}

func TestErrorSummaryAfterTable(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		results := []listResult{
			{Binding: Binding{Key: "movies", Title: "Movie List", MediaType: "movie"}, Err: errors.New("source unavailable")},
			{Binding: Binding{Key: "tv", Title: "TV\nList", MediaType: "tv"}, Stats: SyncStats{Acquisition: AcquisitionStats{Failures: []error{
				errors.New("tv:tmdb:10: no exact acquisition match"),
				errors.New("tv:tmdb:20: acquisition outcome uncertain\ninspect state"),
			}}}, Err: errors.New("joined errors should not be repeated")},
			{Binding: Binding{Key: "skipped", Title: "Skipped", MediaType: "tv"}, Skipped: true},
			{Binding: Binding{Key: "success", Title: "Success", MediaType: "tv"}},
		}
		var output bytes.Buffer
		writeSummary(&output, results, dryRun)
		text := output.String()
		heading := "Error summary | 3 errors\n"
		position := strings.Index(text, heading)
		if position < strings.Index(text, "TOTAL") || position < 0 {
			t.Fatalf("error summary must follow table: %s", text)
		}
		expected := heading +
			"  - TV | TV List | key=tv | tv:tmdb:10: no exact acquisition match\n" +
			"  - TV | TV List | key=tv | tv:tmdb:20: acquisition outcome uncertain inspect state\n" +
			"  - Movies | Movie List | key=movies | source unavailable\n"
		if text[position:] != expected {
			t.Fatalf("unexpected error details: %s", text[position:])
		}
		output.Reset()
		writeSummary(&output, results[2:], dryRun)
		if strings.Contains(output.String(), "Error summary") {
			t.Fatalf("empty error summary: %s", output.String())
		}
	}
}
