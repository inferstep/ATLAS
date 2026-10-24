package main

import (
	"context"
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
	"time"
)

// --- The no-op edit over a demonstrably broken artifact ----------------------
//
// The shape: write_file lands a file that does not parse and says so; edit_file
// demands a read; the model reads; then it sends an edit whose old_str and
// new_str are identical, over and over, until repetition protection ends the
// run with the broken file on disk and no valid version to fall back to.
//
// The loop is already bounded. What the recovery adds is a productive answer:
// the model is copying a span it cannot reproduce with a change applied, and
// being told again that the two sides match gives it nothing new.

// brokenArtifactFixture runs the real loop with a genuine Python syntax check,
// so the failure diagnostic in the ledger is a real one and not a fixture
// invention. plan sees the prompt, so a scripted model can react to what it
// was actually told rather than to the turn number.
func brokenArtifactFixture(t *testing.T, plan func(i int, prompt string) map[string]interface{}) (
	*AgentContext, string, *int, map[string]int, map[string]string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	turns := 0
	census := map[string]int{}
	terminal := map[string]string{}
	var bounces []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			out := map[string]interface{}{"valid": true}
			if diag := pythonSyntaxDiagnostic(in.Code); diag != "" {
				out = map[string]interface{}{"valid": false, "errors": []string{diag}}
			}
			json.NewEncoder(w).Encode(out)
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
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(raw), "single fenced block") {
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": "no block"}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
			return
		}
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= wsCeiling {
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i, string(raw)))
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
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 0
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		census[et]++
		if et == "gate" || et == "tool_result" {
			bounces = append(bounces, et+"|"+string(b))
		}
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	runAgentLoop(ctx, "Write solve.py so it prints 7, then run it.")
	return ctx, dir, &turns, census, terminal, &bounces
}

// pythonSyntaxDiagnostic returns the real interpreter's message, or "" when the
// source parses. The fixture must not invent the diagnostic the ledger holds.
func pythonSyntaxDiagnostic(code string) string {
	cmd := exec.Command("python3", "-c",
		"import ast,sys;\ntry:\n ast.parse(sys.stdin.read())\nexcept SyntaxError as e:\n sys.stdout.write('line %d: %s' % (e.lineno or 0, e.msg))")
	cmd.Stdin = strings.NewReader(code)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

const (
	brokenSolveSource = "def solve()\n    return 7\n\n\nprint(solve())\n"
	noopEditText      = "def solve()"
	fixedSolveHeader  = "def solve():"
)

// noopEditPlan creates the broken file, reads it, then repeats the identical
// no-op edit. It corrects itself only after a prompt holds marker, never
// merely because turns passed.
func noopEditPlan(marker string) func(int, string) map[string]interface{} {
	corrected := false
	verified := false
	return func(i int, prompt string) map[string]interface{} {
		switch {
		case i == 0:
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "solve.py", "content": brokenSolveSource}}
		case i == 1:
			return map[string]interface{}{"type": "tool_call", "name": "read_file",
				"args": map[string]string{"path": "solve.py"}}
		case marker != "" && strings.Contains(prompt, marker) && !corrected:
			corrected = true
			return map[string]interface{}{"type": "tool_call", "name": "edit_file",
				"args": map[string]string{"path": "solve.py",
					"old_str": noopEditText, "new_str": fixedSolveHeader}}
		case corrected && !verified:
			verified = true
			return map[string]interface{}{"type": "tool_call", "name": "run_command",
				"args": map[string]string{"command": "python3 solve.py"}}
		case corrected:
			return map[string]interface{}{"type": "done", "summary": "solve.py prints 7"}
		default:
			return map[string]interface{}{"type": "tool_call", "name": "edit_file",
				"args": map[string]string{"path": "solve.py",
					"old_str": noopEditText, "new_str": noopEditText}}
		}
	}
}

// noopRecoveryMarker is the one phrase the recovery must say, and the phrase
// the scripted model keys on.
const noopRecoveryMarker = "changes nothing"

func TestNoopEditOverBrokenArtifactRecovers(t *testing.T) {
	ctx, dir, turns, census, terminal, bounces := brokenArtifactFixture(t, noopEditPlan(noopRecoveryMarker))
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	t.Logf("noop edit: turns=%d status=%q reason=%q disk=%q",
		*turns, terminal["status"], terminal["reason"], string(got))

	offers := 0
	for _, b := range *bounces {
		if strings.HasPrefix(b, "gate|") && strings.Contains(b, noopRecoveryMarker) {
			offers++
		}
	}
	if offers != 1 {
		t.Fatalf("%d recovery offers, want exactly one", offers)
	}
	// The correction is the model's, and it is materially different.
	if !strings.Contains(string(got), fixedSolveHeader) {
		t.Errorf("the corrected bytes did not land: %q", got)
	}
	if !strings.Contains(string(got), "print(solve())") {
		t.Errorf("the surrounding structure was not preserved: %q", got)
	}
	// Ledger describes the bytes on disk, and they are demonstrably valid.
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Fatal("the ledger does not describe the bytes on disk")
	}
	kind, status := d.CurrentValidation()
	if status != ValidationPassed {
		t.Errorf("validation on the current hash is %s/%s, want passed", kind, status)
	}
	if terminal["status"] != string(TerminalCompleted) ||
		terminal["reason"] != "deliverables_demonstrated" {
		t.Errorf("terminal %q/%q, want completed/deliverables_demonstrated",
			terminal["status"], terminal["reason"])
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result balance: %d vs %d", census["tool_call"], census["tool_result"])
	}
}

