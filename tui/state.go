// PC-062: TUI state machine — converts incoming Envelope events into
// derived UI state (pipeline progress, counters, current stage).
//
// State is a pure function of the event sequence: replaying the same
// events in the same order produces the same state. This keeps the
// model deterministic for tests and makes the renderer trivial.

package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/itigges22/atlas-tui/internal/proxyclient"
)

// stageStatus tracks one logical pipeline stage's lifecycle.
type stageStatus struct {
	Name      string
	StartedAt time.Time
	EndedAt   time.Time // zero if still running
	Success   bool      // valid only when EndedAt is non-zero
	Detail    string    // most recent payload.detail
}

func (s stageStatus) Running() bool { return s.EndedAt.IsZero() }

func (s stageStatus) Duration() time.Duration {
	if s.EndedAt.IsZero() {
		return time.Since(s.StartedAt)
	}
	return s.EndedAt.Sub(s.StartedAt)
}

// pipelineState aggregates stages from the event stream.
type pipelineState struct {
	// Insertion order — stages appear in the pipeline pane top-to-bottom
	// in the order they first started, so users see a chronological view
	// rather than alphabetic.
	order  []string
	byName map[string]*stageStatus

	// Counters surfaced in the stats pane.
	totalEvents   int
	toolCalls     int
	toolSuccesses int
	toolFailures  int
	errors        int
	currentTurn   int

	// Final state.
	done        bool
	doneSuccess bool
	totalMS     int64
}

func newPipelineState() pipelineState {
	return pipelineState{byName: map[string]*stageStatus{}}
}

// apply mutates p with the effect of one envelope. Pure function: same
// envelope sequence → same state.
func (p *pipelineState) apply(ev proxyclient.Envelope) {
	p.totalEvents++

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
			p.byName[ev.Stage] = &stageStatus{
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
			s = &stageStatus{Name: ev.Stage, StartedAt: ts}
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
			p.currentTurn = int(turn)
		}

	case proxyclient.EvtToolResult:
		if boolField(ev.Payload, "success") {
			p.toolSuccesses++
		} else {
			p.toolFailures++
		}

	case proxyclient.EvtError:
		p.errors++

	case proxyclient.EvtDone:
		p.done = true
		p.doneSuccess = boolField(ev.Payload, "success")
		if v, ok := ev.Payload["total_duration_ms"].(float64); ok {
			p.totalMS = int64(v)
		}
	}
}

// stages returns the ordered list of stages for the pipeline pane.
func (p *pipelineState) stages() []*stageStatus {
	out := make([]*stageStatus, 0, len(p.order))
	for _, name := range p.order {
		out = append(out, p.byName[name])
	}
	return out
}

// activeStage returns the first stage currently running, or nil if
// none are active. Used in the stats line.
func (p *pipelineState) activeStage() *stageStatus {
	// Most recent stage_start without a matching stage_end.
	candidates := make([]*stageStatus, 0, len(p.order))
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

// Plan rendering — handles plan_loaded / plan_adherence / plan_revise
// events and surfaces them as chat rows. Plan state is also stashed on
// the model so a future dedicated pane can read it without re-parsing
// events.
//
// One plan_loaded fires when the planner picks a winner; subsequent
// plan_adherence(matched=true) updates flip per-step Satisfied flags;
// plan_revise marks the plan as "being revised" and the next
// plan_loaded supersedes it.

// planStep mirrors proxy/internal/wire/messages.go PlanStep with a Satisfied flag the
// TUI flips when a tool call hits this step.
type planStep struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Why       string `json:"why"`
	Satisfied bool   `json:"-"`
}

// planView is everything the TUI knows about the current plan.
// Replaced wholesale on each plan_loaded; revisions reset Satisfied.
type planView struct {
	Steps        []planStep
	VerifyStepID string
	Rationale    string
	WinningScore float64
	Revision     int
	Revising     bool // true between plan_revise and the next plan_loaded
}

// satisfiedCount reports how many steps are marked Satisfied.
func (p *planView) satisfiedCount() int {
	if p == nil {
		return 0
	}
	n := 0
	for _, s := range p.Steps {
		if s.Satisfied {
			n++
		}
	}
	return n
}

// markSatisfied flips Satisfied on the step with id stepID.
// Returns true if a step was flipped, false if the id was unknown
// (stale plan or mismatched id).
func (p *planView) markSatisfied(stepID string) bool {
	if p == nil {
		return false
	}
	for i := range p.Steps {
		if p.Steps[i].ID == stepID {
			if p.Steps[i].Satisfied {
				return false // already done — no-op
			}
			p.Steps[i].Satisfied = true
			return true
		}
	}
	return false
}

