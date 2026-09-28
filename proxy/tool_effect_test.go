package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Effect classification is a property of the tool, declared on its ToolDef,
// so a new tool cannot be registered without deciding what it can do to the
// workspace. A parallel map keyed by name would drift; this cannot.
//
// The classes describe CAPABILITY, not outcome. A direct mutator still
// reports applied/refused/failed/none per branch; the effect only says the
// tool is capable of mutating and therefore owes a local classification.
func TestEveryRegisteredToolDeclaresAnEffect(t *testing.T) {
	if len(toolRegistry) == 0 {
		t.Fatal("tool registry is empty; this test would prove nothing")
	}
	var missing []string
	for name, def := range toolRegistry {
		if def.Effect == ToolEffectUnknown {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("tools registered without an effect class: %v", missing)
	}
}

// The verified distribution, pinned so a reclassification has to be
// deliberate. stop_background is CommandUnobserved rather than read-only or
// control: it sends SIGTERM/SIGKILL and performs no filesystem comparison, so
// a process killed mid-write may leave partial bytes nobody measured.
func TestEffectDistributionMatchesTheVerifiedAudit(t *testing.T) {
	want := map[string]ToolEffect{
		"read_file": ToolEffectReadOnly, "search_files": ToolEffectReadOnly,
		"list_directory": ToolEffectReadOnly, "find_file": ToolEffectReadOnly,
		"outline_file": ToolEffectReadOnly, "tail_background": ToolEffectReadOnly,

		"write_file": ToolEffectDirectMutation, "edit_file": ToolEffectDirectMutation,
		"structural_edit": ToolEffectDirectMutation, "delete_file": ToolEffectDirectMutation,
		"move_file": ToolEffectDirectMutation, "insert_after": ToolEffectDirectMutation,
		"replace_lines": ToolEffectDirectMutation,

		"run_command":     ToolEffectCommandUnobserved,
		"run_background":  ToolEffectCommandUnobserved,
		"stop_background": ToolEffectCommandUnobserved,
	}
	for name, expect := range want {
		def, ok := toolRegistry[name]
		if !ok {
			t.Errorf("expected tool %q is not registered", name)
			continue
		}
		if def.Effect != expect {
			t.Errorf("%s: effect = %q, want %q", name, def.Effect, expect)
		}
	}
	for name := range toolRegistry {
		if _, known := want[name]; !known {
			t.Errorf("tool %q is registered but absent from the verified audit; "+
				"classify it deliberately", name)
		}
	}
}

func TestDirectMutatorsAreNotClassifiableAtTheBoundary(t *testing.T) {
	for name, def := range toolRegistry {
		if def.Effect != ToolEffectDirectMutation {
			continue
		}
		// A direct mutator's outcome is only knowable at the branch that
		// performed it, so the boundary must never invent one.
		if def.Effect.BoundaryClassifiable() {
			t.Errorf("%s: a direct mutator must not be classifiable at the "+
				"shared boundary", name)
		}
	}
}

// Boundary classification through the REAL executeToolCall, not a mirror.
func TestBoundaryClassifiesReadOnlyAndCommandTools(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}

	cases := []struct {
		name string
		tool string
		args string
		want MutationStatus
	}{
		// Read-only: mutates nothing whether it succeeds or fails.
		{"read success", "read_file", `{"path":"f.txt"}`, MutationNone},
		{"read missing file", "read_file", `{"path":"nope.txt"}`, MutationNone},
		{"list", "list_directory", `{"path":"."}`, MutationNone},
		// Pre-dispatch refusals prove no handler ran.
		{"unknown tool", "no_such_tool", `{}`, MutationNone},
		{"missing args", "read_file", ``, MutationNone},
	}
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\n"), 0o644)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := executeToolCall(c.tool, json.RawMessage(c.args), ctx)
			if res.MutationStatus != c.want {
				t.Errorf("MutationStatus = %q, want %q", res.MutationStatus, c.want)
			}
			if !res.Classified() {
				t.Errorf("result not fully classified: %+v", res)
			}
			if res.ValidationStatus.Passed() {
				t.Error("validation was synthesized as passed")
			}
		})
	}
}

// A command tool that reached its handler is Unobserved regardless of exit
// status: a subprocess may have written before failing.
func TestCommandToolsReportUnobservedIncludingFailure(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.StreamFn = func(string, interface{}) {}

	for _, c := range []struct{ name, cmd string }{
		{"success", "true"},
		{"non-zero exit", "false"},
		{"writes then fails", "echo x > side_effect.txt; exit 3"},
	} {
		t.Run(c.name, func(t *testing.T) {
			args, _ := json.Marshal(map[string]string{"command": c.cmd})
			res := executeToolCall("run_command", args, ctx)
			if res.MutationStatus != MutationUnobserved {
				t.Errorf("MutationStatus = %q, want unobserved", res.MutationStatus)
			}
			if res.MutationStatus.Applied() {
				t.Error("unobserved must never read as applied")
			}
			if !res.Classified() {
				t.Errorf("result not classified: %+v", res)
			}
		})
	}
}

// The boundary must never invent a direct mutator's outcome.
func TestBoundaryDoesNotClassifyDirectMutators(t *testing.T) {
	if ToolEffectDirectMutation.BoundaryClassifiable() {
		t.Fatal("direct mutation must not be boundary-classifiable")
	}
	for _, e := range []ToolEffect{ToolEffectReadOnly, ToolEffectCommandUnobserved} {
		if !e.BoundaryClassifiable() {
			t.Errorf("%q should be boundary-classifiable", e)
		}
	}
	if ToolEffectUnknown.BoundaryClassifiable() {
		t.Fatal("unknown effect must not be boundary-classifiable")
	}
}

// Intermediate-state safety check. The boundary now declines to classify
// direct mutators, and "declines to classify" must mean exactly that: the
// result passes through untouched and the mutation still happens. If
// BoundaryClassifiable were ever read as an authorization check, write_file
// would start failing operationally while every unit test still passed.
func TestDirectMutatorStillMutatesWhileUnclassified(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}

	args, _ := json.Marshal(map[string]string{
		"path": "new.py", "content": "VALUE = 1\n"})
	res := executeToolCall("write_file", args, ctx)

	// 1. The mutation actually occurred.
	got, err := os.ReadFile(filepath.Join(dir, "new.py"))
	if err != nil {
		t.Fatalf("write_file no longer writes: %v", err)
	}
	if string(got) != "VALUE = 1\n" {
		t.Fatalf("bytes on disk = %q", string(got))
	}
	// 2. Legacy Success is untouched.
	if !res.Success {
		t.Fatalf("legacy Success regressed to false: %+v", res)
	}
	// 3. Any classification present came from the LOCAL branch, not the
	// boundary. write_file's direct path is migrated (Slice 1), so applied is
	// expected here; what must never happen is the boundary inventing it for
	// a branch that has not been migrated.
	if !ToolEffectDirectMutation.BoundaryClassifiable() && res.MutationStatus != MutationApplied {
		t.Errorf("locally-classified direct write should report applied, got %q",
			res.MutationStatus)
	}
	// 4. Unknown is a pending-migration state, not an operational rejection.
	if res.Error != "" {
		t.Errorf("unclassified direct mutator was rejected: %q", res.Error)
	}
}

// The same for a direct mutator that legitimately fails: the classification
// is the PRODUCER's, the error is the real one, and the boundary invented
// nothing. delete_file now says none for a target that was never there; what
// must stay true is that the answer came from the handler, since the boundary
// is structurally forbidden from answering for a direct mutator at all.
func TestDirectMutatorFailureCarriesItsProducersClassification(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}

	args, _ := json.Marshal(map[string]string{"path": "missing.txt"})
	res := executeToolCall("delete_file", args, ctx)
	if res.Success {
		t.Fatal("deleting a missing file unexpectedly succeeded")
	}
	if ToolEffectDirectMutation.BoundaryClassifiable() {
		t.Fatal("the boundary must never be able to classify a direct mutator")
	}
	if res.MutationStatus != MutationNone {
		t.Errorf("MutationStatus = %q, want none: nothing was removed",
			res.MutationStatus)
	}
	if !res.Classified() {
		t.Errorf("producer left the result unclassified: %+v", res)
	}
	if res.Error == "" {
		t.Error("the real error was suppressed")
	}
}

// --- write_file Slice 1: non-code and simple filesystem outcomes ------------
//
// writeFileDirect is the terminal writer for the direct (non-V3) path. It
// demonstrates the mutation but performs no syntax check, so it reports
// applied plus an honest validation state -- never passed.

func TestWriteFileDirectClassification(t *testing.T) {
	for _, c := range []struct {
		name, file, content string
		wantKind            ValidationKind
		wantStatus          ValidationStatus
	}{
		{"non-code write", "notes.txt", "hello\n",
			ValidationKindNone, ValidationNotApplicable},
		{"recognized code, unvalidated at this layer", "mod.py", "A = 1\n",
			ValidationKindSyntax, ValidationNotRun},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.PermissionMode = PermissionYolo
			ctx.StreamFn = func(string, interface{}) {}

			args, _ := json.Marshal(map[string]string{"path": c.file, "content": c.content})
			res := executeToolCall("write_file", args, ctx)

			if !res.Success {
				t.Fatalf("legacy Success regressed: %+v", res)
			}
			if res.MutationStatus != MutationApplied {
				t.Errorf("MutationStatus = %q, want applied", res.MutationStatus)
			}
			if res.ValidationKind != c.wantKind {
				t.Errorf("ValidationKind = %q, want %q", res.ValidationKind, c.wantKind)
			}
			if res.ValidationStatus != c.wantStatus {
				t.Errorf("ValidationStatus = %q, want %q", res.ValidationStatus, c.wantStatus)
			}
			if res.ValidationStatus.Passed() {
				t.Error("a successful write must not synthesize validation passed")
			}
			if !res.Classified() {
				t.Errorf("result not fully classified: %+v", res)
			}
			got, err := os.ReadFile(filepath.Join(dir, c.file))
			if err != nil || string(got) != c.content {
				t.Fatalf("final bytes wrong: %q err=%v", string(got), err)
			}
		})
	}
}

// Refusals that happen before the handler can mutate. The unread-existing-file
// guard lives in the agent loop, not here, so this uses a deny-list refusal --
// a pre-dispatch check inside executeToolCall itself.
func TestWriteFileRefusalBeforeMutationLeavesBytes(t *testing.T) {
	dir := t.TempDir()
	prior := "SECRET=keepme\n"
	os.WriteFile(filepath.Join(dir, ".env"), []byte(prior), 0o644)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}

	args, _ := json.Marshal(map[string]string{"path": ".env", "content": "SECRET=stolen\n"})
	res := executeToolCall("write_file", args, ctx)
	if res.Success {
		t.Fatal("expected the deny-list to refuse a write to .env")
	}
	got, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if string(got) != prior {
		t.Fatalf("prior bytes changed on a refusal: %q", string(got))
	}
	// Pre-dispatch refusal: no handler ran, so nothing could have mutated.
	if res.MutationStatus != MutationNone {
		t.Errorf("MutationStatus = %q, want none", res.MutationStatus)
	}
	if res.MutationStatus.Applied() {
		t.Error("a refused write must never read as applied")
	}
	if !res.Classified() {
		t.Errorf("pre-dispatch refusal not classified: %+v", res)
	}
}

// --- write_file Slice 2: local syntax outcomes ------------------------------
//
// A caller upgrades not_run only when it knows the check examined the exact
// bytes the write path used. Both routes below satisfy that: the gate checks
// input.Content and refuses before any byte lands, and the warned write checks
// `content` and hands that same value to writeFileDirect.

// Invalid NEW code lands with a warning. Applied and failed are orthogonal:
// the file is on disk AND it does not parse.
func TestInvalidNewFileIsAppliedAndSyntaxFailed(t *testing.T) {
	dir := t.TempDir()
	got, res := writeThroughLoop(t, dir, "fresh.py", banLoopInvalidBody, true)

	if !res.Success {
		t.Fatalf("legacy Success regressed for a warned write: %+v", res)
	}
	if res.MutationStatus != MutationApplied {
		t.Errorf("MutationStatus = %q, want applied", res.MutationStatus)
	}
	if res.ValidationKind != ValidationKindSyntax || res.ValidationStatus != ValidationFailed {
		t.Errorf("validation = %q/%q, want syntax/failed",
			res.ValidationKind, res.ValidationStatus)
	}
	if res.ValidationDetail == "" {
		t.Error("ValidationDetail must carry the real diagnostic")
	}
	if got != banLoopInvalidBody {
		t.Errorf("bytes on disk are not the checked bytes")
	}
}

// Healthy existing code protected from an invalid overwrite.
func TestInvalidOverwriteIsRefusedAndSyntaxFailed(t *testing.T) {
	dir := t.TempDir()
	// Session-owned: create it valid first, then push invalid bytes.
	_, _ = writeThroughLoop(t, dir, "mod.py", banLoopValidBody, true)
	got, res := writeThroughLoop(t, dir, "mod.py", banLoopInvalidBody, true)

	if res.Success {
		t.Fatalf("an invalid overwrite of healthy code must not succeed: %+v", res)
	}
	if res.MutationStatus != MutationRefused {
		t.Errorf("MutationStatus = %q, want refused", res.MutationStatus)
	}
	if res.MutationStatus.Applied() {
		t.Error("a refusal must never read as applied")
	}
	if res.ValidationKind != ValidationKindSyntax || res.ValidationStatus != ValidationFailed {
		t.Errorf("validation = %q/%q, want syntax/failed",
			res.ValidationKind, res.ValidationStatus)
	}
	if res.ValidationDetail == "" {
		t.Error("ValidationDetail must carry the real diagnostic")
	}
	if got != banLoopValidBody {
		t.Errorf("prior valid bytes were not preserved: %q", got)
	}
}

// writeThroughLoop drives one write_file call through executeToolCall with a
// working syntax checker, returning the final on-disk bytes and the result.
func writeThroughLoop(t *testing.T, dir, rel, content string, withSandbox bool) (string, *ToolResult) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "def alpha(n:")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: invalid syntax (line 4)"}
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	if withSandbox {
		ctx.SandboxURL = srv.URL
	}
	args, _ := json.Marshal(map[string]string{"path": rel, "content": content})
	res := executeToolCall("write_file", args, ctx)
	b, _ := os.ReadFile(filepath.Join(dir, rel))
	return string(b), res
}

