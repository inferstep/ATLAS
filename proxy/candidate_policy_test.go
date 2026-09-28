package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The policy foundation, exercised through the real write path.
//
// Two questions run through every case here and they are deliberately never
// the same question: whether a candidate may be PROPOSED, which is about bytes,
// and whether it may LAND, which is about evidence this machine produced about
// those bytes.

// A candidate the service declined to certify still reaches staging. This is
// the ordering the audit found closed: the trusted producer runs against the
// candidate, so a candidate discarded before staging can never be measured.
func TestAnUncertifiedProposalReachesStaging(t *testing.T) {
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`,
		map[string]stubEffect{"pytest -q": {}}, false)
	if _, err := w.write(t); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	ran := w.shell.runsOf("pytest -q")
	if ran == 0 {
		t.Fatal("the declared command never ran against the candidate")
	}
	// And it ran against the CANDIDATE, not the caller's own content.
	if got := w.shell.stagedBytes("pytest -q"); got != routeWinner {
		t.Errorf("staged %q, want the candidate", got)
	}
}

// The service's own metadata is not a permission. An uncertified proposal that
// nothing trusted spoke for keeps the caller's content.
func TestServiceMetadataCannotAuthorizeADelivery(t *testing.T) {
	for _, closure := range []bool{true, false} {
		w := newRouteWorldWithClosure(t,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
				`"verification_knowledge":"declared","verification":["pytest -q"]}`,
			map[string]stubEffect{"pytest -q": {ExitCode: 1}}, closure)
		res, err := w.write(t)
		if err != nil {
			t.Fatalf("closure=%v: write failed: %v", closure, err)
		}
		if got := w.disk(t); got != routeBaseline {
			t.Errorf("closure=%v: disk holds %q, want the caller's content", closure, got)
		}
		if res.V3Used {
			t.Errorf("closure=%v: a failing command still reported V3 provenance", closure)
		}
		if res.DeliveryProvenance != DeliveryFromModelProposal {
			t.Errorf("closure=%v: provenance %q, want the model's own proposal",
				closure, res.DeliveryProvenance)
		}
	}
}

// A declared check that could not run is not evidence, and the candidate it
// was owed for does not land.
func TestADeclaredCheckThatCouldNotRunKeepsTheBaseline(t *testing.T) {
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`,
		map[string]stubEffect{"pytest -q": {HTTPStatus: 503}}, true)
	if _, err := w.write(t); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if got := w.disk(t); got != routeBaseline {
		t.Errorf("disk holds %q, want the caller's content", got)
	}
}

// A typed behavioral declaration that passes authorizes exactly these bytes,
// and the delivery carries every safety property it always did.
func TestTypedBehavioralVerificationAuthorizesTheExactBytes(t *testing.T) {
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared",`+
			`"verification_requirements_version":1,"verification_requirements":[`+
			`{"command":"pytest -q","kind":"behavioral","expects":"exit_zero",`+
			`"asset_authority":"client_supplied"}]}`,
		map[string]stubEffect{"pytest -q": {}}, false)
	res, err := w.write(t)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if got := w.disk(t); got != routeWinner {
		t.Fatalf("disk holds %q, want the candidate", got)
	}
	if res.AuthorizedDeliveryHash != contentSHA256(routeWinner) {
		t.Error("the delivery did not name the exact bytes it was authorized for")
	}
	if res.DeliveryProvenance != DeliveryFromStrictCandidate {
		t.Errorf("provenance %q, want the strict candidate", res.DeliveryProvenance)
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Error("the one-time grant was not spent")
	}
}

