package types

const MsgTypeAgentAsk = "agent_ask"

type AgentAsk struct {
	ID       string `cbor:"id"`
	Content  string `cbor:"content"`
	Timeout  int    `cbor:"timeout"`
	Hostname string `cbor:"host"`
}

// DecodeAgentAsk is the consumer side of the wire contract.
func DecodeAgentAsk(b []byte) AgentAsk {
	_ = MsgTypeAgentAsk
	return AgentAsk{}
}
