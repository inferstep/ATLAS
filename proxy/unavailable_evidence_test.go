package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// A refusal says truthfully why a candidate did not land.
//
// evidence_missing is the honest answer only when a producer was available and
// did not speak. For a sandbox that was down it would say the candidate had
// nothing to show for itself, when in truth nothing was checked. That is a
// different fact, and the one an operator needs.
//
// Nothing here adds a way to deliver unchecked. Every row below still keeps
// the caller's own content.

func TestTheUnavailableEvidenceReasonMatrix(t *testing.T) {
	const declared = `{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`
	withCommand := `{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],` +
		`"verification_knowledge":"declared","verification":["pytest -q"]}`

	for _, c := range []struct {
		name, contract string
		commands       map[string]stubEffect
		before         func(t *testing.T, w *routeWorld)
		want           AuthorizationReason
	}{
		{name: "structural producer unreachable",
			contract: declared,
			before:   func(t *testing.T, w *routeWorld) { w.ctx.SandboxURL = "http://127.0.0.1:1" },
			want:     ReasonProducerUnavailable},

		{name: "no structural producer configured",
			contract: declared,
			before:   func(t *testing.T, w *routeWorld) { w.ctx.SandboxURL = "" },
			want:     ReasonProducerUnavailable},

		{name: "declared command executor unreachable",
			contract: withCommand,
			before: func(t *testing.T, w *routeWorld) {
				// The structural gate answers; only staging cannot reach one.
				*w.shellGone = true
			},
			want: ReasonProducerUnavailable},

		{name: "declared command refused by the safety gate",
			contract: `{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],` +
				`"verification_knowledge":"declared","verification":["rm -rf /"]}`,
			want: ReasonEvidenceRefused},

		{name: "declared command timed out",
			contract: withCommand,
			commands: map[string]stubEffect{"pytest -q": {ExitCode: -1, TimedOut: true}},
			want:     ReasonEvidenceTimedOut},

		{name: "declared command ran and failed",
			contract: withCommand,
			commands: map[string]stubEffect{"pytest -q": {ExitCode: 1}},
			want:     ReasonEvidenceExecutionFailed},

		{name: "declared command rewrote the candidate",
			contract: withCommand,
			commands: map[string]stubEffect{"pytest -q": {ExitCode: 0, WriteTarget: "rewritten\n"}},
			want:     ReasonEvidenceExecutionFailed},

		{name: "declared command could not be observed",
			contract: withCommand,
			commands: map[string]stubEffect{"pytest -q": {ExitCode: 0, Truncated: true}},
			want:     ReasonProducerNotRun},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newRouteWorld(t, c.contract, c.commands)
			if c.before != nil {
				c.before(t, w)
			}
			recs := captureShadow(t, func() {
				if _, err := w.write(t); err != nil {
					t.Fatalf("write failed: %v", err)
				}
			})
			got := recordsOfKind(recs, "candidate_authorization_decision")
			if len(got) != 1 {
				t.Fatalf("%d decisions, want one", len(got))
			}
			if got[0]["reason"] != string(c.want) {
				t.Errorf("reason %v, want %q", got[0]["reason"], c.want)
			}
			if got[0]["authorized"] != false {
				t.Error("an unmet prerequisite authorized")
			}
			// Fail-closed, every row: the caller's own content survives and
			// nothing unchecked lands.
			onDisk, err := os.ReadFile(w.path)
			if err != nil {
				t.Fatal(err)
			}
			if string(onDisk) == routeWinner {
				t.Error("a candidate landed with its prerequisite unmet")
			}
			if string(onDisk) != routeBaseline {
				t.Errorf("the caller's own content did not survive: %q", string(onDisk))
			}
			if consumedGrants(recs) != 0 {
				t.Error("an unmet prerequisite spent an authorization")
			}
		})
	}
}

