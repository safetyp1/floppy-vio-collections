package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type SyncStats struct {
	Items        int
	Matched      int
	Missing      int
	Added        int
	Removed      int
	Deferred     int
	SourceRead   bool
	MatchesKnown bool
	PreviewKnown bool
	Acquisition  AcquisitionStats
}

type listResult struct {
	Binding Binding
	Stats   SyncStats
	Err     error
	Skipped bool
}

func (result listResult) errors() int {
	return len(result.failures())
}

func (result listResult) failures() []error {
	if len(result.Stats.Acquisition.Failures) > 0 {
		return result.Stats.Acquisition.Failures
	}
	if result.Err != nil {
		return []error{result.Err}
	}
	return nil
}

func (service *Service) output() io.Writer {
	if service.Output != nil {
		return service.Output
	}
	return os.Stdout
}

func logText(text string) string {
	return strings.Join(strings.Fields(strings.Map(func(character rune) rune {
		if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
			return ' '
		}
		return character
	}, text)), " ")
}

func logLine(output io.Writer, format string, values ...any) {
	fmt.Fprintf(output, "%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, values...))
}

func mediaLabel(kind string) string {
	if kind == "tv" {
		return "TV"
	}
	return "Movies"
}

func knownCount(value int, known bool) string {
	if !known {
		return "-"
	}
	return strconv.Itoa(value)
}

func (service *Service) logResult(result listResult) {
	output, stats := service.output(), result.Stats
	targetName, operation, provider := "Silo", "searches", "Arr"
	arrName := "Radarr"
	if result.Binding.MediaType == "tv" {
		arrName = "Sonarr"
	}
	if service.Vio != nil {
		targetName, operation, provider, arrName = "Vio", "requests", "Vio", "Vio"
	}
	logLine(output, "  Source: %s items | Matched in %s: %s | Missing: %s", knownCount(stats.Items, stats.SourceRead), targetName, knownCount(stats.Matched, stats.MatchesKnown), knownCount(stats.Missing, stats.MatchesKnown))
	if service.DryRun {
		logLine(output, "  Collection: %s would add | %s would remove", knownCount(stats.Added, stats.PreviewKnown), knownCount(stats.Removed, stats.PreviewKnown))
		logLine(output, "  %s: %s would request (not checked with %s)", arrName, knownCount(stats.Acquisition.Requested, stats.PreviewKnown), provider)
	} else {
		logLine(output, "  Collection: %d added | %d removed", stats.Added, stats.Removed)
		logLine(output, "  %s: %d %s submitted | %d previously requested", arrName, stats.Acquisition.Submitted, operation, stats.Acquisition.Previous)
	}
	if stats.Deferred > 0 {
		logLine(output, "  NOTE: %d removals deferred until the source is stable", stats.Deferred)
	}
	if stats.Acquisition.Disabled > 0 {
		logLine(output, "  NOTE: Acquisition disabled for %d missing items", stats.Acquisition.Disabled)
	}
	for _, failure := range result.failures() {
		logLine(output, "  ERROR: %s", logText(failure.Error()))
	}
}

func writeSummary(output io.Writer, results []listResult, dryRun bool) {
	header := []string{"Type", "List", "Items", "Matched", "Added", "Removed", "Searched", "Errors", "Status"}
	if dryRun {
		header[4], header[5], header[6] = "Would Add", "Would Remove", "Would Request"
	}
	rows := [][]string{header}
	var errorLines []string
	totals := make([]int, 6)
	partial := false
	for _, kind := range []string{"tv", "movie"} {
		for _, result := range results {
			if result.Binding.MediaType != kind {
				continue
			}
			stats := result.Stats
			searches := stats.Acquisition.Submitted
			if dryRun {
				searches = stats.Acquisition.Requested
			}
			values := []int{stats.Items, stats.Matched, stats.Added, stats.Removed, searches, result.errors()}
			known := []bool{stats.SourceRead, stats.MatchesKnown, !dryRun || stats.PreviewKnown, !dryRun || stats.PreviewKnown, !dryRun || stats.PreviewKnown, true}
			row := []string{mediaLabel(kind), logText(result.Binding.Title)}
			for index, value := range values {
				available := known[index] && !result.Skipped
				row = append(row, knownCount(value, available))
				if available {
					totals[index] += value
				} else {
					partial = true
				}
			}
			label := "OK"
			if result.Skipped {
				label = "Not processed"
			} else if result.Err != nil {
				label = "Error"
			} else if dryRun {
				label = "Preview"
			}
			rows = append(rows, append(row, label))
			if !result.Skipped {
				for _, failure := range result.failures() {
					errorLines = append(errorLines, fmt.Sprintf("  - %s | %s | key=%s | %s", mediaLabel(kind), logText(result.Binding.Title), logText(result.Binding.Key), logText(failure.Error())))
				}
			}
		}
	}
	total := []string{"TOTAL", ""}
	if partial {
		total[1] = "(known counts)"
	}
	for _, value := range totals {
		total = append(total, strconv.Itoa(value))
	}
	rows = append(rows, append(total, ""))
	widths := make([]int, len(header))
	for _, row := range rows {
		for index, cell := range row {
			widths[index] = max(widths[index], utf8.RuneCountInString(cell))
		}
	}
	fmt.Fprintln(output)
	for rowIndex, row := range rows {
		if rowIndex == 1 || rowIndex == len(rows)-1 {
			divider := make([]string, len(widths))
			for index, width := range widths {
				divider[index] = strings.Repeat("-", width)
			}
			fmt.Fprintln(output, strings.Join(divider, "  "))
		}
		cells := make([]string, len(row))
		for index, cell := range row {
			if index >= 2 && index <= 7 {
				cells[index] = fmt.Sprintf("%*s", widths[index], cell)
			} else {
				cells[index] = fmt.Sprintf("%-*s", widths[index], cell)
			}
		}
		fmt.Fprintln(output, strings.TrimRight(strings.Join(cells, "  "), " "))
	}
	if partial {
		fmt.Fprintln(output, "- = unavailable or not processed; totals include known counts only.")
	}
	if len(errorLines) > 0 {
		fmt.Fprintf(output, "\nError summary | %d errors\n", len(errorLines))
		for _, line := range errorLines {
			fmt.Fprintln(output, line)
		}
	}
}