// --- Phase 3A: ledger observation at the shared boundary --------------------
//
// The ledger records what each call did to the session's deliverables. It is
// observational: every assertion below also checks that the tool's own result
// and the bytes on disk are what they were before the ledger existed.

func ledgerToolCtx(t *testing.T, dir string) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.StreamFn = func(string, interface{}) {}
	return ctx
}

func ledgerOf(t *testing.T, ctx *AgentContext, path string) *DeliverableState {
	t.Helper()
	return ctx.Ledger[ledgerKey(ctx, path)]
}

// diskHash is the ledger's only real claim: the recorded hash describes the
// bytes a reader would find right now.
func diskHash(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return hashBytes(b)
}

func TestContentMutatorsRecordTheBytesThatLanded(t *testing.T) {
	for _, c := range []struct {
		name, tool, seed string
		args             map[string]interface{}
		want             string
	}{
		{"write_file", "write_file", "",
			map[string]interface{}{"path": "solve.py", "content": "A = 1\n"},
			"A = 1\n"},
		{"edit_file", "edit_file", "A = 1\n",
			map[string]interface{}{"path": "solve.py", "old_str": "A = 1", "new_str": "A = 2"},
			"A = 2\n"},
		{"insert_after", "insert_after", "A = 1\n",
			map[string]interface{}{"path": "solve.py", "line": 1, "content": "B = 2"},
			"A = 1\nB = 2\n"},
		{"replace_lines", "replace_lines", "A = 1\nB = 2\n",
			map[string]interface{}{"path": "solve.py", "start_line": 1, "end_line": 1,
				"expected_first_line": "A = 1", "expected_last_line": "A = 1",
				"content": "A = 99"},
			"A = 99\nB = 2\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			full := filepath.Join(dir, "solve.py")
			if c.seed != "" {
				os.WriteFile(full, []byte(c.seed), 0o644)
			}
			ctx := ledgerToolCtx(t, dir)
			if c.seed != "" {
				// The edit tools require a current read; that guard is not
				// what this test is measuring.
				r, _ := json.Marshal(map[string]string{"path": "solve.py"})
				executeToolCall("read_file", r, ctx)
			}
			args, _ := json.Marshal(c.args)
			res := executeToolCall(c.tool, args, ctx)
			if !res.Success {
				t.Fatalf("%s failed: %v", c.tool, res.Error)
			}
			got, _ := os.ReadFile(full)
			if string(got) != c.want {
				t.Fatalf("disk bytes = %q, want %q", got, c.want)
			}
			d := ledgerOf(t, ctx, "solve.py")
			if d == nil {
				t.Fatal("the mutation was not recorded")
			}
			if d.CurrentHash != diskHash(t, full) {
				t.Errorf("ledger hash does not describe the bytes on disk")
			}
			if d.CurrentSize != len(c.want) {
				t.Errorf("CurrentSize = %d, want %d", d.CurrentSize, len(c.want))
			}
			// The tool's own evidence is copied, never upgraded.
			if d.ValidationStatus != res.ValidationStatus {
				t.Errorf("ledger status %q != tool status %q",
					d.ValidationStatus, res.ValidationStatus)
			}
			if d.CheckpointHash != "" && res.ValidationStatus != ValidationPassed {
				t.Error("checkpointed bytes that were never reported as passed")
			}
		})
	}
}

// A refused write must leave no trace: the prior bytes stand and the ledger
// does not record a generation that never happened.
func TestRefusedMutationDoesNotAdvanceTheLedger(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=keepme\n"), 0o644)
	ctx := ledgerToolCtx(t, dir)
	args, _ := json.Marshal(map[string]string{"path": ".env", "content": "SECRET=stolen\n"})
	res := executeToolCall("write_file", args, ctx)
	if res.Success {
		t.Fatal("deny-list did not refuse")
	}
	if d := ledgerOf(t, ctx, ".env"); d != nil {
		t.Errorf("a pre-dispatch refusal created a ledger entry: %+v", d)
	}
}

func TestDeleteRecordsATombstoneOnlyWhenTheFileIsGone(t *testing.T) {
	dir := t.TempDir()
	ctx := ledgerToolCtx(t, dir)
	w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": "A = 1\n"})
	executeToolCall("write_file", w, ctx)

	// Failed delete of a path this session never wrote: nothing invented.
	d1, _ := json.Marshal(map[string]string{"path": "never_existed.py"})
	if res := executeToolCall("delete_file", d1, ctx); res.Success {
		t.Fatal("deleting a missing file unexpectedly succeeded")
	}
	if d := ledgerOf(t, ctx, "never_existed.py"); d != nil {
		t.Errorf("a failed delete fabricated a tombstone: %+v", d)
	}

	d2, _ := json.Marshal(map[string]string{"path": "solve.py"})
	if res := executeToolCall("delete_file", d2, ctx); !res.Success {
		t.Fatalf("delete failed: %v", res.Error)
	}
	d := ledgerOf(t, ctx, "solve.py")
	if d == nil || !d.Tombstoned {
		t.Fatalf("delete was not tombstoned: %+v", d)
	}
	if !d.RestoreProhibited {
		t.Error("a deliberate delete must prohibit automatic restoration")
	}
	if _, s := d.CurrentValidation(); s != ValidationUnknown {
		t.Error("a deleted path still reports a current verdict")
	}
}

func TestMoveTombstonesTheSourceAndObservesTheDestinationFresh(t *testing.T) {
	dir := t.TempDir()
	ctx := ledgerToolCtx(t, dir)
	w, _ := json.Marshal(map[string]string{"path": "old.py", "content": "A = 1\n"})
	executeToolCall("write_file", w, ctx)

	m, _ := json.Marshal(map[string]string{"source": "old.py", "destination": "new.py"})
	if res := executeToolCall("move_file", m, ctx); !res.Success {
		t.Fatalf("move failed: %v", res.Error)
	}
	src := ledgerOf(t, ctx, "old.py")
	if src == nil || !src.Tombstoned || !strings.HasPrefix(src.TombstoneReason, "moved:") {
		t.Fatalf("source not tombstoned as moved: %+v", src)
	}
	dst := ledgerOf(t, ctx, "new.py")
	if dst == nil {
		t.Fatal("destination not observed")
	}
	if dst.CurrentHash != diskHash(t, filepath.Join(dir, "new.py")) {
		t.Error("destination hash does not describe the bytes on disk")
	}
	if _, s := dst.CurrentValidation(); s == ValidationPassed {
		t.Error("a rename inherited a verdict earned under the old name")
	}
}

// A shell command can rewrite anything. Paths it did not touch keep their
// evidence; paths it changed lose it, because the verdict described bytes
// that are gone.
func TestRunCommandRehashesTrackedPaths(t *testing.T) {
	dir := t.TempDir()
	ctx := ledgerToolCtx(t, dir)
	for _, f := range []string{"quiet.py", "noisy.py"} {
		w, _ := json.Marshal(map[string]string{"path": f, "content": "A = 1\n"})
		executeToolCall("write_file", w, ctx)
	}
	quietBefore := ledgerOf(t, ctx, "quiet.py").CurrentHash

	c, _ := json.Marshal(map[string]string{"command": "printf 'REWRITTEN\\n' > noisy.py"})
	if res := executeToolCall("run_command", c, ctx); res.MutationStatus != MutationUnobserved {
		t.Fatalf("run_command classification changed: %q", res.MutationStatus)
	}
	if q := ledgerOf(t, ctx, "quiet.py"); q.CurrentHash != quietBefore {
		t.Error("an untouched path was re-recorded")
	}
	n := ledgerOf(t, ctx, "noisy.py")
	if n.CurrentHash != diskHash(t, filepath.Join(dir, "noisy.py")) {
		t.Error("ledger did not catch up to the command's rewrite")
	}
	if _, s := n.CurrentValidation(); s != ValidationUnknown {
		t.Error("a command-rewritten path kept a verdict about bytes that are gone")
	}
}

// The hazard describes work that may exist, not attempts that were made.
// A call the tool refused before dispatch created nothing to be hazardous
// about; a call that may have reached the sandbox and came back with no job
// id did, and nothing can reap that.
func TestBackgroundHazardFollowsDispatchNotTheAttempt(t *testing.T) {
	t.Run("refused before dispatch leaves no hazard", func(t *testing.T) {
		ctx := ledgerToolCtx(t, t.TempDir()) // VerifyOnHost: never dispatches
		ctx.SandboxURL = ""
		args, _ := json.Marshal(map[string]string{"command": "python -m http.server"})
		if res := executeToolCall("run_background", args, ctx); res.Success {
			t.Fatal("expected the start to be refused")
		}
		if workspaceHazardous(ctx) {
			t.Error("a start that never dispatched left a hazard nothing can lower")
		}
	})

	t.Run("may have dispatched, no job id: unresolvable hazard", func(t *testing.T) {
		ctx := ledgerToolCtx(t, t.TempDir())
		ctx.VerifyOnHost = false
		ctx.SandboxURL = "http://127.0.0.1:1" // reachable-looking, actually not
		args, _ := json.Marshal(map[string]string{"command": "python -m http.server"})
		if res := executeToolCall("run_background", args, ctx); res.Success {
			t.Fatal("expected the start to fail")
		}
		if !workspaceHazardous(ctx) {
			t.Error("a start that may have run left no hazard")
		}
		// Nothing to reap: reaping cannot clear what it cannot name.
		reapSessionBackgroundJobs(ctx)
		if !workspaceHazardous(ctx) {
			t.Error("an unidentified job was cleared by reaping nothing")
		}
	})
}

func TestStopBackgroundClearsTheHazardOnlyOnAReapedExit(t *testing.T) {
	var exitCode *int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/jobs/start"):
			json.NewEncoder(w).Encode(map[string]interface{}{"job_id": "j1", "pid": 4242})
		case strings.Contains(r.URL.Path, "/output"):
			// The settle-window tail: the job is live, so it is tracked and
			// owns a hazard until something confirms its exit.
			json.NewEncoder(w).Encode(map[string]interface{}{
				"job_id": "j1", "running": true, "stdout": []string{},
				"stderr": []string{}, "elapsed_sec": 0.1, "command": "sleep 60"})
		case strings.HasSuffix(r.URL.Path, "/stop"):
			out := map[string]interface{}{"job_id": "j1", "killed": true,
				"stdout": []string{}, "stderr": []string{}}
			if exitCode != nil {
				out["exit_code"] = *exitCode
			}
			json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := ledgerToolCtx(t, t.TempDir())
	ctx.VerifyOnHost = false // so the start actually dispatches
	ctx.SandboxURL = srv.URL
	start, _ := json.Marshal(map[string]string{"command": "sleep 60"})
	executeToolCall("run_background", start, ctx)
	if !workspaceHazardous(ctx) {
		t.Fatal("run_background did not raise the hazard")
	}

	// Signalled but not reaped: the writer may still be flushing.
	stop, _ := json.Marshal(map[string]string{"job_id": "j1"})
	executeToolCall("stop_background", stop, ctx)
	if !workspaceHazardous(ctx) {
		t.Error("hazard cleared without a confirmed exit")
	}

	zero := 0
	exitCode = &zero
	executeToolCall("run_background", start, ctx)
	executeToolCall("stop_background", stop, ctx)
	executeToolCall("stop_background", stop, ctx)
	if workspaceHazardous(ctx) {
		t.Error("reaped exits did not clear the hazard")
	}
}

