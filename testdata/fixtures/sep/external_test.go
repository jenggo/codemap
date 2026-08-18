package foo_test

import "example.com/sep"

func ServiceConstructor() *foo.Service {
	return foo.New()
}
