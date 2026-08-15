package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNewEnvelope(t *testing.T) {
	env, err := NewEnvelope("heartbeat", "agent-1", "01J9XKZ8TEST", map[string]int{"seq": 7})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if env.V != ProtocolVersion {
		t.Errorf("v = %d, want %d", env.V, ProtocolVersion)
	}
	if env.Type != "heartbeat" || env.AgentID != "agent-1" || env.MsgID != "01J9XKZ8TEST" {
		t.Errorf("identity fields wrong: %+v", env)
	}
	if env.TS.Location() != time.UTC {
		t.Errorf("ts not UTC: %v", env.TS)
	}
	if time.Since(env.TS) > time.Minute {
		t.Errorf("ts not recent: %v", env.TS)
	}

	// The wire shape is the contract: exact key names per docs/nats-subjects.md.
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"v", "type", "agent_id", "msg_id", "ts", "data"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("wire JSON missing key %q", key)
		}
	}
	if len(decoded) != 6 {
		t.Errorf("wire JSON has %d keys, want 6: %s", len(decoded), raw)
	}
	if string(decoded["data"]) != `{"seq":7}` {
		t.Errorf("data = %s, want {\"seq\":7}", decoded["data"])
	}
	var ts string
	if err := json.Unmarshal(decoded["ts"], &ts); err != nil {
		t.Fatalf("ts decode: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("ts %q is not RFC3339: %v", ts, err)
	}
	if !strings.HasSuffix(ts, "Z") {
		t.Errorf("ts %q is not UTC (no Z suffix)", ts)
	}
}

func TestNewEnvelopeRejectsUnmarshalable(t *testing.T) {
	if _, err := NewEnvelope("x", "a", "m", make(chan int)); err == nil {
		t.Error("want error for unmarshalable data, got nil")
	}
}
