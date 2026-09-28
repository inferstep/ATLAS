package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Outcome table for the whole-file sandbox check. The flat (string, bool) API
// returns ("", true) for TEN distinct conditions of which only one is a pass,
// so `true` means "do not refuse", never "validated". These pin the
// structured core that tells them apart.
func TestSandboxSyntaxOutcomeTable(t *testing.T) {
	sandbox := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(body))
		}))
	}
	valid := sandbox(200, `{"valid":true}`)
	defer valid.Close()
	invalid := sandbox(200, `{"valid":false,"errors":["SyntaxError: bad"]}`)
	defer invalid.Close()
	broken := sandbox(200, `not json`)
	defer broken.Close()
	five00 := sandbox(500, `{}`)
	defer five00.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // now unreachable

	cases := []struct {
		name, path, url string
		nilCtx          bool
		want            ValidationStatus
	}{
		// Applicability is decided BEFORE availability: an unsupported
		// extension is not_applicable even with no sandbox configured.
		{"unsupported extension, no sandbox", "notes.txt", "", false, ValidationNotApplicable},
		{"unsupported extension, sandbox up", "notes.txt", valid.URL, false, ValidationNotApplicable},
		{"jinja is not whole-file gated", "page.jinja", valid.URL, false, ValidationNotApplicable},
		{"applicable, no sandbox", "mod.py", "", false, ValidationNotRun},
		{"applicable, nil context", "mod.py", valid.URL, true, ValidationNotRun},
		{"applicable, unreachable", "mod.py", deadURL, false, ValidationNotRun},
		{"applicable, non-200", "mod.py", five00.URL, false, ValidationNotRun},
		{"applicable, undecodable", "mod.py", broken.URL, false, ValidationNotRun},
		{"demonstrated pass", "mod.py", valid.URL, false, ValidationPassed},
		{"demonstrated failure", "mod.py", invalid.URL, false, ValidationFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var ctx *AgentContext
			if !c.nilCtx {
				ctx = NewAgentContext(t.TempDir(), Tier2Medium)
				ctx.SandboxURL = c.url
			}
			got := sandboxSyntaxOutcome(ctx, c.path, "A = 1\n")
			if got.Status != c.want {
				t.Errorf("Status = %q, want %q (detail %q)", got.Status, c.want, got.Detail)
			}
			// Wrapper parity: only a demonstrated failure may refuse.
			_, ok := checkSandboxSyntax(ctx, c.path, "A = 1\n")
			if wantOK := c.want != ValidationFailed; ok != wantOK {
				t.Errorf("wrapper ok = %v, want %v -- fail-open behavior changed", ok, wantOK)
			}
			if got.Status == ValidationPassed && !got.attempted() {
				t.Error("a pass must count as attempted")
			}
			if got.Status == ValidationNotApplicable && got.applicable() {
				t.Error("not_applicable must not count as applicable")
			}
		})
	}
}

func TestFallbackSyntaxAggregate(t *testing.T) {
	pass := checkOutcome{Status: ValidationPassed}
	fail := checkOutcome{Status: ValidationFailed, Detail: "boom"}
	notRun := checkOutcome{Status: ValidationNotRun, Detail: "unreachable"}
	na := checkOutcome{Status: ValidationNotApplicable}
	unk := checkOutcome{Status: ValidationUnknown}

	for _, c := range []struct {
		name string
		in   fallbackSyntaxOutcome
		want ValidationStatus
	}{
		{"failure is decisive over pass", fallbackSyntaxOutcome{pass, fail}, ValidationFailed},
		{"failure is decisive over not_run", fallbackSyntaxOutcome{notRun, fail}, ValidationFailed},
		{"applicable unavailable wins over pass", fallbackSyntaxOutcome{pass, notRun}, ValidationNotRun},
		{"pass plus not_applicable", fallbackSyntaxOutcome{pass, na}, ValidationPassed},
		{"not_applicable plus pass", fallbackSyntaxOutcome{na, pass}, ValidationPassed},
		{"all not_applicable", fallbackSyntaxOutcome{na, na}, ValidationNotApplicable},
		{"unknown surfaces as unknown, not not_run", fallbackSyntaxOutcome{pass, unk}, ValidationUnknown},
		{"unknown alone surfaces as unknown", fallbackSyntaxOutcome{unk, na}, ValidationUnknown},
		{"failure still decisive over unknown", fallbackSyntaxOutcome{unk, fail}, ValidationFailed},
		{"unknown outranks not_run", fallbackSyntaxOutcome{notRun, unk}, ValidationUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.aggregate(); got.Status != c.want {
				t.Errorf("aggregate = %q, want %q", got.Status, c.want)
			}
		})
	}
}

