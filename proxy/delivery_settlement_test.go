package main

import (
	"encoding/json"
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Discharging existence, after the bytes landed and not before.
//
// The question is narrow on purpose: is the artifact this run authorized still
// there, at the bytes it was authorized at, with the session's own record
// agreeing? Everything else a run owes is owed by its existing owner, and the
// tests that matter most here prove that settlement does not reach any of them.

// settledWorld is a delivery that actually happened, ready to be settled.
type settledWorld struct {
	*routeWorld
	res *ToolResult
}

func newSettledWorld(t *testing.T, contract string, commands map[string]stubEffect) *settledWorld {
	t.Helper()
	w := newRouteWorld(t, contract, commands)
	res, err := w.write(t)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	// The ledger observes the write through its own owner, as the agent loop
	// does after a tool returns.
	args, err := json.Marshal(map[string]string{"path": w.path})
	if err != nil {
		t.Fatal(err)
	}
	recordLedgerEffect("write_file", args, w.ctx, res)
	return &settledWorld{routeWorld: w, res: res}
}

const settledContract = `{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`

// --- the settled case -------------------------------------------------------------

func TestAnAuthorizedDeliverySettlesExistence(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	if w.res.AuthorizedDeliveryHash == "" {
		t.Fatal("the fixture did not deliver through the typed path")
	}
	if owed, why := postDeliverySettlementOwed(w.ctx); owed {
		t.Errorf("an authorized delivery did not settle: %s", why)
	}
	done, owed, _ := settlementStatus(w.ctx, requestObligations(w.ctx), nil)
	if len(done) != 1 || len(owed) != 0 {
		t.Fatalf("settled %v owed %v, want the one existence obligation settled", done, owed)
	}
	if !strings.HasPrefix(done[0], ObligationArtifactExists+":") {
		t.Errorf("settled %v, want the existence obligation", done)
	}
}

// --- what settlement refuses -------------------------------------------------------

// A run that wrote the file successfully but never delivered through the typed
// path has nothing to settle. A successful tool result is not a settlement.
func TestSettlementCannotBeManufactured(t *testing.T) {
	w := newRouteWorld(t, settledContract, nil)
	if err := os.WriteFile(w.path, []byte(routeWinner), 0o644); err != nil {
		t.Fatal(err)
	}
	observeDeliverable(w.ctx, w.path, []byte(routeWinner),
		ValidationKindSyntax, ValidationPassed, "")
	obs := requestObligations(w.ctx)
	settling := postDeliverySettlement(obs)
	if len(settling) == 0 {
		t.Fatal("the declared output owes no settlement, so there is nothing to check")
	}
	for _, o := range settling {
		if ok, _ := settleExistence(w.ctx, o, obs); ok {
			t.Error("a write with no authorization settled existence")
		}
	}
}

func TestALedgerThatNeverSawTheWriteCannotSettle(t *testing.T) {
	w := newRouteWorld(t, settledContract, nil)
	res, err := w.write(t)
	if err != nil {
		t.Fatal(err)
	}
	if res.AuthorizedDeliveryHash == "" {
		t.Fatal("the fixture did not deliver through the typed path")
	}
	// The ledger effect is not recorded. The bytes are right and the delivery
	// happened; the session's own record never saw it.
	obs := requestObligations(w.ctx)
	settling := postDeliverySettlement(obs)
	if len(settling) == 0 {
		t.Fatal("the declared output owes no settlement, so there is nothing to check")
	}
	for _, o := range settling {
		ok, why := settleExistence(w.ctx, o, obs)
		if ok {
			t.Error("settlement did not require the ledger to have seen the write")
		}
		if !strings.Contains(why, "ledger") {
			t.Errorf("reason %q does not name the ledger", why)
		}
	}
}

func TestAChangeAfterDeliveryUnsettlesIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(t *testing.T, w *settledWorld)
		wantWhy string
	}{
		{"the artifact is rewritten", func(t *testing.T, w *settledWorld) {
			if err := os.WriteFile(w.path, []byte("SOMETHING ELSE = 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "bytes on disk"},
		// The ledger records the removal, and a later write of the same bytes
		// does not undo a tombstone for settlement's purposes.
		{"the artifact is deleted and put back", func(t *testing.T, w *settledWorld) {
			tombstoneDeliverable(w.ctx, w.path, "deleted")
			if err := os.WriteFile(w.path, []byte(routeWinner), 0o644); err != nil {
				t.Fatal(err)
			}
		}, ""},
		// The ledger still holds the delivery, but its recorded bytes are not
		// what is at the path.
		{"the ledger record is stale", func(t *testing.T, w *settledWorld) {
			w.ctx.LedgerMu.Lock()
			w.ctx.Ledger[filepath.Clean(w.path)].CurrentHash = contentSHA256("a different story\n")
			w.ctx.LedgerMu.Unlock()
		}, ""},
		// The artifact is present and the ledger agrees, but the delivery was
		// not entitled to replace what it replaced.
		{"the baseline was not preserved", func(t *testing.T, w *settledWorld) {
			w.ctx.grantMu.Lock()
			w.ctx.settlements[filepath.Clean(w.path)].BaselinePreserved = false
			w.ctx.grantMu.Unlock()
		}, ""},
		{"the settlement is another request's", func(t *testing.T, w *settledWorld) {
			w.ctx.grantMu.Lock()
			w.ctx.settlements[filepath.Clean(w.path)].RequestID = "req-somebody-else"
			w.ctx.grantMu.Unlock()
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newSettledWorld(t, settledContract, nil)
			if owed, why := postDeliverySettlementOwed(w.ctx); owed {
				t.Fatalf("the fixture did not settle: %s", why)
			}
			tc.change(t, w)
			owed, why := postDeliverySettlementOwed(w.ctx)
			if !owed {
				t.Error("the delivery is still settled")
			}
			if !strings.Contains(why, tc.wantWhy) {
				t.Errorf("reason %q does not contain %q", why, tc.wantWhy)
			}
		})
	}
}

// The task then declares a command the delivery never answered for.
func TestACommandDeclaredAfterDeliveryUnsettlesIt(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	if owed, why := postDeliverySettlementOwed(w.ctx); owed {
		t.Fatalf("the fixture did not settle: %s", why)
	}
	w.ctx.TaskContract = mustContract(t, w.dir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`)
	commands := 0
	for _, o := range authorizationPrerequisites(requestObligations(w.ctx)) {
		if o.Kind == ObligationDeclaredCommand && o.Required {
			commands++
		}
	}
	if commands != 1 {
		t.Fatalf("the fixture declared %d commands, want 1", commands)
	}
	owed, why := postDeliverySettlementOwed(w.ctx)
	if !owed {
		t.Error("the delivery is still settled")
	}
	if !strings.Contains(why, "declared command") {
		t.Errorf("reason %q does not name the declared command", why)
	}
}

func TestSettlementCannotSettleAnotherTarget(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	other := filepath.Join(w.dir, "other.py")
	if err := os.WriteFile(other, []byte(routeWinner), 0o644); err != nil {
		t.Fatal(err)
	}
	observeDeliverable(w.ctx, other, []byte(routeWinner),
		ValidationKindSyntax, ValidationPassed, "")
	o, ok := newTaskObligation(ObligationArtifactExists, other, "", true)
	if !ok {
		t.Fatal("obligation refused")
	}
	if settled, _ := settleExistence(w.ctx, o, requestObligations(w.ctx)); settled {
		t.Error("one target's delivery settled another's existence")
	}
}

// Settlement is asked only about targets the typed path delivered. A declared
// output the run never wrote is missingExpectedOutputs' question, and answering
// it here too would be two rules for one obligation.
func TestSettlementOwesNothingWithoutATypedDelivery(t *testing.T) {
	for _, tc := range []struct {
		name      string
		contract  string
		delivered bool
	}{
		{"a declared output never written", settledContract, false},
		{"no contract", "", true},
		{"work with no declared outputs", `{"task_mode":"work"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w *routeWorld
			if tc.delivered {
				w = newSettledWorld(t, tc.contract, nil).routeWorld
			} else {
				w = newRouteWorld(t, tc.contract, nil)
			}
			if owed, why := postDeliverySettlementOwed(w.ctx); owed {
				t.Errorf("settlement was owed (%s)", why)
			}
		})
	}
}

// --- what settlement does NOT waive -----------------------------------------------

// TestSettlementWaivesNothingElse reads the owner's own calls. Existence is
// the only obligation it may discharge; every other debt has its own owner,
// and a settlement that reached one would be a second completion rule.
func TestSettlementWaivesNothingElse(t *testing.T) {
	// The owner's CALLS, not its prose: a comment may name the owner of
	// another debt in order to say it is not settlement's question, and the
	// code may not call it.
	files := proxyFiles(t)
	f := files["delivery_settlement.go"]
	if f == nil {
		t.Fatal("the settlement owner is gone")
	}
	banned := map[string]bool{
		"settleMutationDebt": true, "debtResolved": true, "hasUnresolvedDebt": true,
		"settleBackgroundHazard": true, "raiseWorkspaceHazard": true,
		"lowerWorkspaceHazard": true, "decideVerificationDemand": true,
		"decideActionDemand": true, "wantsStateChange": true,
		"promoteFulfilledDeletion": true, "tombstoneDeliverable": true,
		"approvedDeletionPaths": true, "missingExpectedOutputs": true,
		"terminalCompletionAllowed": true, "emitTerminal": true,
		"writeFileRecorded": true, "restoreDeliverable": true,
		"observeDeliverable": true, "consumeAuthorizationGrant": true,
		"mintAuthorizationGrant": true, "decideAuthorization": true,
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if banned[name] {
			t.Errorf("settlement calls %s: existence is the only thing it discharges", name)
		}
		return true
	})
}

// --- the terminal --------------------------------------------------------------

func TestTheTerminalAsksTheExistingOwner(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func finalizeCompletion(")
	if start < 0 {
		t.Fatal("the finalizer is gone")
	}
	end := strings.Index(body[start+1:], "\nfunc ")
	region := body[start:]
	if end >= 0 {
		region = body[start : start+1+end]
	}
	if !strings.Contains(region, "postDeliverySettlementOwed(ctx)") {
		t.Error("the terminal does not ask about settlement")
	}
	// One question, one owner: the finalizer must not re-derive settlement
	// for itself.
	for _, banned := range []string{"settleExistence(", "deliverySettlementFor(", "settlementStatus("} {
		if strings.Contains(region, banned) {
			t.Errorf("the terminal calls %s directly instead of asking the owner", banned)
		}
	}
	// And exactly one production caller of the question.
	if sites := callSites(proxyFiles(t), "postDeliverySettlementOwed"); len(sites) != 1 {
		t.Errorf("settlement is asked from %v, want exactly the finalizer", sites)
	}
}

// The record is written by the delivery owner. Whatever this run produced must
// hold identities only, never the bytes it delivered or replaced.
func TestTheSettlementRecordCarriesNoContent(t *testing.T) {
	w := newRouteWorld(t, settledContract, nil)
	if _, err := w.write(t); err != nil {
		t.Fatal(err)
	}
	w.ctx.grantMu.Lock()
	records := len(w.ctx.settlements)
	blob, err := json.Marshal(w.ctx.settlements)
	w.ctx.grantMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if records == 0 {
		t.Fatal("the delivery left no settlement record to inspect")
	}
	for _, needle := range []string{routeWinner, routeBaseline, "def solve", "return sum", "total += v"} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the settlement record carries %q", needle)
		}
	}
}

