package mv

type T struct{ x int }

func (T) M(v int) int { return v }

func do(fn func(int) int) int { return fn(0) }

func Caller() int {
	t := T{}
	f := t.M      // method value → references
	_ = T.M(t, 1) // method expression call → calls
	_ = do(t.M)   // method value as callback → references
	return f(0)
}
