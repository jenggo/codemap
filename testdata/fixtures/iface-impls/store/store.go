package store

// Handler is an exported interface with implementations of every visibility.
type Handler interface {
	Handle() error
}

// Store is an interface implemented by exactly one exported type.
type Store interface {
	Save() error
}

type publicHandler struct{}

func (publicHandler) Handle() error { return nil }

// PublicHandler is an exported implementer, so the alias of Handler in api
// would produce a visible duplicate edge if aliases were not deduped.
type PublicHandler struct{}

func (PublicHandler) Handle() error { return nil }

type secretHandler struct{}

func (secretHandler) Handle() error { return nil }

// PublicStore is the only exported implementer in this package.
type PublicStore struct{}

func (PublicStore) Save() error { return nil }
