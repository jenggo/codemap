package other

// Imported is declared in a second package so occurrence lookup can resolve a
// name this file references but does not declare.
func Imported() int { return 1 }
