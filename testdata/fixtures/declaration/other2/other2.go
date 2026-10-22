package other2

// Imported shares its name with other.Imported, so a file referencing both
// packages makes the bare name ambiguous.
func Imported() int { return 2 }
