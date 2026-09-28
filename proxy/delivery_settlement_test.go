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
// tests here that matter most are the ones proving settlement does not reach
// any of them.

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
	// The ledger observes the write through its own owner, exactly as the
	// agent loop does after a tool returns.
	recordLedgerEffect("write_file", mustArgs(t, w.path), w.ctx, res)
	return &settledWorld{routeWorld: w, res: res}
}

func mustArgs(t *testing.T, path string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (w *settledWorld) obligations() []taskObligation { return requestObligations(w.ctx) }

func (w *settledWorld) settle(t *testing.T) (bool, string) {
	t.Helper()
	owed, why := postDeliverySettlementOwed(w.ctx)
	return !owed, why
}

const settledContract = `{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`

// --- the settled case -------------------------------------------------------------

func TestAnAuthorizedDeliverySettlesExistence(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	if w.res.AuthorizedDeliveryHash == "" {
		t.Fatal("the fixture did not deliver through the typed path")
	}
	settled, why := w.settle(t)
	if !settled {
		t.Errorf("an authorized delivery did not settle: %s", why)
	}
	// And it settles the obligation it is about.
	done, owed, _ := settlementStatus(w.ctx, w.obligations(), nil)
	if len(done) != 1 || len(owed) != 0 {
		t.Errorf("settled %v owed %v, want the one existence obligation settled", done, owed)
	}
	if !strings.HasPrefix(done[0], ObligationArtifactExists+":") {
		t.Errorf("settled %v, want the existence obligation", done)
	}
}

// --- what settlement refuses -------------------------------------------------------

func TestSettlementCannotBeManufactured(t *testing.T) {
	// A run that wrote the file successfully but never delivered through the
	// typed path has nothing to settle -- and a successful tool result is not
	// a settlement.
	w := newRouteWorld(t, settledContract, nil)
	if err := os.WriteFile(w.path, []byte(routeWinner), 0o644); err != nil {
		t.Fatal(err)
	}
	observeDeliverable(w.ctx, w.path, []byte(routeWinner),
		ValidationKindSyntax, ValidationPassed, "")
	obs := requestObligations(w.ctx)
	for _, o := range postDeliverySettlement(obs) {
		if ok, _ := settleExistence(w.ctx, o, obs); ok {
			t.Error("a write with no authorization settled existence")
		}
	}
}

func TestALaterMutationUnsettlesTheDelivery(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	if settled, _ := w.settle(t); !settled {
		t.Fatal("the fixture did not settle")
	}
	// Something rewrote the artifact afterwards.
	if err := os.WriteFile(w.path, []byte("SOMETHING ELSE = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	settled, why := w.settle(t)
	if settled {
		t.Error("settlement survived the artifact being rewritten")
	}
	if !strings.Contains(why, "bytes on disk") {
		t.Errorf("reason %q does not name the bytes", why)
	}
}

func TestRecreationDoesNotRestoreSettlement(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	// Deleted and put back with the same bytes. The ledger records the
	// removal, and a tombstone is not something a later write undoes for
	// settlement's purposes.
	tombstoneDeliverable(w.ctx, w.path, "deleted")
	if err := os.WriteFile(w.path, []byte(routeWinner), 0o644); err != nil {
		t.Fatal(err)
	}
	if settled, _ := w.settle(t); settled {
		t.Error("settlement survived the artifact being deleted and recreated")
	}
}

func TestAStaleLedgerRecordCannotSettle(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	// The ledger still holds the delivery, but its recorded bytes have moved
	// on from what is at the path.
	w.ctx.LedgerMu.Lock()
	w.ctx.Ledger[filepath.Clean(w.path)].CurrentHash = contentSHA256("a different story\n")
	w.ctx.LedgerMu.Unlock()
	if settled, why := w.settle(t); settled {
		t.Errorf("a stale ledger record settled existence (%s)", why)
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
	// The ledger effect is deliberately NOT recorded. The bytes are right and
	// the delivery happened; the session's own record never saw it.
	obs := requestObligations(w.ctx)
	for _, o := range postDeliverySettlement(obs) {
		ok, why := settleExistence(w.ctx, o, obs)
		if ok {
			t.Error("settlement did not require the ledger to have seen the write")
		}
		if !strings.Contains(why, "ledger") {
			t.Errorf("reason %q does not name the ledger", why)
		}
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
	if settled, _ := settleExistence(w.ctx, o, w.obligations()); settled {
		t.Error("one target's delivery settled another's existence")
	}
}

func TestAStrongerObligationAppearingLaterIsNotDischarged(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	if settled, _ := w.settle(t); !settled {
		t.Fatal("the fixture did not settle")
	}
	// The task now declares a command the delivery never answered for.
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
	settled, why := w.settle(t)
	if settled {
		t.Error("an older delivery discharged an obligation it never answered")
	}
	if !strings.Contains(why, "declared command") {
		t.Errorf("reason %q does not name the command", why)
	}
}

func TestAnUnpreservedBaselineNeverSettles(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	// Rewrite the record as one that did not preserve what it replaced. The
	// artifact is present and the ledger agrees; existence is still not
	// discharged, because the delivery was not entitled to what it did.
	w.ctx.grantMu.Lock()
	w.ctx.settlements[filepath.Clean(w.path)].BaselinePreserved = false
	w.ctx.grantMu.Unlock()
	if settled, why := w.settle(t); settled {
		t.Errorf("an unentitled delivery settled existence (%s)", why)
	}
}

func TestAnotherRequestsSettlementDoesNotCount(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	w.ctx.grantMu.Lock()
	w.ctx.settlements[filepath.Clean(w.path)].RequestID = "req-somebody-else"
	w.ctx.grantMu.Unlock()
	if settled, _ := w.settle(t); settled {
		t.Error("another request's delivery settled this one's existence")
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

func TestSettlementIsOnlyAskedAboutDeliveredTargets(t *testing.T) {
	// A declared output the run never wrote. Settlement has nothing to say --
	// that absence is missingExpectedOutputs' question, and answering it here
	// too would be two rules for one obligation.
	w := newRouteWorld(t, settledContract, nil)
	owed, why := postDeliverySettlementOwed(w.ctx)
	if owed {
		t.Errorf("settlement claimed an undelivered output (%s)", why)
	}
}

func TestContractlessTrafficOwesNoSettlement(t *testing.T) {
	for _, contract := range []string{"", `{"task_mode":"work"}`} {
		w := newSettledWorld(t, contract, nil)
		if owed, why := postDeliverySettlementOwed(w.ctx); owed {
			t.Errorf("%q: contractless traffic owed settlement (%s)", contract, why)
		}
	}
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

func TestTheSettlementRecordCarriesNoContent(t *testing.T) {
	const secret = "TOKEN = 'hunter2'\n" + routeWinner
	w := newRouteWorld(t, settledContract, nil)
	// The record is written by the delivery owner; inspect whatever this run
	// produced for content of any kind.
	if _, err := w.write(t); err != nil {
		t.Fatal(err)
	}
	w.ctx.grantMu.Lock()
	blob, err := json.Marshal(w.ctx.settlements)
	w.ctx.grantMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{secret, "hunter2", "TOKEN", "def solve", "return sum"} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the settlement record carries %q", needle)
		}
	}
}

// TestAnUnsettledDeliveryBlocksCompletion is the behavioural half of the
// terminal integration: a run that delivered and then lost the artifact does
// not get to say it finished.
func TestAnUnsettledDeliveryBlocksCompletion(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	// The artifact is rewritten by something else after the delivery, and the
	// run has current green verification for the bytes that are there now --
	// so every owner ahead of settlement is satisfied and settlement is the
	// one thing left to object.
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

// TestASettledDeliveryRaisesNoSettlementObjection is the other half: when the
// artifact is still what was authorized, settlement says nothing.
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

// TestSettlementDoesNotRescueAnUndeliveredOutput pins the boundary the other
// way: settlement is silent about an output nothing delivered, because
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
