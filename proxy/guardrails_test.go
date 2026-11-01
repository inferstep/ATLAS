package main

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

// A repair request: the verification gate asks for a passing run before done.
func TestIsFixIntentMessage(t *testing.T) {
	for _, c := range []struct {
		msg  string
		want bool
	}{
		{"fix the bug in app.py", true},
		{"the form submission is broken", true},
		{"why isn't this rendering", true},
		{"the page won't load", true},
		{"I'm getting an error", true},
		{"can you verify it works", true},
		{"add a logout button to the header", false},
		// No repair word: for this request only a red run arms the gate.
		{"build me a small API with a couple tests", false},
		{"the tests pass now, no need to fix anything", false},
	} {
		t.Run(c.msg, func(t *testing.T) {
			if got := isFixIntentMessage(c.msg); got != c.want {
				t.Errorf("isFixIntentMessage = %v, want %v", got, c.want)
			}
		})
	}
}

// A request to create or change something on disk: done needs a successful
// write first.
func TestIsActionIntentMessage(t *testing.T) {
	for _, c := range []struct {
		msg  string
		want bool
	}{
		{"rewrite templates/dashboard.html", true},
		{"Rewrite templates/dashboard.html to display a clean SaaS-style metrics dashboard", true},
		{"create a new flask blueprint", true},
		{"add a logout button to the header", true},
		{"implement a /health endpoint", true},
		{"build a metrics page", true},
		{"refactor app.py to use blueprints", true},
		{"replace the hero section with a new one", true},
		{"update the dashboard to show three KPI cards", true},
		{"modify the User model to track login_at", true},
		{"change the dashboard layout to flex", true},
		{"convert this to TypeScript", true},
		{"redesign templates/index.html for SaaS", true},
		{"thanks", false},
		{"what does this code do", false},
		{"explain the dashboard route", false},
		{"why isn't this working", false},
		// "remove" is in no word list; wantsStateChange covers it by what the run inspected.
		{"remove the debug logging from app.py", false},
		{"please explain this, do not change any code", false},
		{"the parser is fine, do not change it", false},
	} {
		t.Run(c.msg, func(t *testing.T) {
			if got := isActionIntentMessage(c.msg); got != c.want {
				t.Errorf("isActionIntentMessage = %v, want %v", got, c.want)
			}
		})
	}
}

// classifyAgentTier is in task_tier.go. These rows are the two rules of this file
// that it reads: an explain-only request is chat, and a negated clause in a
// real task does not make the task chat. The last two rows are task_tier.go's own
// rules: a question with no question mark, read by its opening words, is chat,
// and a bug report, which is neither task wording nor a question, is not.
func TestClassifyAgentTierReadsExplainOnlyAndNegation(t *testing.T) {
	for _, c := range []struct {
		msg  string
		chat bool
	}{
		{"In orders.py, what does find_duplicates do, and what is its time complexity? Just explain — do not change any code.", true},
		{"In orders.py, apply_discount(100, 10) returns 90.0 but a colleague says it should return 90. " +
			"Explain what is going on here and whether it is actually a bug. Do not change the code.", true},
		{"Explain how the retry logic works, without editing anything.", true},
		{"fix the parser but don't change the public API", false},
		{"refactor store.py without changing its behaviour", false},
		{"why does the game store direction as a string", true},
		{"the sidebar overlaps the content on narrow screens", false},
	} {
		t.Run(c.msg, func(t *testing.T) {
			if got := classifyAgentTier(c.msg); (got == Tier0Conversational) != c.chat {
				t.Errorf("classifyAgentTier = %v, want chat: %v", got, c.chat)
			}
		})
	}
}

// isQuestionMessage is in task_tier.go.
func TestQuestionDetectedMidMessage(t *testing.T) {
	if !isQuestionMessage("what does find_duplicates do? Just explain.") {
		t.Error("a question followed by a qualifier is still a question")
	}
	if !isQuestionMessage("In orders.py, what does find_duplicates do") {
		t.Error("a question starter after a clause boundary must count")
	}
	if isQuestionMessage("add a pause toggle to app.py") {
		t.Error("a plain instruction is not a question")
	}
}

