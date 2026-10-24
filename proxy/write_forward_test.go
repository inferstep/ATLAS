package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A syntax gate on a NEW file would put the proxy's judgement in place of
// execution's, and forbid the loop that works: write it, run it, read the real
// traceback, fix it. Under a rejection the model retries blind, resends the
// same content, and the repetition breaker ends the session with no file at
// all. So broken content to a new file lands, with a warning.
//
// On an EXISTING file the gate stays: it stops working code being clobbered.

func TestBrokenContentToANewFileLandsWithAWarning(t *testing.T) {
	root := t.TempDir()
	ctx := &AgentContext{WorkingDir: root, SessionWrites: map[string]bool{}, BodySeen: map[string]bool{}}
	path := filepath.Join(root, "solve.py")
	res, err := writeNewFileWithWarning(path, "solve.py", "def f(:\n", "SyntaxError: invalid syntax (line 1)", ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("the write must land: res=%+v err=%v", res, err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("file must be on disk: %v", statErr)
	}
	var out WriteFileOutput
	if json.Unmarshal(res.Data, &out) != nil || out.Warning == "" {
		t.Fatalf("result must carry the warning: %s", string(res.Data))
	}
	for _, want := range []string{"does not parse", "Run it", "traceback"} {
		if !strings.Contains(out.Warning, want) {
			t.Errorf("warning should say %q: %s", want, out.Warning)
		}
	}
	// The carveout that lets the model fix its own file reads SessionWrites
	// by the path AS THE MODEL SENT IT, not the resolved one.
	if !ctx.SessionWrites["solve.py"] {
		t.Error("a landed write is the agent's own file and must be iterable")
	}
}

// The syntax gate on an existing file protects WORKING code. When the file on
// disk is itself unparseable, rejecting an imperfect fix guarantees that the
// broken version survives, so the fix lands.
//
// Both tests need T2-sized content (def/loop/if), or classifyFileTier routes
// them down the ungated T1 path and the gate under test never runs.

func t2Body(marker string) string {
	return "from chunk import chunks\n\n\ndef main():\n    data = [1, 2, " + marker + ", 4, 5]\n" +
		"    size = 2\n    result = chunks(data, size)\n    print(result)\n" +
		"    for row in result:\n        if len(row) != size:\n            print('short')\n" +
		"        else:\n            print('full')\n    return result\n\n\n" +
		"if __name__ == '__main__':\n    main()\n"
}

func TestARepairOfABrokenFileLandsEvenWhenImperfect(t *testing.T) {
	dir := t.TempDir()
	sb := fakeSyntaxSandbox(t, "**") // markdown bold marks content invalid
	defer sb.Close()
	// V3URL set so the T2 pre-V3 gate is the code under test; the gate
	// decides before any V3 request, so nothing ever calls this address.
	ctx := writeGateCtx(t, "http://127.0.0.1:1", sb.URL, dir)

	// A broken file is already on disk, as a warned new file leaves it.
	if err := os.WriteFile(filepath.Join(dir, "s.py"), []byte(t2Body("**1**")), 0o644); err != nil {
		t.Fatal(err)
	}
	// The repair attempt is also imperfect — it must still land.
	repair := t2Body("**2**")
	args, _ := json.Marshal(map[string]string{"path": "s.py", "content": repair})
	res, err := writeFileTool().Execute(json.RawMessage(args), ctx)
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("an imperfect repair of a broken file must land, got %+v", res)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "s.py"))
	if string(got) != repair {
		t.Errorf("the repair attempt must be what is on disk, got %q", got)
	}
}

