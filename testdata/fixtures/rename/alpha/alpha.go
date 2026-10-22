package alpha

// Greeting is a package-level const used as a rename-collision target.
const Greeting = "hello"

// Widget is the type renamed by the type test.
type Widget struct {
	Name string
}

// Describe is the method renamed by the method test.
func (w Widget) Describe() string { return w.Name }

// Label already exists on Widget, so renaming Describe to Label must refuse.
func (w Widget) Label() string { return w.Name }

// Target is called from three packages.
func Target() int { return 0 }

// SameLine calls Target twice on one line.
func SameLine() int {
	return Target() + Target()
}

// ShadowTarget is the symbol whose shadowed call site forces the refusal.
func ShadowTarget() int { return 0 }

// Shadowed declares a local ShadowTarget in an inner block and calls the real
// one, so the conservative shadowing check cannot tell them apart.
func Shadowed() int {
	{
		ShadowTarget := 1
		_ = ShadowTarget
	}
	return ShadowTarget()
}

// BuildWidget uses Widget in a signature and a composite literal.
func BuildWidget(name string) Widget {
	return Widget{Name: name}
}
