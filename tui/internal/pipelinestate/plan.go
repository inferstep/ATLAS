package pipelinestate

import (
	"fmt"
	"strings"
)

// PlanStep mirrors services/proxy/internal/wire/messages.go PlanStep with a Satisfied flag the
// TUI flips when a tool call hits this step.
type PlanStep struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Why       string `json:"why"`
	Satisfied bool   `json:"-"`
}

// PlanView is everything the TUI knows about the current plan.
// Replaced wholesale on each plan_loaded; revisions reset Satisfied.
type PlanView struct {
	Steps        []PlanStep
	VerifyStepID string
	Rationale    string
	WinningScore float64
	Revision     int
	Revising     bool // true between plan_revise and the next plan_loaded
}

// satisfiedCount reports how many steps are marked Satisfied.
func (p *PlanView) satisfiedCount() int {
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

// MarkSatisfied flips Satisfied on the step with id stepID.
// Returns true if a step was flipped, false if the id was unknown
// (stale plan or mismatched id).
func (p *PlanView) MarkSatisfied(stepID string) bool {
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

// Summary returns a one-line label for chat rows. Examples:
//
//	"Plan: 3 steps · verify=s3 · score 1.00"
//	"Plan revising (rev 1): off-streak..."
//	"Plan: 2/3 satisfied"
func (p *PlanView) Summary() string {
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

// StepLines returns one rendered line per step. Each line is:
//
//	"  s1  ☐  read_file templates/index.html"
//	"  s2  ✓  edit_file templates/index.html"
//	"  s3  ⚐  run_command curl http://localhost:5000/   (verify)"
//
// Glyphs: ☐ unsatisfied, ✓ satisfied, ⚐ verify-step (when not yet
// satisfied). The verify glyph wins over ☐ so the user can spot the
// "this is your evidence" step at a glance.
func (p *PlanView) StepLines() []string {
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
