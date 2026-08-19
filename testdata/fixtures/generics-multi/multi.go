package genericsmulti

// Pair has two type parameters (IndexListExpr in the AST).
type Pair[A, B any] struct {
	First  A
	Second B
}

// MakePair constructs a Pair.
func MakePair[A, B any](a A, b B) Pair[A, B] {
	return Pair[A, B]{First: a, Second: b}
}

// Mapper has two type constraints.
type Mapper[K comparable, V any] interface {
	Get(K) (V, bool)
	Set(K, V)
}