// Composites through the production producer, with request counting so the
// short circuit is proven rather than assumed.
func TestFallbackSyntaxComposites(t *testing.T) {
	const jinja = "page.jinja" // NOT in syntaxGateLanguages; IS embedded-capable
	withScript := "<html><script>let a = 1;</script></html>"

	newSvc := func(embCalls *int, status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/internal/embedded_script_check") {
				*embCalls++
				w.WriteHeader(status)
				w.Write([]byte(body))
				return
			}
			// whole-file sandbox check
			w.WriteHeader(200)
			w.Write([]byte(`{"valid":false,"errors":["SyntaxError: whole-file bad"]}`))
		}))
	}

	t.Run("jinja embedded passes -> aggregate passed", func(t *testing.T) {
		calls := 0
		svc := newSvc(&calls, 200, `{"ok":true,"findings":[]}`)
		defer svc.Close()
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		ctx.SandboxURL, ctx.V3URL = svc.URL, svc.URL
		o := fallbackSyntaxOutcomeFor(ctx, jinja, withScript)
		if o.WholeFile.Status != ValidationNotApplicable {
			t.Errorf("WholeFile = %q, want not_applicable", o.WholeFile.Status)
		}
		if o.Embedded.Status != ValidationPassed {
			t.Errorf("Embedded = %q, want passed", o.Embedded.Status)
		}
		if got := o.aggregate().Status; got != ValidationPassed {
			t.Errorf("aggregate = %q, want passed", got)
		}
		if calls != 1 {
			t.Errorf("embedded calls = %d, want 1", calls)
		}
	})

	t.Run("jinja embedded unavailable -> aggregate not_run", func(t *testing.T) {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		ctx.SandboxURL, ctx.V3URL = "", "" // service absent
		o := fallbackSyntaxOutcomeFor(ctx, jinja, withScript)
		if o.WholeFile.Status != ValidationNotApplicable {
			t.Errorf("WholeFile = %q, want not_applicable", o.WholeFile.Status)
		}
		if o.Embedded.Status != ValidationNotRun {
			t.Errorf("Embedded = %q, want not_run", o.Embedded.Status)
		}
		if got := o.aggregate().Status; got != ValidationNotRun {
			t.Errorf("aggregate = %q, want not_run", got)
		}
	})

	t.Run("jinja embedded finding -> aggregate failed", func(t *testing.T) {
		calls := 0
		svc := newSvc(&calls, 200,
			`{"ok":true,"findings":[{"line":1,"message":"stray paren","snippet":"x)"}]}`)
		defer svc.Close()
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		ctx.SandboxURL, ctx.V3URL = svc.URL, svc.URL
		agg := fallbackSyntaxOutcomeFor(ctx, jinja, withScript).aggregate()
		if agg.Status != ValidationFailed {
			t.Fatalf("aggregate = %q, want failed", agg.Status)
		}
		if !strings.HasPrefix(agg.Detail, embeddedScriptErrPrefix) {
			t.Errorf("diagnostic lost its prefix: %q", agg.Detail)
		}
		// Wrapper parity: only a demonstrated failure refuses.
		msg, ok := checkFallbackSyntax(ctx, jinja, withScript)
		if ok || msg != agg.Detail {
			t.Errorf("wrapper ok=%v msg=%q, want false and the same diagnostic", ok, msg)
		}
	})

	t.Run("whole-file failure short-circuits the embedded call", func(t *testing.T) {
		calls := 0
		svc := newSvc(&calls, 200, `{"ok":true,"findings":[]}`)
		defer svc.Close()
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		ctx.SandboxURL, ctx.V3URL = svc.URL, svc.URL
		// .html IS whole-file gated, and the stub fails it.
		o := fallbackSyntaxOutcomeFor(ctx, "page.html", withScript)
		if o.WholeFile.Status != ValidationFailed {
			t.Fatalf("WholeFile = %q, want failed", o.WholeFile.Status)
		}
		if calls != 0 {
			t.Errorf("embedded calls = %d, want 0 -- the short circuit was lost", calls)
		}
		if o.Embedded.Status != ValidationNotRun {
			t.Errorf("Embedded = %q, want not_run (skipped but applicable)", o.Embedded.Status)
		}
		if o.aggregate().Status != ValidationFailed {
			t.Error("aggregate must remain the whole-file failure")
		}
	})

	t.Run("no embedded content -> not_applicable, zero calls", func(t *testing.T) {
		calls := 0
		svc := newSvc(&calls, 200, `{"ok":true,"findings":[]}`)
		defer svc.Close()
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		ctx.SandboxURL, ctx.V3URL = "", svc.URL
		o := fallbackSyntaxOutcomeFor(ctx, jinja, "<html><p>no script here</p></html>")
		if o.Embedded.Status != ValidationNotApplicable {
			t.Errorf("Embedded = %q, want not_applicable", o.Embedded.Status)
		}
		if calls != 0 {
			t.Errorf("embedded calls = %d, want 0", calls)
		}
		if o.aggregate().Status != ValidationNotApplicable {
			t.Errorf("aggregate = %q, want not_applicable", o.aggregate().Status)
		}
	})
}

