package alias

// Greeter is an interface.
type Greeter interface {
	Greet() string
}

// Helloer is an alias for Greeter.
type Helloer = Greeter

// Base provides Greet.
type Base struct{}

func (b *Base) Greet() string { return "hello" }

// MyHelloer is an alias for Base.
type MyHelloer = Base

// UseGreeter takes a Greeter — an alias should satisfy this.
func UseGreeter(g Greeter) string {
	return g.Greet()
}

// Container has a field whose type is an alias.
type Container struct {
	Name Helloer
}

// Inner carries fields that Outer aliases.
type Inner struct {
	Name string
	Age  int
}

// Outer aliases Inner and should inherit its field shape.
type Outer = Inner
