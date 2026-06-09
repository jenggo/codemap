package pkgA

import "codemap/testdata/multipackage/pkgB"

// ServiceA provides service A functionality.
type ServiceA struct{}

// NewServiceA creates a new ServiceA.
func NewServiceA() *ServiceA {
	return &ServiceA{}
}

// DoSomething uses ServiceB internally.
func (s *ServiceA) DoSomething() string {
	b := pkgB.NewServiceB()
	return b.DoWork()
}

// HelperA is a helper function in package A.
func HelperA() string {
	return "helper-a"
}
