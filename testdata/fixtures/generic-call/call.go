package genericcall

// Identity is called with one type argument.
func Identity[T any](v T) T { return v }

// Zip is called with two type arguments (IndexListExpr).
func Zip[A, B any](a A, b B) {}

// Use drives both generic call forms.
func Use() int {
	Zip[int, string](1, "a")
	return Identity[int](1)
}
