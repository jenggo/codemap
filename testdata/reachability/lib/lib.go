package lib

// Entry is called from main, so it is live.
func Entry() {
	liveHelper()
}

// liveHelper is called by live Entry.
func liveHelper() {}

// orphan is unreachable: unexported, not main/init, called by nothing live.
func orphan() {
	viaOrphan()
}

// viaOrphan is called ONLY by dead orphan. It has an incoming edge, so a
// caller-count heuristic calls it live; reachability from roots does not.
func viaOrphan() {}

// deadLeaf is called by nothing at all.
func deadLeaf() {}

// init runs, so anything it calls is live.
func init() {
	liveHelper()
}