// The outcome table for the seven direct mutators, measured rather than
// asserted from the source. Every branch reachable without the V3 service is
// driven through the real boundary; the table it produces is logged, and the
// invariants that must hold for EVERY row are enforced:
//
//  1. a recorded current hash always describes the bytes on disk;
//  2. a checkpoint exists only where the tool reported an explicit pass;
//  3. a branch that proves it mutated nothing records nothing;
//  4. a branch that DID change disk never reports MutationUnknown, which
//     would mean an unmigrated producer is still guessing.
func TestDirectMutatorOutcomeTable(t *testing.T) {
	type step struct {
		tool string
		args map[string]interface{}
	}
	cases := []struct {
		name  string
		seed  map[string]string
		read  []string
		steps []step
		path  string // the deliverable this row is about
	}{
		{"write_file/new non-code", nil, nil,
			[]step{{"write_file", map[string]interface{}{"path": "notes.txt", "content": "hi\n"}}}, "notes.txt"},
		{"write_file/new code", nil, nil,
			[]step{{"write_file", map[string]interface{}{"path": "m.py", "content": "A = 1\n"}}}, "m.py"},
		{"write_file/deny-list refusal", map[string]string{".env": "S=1\n"}, nil,
			[]step{{"write_file", map[string]interface{}{"path": ".env", "content": "S=2\n"}}}, ".env"},
		{"write_file/escapes workspace", nil, nil,
			[]step{{"write_file", map[string]interface{}{"path": "../out.py", "content": "A = 1\n"}}}, "../out.py"},

		{"edit_file/applied", map[string]string{"m.py": "A = 1\n"}, []string{"m.py"},
			[]step{{"edit_file", map[string]interface{}{"path": "m.py", "old_str": "A = 1", "new_str": "A = 2"}}}, "m.py"},
		{"edit_file/old_str absent", map[string]string{"m.py": "A = 1\n"}, []string{"m.py"},
			[]step{{"edit_file", map[string]interface{}{"path": "m.py", "old_str": "ZZZ", "new_str": "Q"}}}, "m.py"},
		{"edit_file/not read first", map[string]string{"m.py": "A = 1\n"}, nil,
			[]step{{"edit_file", map[string]interface{}{"path": "m.py", "old_str": "A = 1", "new_str": "A = 2"}}}, "m.py"},

		{"structural_edit/function body", map[string]string{"m.py": "def f():\n    return 1\n"}, []string{"m.py"},
			[]step{{"structural_edit", map[string]interface{}{"path": "m.py",
				"selector": "function:f", "content": "def f():\n    return 2\n"}}}, "m.py"},
		{"structural_edit/selector missing", map[string]string{"m.py": "def f():\n    return 1\n"}, []string{"m.py"},
			[]step{{"structural_edit", map[string]interface{}{"path": "m.py",
				"selector": "function:nope", "content": "def nope():\n    pass\n"}}}, "m.py"},

		{"insert_after/applied", map[string]string{"m.py": "A = 1\n"}, []string{"m.py"},
			[]step{{"insert_after", map[string]interface{}{"path": "m.py", "line": 1, "content": "B = 2"}}}, "m.py"},
		{"insert_after/line past end", map[string]string{"m.py": "A = 1\n"}, []string{"m.py"},
			[]step{{"insert_after", map[string]interface{}{"path": "m.py", "line": 99, "content": "B = 2"}}}, "m.py"},

		{"replace_lines/applied", map[string]string{"m.py": "A = 1\nB = 2\n"}, []string{"m.py"},
			[]step{{"replace_lines", map[string]interface{}{"path": "m.py", "start_line": 1, "end_line": 1,
				"expected_first_line": "A = 1", "expected_last_line": "A = 1", "content": "A = 9"}}}, "m.py"},
		{"replace_lines/anchor mismatch", map[string]string{"m.py": "A = 1\nB = 2\n"}, []string{"m.py"},
			[]step{{"replace_lines", map[string]interface{}{"path": "m.py", "start_line": 1, "end_line": 1,
				"expected_first_line": "WRONG", "expected_last_line": "WRONG", "content": "A = 9"}}}, "m.py"},

		{"delete_file/applied", map[string]string{"m.py": "A = 1\n"}, nil,
			[]step{{"delete_file", map[string]interface{}{"path": "m.py"}}}, "m.py"},
		{"delete_file/missing", nil, nil,
			[]step{{"delete_file", map[string]interface{}{"path": "gone.py"}}}, "gone.py"},

		{"move_file/applied", map[string]string{"old.py": "A = 1\n"}, nil,
			[]step{{"move_file", map[string]interface{}{"source": "old.py", "destination": "new.py"}}}, "old.py"},
		{"move_file/missing source", nil, nil,
			[]step{{"move_file", map[string]interface{}{"source": "gone.py", "destination": "new.py"}}}, "gone.py"},
	}

	var unknownMutators []string
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for f, body := range c.seed {
				os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644)
			}
			ctx := ledgerToolCtx(t, dir)
			for _, f := range c.read {
				r, _ := json.Marshal(map[string]string{"path": f})
				executeToolCall("read_file", r, ctx)
			}
			var res *ToolResult
			for _, s := range c.steps {
				args, _ := json.Marshal(s.args)
				res = executeToolCall(s.tool, args, ctx)
			}

			d := ledgerOf(t, ctx, c.path)
			full := filepath.Join(dir, c.path)
			_, statErr := os.Stat(full)

			state := "not recorded"
			if d != nil {
				state = fmt.Sprintf("gen=%d tomb=%v ckpt=%v", d.Generation,
					d.Tombstoned, d.CheckpointHash != "")
			}
			t.Logf("mutation=%-10q validation=%s/%-14q ledger[%s]",
				res.MutationStatus, res.ValidationKind, res.ValidationStatus, state)

			// (3) a producer that asserts disk did not change records nothing.
			if (res.MutationStatus == MutationNone || res.MutationStatus == MutationRefused) && d != nil {
				t.Errorf("a branch that did not change disk created a ledger entry: %+v", d)
			}
			// (4) a producer that has not been migrated yet. Recorded, not
			// judged: this phase reports which branches still guess.
			if res.MutationStatus == MutationUnknown {
				unknownMutators = append(unknownMutators, c.name)
			}
			if d == nil {
				return
			}
			// (1) the hash describes what is actually there.
			if !d.Tombstoned && statErr == nil && d.CurrentHash != diskHash(t, full) {
				t.Errorf("ledger hash does not describe the bytes on disk")
			}
			if d.Tombstoned && statErr == nil {
				t.Error("tombstoned a path that is still on disk")
			}
			// (2) checkpoints come only from an explicit pass.
			if d.CheckpointHash != "" && res.ValidationStatus != ValidationPassed {
				t.Errorf("checkpointed bytes reported as %q, not passed", res.ValidationStatus)
			}
		})
	}
	// Phase 3A.1 migrated every branch reachable here. A new one arriving
	// unclassified is a producer defect, not a note.
	if len(unknownMutators) > 0 {
		t.Errorf("branches reporting MutationUnknown: %v — a direct mutator "+
			"owes a local classification; the boundary will not invent one",
			unknownMutators)
	}
}

// --- Phase 3A: the ledger through the production agent loop -----------------
//
// Two claims, proved together on one run of runAgentLoop:
//
//	the ledger's current hash describes the bytes a reader finds on disk
//	after the loop returns, including a rewrite performed by a shell
//	command rather than by an edit tool; and
//
//	the OUTWARD behaviour -- the full ordered event stream and the final
//	workspace -- is byte-for-byte what the parent commit produced.
//
// The second claim is about SSE and disk, and only those. Phase 3A described
// it as "external behaviour", which overstated it: the model-facing prompt
// was NOT unchanged, because classifying a producer put four new keys in the
// tool message. TestModelPromptBytesAreUnchangedByClassification covers that
// half, and the projection in types.go is what makes it true. ATLAS_LEDGER_TRANSCRIPT
// dumps the transcript so the identical fixture can be run on the parent tree
// and diffed; the hash below is that transcript, pinned.
//
// Verified against parent 5676e49 when Phase 3A landed: the same 52 events,
// byte-identical. Phase 2B changed exactly ONE of those lines -- the terminal
// gained the additive `status` and `reason` keys -- and the other 51 were
// still byte-for-byte the parent's, with the terminal's own `summary`
// unchanged.
//
// The 52-event transcript carried a defect of its own, pinned along with
// everything else: `A = 1\n` was written cleanly and the exit gate still
// announced "solve.py warned and never executed" three times, because the
// warned-run set stored a false value that a key-only reader took as a live
// warning. Removing that drops 18 events -- three bounces, their tool
// results, and the three turns spent on them -- and the run finishes at turn
// 4 instead of turn 7. The 34 that remain are the parent's, in order and
// byte-for-byte, which is what this pin is for.
//
// Re-pinned at 35 events: this fixture's service answers the unresolved-name
// check with 503, and an unreachable check service is now shown to the client
// once per session (noteCheckServiceUnavailable). That one `text` event is the
// only difference; the ledger is still invisible.
//
// Re-pinned for the terminal event alone: nothing in this fixture runs
// solve.py, so the completion's reason is deliverables_parse_only and the
// summary adds "solve.py parses, but nothing in this run ran it." Every other
// event is unchanged; the ledger is still invisible.
const ledgerLoopTranscriptHash = "8508e55eb149f8123201993fa7646888b6d47e548f6b57cf98f4f6153f0104f7"

// prompt_tokens is elided for a fixture reason rather than a timing one: the
// workspace path is part of the system prompt and t.TempDir() varies in
// length between runs, moving the count by a token. Every other field,
// including every tool call and result, is compared exactly.
//
// volatileEventKeys are elided from the transcript: they carry wall-clock
// facts that differ between two runs of the same code, so including them
// would make the comparison meaningless rather than strict.
var volatileEventKeys = map[string]bool{
	"ms": true, "duration": true, "duration_ms": true, "elapsed": true,
	"elapsed_ms": true, "timestamp": true, "seconds": true, "wall_s": true,
	"started_at": true, "ended_at": true, "pid": true, "prompt_tokens": true,
}

func stripVolatile(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, sub := range t {
			if volatileEventKeys[k] || strings.HasSuffix(k, "_ms") {
				continue
			}
			out[k] = stripVolatile(sub)
		}
		return out
	case []interface{}:
		out := make([]interface{}, 0, len(t))
		for _, sub := range t {
			out = append(out, stripVolatile(sub))
		}
		return out
	}
	return v
}

func TestLedgerTracksFinalDiskBytesThroughTheAgentLoop(t *testing.T) {
	dir := t.TempDir()
	rel := "solve.py"

	var turnMu sync.Mutex
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
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
		io.Copy(io.Discard, r.Body)
		turnMu.Lock()
		turn++
		n := turn
		turnMu.Unlock()

		var payload map[string]interface{}
		switch n {
		case 1:
			payload = map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": rel, "content": "A = 1\n"}}
		case 2:
			payload = map[string]interface{}{"type": "tool_call", "name": "read_file",
				"args": map[string]string{"path": rel}}
		case 3:
			payload = map[string]interface{}{"type": "tool_call", "name": "edit_file",
				"args": map[string]string{"path": rel, "old_str": "A = 1", "new_str": "A = 2"}}
		case 4:
			// The case the ledger exists for: bytes changing outside the
			// edit tools, so the last recorded verdict stops being about
			// what is on disk.
			payload = map[string]interface{}{"type": "tool_call", "name": "run_command",
				"args": map[string]string{"command": "printf 'A = 3\\n' > " + rel}}
		default:
			payload = map[string]interface{}{"type": "done",
				"summary": "wrote solve.py"}
		}
		call, _ := json.Marshal(payload)
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	defer srv.Close()

	var evMu sync.Mutex
	var transcript strings.Builder
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 8
	ctx.StreamFn = func(eventType string, data interface{}) {
		raw, _ := json.Marshal(data)
		var generic interface{}
		json.Unmarshal(raw, &generic)
		clean, _ := json.Marshal(stripVolatile(generic))
		evMu.Lock()
		fmt.Fprintf(&transcript, "%s %s\n", eventType, clean)
		evMu.Unlock()
	}

	if err := runAgentLoop(ctx, "Create solve.py."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	full := filepath.Join(dir, rel)
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if string(got) != "A = 3\n" {
		t.Fatalf("final disk bytes = %q, want the command's rewrite", got)
	}

	d := ledgerOf(t, ctx, rel)
	if d == nil {
		t.Fatal("the loop's deliverable was never recorded")
	}
	if d.CurrentHash != hashBytes(got) {
		t.Errorf("ledger hash %s does not describe the final bytes %s",
			d.CurrentHash[:12], hashBytes(got)[:12])
	}
	if _, s := d.CurrentValidation(); s == ValidationPassed {
		t.Error("a shell rewrite inherited the edit tool's verdict")
	}
	for key, entry := range ctx.Ledger {
		if entry.Tombstoned {
			continue
		}
		b, err := os.ReadFile(key)
		if err != nil {
			continue
		}
		if entry.CurrentHash != hashBytes(b) {
			t.Errorf("%s: ledger hash disagrees with disk", key)
		}
	}

	evMu.Lock()
	text := transcript.String()
	evMu.Unlock()
	if out := os.Getenv("ATLAS_LEDGER_TRANSCRIPT"); out != "" {
		os.WriteFile(out, []byte(text), 0o600)
	}
	if h := hashBytes([]byte(text)); ledgerLoopTranscriptHash != "" && h != ledgerLoopTranscriptHash {
		t.Errorf("event stream changed (%s, want %s); the ledger must be "+
			"invisible from outside:\n%s", h[:12], ledgerLoopTranscriptHash[:12], text)
	}
	t.Logf("transcript: %d events, %d bytes, sha256 %s",
		strings.Count(text, "\n"), len(text), hashBytes([]byte(text)))
}

// Recorded, not fixed. SessionWrites is keyed on the raw model-supplied path,
// so two spellings of one file are two entries; the ledger keys on the
// resolved path and is not affected. Pinned so the divergence is visible and
// a later canonicalisation is a deliberate change with its own evidence.
func TestSessionWritesKeyingIsRawWhileTheLedgerIsCanonical(t *testing.T) {
	dir := t.TempDir()
	ctx := ledgerToolCtx(t, dir)
	for _, spelling := range []string{"solve.py", "./solve.py"} {
		args, _ := json.Marshal(map[string]string{"path": spelling, "content": "A = 1\n"})
		if res := executeToolCall("write_file", args, ctx); !res.Success {
			t.Fatalf("write %q failed: %v", spelling, res.Error)
		}
	}
	if len(ctx.SessionWrites) != 2 {
		t.Errorf("SessionWrites keying changed: %v — if this was fixed "+
			"deliberately, update the note on the field", ctx.SessionWrites)
	}
	if len(ctx.Ledger) != 1 {
		t.Errorf("the ledger split one file across %d entries: %v",
			len(ctx.Ledger), ctx.Ledger)
	}
}

// Checkpoint promotion on a production route rather than a direct call. A new
// code file routes through the syntax gate, so with a sandbox answering the
// write earns an explicit pass and those exact bytes become the path's
// checkpoint. The follow-up write is deliberately broken: it must be recorded
// and must NOT displace the checkpoint the passing bytes hold.
func TestCheckpointPromotionThroughTheWriteRoute(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/syntax-check") {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Code string `json:"code"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		bodies = append(bodies, in.Code)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"valid": !strings.Contains(in.Code, "def broken(")})
	}))
	defer srv.Close()

	dir := t.TempDir()
	ctx := ledgerToolCtx(t, dir)
	ctx.SandboxURL = srv.URL

	good := "def f():\n    return 1\n"
	args, _ := json.Marshal(map[string]string{"path": "solve.py", "content": good})
	res := executeToolCall("write_file", args, ctx)
	if res.ValidationStatus != ValidationPassed {
		t.Fatalf("the write route did not produce a pass (%s/%s); this test "+
			"proves nothing without one", res.ValidationKind, res.ValidationStatus)
	}
	d := ledgerOf(t, ctx, "solve.py")
	if d.CheckpointHash != hashBytes([]byte(good)) {
		t.Fatalf("passing bytes were not checkpointed: %+v", d)
	}

	// The syntax gate refuses a broken rewrite outright, so disk and the
	// checkpoint both stand.
	args2, _ := json.Marshal(map[string]string{"path": "solve.py", "content": "def broken(\n"})
	if r := executeToolCall("write_file", args2, ctx); r.MutationStatus != MutationRefused {
		t.Fatalf("expected the gate to refuse the broken rewrite, got %q", r.MutationStatus)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "solve.py")); string(got) != good {
		t.Fatalf("a refused write reached disk: %q", got)
	}

	// Bytes that DO land without a verdict -- a shell rewrite -- are recorded
	// and must not become the checkpoint either.
	cmd, _ := json.Marshal(map[string]string{"command": "printf 'x = (\\n' > solve.py"})
	executeToolCall("run_command", cmd, ctx)
	d = ledgerOf(t, ctx, "solve.py")
	if d.CheckpointHash != hashBytes([]byte(good)) {
		t.Errorf("a failing write displaced the checkpoint: %+v", d)
	}
	if string(d.CheckpointBytes) != good {
		t.Errorf("checkpoint bytes = %q", d.CheckpointBytes)
	}
	if d.CurrentHash == d.CheckpointHash {
		t.Error("the shell rewrite was not recorded as the current bytes")
	}
	if d.CurrentHash != diskHash(t, filepath.Join(dir, "solve.py")) {
		t.Error("current hash does not describe the bytes on disk")
	}
	if _, s := d.CurrentValidation(); s == ValidationPassed {
		t.Error("the failing bytes read as passed")
	}
}

