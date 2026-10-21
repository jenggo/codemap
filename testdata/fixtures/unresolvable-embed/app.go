package app

import "example.com/does-not-exist/foreign"

// Service embeds a type from a module that cannot be loaded, so its method set
// is unknown to go/types.
type Service struct {
	foreign.Base
}

// Unrelated declares methods Service does not have.
type Unrelated interface {
	ValidateKey() error
	UpdateLastUsed() error
}

// ForeignIface embeds a type that cannot be loaded, so its method set is also
// unknown. go/types reports it as Empty, which would make every type satisfy it.
type ForeignIface interface {
	foreign.Base
}

// Impl satisfies ForeignIface only if an unknown method set is treated as
// matching another unknown method set.
type Impl struct{}

func (Impl) ValidateKey() error { return nil }
