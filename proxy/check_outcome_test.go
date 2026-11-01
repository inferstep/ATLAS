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
// returns ("", true) for many distinct conditions, and only one of them is a
// pass, so true means "do not refuse", never "validated". These rows pin the
// structured outcome that tells them apart.
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
		want            ValidationStatus
	}{
		// Applicability is decided BEFORE availability: an unsupported
		// extension is not_applicable even with no sandbox configured.
		{"unsupported extension, no sandbox", "notes.txt", "", ValidationNotApplicable},
		{"unsupported extension, sandbox up", "notes.txt", valid.URL, ValidationNotApplicable},
		{"jinja is not whole-file gated", "page.jinja", valid.URL, ValidationNotApplicable},
		{"applicable, no sandbox", "mod.py", "", ValidationNotRun},
		{"applicable, unreachable", "mod.py", deadURL, ValidationNotRun},
		{"applicable, non-200", "mod.py", five00.URL, ValidationNotRun},
		{"applicable, undecodable", "mod.py", broken.URL, ValidationNotRun},
		{"demonstrated pass", "mod.py", valid.URL, ValidationPassed},
		{"demonstrated failure", "mod.py", invalid.URL, ValidationFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := NewAgentContext(t.TempDir(), Tier2Medium)
			ctx.SandboxURL = c.url
			got := sandboxSyntaxOutcome(ctx, c.path, "A = 1\n")
			if got.Status != c.want {
				t.Errorf("Status = %q, want %q (detail %q)", got.Status, c.want, got.Detail)
			}
			if got.Status == ValidationPassed && !got.attempted() {
				t.Error("a pass must count as attempted")
			}
			// Only a demonstrated failure may refuse.
			_, ok := checkFallbackSyntax(ctx, c.path, "A = 1\n")
			if wantOK := c.want != ValidationFailed; ok != wantOK {
				t.Errorf("ok = %v, want %v -- fail-open behavior changed", ok, wantOK)
			}
		})
	}
}

// With no session at all, an applicable file is a check that did not run, and
// the write is not refused.
func TestSandboxSyntaxOutcomeWithANilContextIsNotRun(t *testing.T) {
	got := sandboxSyntaxOutcome(nil, "mod.py", "A = 1\n")
	if got.Status != ValidationNotRun {
		t.Errorf("Status = %q, want %q (detail %q)", got.Status, ValidationNotRun, got.Detail)
	}
	if _, ok := checkFallbackSyntax(nil, "mod.py", "A = 1\n"); !ok {
		t.Error("ok = false, want true -- fail-open behavior changed")
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

const jinjaPage = "page.jinja" // NOT in syntaxGateLanguages; IS embedded-capable
const pageWithScript = "<html><script>let a = 1;</script></html>"

// embeddedCheckStub answers the embedded-script check with body, counting
// those calls in embCalls, and fails every whole-file sandbox check.
func embeddedCheckStub(embCalls *int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/internal/embedded_script_check") {
			*embCalls++
			w.WriteHeader(200)
			w.Write([]byte(body))
			return
		}
		// whole-file sandbox check
		w.WriteHeader(200)
		w.Write([]byte(`{"valid":false,"errors":["SyntaxError: whole-file bad"]}`))
	}))
}

func TestFallbackSyntaxJinjaEmbeddedPassIsAPass(t *testing.T) {
	calls := 0
	svc := embeddedCheckStub(&calls, `{"ok":true,"findings":[]}`)
	defer svc.Close()
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.SandboxURL, ctx.V3URL = svc.URL, svc.URL
	o := fallbackSyntaxOutcomeFor(ctx, jinjaPage, pageWithScript)
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
}

func TestFallbackSyntaxJinjaEmbeddedUnavailableIsNotRun(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.SandboxURL, ctx.V3URL = "", "" // service absent
	o := fallbackSyntaxOutcomeFor(ctx, jinjaPage, pageWithScript)
	if o.WholeFile.Status != ValidationNotApplicable {
		t.Errorf("WholeFile = %q, want not_applicable", o.WholeFile.Status)
	}
	if o.Embedded.Status != ValidationNotRun {
		t.Errorf("Embedded = %q, want not_run", o.Embedded.Status)
	}
	if got := o.aggregate().Status; got != ValidationNotRun {
		t.Errorf("aggregate = %q, want not_run", got)
	}
}