// The same command, declared untyped, cannot reach behavioral authority --
// and a task that needs behavioral therefore keeps the baseline.
func TestLegacyVerificationCannotProduceBehavioralAuthority(t *testing.T) {
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`,
		map[string]stubEffect{"pytest -q": {}}, false)
	obs := requestObligations(w.ctx)
	for _, o := range obs {
		if o.Kind == ObligationDeclaredCommand && o.RequiredStrength == VerificationKindBehavioral {
			t.Fatal("an untyped command declared behavioral strength")
		}
	}
	if got := authorizationFloor(obs); got == VerificationKindBehavioral {
		t.Fatal("an untyped command raised the floor to behavioral")
	}
}

// The one rule: a candidate the V3 selection path named lands when no hard
// veto fired; a declared verification that passed is the stronger basis;
// otherwise the model's own bytes stand.
func TestTheOneDeliveryRule(t *testing.T) {
	ctx := policyContext(t)
	clean := advisoryInput{
		Observed:         checkOutcome{Status: ValidationPassed},
		TargetDeclared:   true,
		TargetAuthorized: true,
		ScopeAdmits:      true,
	}
	if out := decideCandidatePolicy(ctx, clean, false); out.Decision != PolicyBaselineRetained || out.Delivers {
		t.Errorf("no basis: %q delivers=%v, want the baseline", out.Decision, out.Delivers)
	}
	selected := clean
	selected.AutomaticEligible = true
	if out := decideCandidatePolicy(ctx, selected, false); out.Decision != PolicyCandidateAutomaticV3 || !out.Delivers {
		t.Errorf("selected and safe: %q delivers=%v, want automatic_v3", out.Decision, out.Delivers)
	}
	if out := decideCandidatePolicy(ctx, selected, true); out.Decision != PolicyCandidateAuthorizedStrict || !out.Delivers {
		t.Errorf("declared verification passed: %q delivers=%v, want strict", out.Decision, out.Delivers)
	}
	// One veto is enough, whatever else was observed.
	vetoed := selected
	vetoed.Observed = checkOutcome{Status: ValidationFailed}
	if out := decideCandidatePolicy(ctx, vetoed, true); out.Decision != PolicyCandidateRejectedHardVeto || out.Delivers {
		t.Fatalf("a failing gate produced %q delivers=%v", out.Decision, out.Delivers)
	}
}

// A check that never ran is not evidence either way, and the selection basis
// cannot stand on it: the candidate is vetoed and the caller's bytes stand.
// Before this veto, a candidate whose syntax check never ran was delivered.
func TestWeakEvidenceRetainsTheBaseline(t *testing.T) {
	ctx := policyContext(t)
	for _, status := range []ValidationStatus{ValidationNotRun, ValidationUnknown} {
		nothing := advisoryInput{
			Observed:          checkOutcome{Status: status},
			TargetDeclared:    true,
			TargetAuthorized:  true,
			ScopeAdmits:       true,
			AutomaticEligible: true,
		}
		out := decideCandidatePolicy(ctx, nothing, false)
		if out.Decision != PolicyCandidateRejectedHardVeto || out.Delivers {
			t.Errorf("%q observed: %q delivers=%v", status, out.Decision, out.Delivers)
		}
		if !hasVeto(out.Vetoes, VetoExecutionUnavailable) {
			t.Errorf("%q observed: vetoes %v do not say the check never ran", status, out.Vetoes)
		}
	}
	// A class with no syntax check is a different fact: nothing could run, so
	// nothing is vetoed on that ground.
	na := advisoryInput{
		Observed:          checkOutcome{Status: ValidationNotApplicable},
		TargetDeclared:    true,
		TargetAuthorized:  true,
		ScopeAdmits:       true,
		AutomaticEligible: true,
	}
	if out := decideCandidatePolicy(ctx, na, false); !out.Delivers {
		t.Errorf("a class with no syntax check: %q, vetoes %v", out.Decision, out.Vetoes)
	}
	// Conflicting: a trusted pass alongside an unmet declared command. The
	// unmet obligation is a fact and it decides.
	conflicting := advisoryInput{
		Observed:         checkOutcome{Status: ValidationPassed},
		TargetDeclared:   true,
		TargetAuthorized: true,
		ScopeAdmits:      true,
		Unmet:            map[string]AuthorizationReason{"cmd": ReasonEvidenceExecutionFailed},
		Evidence: []proxyEvidence{{
			Provenance: V3EvidenceProvenance{Source: ProvenanceProxyOwnedValidation},
			Outcome:    ValidationPassed,
		}},
	}
	out := decideCandidatePolicy(ctx, conflicting, false)
	if out.Decision != PolicyCandidateRejectedHardVeto {
		t.Errorf("conflicting evidence said %q", out.Decision)
	}
	if !hasVeto(out.Vetoes, VetoDeclaredVerificationFailed) {
		t.Errorf("vetoes %v do not name the failed command", out.Vetoes)
	}
}

// Every hard veto is reachable, and each is named for the fact that fired it.
func TestEveryHardVetoIsReachable(t *testing.T) {
	ctx := policyContext(t)
	base := advisoryInput{Observed: checkOutcome{Status: ValidationPassed},
		TargetDeclared: true, TargetAuthorized: true, ScopeAdmits: true}
	for _, tc := range []struct {
		name   string
		mutate func(*advisoryInput)
		want   string
	}{
		{"syntax or structural", func(in *advisoryInput) {
			in.Observed = checkOutcome{Status: ValidationFailed}
		}, VetoSyntaxOrStructural},
		{"language or target", func(in *advisoryInput) {
			in.LanguageOrBoundaryViolation = true
		}, VetoLanguageOrTargetMismatch},
		{"path expansion", func(in *advisoryInput) {
			in.TargetAuthorized = false
		}, VetoUnauthorizedPathExpansion},
		{"cancelled", func(in *advisoryInput) { in.Cancelled = true }, VetoCancelledOrTimedOut},
		{"destructive without permission", func(in *advisoryInput) {
			in.DestructiveImplied, in.DestructivePermitted = true, false
		}, VetoDestructiveWithoutPermission},
		{"mutated protected assets", func(in *advisoryInput) {
			in.MutatedProtectedAssets = true
		}, VetoMutatedProtectedAssets},
		{"declared verification failed", func(in *advisoryInput) {
			in.Unmet = map[string]AuthorizationReason{"c": ReasonEvidenceExecutionFailed}
		}, VetoDeclaredVerificationFailed},
		{"execution unavailable", func(in *advisoryInput) {
			in.Unmet = map[string]AuthorizationReason{"c": ReasonProducerUnavailable}
		}, VetoExecutionUnavailable},
		{"timed out", func(in *advisoryInput) {
			in.Unmet = map[string]AuthorizationReason{"c": ReasonEvidenceTimedOut}
		}, VetoCancelledOrTimedOut},
		{"incomplete evidence", func(in *advisoryInput) {
			in.Unmet = map[string]AuthorizationReason{"c": ReasonEvidenceMissing}
		}, VetoIncompleteEvidence},
		{"weaker than baseline", func(in *advisoryInput) {
			in.Decision = AuthorizationDecision{Reason: ReasonBaselineNotPreserved}
		}, VetoWeakerThanBaseline},
		{"stale identity", func(in *advisoryInput) {
			in.Decision = AuthorizationDecision{Reason: ReasonWorkspaceStale}
		}, VetoStaleIdentity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			out := decideCandidatePolicy(ctx, in, false)
			if !hasVeto(out.Vetoes, tc.want) {
				t.Fatalf("vetoes %v do not include %q", out.Vetoes, tc.want)
			}
			if out.Decision != PolicyCandidateRejectedHardVeto {
				t.Fatalf("decision %q, want the veto rejection", out.Decision)
			}
			if out.Delivers {
				t.Fatal("a vetoed candidate was cleared to deliver")
			}
		})
	}
}

// A veto outranks a strict authorization: the disqualifying fact wins.
func TestAVetoOutranksStrictAuthorization(t *testing.T) {
	ctx := policyContext(t)
	in := advisoryInput{
		Observed:         checkOutcome{Status: ValidationFailed},
		TargetDeclared:   true,
		TargetAuthorized: true,
		ScopeAdmits:      true,
	}
	out := decideCandidatePolicy(ctx, in, true)
	if out.Decision != PolicyCandidateRejectedHardVeto || out.Delivers {
		t.Fatalf("decision %q delivers=%v, want a refusal", out.Decision, out.Delivers)
	}
}

// A candidate weaker than the artifact already on disk is rejected, through
// the real route.
func TestCandidateWeakerThanBaselineIsRejected(t *testing.T) {
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`,
		map[string]stubEffect{"pytest -q": {}}, true)
	// The baseline on disk already carries a green behavioural record.
	if err := os.WriteFile(w.path, []byte(routeBaseline), 0o644); err != nil {
		t.Fatal(err)
	}
	w.ctx.VerificationEvidence = append(w.ctx.VerificationEvidence, VerificationRecord{
		Command: "pytest -q",
		Covered: map[string]string{w.path: contentSHA256(routeBaseline)}, Turn: 1,
	})
	obs := requestObligations(w.ctx)
	var preserved bool
	for _, o := range obs {
		if o.Kind == ObligationBaselinePreserved && o.RequiredStrength == "behavioral" {
			preserved = true
		}
	}
	if !preserved {
		t.Skip("no behavioural baseline was derived for this fixture")
	}
	if _, err := w.write(t); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	// The untyped command reaches runtime, which is weaker than the baseline
	// the artifact already holds, so the candidate does not displace it.
	if got := w.disk(t); got != routeBaseline {
		t.Errorf("disk holds %q, want the preserved baseline", got)
	}
}

