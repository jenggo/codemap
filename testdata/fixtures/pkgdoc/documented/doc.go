// Package documented carries a clause comment so the query layer can surface
// its first sentence.
//
// The rest of the comment is not the first sentence.
package documented

// Exported has its own doc comment that must not be borrowed as the package doc.
func Exported() {}
