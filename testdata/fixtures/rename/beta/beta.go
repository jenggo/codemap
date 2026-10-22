package beta

import "renamefixture/alpha"

// CallTarget calls alpha.Target from another package.
func CallTarget() int {
	return alpha.Target()
}

// TwoOnOneLine calls alpha.Target twice on one line.
func TwoOnOneLine() int {
	return alpha.Target() + alpha.Target()
}

// UseWidget uses the renamed type in a signature and a composite literal.
func UseWidget() alpha.Widget {
	w := alpha.Widget{Name: "beta"}
	return w
}
