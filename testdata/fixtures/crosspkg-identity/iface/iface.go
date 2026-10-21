package iface

import "example.com/crosspkg-identity/impl"

// Writer is implemented by impl.Writer across a package boundary.
type Writer interface {
	Write(p impl.Payload) error
}
