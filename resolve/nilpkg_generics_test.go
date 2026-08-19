package resolve

import (
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
)

// TestNilPackageDoesNotPanic verifies that resolve.Run does not panic when
// conf.Check returns nil for a package that fails type-checking. The nil
// guard on resolveInterfaceSatisfaction/resolveStructEmbedding must fire.
func TestNilPackageDoesNotPanic(t *testing.T) {
	pr, err := parse.Run(filepath.Join("..", "testdata", "fixtures", "nilpkg"))
	if err != nil {
		t.Fatal(err)
	}
	// Should not panic even though the package has a type error.
	res := Run(pr)

	// The package should still produce extracted symbols despite the type error.
	found := false
	for _, sym := range res.Symbols {
		if strings.Contains(sym.Symbol.QualifiedName, "nilpkg.Helper") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected nilpkg.Helper symbol to be extracted despite type error")
	}

	// Should have a warning about the type error.
	foundWarn := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "cannot use") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Fatalf("expected type error warning, got: %v", res.Warnings)
	}
}

// TestGenericMultiArgRendering verifies that multi-type-parameter generics
// (IndexListExpr) render correctly in typeString, exprString, and recvTypeString
// instead of falling back to "" or "?".
func TestGenericMultiArgRendering(t *testing.T) {
	pr, err := parse.Run(filepath.Join("..", "testdata", "fixtures", "generics-multi"))
	if err != nil {
		t.Fatal(err)
	}
	res := Run(pr)

	// Pair[A, B] is a generic struct; its type symbol signature should not be
	// empty (generics no longer degrade to "" or "?").
	var foundPair bool
	for _, sym := range res.Symbols {
		if strings.Contains(sym.Symbol.Name, "Pair") && sym.Symbol.Kind == "type" {
			foundPair = true
			if sym.Symbol.Signature == "" {
				t.Errorf("Pair signature is empty, expected it to contain the type name")
			}
			break
		}
	}
	if !foundPair {
		t.Error("expected Pair type symbol")
	}

	// Mapper[K, V] should appear as an interface.
	var foundMapper bool
	for _, sym := range res.Symbols {
		if strings.Contains(sym.Symbol.Name, "Mapper") && sym.Symbol.Kind == "interface" {
			foundMapper = true
			break
		}
	}
	if !foundMapper {
		t.Error("expected Mapper interface symbol")
	}

	// MakePair[A, B] should be a function symbol whose signature renders the
	// type arguments (Pair[A, B]) instead of degrading to "" or "?".
	var foundMakePair bool
	for _, sym := range res.Symbols {
		if strings.Contains(sym.Symbol.Name, "MakePair") && sym.Symbol.Kind == "function" {
			foundMakePair = true
			if !strings.Contains(sym.Symbol.Signature, "Pair[A, B]") {
				t.Errorf("MakePair signature %q should render type args as Pair[A, B]", sym.Symbol.Signature)
			}
			break
		}
	}
	if !foundMakePair {
		t.Error("expected MakePair function symbol")
	}
}
