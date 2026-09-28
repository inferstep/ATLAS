// Typed event protocol for atlas-proxy.
//
// Mirrors the schema defined in atlas/events.py so the same Python
// consumer (atlas/events.py) can read events from both v3-service
// and atlas-proxy. The envelope shape is documented in docs/PROTOCOL.md.
//
// Architecture: a global pub/sub broker. Producers (agent loop, tool
// dispatch, etc.) call Emit() with an Envelope. Subscribers (SSE clients
// connected to /events) get a buffered channel; slow consumers are
// dropped from individual events rather than blocking producers.
//
// SSE-only transport. Cancellation rides POST /cancel.

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
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

// Legal Type values. Mirror atlas.cli.events.EVENT_TYPES.
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

// ---------------------------------------------------------------------------
// Broker
// ---------------------------------------------------------------------------

const subscriberBuffer = 256 // events buffered per slow subscriber before drops kick in

type broker struct {
	mu          sync.Mutex
	subscribers map[chan Envelope]struct{}
	// draining is set once the process is shutting down. The TUI keeps
	// /events open for the life of the session, so a shutdown that waited for
	// subscribers to leave on their own would wait forever; closing their
	// channels is what lets every handler return. After that no new
	// subscriber is accepted, because the stream it would join is over.
	draining bool
}

var defaultBroker = &broker{
	subscribers: map[chan Envelope]struct{}{},
}

// subscribe returns a channel of envelopes, or nil once the broker is
// draining. A nil channel tells handleEvents there is nothing to serve.
func (b *broker) subscribe() chan Envelope {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.draining {
		return nil
	}
	ch := make(chan Envelope, subscriberBuffer)
	b.subscribers[ch] = struct{}{}
	return ch
}

// unsubscribe closes the channel only while it is still registered, so a
// handler's deferred unsubscribe after a drain is a no-op rather than a second
// close. Registration IS the ownership token for closing.
func (b *broker) unsubscribe(ch chan Envelope) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, registered := b.subscribers[ch]; !registered {
		return
	}
	delete(b.subscribers, ch)
	close(ch)
}

// drain ends every subscription so shutdown is not held open by a stream that
// was never going to end on its own. Each channel is closed exactly once,
// under the lock that owns the registry.
func (b *broker) drain() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.draining = true
	for ch := range b.subscribers {
		delete(b.subscribers, ch)
		close(ch)
	}
}

func (b *broker) isDraining() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.draining
}

func (b *broker) emit(ev Envelope) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subscribers {
		// Non-blocking send. Slow consumers lose events — never block
		// the agent loop on a wedged TUI.
		select {
		case ch <- ev:
		default:
		}
	}
}

// Emit publishes an envelope to all subscribers. Safe from any goroutine.
func Emit(ev Envelope) {
	defaultBroker.emit(ev)
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

// handleEvents serves a continuous SSE stream of envelope events. Closes
// when the client disconnects (Request.Context().Done()).
func handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, ErrInternal, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	// Flush the headers immediately. Without this, Go buffers the
	// response until the first body write — meaning the client sees no
	// HTTP/200 until the first event or the 15s heartbeat fires, and
	// any consumer using a connect-then-wait timeout shorter than 15s
	// reads "no response". This was caught by integration test, not
	// unit test.
	flusher.Flush()
	// Send a one-shot SSE comment so the client also gets a body byte
	// immediately — some HTTP middlewares only consider the response
	// "started" after the first body chunk, not after headers alone.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch := defaultBroker.subscribe()
	if ch == nil {
		return // draining: the stream this would join is already over
	}
	defer defaultBroker.unsubscribe(ch)

	// Heartbeat ticker — keeps proxies / load balancers from idling out
	// the connection during quiet stretches in the agent loop.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			line, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", line); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
