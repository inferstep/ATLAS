package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The audit's probes (P-guardrails/INTEGRITY#1-2, P-agent-1/INTEGRITY#1-2,
// P-agent-3/INTEGRITY#2, GB-4#3) are the cases below: a parse, a lint or a
// `--version` counted as verification, `| tail` and `|| true` hid a failing
// test, and a red run after a green one never blocked completion.

func TestClassifyCommandEvidence(t *testing.T) {
	cases := []struct {
		cmd    string
		kind   commandEvidenceKind
		masked bool
	}{
		// The program or its tests ran, and the line reports how.
		{"python3 app.py", evidenceExecution, false},
		{"timeout 10 python3 app.py", evidenceExecution, false},
		{"FOO=1 python3 app.py", evidenceExecution, false},
		{"cd app && python3 main.py", evidenceExecution, false},
		{"(cd app && pytest)", evidenceExecution, false},
		{"(cd app && pytest) > log.txt 2>&1", evidenceExecution, false},
		{`bash -c "pytest -q"`, evidenceExecution, false},
		{`bash -lc 'pytest -q'`, evidenceExecution, false},
		{`sh -ec "python3 a.py; python3 b.py"`, evidenceExecution, false},
		{`bash -euo pipefail -c "pytest | tail -5"`, evidenceExecution, false},
		{"uv run pytest", evidenceExecution, false},
		{"uv run --with requests app.py", evidenceExecution, false},
		{"poetry run python app.py", evidenceExecution, false},
		{"bundle exec rspec", evidenceExecution, false},
		{"npx --yes jest", evidenceExecution, false},
		{"node app.js", evidenceExecution, false},
		{"java -jar app.jar", evidenceExecution, false},
		{"java App.java", evidenceExecution, false},
		{"javac App.java && java App", evidenceExecution, false},
		{"php index.php", evidenceExecution, false},
		{"bash run.sh", evidenceExecution, false},
		{"go run main.go", evidenceExecution, false},
		{"./solve.py", evidenceExecution, false},
		{"echo '1 2' | python3 solve.py", evidenceExecution, false},
		{"python3 solve.py < input.txt", evidenceExecution, false},
		{"pytest || exit 1", evidenceExecution, false},
		{"set -o pipefail; pytest | tail -5", evidenceExecution, false},
		{"set -e; pytest; echo done", evidenceExecution, false},
		{"make test", evidenceExecution, false},
		{"make check", evidenceExecution, false},
		{"python3 -m unittest discover", evidenceExecution, false},
		{"python3 - <<'EOF'\nimport app\nprint(app.solve())\nEOF", evidenceExecution, false},

		// Well formed is not working.
		{"python3 -m py_compile app.py", evidenceStatic, false},
		{`python3 -c "import ast; ast.parse(open('app.py').read())"`, evidenceStatic, false},
		{`python3 -c "import ast, sys; ast.parse(open(sys.argv[1]).read())" app.py`, evidenceStatic, false},
		{"node --check app.js", evidenceStatic, false},
		{"bash -n run.sh", evidenceStatic, false},
		{"php -l index.php", evidenceStatic, false},
		{"ruff check .", evidenceStatic, false},
		{"mypy app.py", evidenceStatic, false},
		{"npx tsc --noEmit", evidenceStatic, false},
		{"go build ./...", evidenceStatic, false},
		{"go vet ./...", evidenceStatic, false},
		{"cargo build", evidenceStatic, false},
		{"npm run build", evidenceStatic, false},
		{"npm run lint:fix", evidenceStatic, false},
		{"yarn build", evidenceStatic, false},
		{"make", evidenceStatic, false},
		{"make lint", evidenceStatic, false},
		{"make typecheck", evidenceStatic, false},
		{"pytest --collect-only -q", evidenceStatic, false},
		{"python3 -m pytest --co", evidenceStatic, false},
		{"python3 --version", evidenceStatic, false},
		{"node -v", evidenceStatic, false},
		{"python3 app.py --help", evidenceStatic, false},
		{"node app.js --version", evidenceStatic, false},
		{"./app --version", evidenceStatic, false},
		{"python3 -m py_compile app.py && echo OK", evidenceStatic, false},

		// Recon and setup.
		{"ls -la", evidenceNone, false},
		{"cat app.py", evidenceNone, false},
		{"echo hi", evidenceNone, false},
		{"pip install flask", evidenceNone, false},
		{"python3 -m pip install flask", evidenceNone, false},
		{"python3 -m venv .venv", evidenceNone, false},
		{"npm install", evidenceNone, false},
		{"make install", evidenceNone, false},
		{"curl -I http://localhost:5000/", evidenceNone, false},

		// The verifying part ran, and the line's status is something else's.
		{"pytest | tail -5", evidenceNone, true},
		{"pytest -q 2>&1 | tail -20", evidenceNone, true},
		{"pytest || true", evidenceNone, true},
		{"python3 app.py; echo done", evidenceNone, true},
		{"python3 app.py &", evidenceNone, true},
		{"python3 app.py 2>&1 | tee out.log", evidenceNone, true},
		{"pytest && echo PASS || echo FAIL", evidenceNone, true},
		{"python3 app.py | grep -q 42", evidenceNone, true},
		{`bash -c "pytest | tail -5"`, evidenceNone, true},
		// curl without -f exits 0 on an error page.
		{"curl http://localhost:5000/", evidenceNone, true},
		{"curl -s http://localhost:5000/ | head -5", evidenceNone, true},

		// A page fetched in a way an HTTP error would fail.
		{"curl -sf http://localhost:5000/", evidenceProbe, false},
		{"curl -s --fail-with-body http://localhost:5000/", evidenceProbe, false},
		{"curl -s http://localhost:5000/ | grep -q Welcome", evidenceProbe, false},
		{"curl -s http://localhost:5000/api | jq -e .ok", evidenceProbe, false},
		{"wget -qO- http://localhost:5000/", evidenceProbe, false},
		// The background start is hidden, the probe after it is not.
		{"python3 app.py & sleep 1 && curl -sf localhost:5000", evidenceProbe, true},
	}
	for _, c := range cases {
		ev := classifyCommandEvidence(c.cmd)
		if ev.Kind != c.kind || ev.Masked != c.masked {
			t.Errorf("%q: kind=%s masked=%v, want kind=%s masked=%v",
				c.cmd, ev.Kind, ev.Masked, c.kind, c.masked)
		}
		if ev.Masked && ev.MaskNote == "" {
			t.Errorf("%q: masked with no note for the model", c.cmd)
		}
	}
}