// summary returns a one-line label for chat rows. Examples:
//
//	"Plan: 3 steps · verify=s3 · score 1.00"
//	"Plan revising (rev 1): off-streak..."
//	"Plan: 2/3 satisfied"
func (p *planView) summary() string {
	if p == nil {
		return ""
	}
	if p.Revising {
		if p.Revision > 0 {
			return fmt.Sprintf("Plan revising (rev %d)…", p.Revision)
		}
		return "Plan revising…"
	}
	n := p.satisfiedCount()
	total := len(p.Steps)
	verify := ""
	if p.VerifyStepID != "" {
		verify = " · verify=" + p.VerifyStepID
	}
	if n == 0 {
		// Initial render: show the score so the user knows the
		// planner picked confidently.
		score := ""
		if p.WinningScore > 0 {
			score = fmt.Sprintf(" · score %.2f", p.WinningScore)
		}
		return fmt.Sprintf("Plan: %d steps%s%s", total, verify, score)
	}
	return fmt.Sprintf("Plan: %d/%d satisfied%s", n, total, verify)
}

// stepLines returns one rendered line per step. Each line is:
//
//	"  s1  ☐  read_file templates/index.html"
//	"  s2  ✓  edit_file templates/index.html"
//	"  s3  ⚐  run_command curl http://localhost:5000/   (verify)"
//
// Glyphs: ☐ unsatisfied, ✓ satisfied, ⚐ verify-step (when not yet
// satisfied). The verify glyph wins over ☐ so the user can spot the
// "this is your evidence" step at a glance.
func (p *planView) stepLines() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.Steps))
	for _, s := range p.Steps {
		glyph := "☐"
		switch {
		case s.Satisfied:
			glyph = "✓"
		case s.ID == p.VerifyStepID:
			glyph = "⚐"
		}
		body := strings.TrimSpace(s.Action)
		if s.Target != "" {
			body += " " + s.Target
		}
		line := fmt.Sprintf("  %s  %s  %s", s.ID, glyph, body)
		if s.ID == p.VerifyStepID && !s.Satisfied {
			line += "   (verify)"
		}
		out = append(out, line)
	}
	return out
}

// applyPlanLoaded replaces m.plan with the freshly-loaded plan.
// Called from the SSE event handler when "plan_loaded" arrives.
func applyPlanLoaded(m *tuiModel, data json.RawMessage) (chatMessage, bool) {
	var p struct {
		Steps        []planStep `json:"steps"`
		VerifyStep   string     `json:"verify_step"`
		Rationale    string     `json:"rationale"`
		WinningScore float64    `json:"winning_score"`
		Revision     int        `json:"revision"`
	}
	if err := json.Unmarshal(data, &p); err != nil || len(p.Steps) == 0 {
		return chatMessage{}, false
	}
	view := &planView{
		Steps:        p.Steps,
		VerifyStepID: p.VerifyStep,
		Rationale:    p.Rationale,
		WinningScore: p.WinningScore,
		Revision:     p.Revision,
	}
	m.plan = view

	// Render as a single multi-line chat row: header summary + step
	// list. One row keeps it scannable without spawning N rows that
	// the user has to mentally re-group.
	lines := []string{view.summary()}
	lines = append(lines, view.stepLines()...)
	if view.Rationale != "" {
		lines = append(lines, "  · "+view.Rationale)
	}
	meta := "plan"
	if p.Revision > 0 {
		meta = fmt.Sprintf("plan rev %d", p.Revision)
	}
	return chatMessage{
		Role: roleSystem,
		Meta: meta,
		Body: strings.Join(lines, "\n"),
	}, true
}

// applyPlanAdherence flips a step's Satisfied flag and returns a
// short status string for the chat row, or empty if no row should
// be added (off-plan calls don't deserve a chat row — they'd flood
// the pane).
func applyPlanAdherence(m *tuiModel, data json.RawMessage) string {
	var p struct {
		Matched    bool   `json:"matched"`
		StepID     string `json:"step_id"`
		StepAction string `json:"step_action"`
		Satisfied  int    `json:"satisfied"`
		Total      int    `json:"total"`
		OffStreak  int    `json:"off_streak"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	if !p.Matched {
		// Off-plan call — silent on the chat side. The pipeline pane
		// can show the streak in a future iteration if useful.
		return ""
	}
	if m.plan != nil {
		m.plan.markSatisfied(p.StepID)
	}
	return fmt.Sprintf("✓ %s satisfied · %s (%d/%d)",
		p.StepID, p.StepAction, p.Satisfied, p.Total)
}

// applyPlanRevise marks the current plan as revising and returns
// the chat row body announcing it. The next plan_loaded will
// replace m.plan with the revised plan.
func applyPlanRevise(m *tuiModel, data json.RawMessage) string {
	var p struct {
		Reason   string `json:"reason"`
		Revision int    `json:"revision"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	if m.plan != nil {
		m.plan.Revising = true
	}
	if p.Reason == "" {
		p.Reason = "off-plan tool sequence"
	}
	return fmt.Sprintf("Plan revising (rev %d): %s", p.Revision, p.Reason)
}
