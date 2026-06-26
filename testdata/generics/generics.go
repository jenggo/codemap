package generics

// Grouped constants for testing grouped var/const detection.
const (
	Pi     = 3.14
	Euler  = 2.71
	Golden = 1.61
)

// Standalone constant for testing ungrouped var/const detection.
const Answer = 42

// Grouped variables for testing grouped var/const detection.
var (
	MaxRetries = 3
	Timeout    = 30
)

// Standalone variable for testing ungrouped var/const detection.
var Counter int

// Server is a generic type.
type Server[T any] struct {
	data T
}

// Get returns the server's data.
func (s *Server[T]) Get() T {
	return s.data
}

// Set assigns new data to the server.
func (s *Server[T]) Set(v T) {
	s.data = v
}

// PlainFunc is a non-method function.
func PlainFunc(x int) int {
	return x + 1
}
