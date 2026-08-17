package types

const MsgTypeAgentAsk = "agent_ask"

type AgentAsk struct {
	ID      string `cbor:"id"`
	Content string `cbor:"content"`
	Timeout int    `cbor:"timeout"`
	Host    string `cbor:"host"`
}

// EncodeAgentAsk is the producer side of the wire contract.
func EncodeAgentAsk(m AgentAsk) []byte {
	_ = MsgTypeAgentAsk
	return nil
}
