package main

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A green run counts as verification only when it shows the program working:
// it ran the program or its tests, or fetched a response body, and the line
// reports that part's exit status. classifyCommandEvidence, in
// command_evidence.go, makes the call.
func TestIsVerificationCommand(t *testing.T) {
	for _, group := range []struct {
		kind     string
		verifies bool
		cmds     []string
	}{
		{"a run of the program or its tests, or a fetched body", true, []string{
			"pytest tests/",
			"python app.py",
			"python3 -m pytest",
			"go test ./...",
			"cargo test",
			"npm test",
			"make test",
			"java App.java",
			"javac App.java && java App",
			"php index.php",
			"bash run.sh",
			"./solve.py",
			"curl -sf http://localhost:5000/",
			"curl -s --fail http://localhost:5000/api",
			"curl -s http://localhost:5000/api | grep -q ok",
			"wget -qO- http://localhost:8000",
		}},
		{"a build, a parse, a lint or a version: well formed is not working", false, []string{
			"go build",
			"npm run build",
			"ruff check src/",
			"mypy app.py",
			"markdownlint README.md",
			"shellcheck scripts/setup.sh",
			"golangci-lint run ./...",
			"python3 -m py_compile app.py",
			"node --check app.js",
			"python3 --version",
		}},
		{"a run whose status the line hides", false, []string{
			"pytest | tail -5",
			"pytest || true",
			"python3 app.py; echo done",
		}},
		{"a curl without -f, which exits 0 on an error page", false, []string{
			"curl http://localhost:8000",
		}},
		{"a probe of the headers only, which a broken page passes", false, []string{
			"curl -I http://localhost:8000",
			"curl --head http://localhost:5000/",
			"curl -sI http://localhost:8000",
			"wget --spider http://localhost:8000",
		}},
		{"looking around, or installing", false, []string{
			"ls -la",
			"cat app.py",
			"grep -r TODO src/",
			"find . -name '*.py'",
			"echo hello",
			"pip install flask",
			"python3 -m pip install flask",
		}},
	} {
		for _, cmd := range group.cmds {
			t.Run(cmd, func(t *testing.T) {
				if got := classifyCommandEvidence(cmd).Kind.verifies(); got != group.verifies {
					t.Errorf("verifies = %v for %s, want %v", got, group.kind, group.verifies)
				}
			})
		}
	}
}

// Every probe a refusal tells the model to run has to count as verification,
// or the refusal sends the model in a circle.
func TestTheProbeARedirectPrescribesCountsAsVerification(t *testing.T) {
	// A probe is quoted in backticks, alone or as run_command("...").
	probe := regexp.MustCompile("`(?:run_command\\(\")?(curl[^`\"]*)")
	for _, c := range []struct {
		name, msg string
		names     bool // the message names a probe
	}{
		{"the foreground-server redirect", foregroundServerRejectionWithSource("python3 -m http.server 8000", nil), true},
		{"the gate on a repair request", verificationRejection(false, false, "job1"), true},
		{"the gate after a blocked server start", verificationRejection(false, true, "job1"), true},
		{"the gate after a red run", verificationRejection(true, false, "job1"), false},
		{"the gate after a red, blocked server start", verificationRejection(true, true, "job1"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			probes := probe.FindAllStringSubmatch(c.msg, -1)
			if (len(probes) > 0) != c.names {
				t.Fatalf("probes %q, want a probe named: %v:\n%s", probes, c.names, c.msg)
			}
			for _, p := range probes {
				if !classifyCommandEvidence(p[1]).Kind.verifies() {
					t.Errorf("the message prescribes %q, which does not count as verification", p[1])
				}
			}
		})
	}
}

// The log line names why the gate fired. A red run outranks the wording of
// the request: it is what actually happened.
func TestGateTriggerPrefersTheConcreteSignal(t *testing.T) {
	for _, c := range []struct {
		userWantsVerification, sawFailedVerification bool
		want                                         string
	}{
		{true, true, "failed-verification"},
		{true, false, "fix-intent"},
		{false, false, "none"},
	} {
		t.Run(c.want, func(t *testing.T) {
			if got := gateTrigger(c.userWantsVerification, c.sawFailedVerification); got != c.want {
				t.Errorf("gateTrigger = %q, want %q", got, c.want)
			}
		})
	}
}

