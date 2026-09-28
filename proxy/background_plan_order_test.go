package main

import (
	"testing"
)

// The background gate asks the run to stop a job it started. A planned step
// that runs a command may need that job, so the gate waits while the plan
// gate still owes one. Smoke run 2026-09-27 (flask_pause rep 1): the gate had
// the run stop the server, the plan gate then demanded a probe of it, and the
// probe could no longer pass. The run ended "stopped" on passing work.

// orderCtx is a run with a live background job (job1) and a three-step plan
// whose last step, a run_command, is not yet done.
func orderCtx(t *testing.T, lastAction string) (*AgentContext, *bgSandbox) {
	t.Helper()
	dir := t.TempDir()
	bg := &bgSandbox{running: true}
	sandbox := newBgSandbox(t, dir, bg)
	t.Cleanup(sandbox.Close)
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = sandbox.URL
	ctx.BackgroundJobs = map[string]string{"job1": "python3 app.py"}
	ctx.Plan = &Plan{WinningScore: 0.9, VerifyStep: "s3", Steps: []PlanStep{
		{ID: "s1", Action: "structural_edit", Target: "app.py"},
		{ID: "s2", Action: "run_background", Target: "python3 app.py"},
		{ID: "s3", Action: lastAction, Target: "python3 check.py"},
	}}
	ctx.PlanStepsSatisfied = []bool{true, true, false}
	return ctx, bg
}

func TestThePlanGateSpeaksBeforeTheBackgroundGate(t *testing.T) {
	ctx, bg := orderCtx(t, "run_command")
	st := &runState{madeProductiveChange: true}
	if gate, msg := st.exitGates(ctx, "Add a pause key to app.py.", "Done."); gate != "plan_gate" {
		t.Fatalf("a planned run is owed while the job is up: want plan_gate first, got %q: %s", gate, msg)
	}
	if len(bg.stopped) != 0 {
		t.Fatalf("the job was stopped before the planned run: %v", bg.stopped)
	}
	ctx.PlanStepsSatisfied[2] = true // the model ran the step against the live job
	if gate, _ := st.exitGates(ctx, "Add a pause key to app.py.", "Done."); gate != "background_gate" {
		t.Fatalf("with the plan done, the job must be stopped next: got %q", gate)
	}
}

// A spent plan gate no longer bounces, so it must not hold the job open: the
// run would end background_work_unresolved instead.
func TestASpentPlanGateDoesNotHoldTheJobOpen(t *testing.T) {
	ctx, _ := orderCtx(t, "run_command")
	st := &runState{madeProductiveChange: true, gateBounces: map[string]int{"plan_gate": maxGateBounces}}
	if gate, _ := st.exitGates(ctx, "Add a pause key to app.py.", "Done."); gate != "background_gate" {
		t.Fatalf("want background_gate once the plan gate is spent, got %q", gate)
	}
}

// Only a step that runs something needs the job. An owed edit does not.
func TestAnOwedEditDoesNotHoldTheJob(t *testing.T) {
	ctx, _ := orderCtx(t, "structural_edit")
	st := &runState{madeProductiveChange: true}
	if gate, _ := st.exitGates(ctx, "Add a pause key to app.py.", "Done."); gate != "background_gate" {
		t.Fatalf("an owed edit does not need the job: want background_gate, got %q", gate)
	}
}