// TestEvidenceMissingIsReservedForAnAvailableProducer keeps the distinction
// meaningful in the other direction: when the producer WAS there and simply
// did not speak for an obligation, evidence_missing is still the right answer.
func TestEvidenceMissingIsReservedForAnAvailableProducer(t *testing.T) {
	w := newAuthWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`,
		"solve.py", authPy, true)
	ev, evID := w.mustObserve(t)
	// Staging was never run. The executor is up; nobody asked it.
	a := w.authorize(evID, nil, ev)
	if a.Decision.Reason != ReasonEvidenceMissing {
		t.Errorf("reason %q, want evidence_missing", a.Decision.Reason)
	}
}

// execution_failed is the only reason that says anything about the candidate.
// Only a command that ran and failed, or ran and rewrote the candidate, gets it.
func TestNoFailureIsDescribedAsACandidateFailure(t *testing.T) {
	for _, c := range []struct {
		outcome stagingCommandOutcome
		mutated bool
		want    AuthorizationReason
	}{
		{stagingUnavailable, false, ReasonProducerUnavailable},
		{stagingRefused, false, ReasonEvidenceRefused},
		{stagingTimedOut, false, ReasonEvidenceTimedOut},
		{stagingBudgetExceeded, false, ReasonEvidenceTimedOut},
		{stagingCancelled, false, ReasonEvidenceCancelled},
		{stagingUnobservable, false, ReasonProducerNotRun},
		{stagingExitedNonZero, false, ReasonEvidenceExecutionFailed},
		{stagingExitedZero, true, ReasonEvidenceExecutionFailed},
	} {
		got := stagingUnmetReason(stagingCommandResult{
			Outcome: c.outcome, MutatedTarget: c.mutated})
		if got != c.want {
			t.Errorf("%s (mutated %v) -> %q, want %q", c.outcome, c.mutated, got, c.want)
		}
	}
}

// An unreachable sandbox and a missing one are both not_run with the producer
// marked unavailable, and neither is a failure. A file class the gate does not
// govern owes nothing, so it reports no outage.
func TestTheStructuralOutageIsDistinctFromNotBeingAsked(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = "http://127.0.0.1:1"
	down := fallbackSyntaxOutcomeFor(ctx, dir+"/solve.py", authPy).aggregate()
	if down.Status != ValidationNotRun || !down.ProducerUnavailable {
		t.Errorf("unreachable sandbox: status=%q unavailable=%v", down.Status, down.ProducerUnavailable)
	}
	ctx.SandboxURL = ""
	none := fallbackSyntaxOutcomeFor(ctx, dir+"/solve.py", authPy).aggregate()
	if none.Status != ValidationNotRun || !none.ProducerUnavailable {
		t.Errorf("no sandbox: status=%q unavailable=%v", none.Status, none.ProducerUnavailable)
	}
	notApplicable := fallbackSyntaxOutcomeFor(ctx, dir+"/notes.md", "# notes\n").aggregate()
	if notApplicable.ProducerUnavailable {
		t.Error("a class the gate does not govern reported an outage")
	}
}

// Feasibility and authorization must agree about availability. Otherwise one
// predicts a task can close while the other refuses every candidate because
// the sandbox is down.
func TestAvailabilityReachesFeasibilityConsistently(t *testing.T) {
	with := producibleStrengthsWith(true)
	without := producibleStrengthsWith(false)
	if len(with) == 0 {
		t.Fatal("nothing is producible with the sandbox up")
	}
	if len(without) != 0 {
		t.Errorf("producible with the sandbox down: %v", without)
	}
}

func TestTruthfulReasonsCarryNoContent(t *testing.T) {
	const secret = "TOKEN = 'hunter2'\n" + routeWinner
	w := newRouteWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest --token=hunter2"]}`,
		map[string]stubEffect{"pytest --token=hunter2": {ExitCode: 1}})
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})
	decisions := recordsOfKind(recs, "candidate_authorization_decision")
	if len(decisions) != 1 {
		t.Fatalf("%d decisions, want one", len(decisions))
	}
	for _, rec := range recs {
		blob, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{
			secret, "hunter2", "TOKEN", "pytest", "--token", w.dir, "solve.py",
			"return sum",
		} {
			if strings.Contains(string(blob), needle) {
				t.Errorf("%v carries %q", rec["record_kind"], needle)
			}
		}
	}
	// The reason written is in the closed vocabulary.
	reason, _ := decisions[0]["reason"].(string)
	if !authorizationReasons[AuthorizationReason(reason)] {
		t.Errorf("reason %q is outside the closed vocabulary", reason)
	}
}
