package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Bounds, pinned.
//
// The failure mode a preference policy invites is not a wrong answer, it is an
// endless search for a better one: another candidate, another refinement, one
// more round until something clears the bar. Every ceiling that stops that is
// asserted here, and the last two are the ones that matter most -- a rejected
// candidate costs the caller nothing extra, and the route ends exactly once
// whatever the policy decided.

// Every bound the candidate path runs under is a value, not a hope.
func TestCandidatePathBoundsAreFinite(t *testing.T) {
	if d := v3CallTimeout(); d <= 0 || d > 10*time.Minute {
		t.Errorf("the V3 call timeout is %v", d)
	}
	b := defaultStagingBudget()
	if b.MaxCommands <= 0 || b.MaxCommands > maxTaskContractEntries {
		t.Errorf("staging command ceiling is %d", b.MaxCommands)
	}
	if b.PerCommandTimeoutSec <= 0 || b.TotalTimeoutSec <= 0 {
		t.Errorf("staging time budget is %+v", b)
	}
	if b.TotalTimeoutSec > 600 {
		t.Errorf("the staging set may run for %ds", b.TotalTimeoutSec)
	}
	if b.MaxCandidates <= 0 {
		t.Errorf("staging candidate ceiling is %d", b.MaxCandidates)
	}
	// A declared set the budget cannot hold whole runs nothing at all, rather
	// than running a prefix of it and calling the rest satisfied.
	if ok, _ := (stagingBudget{MaxCommands: 0}).validate(); ok {
		t.Error("a zero command budget validated")
	}
}

// A rejected candidate costs the caller no extra model turn. The route answers
// the one tool call it was given, with the caller's own content.
func TestRejectionAddsNoModelTurn(t *testing.T) {
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`,
		map[string]stubEffect{"pytest -q": {ExitCode: 1}}, false)
	res, err := w.write(t)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("the caller's own content was not written: %+v", res)
	}
	// One generation call, whatever the policy concluded. A policy that asked
	// for another candidate after a refusal is the unbounded search this
	// pins shut.
	if got := w.generateCalls(); got != 1 {
		t.Errorf("the pipeline was asked %d times for a rejected candidate", got)
	}
	// And the declared command ran once, not once per attempt.
	if got := w.shell.runsOf("pytest -q"); got != 1 {
		t.Errorf("the declared command ran %d times", got)
	}
}

// The same proposal arriving again is one route entry per attempt with one
// ending each, and no accumulation of live grants.
func TestRepeatedProposalsStayBounded(t *testing.T) {
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`,
		map[string]stubEffect{"pytest -q": {ExitCode: 1}}, false)
	for i := 0; i < 5; i++ {
		if _, err := w.write(t); err != nil {
			t.Fatalf("attempt %d failed: %v", i, err)
		}
	}
	if got := w.generateCalls(); got != 5 {
		t.Errorf("five attempts made %d generation calls", got)
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Error("repeated refusals accumulated live grants")
	}
	if got := w.disk(t); got != routeBaseline {
		t.Errorf("disk holds %q after five refusals", got)
	}
}

// Cancellation reaches the policy as a fact, and a cancelled request prefers
// nothing.
func TestCancellationStopsThePolicy(t *testing.T) {
	ctx := policyContext(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	ctx.Ctx = cancelled
	out := decideCandidatePolicy(ctx, advisoryInput{
		Observed:         checkOutcome{Status: ValidationPassed},
		TargetDeclared:   true,
		TargetAuthorized: true,
		ScopeAdmits:      true,
		Cancelled:        ctx.Ctx.Err() != nil,
		Evidence: []proxyEvidence{{
			Provenance: V3EvidenceProvenance{Source: ProvenanceClientDeclaredVerification},
			Outcome:    ValidationPassed,
		}},
	}, false)
	if out.Decision != PolicyCandidateRejectedHardVeto {
		t.Fatalf("a cancelled request decided %q", out.Decision)
	}
	if !hasVeto(out.Vetoes, VetoCancelledOrTimedOut) {
		t.Errorf("vetoes %v do not name the cancellation", out.Vetoes)
	}
}

// When the budget is gone, the baseline is retained and the answer says so.
// Nothing here waits for certainty.
func TestExhaustedBudgetRetainsTheBaselineTruthfully(t *testing.T) {
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`,
		map[string]stubEffect{"pytest -q": {TimedOut: true}}, false)
	res, err := w.write(t)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if got := w.disk(t); got != routeBaseline {
		t.Errorf("disk holds %q, want the caller's own content", got)
	}
	if res.V3Used {
		t.Error("a timed-out verification still claimed V3 provenance")
	}
	if res.DeliveryProvenance != DeliveryFromModelProposal {
		t.Errorf("provenance %q after a timeout", res.DeliveryProvenance)
	}
}

// The policy owner has no loop of its own: it reads facts and returns one
// answer. A source-level check, because a retry added here would be invisible
// to every behavioural test that passes a single input.
func TestThePolicyOwnerDoesNotIterate(t *testing.T) {
	src, err := os.ReadFile("advisory_policy.go")
	if err != nil {
		t.Fatal(err)
	}
	body := codeWithoutComments(string(src))
	decide := body[strings.Index(body, "func decideCandidatePolicy("):]
	decide = decide[:strings.Index(decide, "\n}")]
	for _, banned := range []string{"for ", "go ", "time.Sleep", "retry", "Retry"} {
		if strings.Contains(decide, banned) {
			t.Errorf("the policy owner contains %q", banned)
		}
	}
	// And it calls nothing that could generate: no pipeline, no model, no
	// second opinion.
	for _, banned := range []string{"callV3", "improveContentWithV3", "stageCandidate",
		"http.", "exec."} {
		if strings.Contains(decide, banned) {
			t.Errorf("the policy owner reaches %q", banned)
		}
	}
}

// Advisory changes what a candidate must show. It changes nothing about how a
// candidate that IS chosen gets to disk.
func TestAdvisoryDoesNotWeakenDeliverySafety(t *testing.T) {
	src, err := os.ReadFile("candidate_delivery.go")
	if err != nil {
		t.Fatal(err)
	}
	body := codeWithoutComments(string(src))
	for _, required := range []string{"consumeAuthorizationGrant", "contentSHA256",
		"recordDeliverySettlement"} {
		if !strings.Contains(body, required) {
			t.Errorf("the delivery owner no longer calls %s", required)
		}
	}
	// The policy is not consulted inside the delivery owner: by the time bytes
	// move, the decision has already been made and spent as a grant.
	for _, banned := range []string{"decideCandidatePolicy", "advisoryInput",
		"candidatePolicyOf"} {
		if strings.Contains(body, banned) {
			t.Errorf("the delivery owner re-decides the policy via %s", banned)
		}
	}
}
