package shadowcapture

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Private shadow capture.
//
// ATLAS still works out what the user demanded by reading their English, and
// the client now declares it structurally. Nothing compares the two. This sink
// exists so a later corpus can measure how often they disagree and why -- and
// nothing else: no record it writes is read by any decision, and none reaches a
// wire. It is off unless an operator names a capture file.
//
// Deliberately not /events: that stream is a documented public contract with a
// permanently connected TUI subscriber and no per-session filter, so anything
// emitted there would be both a schema expansion and a disclosure. Deliberately
// not the lens corpus either -- that is training data.

// QueueDepth is how many records the sink holds before it drops. It bounds
// what a wedged or slow disk can cost. A full queue
// drops and counts rather than pushing back on an agent request: a diagnostic
// that can stall a user's run is worse than a diagnostic with a hole in it.
const QueueDepth = 1024

// maxTrackedRequests bounds duplicate detection. Beyond it, new ids stop
// being remembered and the footer says so, rather than growing without limit.
const maxTrackedRequests = 100000

// schemaVersionFooter is the footer's own schema. Each record kind versions
// independently; the proxy declares the others in its task_intake.go.
const schemaVersionFooter = 1

// A sink is open, then closing, then closed, and submission is synchronised
// with that transition rather than merely checking it.
//
// Checking a flag and then sending on the queue cannot be made safe by making
// the flag atomic: the close can land between the check and the send, and a
// send on a closed channel panics -- inside an agent request, which is the one
// thing a diagnostic must never be able to do. So admission is a read lock held
// across the decision AND the enqueue, and the cutoff takes the same lock for
// writing. When the queue closes, no submitter is inside it and no submitter
// can enter and find it open. panic/recover is not used as synchronisation.
type Sink struct {
	queue chan []byte
	done  chan struct{}
	f     *os.File

	accepted  atomic.Int64
	written   atomic.Int64
	dropped   atomic.Int64
	errors    atomic.Int64
	duplicate atomic.Int64
	refused   atomic.Int64 // arrived after the cutoff, outside the acquisition
	overflow  atomic.Bool  // duplicate tracking stopped growing

	admit   sync.RWMutex
	closing bool // guarded by admit

	mu   sync.Mutex
	seen map[string]bool

	closeOnce sync.Once
	closeErr  error // the outcome every Close() caller reports
	finalErr  error // written by the writer before it closes done
}

// active is written once before the listener opens and never again,
// so every later access is a read of an immutable value.
var active atomic.Pointer[Sink]

// Active is the sink records go to, or nil when no capture is configured.
func Active() *Sink { return active.Load() }

// SetActive installs s as the sink records go to and returns the one it
// replaces. The proxy calls it once at startup; tests install their own.
func SetActive(s *Sink) *Sink { return active.Swap(s) }

