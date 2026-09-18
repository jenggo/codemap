package main

import (
	"testing"

	"codemap/query"
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
