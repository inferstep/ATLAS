package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Configuration, telemetry and provenance, audited as facts about the build
// rather than as intentions.

// Neither the model nor the service can reach the mode, and no route reads a
// mode off anything they produce.
func TestNeitherModelNorServiceSelectsTheMode(t *testing.T) {
	files := []string{"candidate_policy.go", "advisory_policy.go", "tools.go",
		"edit_route_delivery.go", "agent.go"}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		body := codeWithoutComments(string(src))
		for _, banned := range []string{
			"CandidatePolicy(v3Result", "CandidatePolicy(result",
			"CandidatePolicy(msg", "CandidatePolicy(response",
			"ParseCandidatePolicy(v3", "ParseCandidatePolicy(result",
		} {
			if strings.Contains(body, banned) {
				t.Errorf("%s derives the mode from %q", f, banned)
			}
		}
	}
	// The wire shape a service answers with carries no policy field at all.
	types, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(types)
	start := strings.Index(body, "type V3GenerateResponse struct")
	if start < 0 {
		t.Fatal("the response type moved")
	}
	resp := body[start : start+strings.Index(body[start:], "\n}")]
	for _, banned := range []string{"Policy", "candidate_policy", "Mode"} {
		if strings.Contains(resp, banned) {
			t.Errorf("the V3 response carries %q", banned)
		}
	}
}

// automatic_v3 delivers what the pipeline selected, and only that.
//
// Its own precondition -- that these are the exact bytes the selection path
// named -- is answered by the authorization owner, not here. A policy that
// decided its own authorization would be the service certifying itself with
// one more step in between.
func TestAutomaticDeliversOnlyWhatTheOwnerAuthorized(t *testing.T) {
	ctx := policyContext(t)
	in := advisoryInput{
		Observed:         checkOutcome{Status: ValidationPassed},
		TargetDeclared:   true,
		TargetAuthorized: true,
		ScopeAdmits:      true,
		Evidence: []proxyEvidence{{
			Provenance: V3EvidenceProvenance{Source: ProvenanceClientDeclaredVerification},
			Outcome:    ValidationPassed,
		}},
	}
	// Without the owner's answer it keeps the baseline: no floor was met and
	// nothing established that these bytes won anything.
	out := decideCandidatePolicy(ctx, in, false)
	if out.Decision != PolicyBaselineRetained {
		t.Fatalf("automatic decided %q with no authorization", out.Decision)
	}
	if out.Delivers {
		t.Fatal("automatic delivered without an authorization")
	}
	// With it, the candidate lands.
	in.AutomaticEligible = true
	out = decideCandidatePolicy(ctx, in, false)
	if out.Decision != PolicyCandidateAutomaticV3 {
		t.Fatalf("automatic decided %q", out.Decision)
	}
	if !out.Delivers {
		t.Fatal("an authorized automatic candidate did not deliver")
	}
	// A veto still outranks it.
	vetoed := in
	vetoed.Observed = checkOutcome{Status: ValidationFailed}
	if got := decideCandidatePolicy(ctx, vetoed, false); got.Decision != PolicyCandidateRejectedHardVeto {
		t.Errorf("a syntax failure under automatic decided %q", got.Decision)
	} else if got.Delivers {
		t.Error("a vetoed automatic candidate delivered")
	}
	// And capture-only takes the licence while keeping the answer.
	suppressed := in
	suppressed.CaptureOnlySuppressed = true
	got := decideCandidatePolicy(ctx, suppressed, false)
	if got.Decision != PolicyCandidateAutomaticV3 {
		t.Errorf("capture-only rewrote the decision to %q", got.Decision)
	}
	if got.Delivers {
		t.Error("capture-only left the licence in place")
	}
	// Delivery is set in exactly one place in the policy owner, and it is the
	// strict branch. A confirmation cannot reach it.
	src, err := os.ReadFile("advisory_policy.go")
	if err != nil {
		t.Fatal(err)
	}
	body := codeWithoutComments(string(src))
	decide := body[strings.Index(body, "func decideCandidatePolicy("):]
	if next := strings.Index(decide, "\nfunc "); next >= 0 {
		decide = decide[:next]
	}
	if n := strings.Count(decide, "out.Delivers ="); n != 1 {
		t.Fatalf("the policy owner assigns Delivers %d times, want once", n)
	}
	// The one assignment applies the acquisition control, so a delivering
	// decision cannot be added later that forgets to be suppressible.
	if !strings.Contains(decide, "out.Delivers = delivers && !in.CaptureOnlySuppressed") {
		t.Error("the delivering assignment does not apply capture-only suppression")
	}
	// And exactly two decisions may set it: the strict authorization, and the
	// automatic one the authorization owner separately approved.
	if n := strings.Count(decide, "delivers = "); n != 2 {
		t.Errorf("%d branches set delivers, want exactly the two delivering "+
			"decisions", n)
	}
	for _, delivering := range []string{
		"out.Decision, delivers = PolicyCandidateAuthorizedStrict, true",
		"out.Decision, delivers = PolicyCandidateAutomaticV3, true",
	} {
		if !strings.Contains(decide, delivering) {
			t.Errorf("a delivering decision changed shape: %s", delivering)
		}
	}
	// Automatic never decides its own eligibility here.
	if !strings.Contains(decide, "in.AutomaticEligible") {
		t.Error("the automatic branch no longer reads the owner's answer")
	}
	for _, banned := range []string{"automaticDeliveryAllowed", "mintAuthorizationGrant"} {
		if strings.Contains(decide, banned) {
			t.Errorf("the policy owner authorizes for itself via %s", banned)
		}
	}
}