// Open prepares the capture file name inside root, the only directory a
// capture may live in, or returns nil when name is empty: no capture is
// configured.
//
// Refuses anything it cannot own: a destination outside the capture root, or
// one that already exists. Appending to a previous capture would silently merge
// two runs into what looks like one, and a corpus cannot tell them apart later.
func Open(root, name string) (*Sink, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("shadow capture root: %w", err)
	}
	target, err := filepath.Abs(filepath.Join(root, name))
	if err != nil {
		return nil, fmt.Errorf("shadow capture path: %w", err)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(name) {
		return nil, fmt.Errorf("ATLAS_SHADOW_CAPTURE=%q resolves outside %s", name, root)
	}
	if _, err := os.Stat(target); err == nil {
		return nil, fmt.Errorf("ATLAS_SHADOW_CAPTURE=%q already exists; a capture must be "+
			"fresh so two runs cannot merge into one file", target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, fmt.Errorf("shadow capture dir: %w", err)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("shadow capture: %w", err)
	}
	return Start(f), nil
}

// Start builds a sink that writes to f and starts its writer.
func Start(f *os.File) *Sink {
	s := newSink(f)
	go s.run()
	return s
}

// newSink builds a sink whose writer has not started, so nothing drains
// the queue until run() is called.
func newSink(f *os.File) *Sink {
	return &Sink{
		queue: make(chan []byte, QueueDepth),
		done:  make(chan struct{}),
		f:     f,
		seen:  map[string]bool{},
	}
}

// Enabled reports whether s is a live sink; a nil sink is not.
func (s *Sink) Enabled() bool { return s != nil }

// run is the only writer. Records, footer, sync and descriptor all belong to
// it, so nothing can ever write the file concurrently with it -- in particular
// not the close hook, which only ever asks it to stop and waits.
func (s *Sink) run() {
	defer close(s.done)
	for line := range s.queue {
		if _, err := s.f.Write(line); err != nil {
			// Counted, never retried, never surfaced: a capture failure is a
			// defective capture, not a failed user run.
			s.errors.Add(1)
			continue
		}
		s.written.Add(1)
	}
	// The queue is closed and drained, and admission stopped before it closed,
	// so no further record exists for this file. Finalise.
	s.finalize()
}

// finalize writes the footer after every record its counters describe, then
// releases the descriptor. Only run() calls it, exactly once, which is what
// makes a footer's presence mean "this acquisition completed".
func (s *Sink) finalize() {
	footer, err := json.Marshal(map[string]interface{}{
		"schema_version":            schemaVersionFooter,
		"record_kind":               "task_contract_shadow_footer",
		"accepted":                  s.accepted.Load(),
		"written":                   s.written.Load(),
		"dropped":                   s.dropped.Load(),
		"errors":                    s.errors.Load(),
		"duplicate_request_ids":     s.duplicate.Load(),
		"request_tracking_overflow": s.overflow.Load(),
		"influences_live_decision":  false,
	})
	if err != nil {
		s.finalErr = err
	} else if _, werr := s.f.Write(append(footer, '\n')); werr != nil {
		s.finalErr = werr
	}
	if serr := s.f.Sync(); serr != nil && s.finalErr == nil {
		s.finalErr = serr
	}
	if cerr := s.f.Close(); cerr != nil && s.finalErr == nil {
		s.finalErr = cerr
	}
}

// Submit enqueues without blocking. A full queue drops and counts; a record
// arriving after the cutoff is refused and counted separately, so it can never
// appear in an accepted total the finalised footer is unable to account for.
func (s *Sink) Submit(rec map[string]interface{}) {
	if s == nil {
		return
	}
	// Marshal outside the admission hold: it is the expensive part and it
	// cannot touch the queue.
	line, err := json.Marshal(rec)
	if err != nil {
		s.errors.Add(1)
		return
	}
	line = append(line, '\n')

	s.admit.RLock()
	defer s.admit.RUnlock()
	if s.closing {
		s.refused.Add(1)
		return
	}
	s.accepted.Add(1)
	select {
	case s.queue <- line:
	default:
		s.dropped.Add(1)
	}
}

// NoteRequest records a request id and reports a duplicate within one capture.
func (s *Sink) NoteRequest(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[id] {
		s.duplicate.Add(1)
		return
	}
	if len(s.seen) >= maxTrackedRequests {
		s.overflow.Store(true)
		return
	}
	s.seen[id] = true
}

// Close stops admission, then waits for the writer to finalise, bounded.
//
// It never writes the footer and never closes the descriptor: doing either here
// could race the writer, and a footer racing a record is a file that looks
// complete and is not. The writer owns finalisation, so a footer exists only
// when every record its counters describe was already written.
//
// If the writer does not finish within the deadline the capture is left with no
// footer and the error says so. That is deliberate. A write already inside a
// blocking filesystem syscall cannot be interrupted portably from another
// goroutine -- Go offers no such guarantee for a regular file, and neither
// closing the descriptor nor cancelling a context unblocks it -- so there is no
// safe cutoff to write a footer after. The process is exiting once this hook
// returns; an acquisition with no footer is correctly readable as defective,
// which is better than one that reads as complete and is not.
func (s *Sink) Close(ctx context.Context, wait time.Duration) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		// The cutoff and the channel close happen under the same exclusive
		// hold, so no submitter is inside and none can enter to find it open.
		s.admit.Lock()
		s.closing = true
		close(s.queue)
		s.admit.Unlock()

		select {
		case <-s.done:
			s.closeErr = s.finalErr
		case <-time.After(wait):
			s.closeErr = fmt.Errorf("shadow capture did not finalise within %v; "+
				"the capture has no footer and is incomplete", wait)
		case <-ctx.Done():
			s.closeErr = fmt.Errorf("shadow capture finalisation cancelled (%w); "+
				"the capture has no footer and is incomplete", ctx.Err())
		}
	})
	return s.closeErr
}
