package main

import (
	"encoding/json"
	"testing"
)

// The plan's verify step names its command by guess. A passing verification
// of the same program satisfies it, whatever the port or flags: a plan may
// say `curl http://127.0.0.1:5000` while the app serves on 5001. Anything
// else keeps the literal rule.

func flaskPlan() *Plan {
	return &Plan{VerifyStep: "s5", Steps: []PlanStep{
		{ID: "s1", Action: "read_file", Target: "app.py"},
		{ID: "s2", Action: "structural_edit", Target: "app.py"},
		{ID: "s3", Action: "structural_edit", Target: "app.py"},
		{ID: "s4", Action: "run_command", Target: "python app.py"},
		{ID: "s5", Action: "run_command", Target: "curl http://127.0.0.1:5000"},
	}}
}

func runArgs(cmd string) json.RawMessage {
	b, _ := json.Marshal(RunCommandInput{Command: cmd})
	return b
}

func TestWhatSatisfiesTheVerifyStep(t *testing.T) {
	for _, c := range []struct {
		name, cmd string
		success   bool
		satisfies bool
	}{
		{"a passing probe on the app's real port", "curl -sf http://127.0.0.1:5001/", true, true},
		{"another program", "python3 app.py", true, false},
		{"a failed probe", "curl -sf http://127.0.0.1:5001/", false, false},
		{"curl without -f proves nothing", "curl http://127.0.0.1:5001/", true, false},
		{"a static check", "python3 -m py_compile app.py", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := &AgentContext{Plan: flaskPlan()}
			ctx.PlanStepsSatisfied = []bool{true, true, true, true, false}
			recordPlanAdherence(ctx, "run_command", runArgs(c.cmd), c.success)
			if ctx.PlanStepsSatisfied[4] != c.satisfies {
				t.Errorf("%q (success %v) satisfied the verify step: %v, want %v",
					c.cmd, c.success, ctx.PlanStepsSatisfied[4], c.satisfies)
			}
		})
	}
}

// Only the verify step is relaxed: an ordinary run_command step still needs
// its own command.
func TestOtherCommandStepsKeepTheLiteralRule(t *testing.T) {
	plan := &Plan{VerifyStep: "s3", Steps: []PlanStep{
		{ID: "s1", Action: "run_command", Target: "pip install -r requirements.txt"},
		{ID: "s2", Action: "run_command", Target: "pytest tests/test_api.py"},
		{ID: "s3", Action: "run_command", Target: "pytest"},
	}}
	ctx := &AgentContext{Plan: plan}
	ctx.PlanStepsSatisfied = []bool{true, false, false}
	recordPlanAdherence(ctx, "run_command", runArgs("python -m pytest -q tests/test_models.py"), true)
	if ctx.PlanStepsSatisfied[1] {
		t.Error("a non-verify step was satisfied by a different command")
	}
	if !ctx.PlanStepsSatisfied[2] {
		t.Error("the verify step (pytest) was not satisfied by a passing python -m pytest")
	}
}

func TestCommandProgram(t *testing.T) {
	for cmd, want := range map[string]string{
		"curl -sf http://127.0.0.1:5001/":  "curl",
		"cd tests && python3 -m pytest -q": "pytest",
		"FLASK_APP=app.py python3.12 x.py": "python",
		"timeout 5 node server.js":         "node",
		"./run.sh --fast":                  "run.sh",
		"":                                 "",
	} {
		if got := commandProgram(cmd); got != want {
			t.Errorf("commandProgram(%q) = %q, want %q", cmd, got, want)
		}
	}
}
