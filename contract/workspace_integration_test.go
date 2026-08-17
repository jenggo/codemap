package contract_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codemap/contract"
	"codemap/query"
	"codemap/store"
	"codemap/workspace"
)

func copyFixture(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs("../testdata/contract-workspace")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	err = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// indexFixture copies the checked-in fixture, indexes it as a workspace, and
// runs contract analysis. Returns the config, an open store, and the db path.
func indexFixture(t *testing.T) (*workspace.Config, *store.Store, string) {
	t.Helper()
	root := copyFixture(t)
	members := make([]workspace.Member, 0, 4)
	for _, m := range []string{"repo-a", "repo-b", "repo-c", "repo-d"} {
		members = append(members, workspace.Member{Module: m, Dir: filepath.Join(root, m)})
	}
	cfg := &workspace.Config{Root: root, Members: members}
	dbPath := filepath.Join(t.TempDir(), "codemap.db")
	if _, err := workspace.IndexAll(cfg, dbPath); err != nil {
		t.Fatalf("indexing workspace: %v", err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	stale, err := contract.ReanalyzeStaleContracts(st, contract.DefaultConfig())
	if err != nil {
		t.Fatalf("contract analysis: %v", err)
	}
	if len(stale) == 0 {
		t.Fatal("expected contract analysis to run after a fresh workspace index")
	}
	return cfg, st, dbPath
}

func findContract(t *testing.T, st *store.Store, from, to string) *store.Contract {
	t.Helper()
	contracts, err := st.QueryContracts(store.ContractFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range contracts {
		if contracts[i].FromRef == from && contracts[i].ToRef == to {
			return &contracts[i]
		}
	}
	return nil
}

func TestContractExtractionMatching(t *testing.T) {
	_, st, _ := indexFixture(t)

	c := findContract(t, st, "repo-a/types.MsgTypeAgentAsk", "repo-b/types.MsgTypeAgentAsk")
	if c == nil {
		t.Fatal("missing Signal A+B contract edge between repo-a and repo-b")
	}
	if c.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 (constant 0.6 + shape 0.4)", c.Confidence)
	}
	if c.Suggested {
		t.Error("full-signal edge must not be suggested")
	}
	if c.Direction != store.ContractDirectionProducer {
		t.Errorf("direction = %q, want producer (encode site → decode site)", c.Direction)
	}
	if c.Severity != store.ContractSeverityCompatible {
		t.Errorf("severity = %q, want compatible for identical shapes", c.Severity)
	}
}

func TestSuggestedShapePairs(t *testing.T) {
	_, st, _ := indexFixture(t)

	expect := [][2]string{
		{"repo-a/types.AgentAsk", "repo-d/types.Profile"},
		{"repo-b/types.AgentAsk", "repo-d/types.Profile"},
	}
	for _, pair := range expect {
		c := findContract(t, st, pair[0], pair[1])
		if c == nil {
			t.Errorf("missing suggested edge %s → %s", pair[0], pair[1])
			continue
		}
		if !c.Suggested {
			t.Errorf("edge %s → %s should be suggested (no shared constant)", pair[0], pair[1])
		}
		if c.Confidence != 0.4 {
			t.Errorf("confidence = %v, want 0.4 (shape signal only)", c.Confidence)
		}
	}
}

func TestConstantDrift(t *testing.T) {
	_, st, _ := indexFixture(t)

	// repo-c's MsgTypeAgentAsk has value "agent_ans" vs "agent_ask"
	// (same name, different value) → breaking constant drift, no edge.
	drifts, err := st.QueryDrift(store.DriftSeverityBreaking, "")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, d := range drifts {
		if d.FromRef == "repo-a/types.MsgTypeAgentAsk" && d.ToRef == "repo-c/types.MsgTypeAgentAsk" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected breaking constant drift between repo-a and repo-c")
	}

	if c := findContract(t, st, "repo-a/types.MsgTypeAgentAsk", "repo-c/types.MsgTypeAgentAsk"); c != nil {
		t.Error("value mismatch must not create a match edge")
	}
}

func TestWireNameAwareMatching(t *testing.T) {
	// repo-a uses Host with cbor:"host"; repo-b uses Hostname with cbor:"host".
	// They must match by effective wire name with no breaking drift.
	_, st, _ := indexFixture(t)
	c := findContract(t, st, "repo-a/types.MsgTypeAgentAsk", "repo-b/types.MsgTypeAgentAsk")
	if c == nil {
		t.Fatal("missing a↔b edge")
	}
	if c.Severity != store.ContractSeverityCompatible {
		t.Errorf("severity = %q, want compatible (cbor tag 'host' matches on both sides)", c.Severity)
	}
}

func TestRuntimeContracts(t *testing.T) {
	_, st, _ := indexFixture(t)

	contracts, err := st.QueryRuntimeContracts("", "")
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		kind, pattern, repo, dir string
	}
	wants := []want{
		{string(store.RuntimeContractRedis), "krucil_size:{date}:{ip}", "repo-a", string(store.ContractDirectionProducer)},
		{string(store.RuntimeContractJetStream), "agent.response.:request_id", "repo-b", string(store.ContractDirectionConsumer)},
		{string(store.RuntimeContractJetStream), "AGENT", "repo-b", string(store.ContractDirectionProducer)},
		{string(store.RuntimeContractWSType), "agent_ask", "repo-a", string(store.ContractDirectionShared)},
	}
	for _, w := range wants {
		if !hasRuntimeContract(contracts, w.kind, w.pattern, w.repo, w.dir) {
			t.Errorf("missing runtime contract %s %q in %s (%s)", w.kind, w.pattern, w.repo, w.dir)
		}
	}

	// Call-site context guard: the log lookalike "krucil_size:99" must not appear.
	if hasRuntimeContract(contracts, string(store.RuntimeContractRedis), "krucil_size:{date}", "repo-a", "") {
		t.Error("log lookalike was captured as a runtime contract")
	}
}

func hasRuntimeContract(contracts []store.RuntimeContract, kind, pattern, repo, dir string) bool {
	for _, c := range contracts {
		if string(c.Kind) == kind && c.Pattern == pattern &&
			(repo == "" || c.Repo == repo) && (dir == "" || string(c.Direction) == dir) {
			return true
		}
	}
	return false
}

func TestSuppression(t *testing.T) {
	_, st, _ := indexFixture(t)

	if err := st.SuppressContracts("repo-a/types.MsgTypeAgentAsk", "repo-b/types.MsgTypeAgentAsk"); err != nil {
		t.Fatal(err)
	}
	if c := findContract(t, st, "repo-a/types.MsgTypeAgentAsk", "repo-b/types.MsgTypeAgentAsk"); c != nil {
		t.Error("suppressed pair must not appear in contracts")
	}

	// Suppression must also remove config-declared pairs at analysis time.
	cfg := contract.DefaultConfig()
	cfg.SuppressPairs = map[string]bool{"repo-a/types.MsgTypeAgentAsk → repo-b/types.MsgTypeAgentAsk": true}
	contracts, _, _, err := contract.RunAnalysis(st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range contracts {
		if c.FromRef == "repo-a/types.MsgTypeAgentAsk" && c.ToRef == "repo-b/types.MsgTypeAgentAsk" {
			t.Error("config-suppressed pair must not be produced by analysis")
		}
	}
}

func TestBlastRadiusAndFindPathTraverseContracts(t *testing.T) {
	_, st, _ := indexFixture(t)

	plain, err := query.GetBlastRadius(st, "repo-a/types.MsgTypeAgentAsk", 2)
	if err != nil {
		t.Fatal(err)
	}
	withContracts, err := query.BlastRadiusWithContracts(st, "repo-a/types.MsgTypeAgentAsk", 2)
	if err != nil {
		t.Fatal(err)
	}
	if withContracts.DirectCallers <= plain.DirectCallers {
		t.Errorf("blast radius must count the krucil consumer; plain=%d withContracts=%d",
			plain.DirectCallers, withContracts.DirectCallers)
	}

	path, err := query.FindPath(st, "repo-a/types.MsgTypeAgentAsk", "repo-b/types.MsgTypeAgentAsk", 5)
	if err != nil {
		t.Fatalf("find_path across a contract edge: %v", err)
	}
	if len(path) == 0 {
		t.Fatal("find_path returned an empty path")
	}
	if path[len(path)-1].EdgeType != "contract" {
		t.Errorf("expected a contract step in the path, got edge types %v",
			pathTypes(path))
	}
}

func pathTypes(path []query.PathStep) []string {
	out := make([]string, len(path))
	for i, step := range path {
		out[i] = step.EdgeType
	}
	return out
}

func TestStalenessReanalysisShowsUpdatedDrift(t *testing.T) {
	cfg, st, dbPath := indexFixture(t)

	// Edit repo-b's AgentAsk struct: Timeout int → string.
	repoBDir := ""
	for _, m := range cfg.Members {
		if m.Module == "repo-b" {
			repoBDir = m.Dir
			break
		}
	}
	msgPath := filepath.Join(repoBDir, "types", "messages.go")
	data, err := os.ReadFile(msgPath)
	if err != nil {
		t.Fatal(err)
	}
	content := strings.ReplaceAll(string(data), "Timeout  int    `cbor:\"timeout\"`", "Timeout  string `cbor:\"timeout\"`")
	if content == string(data) {
		t.Fatal("fixture edit did not apply; check the field literal")
	}
	if err := os.WriteFile(msgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := workspace.ReindexRepo(dbPath, "repo-b", repoBDir); err != nil {
		t.Fatalf("reindexing repo-b: %v", err)
	}

	stale, err := st.IsContractStale("repo-b")
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("repo-b contract data must be stale after its symbol index advanced")
	}

	reanalyzed, err := contract.ReanalyzeStaleContracts(st, contract.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(strings.Join(reanalyzed, ","), "repo-b") {
		t.Errorf("reanalyzed = %v, want [repo-b]", reanalyzed)
	}

	c := findContract(t, st, "repo-a/types.MsgTypeAgentAsk", "repo-b/types.MsgTypeAgentAsk")
	if c == nil {
		t.Fatal("missing a↔b edge after reanalysis")
	}
	if c.Severity != store.ContractSeverityBreaking {
		t.Errorf("severity = %q, want breaking (Timeout type changed on the consumer)", c.Severity)
	}

	drifts, err := st.QueryDrift(store.DriftSeverityBreaking, "")
	if err != nil {
		t.Fatal(err)
	}
	var fieldFound bool
	for _, d := range drifts {
		if d.FromRef != "repo-a/types.MsgTypeAgentAsk" || d.ToRef != "repo-b/types.MsgTypeAgentAsk" {
			continue
		}
		for _, f := range d.Fields {
			if f.WireName == "timeout" && f.Status == "type_changed" {
				fieldFound = true
			}
		}
	}
	if !fieldFound {
		t.Error("expected a type_changed drift entry for field 'timeout'")
	}
}
