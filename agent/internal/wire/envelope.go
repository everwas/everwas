package wire

import (
	"encoding/json"
	"time"
)

// Envelope wraps every JSON message on the wire. Shell byte streams and chunk
// payloads are raw bytes and do not use it.
type Envelope struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	AgentID string          `json:"agent_id"`
	MsgID   string          `json:"msg_id"`
	TS      time.Time       `json:"ts"`
	Data    json.RawMessage `json:"data"`
}

// NewEnvelope marshals data into a versioned envelope.
func NewEnvelope(msgType, agentID, msgID string, data any) (Envelope, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		V:       ProtocolVersion,
		Type:    msgType,
		AgentID: agentID,
		MsgID:   msgID,
		TS:      time.Now().UTC(),
		Data:    raw,
	}, nil
}