func TestMaskNoteNamesWhatHidTheResult(t *testing.T) {
	for cmd, want := range map[string]string{
		"pytest | tail -5":           "`| tail`",
		"pytest || true":             "`|| true`",
		"python3 app.py; echo done":  "`; echo`",
		"python3 app.py &":           "background",
		"curl http://localhost:5000": "curl -sf",
	} {
		if note := classifyCommandEvidence(cmd).MaskNote; !strings.Contains(note, want) {
			t.Errorf("%q: note %q does not name %s", cmd, note, want)
		}
	}
}

// Only a segment that ran something binds the files it names. A parse in the
// same chain names a file it never ran; a compile step whose output a later
// segment ran does bind its source.
func TestCoveringSegments(t *testing.T) {
	for cmd, want := range map[string][]string{
		"python3 -m py_compile a.py && python3 b.py": {"python3 b.py"},
		"javac App.java && java App":                 {"javac App.java", "java App"},
		"gcc main.c -o main && ./main":               {"gcc main.c -o main", "./main"},
		"ruff check a.py && pytest -q":               {"pytest -q"},
		"python3 -m py_compile a.py":                 nil,
	} {
		var got []string
		for _, seg := range classifyCommandEvidence(cmd).Covering {
			got = append(got, strings.TrimSpace(seg))
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%q: covering %q, want %q", cmd, got, want)
		}
	}
}