// --- Phase 3A.1: family-complete direct-mutator classification --------------
//
// Every branch of the seven direct mutators that a session can reach, driven
// through the real boundary. A direct mutator owes a local classification --
// the boundary refuses to invent one -- so an unclassified branch is a
// producer that has not spoken, and the ledger downstream cannot tell it
// apart from one that deliberately did nothing.
//
// Each row states what the branch PROVES about disk, and the test checks that
// claim against the filesystem rather than against the tool's own words.

type mutatorCase struct {
	name string
	tool string
	// seed files written before the call; read lists files to read_file first.
	seed map[string]string
	read []string
	args string
	// setup runs last, after seeding, and may break the filesystem.
	setup func(t *testing.T, dir string)
	// v3 answers structural_edit's splice request when non-nil.
	v3 func(w http.ResponseWriter, r *http.Request)

	wantMutation MutationStatus
	// the deliverable this row is about, and its bytes afterwards. An empty
	// want means "must not exist".
	path string
	want string
	// skip records a branch that exists in production and cannot be reached
	// from a test process, with the reason.
	skip string
}

func runMutatorCase(t *testing.T, c mutatorCase) {
	t.Helper()
	if c.skip != "" {
		t.Skip(c.skip)
	}
	dir := t.TempDir()
	for f, body := range c.seed {
		full := filepath.Join(dir, f)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := ledgerToolCtx(t, dir)
	if c.v3 != nil {
		srv := httptest.NewServer(http.HandlerFunc(c.v3))
		defer srv.Close()
		ctx.V3URL = srv.URL
		ctx.SandboxURL = srv.URL
	}
	for _, f := range c.read {
		r, _ := json.Marshal(map[string]string{"path": f})
		executeToolCall("read_file", r, ctx)
	}
	if c.setup != nil {
		c.setup(t, dir)
	}

	res := executeToolCall(c.tool, json.RawMessage(c.args), ctx)

	ledgerNote := "not recorded"
	if c.path != "" {
		if d := ledgerOf(t, ctx, c.path); d != nil {
			k, st := d.CurrentValidation()
			ledgerNote = fmt.Sprintf("gen=%d tomb=%v ckpt=%v current=%s/%s",
				d.Generation, d.Tombstoned, d.CheckpointHash != "", k, st)
		}
	}
	t.Logf("%-15s | %-9s | %-10s | %-14s | %s", c.name,
		res.MutationStatus, res.ValidationKind, res.ValidationStatus, ledgerNote)

	if !res.Classified() {
		t.Errorf("unclassified branch: mutation=%q kind=%q status=%q err=%q",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus, res.Error)
	}
	if res.MutationStatus != c.wantMutation {
		t.Errorf("MutationStatus = %q, want %q (err=%q)",
			res.MutationStatus, c.wantMutation, res.Error)
	}
	// The claim is checked against disk, not against the result.
	if c.path != "" {
		full := filepath.Join(dir, c.path)
		got, err := os.ReadFile(full)
		switch {
		case c.want == "" && err == nil:
			t.Errorf("%s still exists with %q", c.path, got)
		case c.want != "" && err != nil:
			t.Errorf("%s missing: %v", c.path, err)
		case c.want != "" && string(got) != c.want:
			t.Errorf("%s = %q, want %q", c.path, got, c.want)
		}
	}
	// A passed verdict must name the bytes that are there.
	if res.ValidationStatus == ValidationPassed && c.path != "" {
		if d := ledgerOf(t, ctx, c.path); d != nil {
			if k, s := d.CurrentValidation(); s != ValidationPassed || k == ValidationKindUnknown {
				t.Errorf("a pass did not survive into the ledger: %v/%v", k, s)
			}
		}
	}
}

// v3SpliceStub answers /internal/structural_edit by replacing the named
// function's body, and /syntax-check as valid, so structural_edit's branches
// are reachable without the real service.
func v3SpliceStub(t *testing.T, newContent string, ok bool, errMsg string) func(http.ResponseWriter, *http.Request) {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/internal/structural_edit"):
			var in struct{ Source, Selector, Content string }
			json.NewDecoder(r.Body).Decode(&in)
			body := map[string]interface{}{"success": ok, "error": errMsg,
				"language": "python", "old_size": len(in.Source), "new_size": len(newContent)}
			if ok {
				out := newContent
				if out == "\x00same" {
					out = in.Source
				}
				body["new_content"] = out
				body["new_size"] = len(out)
			}
			json.NewEncoder(w).Encode(body)
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}
}

func TestEveryDeleteFileOutcomeIsClassified(t *testing.T) {
	for _, c := range []mutatorCase{
		{name: "malformed args", tool: "delete_file", args: `{"path":123}`,
			wantMutation: MutationNone},
		{name: "empty path", tool: "delete_file", args: `{"path":"  "}`,
			wantMutation: MutationNone},
		{name: "target absent", tool: "delete_file", args: `{"path":"gone.py"}`,
			wantMutation: MutationNone, path: "gone.py"},
		{name: "directory not empty", tool: "delete_file",
			seed: map[string]string{"pkg/a.py": "A = 1\n"}, args: `{"path":"pkg"}`,
			wantMutation: MutationRefused, path: "pkg/a.py", want: "A = 1\n"},
		{name: "removal fails", tool: "delete_file",
			seed: map[string]string{"locked/a.py": "A = 1\n"}, args: `{"path":"locked/a.py"}`,
			setup: func(t *testing.T, dir string) {
				if err := os.Chmod(filepath.Join(dir, "locked"), 0o555); err != nil {
					t.Skip("cannot make a directory read-only here")
				}
				t.Cleanup(func() { os.Chmod(filepath.Join(dir, "locked"), 0o755) })
			},
			wantMutation: MutationFailed, path: "locked/a.py", want: "A = 1\n"},
		{name: "removed", tool: "delete_file",
			seed: map[string]string{"a.py": "A = 1\n"}, args: `{"path":"a.py"}`,
			wantMutation: MutationApplied, path: "a.py"},
		{name: "empty directory removed", tool: "delete_file",
			setup: func(t *testing.T, dir string) { os.Mkdir(filepath.Join(dir, "empty"), 0o755) },
			args:  `{"path":"empty"}`, wantMutation: MutationApplied, path: "empty"},
	} {
		t.Run(c.name, func(t *testing.T) { runMutatorCase(t, c) })
	}
}

func TestEveryMoveFileOutcomeIsClassified(t *testing.T) {
	for _, c := range []mutatorCase{
		{name: "malformed args", tool: "move_file", args: `{"source":123}`,
			wantMutation: MutationNone},
		{name: "missing destination", tool: "move_file", args: `{"source":"a.py","destination":" "}`,
			wantMutation: MutationNone},
		{name: "source absent", tool: "move_file", args: `{"source":"gone.py","destination":"new.py"}`,
			wantMutation: MutationNone, path: "new.py"},
		{name: "same path", tool: "move_file",
			seed: map[string]string{"a.py": "A = 1\n"}, args: `{"source":"a.py","destination":"./a.py"}`,
			wantMutation: MutationNone, path: "a.py", want: "A = 1\n"},
		{name: "destination occupied", tool: "move_file",
			seed:         map[string]string{"a.py": "A = 1\n", "b.py": "B = 2\n"},
			args:         `{"source":"a.py","destination":"b.py"}`,
			wantMutation: MutationRefused, path: "b.py", want: "B = 2\n"},
		{name: "cannot create destination dir", tool: "move_file",
			seed: map[string]string{"a.py": "A = 1\n", "ro/keep": "x"},
			setup: func(t *testing.T, dir string) {
				if err := os.Chmod(filepath.Join(dir, "ro"), 0o555); err != nil {
					t.Skip("cannot make a directory read-only here")
				}
				t.Cleanup(func() { os.Chmod(filepath.Join(dir, "ro"), 0o755) })
			},
			args:         `{"source":"a.py","destination":"ro/sub/a.py"}`,
			wantMutation: MutationFailed, path: "a.py", want: "A = 1\n"},
		{name: "renamed", tool: "move_file",
			seed: map[string]string{"a.py": "A = 1\n"}, args: `{"source":"a.py","destination":"new.py"}`,
			wantMutation: MutationApplied, path: "new.py", want: "A = 1\n"},
		{name: "moved into a directory", tool: "move_file",
			seed: map[string]string{"a.py": "A = 1\n"},
			setup: func(t *testing.T, dir string) {
				os.Mkdir(filepath.Join(dir, "sub"), 0o755)
			},
			args: `{"source":"a.py","destination":"sub"}`,
			// The tool resolves the directory to sub/a.py.
			wantMutation: MutationApplied, path: "sub/a.py", want: "A = 1\n"},
		{name: "cross-device copy path", tool: "move_file",
			skip: "os.Rename only fails across filesystems; one temp dir cannot straddle two"},
	} {
		t.Run(c.name, func(t *testing.T) { runMutatorCase(t, c) })
	}
}

func TestEveryInsertAfterOutcomeIsClassified(t *testing.T) {
	seed := map[string]string{"m.py": "A = 1\n"}
	for _, c := range []mutatorCase{
		{name: "malformed args", tool: "insert_after", args: `{"line":"x"}`,
			wantMutation: MutationNone},
		{name: "path missing", tool: "insert_after", args: `{"line":1,"content":"B = 2"}`,
			wantMutation: MutationNone},
		{name: "content empty", tool: "insert_after", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","line":1,"content":""}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\n"},
		{name: "file not read", tool: "insert_after", seed: seed,
			args:         `{"path":"m.py","line":1,"content":"B = 2"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\n"},
		{name: "file vanished after the read", tool: "insert_after", seed: seed, read: []string{"m.py"},
			setup:        func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "m.py")) },
			args:         `{"path":"m.py","line":1,"content":"B = 2"}`,
			wantMutation: MutationNone, path: "m.py"},
		{name: "line past end", tool: "insert_after", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","line":99,"content":"B = 2"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\n"},
		{name: "inserted", tool: "insert_after", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","line":1,"content":"B = 2"}`,
			wantMutation: MutationApplied, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "inserted at the top", tool: "insert_after", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","line":0,"content":"B = 2"}`,
			wantMutation: MutationApplied, path: "m.py", want: "B = 2\nA = 1\n"},
	} {
		t.Run(c.name, func(t *testing.T) { runMutatorCase(t, c) })
	}
}