// --- Phase 3A: observational deliverable ledger -----------------------------
//
// At a stall the server knows only which paths were touched — not whether the
// current bytes are valid, and not what the last good version was.
// ValidationStatus never crosses the event boundary, so a finaliser had
// nothing true to say and nothing safe to leave behind. This records both.
// Phase 3A observes only: no restoration, no behaviour change.

func ledgerCtx(t *testing.T) *AgentContext {
	t.Helper()
	return NewAgentContext(t.TempDir(), Tier2Medium)
}

func TestFirstPassedObservationCreatesACheckpoint(t *testing.T) {
	ctx := ledgerCtx(t)
	body := []byte("x = 1\n")
	observeDeliverable(ctx, "solve.py", body, ValidationKindSyntax, ValidationPassed, "")
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d == nil || d.CheckpointHash != hashBytes(body) {
		t.Fatalf("no checkpoint created: %+v", d)
	}
	if string(d.CheckpointBytes) != string(body) {
		t.Errorf("checkpoint bytes wrong: %q", d.CheckpointBytes)
	}
	if k, s := d.CurrentValidation(); k != ValidationKindSyntax || s != ValidationPassed {
		t.Errorf("current validation = %v/%v", k, s)
	}
}

func TestALaterPassedHashReplacesTheCheckpoint(t *testing.T) {
	ctx := ledgerCtx(t)
	observeDeliverable(ctx, "solve.py", []byte("v1\n"), ValidationKindSyntax, ValidationPassed, "")
	observeDeliverable(ctx, "solve.py", []byte("v2222\n"), ValidationKindSyntax, ValidationPassed, "")
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if string(d.CheckpointBytes) != "v2222\n" {
		t.Errorf("newer passed bytes did not replace the checkpoint: %q", d.CheckpointBytes)
	}
	if d.CheckpointHash != hashBytes([]byte("v2222\n")) {
		t.Error("checkpoint hash not updated")
	}
}

