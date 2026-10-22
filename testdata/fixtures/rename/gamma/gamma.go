package gamma

import "renamefixture/alpha"

// CallTarget calls alpha.Target from a third package.
func CallTarget() int {
	return alpha.Target()
}
