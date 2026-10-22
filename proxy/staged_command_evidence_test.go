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

// newStagedCmdWorld builds a request that declared solve.py and `pytest`, with
// solve.py delivered and its one-time grant spent, as delivery leaves it.
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
	id := stagingIdentity{
		RequestID: "req-staged", InvocationID: "req-staged:inv:1",
		CandidateInstanceID: "req-staged:inv:1:cand", CandidateHash: hash,
		TargetPath: target, WorkspaceGeneration: generation, WorkspaceStateHash: state,
	}
	key := grantKey(id.RequestID, id.InvocationID, id.CandidateInstanceID, target)
	if ctx.grants == nil {
		ctx.grants = map[string]*authorizationGrant{}
	}
	ctx.grants[key] = &authorizationGrant{
		ID: key, RequestID: id.RequestID, InvocationID: id.InvocationID,
		CandidateInstanceID: id.CandidateInstanceID, CandidateHash: hash,
		TargetPath: target, retired: grantConsumed,
	}
	return &stagedCmdWorld{ctx: ctx, dir: dir, target: target, hash: hash, id: id, ob: ob, tc: tc}
}

// result is a clean staged run of the declared command over the delivered
// bytes.
func (w *stagedCmdWorld) result() stagingCommandResult {
	return stagingCommandResult{
		CommandIdentity: contentSHA256(stagedDeclaredCommand), ObligationID: w.ob.ID,
		Outcome: stagingExitedZero, TargetHashBefore: w.hash, TargetHashAfter: w.hash,
		WorkspaceHashBefore: "ws", WorkspaceHashAfter: "ws",
	}
}

// --- the path that should work ----------------------------------------------

// A clean staged run meets the command and the coverage demand. It needs no
// settlement record, which cannot exist yet when the gate asks.
func TestStagedCommandOverExactBytesClearsBothDemands(t *testing.T) {
	w := newStagedCmdWorld(t)
	recordStagedCommandFulfillment(w.ctx, w.ob, w.result(), w.id)
	if len(w.ctx.StagedCommands) != 1 {
		t.Fatalf("nothing was recorded: %+v", w.ctx.StagedCommands)
	}
	if deliverySettlementFor(w.ctx, w.target) != nil {
		t.Fatal("precondition: no settlement record should exist yet")
	}
	if d := decideVerificationDemand(w.ctx, w.tc, []string{"solve.py"}); !d.Met {
		t.Fatalf("staged evidence carries both bindings, got %+v", d)
	}
	if !stagedCommandSatisfied(w.ctx, stagedDeclaredCommand) {
		t.Error("command obligation not satisfied")
	}
	if !stagedCoverageSatisfied(w.ctx, w.target, w.hash) {
		t.Error("path coverage not satisfied")
	}
}

// --- every binding, one at a time -------------------------------------------

func TestStagedEvidenceFailsClosedOnEveryBrokenBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(w *stagedCmdWorld, r *stagingCommandResult)
	}{
		{"candidate hash mismatch", func(w *stagedCmdWorld, r *stagingCommandResult) {
			w.id.CandidateHash = contentSHA256("other bytes\n")
		}},
		{"changed target during staging", func(w *stagedCmdWorld, r *stagingCommandResult) {
			r.MutatedTarget = true
		}},
		{"changed workspace during staging", func(w *stagedCmdWorld, r *stagingCommandResult) {
			r.MutatedWorkspace = true
		}},
		{"wrong obligation", func(w *stagedCmdWorld, r *stagingCommandResult) {
			r.ObligationID = "declared_command:0000"
		}},
		{"wrong command identity", func(w *stagedCmdWorld, r *stagingCommandResult) {
			r.CommandIdentity = contentSHA256("make check")
		}},
		{"staged against other bytes", func(w *stagedCmdWorld, r *stagingCommandResult) {
			r.TargetHashBefore = contentSHA256("something else\n")
		}},
		{"incomplete identity", func(w *stagedCmdWorld, r *stagingCommandResult) {
			w.id.CandidateInstanceID = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newStagedCmdWorld(t)
			r := w.result()
			tc.break_(w, &r)
			recordStagedCommandFulfillment(w.ctx, w.ob, r, w.id)
			if len(w.ctx.StagedCommands) != 0 {
				t.Fatal("the run was admitted")
			}
			if d := decideVerificationDemand(w.ctx, w.tc, []string{"solve.py"}); d.Met {
				t.Fatalf("the run satisfied the demand: %+v", d)
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
		recordStagedCommandFulfillment(w.ctx, w.ob, r, w.id)
		if len(w.ctx.StagedCommands) != 0 {
			t.Errorf("%q was admitted as evidence", outcome)
		}
	}
}

// A clean staged run was recorded; then one thing changes. The evidence no
// longer meets the demand.
func TestStagedEvidenceStopsCountingWhenItsContextChanges(t *testing.T) {
	setGrant := func(w *stagedCmdWorld, state grantRetirement) {
		w.ctx.grants[grantKey(w.id.RequestID, w.id.InvocationID, w.id.CandidateInstanceID, w.target)].retired = state
	}
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, w *stagedCmdWorld)
	}{
		{"another request", func(t *testing.T, w *stagedCmdWorld) {
			w.ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-other")
		}},
		{"another invocation", func(t *testing.T, w *stagedCmdWorld) {
			w.ctx.StagedCommands[0].InvocationID = "req-staged:inv:2"
		}},
		{"another candidate instance", func(t *testing.T, w *stagedCmdWorld) {
			w.ctx.StagedCommands[0].CandidateInstanceID = "other"
		}},
		{"another target", func(t *testing.T, w *stagedCmdWorld) {
			w.ctx.StagedCommands[0].TargetPath = filepath.Join(w.dir, "other.py")
		}},
		{"grant never consumed", func(t *testing.T, w *stagedCmdWorld) { setGrant(w, grantLive) }},
		{"grant only attempted", func(t *testing.T, w *stagedCmdWorld) { setGrant(w, grantAttempted) }},
		{"grant cancelled", func(t *testing.T, w *stagedCmdWorld) { setGrant(w, grantCancelled) }},
		{"no grant at all", func(t *testing.T, w *stagedCmdWorld) {
			delete(w.ctx.grants, grantKey(w.id.RequestID, w.id.InvocationID, w.id.CandidateInstanceID, w.target))
		}},
		{"the delivered bytes replaced after delivery", func(t *testing.T, w *stagedCmdWorld) {
			body := "print('rewritten')\n"
			if err := os.WriteFile(w.target, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			observeDeliverable(w.ctx, "solve.py", []byte(body), ValidationKindSyntax, ValidationPassed, "")
		}},
		{"a ledger that does not validate these bytes", func(t *testing.T, w *stagedCmdWorld) {
			w.ctx.LedgerMu.Lock()
			w.ctx.Ledger[ledgerKey(w.ctx, w.target)].ValidationStatus = ValidationUnknown
			w.ctx.LedgerMu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newStagedCmdWorld(t)
			recordStagedCommandFulfillment(w.ctx, w.ob, w.result(), w.id)
			tc.change(t, w)
			if d := decideVerificationDemand(w.ctx, w.tc, []string{"solve.py"}); d.Met {
				t.Fatalf("the evidence still satisfied the demand: %+v", d)
			}
		})
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
	if d := decideVerificationDemand(w.ctx, w.tc, []string{"solve.py"}); d.Met {
		t.Fatal("a command the client never declared discharged the client's obligation")
	}
	if stagedCommandSatisfied(w.ctx, stagedDeclaredCommand) {
		t.Fatal("an invented command satisfied the declared one")
	}
}