func TestOnlyAnExplicitPassPromotes(t *testing.T) {
	for _, st := range []ValidationStatus{
		ValidationFailed, ValidationNotRun, ValidationNotApplicable, ValidationUnknown,
	} {
		ctx := ledgerCtx(t)
		observeDeliverable(ctx, "solve.py", []byte("x\n"), ValidationKindSyntax, st, "")
		d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
		if d.CheckpointHash != "" {
			t.Errorf("status %q promoted a checkpoint", st)
		}
		if _, s := d.CurrentValidation(); s != st {
			t.Errorf("status %q not recorded faithfully (got %q)", st, s)
		}
	}
}

func TestBaselineBytesNeverPromoteWithoutTheirOwnPass(t *testing.T) {
	ctx := ledgerCtx(t)
	// A pre-mutation snapshot observed with no verdict of its own.
	observeDeliverable(ctx, "solve.py", []byte("baseline\n"), ValidationKindUnknown,
		ValidationUnknown, "pre-mutation snapshot")
	if d := ctx.Ledger[ledgerKey(ctx, "solve.py")]; d.CheckpointHash != "" {
		t.Error("baseline bytes were promoted without explicit passed evidence")
	}
}

func TestCanonicalAliasesShareOneEntry(t *testing.T) {
	ctx := ledgerCtx(t)
	observeDeliverable(ctx, "solve.py", []byte("a\n"), ValidationKindSyntax, ValidationPassed, "")
	observeDeliverable(ctx, "./solve.py", []byte("bb\n"), ValidationKindSyntax, ValidationPassed, "")
	if len(ctx.Ledger) != 1 {
		t.Fatalf("aliases created %d entries: %v", len(ctx.Ledger), ctx.Ledger)
	}
	if d := ctx.Ledger[ledgerKey(ctx, "solve.py")]; string(d.CheckpointBytes) != "bb\n" {
		t.Errorf("alias write did not update the shared entry: %q", d.CheckpointBytes)
	}
}

func TestStaleHashMakesValidationHistoricalNotCurrent(t *testing.T) {
	ctx := ledgerCtx(t)
	observeDeliverable(ctx, "solve.py", []byte("good\n"), ValidationKindSyntax, ValidationPassed, "")
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	d.CurrentHash = hashBytes([]byte("something else\n")) // disk moved under us
	k, s := d.CurrentValidation()
	if s != ValidationUnknown || k != ValidationKindUnknown {
		t.Errorf("stale verdict presented as current: %v/%v", k, s)
	}
	if d.ValidationStatus != ValidationPassed {
		t.Error("historical evidence must be preserved, not erased")
	}
}

func TestCommandRehashKeepsUnchangedAndInvalidatesMutated(t *testing.T) {
	ctx := ledgerCtx(t)
	quiet := filepath.Join(ctx.WorkingDir, "quiet.py")
	noisy := filepath.Join(ctx.WorkingDir, "noisy.py")
	os.WriteFile(quiet, []byte("q\n"), 0o644)
	os.WriteFile(noisy, []byte("n\n"), 0o644)
	observeDeliverable(ctx, "quiet.py", []byte("q\n"), ValidationKindSyntax, ValidationPassed, "")
	observeDeliverable(ctx, "noisy.py", []byte("n\n"), ValidationKindSyntax, ValidationPassed, "")

	// A shell effect rewrites one of them.
	os.WriteFile(noisy, []byte("rewritten by a command\n"), 0o644)
	invalidateTrackedValidation(ctx)

	if _, s := ctx.Ledger[ledgerKey(ctx, "quiet.py")].CurrentValidation(); s != ValidationPassed {
		t.Error("an unchanged file lost its verdict")
	}
	nd := ctx.Ledger[ledgerKey(ctx, "noisy.py")]
	if _, s := nd.CurrentValidation(); s != ValidationUnknown {
		t.Error("a command-mutated file kept a verdict about bytes that are gone")
	}
	if nd.CurrentHash != hashBytes([]byte("rewritten by a command\n")) {
		t.Error("current hash not refreshed after the command")
	}
	if nd.CheckpointHash == "" {
		t.Error("the checkpoint of previously-passed bytes should survive")
	}
}

