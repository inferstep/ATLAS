package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itigges22/atlas-proxy/internal/taskcontract"
	"github.com/itigges22/atlas-proxy/internal/wire"
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
	if res := executeToolCall("write_file", args, ctx); res.ValidationStatus != wire.ValidationPassed {
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
	if k, s := d.CurrentValidation(); s != wire.ValidationPassed || k != wire.ValidationKindSyntax {
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
		mutate     func(t *testing.T, ctx *AgentContext, dir string)
		wantReason string
	}{
		{name: "current bytes parse, so nothing is broken",
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				// Semantically wrong but syntactically fine: the checker has
				// no opinion to offer, so neither does recovery.
				os.WriteFile(filepath.Join(dir, "solve.py"),
					[]byte("def solve():\n    return [9, 9]\n"), 0o644)
			},
			// The fresh check passes, so those bytes become the checkpoint
			// and there is nothing safer to go back to.
			wantReason: "already holds the last version shown to be valid"},
		{name: "checker unavailable",
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				// Take the checker away AFTER the checkpoint was earned, so
				// the only thing missing is the fresh verdict.
				ctx.SandboxURL = "http://127.0.0.1:1"
			},
			wantReason: "not shown to be broken"},
		{name: "incomparable validation kinds",
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
				d.CheckpointKind = wire.ValidationKindStructural
			},
			wantReason: "not checked the same way"},
		{name: "checkpoint hash is stale",
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
				d.CheckpointHash = hashBytes([]byte("something else"))
			},
			wantReason: "could not be verified"},
		{name: "checkpoint bytes evicted",
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
				d.CheckpointBytes = nil
			},
			wantReason: "no longer available"},
		{name: "path was deleted on purpose",
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				tombstoneDeliverable(ctx, "solve.py", "deleted")
			},
			wantReason: "deleted or moved on purpose"},
		{name: "path was moved on purpose",
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				tombstoneDeliverable(ctx, "solve.py", "moved:elsewhere.py")
			},
			wantReason: "deleted or moved on purpose"},
		{name: "a background job may still be writing",
			mutate: func(t *testing.T, ctx *AgentContext, dir string) {
				raiseWorkspaceHazard(ctx, "job1")
			},
			wantReason: "background job"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := restoreCtx(t, dir, true)
			seedCheckpoint(t, ctx, dir, "solve.py")
			c.mutate(t, ctx, dir)
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
		wire.ValidationKindSyntax, wire.ValidationFailed, "SyntaxError")

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

// checkpointRestorable says yes only for a whole, verified earlier version and
// a current version shown broken by the same kind of check. Each row changes
// one thing from that state and names the refusal it gets.
func TestCheckpointRestorableNamesEachReasonNotToRestore(t *testing.T) {
	// A checkpoint exactly at the size limit is still kept; one byte more is not.
	good := bytes.Repeat([]byte("#"), maxCheckpointFileBytes)
	large := make([]byte, maxCheckpointFileBytes+1)
	restorable := func() *taskcontract.DeliverableState {
		return &taskcontract.DeliverableState{
			Generation: 1, CheckpointHash: hashBytes(good), CheckpointBytes: good,
			CheckpointKind: wire.ValidationKindSyntax, ValidationKind: wire.ValidationKindSyntax,
			ValidationStatus: wire.ValidationFailed, ValidatedHash: "current",
		}
	}
	if ok, why := checkpointRestorable(restorable(), "current", false); !ok {
		t.Fatalf("the restorable state was refused: %s", why)
	}
	if ok, _ := checkpointRestorable(nil, "current", false); ok {
		t.Error("no deliverable at all was restorable")
	}
	for _, tc := range []struct {
		name      string
		change    func(d *taskcontract.DeliverableState)
		current   string
		hazardous bool
		reason    string
	}{
		{"never written", func(d *taskcontract.DeliverableState) { d.Generation = 0 }, "current", false,
			"not a deliverable this session wrote"},
		{"deleted on purpose", func(d *taskcontract.DeliverableState) { d.Tombstoned = true }, "current", false,
			"deleted or moved on purpose"},
		{"restoration prohibited", func(d *taskcontract.DeliverableState) { d.RestoreProhibited = true }, "current", false,
			"restoration prohibited for this path"},
		{"no valid version", func(d *taskcontract.DeliverableState) { d.CheckpointHash = "" }, "current", false,
			"no version of it was ever shown to be valid"},
		{"no valid version kept, and why", func(d *taskcontract.DeliverableState) {
			d.CheckpointHash, d.CheckpointUnavailable = "", "over the size limit"
		}, "current", false, "no earlier valid version was kept (over the size limit)"},
		{"no bytes", func(d *taskcontract.DeliverableState) { d.CheckpointBytes = nil }, "current", false,
			"the earlier valid version is no longer available"},
		{"too large", func(d *taskcontract.DeliverableState) {
			d.CheckpointBytes, d.CheckpointHash = large, hashBytes(large)
		}, "current", false, "the earlier valid version is too large to hold"},
		{"bytes and hash disagree", func(d *taskcontract.DeliverableState) { d.CheckpointHash = "another" }, "current", false,
			"the earlier valid version could not be verified"},
		{"already the valid version", func(d *taskcontract.DeliverableState) {}, hashBytes(good), false,
			"the file already holds the last version shown to be valid"},
		{"current contents never checked", func(d *taskcontract.DeliverableState) { d.ValidatedHash = "older" }, "current", false,
			"the current contents were never checked"},
		{"current contents not shown broken", func(d *taskcontract.DeliverableState) { d.ValidationStatus = wire.ValidationPassed },
			"current", false, "the current contents were not shown to be broken"},
		{"both checked by an unknown kind", func(d *taskcontract.DeliverableState) {
			d.CheckpointKind, d.ValidationKind = wire.ValidationKindUnknown, wire.ValidationKindUnknown
		}, "current", false, "the two versions were not checked the same way"},
		{"checked by different kinds", func(d *taskcontract.DeliverableState) { d.CheckpointKind = wire.ValidationKindNone },
			"current", false, "the two versions were not checked the same way"},
		{"a background job may write", func(d *taskcontract.DeliverableState) {}, "current", true,
			"a background job may still be writing"},
	} {
		d := restorable()
		tc.change(d)
		ok, why := checkpointRestorable(d, tc.current, tc.hazardous)
		if ok {
			t.Errorf("%s: restorable", tc.name)
		}
		if why != tc.reason {
			t.Errorf("%s: refused as %q, want %q", tc.name, why, tc.reason)
		}
	}
}
