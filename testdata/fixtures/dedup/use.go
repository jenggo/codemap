package samepair

func Caller() int {
	svc := NewService() // package-function call
	a := svc.Get()      // method call
	f := svc.Get        // method value
	_ = f()
	_ = svc.x    // field access
	_ = Global() // var function call
	return a
}
