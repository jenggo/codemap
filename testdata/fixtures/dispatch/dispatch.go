package dispatch

import "io"

// writer escapes into io.Writer; nothing calls Write directly.
type writer struct{}

func (w *writer) Write(p []byte) (int, error) { return len(p), nil }

type holder struct{ w io.Writer }

func assign() *holder { return &holder{w: &writer{}} }

type reader interface {
	Read(p []byte) (int, error)
}

// called is reached both directly and through an interface value.
type called struct{}

func (c *called) Run() {}

type runner struct{ r interface{ Run() } }

func useCalled() {
	c := &called{}
	c.Run()
	_ = runner{r: c}
}

// plain is a function, so it has no receiver to dispatch through.
func plain() {}

// isolated is never assigned to an interface field or embedded, and nothing
// calls its method, so it has no escaping sites.
type isolated struct{}

func (i *isolated) Standalone() {}
