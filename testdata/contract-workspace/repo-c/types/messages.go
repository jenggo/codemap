package types

// MsgTypeAgentAsk is a divergent wire message type (different value).
const MsgTypeAgentAsk = "agent_ans"

// AgentAsk is a structurally similar but unrelated struct.
type AgentAsk struct {
	ID      string `cbor:"id"`
	Content string `cbor:"content"`
}
