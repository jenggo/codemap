package interfaces

// Reader defines an interface for reading data.
type Reader interface {
	Read(p []byte) (n int, err error)
}

// Writer defines an interface for writing data.
type Writer interface {
	Write(p []byte) (n int, err error)
}

// Buffer implements both Reader and Writer.
type Buffer struct {
	data []byte
}

// Read implements the Reader interface.
func (b *Buffer) Read(p []byte) (int, error) {
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

// Write implements the Writer interface.
func (b *Buffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

// Process reads from a Reader and writes to a Writer.
func Process(r Reader, w Writer) error {
	buf := make([]byte, 1024)
	n, err := r.Read(buf)
	if err != nil {
		return err
	}
	_, err = w.Write(buf[:n])
	return err
}