// A here-document body is data for the command that reads it, not shell text:
// its `|`, `;` and `&&` must not split the line, and the body stays with the
// segment so a reader of the command still sees what it imports.
func TestHereDocumentsAreNotSplit(t *testing.T) {
	cmd := "cat > app.py <<'EOF'\nimport os; print(1) | x && y\nEOF\npython3 app.py"
	segs, ops, bodies := splitTopLevelShell(cmd)
	if len(segs) != 2 || strings.TrimSpace(segs[1]) != "python3 app.py" {
		t.Fatalf("segs=%q ops=%q", segs, ops)
	}
	if bodies[0] != "import os; print(1) | x && y" || bodies[1] != "" {
		t.Fatalf("bodies=%q", bodies)
	}
	if ev := classifyCommandEvidence(cmd); ev.Kind != evidenceExecution || ev.Masked {
		t.Fatalf("heredoc then run: %+v", ev)
	}
	tabbed := "cat <<-EOF\n\tfoo; bar\n\tEOF\npytest"
	if segs, _, _ := splitTopLevelShell(tabbed); len(segs) != 2 {
		t.Fatalf("<<- body was split: %q", segs)
	}
	run := "python3 - <<'EOF'\nimport store\nstore.add(1)\nEOF"
	ev := classifyCommandEvidence(run)
	if len(ev.Honest) != 1 || !strings.Contains(ev.Honest[0], "import store") {
		t.Fatalf("the body did not stay with its command: %q", ev.Honest)
	}
	if !executionAttempt(strings.Join(ev.Honest, " && "), "store.py") {
		t.Fatal("an import inside a here-document is no longer an execution attempt")
	}
}

// --- what the loop records -----------------------------------------------------

func ranClean(stdout string) *ToolResult {
	b, _ := json.Marshal(RunCommandOutput{Stdout: stdout})
	return &ToolResult{Success: true, Data: b}
}

func ranRed(stderr string) *ToolResult {
	b, _ := json.Marshal(RunCommandOutput{Stderr: stderr, ExitCode: 1})
	return &ToolResult{Success: false, Data: b, Error: "exit status 1"}
}

// A red run after a green one takes the verification back: the latest result
// on these bytes is the one that describes them (P-agent-1/INTEGRITY#2).
func TestARedRunAfterAGreenOneTakesTheVerificationBack(t *testing.T) {
	tc := sepContract([]string{"app.py"}, nil)
	ctx, dir := sepCtx(t, tc)
	h := sepWrite(t, ctx, dir, "app.py", "print('ok')\n")
	st := &runState{userWantsVerification: true}

	st.observeVerification(ctx, "", 1, "python3 app.py", ranClean("ok\n"))
	if !st.verifiedThisLoop || st.verificationDemandedAndUnmet() {
		t.Fatal("a clean run of the program did not verify it")
	}
	if !pathCoverageSatisfied(ctx, resolveAgentPath(ctx, "app.py"), h) {
		t.Fatal("the clean run did not bind app.py")
	}

	st.observeVerification(ctx, "", 2, "python3 app.py", ranRed("Traceback: boom"))
	if st.verifiedThisLoop || !st.sawFailedVerification || !st.verificationDemandedAndUnmet() {
		t.Fatalf("a red run after a green one left the run verified: verified=%v latch=%v",
			st.verifiedThisLoop, st.sawFailedVerification)
	}
	if pathCoverageSatisfied(ctx, resolveAgentPath(ctx, "app.py"), h) {
		t.Fatal("a failed run over the same bytes left the earlier pass standing")
	}
	if d := decideVerificationDemand(ctx, tc, []string{"app.py"}); d.Met {
		t.Fatal("the work contract is still met by a pass the latest run contradicts")
	}

	st.observeVerification(ctx, "", 3, "python3 app.py", ranClean("ok\n"))
	if !st.verifiedThisLoop || st.sawFailedVerification {
		t.Fatal("a green run after the red one did not verify again")
	}
	if d := decideVerificationDemand(ctx, tc, []string{"app.py"}); !d.Met {
		t.Fatalf("the latest green run does not meet the contract: %+v", d)
	}
}

