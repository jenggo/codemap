package b

// Service reuses the receiver name declared by package a, but has no methods in
// common with a.Runner and therefore implements nothing.
type Service struct{}

func (Service) Name() string     { return "b" }
func (Service) Describe() string { return "b.Service" }
