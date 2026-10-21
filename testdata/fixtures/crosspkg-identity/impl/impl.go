package impl

import "example.com/crosspkg-identity/shared"

// Payload is referenced by iface.Writer's method signature, so the interface
// and the implementer only match when both packages see the same Payload.
type Payload struct {
	ID shared.ID
}

// Writer implements iface.Writer.
type Writer struct{}

func (Writer) Write(p Payload) error { return nil }