func TestBackgroundHazardOnlyClearsOnConfirmedExit(t *testing.T) {
	ctx := ledgerCtx(t)
	if workspaceHazardous(ctx) {
		t.Fatal("hazard set before any background work")
	}
	raiseWorkspaceHazard(ctx, "job1")
	if !workspaceHazardous(ctx) {
		t.Fatal("run_background must mark the workspace concurrently mutable")
	}
	// stop_background does not clear it: a signalled process may still flush.
	if !workspaceHazardous(ctx) {
		t.Fatal("hazard cleared by a stop request")
	}
	clearWorkspaceHazard(ctx, "job1") // confirmed exit
	if workspaceHazardous(ctx) {
		t.Error("confirmed exit did not clear the hazard")
	}
}

func TestDeleteTombstonesRetainsBytesAndProhibitsRestore(t *testing.T) {
	ctx := ledgerCtx(t)
	observeDeliverable(ctx, "solve.py", []byte("x\n"), ValidationKindSyntax, ValidationPassed, "")
	tombstoneDeliverable(ctx, "solve.py", "deleted")
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if !d.Tombstoned || !d.RestoreProhibited {
		t.Fatal("delete must tombstone and prohibit restoration")
	}
	if len(d.CheckpointBytes) == 0 {
		t.Error("bytes should be retained for a later policy decision")
	}
	if _, s := d.CurrentValidation(); s != ValidationUnknown {
		t.Error("a deleted file has no current validation")
	}
}

func TestMoveTombstonesSourceAndDoesNotTransferTheVerdict(t *testing.T) {
	ctx := ledgerCtx(t)
	observeDeliverable(ctx, "old.py", []byte("x\n"), ValidationKindSyntax, ValidationPassed, "")
	tombstoneDeliverable(ctx, "old.py", "moved:"+ledgerKey(ctx, "new.py"))
	src := ctx.Ledger[ledgerKey(ctx, "old.py")]
	if !src.Tombstoned || !strings.HasPrefix(src.TombstoneReason, "moved:") {
		t.Fatal("source not tombstoned as moved")
	}
	if dst := ctx.Ledger[ledgerKey(ctx, "new.py")]; dst != nil {
		t.Error("destination must be observed freshly, never inherited")
	}
}

func TestCheckpointCeilingsAreDeterministicAndFailClosed(t *testing.T) {
	ctx := ledgerCtx(t)
	big := make([]byte, maxCheckpointFileBytes+1)
	observeDeliverable(ctx, "big.py", big, ValidationKindSyntax, ValidationPassed, "")
	d := ctx.Ledger[ledgerKey(ctx, "big.py")]
	if d.CheckpointHash != "" {
		t.Error("an over-ceiling file was checkpointed")
	}
	if d.CheckpointUnavailable == "" {
		t.Error("checkpoint_unavailable must be recorded explicitly")
	}
	if _, s := d.CurrentValidation(); s != ValidationPassed {
		t.Error("the observation must survive even when the bytes cannot")
	}

	// Session ceiling: fill it, then prove the NEXT path is rejected rather
	// than some arbitrary existing entry being evicted.
	ctx2 := ledgerCtx(t)
	chunk := make([]byte, maxCheckpointFileBytes)
	for i := 0; i < maxCheckpointSessionBytes/maxCheckpointFileBytes; i++ {
		observeDeliverable(ctx2, fmt.Sprintf("f%d.py", i), chunk,
			ValidationKindSyntax, ValidationPassed, "")
	}
	held := 0
	for _, d := range ctx2.Ledger {
		held += len(d.CheckpointBytes)
	}
	observeDeliverable(ctx2, "one_too_many.py", chunk, ValidationKindSyntax, ValidationPassed, "")
	after := 0
	for _, d := range ctx2.Ledger {
		after += len(d.CheckpointBytes)
	}
	if after != held {
		t.Errorf("session ceiling evicted an existing entry: %d -> %d", held, after)
	}
	if ctx2.Ledger[ledgerKey(ctx2, "one_too_many.py")].CheckpointUnavailable == "" {
		t.Error("the rejected entry must say why")
	}
}
