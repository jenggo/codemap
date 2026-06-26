package util

// Helper is exported and used by cmd's main.
func Helper() string {
	return "util"
}

// StrayExported is exported but never called.
func StrayExported() string {
	return "stray"
}

func internal() string {
	return "private"
}
