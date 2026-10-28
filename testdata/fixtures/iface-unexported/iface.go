package iface

// task is an internal constraint interface: its only method is unexported, so
// the extractor must still index it or the declaration is invisible.
type task interface {
	getIDs() (requestID, server)
}

// Store mixes an exported and an unexported method, so an implementation
// report can be checked against a partial-visibility interface.
type Store interface {
	Save() error
	reset()
}

// Number is a union-only constraint with no methods at all.
type Number interface {
	int | int64
}

// Payload unions two named types, so the type set is discoverable only if the
// union terms produce reference edges.
type Payload interface {
	Job | Cache
}

type requestID string

type server string

// Job implements task and has extra methods the interface does not declare.
type Job struct{}

func (Job) getIDs() (requestID, server) { return requestID(""), server("") }
func (Job) Run()                        {}
func (Job) Reset()                      {}

// Cache implements Store and also has a method Store does not declare.
type Cache struct{}

func (Cache) Save() error { return nil }
func (Cache) reset()      {}
func (Cache) Flush()      {}

// Deliver calls through the constraint interface, producing a call edge whose
// target must resolve to the indexed interface method. It is exported so the
// call path is live: an unexported, uncalled root would make the implementer
// look unreachable regardless of the dispatch.
func Deliver[T task](t T) (requestID, server) {
	return t.getIDs()
}

// Sum is a generic function over the union-only constraint.
func Sum[T Number](a, b T) T { return a + b }
