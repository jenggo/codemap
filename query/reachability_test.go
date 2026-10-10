package query

import "testing"

// TestUnusedSymbolsReachability pins the difference between "nothing
// references it" and "nothing reachable references it".
//
//   - orphan is unreferenced, so the old caller-count heuristic and
//     reachability agree it is dead.
//   - viaOrphan is called by orphan, so it HAS an incoming edge and the old
//     heuristic treated it as live. It is only reachable through dead code, so
//     reachability-from-roots reports it.
//   - liveHelper is reached from Entry, which main calls, so it stays live.
//   - init runs by itself, so its callees stay live even without a caller.
func TestUnusedSymbolsReachability(t *testing.T) {
	s := setupQueryStoreAt(t, "../testdata/reachability")

	unused, err := UnusedSymbols(s)
	if err != nil {
		t.Fatalf("UnusedSymbols: %v", err)
	}

	got := make(map[string]bool, len(unused))
	for _, u := range unused {
		got[u.QualifiedName] = true
		t.Logf("unused: %s", u.QualifiedName)
	}

	for _, want := range []string{
		"codemap/testdata/reachability/lib.orphan",
		"codemap/testdata/reachability/lib.viaOrphan",
		"codemap/testdata/reachability/lib.deadLeaf",
	} {
		if !got[want] {
			t.Errorf("%s must be reported as unreachable", want)
		}
	}

	for _, notWant := range []string{
		"codemap/testdata/reachability/cmd.main",
		"codemap/testdata/reachability/lib.Entry",
		"codemap/testdata/reachability/lib.liveHelper",
		"codemap/testdata/reachability/lib.init",
	} {
		if got[notWant] {
			t.Errorf("%s is reachable from a live root and must not be reported", notWant)
		}
	}
}