// The policy record is identities and closed values. Nothing else.
func TestPolicyTelemetryCarriesNoContent(t *testing.T) {
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q --secret hunter2"]}`,
		map[string]stubEffect{"pytest -q --secret hunter2": {}}, false)
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})
	kinds := []string{"candidate_policy_decision", "candidate_mutation_scope"}
	seen := 0
	for _, kind := range kinds {
		for _, r := range recordsOfKind(recs, kind) {
			seen++
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			blob := string(raw)
			for _, secret := range []string{
				routeWinner, routeBaseline, "hunter2", "pytest", w.path,
				"Make solve fast.", w.dir,
			} {
				if secret != "" && strings.Contains(blob, secret) {
					t.Errorf("%s carries %q", kind, secret)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no policy or scope records were written")
	}
}

// With the capture off the policy still decides, writes nothing, and allocates
// nothing for a record it is not going to make.
func TestCaptureDisabledIsInert(t *testing.T) {
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		nil, false)
	before, err := os.ReadFile(w.path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	res, err := w.write(t)
	if err != nil {
		t.Fatalf("write failed with the sink off: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("the route stopped delivering with the sink off: %+v", res)
	}
	_ = before
	// The record writers return before building anything when the sink is off.
	for _, fn := range []string{"recordCandidatePolicyDecision", "recordMutationScope"} {
		src, err := os.ReadFile(map[string]string{
			"recordCandidatePolicyDecision": "candidate_policy.go",
			"recordMutationScope":           "mutation_scope.go",
		}[fn])
		if err != nil {
			t.Fatal(err)
		}
		body := string(src)
		f := body[strings.Index(body, "func "+fn+"("):]
		f = f[:strings.Index(f, "\n}")]
		guard := strings.Index(f, "if !sink.enabled() {")
		submit := strings.Index(f, "sink.submit(")
		if guard < 0 || submit < 0 || guard > submit {
			t.Errorf("%s builds its record before checking the sink", fn)
		}
	}
}

// Provenance describes the bytes that are on disk, and neither the model nor
// the service can set it.
func TestDeliveredProvenanceCannotBeForged(t *testing.T) {
	// The field is not on any wire type the model or the service fills.
	types, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(types)
	for _, typeName := range []string{"type V3GenerateResponse struct",
		"type WriteFileOutput struct", "type modelFacingResult struct"} {
		start := strings.Index(body, typeName)
		if start < 0 {
			continue
		}
		decl := body[start : start+strings.Index(body[start:], "\n}")]
		if strings.Contains(decl, "DeliveryProvenance") ||
			strings.Contains(decl, "delivery_provenance") {
			t.Errorf("%s carries the provenance", typeName)
		}
	}
	// It is set only beside a delivery that happened, from the policy outcome.
	for _, f := range []string{"tools.go", "edit_route_delivery.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(codeWithoutComments(string(src)), "\n") {
			if !strings.Contains(line, "DeliveryProvenance") {
				continue
			}
			trimmed := strings.TrimSpace(line)
			// The one setter, its own body, the field's declaration in a
			// closed helper, and the terminal's event. Nothing reads a
			// provenance off a response, a message or an envelope.
			allowed := strings.Contains(trimmed, "withDeliveryProvenance(") ||
				strings.Contains(trimmed, "deliveryProvenanceFor(") ||
				strings.Contains(trimmed, "emitDeliveryProvenance(") ||
				trimmed == "result.DeliveryProvenance = provenance" ||
				strings.Contains(trimmed, "res.DeliveryProvenance")
			if !allowed {
				t.Errorf("%s sets the provenance from %q", f, trimmed)
			}
		}
	}
	// And what it says matches what landed.
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared",`+
			`"verification_requirements_version":1,"verification_requirements":[`+
			`{"command":"pytest -q","kind":"behavioral","expects":"exit_zero",`+
			`"asset_authority":"client_supplied"}]}`,
		map[string]stubEffect{"pytest -q": {}}, false)
	res, err := w.write(t)
	if err != nil {
		t.Fatal(err)
	}
	landed := w.disk(t)
	switch res.DeliveryProvenance {
	case DeliveryFromStrictCandidate:
		if landed != routeWinner {
			t.Errorf("provenance says a candidate landed; disk holds %q", landed)
		}
	case DeliveryFromModelProposal:
		if landed != routeBaseline {
			t.Errorf("provenance says the model's bytes landed; disk holds %q", landed)
		}
	default:
		t.Errorf("unexpected provenance %q", res.DeliveryProvenance)
	}
}

// Every route that can put V3 bytes on disk is registered, and the registry is
// the only way to reach one.
func TestEveryByteToDiskRouteStaysRegistered(t *testing.T) {
	for _, site := range []string{
		"tools.go:writeFileWithV3",
		"edit_route_delivery.go:deliverEditCandidate",
		"candidate_delivery.go:deliverAuthorizedCandidate",
	} {
		if !registeredV3DeliveryRoute(site) {
			t.Errorf("%s is not a registered delivery route", site)
		}
	}
	if registeredV3DeliveryRoute("tools.go:somethingElse") {
		t.Error("an unregistered site was accepted")
	}
}
