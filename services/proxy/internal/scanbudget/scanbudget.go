// Package scanbudget is the stated limit on the workspace scans a session
// start runs before its first turn, and the report that says when one is slow.
//
// Part 1 of #365 made a single open unable to wait on a file that is not a
// regular file. A scan over a deep tree can still take arbitrarily long, and
// the drain a shutdown performs is bounded only by the session total (600s).
// So a scan stops at Budget and returns the partial picture it has, and a scan
// that runs past Slow says so instead of leaving the start silent.
//
// It is its own package because the proxy's main package may not gain a file:
// the flat folder holds the agent loop, and a new subject belongs beside the
// other internal packages.
package scanbudget

import (
	"sync/atomic"
	"time"

	"github.com/itigges22/atlas-proxy/internal/wire"
)

const (
	// Budget is how long a start scan may run before it stops and returns
	// what it has. A constant, not an environment switch: the project does
	// not add switches (removed_switches_test.go exists for the ones that
	// were taken out), and #365 asks for a stated time, not a knob.
	Budget = 5 * time.Second
	// Slow is when a running scan admits it is slow. Well inside Budget, so
	// a scan that is merely slow still finishes after reporting.
	Slow = 500 * time.Millisecond
	// Stage is the pipeline stage a slow start scan reports under, the same
	// row the TUI's pipeline pane already renders for other stages.
	Stage = "workspace"
)

// Deadline is the instant a scan that starts now must stop at.
func Deadline() time.Time { return time.Now().Add(Budget) }

// Expired reports whether a scan that began with deadline has run out. A zero
// deadline means no limit, which is what a direct call in a test uses.
func Expired(deadline time.Time) bool {
	return !deadline.IsZero() && time.Now().After(deadline)
}

// Emitter is how a watch reaches the event stream: the caller passes the
// proxy's Emit, so this package holds no global broker of its own.
type Emitter func(wire.Envelope)

// Watch reports one scan that runs longer than slow: stage_start once, at the
// threshold, and stage_end with the duration and the file count when the scan
// ends. A scan that finishes inside the threshold emits nothing, which is the
// ordinary case and must stay silent.
type Watch struct {
	stage   string
	started time.Time
	timer   *time.Timer
	emit    Emitter
	fired   atomic.Bool // set by the timer goroutine before it emits
}

// Begin starts watching a scan of stage. slow is normally Slow; a test passes
// a shorter one so the case does not wait.
func Begin(stage string, slow time.Duration, emit Emitter) *Watch {
	w := &Watch{stage: stage, started: time.Now(), emit: emit}
	w.timer = time.AfterFunc(slow, func() {
		w.fired.Store(true)
		emit(wire.NewEnvelope(wire.EvtStageStart, stage, map[string]interface{}{
			"detail": "scanning the workspace",
			"slow":   true,
		}))
	})
	return w
}

// End closes the watch. files is what the scan read, reported only when the
// scan was slow enough to have said it started.
func (w *Watch) End(files int, truncated bool) {
	if w.timer.Stop() && !w.fired.Load() {
		return // finished inside the threshold: nothing to say
	}
	if !w.fired.Load() {
		return // stopped just before the timer would have fired
	}
	w.emit(wire.Envelope{
		EventID:    wire.NewEventID(),
		Timestamp:  float64(time.Now().UnixNano()) / 1e9,
		Type:       wire.EvtStageEnd,
		Stage:      w.stage,
		DurationMS: time.Since(w.started).Milliseconds(),
		Payload: map[string]interface{}{
			"files":     files,
			"truncated": truncated,
		},
	})
}
