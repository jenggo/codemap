package pkgB

// ServiceB provides service B functionality.
type ServiceB struct{}

// NewServiceB creates a new ServiceB.
func NewServiceB() *ServiceB {
	return &ServiceB{}
}

// DoWork performs some work.
func (s *ServiceB) DoWork() string {
	return "work-done"
}

// HelperB is a helper function in package B.
func HelperB() string {
	return "helper-b"
}