func TestEveryReplaceLinesOutcomeIsClassified(t *testing.T) {
	seed := map[string]string{"m.py": "A = 1\nB = 2\n"}
	big := map[string]string{"m.py": strings.Repeat("X = 0\n", 100)}
	for _, c := range []mutatorCase{
		{name: "malformed args", tool: "replace_lines", args: `{"start_line":"x"}`,
			wantMutation: MutationNone},
		{name: "path missing", tool: "replace_lines", args: `{"start_line":1,"end_line":1}`,
			wantMutation: MutationNone},
		{name: "file not read", tool: "replace_lines", seed: seed,
			args:         `{"path":"m.py","start_line":1,"end_line":1,"expected_first_line":"A = 1","expected_last_line":"A = 1","content":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "file vanished after the read", tool: "replace_lines", seed: seed, read: []string{"m.py"},
			setup:        func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "m.py")) },
			args:         `{"path":"m.py","start_line":1,"end_line":1,"expected_first_line":"A = 1","expected_last_line":"A = 1","content":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py"},
		{name: "range invalid", tool: "replace_lines", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","start_line":5,"end_line":9,"expected_first_line":"A = 1","expected_last_line":"A = 1","content":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "span too large", tool: "replace_lines", seed: big, read: []string{"m.py"},
			args:         `{"path":"m.py","start_line":1,"end_line":99,"expected_first_line":"X = 0","expected_last_line":"X = 0","content":"X = 1"}`,
			wantMutation: MutationNone, path: "m.py", want: strings.Repeat("X = 0\n", 100)},
		{name: "anchor mismatch", tool: "replace_lines", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","start_line":1,"end_line":1,"expected_first_line":"WRONG","expected_last_line":"WRONG","content":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "replacement identical", tool: "replace_lines", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","start_line":1,"end_line":1,"expected_first_line":"A = 1","expected_last_line":"A = 1","content":"A = 1"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "replaced", tool: "replace_lines", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","start_line":1,"end_line":1,"expected_first_line":"A = 1","expected_last_line":"A = 1","content":"A = 9"}`,
			wantMutation: MutationApplied, path: "m.py", want: "A = 9\nB = 2\n"},
	} {
		t.Run(c.name, func(t *testing.T) { runMutatorCase(t, c) })
	}
}

func TestEveryEditFileOutcomeIsClassified(t *testing.T) {
	seed := map[string]string{"m.py": "A = 1\nB = 2\n"}
	for _, c := range []mutatorCase{
		{name: "malformed args", tool: "edit_file", args: `{"path":123}`,
			wantMutation: MutationNone},
		{name: "empty path", tool: "edit_file", args: `{"path":" ","old_str":"a","new_str":"b"}`,
			wantMutation: MutationNone},
		{name: "old_str empty", tool: "edit_file", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","old_str":"","new_str":"b"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "file not read", tool: "edit_file", seed: seed,
			args:         `{"path":"m.py","old_str":"A = 1","new_str":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "file vanished after the read", tool: "edit_file", seed: seed, read: []string{"m.py"},
			setup:        func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "m.py")) },
			args:         `{"path":"m.py","old_str":"A = 1","new_str":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py"},
		{name: "old_str absent", tool: "edit_file", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","old_str":"ZZZ","new_str":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "ambiguous match", tool: "edit_file",
			seed: map[string]string{"m.py": "A = 1\nA = 1\n"}, read: []string{"m.py"},
			args:         `{"path":"m.py","old_str":"A = 1","new_str":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nA = 1\n"},
		{name: "no-op edit", tool: "edit_file", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","old_str":"A = 1","new_str":"A = 1"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "applied", tool: "edit_file", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","old_str":"A = 1","new_str":"A = 9"}`,
			wantMutation: MutationApplied, path: "m.py", want: "A = 9\nB = 2\n"},
	} {
		t.Run(c.name, func(t *testing.T) { runMutatorCase(t, c) })
	}
}

func TestEveryStructuralEditOutcomeIsClassified(t *testing.T) {
	src := "def f():\n    return 1\n"
	seed := map[string]string{"m.py": src}
	spliced := "def f():\n    return 2\n"
	for _, c := range []mutatorCase{
		{name: "malformed args", tool: "structural_edit", args: `{"path":123}`,
			wantMutation: MutationNone},
		{name: "empty path", tool: "structural_edit", args: `{"path":" ","selector":"function:f","content":"x"}`,
			wantMutation: MutationNone},
		{name: "empty selector", tool: "structural_edit", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","selector":" ","content":"x"}`,
			wantMutation: MutationNone, path: "m.py", want: src},
		{name: "file not read", tool: "structural_edit", seed: seed,
			args:         `{"path":"m.py","selector":"function:f","content":"def f():\n    return 2\n"}`,
			wantMutation: MutationNone, path: "m.py", want: src},
		{name: "empty content deletes the node", tool: "structural_edit", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","selector":"function:f","content":"   "}`,
			wantMutation: MutationNone, path: "m.py", want: src},
		{name: "runaway content", tool: "structural_edit", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","selector":"function:f","content":"` + strings.Repeat("x", 9000) + `"}`,
			wantMutation: MutationNone, path: "m.py", want: src},
		{name: "v3 unreachable", tool: "structural_edit", seed: seed, read: []string{"m.py"},
			args:         `{"path":"m.py","selector":"function:f","content":"def f():\n    return 2\n"}`,
			wantMutation: MutationNone, path: "m.py", want: src},
		{name: "selector matched nothing", tool: "structural_edit", seed: seed, read: []string{"m.py"},
			v3:           v3SpliceStub(t, "", false, "selector function:nope not found"),
			args:         `{"path":"m.py","selector":"function:nope","content":"def nope():\n    pass\n"}`,
			wantMutation: MutationNone, path: "m.py", want: src},
		{name: "splice is a no-op", tool: "structural_edit", seed: seed, read: []string{"m.py"},
			v3:           v3SpliceStub(t, "\x00same", true, ""),
			args:         `{"path":"m.py","selector":"function:f","content":"def f():\n    return 1\n"}`,
			wantMutation: MutationNone, path: "m.py", want: src},
		{name: "spliced", tool: "structural_edit", seed: seed, read: []string{"m.py"},
			v3:           v3SpliceStub(t, spliced, true, ""),
			args:         `{"path":"m.py","selector":"function:f","content":"def f():\n    return 2\n"}`,
			wantMutation: MutationApplied, path: "m.py", want: spliced},
	} {
		t.Run(c.name, func(t *testing.T) { runMutatorCase(t, c) })
	}
}

func TestEveryWriteFileOutcomeIsClassified(t *testing.T) {
	for _, c := range []mutatorCase{
		{name: "malformed args", tool: "write_file", args: `{"path":123}`,
			wantMutation: MutationNone},
		{name: "empty path", tool: "write_file", args: `{"path":" ","content":"x"}`,
			wantMutation: MutationNone},
		{name: "deny-list target", tool: "write_file",
			seed: map[string]string{".env": "S=1\n"}, args: `{"path":".env","content":"S=2\n"}`,
			wantMutation: MutationNone, path: ".env", want: "S=1\n"},
		{name: "escapes the workspace", tool: "write_file",
			args: `{"path":"../out.py","content":"A = 1\n"}`, wantMutation: MutationNone},
		{name: "echoed write", tool: "write_file",
			seed:         map[string]string{"m.py": strings.Repeat("A = 1\n", 40)},
			args:         `{"path":"m.py","content":"` + strings.Repeat("A = 1\\n", 40) + `"}`,
			wantMutation: MutationRefused, path: "m.py", want: strings.Repeat("A = 1\n", 40)},
		{name: "written", tool: "write_file",
			args:         `{"path":"m.py","content":"A = 1\n"}`,
			wantMutation: MutationApplied, path: "m.py", want: "A = 1\n"},
	} {
		t.Run(c.name, func(t *testing.T) { runMutatorCase(t, c) })
	}
}

// A refusal asserts the bytes did not land, so the path it names is not this
// session's deliverable. Recording it would let any file the model was
// refused a write to look like something the session produced.
func TestARefusedWriteDoesNotEnterTheLedger(t *testing.T) {
	body := strings.Repeat("A = 1\n", 40)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "fixture.py"), []byte(body), 0o644)
	ctx := ledgerToolCtx(t, dir)

	args, _ := json.Marshal(map[string]string{"path": "fixture.py", "content": body})
	res := executeToolCall("write_file", args, ctx)
	if res.MutationStatus != MutationRefused {
		t.Fatalf("expected the echoed-write guard to refuse, got %q: %v",
			res.MutationStatus, res.Error)
	}
	if d := ledgerOf(t, ctx, "fixture.py"); d != nil {
		t.Errorf("a refusal created a ledger entry: %+v", d)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "fixture.py")); string(got) != body {
		t.Error("the refusal touched disk")
	}
}

// Which mutators can reach an explicit pass at all, and therefore which ones
// can ever leave a checkpoint behind. Phase 3B needs this answered per tool
// rather than assumed: a tool that structurally cannot produce a pass is
// restoration-ineligible no matter how the policy is written.
func TestWhichMutatorsCanEverPromoteACheckpoint(t *testing.T) {
	src := "def f():\n    return 1\n"
	sandbox := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
		case strings.HasSuffix(r.URL.Path, "/internal/structural_edit"):
			var in struct{ Source string }
			json.NewDecoder(r.Body).Decode(&in)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true,
				"language": "python", "new_content": "def f():\n    return 2\n",
				"old_size": len(in.Source), "new_size": 22})
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}
	for _, c := range []struct {
		tool, args string
		read       bool
	}{
		{"write_file", `{"path":"m.py","content":"def f():\n    return 9\n"}`, false},
		{"edit_file", `{"path":"m.py","old_str":"return 1","new_str":"return 2"}`, true},
		{"insert_after", `{"path":"m.py","line":2,"content":"G = 1"}`, true},
		{"replace_lines", `{"path":"m.py","start_line":2,"end_line":2,"expected_first_line":"    return 1","expected_last_line":"    return 1","content":"    return 2"}`, true},
		{"structural_edit", `{"path":"m.py","selector":"function:f","content":"def f():\n    return 2\n"}`, true},
		{"delete_file", `{"path":"m.py"}`, false},
		{"move_file", `{"source":"m.py","destination":"n.py"}`, false},
	} {
		t.Run(c.tool, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "m.py"), []byte(src), 0o644)
			srv := httptest.NewServer(http.HandlerFunc(sandbox))
			defer srv.Close()
			ctx := ledgerToolCtx(t, dir)
			ctx.SandboxURL = srv.URL
			ctx.V3URL = srv.URL
			if c.read {
				r, _ := json.Marshal(map[string]string{"path": "m.py"})
				executeToolCall("read_file", r, ctx)
			}
			res := executeToolCall(c.tool, json.RawMessage(c.args), ctx)
			promoted := false
			for _, d := range ctx.Ledger {
				if d.CheckpointHash != "" {
					promoted = true
				}
			}
			t.Logf("%-16s validation=%s/%-14s checkpoint=%v", c.tool,
				res.ValidationKind, res.ValidationStatus, promoted)
			// The invariant, whatever the answer: a checkpoint exists only
			// where the producer said passed for the bytes that landed.
			if promoted != (res.ValidationStatus == ValidationPassed) {
				t.Errorf("checkpoint=%v but validation=%q", promoted, res.ValidationStatus)
			}
		})
	}
}

// --- Phase 3A.2: the bytes the next model turn actually receives ------------
//
// The SSE transcript proved the OUTWARD stream was unchanged. It could not
// prove anything about the prompt, because the classification never reached
// SSE in the first place -- it reached the conversation. This captures the
// request bodies the proxy sends to the inference endpoint, which is
// literally what the next turn is conditioned on.
//
// The fixture drives two branches whose classification changed in the
// migration: edit_file with an absent old_str (unclassified -> none) and
// delete_file on a file that exists (unclassified -> applied). Before the
// projection those two turns carried mutation_status/validation_kind/
// validation_status into the prompt; after it they do not, and the bytes
// match the parent commit exactly.
//
// Verified against parent 20952c6 by running this fixture unchanged there:
// the same 7 requests, and the parent's bytes become these EXACTLY when the
// four classification keys are removed and nothing else is touched
// (13342 -> 12025 bytes, byte-identical).
//
// Re-pinned once since, at 7 requests / 12081 bytes: the fixture renames a
// file, and a rename did not count as a productive change, so the action gate
// bounced the model's `done` twice to tell it that nothing had been done while
// final.py sat on disk. Counting move_file as work removes those two turns and
// the run reaches the debt gate directly (5 requests / 6000 bytes). The
// classification keys this pin exists for are absent from both.
//
// Re-pinned at 5 requests / 6072 bytes: the edit_file refusal and the forced
// structural_edit steer no longer offer placeholder selectors; for this
// fixture's solve.py, which defines nothing a selector can name, they point to
// outline_file and replace_lines. Classification keys are still absent.
//
// Re-pinned at 5 requests / 6072 bytes: the system prompt grew from 15563 to
// 15740 bytes because it stopped calling a build, lint or syntax check
// verification, names `curl -sf` as the probe, and its run_command example
// runs the program instead of py_compile. Every other byte is unchanged.
//
// Re-pinned at 5 requests / 6072 bytes: the system prompt grew from 15740 to
// 15871 bytes because it now says a shell `rm` is for files the run created,
// and that deleting a file that was already here goes through delete_file.
// Every other byte is unchanged.
const modelPromptBytesHash = "47e39feb5a8be623e1e1bdaf420dc1727f705b563dbd7436dad126cee98fa3ad"

// conversationBytes keeps every message except the system prompt, whose tool
// descriptions are rendered in Go map order and therefore differ between two
// runs of identical code. That ordering is a pre-existing property of the
// prompt builder and has nothing to do with this boundary; the scan for
// classification keys below still covers the system prompt in full.
func conversationBytes(t *testing.T, prompts []string) string {
	t.Helper()
	var out strings.Builder
	for i, raw := range prompts {
		var req struct {
			Messages []AgentMessage `json:"messages"`
		}
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			t.Fatalf("request %d is not decodable: %v", i, err)
		}
		fmt.Fprintf(&out, "--- request %d ---\n", i)
		for _, m := range req.Messages {
			if m.Role == "system" {
				fmt.Fprintf(&out, "system <%d bytes elided>\n", len(m.Content))
				continue
			}
			fmt.Fprintf(&out, "%s|%s|%s|%s\n", m.Role, m.ToolName, m.ToolCallID, m.Content)
		}
	}
	return out.String()
}