// brokenArtifactLedger builds the one state the recovery reads: a
// session-written file on disk whose exact current bytes carry a demonstrated
// syntax failure, with the model having actually seen them. mutate then
// breaks one clause of that state.
func brokenArtifactLedger(t *testing.T, mutate func(*AgentContext, *DeliverableState, string)) (*AgentContext, *runState) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "solve.py")
	os.WriteFile(path, []byte(brokenSolveSource), 0o644)
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.RecordFileRead(path, brokenSolveSource)
	ctx.RecordBodySeen(path)
	h := hashBytes([]byte(brokenSolveSource))
	d := &DeliverableState{
		Path: "solve.py", CurrentHash: h, CurrentSize: len(brokenSolveSource), Generation: 1,
		ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationFailed,
		ValidationDetail: "line 1: expected ':'", ValidatedHash: h,
	}
	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*DeliverableState{}
	}
	ctx.Ledger[ledgerKey(ctx, "solve.py")] = d
	if mutate != nil {
		mutate(ctx, d, path)
	}
	return ctx, &runState{}
}

// Entry is a conjunction of evidence clauses. Each case removes one and must
// produce nothing — no source, no diagnostic, no state written.
func TestBrokenArtifactRecoveryEntryPredicates(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*AgentContext, *DeliverableState, string)
	}{
		{"validation unknown", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus, d.ValidationKind = ValidationUnknown, ValidationKindUnknown
		}},
		{"validation not_run", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus = ValidationNotRun
		}},
		{"validation not_applicable", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus = ValidationNotApplicable
		}},
		{"current bytes are valid", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus = ValidationPassed
		}},
		{"failure describes older bytes", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidatedHash = hashBytes([]byte("something else"))
		}},
		{"disk moved under the ledger", func(_ *AgentContext, _ *DeliverableState, path string) {
			os.WriteFile(path, []byte("def solve():\n    return 7\n"), 0o644)
		}},
		{"file was never read", func(ctx *AgentContext, _ *DeliverableState, path string) {
			ctx.BodySeen = map[string]bool{}
		}},
		{"not written by this session", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.Generation = 0
		}},
		{"a safer checkpoint exists", func(_ *AgentContext, d *DeliverableState, _ string) {
			good := []byte("def solve():\n    return 7\n")
			d.CheckpointBytes, d.CheckpointHash = good, hashBytes(good)
			d.CheckpointKind = ValidationKindSyntax
		}},
		{"file is gone", func(_ *AgentContext, _ *DeliverableState, path string) {
			os.Remove(path)
		}},
		{"no budget to act on it", func(ctx *AgentContext, _ *DeliverableState, _ string) {
			c, cancel := context.WithDeadline(context.Background(), time.Now().Add(5*time.Second))
			t.Cleanup(cancel)
			ctx.Ctx = c
		}},
		{"work context already ended", func(ctx *AgentContext, _ *DeliverableState, _ string) {
			c, cancel := context.WithCancel(context.Background())
			cancel()
			ctx.Ctx = c
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, st := brokenArtifactLedger(t, c.mutate)
			// Twice: the recurrence is what would arm it if it were eligible.
			for i := 0; i < 2; i++ {
				if msg := brokenArtifactRecovery(ctx, st, "solve.py"); msg != "" {
					t.Fatalf("recovered on %q: %.120s", c.name, msg)
				}
			}
			if len(st.brokenArtifactRecovered) != 0 {
				t.Errorf("recovery state was written anyway: %v", st.brokenArtifactRecovered)
			}
		})
	}
}