// The refusal says which of its reasons holds. After a red run the model has
// seen the output and needs to make the edit; after a streak of red runs, to
// start the file again.
func TestTheVerificationGateSaysWhyItRefused(t *testing.T) {
	for _, c := range []struct {
		name, msg string
		holds     map[string]bool // a phrase, and whether the message holds it
	}{
		{"a red run", verificationRejection(true, false, ""),
			map[string]bool{"FAILED": true, "Describing the fix is not applying it": true, "fix/repair request": false}},
		{"a repair request that nothing verified", verificationRejection(false, false, ""),
			map[string]bool{"fix/repair request": true, "FAILED": false}},
		{"three red runs in a row", verificationRejectionFor(true, false, "", 3, "", "", ""),
			map[string]bool{"Rewrite the file from scratch": true}},
		{"one red run", verificationRejectionFor(true, false, "", 1, "", "", ""), map[string]bool{"from scratch": false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for phrase, held := range c.holds {
				if strings.Contains(c.msg, phrase) != held {
					t.Errorf("message holds %q: %v, want %v:\n%s", phrase, !held, held, c.msg)
				}
			}
		})
	}
}

// verificationDemandedAndUnmet is in agent.go; the verification gate fires on
// the same condition: a red run, or a repair request, with no passing run
// since. That a spent bounce budget leaves it unmet is checked in
// TestEachExitGateHasItsOwnBounceBudget.
func TestVerificationDemandedAndUnmet(t *testing.T) {
	for _, c := range []struct {
		name  string
		st    *runState
		unmet bool
	}{
		{"a red run, on a request with no repair word", &runState{sawFailedVerification: true}, true},
		{"a repair request that nothing verified", &runState{userWantsVerification: true}, true},
		{"a passing run since the red one", &runState{sawFailedVerification: true, verifiedThisLoop: true}, false},
		{"nothing asked for, nothing run", &runState{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.st.verificationDemandedAndUnmet(); got != c.unmet {
				t.Errorf("verificationDemandedAndUnmet = %v, want %v", got, c.unmet)
			}
		})
	}
}

// Running a file, for the run-first gate, means an executor ran it: by name,
// through -m, directly, or by importing it in inline code. Naming the file is
// not running it.
func TestImportingAModuleCountsAsRunningIt(t *testing.T) {
	for _, c := range []struct {
		why, cmd, path string
		want           bool
	}{
		{"inline code that imports it", `python3 -c "import store; store.complete_todo(1)"`, "store.py", true},
		{"inline code with a from-import", `python3 -c "from store import add; add('x')"`, "store.py", true},
		{"named directly", `python3 store.py`, "store.py", true},
		{"named through -m", `python3 -m pytest test_store.py`, "test_store.py", true},
		{"run directly", `./store.py`, "store.py", true},
		{"inline code that imports another module", `python3 -c "import other; other.go()"`, "store.py", false},
		{"no executor", `echo "import store"`, "store.py", false},
		{"inline code that only mentions the name", `python3 -c "print('store is fine')"`, "store.py", false},
		{"reading is not running", `cat store.py`, "store.py", false},
	} {
		t.Run(c.why, func(t *testing.T) {
			if got := executionAttempt(c.cmd, c.path); got != c.want {
				t.Errorf("executionAttempt(%q, %q) = %v, want %v", c.cmd, c.path, got, c.want)
			}
		})
	}
}

