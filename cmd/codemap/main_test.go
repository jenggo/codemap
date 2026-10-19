package main

import (
	"strings"
	"testing"

	"codemap/query"
	"codemap/render"
	"codemap/store"
)

// applyOpts collapses the built option list onto fresh query options.
func applyOpts(t *testing.T, opts []query.Option) *query.Options {
	t.Helper()
	options := &query.Options{}
	for _, apply := range opts {
		apply(options)
	}
	return options
}

func TestBuildQueryOptsGenerated(t *testing.T) {
	for _, tc := range []struct {
		flag string
		want store.GeneratedFilter
	}{
		{"any", store.GeneratedAny},
		{"exclude", store.GeneratedExclude},
		{"only", store.GeneratedOnly},
	} {
		opts, err := buildQueryOpts(parsedFlags{generated: tc.flag})
		if err != nil {
			t.Fatalf("buildQueryOpts(%q): %v", tc.flag, err)
		}
		if got := applyOpts(t, opts).Generated; got != tc.want {
			t.Errorf("--generated %s: Generated = %q, want %q", tc.flag, got, tc.want)
		}
	}

	// No flag leaves the default in place.
	opts, err := buildQueryOpts(parsedFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if got := applyOpts(t, opts).Generated; got != "" {
		t.Errorf("no --generated: Generated = %q, want the zero value", got)
	}

	if _, err := buildQueryOpts(parsedFlags{generated: "sometimes"}); err == nil {
		t.Fatal("expected an error for an unknown --generated value")
	}
}

// TestParseArgsDropsBooleanFlagsFromPositionals guards a leak that corrupted
// every positional command: handleFlag reported failure for boolean flags, so
// they landed in filteredArgs and displaced the real arguments. The symptom was
// "search-text foo --regex" searching for files matching "--regex".
func TestParseArgsDropsBooleanFlagsFromPositionals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"trailing bool flag", []string{"foo", "--regex"}, []string{"foo"}},
		{"pattern and file pattern", []string{"foo", "vcs", "--regex"}, []string{"foo", "vcs"}},
		{"leading bool flag", []string{"--regex", "foo"}, []string{"foo"}},
		{"value flag keeps its value out of positionals", []string{"foo", "--kind", "function"}, []string{"foo"}},
		{"empty string is skipped", []string{"", "foo"}, []string{"foo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := parseArgs(tc.args, "unused.db")
			if len(got) != len(tc.want) {
				t.Fatalf("parseArgs(%q) positionals = %q, want %q", tc.args, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("parseArgs(%q) positionals = %q, want %q", tc.args, got, tc.want)
				}
			}
		})
	}
}

// TestSummaryDefaultsToText verifies the CLI summary renders prose unless the
// caller asked for a format, while MCP keeps TOON.
func TestSummaryDefaultsToText(t *testing.T) {
	flags, _ := parseArgs([]string{"summary"}, "unused.db")
	if flags.formatSet {
		t.Fatal("summary must not mark the format as explicitly set")
	}
	opts := summaryRenderOpts(flags, nil)
	if len(opts) != 1 {
		t.Fatalf("expected one render option, got %d", len(opts))
	}
	text := render.RenderSummary(&query.SummaryResult{Module: "m", TotalSymbols: 1}, opts...)
	if !strings.Contains(text, "m | 0 packages") {
		t.Errorf("summary without an explicit format must render text, got %q", text)
	}

	jsonFlags, _ := parseArgs([]string{"summary", "--json"}, "unused.db")
	if !jsonFlags.formatSet {
		t.Fatal("--json must mark the format as explicitly set")
	}
}
