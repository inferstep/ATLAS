package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A plan step is the work, not the tool call. A write that lands with a
// failing check is a successful tool call, but the file is broken and has
// never been run. Ticked off, the plan reminder would tell the model on every
// later request that the broken file is finished work not to revisit.

func planWorld(t *testing.T) *AgentContext {
	t.Helper()
	ctx, _ := exactEditWorld(t, "app.py", "x = 1\n")
	steps := []PlanStep{
		{ID: "s1", Action: "write_file", Target: "app.py"},
		{ID: "s2", Action: "run_command", Target: "python3 app.py"},
	}
	ctx.Plan = &Plan{Steps: steps, VerifyStep: "s2"}
	ctx.PlanStepsSatisfied = make([]bool, len(steps))
	return ctx
}

func TestAWarnedWriteDoesNotSatisfyItsPlanStep(t *testing.T) {
	ctx := planWorld(t)
	args, _ := json.Marshal(map[string]string{"path": "app.py"})
	// The ledger records what the check said about the bytes that landed.
	recordWarnedValidation(t, ctx, "app.py")
	recordPlanAdherence(ctx, "write_file", args, true)
	if ctx.PlanStepsSatisfied[0] {
		t.Error("a write that landed with a failing check ticked its step off")
	}
	note := buildPlanReminder(ctx)
	if strings.Contains(note, "Done: s1") {
		t.Errorf("the reminder calls the broken file finished work:\n%s", note)
	}
	if !strings.Contains(note, `"s1"`) {
		t.Errorf("the reminder does not keep s1 as the current step:\n%s", note)
	}
}

func TestACleanWriteStillSatisfiesItsPlanStep(t *testing.T) {
	ctx := planWorld(t)
	args, _ := json.Marshal(map[string]string{"path": "app.py"})
	recordPlanAdherence(ctx, "write_file", args, true)
	if !ctx.PlanStepsSatisfied[0] {
		t.Error("a clean write did not satisfy its step")
	}
	if note := buildPlanReminder(ctx); !strings.Contains(note, "Done: s1") {
		t.Errorf("the reminder does not credit the finished step:\n%s", note)
	}
}

// Fixing the file finishes the step: the rule delays credit, it does not
// withhold it.
func TestARepairedFileSatisfiesTheStep(t *testing.T) {
	ctx := planWorld(t)
	args, _ := json.Marshal(map[string]string{"path": "app.py"})
	recordWarnedValidation(t, ctx, "app.py")
	recordPlanAdherence(ctx, "write_file", args, true)
	if ctx.PlanStepsSatisfied[0] {
		t.Fatal("precondition: the warned write should not have satisfied the step")
	}
	clearValidation(t, ctx, "app.py")
	recordPlanAdherence(ctx, "write_file", args, true)
	if !ctx.PlanStepsSatisfied[0] {
		t.Error("the repaired write still did not satisfy the step")
	}
}

// Non-write steps are untouched: a verification command is judged by whether
// it succeeded, as before.
func TestAVerificationStepIsUnaffectedByFileWarnings(t *testing.T) {
	ctx := planWorld(t)
	recordWarnedValidation(t, ctx, "app.py")
	args, _ := json.Marshal(map[string]interface{}{"command": "python3 app.py", "timeout": 30})
	recordPlanAdherence(ctx, "run_command", args, true)
	if !ctx.PlanStepsSatisfied[1] {
		t.Error("a successful verification step was withheld because another file carries a warning")
	}
}

func recordWarnedValidation(t *testing.T, ctx *AgentContext, rel string) {
	t.Helper()
	key := ledgerKey(ctx, rel)
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*DeliverableState{}
	}
	d := ctx.Ledger[key]
	if d == nil {
		d = &DeliverableState{}
		ctx.Ledger[key] = d
	}
	h := fileSHA256(ctx, rel)
	d.CurrentHash, d.ValidatedHash = h, h
	d.ValidationKind, d.ValidationStatus = ValidationKindSyntax, ValidationFailed
	d.ValidationDetail = "SyntaxError: unmatched ')' (line 111)"
}

func clearValidation(t *testing.T, ctx *AgentContext, rel string) {
	t.Helper()
	key := ledgerKey(ctx, rel)
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	if d := ctx.Ledger[key]; d != nil {
		h := fileSHA256(ctx, rel)
		d.CurrentHash, d.ValidatedHash = h, h
		d.ValidationKind, d.ValidationStatus = ValidationKindSyntax, ValidationPassed
		d.ValidationDetail = ""
	}
}
