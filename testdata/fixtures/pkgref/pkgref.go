package pkgref

// Helper returns a value.
func Helper() int { return 42 }

// Default uses Helper at package level.
var Default = Helper()

// Compute references a type at package level.
type Config struct{ X int }

// Make builds a Config at package level.
var Make = &Config{}
