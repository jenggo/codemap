package query_test

import (
	"path/filepath"
	"testing"

	"codemap/query"
	"codemap/store"
)

func setupDispatchStore(t *testing.T) *store.Store {
	t.Helper()
	abs, err := filepath.Abs("../testdata/fixtures/dispatch")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return setupFixtureStore(t, abs)
}

// TestCallersOfReportsInterfaceDispatch pins that a method reached only through
// an interface-typed field is not reported as having zero callers: the escape
// site is named, marked dispatch, and points at the receiver type.
func TestCallersOfReportsInterfaceDispatch(t *testing.T) {
	s := setupDispatchStore(t)

	rows, err := query.CallersOf(s, "example.com/dispatch.writer.Write")
	if err != nil {
		t.Fatalf("CallersOf: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 dispatch row, got %d (%+v)", len(rows), rows)
	}
	row := rows[0]
	if !row.Dispatch {
		t.Error("row is not marked dispatch")
	}
	if row.ToRef != "example.com/dispatch.writer" {
		t.Errorf("to_ref = %q, want the receiver type", row.ToRef)
	}
	if row.FromRef != "example.com/dispatch.assign" {
		t.Errorf("from_ref = %q, want the assigning function", row.FromRef)
	}
	if row.EdgeType != "references" {
		t.Errorf("edge_type = %q, want references", row.EdgeType)
	}
	if row.PosLine != 12 {
		t.Errorf("pos_line = %d, want 12 (the assignment)", row.PosLine)
	}
}

// TestCallersOfDispatchWithDirectCaller pins that a method reached both ways
// reports the calls edge and the dispatch row.
func TestCallersOfDispatchWithDirectCaller(t *testing.T) {
	s := setupDispatchStore(t)

	rows, err := query.CallersOf(s, "example.com/dispatch.called.Run")
	if err != nil {
		t.Fatalf("CallersOf: %v", err)
	}
	var calls, dispatch int
	for _, r := range rows {
		if r.EdgeType == "calls" && r.FromRef == "example.com/dispatch.useCalled" {
			calls++
		}
		if r.Dispatch {
			dispatch++
		}
	}
	if calls != 1 {
		t.Errorf("direct calls rows = %d, want 1 (%+v)", calls, rows)
	}
	if dispatch != 1 {
		t.Errorf("dispatch rows = %d, want 1 (%+v)", dispatch, rows)
	}
}

// TestCallersOfSkipsDispatchForInterfaceMethod pins that an interface method
// gets no dispatch rows: its call sites are ordinary edges already.
func TestCallersOfSkipsDispatchForInterfaceMethod(t *testing.T) {
	s := setupDispatchStore(t)

	rows, err := query.CallersOf(s, "example.com/dispatch.reader.Read")
	if err != nil {
		t.Fatalf("CallersOf: %v", err)
	}
	for _, r := range rows {
		if r.Dispatch {
			t.Errorf("interface method reported a dispatch row: %+v", r)
		}
	}
}

// TestCallersOfSkipsDispatchForFunction pins that a plain function, which has no
// receiver, gets no dispatch rows.
func TestCallersOfSkipsDispatchForFunction(t *testing.T) {
	s := setupDispatchStore(t)

	rows, err := query.CallersOf(s, "example.com/dispatch.plain")
	if err != nil {
		t.Fatalf("CallersOf: %v", err)
	}
	for _, r := range rows {
		if r.Dispatch {
			t.Errorf("function reported a dispatch row: %+v", r)
		}
	}
}

// TestCallersOfNoDispatchForUnescapedReceiver pins the honest-negative case: a
// method whose receiver type is never used as an interface value has no
// escape sites, so the result stays empty rather than inventing a row.
func TestCallersOfNoDispatchForUnescapedReceiver(t *testing.T) {
	s := setupDispatchStore(t)

	rows, err := query.CallersOf(s, "example.com/dispatch.isolated.Standalone")
	if err != nil {
		t.Fatalf("CallersOf: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected no rows, got %+v", rows)
	}
}

// TestCallersOfDispatchSurvivesEdgeTypeFilter pins that asking for calls only
// still returns the dispatch rows: an empty list would read as "dead method".
func TestCallersOfDispatchSurvivesEdgeTypeFilter(t *testing.T) {
	s := setupDispatchStore(t)

	rows, err := query.CallersOf(s, "example.com/dispatch.writer.Write", query.WithEdgeTypes("calls"))
	if err != nil {
		t.Fatalf("CallersOf: %v", err)
	}
	dispatch := 0
	for _, r := range rows {
		if r.Dispatch {
			dispatch++
		}
	}
	if dispatch != 1 {
		t.Errorf("dispatch rows = %d, want 1 despite the edge_types filter (%+v)", dispatch, rows)
	}
}
