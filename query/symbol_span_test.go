package query

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSymbolSpan(t *testing.T) {
	abs, err := filepath.Abs("../testdata/generics")
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	file := filepath.Join(abs, "generics.go")
	data := readFile(t, file)

	tests := []struct {
		name          string
		posLine       int
		qn            string
		kind          string
		wantSubstr    string
		wantGrouped   bool
		wantMembers   int
		wantDocOffset bool
	}{
		{
			name:          "function body",
			posLine:       38,
			qn:            "PlainFunc",
			kind:          "function",
			wantSubstr:    "func PlainFunc(x int) int {",
			wantGrouped:   false,
			wantDocOffset: true,
		},
		{
			name:          "method on generic receiver",
			posLine:       28,
			qn:            "Get",
			kind:          "method",
			wantSubstr:    "func (s *Server[T]) Get() T {",
			wantGrouped:   false,
			wantDocOffset: true,
		},
		{
			name:          "struct type",
			posLine:       23,
			qn:            "Server",
			kind:          "type",
			wantSubstr:    "type Server[T any] struct {",
			wantGrouped:   false,
			wantDocOffset: true,
		},
		{
			name:          "standalone const",
			posLine:       11,
			qn:            "Answer",
			kind:          "const",
			wantSubstr:    "const Answer = 42",
			wantGrouped:   false,
			wantDocOffset: true,
		},
		{
			name:          "standalone var",
			posLine:       20,
			qn:            "Counter",
			kind:          "var",
			wantSubstr:    "var Counter int",
			wantGrouped:   false,
			wantDocOffset: false,
		},
		{
			name:          "grouped const",
			posLine:       5,
			qn:            "Pi",
			kind:          "const",
			wantSubstr:    "const (",
			wantGrouped:   true,
			wantMembers:   3,
			wantDocOffset: true,
		},
		{
			name:          "grouped var",
			posLine:       15,
			qn:            "MaxRetries",
			kind:          "var",
			wantSubstr:    "var (",
			wantGrouped:   true,
			wantMembers:   2,
			wantDocOffset: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, end, docOff, grouped, members, err := symbolSpan(file, tc.posLine, tc.qn, tc.kind)
		if err != nil {
			t.Fatalf("symbolSpan: %v", err)
		}
			if start < 0 || end <= start || end > len(data) {
				t.Fatalf("offsets out of range: start=%d end=%d len=%d", start, end, len(data))
			}
			body := string(data[start:end])
			if !strings.Contains(body, tc.wantSubstr) {
				t.Errorf("body does not contain %q\nbody: %s", tc.wantSubstr, body)
			}
			if grouped != tc.wantGrouped {
				t.Errorf("partOfGroup: got %v want %v", grouped, tc.wantGrouped)
			}
			if grouped && members != tc.wantMembers {
				t.Errorf("groupMembers: got %d want %d", members, tc.wantMembers)
			}
			if tc.wantDocOffset && docOff < 0 {
				t.Errorf("expected doc offset, got %d", docOff)
			}
		})
	}
}

func TestSymbolSpanNotFound(t *testing.T) {
	abs, _ := filepath.Abs("../testdata/generics")
	file := filepath.Join(abs, "generics.go")
	_, _, _, _, _, err := symbolSpan(file, 1, "NonExistent", "function")
	if err == nil {
		t.Fatal("expected error for non-existent symbol")
	}
}

func TestSymbolSpanFileMissing(t *testing.T) {
	_, _, _, _, _, err := symbolSpan("/nonexistent/file.go", 1, "X", "function")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestSymbolSpanInterfaceType(t *testing.T) {
	abs, _ := filepath.Abs("../testdata/interfaces")
	file := filepath.Join(abs, "interfaces.go")
	data := readFile(t, file)
	start, end, _, _, _, err := symbolSpan(file, 4, "Reader", "type")
	if err != nil {
		t.Fatalf("symbolSpan: %v", err)
	}
	body := string(data[start:end])
	if !strings.Contains(body, "type Reader interface") {
		t.Errorf("body does not contain type Reader interface\nbody: %s", body)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	d, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readFile %s: %v", path, err)
	}
	return d
}
