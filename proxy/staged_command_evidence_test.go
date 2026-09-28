package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Staged evidence is the strong kind: it names the request, the invocation,
// the candidate instance, the exact bytes, the canonical target, the baseline
// and workspace it was bound to, the exact command by hash, and a structured
// outcome. These tests hold it to every one of those bindings.

type stagedCmdWorld struct {
	ctx    *AgentContext
	dir    string
	target string
	hash   string
	id     stagingIdentity
	ob     taskObligation
	tc     *TaskContract
}

const stagedDeclaredCommand = "pytest"

func newStagedCmdWorld(t *testing.T) *stagedCmdWorld {
	t.Helper()
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-staged")
	tc := sepContract([]string{"solve.py"}, []string{stagedDeclaredCommand})
	ctx.TaskContract = tc

	body := "print('candidate')\n"
	target := filepath.Join(dir, "solve.py")
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx.SessionWrites["solve.py"] = true
	hash := contentSHA256(body)
	observeDeliverable(ctx, "solve.py", []byte(body), ValidationKindSyntax, ValidationPassed, "")

	ob, ok := newTaskObligation(ObligationDeclaredCommand, stagedDeclaredCommand, VerificationKindRuntime, true)
	if !ok {
		t.Fatal("obligation")
	}
	generation, state := workspaceIdentity(ctx)
	w := &stagedCmdWorld{ctx: ctx, dir: dir, target: target, hash: hash, ob: ob, tc: tc,
		id: stagingIdentity{
			RequestID: "req-staged", InvocationID: "req-staged:inv:1",
			CandidateInstanceID: "req-staged:inv:1:cand", CandidateHash: hash,
			TargetPath: target, WorkspaceGeneration: generation, WorkspaceStateHash: state,
		}}
	w.consumeGrant(t)
	return w
}

// consumeGrant puts a spent one-time grant in the store, as delivery leaves it.
func (w *stagedCmdWorld) consumeGrant(t *testing.T) {
	t.Helper()
	key := grantKey(w.id.RequestID, w.id.InvocationID, w.id.CandidateInstanceID, w.target)
	if w.ctx.grants == nil {
		w.ctx.grants = map[string]*authorizationGrant{}
	}
	w.ctx.grants[key] = &authorizationGrant{
		ID: key, RequestID: w.id.RequestID, InvocationID: w.id.InvocationID,
		CandidateInstanceID: w.id.CandidateInstanceID, CandidateHash: w.hash,
		TargetPath: w.target, retired: grantConsumed,
	}
}

func (w *stagedCmdWorld) result() stagingCommandResult {
	return stagingCommandResult{
		CommandIdentity: contentSHA256(stagedDeclaredCommand), ObligationID: w.ob.ID,
		Outcome: stagingExitedZero, TargetHashBefore: w.hash, TargetHashAfter: w.hash,
		WorkspaceHashBefore: "ws", WorkspaceHashAfter: "ws",
	}
}

func (w *stagedCmdWorld) record(r stagingCommandResult) {
	recordStagedCommandFulfillment(w.ctx, w.ob, r, w.id)
}

func (w *stagedCmdWorld) demand() verificationDemand {
	return decideVerificationDemand(w.ctx, w.tc, []string{"solve.py"})
}

// --- the path that should work ----------------------------------------------

func TestStagedCommandOverExactBytesClearsBothDemands(t *testing.T) {
	w := newStagedCmdWorld(t)
	w.record(w.result())
	if len(w.ctx.StagedCommands) != 1 {
		t.Fatalf("nothing was recorded: %+v", w.ctx.StagedCommands)
	}
	if d := w.demand(); !d.Met {
		t.Fatalf("staged evidence carries both bindings, got %+v", d)
	}
	if !stagedCommandSatisfied(w.ctx, stagedDeclaredCommand) {
		t.Error("command obligation not satisfied")
	}
	if !stagedCoverageSatisfied(w.ctx, w.target, w.hash) {
		t.Error("path coverage not satisfied")
	}
}

func TestVerificationDoesNotDependOnASettlementRecord(t *testing.T) {
	w := newStagedCmdWorld(t)
	w.record(w.result())
	if deliverySettlementFor(w.ctx, w.target) != nil {
		t.Fatal("precondition: no settlement record should exist yet")
	}
	if d := w.demand(); !d.Met {
		t.Fatalf("the gate needed a settlement record it cannot have yet: %+v", d)
	}
	// And settlement is still owed afterwards, in its own place.
	if owed, _ := postDeliverySettlementOwed(w.ctx); owed {
		t.Log("settlement owed, as its own separate gate")
	}
}

// --- every binding, one at a time -------------------------------------------

