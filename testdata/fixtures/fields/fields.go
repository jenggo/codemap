package fields

import "example.com/fields/remote"

// Tagged carries a mix of tagged, untagged, and embedded fields.
type Tagged struct {
	UserID  string `json:"user_id"`
	Name    string `json:"name,omitempty"`
	Both    string `cbor:"cbor_name" json:"json_name"`
	Skipped string `json:"-"`
	Plain   int
	Embedded
}

// Embedded is embedded by Tagged.
type Embedded struct {
	Value string
}

// Empty has no fields.
type Empty struct{}

// Local is aliased by LocalAlias.
type Local struct {
	X int
}

// LocalAlias targets a same-package struct.
type LocalAlias = Local

// RemoteAlias targets a cross-package struct.
type RemoteAlias = remote.Remote

// Greeter is an interface, not a struct.
type Greeter interface {
	Greet() string
}

// C is a const.
const C = 1

// V is a var.
var V = 2

// F is a function.
func F() {}

// Method on Tagged, so `show` can be checked for a method kind.
func (t Tagged) Method() string { return t.UserID }
