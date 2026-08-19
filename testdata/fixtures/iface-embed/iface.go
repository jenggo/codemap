package iface

// Logger is a base interface.
type Logger interface {
	Log(msg string)
}

// Closer is another base interface.
type Closer interface {
	Close() error
}

// ReadWriter embeds Logger and Closer.
type ReadWriter interface {
	Logger
	Closer
	Read(p []byte) (n int, err error)
	Write(p []byte) (n int, err error)
}

// Console implements ReadWriter.
type Console struct{}

func (c *Console) Log(msg string)              {}
func (c *Console) Close() error                { return nil }
func (c *Console) Read(p []byte) (int, error)  { return 0, nil }
func (c *Console) Write(p []byte) (int, error) { return 0, nil }
