package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/itigges22/atlas-tui/internal/pipelinestate"
)

func TestApplyPlanLoadedReplacesPriorState(t *testing.T) {
	m := &tuiModel{}
	// First load — initial plan.
	msg, ok := applyPlanLoaded(m, json.RawMessage(`{"verify_step": "s2", "winning_score": 0.9, "revision": 0,
		"steps": [{"id": "s1", "action": "read_file", "target": "app.py", "why": "inspect"},
		          {"id": "s2", "action": "run_command", "target": "pytest", "why": "verify"}]}`))
	if !ok {
		t.Fatal("applyPlanLoaded returned ok=false on valid payload")
	}
	if m.plan == nil || len(m.plan.Steps) != 2 {
		t.Fatalf("m.plan not populated correctly: %+v", m.plan)
	}
	if msg.Meta != "plan" {
		t.Errorf("first plan meta = %q, want %q", msg.Meta, "plan")
	}

	// Mark s1 satisfied, then load a revision — Satisfied should reset.
	m.plan.MarkSatisfied("s1")
	msg, ok = applyPlanLoaded(m, json.RawMessage(`{"verify_step": "s2", "winning_score": 1.0, "revision": 1,
		"steps": [{"id": "s1", "action": "edit_file", "target": "app.py", "why": "fix"},
		          {"id": "s2", "action": "run_command", "target": "pytest", "why": "verify"}]}`))
	if !ok {
		t.Fatal("applyPlanLoaded(revision) returned ok=false")
	}
	if m.plan.Steps[0].Satisfied {
		t.Error("revision should reset Satisfied flags")
	}
	if msg.Meta != "plan rev 1" {
		t.Errorf("revision meta = %q, want %q", msg.Meta, "plan rev 1")
	}
}

func TestApplyPlanLoadedRejectsEmpty(t *testing.T) {
	m := &tuiModel{}
	if _, ok := applyPlanLoaded(m, json.RawMessage(`{"steps":[]}`)); ok {
		t.Error("empty steps should be rejected")
	}
	if _, ok := applyPlanLoaded(m, json.RawMessage(`not json`)); ok {
		t.Error("malformed json should be rejected")
	}
}

// A tool call off the plan changes nothing and shows nothing; one that
// matches a step marks it satisfied and shows a row naming the step.
func TestApplyPlanAdherence(t *testing.T) {
	m := &tuiModel{plan: &pipelinestate.PlanView{Steps: []pipelinestate.PlanStep{{ID: "s1"}, {ID: "s2"}}}}
	body := applyPlanAdherence(m, json.RawMessage(`{"matched": false, "tool": "list_directory", "off_streak": 2}`))
	if body != "" {
		t.Errorf("unmatched adherence should be silent, got %q", body)
	}
	if m.plan.Steps[0].Satisfied || m.plan.Steps[1].Satisfied {
		t.Error("unmatched call shouldn't flip any step")
	}
	body = applyPlanAdherence(m, json.RawMessage(
		`{"matched": true, "step_id": "s1", "step_action": "read_file", "satisfied": 1, "total": 2}`))
	if !strings.Contains(body, "s1") {
		t.Errorf("matched adherence row %q, want one naming step s1", body)
	}
	if !m.plan.Steps[0].Satisfied {
		t.Error("matched adherence didn't flip Satisfied")
	}
}

func TestApplyPlanReviseSetsRevisingFlag(t *testing.T) {
	m := &tuiModel{plan: &pipelinestate.PlanView{Steps: []pipelinestate.PlanStep{{ID: "s1"}}}}
	body := applyPlanRevise(m, json.RawMessage(`{"reason": "off-plan x3", "revision": 1}`))
	if body == "" || !strings.Contains(body, "rev 1") {
		t.Errorf("revise body wrong: %q", body)
	}
	if !m.plan.Revising {
		t.Error("Revising flag not set")
	}
}