func TestModelPromptBytesAreUnchangedByClassification(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "solve.py"), []byte("A = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	turn := 0
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
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
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		// The workspace path is baked into the prompt and differs per run.
		prompts = append(prompts, strings.ReplaceAll(string(body), dir, "<WORKSPACE>"))
		turn++
		n := turn
		mu.Unlock()

		var payload map[string]interface{}
		switch n {
		case 1:
			payload = map[string]interface{}{"type": "tool_call", "name": "read_file",
				"args": map[string]string{"path": "solve.py"}}
		case 2:
			// Classification changed here: none, was unknown.
			payload = map[string]interface{}{"type": "tool_call", "name": "edit_file",
				"args": map[string]string{"path": "solve.py",
					"old_str": "NOT PRESENT", "new_str": "B = 2"}}
		case 3:
			// And here: applied, was unknown. move_file rather than
			// delete_file because delete forces the loop to stop, so its
			// result would never condition another turn.
			payload = map[string]interface{}{"type": "tool_call", "name": "move_file",
				"args": map[string]string{"source": "solve.py", "destination": "final.py"}}
		default:
			payload = map[string]interface{}{"type": "done", "summary": "renamed solve.py"}
		}
		call, _ := json.Marshal(payload)
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 8
	ctx.StreamFn = func(string, interface{}) {}

	if err := runAgentLoop(ctx, "Rename solve.py."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	mu.Lock()
	joined := strings.Join(prompts, "\n\x1e\n")
	conversation := conversationBytes(t, prompts)
	mu.Unlock()
	if len(prompts) < 3 {
		t.Fatalf("only %d model requests captured; the fixture did not reach "+
			"the branches it is meant to exercise", len(prompts))
	}

	// The direct claim, independent of the pinned hash: no classification key
	// reached the prompt on any turn.
	for _, key := range []string{"mutation_status", "validation_kind",
		"validation_status", "validation_detail"} {
		if strings.Contains(joined, key) {
			t.Errorf("%q reached the model prompt", key)
		}
	}
	// The tool messages are still there, carrying their legacy shape.
	if !strings.Contains(joined, `string to replace not found in file`) {
		t.Error("the refused edit_file result never reached the prompt at all")
	}
	if !strings.Contains(joined, `{\"moved\":true`) {
		t.Error("the applied move_file result never reached the prompt")
	}

	if out := os.Getenv("ATLAS_PROMPT_BYTES"); out != "" {
		os.WriteFile(out, []byte(conversation), 0o600)
	}
	h := hashBytes([]byte(conversation))
	if modelPromptBytesHash != "" && h != modelPromptBytesHash {
		t.Errorf("conversation bytes changed (%s, want %s):\n%s",
			h[:12], modelPromptBytesHash[:12], conversation)
	}
	t.Logf("conversation: %d requests, %d bytes, sha256 %s",
		len(prompts), len(conversation), h)
}

// --- Phase 3B: restoration eligibility --------------------------------------
//
// Every clause is a reason NOT to act, so each case here is a way the ledger
// can be wrong or incomplete and must decline rather than guess. Restoration
// overwrites a file in the user's workspace; the bar is evidence about the
// exact bytes, not a plausible story about them.

// restoreCtx wires a sandbox whose syntax check fails on "]]" -- the same
// shape the terminal fixture uses -- so the fresh check the decision depends
// on is a real round trip.
func restoreCtx(t *testing.T, dir string, syntaxUp bool) *AgentContext {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !syntaxUp || !strings.HasSuffix(r.URL.Path, "/syntax-check") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		var in struct{ Code string }
		json.NewDecoder(r.Body).Decode(&in)
		valid := !strings.Contains(in.Code, "]]")
		out := map[string]interface{}{"valid": valid}
		if !valid {
			out["errors"] = []string{"SyntaxError: unmatched ']'"}
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	ctx := ledgerToolCtx(t, dir)
	ctx.SandboxURL = srv.URL
	return ctx
}

const (
	restoreOK     = "def solve():\n    return [1, 2]\n"
	restoreBroken = "def solve():\n    return [1, 2]]\n"
)

// seedCheckpoint writes good bytes through the real write path so the
// checkpoint comes from a production pass, then corrupts the file behind the
// tools' back, exactly as a shell command would.
func seedCheckpoint(t *testing.T, ctx *AgentContext, dir, rel string) {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"path": rel, "content": restoreOK})
	if res := executeToolCall("write_file", args, ctx); res.ValidationStatus != ValidationPassed {
		t.Fatalf("seed write did not pass: %s/%s (%s)",
			res.ValidationKind, res.ValidationStatus, res.Error)
	}
	if d := ledgerOf(t, ctx, rel); d == nil || d.CheckpointHash == "" {
		t.Fatal("seed write left no checkpoint")
	}
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(restoreBroken), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreReplacesDemonstratedBrokenBytesExactly(t *testing.T) {
	dir := t.TempDir()
	ctx := restoreCtx(t, dir, true)
	seedCheckpoint(t, ctx, dir, "solve.py")

	writesBefore := len(ctx.SessionWrites)
	dec := restoreDeliverable(ctx, ledgerKey(ctx, "solve.py"))
	if !dec.Restored {
		t.Fatalf("eligible restore declined: %+v", dec)
	}
	// Recovery is a system action: it does not count as the model having
	// written anything, so nothing that reads like progress may move.
	if len(ctx.SessionWrites) != writesBefore {
		t.Error("restoration registered itself as a session write")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	if string(got) != restoreOK {
		t.Fatalf("disk = %q, want the checkpoint", got)
	}
	d := ledgerOf(t, ctx, "solve.py")
	if d.CurrentHash != d.CheckpointHash || d.CurrentHash != hashBytes(got) {
		t.Error("the ledger does not describe the restored bytes exactly")
	}
	// The evidence carried over is the one already earned for those bytes.
	if k, s := d.CurrentValidation(); s != ValidationPassed || k != ValidationKindSyntax {
		t.Errorf("restored entry reports %v/%v", k, s)
	}
	if !d.Recovered {
		t.Error("the restore was not recorded as system recovery")
	}
	if dec.Path != "solve.py" {
		t.Errorf("disclosure path = %q, want the workspace-relative name", dec.Path)
	}
}

func TestRestoreDeclinesEveryIneligibleShape(t *testing.T) {
	for _, c := range []struct {
		name       string
		syntaxUp   bool
		mutate     func(t *testing.T, ctx *AgentContext, dir string)
		wantReason string
	}{
		{name: "current bytes parse, so nothing is broken", syntaxUp: true,
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				// Semantically wrong but syntactically fine: the checker has
				// no opinion to offer, so neither does recovery.
				os.WriteFile(filepath.Join(dir, "solve.py"),
					[]byte("def solve():\n    return [9, 9]\n"), 0o644)
			},
			// The fresh check passes, so those bytes become the checkpoint
			// and there is nothing safer to go back to.
			wantReason: "already holds the last version shown to be valid"},
		{name: "checker unavailable", syntaxUp: false,
			wantReason: "not shown to be broken"},
		{name: "incomparable validation kinds", syntaxUp: true,
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
				d.CheckpointKind = ValidationKindStructural
			},
			wantReason: "not checked the same way"},
		{name: "checkpoint hash is stale", syntaxUp: true,
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
				d.CheckpointHash = hashBytes([]byte("something else"))
			},
			wantReason: "could not be verified"},
		{name: "checkpoint bytes evicted", syntaxUp: true,
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
				d.CheckpointBytes = nil
			},
			wantReason: "no longer available"},
		{name: "path was deleted on purpose", syntaxUp: true,
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				tombstoneDeliverable(ctx, "solve.py", "deleted")
			},
			wantReason: "deleted or moved on purpose"},
		{name: "path was moved on purpose", syntaxUp: true,
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				tombstoneDeliverable(ctx, "solve.py", "moved:elsewhere.py")
			},
			wantReason: "deleted or moved on purpose"},
		{name: "a background job may still be writing", syntaxUp: true,
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				raiseWorkspaceHazard(ctx, "job1")
			},
			wantReason: "background job"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := restoreCtx(t, dir, true)
			seedCheckpoint(t, ctx, dir, "solve.py")
			if !c.syntaxUp {
				// Take the checker away AFTER the checkpoint was earned, so
				// the only thing missing is the fresh verdict.
				ctx.SandboxURL = "http://127.0.0.1:1"
			}
			if c.mutate != nil {
				c.mutate(t, ctx, dir)
			}
			before, _ := os.ReadFile(filepath.Join(dir, "solve.py"))

			dec := restoreDeliverable(ctx, ledgerKey(ctx, "solve.py"))
			if dec.Restored {
				t.Fatalf("restored on an ineligible shape: %+v", dec)
			}
			if !strings.Contains(dec.Reason, c.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", dec.Reason, c.wantReason)
			}
			after, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
			if string(after) != string(before) {
				t.Errorf("a declined restore still touched disk: %q", after)
			}
		})
	}
}

// A path spelled two ways is one file and gets one decision.
func TestRestoreDecidesOncePerCanonicalPath(t *testing.T) {
	dir := t.TempDir()
	ctx := restoreCtx(t, dir, true)
	seedCheckpoint(t, ctx, dir, "solve.py")
	// Same file, other spelling: the ledger already keys canonically, and the
	// walk must not produce two decisions for it.
	observeDeliverable(ctx, "./solve.py", []byte(restoreBroken),
		ValidationKindSyntax, ValidationFailed, "SyntaxError")

	decisions := restoreSaferDeliverables(ctx)
	if len(decisions) != 1 {
		t.Fatalf("got %d decisions for one file: %+v", len(decisions), decisions)
	}
	if !decisions[0].Restored {
		t.Errorf("the aliased path was not restored: %+v", decisions[0])
	}
}

// A restore that cannot land reports the real error and leaves what is there.
func TestRestoreFailurePreservesCurrentBytesAndTheError(t *testing.T) {
	dir := t.TempDir()
	ctx := restoreCtx(t, dir, true)
	sub := filepath.Join(dir, "pkg")
	os.MkdirAll(sub, 0o755)
	seedCheckpoint(t, ctx, dir, "pkg/solve.py")
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Skip("cannot make a directory read-only here")
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })

	dec := restoreDeliverable(ctx, ledgerKey(ctx, "pkg/solve.py"))
	if dec.Restored {
		t.Fatal("a restore that could not write reported success")
	}
	if !dec.Attempted {
		t.Fatal("the failure was not recorded as an attempt")
	}
	if !strings.Contains(dec.Reason, "cannot write") && !strings.Contains(dec.Reason, "permission") {
		t.Errorf("the real error was replaced by prose: %q", dec.Reason)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "pkg", "solve.py"))
	if string(got) != restoreBroken {
		t.Errorf("a failed restore changed the file: %q", got)
	}
	if d := ledgerOf(t, ctx, "pkg/solve.py"); d.Recovered {
		t.Error("a failed restore was recorded as recovery")
	}
}

// Multi-file recovery is per path. Two deliverables, one eligible and one
// not, must produce two independent decisions.
func TestRestoreIsPerPathAndNotATransaction(t *testing.T) {
	dir := t.TempDir()
	ctx := restoreCtx(t, dir, true)
	seedCheckpoint(t, ctx, dir, "a.py")
	seedCheckpoint(t, ctx, dir, "b.py")
	// b.py is just as broken, and just as much this session's work, but its
	// bytes are gone. One file recovers, the other does not, and the reader
	// is told which is which.
	ctx.Ledger[ledgerKey(ctx, "b.py")].CheckpointBytes = nil

	decisions := restoreSaferDeliverables(ctx)
	if len(decisions) != 2 {
		t.Fatalf("got %d decisions: %+v", len(decisions), decisions)
	}
	byPath := map[string]restoreDecision{}
	for _, d := range decisions {
		byPath[d.Path] = d
	}
	if !byPath["a.py"].Restored {
		t.Errorf("the eligible path was not restored: %+v", byPath["a.py"])
	}
	if byPath["b.py"].Restored {
		t.Errorf("a path with no held bytes was restored: %+v", byPath["b.py"])
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.py")); string(got) != restoreBroken {
		t.Errorf("b.py changed despite being ineligible: %q", got)
	}
	text := restorationDisclosure(decisions)
	if !strings.Contains(text, "a.py") || !strings.Contains(text, "b.py") {
		t.Errorf("the disclosure hides one of the two paths: %s", text)
	}
	if !strings.Contains(text, "each was decided on its own") {
		t.Errorf("the disclosure does not say recovery is per path: %s", text)
	}
	// No ledger vocabulary reaches the reader.
	for _, leak := range []string{"hash", "checkpoint", "generation", "ValidationPassed",
		"mutation_status", "validation_status"} {
		if strings.Contains(text, leak) {
			t.Errorf("the disclosure exposes ledger internals (%q): %s", leak, text)
		}
	}
}

// A deleted or moved path produces no decision at all: there is nothing on
// disk that is broken, and nothing to say about a file the model removed.
func TestTombstonedPathsAreSilentAndNeverResurrected(t *testing.T) {
	dir := t.TempDir()
	ctx := restoreCtx(t, dir, true)
	seedCheckpoint(t, ctx, dir, "gone.py")
	os.Remove(filepath.Join(dir, "gone.py"))
	tombstoneDeliverable(ctx, "gone.py", "deleted")

	for _, dec := range restoreSaferDeliverables(ctx) {
		if dec.Path == "gone.py" {
			t.Errorf("a deleted path produced a decision: %+v", dec)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.py")); err == nil {
		t.Error("a deleted file was resurrected")
	}
}

// --- A successful delete must not end the session -----------------------------
//
// delete_file returned an internal `__FORCE_DONE__` sentinel and the loop
// turned it into a terminal on the spot. The stated reason was to stop the
// model narrating after a destructive op, but the cost is that the delete
// swallows the rest of the task: asked to delete a.py AND write report.py, a
// run that happens to delete first ends before report.py is ever attempted,
// and the outcome depends on nothing but tool ordering.
//
// Removing the pre-empt does not make a deletion completable. The tombstone
// rule at the real terminal is unchanged and still fail-closed; what changes
// is that the run gets to finish the work it was asked for and be judged on
// all of it.

// delSymlinkTo marks a seed entry that should be created as a relative
// symlink to real.py rather than as a file.
const delSymlinkTo = "\x00symlink\x00"

const delSeed = "def solve():\n    return 7\n\n\nprint(solve())\n"

type delLoop struct {
	dir      string
	turns    int
	census   map[string]int
	terminal map[string]string
	seq      []string
	ctx      *AgentContext
}

// delLoopApprove selects whether the fixture answers deletion prompts. It is
// package-level rather than a parameter so the existing call sites keep their
// shape; delLoopFixtureApprove sets it for one run.
var delLoopApprove = true

func delLoopFixtureApprove(t *testing.T, seed map[string]string, prompt string,
	plan func(i int) map[string]interface{}, approve bool) *delLoop {
	t.Helper()
	prev := delLoopApprove
	delLoopApprove = approve
	t.Cleanup(func() { delLoopApprove = prev })
	return delLoopFixture(t, seed, prompt, plan)
}

func delLoopFixture(t *testing.T, seed map[string]string, prompt string,
	plan func(i int) map[string]interface{}) *delLoop {
	t.Helper()
	r := &delLoop{dir: t.TempDir(), census: map[string]int{}, terminal: map[string]string{}}
	for n, b := range seed {
		p := filepath.Join(r.dir, n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if b == delSymlinkTo {
			os.Symlink("real.py", p)
			continue
		}
		os.WriteFile(p, []byte(b), 0o644)
	}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasPrefix(req.URL.Path, "/v3/"), strings.HasPrefix(req.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(req.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(req.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(req.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(r.dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(req.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, req)
			return
		}
		io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		mu.Lock()
		i := r.turns
		r.turns++
		mu.Unlock()
		if i >= 20 {
			http.Error(w, "ceiling", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(srv.Close)

	r.ctx = NewAgentContext(r.dir, Tier2Medium)
	r.ctx.InferenceURL, r.ctx.SandboxURL, r.ctx.V3URL = srv.URL, srv.URL, srv.URL
	r.ctx.PermissionMode = PermissionYolo
	r.ctx.TrustMode = trustFullyTrusted
	r.ctx.VerifyOnHost = true
	r.ctx.MaxTurns = 0
	if delLoopApprove {
		autoApproveDeletes(t, r.ctx, "delloop-"+filepath.Base(r.dir))
	} else {
		autoAnswerDeletes(t, r.ctx, "delloop-deny-"+filepath.Base(r.dir), "deny")
	}
	r.ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		r.census[et]++
		switch et {
		case "tool_call":
			var tc struct{ Name string }
			json.Unmarshal(b, &tc)
			r.seq = append(r.seq, tc.Name)
		case "done":
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				r.terminal[k] = v
			}
		}
	}
	runAgentLoop(r.ctx, prompt)
	return r
}

func (r *delLoop) present() []string {
	var out []string
	filepath.Walk(r.dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(r.dir, p)
		if !strings.HasPrefix(rel, ".") {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (r *delLoop) tombstones() []string {
	var out []string
	for k, d := range r.ctx.Ledger {
		if d.Tombstoned {
			out = append(out, fmt.Sprintf("%s{%s,prohibited=%v}",
				filepath.Base(k), d.TombstoneReason, d.RestoreProhibited))
		}
	}
	sort.Strings(out)
	return out
}

func (r *delLoop) check(t *testing.T, name string) {
	t.Helper()
	t.Logf("%s: seq=%v disk=%v tombs=%v status=%q reason=%q calls=%d results=%d",
		name, r.seq, r.present(), r.tombstones(), r.terminal["status"], r.terminal["reason"],
		r.census["tool_call"], r.census["tool_result"])
	if r.census["done"] != 1 {
		t.Errorf("%d terminal events", r.census["done"])
	}
	if r.census["tool_call"] != r.census["tool_result"] {
		t.Errorf("call/result imbalance %d vs %d", r.census["tool_call"], r.census["tool_result"])
	}
}

func dlDel(p string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "delete_file",
		"args": map[string]string{"path": p}}
}
func dlWrite(p, c string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "write_file",
		"args": map[string]string{"path": p, "content": c}}
}

// The defect: the remaining requested write is never attempted.
func TestDeleteDoesNotSwallowTheRestOfTheTask(t *testing.T) {
	r := delLoopFixture(t, map[string]string{"a.py": delSeed},
		"Delete a.py and create report.py with the summary.",
		func(i int) map[string]interface{} {
			switch i {
			case 0:
				return dlDel("a.py")
			case 1:
				return dlWrite("report.py", delSeed)
			}
			return map[string]interface{}{"type": "done", "summary": "deleted a.py, wrote report.py"}
		})
	r.check(t, "delete-first")

	if len(r.seq) < 2 || r.seq[1] != "write_file" {
		t.Fatalf("the run ended at the delete: %v — the requested write was never attempted", r.seq)
	}
	got := r.present()
	if len(got) != 1 || got[0] != "report.py" {
		t.Errorf("disk is %v, want only report.py", got)
	}
	// Deletion semantics are untouched: confirmed absence, tombstoned,
	// restoration prohibited.
	tomb := r.tombstones()
	if len(tomb) != 1 || !strings.Contains(tomb[0], "a.py{deleted,prohibited=true}") {
		t.Errorf("tombstone state changed: %v", tomb)
	}
	// This fixture approves the deletion, so the run is judged on all of its
	// work: the removal the user authorised AND the file it was asked to
	// write. The subject here is that the write happened at all. Nothing ran
	// report.py, so the deliverable's reason is the parse-only one.
	if r.terminal["reason"] != "deliverables_parse_only" {
		t.Errorf("reason=%q, want the deliverable to own the terminal", r.terminal["reason"])
	}
}

// Every other delete shape: the loop continues, and the terminal is decided at
// the real terminal point by the rules that already exist.
func TestDeleteContinuationMatrix(t *testing.T) {
	for _, c := range []struct {
		name, prompt string
		seed         map[string]string
		plan         func(i int) map[string]interface{}
		wantSeq      int
		wantReason   string
	}{
		{"write then delete", "Create report.py with the summary.",
			map[string]string{"a.py": delSeed},
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlWrite("report.py", delSeed)
				case 1:
					return dlDel("a.py")
				}
				return map[string]interface{}{"type": "done", "summary": "wrote report.py"}
			}, 2, "deliverables_parse_only"},
		{"delete only", "Delete a.py.",
			map[string]string{"a.py": delSeed},
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlDel("a.py")
				}
				return map[string]interface{}{"type": "done", "summary": "deleted a.py"}
			}, 1, "approved_deletions_demonstrated"},
		{"delete then a read that only happens if the loop continues", "Delete a.py.",
			map[string]string{"a.py": delSeed, "b.py": delSeed},
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlDel("a.py")
				case 1:
					return map[string]interface{}{"type": "tool_call", "name": "read_file",
						"args": map[string]string{"path": "b.py"}}
				}
				return map[string]interface{}{"type": "done", "summary": "deleted a.py"}
			}, 2, "approved_deletions_demonstrated"},
		{"repeated delete of a now-absent path", "Delete a.py.",
			map[string]string{"a.py": delSeed},
			func(i int) map[string]interface{} {
				return dlDel("a.py")
			}, 2, ""},
		// The refusal is unchanged and so is the terminal: the structured
		// intent is recorded before the target is judged, so a refused
		// deletion still owes something and cannot report completed.
		{"non-empty directory is refused, unchanged", "Delete pkg.",
			map[string]string{"pkg/mod.py": delSeed},
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlDel("pkg")
				}
				return map[string]interface{}{"type": "done", "summary": "removed pkg"}
			}, 1, "unresolved_mutation_debt"},
		{"missing path fails, unchanged", "Delete gone.py.",
			map[string]string{},
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlDel("gone.py")
				}
				return map[string]interface{}{"type": "done", "summary": "not there"}
			}, 1, "no_file_obligation"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := delLoopFixture(t, c.seed, c.prompt, c.plan)
			r.check(t, c.name)
			if len(r.seq) < c.wantSeq {
				t.Errorf("%d tool calls, want at least %d — the loop stopped early", len(r.seq), c.wantSeq)
			}
			if c.wantReason != "" && r.terminal["reason"] != c.wantReason {
				t.Errorf("reason=%q, want %q", r.terminal["reason"], c.wantReason)
			}
			if r.turns >= 20 {
				t.Errorf("%d turns without a terminal", r.turns)
			}
		})
	}
}