// The positive case, and the shape of what it says.
func TestBrokenArtifactRecoveryArmsOnTheRecurrence(t *testing.T) {
	ctx, st := brokenArtifactLedger(t, nil)
	if msg := brokenArtifactRecovery(ctx, st, "solve.py"); msg != "" {
		t.Fatalf("one accidental no-op recovered: %.120s", msg)
	}
	msg := brokenArtifactRecovery(ctx, st, "solve.py")
	if msg == "" {
		t.Fatal("the recurrence did not recover")
	}
	for _, want := range []string{
		"solve.py",                // canonical path
		"return 7",                // bounded current source
		"line 1: expected ':'",    // the diagnostic bound to these exact bytes
		"old_str and new_str are", // why the edit changes nothing
		"actually differs",        // what the next mutation has to do
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the recovery does not supply %q:\n%s", want, msg)
		}
	}
	// Once per evidence generation, and an alias is the same generation.
	for _, spelling := range []string{"solve.py", "./solve.py"} {
		if again := brokenArtifactRecovery(ctx, st, spelling); again != "" {
			t.Errorf("%s bought a second recovery: got %v", spelling, again)
		}
	}
	// A different path is a different problem.
	other := filepath.Join(ctx.WorkingDir, "other.py")
	os.WriteFile(other, []byte(brokenSolveSource), 0o644)
	ctx.RecordBodySeen(other)
	h := hashBytes([]byte(brokenSolveSource))
	ctx.Ledger[ledgerKey(ctx, "other.py")] = &DeliverableState{
		Path: "other.py", CurrentHash: h, Generation: 1,
		ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationFailed,
		ValidationDetail: "line 1: expected ':'", ValidatedHash: h,
	}
	if first := brokenArtifactRecovery(ctx, st, "other.py"); first != "" {
		t.Errorf("another path inherited solve.py's recurrence: got %v", first)
	}
	if brokenArtifactRecovery(ctx, st, "other.py") == "" {
		t.Error("another path was denied its own recovery")
	}
}

// New bytes are a new evidence generation: the recovery re-arms, and it cannot
// carry the old diagnostic across.
func TestBrokenArtifactRecoveryRearmsOnNewEvidence(t *testing.T) {
	ctx, st := brokenArtifactLedger(t, nil)
	brokenArtifactRecovery(ctx, st, "solve.py")
	if brokenArtifactRecovery(ctx, st, "solve.py") == "" {
		t.Fatal("the first generation did not recover")
	}
	// The model changes the file and it is still broken, differently.
	next := "def solve()\n    return 8\n"
	os.WriteFile(filepath.Join(ctx.WorkingDir, "solve.py"), []byte(next), 0o644)
	h := hashBytes([]byte(next))
	key := ledgerKey(ctx, "solve.py")
	// Stale evidence first: the ledger still describes the old bytes.
	if msg := brokenArtifactRecovery(ctx, st, "solve.py"); msg != "" {
		t.Errorf("stale failure detail was reused on new bytes: %.120s", msg)
	}
	ctx.Ledger[key].CurrentHash, ctx.Ledger[key].ValidatedHash = h, h
	ctx.Ledger[key].ValidationDetail = "line 1: still expected ':'"
	ctx.RecordBodySeen(filepath.Join(ctx.WorkingDir, "solve.py"))
	if msg := brokenArtifactRecovery(ctx, st, "solve.py"); msg != "" {
		t.Errorf("the new generation recovered on one accidental no-op: got %v", msg)
	}
	msg := brokenArtifactRecovery(ctx, st, "solve.py")
	if msg == "" {
		t.Fatal("the new evidence generation did not re-arm")
	}
	if strings.Contains(msg, "line 1: expected ':'") || !strings.Contains(msg, "still expected") {
		t.Errorf("the recovery carried the previous generation's diagnostic:\n%s", msg)
	}
	if !strings.Contains(msg, "return 8") {
		t.Errorf("the recovery showed stale source:\n%s", msg)
	}
}

