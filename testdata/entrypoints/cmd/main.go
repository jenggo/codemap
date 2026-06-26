package main

import "codemap/testdata/entrypoints/util"

// ThisExportedIsCalled is exported and called from main.
func ThisExportedIsCalled() string {
	return "called"
}

// ThisExportedIsNotCalled is exported but never called.
func ThisExportedIsNotCalled() string {
	return "ghost"
}

func main() {
	println(util.Helper())
	println(ThisExportedIsCalled())
}
