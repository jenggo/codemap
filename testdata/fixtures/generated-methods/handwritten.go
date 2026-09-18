package genmethods

// Server is hand-written. Its Handle method shares name, exported state, kind,
// and match tier with the generated AutoHandler.Handle, so provenance is the
// only thing that can decide their ranking.
type Server struct{}

func (Server) Handle() {}