// State is released by a materially different mutation on the SAME path, and
// by nothing else.
func TestBrokenArtifactStateReleaseIsNarrow(t *testing.T) {
	arm := func(t *testing.T) (*AgentContext, *runState) {
		ctx, st := brokenArtifactLedger(t, nil)
		brokenArtifactRecovery(ctx, st, "solve.py")
		if brokenArtifactRecovery(ctx, st, "solve.py") == "" {
			t.Fatal("setup did not arm")
		}
		return ctx, st
	}
	held := func(st *runState) bool { return len(st.brokenArtifactRecovered) > 0 }

	for _, c := range []struct {
		name    string
		tool    string
		args    string
		release bool
	}{
		{"another no-op on the same path", "edit_file",
			`{"path":"solve.py","old_str":"x","new_str":"x"}`, false},
		{"a read of the same path", "read_file", `{"path":"solve.py"}`, false},
		{"success on an unrelated path", "write_file",
			`{"path":"other.py","content":"print(1)\n"}`, false},
		{"a real edit of the same path", "edit_file",
			`{"path":"solve.py","old_str":"a","new_str":"b"}`, true},
		{"a real edit spelled as an alias", "edit_file",
			`{"path":"./solve.py","old_str":"a","new_str":"b"}`, true},
		{"a rewrite of the same path", "write_file",
			`{"path":"solve.py","content":"print(7)\n"}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, st := arm(t)
			clearBrokenArtifactState(ctx, st, c.tool, json.RawMessage(c.args))
			if held(st) == c.release {
				t.Errorf("release=%v, want %v", !held(st), c.release)
			}
		})
	}
}

// Loop-level controls: what the run does around the recovery.
func TestNoopEditRecoveryLoopControls(t *testing.T) {
	t.Run("one accidental no-op gets only the tool's answer", func(t *testing.T) {
		// It corrects itself off edit_file's OWN refusal, which is unchanged.
		ctx, dir, turns, census, terminal, bounces := brokenArtifactFixture(t,
			noopEditPlan("would change nothing"))
		got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
		t.Logf("accidental: turns=%d status=%q reason=%q", *turns,
			terminal["status"], terminal["reason"])
		for _, b := range *bounces {
			if strings.Contains(b, noopRecoveryMarker) {
				t.Fatal("a single no-op drew the recovery")
			}
		}
		if terminal["status"] != string(TerminalCompleted) {
			t.Errorf("following the tool's own refusal did not complete: %q/%q",
				terminal["status"], terminal["reason"])
		}
		if !strings.Contains(string(got), fixedSolveHeader) {
			t.Errorf("the correction did not land: %q", got)
		}
		d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
		if _, status := d.CurrentValidation(); status != ValidationPassed {
			t.Errorf("validation on the current hash is %s, want passed", status)
		}
		if census["done"] != 1 {
			t.Errorf("%d terminal events", census["done"])
		}
	})

	t.Run("a model that ignores the recovery stops honestly", func(t *testing.T) {
		// marker "" — nothing makes it correct itself.
		ctx, dir, turns, census, terminal, bounces := brokenArtifactFixture(t, noopEditPlan(""))
		got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
		t.Logf("ignored: turns=%d status=%q reason=%q", *turns,
			terminal["status"], terminal["reason"])

		offers := 0
		for _, b := range *bounces {
			if strings.HasPrefix(b, "gate|") && strings.Contains(b, noopRecoveryMarker) {
				offers++
			}
		}
		if offers != 1 {
			t.Errorf("%d recovery offers, want exactly one even when ignored", offers)
		}
		if *turns >= wsCeiling {
			t.Fatalf("%d turns without a terminal", *turns)
		}
		if NormalizeTerminalStatus(terminal["status"]).Completed() {
			t.Errorf("an unresolved broken artifact reported %q", terminal["status"])
		}
		// Nothing was mutated or run on the model's behalf.
		if string(got) != brokenSolveSource {
			t.Errorf("the file changed without a model mutation: %q", got)
		}
		d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
		if _, status := d.CurrentValidation(); status != ValidationFailed {
			t.Errorf("the ledger lost the failure on the current bytes: %s", status)
		}
		// Nothing safer ever existed to fall back to, which is why this case
		// needs a recovery rather than restoration of a checkpoint.
		ok, why := checkpointRestorable(d, hashBytes(got), false)
		t.Logf("checkpoint: restorable=%v (%s)", ok, why)
		if ok {
			t.Error("an eligible checkpoint existed and should have owned this")
		}
		if d.CheckpointHash != "" {
			t.Errorf("a checkpoint was held for a file never shown valid: %q", d.CheckpointHash)
		}
		if census["done"] != 1 {
			t.Errorf("%d terminal events", census["done"])
		}
		if census["tool_call"] != census["tool_result"] {
			t.Errorf("call/result balance: %d vs %d",
				census["tool_call"], census["tool_result"])
		}
	})
}

// The recovery turn is counted exactly once: it never reaches the tool, so no
// post-execution accounting runs for it, and the resend ban below it is
// skipped. One no-op edit, one increment.
func TestNoopEditRecoveryCountsTheRefusalOnce(t *testing.T) {
	ctx, st := brokenArtifactLedger(t, nil)
	ctx.RecentFailurePaths = nil
	before := len(ctx.RecentFailurePaths)
	if brokenArtifactRecovery(ctx, st, "solve.py") != "" {
		t.Fatal("armed on the first no-op")
	}
	// The helper itself accounts for nothing; the loop does, once per turn.
	if len(ctx.RecentFailurePaths) != before {
		t.Errorf("the recovery helper wrote failure accounting of its own: %v",
			ctx.RecentFailurePaths)
	}
	if brokenArtifactRecovery(ctx, st, "solve.py") == "" {
		t.Fatal("the recurrence did not arm")
	}
	if len(ctx.RecentFailurePaths) != before {
		t.Errorf("the recovery helper wrote failure accounting of its own: %v",
			ctx.RecentFailurePaths)
	}
	// And it launches nothing: no mutation, no command, no read state invented.
	if len(ctx.SessionWrites) != 0 {
		t.Errorf("the recovery wrote something: %v", ctx.SessionWrites)
	}
}