// --- A demonstrated move is not a deletion ------------------------------------
//
// The tombstone rule refuses completion whenever anything was removed, because
// whether removing it was the task is not knowable. That is right for a plain
// delete. A move is a different fact: the source is gone AND the bytes are
// somewhere the ledger can point at, and TombstoneReason already records where
// (`moved:<canonical destination>`). Nothing about user intent has to be
// guessed to see that, so a move whose destination is demonstrated should not
// be treated as an unexplained removal.
//
// A plain deletion keeps today's refusal exactly.

func dlMove(src, dst string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "move_file",
		"args": map[string]string{"source": src, "destination": dst}}
}

func TestDemonstratedMoveDoesNotBlockCompletion(t *testing.T) {
	for _, c := range []struct {
		name, prompt string
		seed         map[string]string
		plan         func(i int) map[string]interface{}
		wantStatus   TerminalStatus
		wantReason   string
	}{
		{"1 validated move completes", "Rename old.py to new.py.",
			map[string]string{"old.py": delSeed},
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlMove("old.py", "new.py")
				}
				return map[string]interface{}{"type": "done", "summary": "renamed old.py to new.py"}
			}, TerminalCompleted, ""},
		{"6 several demonstrated moves complete", "Rename a.py to x.py and b.py to y.py.",
			map[string]string{"a.py": delSeed, "b.py": delSeed},
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlMove("a.py", "x.py")
				case 1:
					return dlMove("b.py", "y.py")
				}
				return map[string]interface{}{"type": "done", "summary": "renamed both"}
			}, TerminalCompleted, ""},
		// An unapproved deletion never reaches a tombstone -- nothing is
		// removed -- so it is stopped earlier, by the debt it still owes. The
		// property is that it cannot complete; which gate says so is not the
		// subject. delete_intent_unestablished now covers the narrower case of
		// a tombstone with no fulfilled approval behind it (see case 15).
		{"11 an unapproved delete still cannot complete", "Delete a.py.",
			map[string]string{"a.py": delSeed},
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlDel("a.py")
				}
				return map[string]interface{}{"type": "done", "summary": "deleted a.py"}
			}, TerminalIncomplete, ""},
		{"12 an unapproved unrequested delete still cannot complete", "Make the tests pass.",
			map[string]string{"a.py": delSeed},
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlDel("a.py")
				}
				return map[string]interface{}{"type": "done", "summary": "removed the failing file"}
			}, TerminalIncomplete, ""},
		{"a move and a delete together still cannot complete", "Rename a.py to x.py.",
			map[string]string{"a.py": delSeed, "b.py": delSeed},
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlMove("a.py", "x.py")
				case 1:
					return dlDel("b.py")
				}
				return map[string]interface{}{"type": "done", "summary": "renamed and cleaned up"}
			}, TerminalIncomplete, ""},
		{"13 destination deleted after the move", "Rename old.py to new.py.",
			map[string]string{"old.py": delSeed},
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlMove("old.py", "new.py")
				case 1:
					return dlDel("new.py")
				}
				return map[string]interface{}{"type": "done", "summary": "renamed"}
			}, TerminalIncomplete, ""},
		{"14 text exit after a demonstrated move", "Rename old.py to new.py.",
			map[string]string{"old.py": delSeed},
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlMove("old.py", "new.py")
				}
				return map[string]interface{}{"type": "text", "content": "Renamed old.py to new.py."}
			}, TerminalCompleted, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Deletions here are deliberately NOT approved: these cases exist
			// to prove an unapproved removal still blocks, which is the half
			// of the tombstone rule that did not change.
			approve := !strings.Contains(c.name, "delete")
			r := delLoopFixtureApprove(t, c.seed, c.prompt, c.plan, approve)
			r.check(t, c.name)
			t.Logf("   summary=%.140s", r.terminal["summary"])
			got := NormalizeTerminalStatus(r.terminal["status"])
			if got != c.wantStatus {
				t.Errorf("status=%q reason=%q, want %s",
					r.terminal["status"], r.terminal["reason"], c.wantStatus)
			}
			if c.wantReason != "" && r.terminal["reason"] != c.wantReason {
				t.Errorf("reason=%q, want %q", r.terminal["reason"], c.wantReason)
			}
			// Restoration stays prohibited on every tombstone, always.
			for k, d := range r.ctx.Ledger {
				if d.Tombstoned && !d.RestoreProhibited {
					t.Errorf("%s is tombstoned without RestoreProhibited", filepath.Base(k))
				}
			}
			// An incomplete MOVE must never claim nothing was written while
			// its destination is on disk. A denied deletion is different: the
			// seed file is still there precisely because nothing happened, so
			// saying nothing was written is the truth.
			movedSomething := strings.Contains(c.name, "move")
			if movedSomething && !got.Completed() && len(r.present()) > 0 &&
				claimsNothingWritten(r.terminal["summary"]) {
				t.Errorf("summary claims nothing was written while %v is on disk:\n%s",
					r.present(), r.terminal["summary"])
			}
		})
	}
}

// The predicate itself, on the cases a loop fixture cannot stage cleanly.
func TestMoveTombstoneDemonstrationPredicate(t *testing.T) {
	build := func(t *testing.T, reason string, mutate func(dir string, src, dst *DeliverableState)) (*AgentContext, *runState) {
		t.Helper()
		dir := t.TempDir()
		// The demonstration contract re-reads from disk and asks the checker,
		// so the predicate needs one to answer.
		syn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			var in struct{ Code string }
			json.NewDecoder(req.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "def solve(:")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"invalid syntax (line 1)"}
			}
			json.NewEncoder(w).Encode(out)
		}))
		t.Cleanup(syn.Close)
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.SandboxURL = syn.URL
		if ctx.Ledger == nil {
			ctx.Ledger = map[string]*DeliverableState{}
		}
		dstPath := filepath.Join(dir, "new.py")
		os.WriteFile(dstPath, []byte(delSeed), 0o644)
		h := hashBytes([]byte(delSeed))
		dst := &DeliverableState{Path: "new.py", CurrentHash: h, Generation: 1,
			ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
			ValidatedHash: h}
		src := &DeliverableState{Path: "old.py", Generation: 1, Tombstoned: true,
			RestoreProhibited: true,
			TombstoneReason:   strings.Replace(reason, "<dst>", ledgerKey(ctx, "new.py"), 1)}
		ctx.Ledger[ledgerKey(ctx, "old.py")] = src
		ctx.Ledger[ledgerKey(ctx, "new.py")] = dst
		if mutate != nil {
			mutate(dir, src, dst)
		}
		return ctx, &runState{}
	}
	for _, c := range []struct {
		name   string
		reason string
		mutate func(dir string, src, dst *DeliverableState)
		want   bool // true = still blocks completion
	}{
		{"demonstrated move", "moved:<dst>", nil, false},
		{"plain deletion", "deleted", nil, true},
		{"2 destination missing", "moved:<dst>", func(dir string, _, _ *DeliverableState) {
			os.Remove(filepath.Join(dir, "new.py"))
		}, true},
		{"3 destination stale in the ledger", "moved:<dst>", func(_ string, _, dst *DeliverableState) {
			dst.CurrentHash = hashBytes([]byte("other"))
		}, true},
		// move_file deliberately records the destination as unknown -- a pass
		// earned under the old name says nothing about this path -- so the
		// demonstration is read from disk, not inherited. Unknown plus a
		// matching hash plus bytes that pass IS demonstrated; what is not
		// demonstrated is covered by the stale-hash and bad-bytes cases.
		{"3b unknown ledger verdict, bytes still demonstrate", "moved:<dst>",
			func(_ string, _, dst *DeliverableState) {
				dst.ValidationStatus, dst.ValidatedHash = ValidationUnknown, ""
			}, false},
		{"4 source still exists", "moved:<dst>", func(dir string, _, _ *DeliverableState) {
			os.WriteFile(filepath.Join(dir, "old.py"), []byte(delSeed), 0o644)
		}, true},
		{"9 malformed move reason", "moved:", nil, true},
		{"9b move reason naming nothing on disk", "moved:/nowhere/gone.py", nil, true},
		{"destination bytes no longer parse", "moved:<dst>", func(dir string, _, dst *DeliverableState) {
			bad := []byte("def solve(:\n")
			os.WriteFile(filepath.Join(dir, "new.py"), bad, 0o644)
			dst.CurrentHash, dst.ValidatedHash = hashBytes(bad), hashBytes(bad)
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, st := build(t, c.reason, c.mutate)
			_ = st
			got := blockingTombstone(ctx)
			t.Logf("%s: blocks=%v", c.name, got)
			if got != c.want {
				t.Errorf("blocks=%v, want %v", got, c.want)
			}
		})
	}
}

