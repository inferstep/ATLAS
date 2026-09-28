package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The wire half of evidence provenance.
//
// The envelope already carries how strong a verifier was. It does not carry
// who the verifier was, so a model-generated test that ran and a repository
// test that ran arrive identical. This adds the source and the identities the
// evidence is bound to, and authorizes nothing with them: every test below
// that touches delivery proves the decision is UNCHANGED.

func provenanceFixture() V3EvidenceProvenance {
	return V3EvidenceProvenance{
		Source:              ProvenanceRepoOwnedCheck,
		RequestID:           "req-1",
		InvocationID:        "inv-1",
		CandidateInstanceID: "inv-1:generated:0",
		CandidateHash:       strings.Repeat("c", 64),
		WorkspaceGeneration: 7,
		WorkspaceStateHash:  strings.Repeat("w", 64),
		CommandIdentity:     "python3 solve.py",
		ObligationID:        "oracle_cases_pass",
		RequiredStrength:    "oracle",
		ObservedStrength:    "oracle",
	}
}

// --- closed vocabulary, fail closed ---------------------------------------

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

func TestUnknownAuthorityCriticalValuesFailClosed(t *testing.T) {
	for _, c := range []struct {
		name string
		mut  func(*V3EvidenceProvenance)
	}{
		{"unknown source", func(p *V3EvidenceProvenance) { p.Source = "trusted" }},
		{"empty source", func(p *V3EvidenceProvenance) { p.Source = "" }},
		{"unknown required strength", func(p *V3EvidenceProvenance) { p.RequiredStrength = "strong" }},
		{"unknown observed strength", func(p *V3EvidenceProvenance) { p.ObservedStrength = "" }},
		{"missing candidate hash", func(p *V3EvidenceProvenance) { p.CandidateHash = "" }},
		{"missing request", func(p *V3EvidenceProvenance) { p.RequestID = "" }},
		{"missing invocation", func(p *V3EvidenceProvenance) { p.InvocationID = "" }},
		{"missing obligation", func(p *V3EvidenceProvenance) { p.ObligationID = "" }},
		{"negative generation", func(p *V3EvidenceProvenance) { p.WorkspaceGeneration = -1 }},
	} {
		p := provenanceFixture()
		c.mut(&p)
		if ok, _ := p.MayAuthorize(); ok {
			t.Errorf("%s: authorized", c.name)
		}
	}
}

// --- source-specific authority --------------------------------------------

func TestModelGeneratedIsRecordedButNeverAuthorizing(t *testing.T) {
	p := provenanceFixture()
	p.Source = ProvenanceModelGenerated
	if p.Source != ProvenanceModelGenerated {
		t.Fatal("the source was not recorded")
	}
	ok, why := p.MayAuthorize()
	if ok || !strings.Contains(why, ProvenanceModelGenerated) {
		t.Fatalf("model-generated authorized: ok=%v why=%q", ok, why)
	}
}

func TestLegacyIsReadableButNeverAuthorizing(t *testing.T) {
	p := provenanceFixture()
	p.Source = ProvenanceLegacy
	ok, why := p.MayAuthorize()
	if ok || !strings.Contains(why, ProvenanceLegacy) {
		t.Fatalf("legacy authorized: ok=%v why=%q", ok, why)
	}
}

func TestProxyValidationMayCloseSyntaxOnly(t *testing.T) {
	p := provenanceFixture()
	p.Source = ProvenanceProxyOwnedValidation
	p.ObligationID, p.RequiredStrength, p.ObservedStrength = "parses", "syntax", "syntax"
	if ok, why := p.MayAuthorize(); !ok {
		t.Fatalf("syntax obligation refused: %s", why)
	}
	p.RequiredStrength, p.ObligationID = "behavioral", "behaves"
	if ok, _ := p.MayAuthorize(); ok {
		t.Fatal("a syntax gate closed a behavioural obligation")
	}
}

func TestObservedStrengthMustReachTheRequirement(t *testing.T) {
	p := provenanceFixture()
	p.ObservedStrength = "syntax" // repo-owned, but it only compiled
	if ok, why := p.MayAuthorize(); ok {
		t.Fatalf("authorized below the required strength: %s", why)
	}
}

// --- binding ---------------------------------------------------------------

func TestEvidenceCannotCrossAnIdentity(t *testing.T) {
	for _, c := range []struct {
		field string
		mut   func(*V3EvidenceProvenance)
	}{
		{"request_id", func(p *V3EvidenceProvenance) { p.RequestID = "req-2" }},
		{"invocation_id", func(p *V3EvidenceProvenance) { p.InvocationID = "inv-2" }},
		{"candidate_instance_id", func(p *V3EvidenceProvenance) { p.CandidateInstanceID = "inv-1:generated:1" }},
		{"candidate_hash", func(p *V3EvidenceProvenance) { p.CandidateHash = strings.Repeat("d", 64) }},
		{"command_identity", func(p *V3EvidenceProvenance) { p.CommandIdentity = "python3 other.py" }},
		{"obligation_id", func(p *V3EvidenceProvenance) { p.ObligationID = "other" }},
		{"workspace_generation", func(p *V3EvidenceProvenance) { p.WorkspaceGeneration = 8 }},
		{"workspace_state_hash", func(p *V3EvidenceProvenance) { p.WorkspaceStateHash = strings.Repeat("z", 64) }},
	} {
		held := provenanceFixture()
		asked := provenanceFixture()
		c.mut(&asked)
		ok, why := held.BindsTo(asked)
		if ok {
			t.Errorf("%s was allowed to differ", c.field)
		}
		if !strings.Contains(why, c.field) {
			t.Errorf("%s: reason %q does not name the field", c.field, why)
		}
	}
}

