package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Restoration overwrites a file in the user's workspace, so the bar is
// evidence about the exact bytes, not a plausible story about them. Every
// clause is a reason not to act: each case below is a way the ledger can be
// wrong or incomplete, and restoration must decline rather than guess.

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
