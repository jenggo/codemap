package app

import "example.com/brokendep/broken"

// Dep references the broken dependency so type-checking must import it.
var _ = broken.Bad