// wantsStateChange decides whether done needs a change on disk: by the
// request's wording, or, for wording no list covers, by a run that went into
// the project on a request that is not chat. A request that is answered by
// looking does not need one.
func TestWantsStateChange(t *testing.T) {
	for _, c := range []struct {
		name      string
		msg       string
		tier      Tier
		inspected bool
		want      bool
	}{
		{"action wording, before anything is inspected", "add a logout button to the navbar", Tier2Medium, false, true},
		{"a removal, after the run inspected the project", "remove the debug logging from app.py", Tier2Medium, true, true},
		{"a bug report, after the run inspected the project", "the sidebar overlaps the content on narrow screens", Tier2Medium, true, true},
		{"a question that opened files", "why does the game store direction as a string", Tier0Conversational, true, false},
		{"an acknowledgement, with nothing inspected", "thanks, that looks great", Tier2Medium, false, false},
		{"read-only: list the files", "Please list the files in the directory you are in.", Tier1Simple, true, false},
		{"read-only: list files", "list files", Tier1Simple, true, false},
		{"read-only: show me", "show me the config", Tier1Simple, true, false},
		{"read-only: what files", "what files are in src/", Tier1Simple, true, false},
		{"read-only: where is", "where is the router defined", Tier1Simple, true, false},
		{"read-only: print the", "print the contents of app.py", Tier1Simple, true, false},
		{"an action verb outranks a read verb", "add a list of files to the README", Tier1Simple, true, true},
		{"fix intent outranks show me", "show me the bug and repair it", Tier1Simple, true, true},
		{"fix intent outranks find", "find and fix the bug in the parser", Tier1Simple, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := wantsStateChange(c.msg, c.tier, c.inspected); got != c.want {
				t.Errorf("wantsStateChange(%q) = %v, want %v", c.msg, got, c.want)
			}
		})
	}
}

// A change the user forbade is never also demanded, whatever the task
// contract says: the read-only guard refuses every edit of such a run, so a
// demand for one could never be met.
func TestAForbiddenChangeIsNeverAlsoDemanded(t *testing.T) {
	work := &TaskContract{TaskMode: TaskModeWork}
	for _, c := range []struct {
		name     string
		contract *TaskContract
		ask      string
		tier     Tier
		required bool
	}{
		{"just explain, under a work contract", work,
			"In orders.py, what does find_duplicates do, and what is its time complexity? Just explain — do not change any code.", Tier0Conversational, false},
		{"explain, then forbid, with no contract", nil,
			"Explain what is going on here and whether it is actually a bug. Do not change the code.", Tier2Medium, false},
		{"tell me, under a work contract", work,
			"Find the cause. Tell me which file and which comparison is wrong — do not change any code.", Tier0Conversational, false},
		{"ordinary work under a work contract", work, "add a median() function to stats.py", Tier2Medium, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if d := decideActionDemand(c.contract, c.ask, c.tier, true); d.Required != c.required {
				t.Errorf("Required = %v, want %v (source %s)", d.Required, c.required, d.Source)
			}
		})
	}
}

