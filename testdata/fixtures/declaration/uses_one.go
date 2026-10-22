package decl

import "declfixture/other"

// UsesOneImported references exactly one imported symbol, so the bare name
// resolves unambiguously.
func UsesOneImported() int {
	return other.Imported()
}
