package types

// Profile has the same shape as repo-a/types.AgentAsk but shares no message
// type constant, so it is a false-positive candidate for shape matching.
type Profile struct {
	ID      string `cbor:"id"`
	Content string `cbor:"content"`
	Timeout int    `cbor:"timeout"`
	Host    string `cbor:"host"`
}
