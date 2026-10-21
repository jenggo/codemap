package svc

import "net/http"

// Handler aliases a type outside the index. The alias is the only indexed
// symbol for it, so edges must stay attached to this name.
type Handler = http.Handler

// MyHandler implements the aliased interface.
type MyHandler struct{}

func (MyHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}