// A parse or a lint is not verification. It is not recorded, it does not
// discharge the contract, and the run is told why at the exit
// (P-agent-3/INTEGRITY#2, P-tools-2/INTEGRITY#1).
func TestAStaticCheckIsNeverVerification(t *testing.T) {
	tc := sepContract([]string{"app.py"}, nil)
	ctx, dir := sepCtx(t, tc)
	sepWrite(t, ctx, dir, "app.py", "print('ok')\n")
	st := &runState{userWantsVerification: true}

	for _, cmd := range []string{"python3 -m py_compile app.py", "ruff check app.py", "python3 --version && cat app.py"} {
		st.observeVerification(ctx, "", 1, cmd, ranClean(""))
	}
	if st.verifiedThisLoop || !st.verificationDemandedAndUnmet() {
		t.Fatal("a static check verified the run")
	}
	if len(ctx.VerificationEvidence) != 0 {
		t.Fatalf("an undeclared static check was recorded: %+v", ctx.VerificationEvidence)
	}
	if d := decideVerificationDemand(ctx, tc, []string{"app.py"}); d.Met {
		t.Fatal("a static check met the work contract")
	}
	if !strings.Contains(st.uncountedCheck, "well formed") {
		t.Fatalf("the run is not told why the check did not count: %q", st.uncountedCheck)
	}
}

// A test whose failure `| tail` hid is not a pass (P-guardrails/INTEGRITY#1).
func TestAHiddenResultIsNeverVerification(t *testing.T) {
	tc := sepContract([]string{"app.py"}, nil)
	ctx, dir := sepCtx(t, tc)
	sepWrite(t, ctx, dir, "app.py", "print('ok')\n")
	st := &runState{userWantsVerification: true}

	st.observeVerification(ctx, "", 1, "python3 app.py 2>&1 | tail -5", ranClean("Traceback: boom\n"))
	if st.verifiedThisLoop || len(ctx.VerificationEvidence) != 0 {
		t.Fatal("a run whose status `| tail` replaced was counted")
	}
	if !strings.Contains(st.uncountedCheck, "| tail") {
		t.Fatalf("the run is not told what hid the result: %q", st.uncountedCheck)
	}
	// And a red line whose verifying part was hidden says nothing either way.
	st.observeVerification(ctx, "", 2, "python3 app.py; false", ranRed(""))
	if st.sawFailedVerification {
		t.Fatal("a red line latched on a result it never reported")
	}
}

// A failed lint holds the gate until the same lint passes or a real run
// passes. A failed run is not cleared by a lint.
func TestAFailedCheckIsClearedOnlyByItsOwnPassOrARun(t *testing.T) {
	ctx, dir := sepCtx(t, nil)
	sepWrite(t, ctx, dir, "app.py", "print('ok')\n")

	st := &runState{}
	st.observeVerification(ctx, "", 1, "ruff check app.py", ranRed("E501"))
	if !st.sawFailedVerification {
		t.Fatal("a failed lint did not latch")
	}
	st.observeVerification(ctx, "", 2, "mypy app.py", ranClean(""))
	if !st.sawFailedVerification {
		t.Fatal("a different check cleared the lint's failure")
	}
	st.observeVerification(ctx, "", 3, "ruff check app.py", ranClean(""))
	if st.sawFailedVerification || st.verifiedThisLoop {
		t.Fatalf("the lint passing again: latch=%v verified=%v, want the latch clear and nothing verified",
			st.sawFailedVerification, st.verifiedThisLoop)
	}

	st = &runState{}
	st.observeVerification(ctx, "", 1, "python3 app.py", ranRed("Traceback"))
	st.observeVerification(ctx, "", 2, "ruff check app.py", ranClean(""))
	if !st.sawFailedVerification {
		t.Fatal("a lint cleared a failed run")
	}
	st.observeVerification(ctx, "", 3, "python3 app.py", ranClean("ok\n"))
	if st.sawFailedVerification || !st.verifiedThisLoop {
		t.Fatal("a passing run did not clear the failed one")
	}
}

