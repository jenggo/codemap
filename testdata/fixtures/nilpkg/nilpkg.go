package nilpkg

// This file has a type error that causes conf.Check to fail.
// The resolve pipeline should not panic even when typePkg is nil.

var Broken int = "not an int"

func Helper() string {
	return "ok"
}
