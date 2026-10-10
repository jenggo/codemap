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

// main is unexported, uncalled, and NOT a program entry point: this package's
// clause is util, not main. It must stay reportable as dead code.
func main() {}

func init() {}

// deadHandler is unexported and uncalled; it must be reported.
func deadHandler() {}
