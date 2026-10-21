package a

// Runner is implemented by two types in this package, both named in a way that
// collides with a receiver name declared in package b.
type Runner interface {
	Run()
}

type Service struct{}

func (Service) Run()   {}
func (Service) Stop()  {}
func (Service) Reset() {}

type Single struct{}

func (Single) Run() {}