func TestFallbackSyntaxJinjaEmbeddedFindingIsAFailure(t *testing.T) {
	calls := 0
	svc := embeddedCheckStub(&calls,
		`{"ok":true,"findings":[{"line":1,"message":"stray paren","snippet":"x)"}]}`)
	defer svc.Close()
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.SandboxURL, ctx.V3URL = svc.URL, svc.URL
	agg := fallbackSyntaxOutcomeFor(ctx, jinjaPage, pageWithScript).aggregate()
	if agg.Status != ValidationFailed {
		t.Fatalf("aggregate = %q, want failed", agg.Status)
	}
	if !strings.HasPrefix(agg.Detail, embeddedScriptErrPrefix) {
		t.Errorf("diagnostic lost its prefix: %q", agg.Detail)
	}
	// Wrapper parity: only a demonstrated failure refuses.
	msg, ok := checkFallbackSyntax(ctx, jinjaPage, pageWithScript)
	if ok || msg != agg.Detail {
		t.Errorf("wrapper ok=%v msg=%q, want false and the same diagnostic", ok, msg)
	}
}

func TestFallbackSyntaxWholeFileFailureShortCircuitsTheEmbeddedCall(t *testing.T) {
	calls := 0
	svc := embeddedCheckStub(&calls, `{"ok":true,"findings":[]}`)
	defer svc.Close()
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.SandboxURL, ctx.V3URL = svc.URL, svc.URL
	// .html IS whole-file gated, and the stub fails it.
	o := fallbackSyntaxOutcomeFor(ctx, "page.html", pageWithScript)
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
}

func TestFallbackSyntaxNoEmbeddedContentIsNotApplicable(t *testing.T) {
	calls := 0
	svc := embeddedCheckStub(&calls, `{"ok":true,"findings":[]}`)
	defer svc.Close()
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.SandboxURL, ctx.V3URL = "", svc.URL
	o := fallbackSyntaxOutcomeFor(ctx, jinjaPage, "<html><p>no script here</p></html>")
	if o.Embedded.Status != ValidationNotApplicable {
		t.Errorf("Embedded = %q, want not_applicable", o.Embedded.Status)
	}
	if calls != 0 {
		t.Errorf("embedded calls = %d, want 0", calls)
	}
	if o.aggregate().Status != ValidationNotApplicable {
		t.Errorf("aggregate = %q, want not_applicable", o.aggregate().Status)
	}
}

// --- The deliverable ledger -------------------------------------------------
//
// For each path the run touched, the ledger records whether the current bytes
// passed a check and which bytes last passed. ValidationStatus never crosses
// the event boundary, so without the ledger a finaliser would have nothing
// true to say and nothing safe to leave behind.

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

// Only an explicit pass makes a checkpoint. Every other status is still
// recorded as it was observed.
func TestOnlyAnExplicitPassPromotes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   ValidationKind
		status ValidationStatus
		reason string
	}{
		{"failed", ValidationKindSyntax, ValidationFailed, ""},
		{"not run", ValidationKindSyntax, ValidationNotRun, ""},
		{"not applicable", ValidationKindSyntax, ValidationNotApplicable, ""},
		{"unknown", ValidationKindSyntax, ValidationUnknown, ""},
		// A snapshot taken before a change has no verdict of its own.
		{"baseline snapshot", ValidationKindUnknown, ValidationUnknown, "pre-mutation snapshot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := ledgerCtx(t)
			observeDeliverable(ctx, "solve.py", []byte("x\n"), tc.kind, tc.status, tc.reason)
			d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
			if d.CheckpointHash != "" {
				t.Errorf("status %q promoted a checkpoint", tc.status)
			}
			if _, s := d.CurrentValidation(); s != tc.status {
				t.Errorf("status %q not recorded faithfully (got %q)", tc.status, s)
			}
		})
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
