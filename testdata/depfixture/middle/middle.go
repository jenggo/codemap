package middle

import "codemap/testdata/depfixture/base"

// Service composes the base Greeter.
type Service struct {
	G *base.Greeter
}

// Run invokes the base Greeter.
func (s *Service) Run() string {
	return s.G.Greet()
}