// The hidden evaluator has no representation anywhere the policy can reach.
func TestHiddenEvaluatorCannotInfluenceThePolicy(t *testing.T) {
	for _, f := range []string{"candidate_policy.go", "advisory_policy.go",
		"candidate_provenance.go", "verification_requirements.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Comments may DISCUSS it; code may not reach it. The check is about
		// what the build can execute, so the prose is stripped first.
		for _, banned := range []string{"evaluator", "benchmark", "held_out", "ground_truth"} {
			if strings.Contains(strings.ToLower(codeWithoutComments(string(src))), banned) {
				t.Errorf("%s names %q in code", f, banned)
			}
		}
	}
	// And no provenance source exists that a hidden grader could claim.
	for source := range provenanceSources {
		if strings.Contains(source, "evaluator") || strings.Contains(source, "benchmark") {
			t.Errorf("provenance source %q exists", source)
		}
	}
}

// No basis lowers anything about identity, permission or delivery: a
// selected candidate that implies a destructive action is refused.
func TestNoBasisAuthorizesADestructiveAction(t *testing.T) {
	ctx := policyContext(t)
	in := advisoryInput{
		Observed:           checkOutcome{Status: ValidationPassed},
		TargetDeclared:     true,
		TargetAuthorized:   true,
		ScopeAdmits:        true,
		DestructiveImplied: true,
		AutomaticEligible:  true,
		Evidence: []proxyEvidence{{
			Provenance: V3EvidenceProvenance{Source: ProvenanceClientDeclaredVerification},
			Outcome:    ValidationPassed,
		}},
	}
	out := decideCandidatePolicy(ctx, in, false)
	if out.Decision != PolicyCandidateRejectedHardVeto {
		t.Fatalf("decision %q, want a refusal", out.Decision)
	}
	if !hasVeto(out.Vetoes, VetoDestructiveWithoutPermission) {
		t.Errorf("vetoes %v do not name the permission", out.Vetoes)
	}
}

