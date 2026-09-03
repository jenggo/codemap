// Package opt is the single implementation of the functional-options shape
// previously duplicated across the query and render packages: an Option
// type, constructors derived from setters, and an apply loop.
package opt

// Option mutates a target value of type T.
type Option[T any] func(*T)

// New derives an option constructor from a setter. Both T and V are
// inferred from the setter's signature, so call sites need no explicit type
// arguments:
//
//	var WithRepo = opt.New(func(o *Options, repo string) { o.Repo = repo })
//
// The result type is func(V) Option[T], matching the With* constructor
// signature the packages previously declared by hand.
func New[T, V any](set func(*T, V)) func(V) Option[T] {
	return func(v V) Option[T] {
		return func(t *T) { set(t, v) }
	}
}

// Apply runs opts against target in order.
func Apply[T any](target *T, opts []Option[T]) {
	for _, o := range opts {
		o(target)
	}
}
