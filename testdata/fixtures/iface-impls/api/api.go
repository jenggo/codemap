package api

import "example.com/iface-impls/store"

// Handler is an alias for the store interface.
type Handler = store.Handler

type localHandler struct{}

func (localHandler) Handle() error { return nil }
