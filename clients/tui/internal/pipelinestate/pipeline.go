// Package pipelinestate is the TUI state machine (PC-062): it converts
// incoming Envelope events into derived UI state (pipeline progress,
// counters, current stage), and plan.go holds the plan the planner picked.
//
// State is a pure function of the event sequence: replaying the same
// events in the same order produces the same state. This keeps the
// model deterministic for tests and makes the renderer trivial.
package pipelinestate

import (
	"sort"
	"time"

	"github.com/itigges22/atlas-tui/internal/proxyclient"
)

// Stage tracks one logical pipeline stage's lifecycle.
type Stage struct {
	Name      string
	StartedAt time.Time
	EndedAt   time.Time // zero if still running
	Success   bool      // valid only when EndedAt is non-zero
	Detail    string    // most recent payload.detail
}

func (s Stage) Running() bool { return s.EndedAt.IsZero() }

func (s Stage) Duration() time.Duration {
	if s.EndedAt.IsZero() {
		return time.Since(s.StartedAt)
	}
	return s.EndedAt.Sub(s.StartedAt)
}

// Pipeline aggregates stages from the event stream.
type Pipeline struct {
	// Insertion order — stages appear in the pipeline pane top-to-bottom
	// in the order they first started, so users see a chronological view
	// rather than alphabetic.
	order  []string
	byName map[string]*Stage

	// Counters surfaced in the stats pane.
	TotalEvents   int
	toolCalls     int // counted but not shown; only the tests read it
	ToolSuccesses int
	ToolFailures  int
	Errors        int
	CurrentTurn   int

	// Final state.
	Done        bool
	DoneSuccess bool
	TotalMS     int64
}

// New returns an empty pipeline. Use it rather than Pipeline{}: Apply needs
// the stage map New makes.
func New() Pipeline {
	return Pipeline{byName: map[string]*Stage{}}
}

// Apply mutates p with the effect of one envelope. Pure function: same
// envelope sequence → same state.
func (p *Pipeline) Apply(ev proxyclient.Envelope) {
	p.TotalEvents++

	switch ev.Type {
	case proxyclient.EvtStageStart:
		ts := envTime(ev)
		if existing, ok := p.byName[ev.Stage]; ok {
			// Re-entry into a logical stage (e.g. probe_retry). Reset
			// timing — current run started now, prior result discarded
			// from the visible view.
			existing.StartedAt = ts
			existing.EndedAt = time.Time{}
			existing.Detail = payloadString(ev.Payload, "detail")
		} else {
			p.byName[ev.Stage] = &Stage{
				Name:      ev.Stage,
				StartedAt: ts,
				Detail:    payloadString(ev.Payload, "detail"),
			}
			p.order = append(p.order, ev.Stage)
		}

	case proxyclient.EvtStageEnd:
		ts := envTime(ev)
		s, ok := p.byName[ev.Stage]
		if !ok {
			// stage_end without a matching start — synthesize an entry
			// so the pane shows it. Doesn't pretend the duration is
			// meaningful in that case.
			s = &Stage{Name: ev.Stage, StartedAt: ts}
			p.byName[ev.Stage] = s
			p.order = append(p.order, ev.Stage)
		}
		s.EndedAt = ts
		s.Success = boolField(ev.Payload, "success")
		if d := payloadString(ev.Payload, "detail"); d != "" {
			s.Detail = d
		}

	case proxyclient.EvtToolCall:
		p.toolCalls++
		if turn, ok := ev.Payload["turn"].(float64); ok {
			p.CurrentTurn = int(turn)
		}

	case proxyclient.EvtToolResult:
		if boolField(ev.Payload, "success") {
			p.ToolSuccesses++
		} else {
			p.ToolFailures++
		}

	case proxyclient.EvtError:
		p.Errors++

	case proxyclient.EvtDone:
		p.Done = true
		p.DoneSuccess = boolField(ev.Payload, "success")
		if v, ok := ev.Payload["total_duration_ms"].(float64); ok {
			p.TotalMS = int64(v)
		}
	}
}

// Stages returns the ordered list of stages for the pipeline pane.
func (p *Pipeline) Stages() []*Stage {
	out := make([]*Stage, 0, len(p.order))
	for _, name := range p.order {
		out = append(out, p.byName[name])
	}
	return out
}

// ActiveStage returns the first stage currently running, or nil if
// none are active. Used in the stats line.
func (p *Pipeline) ActiveStage() *Stage {
	// Most recent stage_start without a matching stage_end.
	candidates := make([]*Stage, 0, len(p.order))
	for _, name := range p.order {
		if s := p.byName[name]; s.Running() {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// Sort by StartedAt desc — innermost (most recently started) wins.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].StartedAt.After(candidates[j].StartedAt)
	})
	return candidates[0]
}

func envTime(ev proxyclient.Envelope) time.Time {
	return time.Unix(0, int64(ev.Timestamp*1e9))
}

func boolField(p map[string]interface{}, key string) bool {
	v, ok := p[key]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func payloadString(p map[string]interface{}, key string) string {
	v, ok := p[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
