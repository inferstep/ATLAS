package main

import (
	"testing"
)

// The background gate asks the run to stop a job it started. A planned step
// that runs a command may need that job, so the gate waits while the plan
// gate still owes one. Otherwise the run stops the server, the plan gate then
// demands a probe of it, the probe cannot pass, and passing work ends
// "stopped".

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

// Only an owed step that runs something holds the job open. A spent plan gate
// does not bounce, so holding the job for it would end the run
// background_work_unresolved; an owed edit does not need the job.
func TestTheBackgroundGateSpeaksWhenNoRunStepIsOwed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lastAction string
		st         *runState
	}{
		{"the plan gate is spent", "run_command",
			&runState{madeProductiveChange: true, gateBounces: map[string]int{"plan_gate": maxGateBounces}}},
		{"the owed step is an edit", "structural_edit",
			&runState{madeProductiveChange: true}},
	} {
		ctx, _ := orderCtx(t, tc.lastAction)
		if gate, _ := tc.st.exitGates(ctx, "Add a pause key to app.py.", "Done."); gate != "background_gate" {
			t.Errorf("%s: want background_gate, got %q", tc.name, gate)
		}
	}
}
