package decl

import (
	"declfixture/other"
	"declfixture/other2"
)

// UsesImported references symbols declared in other packages, so occurrence
// lookup must follow the index rather than the file.
func UsesImported() int {
	return other.Imported() + other2.Imported()
}
