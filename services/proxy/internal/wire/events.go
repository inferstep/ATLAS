package wire

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Envelope is the wire-format event. JSON tags match the Python dataclass
// in atlas/events.py exactly — any change here MUST also be made there.
type Envelope struct {
	EventID    string                 `json:"event_id"`
	Timestamp  float64                `json:"timestamp"`
	Type       string                 `json:"type"`
	Stage      string                 `json:"stage"`
	Payload    map[string]interface{} `json:"payload"`
	DurationMS int64                  `json:"duration_ms,omitempty"`
}

// Legal Type values. Mirror atlas.events.EVENT_TYPES.
const (
	EvtStageStart = "stage_start"
	EvtStageEnd   = "stage_end"
	EvtToolCall   = "tool_call"
	EvtToolResult = "tool_result"
	EvtMetric     = "metric"
	EvtError      = "error"
	EvtDone       = "done"
)

// NewEventID returns a short, log-readable, session-unique id. Same format
// as the Python helper: "evt_" + 8 hex chars.
func NewEventID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// fall back to time-based to never block — collisions are not a
		// correctness concern (event_ids are display/log identifiers)
		return fmt.Sprintf("evt_%08x", time.Now().UnixNano()&0xffffffff)
	}
	return "evt_" + hex.EncodeToString(b[:])
}

// NewEnvelope builds a well-formed envelope with sensible defaults.
func NewEnvelope(typ, stage string, payload map[string]interface{}) Envelope {
	if payload == nil {
		payload = map[string]interface{}{}
	}
	return Envelope{
		EventID:   NewEventID(),
		Timestamp: float64(time.Now().UnixNano()) / 1e9,
		Type:      typ,
		Stage:     stage,
		Payload:   payload,
	}
}