// A run that delivered and then lost the artifact does not get to say it
// finished.
func TestAnUnsettledDeliveryBlocksCompletion(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	// Something else rewrites the artifact after the delivery, and the run has
	// current green verification for the bytes that are there now. Every owner
	// ahead of settlement is satisfied, so settlement is the one thing left to
	// object.
	const replaced = "SOMETHING ELSE = 1\n"
	if err := os.WriteFile(w.path, []byte(replaced), 0o644); err != nil {
		t.Fatal(err)
	}
	observeDeliverable(w.ctx, w.path, []byte(replaced),
		ValidationKindSyntax, ValidationPassed, "")
	w.ctx.VerificationEvidence = append(w.ctx.VerificationEvidence, VerificationRecord{
		Command: "python3 solve.py",
		Covered: map[string]string{w.path: contentSHA256(replaced)}, Turn: 1,
	})

	status, reason := finalizeCompletion(w.ctx, &runState{
		expectedOutputs:      []string{"solve.py"},
		madeProductiveChange: true,
	}, "make it fast", "")
	if status == TerminalCompleted {
		t.Error("a run whose delivered artifact was replaced reported completion")
	}
	if reason != "post_delivery_settlement_pending" {
		t.Errorf("status %q reason %q, want the settlement objection", status, reason)
	}
}

// When the artifact is still what was authorized, settlement says nothing.
func TestASettledDeliveryRaisesNoSettlementObjection(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	w.ctx.VerificationEvidence = append(w.ctx.VerificationEvidence, VerificationRecord{
		Command: "python3 solve.py",
		Covered: map[string]string{w.path: contentSHA256(routeWinner)}, Turn: 1,
	})
	_, reason := finalizeCompletion(w.ctx, &runState{
		expectedOutputs:      []string{"solve.py"},
		madeProductiveChange: true,
	}, "make it fast", "")
	if reason == "post_delivery_settlement_pending" {
		t.Error("a settled delivery was reported unsettled")
	}
}

// Settlement is silent about an output nothing delivered, because
// missingExpectedOutputs already owns that.
func TestSettlementDoesNotRescueAnUndeliveredOutput(t *testing.T) {
	w := newRouteWorld(t, settledContract, nil)
	status, reason := finalizeCompletion(w.ctx, &runState{
		expectedOutputs: []string{"solve.py"}}, "make it fast", "")
	if reason == "post_delivery_settlement_pending" {
		t.Error("settlement answered for an output it never saw delivered")
	}
	if status == TerminalCompleted {
		t.Error("a run that produced nothing was reported complete")
	}
}