// A runner that found nothing to run has not shown the program broken, so it
// does not latch the gate. A real failure still does, including one that says
// "No such file or directory" about a file the program should have made.
func TestACommandThatNeverRanIsNotAFailedVerification(t *testing.T) {
	for _, c := range []struct {
		name     string
		result   *ToolResult
		neverRan bool
	}{
		{"pytest found no such file", &ToolResult{Error: "ERROR: file or directory not found: tests/test_syntax_check.py"}, true},
		{"pytest ran no tests", &ToolResult{Error: "collected 0 items\nno tests ran in 0.01s"}, true},
		{"python could not open the script",
			&ToolResult{Error: "python3: can't open file '/w/missing.py': [Errno 2] No such file or directory"}, true},
		{"an assertion failed", &ToolResult{Error: "FAILED test_store.py::test_add - assert 0 == 1\n1 failed, 2 passed"}, false},
		{"the program did not create its output",
			&ToolResult{Error: "FileNotFoundError: [Errno 2] No such file or directory: 'todos.json'"}, false},
		{"no result", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := verificationNeverRan(c.result); got != c.neverRan {
				t.Errorf("verificationNeverRan = %v, want %v", got, c.neverRan)
			}
		})
	}
}

// phase_solved starts as "none" and is overwritten only when a candidate
// passes, so only the phases that mean a pass count as verified.
func TestVerifiedPhase(t *testing.T) {
	for _, c := range []struct {
		phase    string
		verified bool
	}{
		{"probe", true},
		{"phase1", true},
		{"pr_cot", true},
		{"refinement", true},
		{"budget", true},
		{"none", false},
	} {
		t.Run(c.phase, func(t *testing.T) {
			if got := verifiedPhase(c.phase); got != c.verified {
				t.Errorf("verifiedPhase = %v, want %v", got, c.verified)
			}
		})
	}
}

// A program that exits 0 and prints nothing, on a task that promised printed
// output, is not verified: a comment may have swallowed the end of the file.
func TestASilentRunIsNotVerificationWhenOutputWasPromised(t *testing.T) {
	prints := "Write solve.py that reads input.txt and prints the answer on a single line."
	quiet := `{"stdout":"","stderr":"","exit_code":0}`
	for _, c := range []struct {
		name, prompt, cmd, data string
		silent                  bool
	}{
		{"no output on a task that promised it", prints, "python3 solve.py", quiet, true},
		{"real output", prints, "python3 solve.py", `{"stdout":"42 7","exit_code":0}`, false},
		{"a task that promised no output", "fix the bug in app.py", "python3 app.py", quiet, false},
		{"a build step, which prints nothing", prints, "go build ./...", quiet, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := silentRunWhenOutputPromised(&AgentContext{}, c.prompt, c.cmd, json.RawMessage(c.data)); got != c.silent {
				t.Errorf("silentRunWhenOutputPromised = %v, want %v", got, c.silent)
			}
		})
	}
}

// resolveVerifyTarget is in main.go.
// Only the operator chooses the host, with both settings. The workspace's
// settings file may choose the sandbox; a file that names the host is
// ignored, with a log line.
func TestOnlyTheOperatorChoosesTheHostAsTheVerifyTarget(t *testing.T) {
	for _, c := range []struct {
		name, verifyIn, trustMode, fileTarget, want string
		wantLogLine                                 bool
	}{
		{"the defaults", "", "", "", "sandbox", false},
		{"the operator's host, fully trusted", "host", "fully-trusted", "", "host", false},
		{"the operator's host, with the default trust mode", "host", "", "", "sandbox", false},
		{"the workspace file names the host", "", "", "host", "sandbox", true},
		{"the workspace file names the sandbox", "host", "fully-trusted", "sandbox", "sandbox", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ATLAS_VERIFY_IN", c.verifyIn)
			t.Setenv("ATLAS_TRUST_MODE", c.trustMode)
			dir := t.TempDir()
			if c.fileTarget != "" {
				writeInWorkspace(t, dir, filepath.Join(".atlas", "config.toml"), "[execution]\ntarget = \""+c.fileTarget+"\"\n")
			}
			var got string
			logged := captureLog(t, func() { got = resolveVerifyTarget(dir) })
			if got != c.want {
				t.Errorf("verify target = %q, want %q", got, c.want)
			}
			if strings.Contains(logged, `names target = "host"; ignored`) != c.wantLogLine {
				t.Errorf("log = %q; want the line that the file's host is ignored: %v", logged, c.wantLogLine)
			}
		})
	}
}
