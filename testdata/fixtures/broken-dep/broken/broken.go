package broken

// Bad fails type-checking on purpose: a string constant cannot be an int.
var Bad int = "not an int"