func TestAWorkingFileIsStillProtectedFromBrokenContent(t *testing.T) {
	dir := t.TempDir()
	sb := fakeSyntaxSandbox(t, "**")
	defer sb.Close()
	ctx := writeGateCtx(t, "http://127.0.0.1:1", sb.URL, dir)

	healthy := t2Body("3")
	if err := os.WriteFile(filepath.Join(dir, "s.py"), []byte(healthy), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx.RecordFileRead(filepath.Join(dir, "s.py"), healthy)
	args, _ := json.Marshal(map[string]string{"path": "s.py", "content": t2Body("**3**")})
	res, _ := writeFileTool().Execute(json.RawMessage(args), ctx)
	if res != nil && res.Success {
		t.Error("working code must not be clobbered with broken content")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "s.py"))
	if string(got) != healthy {
		t.Errorf("the healthy file must survive, got %q", got)
	}
}

// Verification is of bytes, not of a moment. A file whose bytes changed after
// the verification snapshot is drift, and so is a file the snapshot never saw.
func TestDriftAfterVerificationIsNamed(t *testing.T) {
	dir := t.TempDir()
	ctx := &AgentContext{WorkingDir: dir, SessionWrites: map[string]bool{"solve.py": true}}
	if err := os.WriteFile(filepath.Join(dir, "solve.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Verified before this file was ever written: absent from the snapshot.
	if got := driftedSinceVerification(ctx, map[string]string{}); got != "solve.py" {
		t.Fatalf("a never-verified artifact is drift by definition, got %q", got)
	}
	snap := sessionWriteHashes(ctx)
	if len(snap) != 1 {
		t.Fatalf("snapshot should cover the written file: %v", snap)
	}
	if got := driftedSinceVerification(ctx, snap); got != "" {
		t.Fatalf("unchanged bytes are not drift: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "solve.py"), []byte("print(2)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := driftedSinceVerification(ctx, snap); got != "solve.py" {
		t.Fatalf("rewritten bytes must be drift, got %q", got)
	}
}

// With two changed files, the artifact gate names the first in sorted order,
// every time, so one run never gets two different texts.
func TestOfTwoChangedFilesTheSameOneIsNamedEachTime(t *testing.T) {
	ctx := &AgentContext{WorkingDir: t.TempDir(), SessionWrites: map[string]bool{"b.py": true, "a.py": true}}
	for try := 1; try <= 100; try++ {
		if got := driftedSinceVerification(ctx, map[string]string{}); got != "a.py" {
			t.Fatalf("try %d: %q, want a.py", try, got)
		}
	}
}

// With two warned files, the run-first gate names the first in sorted order,
// every time.
func TestOfTwoWarnedFilesTheRunFirstGateNamesTheSameOneEachTime(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	for try := 1; try <= 100; try++ {
		st := &runState{pendingWarnedRun: map[string]bool{"b.py": true, "a.py": true}}
		gate, msg := st.exitGates(ctx, "fix the script", "Done.")
		if gate != "run_first_gate" || !strings.HasPrefix(msg, "`a.py` is on disk with a parse warning") {
			t.Fatalf("try %d: %s %q, want run_first_gate naming a.py", try, gate, msg)
		}
	}
}

// A warned landing is artifact state, not a rewrite throttle. The mark must
// survive everything short of an actual execution attempt of the file: naming
// it (cat/grep/ls) proves nothing about runtime behavior and must not
// discharge it.
func TestExecutionAttemptDischarge(t *testing.T) {
	cases := []struct {
		cmd  string
		path string
		want bool
	}{
		{"python3 solve.py", "solve.py", true},
		{"python solve.py < input.txt", "solve.py", true},
		{"timeout 10 python3 solve.py", "solve.py", true},
		{"python3 -u solve.py", "solve.py", true},
		{"./solve.py", "solve.py", true},
		{"cd /w && python3 ./solve.py", "solve.py", true},
		{"python3 sub/solve.py", "sub/solve.py", true},
		{"pytest test_solve.py", "test_solve.py", true},
		{"node app.js", "app.js", true},
		{"bash run.sh", "run.sh", true},
		// Naming is not running.
		{"cat solve.py", "solve.py", false},
		{"grep main solve.py", "solve.py", false},
		{"ls solve.py", "solve.py", false},
		{"wc -l solve.py", "solve.py", false},
		{"echo solve.py", "solve.py", false},
		{"ls", "solve.py", false},
		{"python3 other.py", "solve.py", false},
		// Execution in one chain segment doesn't bless a file only
		// named in another.
		{"cat solve.py && python3 other.py", "solve.py", false},
		{"python3 solve.py && cat notes.txt", "solve.py", true},
	}
	for _, c := range cases {
		if got := executionAttempt(c.cmd, c.path); got != c.want {
			t.Errorf("executionAttempt(%q, %q) = %v, want %v", c.cmd, c.path, got, c.want)
		}
	}
}

// --- The warned mark is a set of active warnings -----------------------------
//
// The exit gate ranges over pendingWarnedRun's keys without looking at the
// values, so the set must hold only active warnings. A clean landing stored as
// false would announce a file that parses as "on disk with a parse warning",
// and the model would spend the session rewriting a valid file. The two
// mid-loop readers ask for the value.

// warnedRunFixture drives the real loop with a genuine Python syntax check and
// reports every exit gate the session emitted over a warned file:
// run_first_gate, or repair_gate, which owns a warned file that still does not
// parse. With sandbox set, commands run in the sandbox stub, which answers the
// background-job endpoints: the only route a server script may run through.
// Otherwise they run on the host.
func warnedRunFixture(t *testing.T, plan func(i int) map[string]interface{}, sandbox bool) (
	*AgentContext, string, map[string]int, map[string]string, []string) {
	t.Helper()
	dir := t.TempDir()
	turns := 0
	census := map[string]int{}
	terminal := map[string]string{}
	var gates []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if d := warnedRunSyntax(in.Code); d != "" {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"valid": false, "errors": []string{d}})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(r.URL.Path, "/jobs/start"):
			json.NewEncoder(w).Encode(map[string]interface{}{"job_id": "job1", "pid": 7})
			return
		case strings.HasSuffix(r.URL.Path, "/jobs/job1/output"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"job_id": "job1", "running": false, "exit_code": 1,
				"stdout": []string{}, "stderr": []string{"SyntaxError: '(' was never closed"}})
			return
		case strings.HasSuffix(r.URL.Path, "/jobs/job1/stop"):
			json.NewEncoder(w).Encode(map[string]interface{}{"job_id": "job1", "killed": false})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= 30 {
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = !sandbox
	ctx.MaxTurns = 0
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		census[et]++
		if et == "gate" {
			var g struct{ Gate, Reason string }
			if json.Unmarshal(b, &g) == nil && (g.Gate == "run_first_gate" || g.Gate == "repair_gate") {
				gates = append(gates, g.Reason)
			}
		}
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	runAgentLoop(ctx, "Write solve.py so it prints 7.")
	return ctx, dir, census, terminal, gates
}

func warnedRunSyntax(code string) string {
	cmd := exec.Command("python3", "-c",
		"import ast,sys\ntry:\n ast.parse(sys.stdin.read())\nexcept SyntaxError as e:\n sys.stdout.write('%s (line %d)' % (e.msg, e.lineno or 0))")
	cmd.Stdin = strings.NewReader(code)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

const (
	warnedRunValid  = "def solve():\n    return 7\n\n\nprint(solve())\n"
	warnedRunBroken = "def solve():\n    return 7\n\nprint(solve()\n"
)

func writeCall(path, content string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "write_file",
		"args": map[string]string{"path": path, "content": content}}
}

// What the mark says after each landing sequence.
func TestPendingWarnedRunIsASetOfActiveWarnings(t *testing.T) {
	for _, c := range []struct {
		name      string
		sandbox   bool
		plan      func(i int) map[string]interface{}
		wantGates bool
		wantDisk  string
	}{
		{
			// A clean landing is not a warning.
			name: "clean landing",
			plan: func(i int) map[string]interface{} {
				if i == 0 {
					return writeCall("solve.py", warnedRunValid)
				}
				return map[string]interface{}{"type": "done", "summary": "wrote solve.py"}
			},
			wantGates: false, wantDisk: warnedRunValid,
		},
		{
			// A genuine warning still gates.
			name: "warned landing",
			plan: func(i int) map[string]interface{} {
				if i == 0 {
					return writeCall("solve.py", warnedRunBroken)
				}
				return map[string]interface{}{"type": "done", "summary": "wrote solve.py"}
			},
			wantGates: true, wantDisk: warnedRunBroken,
		},
		{
			// Running it discharges the mark, and the clean rewrite that
			// follows must not re-arm it. (A rewrite BEFORE the run is bounced
			// on purpose: the warned version has to be executed first.)
			name: "warned, run, then clean rewrite",
			plan: func(i int) map[string]interface{} {
				switch i {
				case 0:
					return writeCall("solve.py", warnedRunBroken)
				case 1:
					return map[string]interface{}{"type": "tool_call", "name": "run_command",
						"args": map[string]string{"command": "python3 solve.py"}}
				case 2:
					return writeCall("solve.py", warnedRunValid)
				}
				return map[string]interface{}{"type": "done", "summary": "fixed solve.py"}
			},
			wantGates: false, wantDisk: warnedRunValid,
		},
		{
			// A background run is a run. The foreground-server gate sends
			// every server script to run_background, and its settle window
			// returns the same traceback run_command would.
			name:    "warned, background run, then clean rewrite",
			sandbox: true,
			plan: func(i int) map[string]interface{} {
				switch i {
				case 0:
					return writeCall("solve.py", warnedRunBroken)
				case 1:
					return map[string]interface{}{"type": "tool_call", "name": "run_background",
						"args": map[string]string{"command": "python3 solve.py"}}
				case 2:
					return writeCall("solve.py", warnedRunValid)
				}
				return map[string]interface{}{"type": "done", "summary": "fixed solve.py"}
			},
			wantGates: false, wantDisk: warnedRunValid,
		},
		{
			// A clean landing on one path must not mask a genuine warning
			// that arrives after it. (A warned rewrite of the SAME path cannot
			// happen: over valid bytes the healthy-baseline gate refuses
			// invalid content outright.)
			name: "clean landing then a warned landing elsewhere",
			plan: func(i int) map[string]interface{} {
				switch i {
				case 0:
					return writeCall("solve.py", warnedRunValid)
				case 1:
					return writeCall("helper.py", warnedRunBroken)
				}
				return map[string]interface{}{"type": "done", "summary": "wrote both"}
			},
			wantGates: true, wantDisk: warnedRunValid,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, dir, census, terminal, gates := warnedRunFixture(t, c.plan, c.sandbox)
			if (len(gates) > 0) != c.wantGates {
				t.Errorf("%d warned-file exit gate events %q, want any=%v (terminal %q/%q)",
					len(gates), gates, c.wantGates, terminal["status"], terminal["reason"])
			}
			got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
			if string(got) != c.wantDisk {
				t.Errorf("solve.py on disk is not what the sequence wrote: %q", got)
			}
			if census["done"] != 1 {
				t.Errorf("%d terminal events", census["done"])
			}
			// The gate never invents a warning about bytes the ledger says
			// are fine.
			if !c.wantGates {
				d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
				if d == nil {
					t.Fatal("solve.py is not in the ledger")
				}
				if _, status := d.CurrentValidation(); status != ValidationPassed {
					t.Errorf("solve.py should be exact-hash passed, got %s", status)
				}
			}
		})
	}
}

// Discharge is by file, not by spelling: a clean rewrite of ./solve.py retires
// solve.py's warning, or the mark outlives the bytes it describes.
func TestWarnedRunMarkIsDischargedByFileNotSpelling(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	st := &runState{}
	st.markWarnedRun(ctx, "solve.py", true)
	if len(st.pendingWarnedRun) != 1 {
		t.Fatalf("a warned landing must arm the mark: %v", st.pendingWarnedRun)
	}
	st.markWarnedRun(ctx, "./solve.py", false)
	if len(st.pendingWarnedRun) != 0 {
		t.Errorf("a clean rewrite of the same file left the warning standing: %v",
			st.pendingWarnedRun)
	}
	// A different file keeps its own.
	st.markWarnedRun(ctx, "solve.py", true)
	st.markWarnedRun(ctx, "helper.py", false)
	if !st.pendingWarnedRun["solve.py"] {
		t.Error("a clean landing elsewhere discharged an unrelated warning")
	}
	// Never stores an inactive value.
	for p, v := range st.pendingWarnedRun {
		if !v {
			t.Errorf("%s is in the set with value false", p)
		}
	}
}

// Structural: the set has one writer, and no site may store an inactive value
// that a key-only reader would read as active.
//
// The exit gate ranges over keys without looking at the value, which is only
// correct while membership means "warned". A future assignment of a computed
// boolean would silently reintroduce the false gate, so this pins the shape
// rather than the behaviour.
func TestPendingWarnedRunHasOneWriter(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	var assigns, keyOnlyReads int
	for i, line := range strings.Split(string(src), "\n") {
		l := strings.TrimSpace(line)
		if !strings.Contains(l, "pendingWarnedRun") {
			continue
		}
		if strings.Contains(l, "pendingWarnedRun[") && strings.Contains(l, "=") &&
			!strings.Contains(l, "==") && !strings.Contains(l, "delete(") {
			assigns++
			// The one permitted assignment, inside markWarnedRun.
			if !strings.HasSuffix(l, "= true") {
				t.Errorf("agent.go:%d stores a non-true value in the warned set: %s", i+1, l)
			}
		}
		if strings.HasPrefix(l, "for ") && strings.Contains(l, "s.pendingWarnedRun") {
			keyOnlyReads++
		}
	}
	if assigns != 1 {
		t.Errorf("%d assignments into the warned set, want exactly one (markWarnedRun)", assigns)
	}
	if keyOnlyReads == 0 {
		t.Error("no key-only reader found; this guard is pinning something that moved")
	}
	t.Logf("warned set: %d assignment, %d key-only reader(s)", assigns, keyOnlyReads)
}

// --- The gate names a call the tools will accept ---------------------------
//
// `run_command python3 app.py` is refused by the foreground-server redirect
// when app.py serves, so a gate that demands it is a contradiction. The
// instruction has to be the one the redirect would give.
func TestRunFirstInstructionNamesTheToolThatCanRunTheFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.py"),
		[]byte("from flask import Flask\napp = Flask(__name__)\napp.run(port=5000)\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "solve.py"), []byte("print(7)\n"), 0o644)
	ctx := NewAgentContext(dir, Tier2Medium)

	server := runFirstInstruction(ctx, "app.py")
	if !strings.Contains(server, "run_background") || !strings.Contains(server, "python3 app.py") {
		t.Errorf("server script must be routed to run_background: %s", server)
	}
	if strings.Contains(server, "run_command") {
		t.Errorf("server script must not be sent to run_command: %s", server)
	}
	if foregroundServerRejectionWithSource("python3 app.py", workspaceFileReader(ctx)) == "" {
		t.Fatalf("fixture app.py is not detected as a server; the test proves nothing")
	}

	plain := runFirstInstruction(ctx, "solve.py")
	if !strings.Contains(plain, "run_command") || !strings.Contains(plain, "python3 solve.py") {
		t.Errorf("plain script must be run with run_command: %s", plain)
	}
	if got := runFirstInstruction(ctx, "server.js"); !strings.Contains(got, "node server.js") {
		t.Errorf("a .js file is run with node: %s", got)
	}
}
