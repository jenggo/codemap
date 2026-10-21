package store

import (
	"path/filepath"
	"testing"
)

func TestMethodsByQualifiedReceiver(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	ctx := t.Context()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	exec(`INSERT INTO repos (id, module_path, dir) VALUES (1, 'example.com/a', '/a')`)
	exec(`INSERT INTO packages (id, path, name, dir, repo_id) VALUES
		(1, 'example.com/a', 'a', '/a', 1),
		(2, 'example.com/b', 'b', '/b', 1),
		(3, 'example.com/a/query_tokens', 'query_tokens', '/a/query_tokens', 1)`)
	exec(`INSERT INTO symbols (id, qualified_name, package_id, name, kind, receiver, signature, doc, pos_file, pos_line, exported, is_test, repo_id) VALUES
		(1, 'example.com/a.Service.Run', 1, 'Run', 'method', 'Service', 'func ()', '', 'a.go', 10, 1, 0, 1),
		(2, 'example.com/a.Service.Stop', 1, 'Stop', 'method', 'Service', 'func ()', '', 'a.go', 20, 1, 0, 1),
		(3, 'example.com/b.Service.Run', 2, 'Run', 'method', 'Service', 'func ()', '', 'b.go', 10, 1, 0, 1),
		(4, 'example.com/a/query_tokens.Service.Query', 3, 'Query', 'method', 'Service', 'func ()', '', 'q.go', 10, 1, 0, 1),
		(5, 'example.com/a/query_tokens.Service.Ping', 3, 'Ping', 'method', 'Service', 'func ()', '', 'q.go', 20, 1, 1, 1),
		(6, 'example.com/a.Service', 1, 'Service', 'type', '', 'struct{}', '', 'a.go', 5, 1, 0, 1)`)

	got, err := s.MethodsByQualifiedReceiver("example.com/a.Service", false)
	if err != nil {
		t.Fatalf("MethodsByQualifiedReceiver: %v", err)
	}
	if names := symbolNames(got); len(names) != 2 || names[0] != "example.com/a.Service.Run" || names[1] != "example.com/a.Service.Stop" {
		t.Fatalf("example.com/a.Service methods = %v, want only a.Service.Run and a.Service.Stop", names)
	}

	got, err = s.MethodsByQualifiedReceiver("example.com/b.Service", false)
	if err != nil {
		t.Fatalf("MethodsByQualifiedReceiver: %v", err)
	}
	if names := symbolNames(got); len(names) != 1 || names[0] != "example.com/b.Service.Run" {
		t.Fatalf("example.com/b.Service methods = %v, want only b.Service.Run", names)
	}

	got, err = s.MethodsByQualifiedReceiver("example.com/a/query_tokens.Service", false)
	if err != nil {
		t.Fatalf("MethodsByQualifiedReceiver: %v", err)
	}
	if names := symbolNames(got); len(names) != 1 || names[0] != "example.com/a/query_tokens.Service.Query" {
		t.Fatalf("query_tokens.Service methods = %v, want only Query (underscore matched literally, test method excluded)", names)
	}

	got, err = s.MethodsByQualifiedReceiver("example.com/a/query_tokens.Service", true)
	if err != nil {
		t.Fatalf("MethodsByQualifiedReceiver: %v", err)
	}
	if names := symbolNames(got); len(names) != 2 {
		t.Fatalf("query_tokens.Service methods with tests = %v, want Query and Ping", names)
	}

	got, err = s.MethodsByQualifiedReceiver("example.com/a.Missing", false)
	if err != nil {
		t.Fatalf("unknown type: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("unknown type should return non-nil empty slice, got %v", got)
	}
}

func symbolNames(syms []Symbol) []string {
	names := make([]string, 0, len(syms))
	for _, sym := range syms {
		names = append(names, sym.QualifiedName)
	}
	return names
}
