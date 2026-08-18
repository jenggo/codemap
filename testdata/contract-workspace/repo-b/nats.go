package types

import "github.com/nats-io/nats.go"

// jetStream is satisfied by the NATS client in the real workspace.
type jetStream interface {
	Subscribe(subject string, cb func())
}

// StreamConfig names a JetStream stream.
type StreamConfig struct {
	Name string
}

// AddStream creates the agent stream.
func AddStream(js interface{ AddStream(string) error }) error {
	return js.AddStream(StreamConfig{Name: "AGENT"}.Name)
}

// Listen subscribes to per-request agent responses.
func Listen(js jetStream) {
	js.Subscribe("agent.response.<request_id>", func() {})
}

var _ = nats.Options{}
