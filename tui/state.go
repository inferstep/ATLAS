// Plan rendering — handles plan_loaded / plan_adherence / plan_revise
// events and surfaces them as chat rows. Plan state is also stashed on
// the model, as a pipelinestate.PlanView, so a future dedicated pane can
// read it without re-parsing events.
//
// One plan_loaded fires when the planner picks a winner; subsequent
// plan_adherence(matched=true) updates flip per-step Satisfied flags;
// plan_revise marks the plan as "being revised" and the next
// plan_loaded supersedes it.

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/itigges22/atlas-tui/internal/pipelinestate"
	"github.com/itigges22/atlas-tui/internal/sessionstore"
)

// applyPlanLoaded replaces m.plan with the freshly-loaded plan.
// Called from the SSE event handler when "plan_loaded" arrives.
func applyPlanLoaded(m *tuiModel, data json.RawMessage) (sessionstore.ChatMessage, bool) {
	var p struct {
		Steps        []pipelinestate.PlanStep `json:"steps"`
		VerifyStep   string                   `json:"verify_step"`
		Rationale    string                   `json:"rationale"`
		WinningScore float64                  `json:"winning_score"`
		Revision     int                      `json:"revision"`
	}
	if err := json.Unmarshal(data, &p); err != nil || len(p.Steps) == 0 {
		return sessionstore.ChatMessage{}, false
	}
	view := &pipelinestate.PlanView{
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
	lines := []string{view.Summary()}
	lines = append(lines, view.StepLines()...)
	if view.Rationale != "" {
		lines = append(lines, "  · "+view.Rationale)
	}
	meta := "plan"
	if p.Revision > 0 {
		meta = fmt.Sprintf("plan rev %d", p.Revision)
	}
	return sessionstore.ChatMessage{
		Role: sessionstore.RoleSystem,
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
		m.plan.MarkSatisfied(p.StepID)
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