func TestStagedEvidenceFailsClosedOnEveryBrokenBinding(t *testing.T) {
	cases := map[string]func(*stagedCmdWorld, *stagingCommandResult){
		"candidate hash mismatch": func(w *stagedCmdWorld, r *stagingCommandResult) {
			w.id.CandidateHash = contentSHA256("other bytes\n")
		},
		"changed target during staging": func(w *stagedCmdWorld, r *stagingCommandResult) {
			r.MutatedTarget = true
		},
		"changed workspace during staging": func(w *stagedCmdWorld, r *stagingCommandResult) {
			r.MutatedWorkspace = true
		},
		"wrong obligation": func(w *stagedCmdWorld, r *stagingCommandResult) {
			r.ObligationID = "declared_command:0000"
		},
		"wrong command identity": func(w *stagedCmdWorld, r *stagingCommandResult) {
			r.CommandIdentity = contentSHA256("make check")
		},
		"staged against other bytes": func(w *stagedCmdWorld, r *stagingCommandResult) {
			r.TargetHashBefore = contentSHA256("something else\n")
		},
		"incomplete identity": func(w *stagedCmdWorld, r *stagingCommandResult) {
			w.id.CandidateInstanceID = ""
		},
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			w := newStagedCmdWorld(t)
			r := w.result()
			break_(w, &r)
			w.record(r)
			if len(w.ctx.StagedCommands) != 0 {
				t.Fatalf("%s was admitted", name)
			}
			if d := w.demand(); d.Met {
				t.Fatalf("%s satisfied the demand", name)
			}
		})
	}
}

func TestEveryUntrustworthyStagingOutcomeIsRefused(t *testing.T) {
	for _, outcome := range []stagingCommandOutcome{
		stagingExitedNonZero, stagingRefused, stagingCancelled, stagingTimedOut,
		stagingBudgetExceeded, stagingUnobservable, stagingUnavailable,
		stagingMutatedTarget, stagingMutatedWorkspace,
	} {
		w := newStagedCmdWorld(t)
		r := w.result()
		r.Outcome = outcome
		w.record(r)
		if len(w.ctx.StagedCommands) != 0 {
			t.Errorf("%q was admitted as evidence", outcome)
		}
	}
}

func TestStagedEvidenceFromAnotherIdentityDoesNotCount(t *testing.T) {
	cases := map[string]func(*stagedCmdWorld){
		"another request": func(w *stagedCmdWorld) {
			w.ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-other")
		},
		"another invocation":         func(w *stagedCmdWorld) { w.ctx.StagedCommands[0].InvocationID = "req-staged:inv:2" },
		"another candidate instance": func(w *stagedCmdWorld) { w.ctx.StagedCommands[0].CandidateInstanceID = "other" },
		"another target":             func(w *stagedCmdWorld) { w.ctx.StagedCommands[0].TargetPath = filepath.Join(w.dir, "other.py") },
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			w := newStagedCmdWorld(t)
			w.record(w.result())
			break_(w)
			if d := w.demand(); d.Met {
				t.Fatalf("%s satisfied the demand", name)
			}
		})
	}
}

func TestAnUnconsumedOrReplayedGrantFailsClosed(t *testing.T) {
	for name, state := range map[string]grantRetirement{
		"never consumed": grantLive,
		"attempted":      grantAttempted,
		"cancelled":      grantCancelled,
	} {
		t.Run(name, func(t *testing.T) {
			w := newStagedCmdWorld(t)
			w.record(w.result())
			key := grantKey(w.id.RequestID, w.id.InvocationID, w.id.CandidateInstanceID, w.target)
			w.ctx.grants[key].retired = state
			if d := w.demand(); d.Met {
				t.Fatalf("%s satisfied the demand", name)
			}
		})
	}
	t.Run("no grant at all", func(t *testing.T) {
		w := newStagedCmdWorld(t)
		w.record(w.result())
		delete(w.ctx.grants, grantKey(w.id.RequestID, w.id.InvocationID,
			w.id.CandidateInstanceID, w.target))
		if d := w.demand(); d.Met {
			t.Fatal("staged evidence counted with no consumed grant")
		}
	})
}

func TestAMutationAfterDeliveryInvalidatesStagedEvidence(t *testing.T) {
	w := newStagedCmdWorld(t)
	w.record(w.result())
	if d := w.demand(); !d.Met {
		t.Fatalf("precondition: %+v", d)
	}
	body := "print('rewritten')\n"
	if err := os.WriteFile(w.target, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	observeDeliverable(w.ctx, "solve.py", []byte(body), ValidationKindSyntax, ValidationPassed, "")
	if d := w.demand(); d.Met {
		t.Fatal("evidence about the delivered bytes survived them being replaced")
	}
}

func TestALedgerThatDoesNotValidateTheseBytesFailsClosed(t *testing.T) {
	w := newStagedCmdWorld(t)
	w.record(w.result())
	w.ctx.LedgerMu.Lock()
	w.ctx.Ledger[ledgerKey(w.ctx, w.target)].ValidationStatus = ValidationUnknown
	w.ctx.LedgerMu.Unlock()
	if d := w.demand(); d.Met {
		t.Fatal("an unvalidated ledger entry satisfied the demand")
	}
}

func TestAModelCommandNotInTheContractCannotSatisfyCommandDemand(t *testing.T) {
	w := newStagedCmdWorld(t)
	// The model runs something plausible of its own invention, staged and green.
	invented, ok := newTaskObligation(ObligationDeclaredCommand, "python3 -m pytest -q", VerificationKindRuntime, true)
	if !ok {
		t.Fatal("obligation")
	}
	r := w.result()
	r.ObligationID = invented.ID
	r.CommandIdentity = contentSHA256(invented.Subject)
	recordStagedCommandFulfillment(w.ctx, invented, r, w.id)
	if d := w.demand(); d.Met {
		t.Fatal("a command the client never declared discharged the client's obligation")
	}
	if stagedCommandSatisfied(w.ctx, stagedDeclaredCommand) {
		t.Fatal("an invented command satisfied the declared one")
	}
}
