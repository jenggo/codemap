package cgo

/*
#include <stdlib.h>
*/
import "C"

func Allocate() {
	C.malloc(100)
}

func Release() {
	C.free(nil)
}