func TestIdenticalIdentityBinds(t *testing.T) {
	held := provenanceFixture()
	if ok, why := held.BindsTo(provenanceFixture()); !ok {
		t.Fatalf("identical bindings refused: %s", why)
	}
}

func TestAStaleWorkspaceGenerationFailsClosed(t *testing.T) {
	held := provenanceFixture()
	asked := provenanceFixture()
	asked.WorkspaceGeneration = held.WorkspaceGeneration + 2 // mutations since
	if ok, why := held.BindsTo(asked); ok || !strings.Contains(why, "workspace_generation") {
		t.Fatalf("stale evidence bound: ok=%v why=%q", ok, why)
	}
}

// --- wire compatibility ----------------------------------------------------

func TestProvenanceIsOmittedWhenAbsent(t *testing.T) {
	var e V3EvidenceEnvelope
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "provenance") {
		t.Fatalf("an absent provenance was serialised: %s", b)
	}
}

func TestALegacyEnvelopeStillParses(t *testing.T) {
	// Exactly the shape a pre-provenance producer emits.
	const legacy = `{"wire_version":"1.0.0","record_schema_version":"1.1.0",
	 "identity":{"contract_id":"c","contract_version":"1","artifact_scope":"s",
	  "evaluation_context_hash":"ctx","adapter_id":"a","adapter_version":"v",
	  "candidate_content_hash":"h"},
	 "evaluation":{"evidence_strength":"syntax","execution_status":"ok",
	  "supported":true,"requirements_complete":false,"closure_eligible":false},
	 "selection":{"status":"best_not_closure_eligible"}}`
	var e V3EvidenceEnvelope
	if err := json.Unmarshal([]byte(legacy), &e); err != nil {
		t.Fatalf("a legacy envelope no longer parses: %v", err)
	}
	if e.Provenance != nil {
		t.Fatal("a legacy envelope invented provenance")
	}
	if a, why := e.Validate(); a != EvidenceAvailable {
		t.Fatalf("a legacy envelope stopped validating: %s (%s)", a, why)
	}
}

func TestUnknownUnrelatedFieldsStayForwardCompatible(t *testing.T) {
	const future = `{"wire_version":"1.0.0","record_schema_version":"1.1.0",
	 "some_future_field":{"nested":[1,2,3]},
	 "identity":{"contract_id":"c","contract_version":"1","artifact_scope":"s",
	  "evaluation_context_hash":"ctx","adapter_id":"a","adapter_version":"v",
	  "candidate_content_hash":"h","future_id":"x"},
	 "evaluation":{"evidence_strength":"syntax","execution_status":"ok",
	  "supported":true,"requirements_complete":false,"closure_eligible":false},
	 "selection":{"status":"best_not_closure_eligible"}}`
	var e V3EvidenceEnvelope
	if err := json.Unmarshal([]byte(future), &e); err != nil {
		t.Fatalf("an unrelated additive field broke the reader: %v", err)
	}
	if a, _ := e.Validate(); a != EvidenceAvailable {
		t.Fatal("an unrelated additive field changed availability")
	}
}

// --- inertness: delivery is unchanged --------------------------------------

func TestProvenancePresenceDoesNotChangeAuthorization(t *testing.T) {
	code := "print(1)\n"
	build := func(withProv bool) *V3EvidenceEnvelope {
		e := &V3EvidenceEnvelope{
			WireVersion: "1.0.0",
			Identity: V3EvidenceIdentity{
				ContractID: "c", ContractVersion: "1", ArtifactScope: "s",
				EvaluationContextHash: "ctx", AdapterID: "a", AdapterVersion: "v",
				CandidateContentHash: hashBytes([]byte(code)),
			},
			Evaluation: V3EvidenceEvaluation{
				EvidenceStrength: "oracle", ExecutionStatus: "ok", Supported: true,
				RequirementsComplete: true, ClosureEligible: true,
			},
			Selection: V3EvidenceSelection{Status: "verified_winner"},
		}
		if withProv {
			p := provenanceFixture()
			p.Source = ProvenanceModelGenerated // the least trusted source
			p.CandidateHash = hashBytes([]byte(code))
			e.Provenance = &p
		}
		return e
	}
	okWithout, whyWithout := EvidenceSupportsProvenanceFor(build(false), code)
	okWith, whyWith := EvidenceSupportsProvenanceFor(build(true), code)
	if okWithout != okWith || whyWithout != whyWith {
		t.Fatalf("provenance changed the delivery decision: without=(%v,%q) with=(%v,%q)",
			okWithout, whyWithout, okWith, whyWith)
	}
}

func TestNoProductionCallerConsultsProvenanceYet(t *testing.T) {
	for _, file := range []string{"v3_bridge.go", "tools.go", "agent.go", "gates.go"} {
		src := readSourceForTest(t, file)
		for _, fn := range []string{"MayAuthorize(", "BindsTo("} {
			if strings.Contains(src, fn) {
				t.Errorf("%s calls %s; wiring authorization is the next slice", file, fn)
			}
		}
	}
}
