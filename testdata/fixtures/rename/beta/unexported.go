package beta

import "renamefixture/alpha"

// callInternal references alpha's unexported symbol from another package, which
// Go rejects. The index still records the edge, which is the situation a rename
// must refuse rather than leave unresolved.
func callInternal() int {
	return alpha.internal()
}
