package base

// Greeter provides a base greeting.
type Greeter struct {
	Name string
}

// Greet produces a greeting.
func (g *Greeter) Greet() string {
	return "hello, " + g.Name
}
