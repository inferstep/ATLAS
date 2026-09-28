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

// A checker the sandbox stopped before a verdict (a wall-clock or memory
// ceiling, or it never started) is not a pass and not a syntax error, and it
// is not the sandbox being down either: that flag feeds producer-unavailable
// reasons elsewhere (audit S-sandbox/INTEGRITY#1).
func TestAStoppedSyntaxCheckerIsNotRun(t *testing.T) {
	replies := map[string]map[string]interface{}{
		"stopped": {"valid": false, "status": "not_run", "outcome": "timed_out",
			"errors": []string{"syntax verification unavailable: the checker ended timed_out"}},
		"broken": {"valid": false, "status": "checked", "outcome": "completed",
			"errors": []string{"SyntaxError: invalid syntax (line 2)"}},
		"clean": {"valid": true, "status": "checked", "outcome": "completed", "errors": []string{}},
		// A sandbox that predates the field answers as before.
		"legacy": {"valid": true, "errors": []string{}},
	}
	for name, reply := range replies {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(reply)
		}))
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		ctx.SandboxURL = srv.URL
		got := sandboxSyntaxOutcome(ctx, "app.py", "x = 1\n")
		srv.Close()
		switch name {
		case "stopped":
			if got.Status != ValidationNotRun || got.ProducerUnavailable {
				t.Errorf("stopped checker: %+v, want not_run without producer-unavailable", got)
			}
		case "broken":
			if got.Status != ValidationFailed {
				t.Errorf("syntax error: %+v, want failed", got)
			}
		default:
			if got.Status != ValidationPassed {
				t.Errorf("%s: %+v, want passed", name, got)
			}
		}
	}
}

// P-gates/INTEGRITY#1: completion read only the whole-file parse, so a Flask
// app whose embedded <script> the harness had just found broken was
// "demonstrated", and a clean run of the server then overwrote that verdict.
func TestABrokenEmbeddedScriptIsNotDemonstrated(t *testing.T) {
	app := flaskWithScript(strayParenLine)
	for _, reachable := range []bool{true, false} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(app), 0o644); err != nil {
			t.Fatal(err)
		}
		sbx := fakeSyntaxSandbox(t, "")
		v3 := fakeV3Embedded(t, "'DOWN');", nil)
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.SandboxURL = sbx.URL
		ctx.V3URL = v3.URL
		if !reachable {
			ctx.V3URL = "http://127.0.0.1:9"
		}
		ok, why := terminalCompletionAllowed(ctx, []string{"app.py"})
		sbx.Close()
		v3.Close()
		if reachable && ok {
			t.Fatalf("a demonstrated embedded-script failure completed: %s", why)
		}
		// Fail-soft: an embedded check that could not run is no finding.
		if !reachable && !ok {
			t.Fatalf("an unreachable embedded check blocked completion: %s", why)
		}
	}
}

func TestACleanRunDoesNotSettleAnEmbeddedScriptFailure(t *testing.T) {
	dir := t.TempDir()
	app := flaskWithScript(strayParenLine)
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(app), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	key := ledgerKey(ctx, "app.py")
	observeDeliverable(ctx, key, []byte(app), ValidationKindSyntax, ValidationFailed,
		embeddedScriptErrPrefix+"app.py line 7: unexpected `)`")
	st := &runState{mutationDebt: map[string]*mutationDebtEntry{key: {Rel: "app.py", Kind: debtContent}}}

	settleDebtByExecution(ctx, st, "python3 app.py", true)

	if len(st.mutationDebt) != 1 {
		t.Fatal("a clean server run settled the debt of a broken embedded script")
	}
	if d := ctx.Ledger[key]; d.ValidationKind == ValidationKindExecution {
		t.Fatal("the embedded-script failure was overwritten with execution/passed")
	}
}

// V-pipeline/INTEGRITY#2: the run was told "V3 verified this edit ... The
// fix is on disk and build-checked ... respond NOW with done" whenever V3's
// phase name sounded like success, including a phase1 reached by agreement
// between candidates or a compile, with nothing ever running the code.
func TestTheV3NudgeSaysWhatARunShowed(t *testing.T) {
	ctx, dir := sepCtx(t, nil)
	h := sepWrite(t, ctx, dir, "app.py", "print('ok')\n")

	msg, shown := v3DeliveryNudge(ctx, "phase1", 3, 0.42, "app.py", "")
	if shown || strings.Contains(msg, "respond with") || strings.Contains(msg, "build-checked") ||
		!strings.Contains(msg, "python3 app.py") {
		t.Fatalf("nothing ran app.py, and the nudge says: %q", msg)
	}

	ctx.VerificationEvidence = append(ctx.VerificationEvidence,
		sepStamp(ctx, "python3 app.py", map[string]string{resolveAgentPath(ctx, "app.py"): h}))
	msg, shown = v3DeliveryNudge(ctx, "phase1", 3, 0.42, "app.py", "")
	if !shown || !strings.Contains(msg, "shows it working") {
		t.Fatalf("a current run shows app.py working, and the nudge says: %q", msg)
	}

	msg, _ = v3DeliveryNudge(ctx, "phase1", 3, 0.42, "app.py", "app.py")
	if !strings.Contains(msg, "ITS OWN version of app.py") || strings.Contains(msg, "respond with") {
		t.Fatalf("a superseded delivery is told to finish: %q", msg)
	}
}

// P-safety/INTEGRITY#4: the identical-resend refusal keyed structural_edit on
// path and selector only, so a corrected body was refused "byte for byte",
// the tool was banned for the file and the run ended repeated_refusal.
func TestAStructuralEditWithANewBodyIsNotAResend(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	call := func(body string) json.RawMessage {
		b, _ := json.Marshal(StructuralEditInput{Path: "app.py", Selector: "function:update", Content: body})
		return b
	}
	recordFailedToolCall(ctx, "structural_edit", call("def update(:\n    pass\n"), "does not parse")

	if msg := identicalRetryRefusal(ctx, "structural_edit", call("def update():\n    pass\n")); msg != "" {
		t.Fatalf("a corrected body was refused as a byte-for-byte re-send: %q", msg)
	}
	if msg := identicalRetryRefusal(ctx, "structural_edit", call("def update(:\n    pass\n")); msg == "" {
		t.Fatal("an identical re-send of a refused call is no longer refused")
	}
	// The repeat window keeps its own identity: the selector, whatever the body.
	if toolCallSignature("structural_edit", call("a")) != toolCallSignature("structural_edit", call("b")) {
		t.Fatal("the repeat window no longer treats bodies against one selector as repeats")
	}
}