// The file a task asks for, which the run is held to at the end. A file the
// task only reads is not one.
func TestExpectedOutputPaths(t *testing.T) {
	for _, c := range []struct {
		prompt string
		want   []string
	}{
		{"Please save your solution in the file sol.sql", []string{"sol.sql"}},
		{"Recover the rows and create a JSON file in recover.json", []string{"recover.json"}},
		{"Write the output to out.txt", []string{"out.txt"}},
		{"create a file called out.html that survives the filter", []string{"out.html"}},
		{"read the file input.txt and write the result to output.txt", []string{"output.txt"}},
		{"I have a sqlite database in trunc.db that was corrupted", nil},
		{"the final repository must include repo/algo.py. The file repo/algo.py must exist " +
			"in the merged result and must contain a function named map", []string{"repo/algo.py"}},
	} {
		t.Run(c.prompt, func(t *testing.T) {
			if got := expectedOutputPaths(c.prompt); !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestTheGateMessagesNameWhatIsMissing(t *testing.T) {
	for _, c := range []struct {
		name, msg string
		wants     []string
	}{
		{"the task's output file does not exist", expectedOutputMissingMessage([]string{"sol.sql"}),
			[]string{"`sol.sql`", "does not exist"}},
		{"nothing changed on disk", actionWithoutProductiveChangeMessage("rewrite templates/dashboard.html..."),
			[]string{"Cannot declare `done`", "write_file", "edit_file", "structural_edit", "Verification", "NOT the task"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, want := range c.wants {
				if !strings.Contains(c.msg, want) {
					t.Errorf("message lacks %q: %s", want, c.msg)
				}
			}
		})
	}
}

// A placeholder passes every syntax gate and ships nothing. Short content
// with real substance is not one.
func TestLooksLikeStub(t *testing.T) {
	pricingPage := `<!DOCTYPE html>
<html><head><title>Pricing</title></head>
<body>
  <h1>Pricing Page</h1>
  <p>Choose the plan that fits your needs.</p>
  <ul>
    <li>Free — $0/mo: 1 project, 100 calls/day</li>
    <li>Pro — $20/mo: unlimited projects, 10k calls/day</li>
    <li>Team — $80/mo: SSO, audit log, priority support</li>
  </ul>
  <p>All plans include a 14-day trial.</p>
</body></html>`
	for _, c := range []struct {
		name, path, content string
		stub                bool
	}{
		{"an html page that is only a heading", "templates/pricing.html",
			"<!DOCTYPE html>\n<html>\n<head>\n    <title>Pricing</title>\n</head>\n<body>\n    <h1>Pricing Page</h1>\n</body>\n</html>", true},
		{"an html page with content", "templates/pricing.html", pricingPage, false},
		{"python that is only pass", "widget.py", "pass", true},
		{"python that is only a to-do", "widget.py", "# TODO: implement\n", true},
		{"a python one-liner", "widget.py", "from flask import Blueprint\nbp = Blueprint('widget', __name__)\n", false},
		{"empty content", "a.txt", "", true},
		{"a short shell script", "scripts/probe.sh", "#!/bin/sh\nexec curl -sf http://localhost:8080/health\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeStub(c.path, c.content); (got != "") != c.stub {
				t.Errorf("looksLikeStub = %q, want a stub: %v", got, c.stub)
			}
		})
	}
}

// done is refused while a planned step never landed, but only on a plan that
// is evidence: one the planner rated well, with more than one step, and that
// the matcher is tracking (at least one step matched).
func TestPlanIncompleteMessage(t *testing.T) {
	plan := &Plan{WinningScore: 0.8, Steps: []PlanStep{
		{ID: "s1", Action: "replace setInterval with a variable-delay loop"},
		{ID: "s2", Action: "decrement the delay by 3ms per food eaten"},
	}}
	weak := &Plan{WinningScore: 0.2, Steps: plan.Steps}
	oneStep := &Plan{WinningScore: 0.8, Steps: plan.Steps[:1]}
	for _, c := range []struct {
		name      string
		plan      *Plan
		satisfied []bool
		wants     []string // nil: done may go ahead
	}{
		{"a planned step never landed", plan, []bool{true, false}, []string{"s2", "decrement the delay", "1 of 2"}},
		{"every step landed", plan, []bool{true, true}, nil},
		{"the planner rated the plan weak", weak, []bool{true, false}, nil},
		{"no step matched at all", plan, []bool{false, false}, nil},
		{"a one-step plan", oneStep, []bool{false}, nil},
		{"no plan", nil, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			msg := planIncompleteMessage(&AgentContext{Plan: c.plan, PlanStepsSatisfied: c.satisfied})
			if (msg != "") != (c.wants != nil) {
				t.Fatalf("message %q, want a refusal: %v", msg, c.wants != nil)
			}
			for _, want := range c.wants {
				if !strings.Contains(msg, want) {
					t.Errorf("message lacks %q:\n%s", want, msg)
				}
			}
		})
	}
}

// Only an explicit prohibition in the user's words forbids a change. A
// question alone does not, nor does work that also asks for an explanation.
func TestMutationForbidden(t *testing.T) {
	writeFile := getTool("write_file")
	for _, c := range []struct {
		ask       string
		forbidden bool
	}{
		{"In orders.py, what does find_duplicates do, and what is its time complexity? Just explain — do not change any code.", true},
		{"Find what is causing it and tell me — do not fix it yet.", true},
		{"add a median() function to stats.py", false},
		{"what does find_duplicates do?", false},
		{"explain the retry logic and then add a test for it", false},
	} {
		t.Run(c.ask, func(t *testing.T) {
			ctx := &AgentContext{HumanTask: c.ask}
			if got := mutationForbidden(ctx); got != c.forbidden {
				t.Errorf("mutationForbidden = %v, want %v", got, c.forbidden)
			}
			if got := readOnlyRequestRefusal(writeFile, ctx); (got != "") != c.forbidden {
				t.Errorf("write_file refusal %q, want a refusal: %v", got, c.forbidden)
			}
		})
	}
}

// Under an explicit read-only request every tool that can change the
// workspace is refused, the shell included; reading stays available, because
// the answer still needs it.
func TestAnExplicitReadOnlyRequestRefusesEveryMutatingTool(t *testing.T) {
	ctx := &AgentContext{HumanTask: "Just explain what this does — do not change any code."}
	for _, c := range []struct {
		tool    string
		refused bool
	}{
		{"write_file", true}, {"edit_file", true}, {"structural_edit", true}, {"insert_after", true},
		{"replace_lines", true}, {"delete_file", true}, {"move_file", true},
		{"run_command", true}, {"run_background", true},
		{"read_file", false}, {"outline_file", false}, {"search_files", false},
		{"list_directory", false}, {"find_file", false},
	} {
		t.Run(c.tool, func(t *testing.T) {
			tool := getTool(c.tool)
			if tool == nil {
				t.Fatalf("tool %s is not registered", c.tool)
			}
			if got := readOnlyRequestRefusal(tool, ctx); (got != "") != c.refused {
				t.Errorf("refusal %q, want a refusal: %v (effect %q)", got, c.refused, tool.Effect)
			}
		})
	}
	msg := readOnlyRequestRefusal(getTool("write_file"), ctx)
	for _, want := range []string{"not run", "NOT to change", "read_file", `"type":"text"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not point at the answer (lacks %q):\n%s", want, msg)
		}
	}
}

// Running a test that imports the fix verifies the fix: coverage follows the
// command into the workspace files it loads. A later rewrite of a covered file
// takes that coverage back.
func TestRunningATestCoversTheCodeTheTestImports(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.TaskContract = &TaskContract{TaskMode: TaskModeWork}
	writeAsTheModel(t, ctx, "stats.py", "def mean(v):\n    return sum(v) / len(v)\n\n\ndef median(v):\n    s = sorted(v)\n    return s[len(s) // 2]\n")
	writeAsTheModel(t, ctx, "test_stats.py", "from stats import mean, median\n\nassert mean([1, 2, 3]) == 2\nassert median([3, 1, 2]) == 2\nprint('passed')\n")

	covered := coverageForGreenCommand(ctx, "python3 test_stats.py")
	if _, ok := covered[resolveAgentPath(ctx, "stats.py")]; !ok {
		t.Fatalf("running the test did not cover the code it imports; covered=%v", covered)
	}
	ctx.VerificationEvidence = append(ctx.VerificationEvidence, VerificationRecord{
		Command: "python3 test_stats.py", Covered: covered, Turn: 1,
	})
	if d := decideVerificationDemand(ctx, ctx.TaskContract, nil); d.Required && !d.Met {
		t.Errorf("a green test over the deliverable left verification unmet (missing %q)", d.Missing)
	}

	writeAsTheModel(t, ctx, "stats.py", "def mean(v):\n    return 0\n")
	if d := decideVerificationDemand(ctx, ctx.TaskContract, nil); d.Met {
		t.Error("rewriting a covered file after a green run left it verified")
	}
}

// writeAsTheModel writes a workspace file through the write_file tool, so the
// session records it as the model's own write.
func writeAsTheModel(t *testing.T, ctx *AgentContext, rel, body string) {
	t.Helper()
	args, err := json.Marshal(map[string]string{"path": rel, "content": body})
	if err != nil {
		t.Fatal(err)
	}
	if res := executeToolCall("write_file", args, ctx); res == nil || !res.Success {
		t.Fatalf("write of %s failed: %+v", rel, res)
	}
}

// Coverage stops at what the command ran: its entry point and the files that
// code refers to. A file nothing refers to stays uncovered.
func TestCoverageDoesNotSpreadToUnreferencedFiles(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	writeAsTheModel(t, ctx, "driver.py", "# prints some stats about a list\nmy_stats = [1, 2, 3]\nprint(sum(my_stats))\n")
	writeAsTheModel(t, ctx, "unrelated.py", "def untouched():\n    return 1\n")

	covered := coverageForGreenCommand(ctx, "python3 driver.py")
	if _, ok := covered[resolveAgentPath(ctx, "driver.py")]; !ok {
		t.Errorf("the named entry point itself was not covered: %v", covered)
	}
	if _, ok := covered[resolveAgentPath(ctx, "unrelated.py")]; ok {
		t.Errorf("coverage reached a file the run never referenced: %v", covered)
	}
}

// A file counts as loaded when the source names it outright, or imports its
// module by name. A word that only looks like the module name does not count.
func TestModuleReferenceRequiresImportContext(t *testing.T) {
	for _, c := range []struct {
		why, src string
		want     bool
	}{
		{"a python from-import", "from stats import median\n", true},
		{"a python import", "import stats\n", true},
		{"a js require", "const s = require('./stats')\n", true},
		{"the file named outright", "print(open('stats.py').read())\n", true},
		{"a mention in prose", "# these are the stats we want\n", false},
		{"a longer identifier is another module", "statsd = 1\nimport statsd\n", false},
	} {
		t.Run(c.why, func(t *testing.T) {
			if got := sourceReferencesPath(c.src, "/w/stats.py"); got != c.want {
				t.Errorf("sourceReferencesPath(%q) = %v, want %v", c.src, got, c.want)
			}
		})
	}
}

// exitGates is in gates_exit.go. Each gate has its own bounce budget: one gate
// that spends all of its bounces does not silence the next, and once both are
// spent the exit goes through. A spent budget is not a passing verification.
func TestEachExitGateHasItsOwnBounceBudget(t *testing.T) {
	ctx := &AgentContext{WorkingDir: t.TempDir(), Tier: Tier2Medium}
	// A repair request that nothing verified and nothing changed: the
	// verification gate and the action gate both hold.
	st := &runState{userWantsVerification: true, inspectedWorkspace: true}
	var got []string
	for range 2*maxGateBounces + 1 {
		gate, rejection := st.exitGates(ctx, "fix the pause toggle in app.py so the spacebar works", "done")
		if gate != "" && rejection == "" {
			t.Errorf("%s bounced the exit with no rejection", gate)
		}
		got = append(got, gate)
	}
	want := append(slices.Repeat([]string{"verification_gate"}, maxGateBounces),
		slices.Repeat([]string{"action_gate"}, maxGateBounces)...)
	want = append(want, "")
	if !slices.Equal(got, want) {
		t.Errorf("gates in order of the exits: %q, want %q", got, want)
	}
	spent := map[string]int{"verification_gate": maxGateBounces, "action_gate": maxGateBounces}
	if !maps.Equal(st.gateBounces, spent) {
		t.Errorf("bounces charged: %v, want %v", st.gateBounces, spent)
	}
	if !st.verificationDemandedAndUnmet() {
		t.Error("the spent verification budget was taken for a passing verification")
	}
}

// resolveAgentPath is in tools.go. The guardrails compare paths through it,
// so a host path, a relative path and a "workspace/" prefix name one file.
func TestResolveAgentPath(t *testing.T) {
	container := &AgentContext{WorkingDir: "/workspace", HostWorkingDir: "/home/isaac/snake"}
	noHost := &AgentContext{WorkingDir: "/tmp/proj"}
	for _, c := range []struct {
		name     string
		ctx      *AgentContext
		in, want string
	}{
		{"a host path becomes the container path", container, "/home/isaac/snake/app.py", "/workspace/app.py"},
		{"the host root itself", container, "/home/isaac/snake", "/workspace"},
		{"the host root with a trailing slash", container, "/home/isaac/snake/", "/workspace"},
		{"an absolute path outside the host root", container, "/etc/passwd", "/etc/passwd"},
		{"a lookalike of the host root", container, "/home/isaac/snakebar/app.py", "/home/isaac/snakebar/app.py"},
		{"a workspace/ prefix is the project root", container, "workspace/app.py", "/workspace/app.py"},
		{"workspace alone is the project root", container, "workspace", "/workspace"},
		{"./workspace/ is the project root", container, "./workspace/app.py", "/workspace/app.py"},
		{"a relative path with no prefix", container, "app.py", "/workspace/app.py"},
		{"an absolute path with no host mapping", noHost, "/home/x/file.py", "/home/x/file.py"},
		{"a relative path joins the working directory", noHost, "src/x.py", "/tmp/proj/src/x.py"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveAgentPath(c.ctx, c.in); got != c.want {
				t.Errorf("resolveAgentPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
