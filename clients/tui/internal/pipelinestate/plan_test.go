package pipelinestate

import (
	"strings"
	"testing"
)

func TestPlanViewSummaryShapes(t *testing.T) {
	cases := []struct {
		name string
		view *PlanView
		want string
	}{
		{
			"initial: shows step count + verify + score",
			&PlanView{
				Steps:        []PlanStep{{ID: "s1"}, {ID: "s2"}, {ID: "s3"}},
				VerifyStepID: "s3",
				WinningScore: 1.0,
			},
			"Plan: 3 steps · verify=s3 · score 1.00",
		},
		{
			"partial progress: shows N/M satisfied",
			&PlanView{
				Steps:        []PlanStep{{ID: "s1", Satisfied: true}, {ID: "s2"}},
				VerifyStepID: "s2",
				WinningScore: 0.8,
			},
			"Plan: 1/2 satisfied · verify=s2",
		},
		{
			"revising state",
			&PlanView{Revising: true, Revision: 1},
			"Plan revising (rev 1)…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.view.Summary(); got != tc.want {
				t.Errorf("Summary() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlanViewMarkSatisfied(t *testing.T) {
	view := &PlanView{
		Steps: []PlanStep{
			{ID: "s1", Action: "read_file"},
			{ID: "s2", Action: "edit_file"},
		},
	}
	if !view.MarkSatisfied("s1") {
		t.Error("MarkSatisfied(s1) returned false on first call")
	}
	if !view.Steps[0].Satisfied {
		t.Error("step s1 not actually marked satisfied")
	}
	if view.MarkSatisfied("s1") {
		t.Error("MarkSatisfied(s1) should be no-op on second call")
	}
	if view.MarkSatisfied("nonexistent") {
		t.Error("MarkSatisfied for unknown id should return false")
	}
}

func TestPlanViewStepLines(t *testing.T) {
	view := &PlanView{
		Steps: []PlanStep{
			{ID: "s1", Action: "read_file", Target: "app.py", Satisfied: true},
			{ID: "s2", Action: "edit_file", Target: "app.py"},
			{ID: "s3", Action: "run_command", Target: "curl http://localhost:5000/"},
		},
		VerifyStepID: "s3",
	}
	lines := view.StepLines()
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
