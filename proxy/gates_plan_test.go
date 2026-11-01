package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The planner is prompted to name tools verbatim, so a freeform action
// ("verify with curl") counts as off-plan.
func TestActionMatchesTool(t *testing.T) {
	for _, c := range []struct {
		action, tool string
		want         bool
	}{
		{"read_file", "read_file", true},
		{"read", "read_file", true},
		{"write_file", "write_file", true},
		{"edit", "edit_file", true},
		{"run_command", "run_command", true},
		{"run", "run_command", true},
		// The edit tools are one family: an edit_file step is satisfied by
		// whichever of them made the edit.
		{"edit_file", "insert_after", true},
		{"edit_file", "structural_edit", true},
		{"edit_file", "replace_lines", true},
		{"investigate the bug", "read_file", false},
		{"verify with curl", "run_command", false},
		// Replacing a whole file is a different act from editing one.
		{"write_file", "edit_file", false},
		{"", "read_file", false},
		{"read", "", false},
	} {
		t.Run(c.action+" by "+c.tool, func(t *testing.T) {
			if got := actionMatchesTool(c.action, c.tool); got != c.want {
				t.Errorf("actionMatchesTool(%q, %q) = %v, want %v", c.action, c.tool, got, c.want)
			}
		})
	}
}

func TestTargetsOverlap(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"app.py", "app.py", true},
		{"app.py", "/workspace/app.py", true},
		{"templates/index.html", "/workspace/templates/index.html", true},
		{"./app.py", "app.py", true},
		{"app.py", "tests/app.py", true},
		// A path matches at a component boundary only.
		{"app.py", "tests/test_app.py", false},
		{"app.py", "src/main.go", false},
		{"curl http://localhost:5000/", "curl http://localhost:5000/hello", true},
		{"", "app.py", false},
	} {
		t.Run(c.a+" and "+c.b, func(t *testing.T) {
			if got := targetsOverlap(c.a, c.b); got != c.want {
				t.Errorf("targetsOverlap(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestExtractToolTarget(t *testing.T) {
	for _, c := range []struct {
		tool, args, want string
	}{
		{"read_file", `{"path": "app.py"}`, "app.py"},
		{"write_file", `{"path": "x.py", "content": "..."}`, "x.py"},
		{"edit_file", `{"path": "y.py", "old_str": "a", "new_str": "b"}`, "y.py"},
		{"structural_edit", `{"path": "y.py", "selector": "function:f", "content": "def f(): pass"}`, "y.py"},
		{"insert_after", `{"path": "y.py", "line": 3, "content": "x = 1"}`, "y.py"},
		{"replace_lines", `{"path": "y.py", "start_line": 1, "end_line": 2, "content": "x = 1"}`, "y.py"},
		{"run_command", `{"command": "pytest tests/"}`, "pytest tests/"},
		{"list_directory", `{"path": "src"}`, "src"},
		{"plan_revise", `{"reason": "x"}`, ""}, // no target of its own
	} {
		t.Run(c.tool, func(t *testing.T) {
			if got := extractToolTarget(c.tool, json.RawMessage(c.args)); got != c.want {
				t.Errorf("extractToolTarget(%s, %s) = %q, want %q", c.tool, c.args, got, c.want)
			}
		})
	}
}

// A call satisfies the first unsatisfied step whose action matches the tool
// and whose target, when both have one, is the call's.
func TestMatchPlanStep(t *testing.T) {
	if got := matchPlanStep(nil, nil, "read_file", nil); got != -1 {
		t.Errorf("with no plan: matched step %d, want -1", got)
	}
	plan := &Plan{Steps: []PlanStep{
		{ID: "s1", Action: "read_file", Target: "templates/index.html"},
		{ID: "s2", Action: "edit_file", Target: "templates/index.html"},
		{ID: "s3", Action: "run_command", Target: "curl http://localhost:5000/"},
	}}
	for _, c := range []struct {
		name       string
		satisfied  []bool
		tool, args string
		want       int
	}{
		{"a read of the step's file by its full path", []bool{false, false, false},
			"read_file", `{"path": "/workspace/templates/index.html"}`, 0},
		{"a step already satisfied is not matched again", []bool{true, false, false},
			"read_file", `{"path": "/workspace/templates/index.html"}`, -1},
		{"an edit", []bool{true, false, false},
			"edit_file", `{"path": "templates/index.html"}`, 1},
		{"an edit by another edit tool", []bool{true, false, false},
			"insert_after", `{"path": "templates/index.html"}`, 1},
		{"an edit of another file", []bool{true, false, false},
			"insert_after", `{"path": "templates/other.html"}`, -1},
		{"a whole-file write is not an edit", []bool{true, false, false},
			"write_file", `{"path": "templates/index.html", "content": "x"}`, -1},
		{"the step's command, past an unsatisfied step it does not match", []bool{true, false, false},
			"run_command", `{"command": "curl http://localhost:5000/"}`, 2},
		{"a tool no step names", []bool{false, false, false},
			"list_directory", `{"path": "."}`, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := matchPlanStep(plan, c.satisfied, c.tool, json.RawMessage(c.args)); got != c.want {
				t.Errorf("%s %s matched step %d, want %d", c.tool, c.args, got, c.want)
			}
		})
	}
}

// Satisfied steps and the off-plan streak, call by call. Exploration neither
// satisfies a step nor counts as off-plan; the streak asks for a revision at
// planAutoReviseThreshold (5) calls, and no longer once the revisions are used up.
func TestRecordPlanAdherence(t *testing.T) {
	ctx := &AgentContext{}
	if recordPlanAdherence(ctx, "read_file", nil, true) {
		t.Fatal("with no plan, a call asked for a revision")
	}
	if ctx.PlanStepsSatisfied != nil {
		t.Fatal("with no plan, satisfied steps were allocated")
	}

	ctx.Plan = &Plan{VerifyStep: "s2", Steps: []PlanStep{
		{ID: "s1", Action: "read_file", Target: "app.py"},
		{ID: "s2", Action: "run_command", Target: "pytest"},
	}}
	for _, call := range []struct {
		why        string
		tool, args string
		success    bool
		wantRevise bool
		wantStreak int
		satisfied  []bool
	}{
		{"an on-plan read satisfies s1", "read_file", `{"path": "app.py"}`, true, false, 0, []bool{true, false}},
		{"exploration is neutral", "list_directory", `{"path": "."}`, true, false, 0, []bool{true, false}},
		{"a failed run of the verify step satisfies nothing and is off-plan", "run_command", `{"command": "pytest"}`, false, false, 1, []bool{true, false}},
		{"off-plan 2", "run_command", `{"command": "echo a"}`, true, false, 2, []bool{true, false}},
		{"off-plan 3", "run_command", `{"command": "echo b"}`, true, false, 3, []bool{true, false}},
		{"off-plan 4", "run_command", `{"command": "echo c"}`, true, false, 4, []bool{true, false}},
		{"off-plan 5 asks for a revision", "run_command", `{"command": "echo d"}`, true, true, 5, []bool{true, false}},
	} {
		revise := recordPlanAdherence(ctx, call.tool, json.RawMessage(call.args), call.success)
		if revise != call.wantRevise {
			t.Errorf("%s: revise %v, want %v", call.why, revise, call.wantRevise)
		}
		if ctx.PlanOffStreak != call.wantStreak {
			t.Errorf("%s: streak %d, want %d", call.why, ctx.PlanOffStreak, call.wantStreak)
		}
		if !slices.Equal(ctx.PlanStepsSatisfied, call.satisfied) {
			t.Errorf("%s: satisfied %v, want %v", call.why, ctx.PlanStepsSatisfied, call.satisfied)
		}
	}

	ctx.PlanRevisions = planMaxRevisions
	if recordPlanAdherence(ctx, "run_command", json.RawMessage(`{"command": "echo e"}`), true) {
		t.Errorf("past the revision cap a streak of %d still asked for a revision", ctx.PlanOffStreak)
	}
}

// revisePlan's guards return before the planner is called and before any
// plan_revise event: the TUI renders that event, and with no revision behind
// it the user is shown work that did not happen.
func TestRevisePlanGuardsEmitNoEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  *AgentContext
	}{
		{"nil plan", &AgentContext{Plan: nil}},
		{"at the revision cap", &AgentContext{
			Plan:          &Plan{Steps: []PlanStep{{ID: "s1", Action: "read_file", Target: "a.py"}}},
			PlanRevisions: planMaxRevisions,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			tc.ctx.StreamFn = func(kind string, _ interface{}) { events = append(events, kind) }
			revisePlan(tc.ctx, "add a health endpoint", "off-plan thrash")
			if len(events) != 0 {
				t.Errorf("guard must return before streaming, got events %v", events)
			}
			if tc.ctx.PlanRevisions > planMaxRevisions {
				t.Errorf("revision counter advanced past the cap: %d", tc.ctx.PlanRevisions)
			}
		})
	}
}

// A revision with the same steps is not a revision, whatever its rationale
// says; revisePlan then drops the plan rather than bounce the model on it again.
func TestSamePlanSteps(t *testing.T) {
	readThenEdit := func(rationale string) *Plan {
		return &Plan{Rationale: rationale, Steps: []PlanStep{
			{ID: "s1", Action: "read_file", Target: "app.py"},
			{ID: "s2", Action: "structural_edit", Target: "app.py"},
		}}
	}
	for _, c := range []struct {
		name string
		a, b *Plan
		want bool
	}{
		{"the same steps", readThenEdit(""), readThenEdit(""), true},
		{"the same steps, reworded", readThenEdit("fix it"), readThenEdit("repair the thing"), true},
		{"different actions", readThenEdit(""), &Plan{Steps: []PlanStep{
			{ID: "s1", Action: "edit_file", Target: "app.py"},
			{ID: "s2", Action: "run_command", Target: "pytest"},
		}}, false},
		{"a different number of steps", readThenEdit(""), &Plan{Steps: []PlanStep{
			{ID: "s1", Action: "read_file", Target: "app.py"},
		}}, false},
		{"no first plan", nil, readThenEdit(""), false},
		{"no second plan", readThenEdit(""), nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := samePlanSteps(c.a, c.b); got != c.want {
				t.Errorf("samePlanSteps = %v, want %v", got, c.want)
			}
		})
	}
}

// The verification gate shows this hint when it refuses a `done`. It must name
// something to run even when the plan does not: an empty hint renders as
// "run  to prove it".
func TestPlanVerifyHint(t *testing.T) {
	generic := "the appropriate test/curl/run command"
	for _, c := range []struct {
		name string
		plan *Plan
		want string
	}{
		{"no plan", nil, generic},
		{"a plan with no verify step", &Plan{VerifyStep: ""}, generic},
		{"a verify step", &Plan{VerifyStep: "pytest -q test_app.py"}, "pytest -q test_app.py"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := planVerifyHint(c.plan); got != c.want {
				t.Errorf("planVerifyHint = %q, want %q", got, c.want)
			}
		})
	}
}

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