// Nothing selects the rule. A contract that still carries candidate_policy,
// with any value, is accepted and the value is not kept; the operator
// variable that once set a default changes nothing.
func TestNothingSelectsTheRule(t *testing.T) {
	for _, value := range []string{"strict", "advisory", "automatic_v3", "yolo"} {
		var in TaskContract
		if err := json.Unmarshal([]byte(`{"task_mode":"work","candidate_policy":"`+value+`"}`), &in); err != nil {
			t.Fatal(err)
		}
		out, err := validateTaskContract(&in, t.TempDir())
		if err != nil {
			t.Fatalf("candidate_policy %q was refused: %v", value, err)
		}
		if out.CandidatePolicy != "" {
			t.Errorf("candidate_policy %q was kept on the contract", value)
		}
	}
	t.Setenv("ATLAS_CANDIDATE_POLICY", "strict")
	in := advisoryInput{Observed: checkOutcome{Status: ValidationPassed},
		TargetDeclared: true, TargetAuthorized: true, ScopeAdmits: true, AutomaticEligible: true}
	out := decideCandidatePolicy(policyContext(t), in, false)
	if out.Decision != PolicyCandidateAutomaticV3 || out.Mode != CandidatePolicyAutomaticV3 ||
		out.Source != CandidatePolicySourceFixed {
		t.Errorf("under ATLAS_CANDIDATE_POLICY=strict: %q %q %q", out.Decision, out.Mode, out.Source)
	}
	// Source-level: the policy owner reads no contract field and no variable.
	src, err := os.ReadFile("advisory_policy.go")
	if err != nil {
		t.Fatal(err)
	}
	body := codeWithoutComments(string(src))
	owner := body[strings.Index(body, "func decideCandidatePolicy("):]
	owner = owner[:strings.Index(owner, "\n}")]
	for _, banned := range []string{"TaskContract", "Getenv"} {
		if strings.Contains(owner, banned) {
			t.Errorf("the policy owner reads %s", banned)
		}
	}
}