// A command the client declared is recorded whatever it is, so the
// declaration can be discharged, and a static one still covers no file.
func TestADeclaredStaticCommandDischargesItselfAndCoversNothing(t *testing.T) {
	tc := sepContract([]string{"app.py"}, []string{"ruff check app.py"})
	ctx, dir := sepCtx(t, tc)
	sepWrite(t, ctx, dir, "app.py", "print('ok')\n")
	st := &runState{}

	st.observeVerification(ctx, "", 1, "ruff check app.py", ranClean(""))
	if !commandObligationSatisfied(ctx, "ruff check app.py") {
		t.Fatal("the declared command ran green and its obligation is still open")
	}
	d := decideVerificationDemand(ctx, tc, []string{"app.py"})
	if d.Met || d.MissingCommand || !strings.HasSuffix(d.Missing, "app.py") {
		t.Fatalf("a declared lint covered the file it linted: %+v", d)
	}
}

// The latest current run of a declared command decides it.
func TestADeclaredCommandIsDecidedByItsLatestRun(t *testing.T) {
	tc := sepContract(nil, []string{"pytest"})
	ctx, _ := sepCtx(t, tc)
	st := &runState{}

	st.observeVerification(ctx, "", 1, "pytest", ranClean("1 passed"))
	if !commandObligationSatisfied(ctx, "pytest") {
		t.Fatal("a green declared command did not discharge itself")
	}
	st.observeVerification(ctx, "", 2, "pytest", ranRed("1 failed"))
	if commandObligationSatisfied(ctx, "pytest") {
		t.Fatal("a pass followed by a failure on the same workspace still satisfies")
	}
	d := decideVerificationDemand(ctx, tc, nil)
	if d.Met || !d.MissingCommand || d.Missing != "pytest" {
		t.Fatalf("demand = %+v, want the declared command missing", d)
	}
	if msg := contractDemandMessage(ctx, d); !strings.Contains(msg, "the task requires `pytest`") {
		t.Fatalf("message does not name the declared command: %q", msg)
	}
}

// The unmet contract is said at the exit, while the run can still act on it,
// with the command that would run the file, relative to the workspace.
func TestTheExitSaysWhatTheContractStillOwes(t *testing.T) {
	tc := sepContract([]string{"app.py"}, nil)
	ctx, dir := sepCtx(t, tc)
	sepWrite(t, ctx, dir, "app.py", "print('ok')\n")
	st := &runState{madeProductiveChange: true, productiveChanges: 1, toolsRun: 2}
	st.observeVerification(ctx, "", 1, "python3 -m py_compile app.py", ranClean(""))

	gate, msg := st.exitGates(ctx, "Write app.py.", "Finished.")
	if gate != "verification_gate" {
		t.Fatalf("gate = %q, want verification_gate", gate)
	}
	for _, want := range []string{"`app.py`", `python3 app.py`, "well formed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("exit message %q does not contain %q", msg, want)
		}
	}
	if strings.Contains(msg, dir) {
		t.Errorf("exit message leaks the absolute workspace path: %q", msg)
	}
	for i := 0; i < maxGateBounces; i++ {
		st.exitGates(ctx, "Write app.py.", "Finished.")
	}
	if gate, _ := st.exitGates(ctx, "Write app.py.", "Finished."); gate == "verification_gate" {
		t.Fatal("the contract bounce is not capped")
	}
}

// An execution whose result the line hid did not come up clean, so it cannot
// settle content debt as "executed clean".
func TestAHiddenExecutionSettlesNoDebt(t *testing.T) {
	ctx, dir := sepCtx(t, nil)
	sepWrite(t, ctx, dir, "app.py", "print('ok')\n")
	key := ledgerKey(ctx, "app.py")
	// Content debt: bytes on disk that no route gave a verdict (see
	// TestACleanExecutionSettlesContentDebt).
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[key]
	d.ValidationKind, d.ValidationStatus, d.ValidatedHash = ValidationKindUnknown, ValidationUnknown, ""
	ctx.LedgerMu.Unlock()
	st := &runState{mutationDebt: map[string]*mutationDebtEntry{key: {Rel: "app.py", Kind: debtContent}}}
	settleDebtByExecution(ctx, st, "python3 app.py | tail -5", true)
	if len(st.mutationDebt) != 1 {
		t.Fatal("a run whose status `| tail` replaced settled content debt")
	}
	settleDebtByExecution(ctx, st, "python3 -m py_compile app.py", true)
	if len(st.mutationDebt) != 1 {
		t.Fatal("a parse settled content debt as an execution")
	}
	settleDebtByExecution(ctx, st, "python3 app.py", true)
	if len(st.mutationDebt) != 0 {
		t.Fatal("a clean run no longer settles content debt")
	}
}

