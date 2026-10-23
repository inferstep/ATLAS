package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlanViewSummaryShapes(t *testing.T) {
	cases := []struct {
		name string
		view *planView
		want string
	}{
		{
			"initial: shows step count + verify + score",
			&planView{
				Steps:        []planStep{{ID: "s1"}, {ID: "s2"}, {ID: "s3"}},
				VerifyStepID: "s3",
				WinningScore: 1.0,
			},
			"Plan: 3 steps · verify=s3 · score 1.00",
		},
		{
			"partial progress: shows N/M satisfied",
			&planView{
				Steps:        []planStep{{ID: "s1", Satisfied: true}, {ID: "s2"}},
				VerifyStepID: "s2",
				WinningScore: 0.8,
			},
			"Plan: 1/2 satisfied · verify=s2",
		},
		{
			"revising state",
			&planView{Revising: true, Revision: 1},
			"Plan revising (rev 1)…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.view.summary(); got != tc.want {
				t.Errorf("summary() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlanViewMarkSatisfied(t *testing.T) {
	view := &planView{
		Steps: []planStep{
			{ID: "s1", Action: "read_file"},
			{ID: "s2", Action: "edit_file"},
		},
	}
	if !view.markSatisfied("s1") {
		t.Error("markSatisfied(s1) returned false on first call")
	}
	if !view.Steps[0].Satisfied {
		t.Error("step s1 not actually marked satisfied")
	}
	if view.markSatisfied("s1") {
		t.Error("markSatisfied(s1) should be no-op on second call")
	}
	if view.markSatisfied("nonexistent") {
		t.Error("markSatisfied for unknown id should return false")
	}
}

func TestPlanViewStepLines(t *testing.T) {
	view := &planView{
		Steps: []planStep{
			{ID: "s1", Action: "read_file", Target: "app.py", Satisfied: true},
			{ID: "s2", Action: "edit_file", Target: "app.py"},
			{ID: "s3", Action: "run_command", Target: "curl http://localhost:5000/"},
		},
		VerifyStepID: "s3",
	}
	lines := view.stepLines()
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	// Satisfied step uses ✓
	if !strings.Contains(lines[0], "✓") || !strings.Contains(lines[0], "read_file") {
		t.Errorf("satisfied line wrong: %q", lines[0])
	}
	// Plain unsatisfied step uses ☐
	if !strings.Contains(lines[1], "☐") {
		t.Errorf("unsatisfied line wrong: %q", lines[1])
	}
	// Verify step (unsatisfied) uses ⚐ + (verify) marker
	if !strings.Contains(lines[2], "⚐") || !strings.Contains(lines[2], "(verify)") {
		t.Errorf("verify line wrong: %q", lines[2])
	}
}

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
	m.plan.markSatisfied("s1")
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
	m := &tuiModel{plan: &planView{Steps: []planStep{{ID: "s1"}, {ID: "s2"}}}}
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
	m := &tuiModel{plan: &planView{Steps: []planStep{{ID: "s1"}}}}
	body := applyPlanRevise(m, json.RawMessage(`{"reason": "off-plan x3", "revision": 1}`))
	if body == "" || !strings.Contains(body, "rev 1") {
		t.Errorf("revise body wrong: %q", body)
	}
	if !m.plan.Revising {
		t.Error("Revising flag not set")
	}
}