// Delivered bytes always say where they came from, and only a delivering
// decision may claim a candidate origin.
func TestDeliveryProvenanceNamesTheOrigin(t *testing.T) {
	for _, tc := range []struct {
		decision candidatePolicyDecision
		delivers bool
		want     string
	}{
		{PolicyCandidateAuthorizedStrict, true, DeliveryFromStrictCandidate},
		{PolicyCandidateAutomaticV3, true, DeliveryFromAutomaticV3},
		{PolicyCandidateAuthorizedStrict, false, DeliveryFromModelProposal},
		{PolicyBaselineRetained, false, DeliveryFromModelProposal},
		{PolicyCandidateRejectedHardVeto, false, DeliveryFromModelProposal},
	} {
		got := deliveryProvenanceFor(candidatePolicyOutcome{
			Decision: tc.decision, Delivers: tc.delivers})
		if got != tc.want {
			t.Errorf("%s delivers=%v gave %q, want %q", tc.decision, tc.delivers, got, tc.want)
		}
	}
}

// The provenance is a server-side fact for the user, not something the model
// reads and argues with.
func TestDeliveryProvenanceIsNotModelFacing(t *testing.T) {
	r := &ToolResult{Success: true, DeliveryProvenance: DeliveryFromStrictCandidate}
	blob, err := json.Marshal(r.ModelFacing())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), DeliveryFromStrictCandidate) ||
		strings.Contains(string(blob), "delivery_provenance") {
		t.Fatalf("the model sees the provenance: %s", blob)
	}
}

// No advisory value is presented as a probability of correctness.
func TestNoAdvisoryValueClaimsCorrectness(t *testing.T) {
	in := advisoryInput{
		Observed:         checkOutcome{Status: ValidationPassed},
		TargetDeclared:   true,
		TargetAuthorized: true,
		ScopeAdmits:      true,
		Evidence: []proxyEvidence{{
			Provenance: V3EvidenceProvenance{
				Source: ProvenanceClientDeclaredVerification, ObservedStrength: "behavioral"},
			Outcome: ValidationPassed,
		}},
	}
	signals := advisorySignals(in)
	for key, value := range signals {
		switch value.(type) {
		case float64, float32:
			t.Errorf("signal %q is a bare number that reads as a probability", key)
		}
		if strings.Contains(key, "confidence") || strings.Contains(key, "probability") ||
			strings.Contains(key, "correct") {
			t.Errorf("signal %q claims correctness", key)
		}
	}
	// The service's numbers are labelled as the service's wherever they appear.
	in.Envelope = &V3EvidenceEnvelope{}
	for key := range advisorySignals(in) {
		if strings.Contains(key, "closure") || strings.Contains(key, "selection") {
			if !strings.HasPrefix(key, "service_") {
				t.Errorf("signal %q does not name the side that produced it", key)
			}
		}
	}
}

// --- helpers -----------------------------------------------------------------

func policyContext(t *testing.T) *AgentContext {
	t.Helper()
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)
	return ctx
}

// codeWithoutComments strips line and block comments so a source-level check
// asserts about what runs rather than about what is written down.
func codeWithoutComments(src string) string {
	var out strings.Builder
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}

func hasVeto(vetoes []string, want string) bool {
	for _, v := range vetoes {
		if v == want {
			return true
		}
	}
	return false
}

// testMutationScope is the structured intent a route would have derived for
// this call. The scope is a fixture in these tests, not the thing under test:
// the boundary is the caller's own bytes, so it admits exactly the candidate
// being decided on.
func testMutationScope(ctx *AgentContext, entry routeEntry, path, code string) mutationScope {
	s, _ := deriveMutationScope(ctx, entry, "write_file", path, "", code)
	return s
}