// The run-first instruction names a command that runs the file.
func TestRunCommandForEachExecutableLanguage(t *testing.T) {
	for path, want := range map[string]string{
		"solve.py":  "python3 solve.py",
		"app.js":    "node app.js",
		"main.go":   "go run main.go",
		"App.java":  "java App.java",
		"Main.kt":   "kotlinc Main.kt -include-runtime -d Main.jar && java -jar Main.jar",
		"run.sh":    "bash run.sh",
		"index.php": "php index.php",
		"mod.mjs":   "node mod.mjs",
	} {
		if got := runCommandFor(path); got != want {
			t.Errorf("runCommandFor(%q) = %q, want %q", path, got, want)
		}
		if !isVerificationCommand(runCommandFor(path)) {
			t.Errorf("the command quoted for %s does not count as verification", path)
		}
	}
}

// --- runners that name no file ------------------------------------------------

// A bare test runner covers the session's files it discovers, and what they
// import, so a run verified only by `pytest`, `go test ./...` or `npm test`
// can meet a work contract.
func TestARunnerCoversWhatItDiscovers(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		command string
		covered []string
		not     []string
	}{
		{"bare pytest", map[string]string{
			"stats.py":      "def median(xs):\n    return sorted(xs)[len(xs)//2]\n",
			"test_stats.py": "from stats import median\n\ndef test_m():\n    assert median([3,1,2]) == 2\n",
			"notes.py":      "print('unrelated')\n",
		}, "pytest -q", []string{"stats.py", "test_stats.py"}, []string{"notes.py"}},
		{"pytest on a directory", map[string]string{
			"tests/test_a.py": "def test_a():\n    assert True\n",
			"test_b.py":       "def test_b():\n    assert True\n",
		}, "python3 -m pytest -k a tests/", []string{"tests/test_a.py"}, []string{"test_b.py"}},
		{"cd then pytest", map[string]string{
			"sub/test_a.py": "def test_a():\n    assert True\n",
			"test_b.py":     "def test_b():\n    assert True\n",
		}, "cd sub && pytest", []string{"sub/test_a.py"}, []string{"test_b.py"}},
		{"go test with tests", map[string]string{
			"main.go":      "package main\n\nfunc add(a, b int) int { return a + b }\n\nfunc main() {}\n",
			"main_test.go": "package main\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {}\n",
		}, "go test ./...", []string{"main.go", "main_test.go"}, nil},
		{"go test without tests only compiles", map[string]string{
			"main.go": "package main\n\nfunc main() {}\n",
		}, "go test ./...", nil, []string{"main.go"}},
		{"go run of a package", map[string]string{
			"main.go": "package main\n\nfunc main() {}\n",
		}, "go run .", []string{"main.go"}, nil},
		{"npm test", map[string]string{
			"app.js":      "module.exports = (a, b) => a + b\n",
			"app.test.js": "const add = require('./app')\ntest('adds', () => expect(add(1, 2)).toBe(3))\n",
		}, "npm test", []string{"app.js", "app.test.js"}, nil},
		{"a hidden runner result covers nothing", map[string]string{
			"test_a.py": "def test_a():\n    assert True\n",
		}, "pytest | tail -3", nil, []string{"test_a.py"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, dir := sepCtx(t, nil)
			for name, body := range c.files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
					t.Fatal(err)
				}
				sepWrite(t, ctx, dir, name, body)
			}
			st := &runState{}
			st.observeVerification(ctx, "", 1, c.command, ranClean("ok\n"))
			for _, name := range c.covered {
				if !pathCoverageSatisfied(ctx, resolveAgentPath(ctx, name), fileSHA256(ctx, name)) {
					t.Errorf("%q did not cover %s", c.command, name)
				}
			}
			for _, name := range c.not {
				if pathCoverageSatisfied(ctx, resolveAgentPath(ctx, name), fileSHA256(ctx, name)) {
					t.Errorf("%q covered %s, which it never ran", c.command, name)
				}
			}
		})
	}
}
