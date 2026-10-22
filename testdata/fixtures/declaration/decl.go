package decl

// Gadget is a type with a method, so a position inside a method body can be
// attributed to the method rather than to the receiver type.
type Gadget struct {
	Name string
}

// Describe returns the gadget's name.
func (g Gadget) Describe() string {
	return g.Name
}

// Outer is a function with a body, a nested literal, and a call to a symbol
// declared in another file.
func Outer() string {
	fn := func() string {
		return Helper()
	}
	return fn()
}

// Helper is declared in this file so occurrence lookup resolves locally.
func Helper() string { return "helper" }