// The rest of the move matrix: partial work, aliases, and a move that only
// half happened.
func TestDemonstratedMoveMatrixRemaining(t *testing.T) {
	t.Run("5 move plus an unmet named deliverable", func(t *testing.T) {
		r := delLoopFixture(t, map[string]string{"old.py": delSeed},
			"Rename old.py to new.py and create report.py with the summary.",
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlMove("old.py", "new.py")
				}
				return map[string]interface{}{"type": "done", "summary": "renamed old.py"}
			})
		r.check(t, "5 unmet deliverable")
		t.Logf("   summary=%.160s", r.terminal["summary"])
		if NormalizeTerminalStatus(r.terminal["status"]).Completed() {
			t.Errorf("completed with report.py never written: %q", r.terminal["reason"])
		}
		// The unmet obligation is what it names, not the move.
		if r.terminal["reason"] == "delete_intent_unestablished" {
			t.Error("the move is still being reported as an unexplained removal")
		}
		if claimsNothingWritten(r.terminal["summary"]) {
			t.Errorf("summary claims nothing was written while %v is on disk:\n%s",
				r.present(), r.terminal["summary"])
		}
	})

	t.Run("7 several moves, one not demonstrated", func(t *testing.T) {
		r := delLoopFixture(t, map[string]string{"a.py": delSeed, "b.py": delSeed},
			"Rename a.py to x.py and b.py to y.py.",
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlMove("a.py", "x.py")
				case 1:
					return dlMove("b.py", "y.py")
				case 2:
					// y.py is removed again, so that move is no longer shown.
					return dlDel("y.py")
				}
				return map[string]interface{}{"type": "done", "summary": "renamed both"}
			})
		r.check(t, "7 one incomplete")
		if NormalizeTerminalStatus(r.terminal["status"]).Completed() {
			t.Errorf("completed with one move undemonstrated: %q", r.terminal["reason"])
		}
	})

	t.Run("8 alias spellings resolve to one pair", func(t *testing.T) {
		r := delLoopFixture(t, map[string]string{"old.py": delSeed},
			"Rename old.py to new.py.",
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlMove("./old.py", "./new.py")
				}
				return map[string]interface{}{"type": "done", "summary": "renamed"}
			})
		r.check(t, "8 aliases")
		if r.terminal["status"] != string(TerminalCompleted) {
			t.Errorf("alias spellings did not resolve: %q/%q",
				r.terminal["status"], r.terminal["reason"])
		}
		if len(r.tombstones()) != 1 {
			t.Errorf("aliases produced %d tombstones: %v", len(r.tombstones()), r.tombstones())
		}
	})

	t.Run("10 source gone but destination never landed", func(t *testing.T) {
		// The destination is removed straight after, which is the observable
		// end state of a copy that did not survive.
		r := delLoopFixture(t, map[string]string{"old.py": delSeed},
			"Rename old.py to new.py.",
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlMove("old.py", "new.py")
				case 1:
					return dlDel("new.py")
				}
				return map[string]interface{}{"type": "done", "summary": "renamed"}
			})
		r.check(t, "10 destination gone")
		if NormalizeTerminalStatus(r.terminal["status"]).Completed() {
			t.Errorf("completed with neither source nor destination present: %q",
				r.terminal["reason"])
		}
		if len(r.present()) != 0 {
			t.Errorf("disk should be empty, got %v", r.present())
		}
	})
}

// claimsNothingWritten matches every phrasing the terminal uses to say the run
// left disk untouched. A summary that says this while a deliverable is on disk
// is false whichever wording it reaches for.
func claimsNothingWritten(summary string) bool {
	for _, phrase := range []string{
		"Nothing was written to disk",
		"Nothing was written —",
		"no file was created or changed",
	} {
		if strings.Contains(summary, phrase) {
			return true
		}
	}
	return false
}

// autoApproveDeletes answers every delete_file permission request with a
// one-time allow, so a fixture testing the LOOP is not also testing the
// handshake. Deleting now always asks -- yolo included -- and these fixtures
// exist to measure continuation and completion, not confirmation.
func autoApproveDeletes(t *testing.T, ctx *AgentContext, sessionID string) {
	t.Helper()
	autoAnswerDeletes(t, ctx, sessionID, "allow")
}

// autoAnswerDeletes answers every pending prompt for this session with the
// given decision, so a fixture never waits out the real fail-safe.
func autoAnswerDeletes(t *testing.T, ctx *AgentContext, sessionID, decision string) {
	t.Helper()
	ctx.PassID = sessionID
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			pendingPermissions.Range(func(k, _ any) bool {
				key, _ := k.(string)
				if !strings.HasPrefix(key, sessionID+"|") {
					return true
				}
				callID := strings.TrimPrefix(key, sessionID+"|")
				body := fmt.Sprintf(
					`{"session_id":%q,"tool_call_id":%q,"decision":%q,"scope":"once"}`,
					sessionID, callID, decision)
				r := httptest.NewRequest(http.MethodPost, "/v1/permission",
					strings.NewReader(body))
				handlePermission(httptest.NewRecorder(), r)
				return true
			})
		}
	}()
}

// A refused deletion owes something, and nothing about the refusal retires it.
//
// The structured intent is recorded once the path parses, canonicalises and
// clears the workspace boundary -- before the target is judged and before the
// user is asked -- so a preflight refusal, a denial, a timeout and a cancel
// all leave the debt standing. Recording the model's tool intent is not user
// authorisation, and no word in the prompt is consulted anywhere here.
func TestRefusedDeletionCannotAuthoriseCompletion(t *testing.T) {
	for _, c := range []struct {
		name       string
		seed       map[string]string
		plan       func(i int) map[string]interface{}
		approve    bool
		wantStatus TerminalStatus
		wantReason string
		wantOnDisk []string
	}{
		{"denied deletion then done", map[string]string{"a.py": delSeed},
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlDel("a.py")
				}
				return map[string]interface{}{"type": "done", "summary": "removed a.py"}
			}, false, TerminalIncomplete, "unresolved_mutation_debt", []string{"a.py"}},

		{"denied deletion then unrelated success", map[string]string{"a.py": delSeed},
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlDel("a.py")
				case 1:
					return dlWrite("report.py", delSeed)
				}
				return map[string]interface{}{"type": "done", "summary": "wrote report.py"}
			}, false, TerminalIncomplete, "unresolved_mutation_debt",
			[]string{"a.py", "report.py"}},

		// A rewrite is not a retirement of a delete debt: that debt retires on
		// the path being confirmed ABSENT, and rewriting it leaves it present.
		// The existing causal rule converts content-then-delete, not the other
		// way round, so a denied deletion followed by a same-path write still
		// owes the deletion and the run stays incomplete. Fail-closed, and the
		// alternative would let any later write launder a denied removal.
		{"denied deletion then a same-path rewrite still owes the deletion",
			map[string]string{"a.py": delSeed},
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlDel("a.py")
				case 1:
					return dlWrite("a.py", "def solve():\n    return 8\n\n\nprint(solve())\n")
				}
				return map[string]interface{}{"type": "done", "summary": "rewrote a.py"}
			}, false, TerminalIncomplete, "unresolved_mutation_debt", []string{"a.py"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := delLoopFixtureApprove(t, c.seed, "Tidy the workspace.", c.plan, c.approve)
			r.check(t, c.name)
			got := NormalizeTerminalStatus(r.terminal["status"])
			if got != c.wantStatus {
				t.Errorf("status=%q reason=%q, want %s",
					r.terminal["status"], r.terminal["reason"], c.wantStatus)
			}
			if c.wantReason != "" && r.terminal["reason"] != c.wantReason {
				t.Errorf("reason=%q, want %q", r.terminal["reason"], c.wantReason)
			}
			for _, want := range c.wantOnDisk {
				if _, err := os.Stat(filepath.Join(r.dir, want)); err != nil {
					t.Errorf("%s is missing: %v", want, err)
				}
			}
		})
	}
}

// Timeout and cancel are refusals too, and leave the same debt standing.
func TestUnansweredDeletionCannotAuthoriseCompletion(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "timeout" {
				t.Setenv("ATLAS_PERMISSION_TIMEOUT_SEC", "1")
			}
			r := delLoopFixtureApprove(t, map[string]string{"a.py": delSeed},
				"Tidy the workspace.",
				func(i int) map[string]interface{} {
					if i == 0 {
						return dlDel("a.py")
					}
					return map[string]interface{}{"type": "done", "summary": "removed a.py"}
				}, false)
			r.check(t, mode)
			if NormalizeTerminalStatus(r.terminal["status"]).Completed() {
				t.Errorf("%s reported %q/%q", mode, r.terminal["status"], r.terminal["reason"])
			}
			if _, err := os.Stat(filepath.Join(r.dir, "a.py")); err != nil {
				t.Errorf("the file was removed without an answer: %v", err)
			}
		})
	}
}

// --- Completion from fulfilled, user-approved deletions ----------------------
//
// A deletion the user approved and the system carried out is work, and a run
// whose work was exactly that has finished it. Everything else about the
// tombstone gate stays as it was: an unapproved removal still blocks, and a
// move is still owned by its own predicate.

func TestApprovedDeletionCompletionMatrix(t *testing.T) {
	valid := delSeed
	for _, c := range []struct {
		name       string
		seed       map[string]string
		plan       func(i int) map[string]interface{}
		approve    bool
		wantStatus TerminalStatus
		wantReason string
		wantGone   []string
		wantThere  []string
	}{
		{"1 one approved deletion", map[string]string{"a.py": valid},
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlDel("a.py")
				}
				return map[string]interface{}{"type": "done", "summary": "removed a.py"}
			}, true, TerminalCompleted, "approved_deletions_demonstrated",
			[]string{"a.py"}, nil},

		{"2 two approved deletions", map[string]string{"a.py": valid, "b.py": valid},
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlDel("a.py")
				case 1:
					return dlDel("b.py")
				}
				return map[string]interface{}{"type": "done", "summary": "removed both"}
			}, true, TerminalCompleted, "approved_deletions_demonstrated",
			[]string{"a.py", "b.py"}, nil},

		{"3 one approved deletion fails", map[string]string{"a.py": valid, "pkg/m.py": valid},
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlDel("a.py")
				case 1:
					return dlDel("pkg") // non-empty: refused before asking
				}
				return map[string]interface{}{"type": "done", "summary": "tidied up"}
			}, true, TerminalIncomplete, "unresolved_mutation_debt",
			[]string{"a.py"}, []string{"pkg/m.py"}},

		{"4 approved delete plus a demonstrated deliverable",
			map[string]string{"a.py": valid},
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlDel("a.py")
				case 1:
					return dlWrite("report.py", valid)
				}
				return map[string]interface{}{"type": "done", "summary": "done"}
			}, true, TerminalCompleted, "deliverables_parse_only",
			[]string{"a.py"}, []string{"report.py"}},

		{"9 denied deletion", map[string]string{"a.py": valid},
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlDel("a.py")
				}
				return map[string]interface{}{"type": "done", "summary": "removed a.py"}
			}, false, TerminalIncomplete, "unresolved_mutation_debt",
			nil, []string{"a.py"}},

		{"15 approved deletion then the path is recreated",
			map[string]string{"a.py": valid},
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlDel("a.py")
				case 1:
					return dlWrite("a.py", valid)
				}
				return map[string]interface{}{"type": "done", "summary": "removed then rewrote a.py"}
			}, true, TerminalIncomplete, "delete_intent_unestablished",
			nil, []string{"a.py"}},

		{"18 approved symlink deletion leaves the target",
			map[string]string{"real.py": valid, "link.py": delSymlinkTo},
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlDel("link.py")
				}
				return map[string]interface{}{"type": "done", "summary": "removed the link"}
			}, true, TerminalCompleted, "approved_deletions_demonstrated",
			[]string{"link.py"}, []string{"real.py"}},

		{"22 alias spelling is one obligation", map[string]string{"a.py": valid},
			func(i int) map[string]interface{} {
				if i == 0 {
					return dlDel("./a.py")
				}
				return map[string]interface{}{"type": "done", "summary": "removed a.py"}
			}, true, TerminalCompleted, "approved_deletions_demonstrated",
			[]string{"a.py"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := delLoopFixtureApprove(t, c.seed, "Tidy the workspace.", c.plan, c.approve)
			r.check(t, c.name)
			t.Logf("   summary=%.150s", r.terminal["summary"])
			if got := NormalizeTerminalStatus(r.terminal["status"]); got != c.wantStatus {
				t.Errorf("status=%q reason=%q, want %s",
					r.terminal["status"], r.terminal["reason"], c.wantStatus)
			}
			if c.wantReason != "" && r.terminal["reason"] != c.wantReason {
				t.Errorf("reason=%q, want %q", r.terminal["reason"], c.wantReason)
			}
			for _, p := range c.wantGone {
				if _, err := os.Lstat(filepath.Join(r.dir, p)); !os.IsNotExist(err) {
					t.Errorf("%s is still there", p)
				}
			}
			for _, p := range c.wantThere {
				if _, err := os.Lstat(filepath.Join(r.dir, p)); err != nil {
					t.Errorf("%s is missing: %v", p, err)
				}
			}
			// A completed deletion run names what it removed, and never says
			// nothing was written.
			if c.wantReason == "approved_deletions_demonstrated" {
				for _, p := range c.wantGone {
					if !strings.Contains(r.terminal["summary"], p) {
						t.Errorf("the summary does not name %s: %q", p, r.terminal["summary"])
					}
				}
				if claimsNothingWritten(r.terminal["summary"]) {
					t.Errorf("the summary claims nothing was written: %q", r.terminal["summary"])
				}
				for _, leak := range []string{"sha256", "tombstone", "RestoreProhibited",
					"generation", "inode", "permission"} {
					if strings.Contains(strings.ToLower(r.terminal["summary"]),
						strings.ToLower(leak)) {
						t.Errorf("the summary leaks %q: %q", leak, r.terminal["summary"])
					}
				}
			}
		})
	}
}

// Recreation, and re-deletion, across generations.
func TestApprovedDeletionAcrossGenerations(t *testing.T) {
	valid := delSeed
	for _, c := range []struct {
		name       string
		plan       func(i int) map[string]interface{}
		wantStatus TerminalStatus
		wantReason string
	}{
		{"16 deleted, recreated, deleted again with fresh approval",
			func(i int) map[string]interface{} {
				switch i {
				case 0:
					return dlDel("a.py")
				case 1:
					return dlWrite("a.py", valid)
				case 2:
					return dlDel("a.py")
				}
				return map[string]interface{}{"type": "done", "summary": "removed a.py"}
			}, TerminalCompleted, "approved_deletions_demonstrated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := delLoopFixtureApprove(t, map[string]string{"a.py": valid},
				"Tidy the workspace.", c.plan, true)
			r.check(t, c.name)
			t.Logf("   summary=%.140s", r.terminal["summary"])
			if got := NormalizeTerminalStatus(r.terminal["status"]); got != c.wantStatus {
				t.Errorf("status=%q reason=%q, want %s",
					r.terminal["status"], r.terminal["reason"], c.wantStatus)
			}
			if r.terminal["reason"] != c.wantReason {
				t.Errorf("reason=%q, want %q", r.terminal["reason"], c.wantReason)
			}
			if _, err := os.Lstat(filepath.Join(r.dir, "a.py")); !os.IsNotExist(err) {
				t.Error("a.py is still there")
			}
		})
	}
}
