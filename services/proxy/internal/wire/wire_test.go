package wire

import (
	"strings"
	"testing"
)

func TestOnlyOneOutcomeMeansTheCommandFinished(t *testing.T) {
	for _, o := range []ExecutionOutcome{
		ExecutionCompleted, ExecutionTimedOut, ExecutionMemoryExhausted,
		ExecutionProcessLimit, ExecutionOutputLimit, ExecutionCancelled,
		ExecutionSpawnFailed, ExecutionUnclassified,
	} {
		if !knownExecutionOutcome(o) {
			t.Errorf("%q is not in the closed set", o)
		}
		if CommandCompleted(o) != (o == ExecutionCompleted) {
			t.Errorf("%q answers the completion question wrongly", o)
		}
	}
	if knownExecutionOutcome("invented") {
		t.Error("an invented outcome was accepted")
	}
	// An executor that has not been taught this vocabulary, and one that grew
	// a member this build has not been taught, both fail closed.
	for _, raw := range []string{"", "invented", "COMPLETED", "completed "} {
		if got := CanonicalExecutionOutcome(raw); got != ExecutionUnclassified {
			t.Errorf("%q canonicalised to %q, want unclassified", raw, got)
		}
	}
	if CanonicalExecutionOutcome("completed") != ExecutionCompleted {
		t.Error("the one completion member does not survive canonicalisation")
	}
}

func TestProvenanceSourcesAreClosedAndSourceSpecific(t *testing.T) {
	want := []string{
		ProvenanceRepoOwnedCheck, ProvenanceClientDeclaredExample,
		ProvenanceClientDeclaredVerification, ProvenanceProxyOwnedValidation,
		ProvenanceModelGenerated, ProvenanceLegacy, ProvenanceUnknown,
	}
	if len(provenanceSources) != len(want) {
		t.Fatalf("%d sources, want %d", len(provenanceSources), len(want))
	}
	for _, s := range want {
		if !provenanceSources[s] {
			t.Errorf("%q missing from the closed vocabulary", s)
		}
	}
}

func TestTheHiddenEvaluatorHasNoWireRepresentation(t *testing.T) {
	for s := range provenanceSources {
		low := strings.ToLower(s)
		for _, banned := range []string{"hidden", "holdout", "benchmark", "evaluator"} {
			if strings.Contains(low, banned) {
				t.Errorf("source %q names an evaluator", s)
			}
		}
	}
}
