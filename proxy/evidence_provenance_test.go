package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Evidence provenance on the V3 wire.
//
// The envelope carries how strong a verifier was. Provenance also says who the
// verifier was, so a model-generated test and a repository test that both ran
// do not arrive identical, and it names the identities the evidence is bound
// to. Provenance on an envelope does not change the V3 delivery decision.

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

// MayAuthorize refuses provenance it cannot trust or cannot read, and the
// reason says what is wrong. The unchanged fixture authorizes, so each row is
// refused for its own change.
func TestMayAuthorizeRefusesUntrustedOrIncompleteProvenance(t *testing.T) {
	fixture := provenanceFixture()
	if ok, why := fixture.MayAuthorize(); !ok {
		t.Fatalf("the unchanged fixture was refused: %s", why)
	}
	for _, tc := range []struct {
		name      string
		change    func(*V3EvidenceProvenance)
		reasonHas string
	}{
		{"unknown source", func(p *V3EvidenceProvenance) { p.Source = "trusted" }, "unknown evidence source"},
		{"empty source", func(p *V3EvidenceProvenance) { p.Source = "" }, "unknown evidence source"},
		{"model generated", func(p *V3EvidenceProvenance) { p.Source = ProvenanceModelGenerated }, ProvenanceModelGenerated},
		{"legacy", func(p *V3EvidenceProvenance) { p.Source = ProvenanceLegacy }, ProvenanceLegacy},
		{"unknown required strength", func(p *V3EvidenceProvenance) { p.RequiredStrength = "strong" }, "unknown required strength"},
		{"unknown observed strength", func(p *V3EvidenceProvenance) { p.ObservedStrength = "" }, "unknown observed strength"},
		{"observed below required", func(p *V3EvidenceProvenance) { p.ObservedStrength = "syntax" }, "below the required"},
		{"missing candidate hash", func(p *V3EvidenceProvenance) { p.CandidateHash = "" }, "candidate_hash is missing"},
		{"missing request", func(p *V3EvidenceProvenance) { p.RequestID = "" }, "request_id is missing"},
		{"missing invocation", func(p *V3EvidenceProvenance) { p.InvocationID = "" }, "invocation_id is missing"},
		{"missing obligation", func(p *V3EvidenceProvenance) { p.ObligationID = "" }, "obligation_id is missing"},
		{"negative generation", func(p *V3EvidenceProvenance) { p.WorkspaceGeneration = -1 }, "workspace_generation is negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := provenanceFixture()
			tc.change(&p)
			ok, why := p.MayAuthorize()
			if ok {
				t.Fatal("authorized")
			}
			if !strings.Contains(why, tc.reasonHas) {
				t.Errorf("reason %q does not contain %q", why, tc.reasonHas)
			}
		})
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

// --- binding ---------------------------------------------------------------

// Evidence binds only to the identity it was observed for. Identical
// identities bind, and a difference in any one field refuses and names it.
func TestEvidenceCannotCrossAnIdentity(t *testing.T) {
	held := provenanceFixture()
	if ok, why := held.BindsTo(provenanceFixture()); !ok {
		t.Fatalf("identical bindings refused: %s", why)
	}
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

// An envelope without provenance still parses and validates: the shape an
// older producer emits, and one with unknown fields that have nothing to do
// with provenance.
func TestEnvelopesWithoutProvenanceStillParse(t *testing.T) {
	for _, tc := range []struct{ name, envelope string }{
		{"older producer", `{"wire_version":"1.0.0","record_schema_version":"1.1.0",
		 "identity":{"contract_id":"c","contract_version":"1","artifact_scope":"s",
		  "evaluation_context_hash":"ctx","adapter_id":"a","adapter_version":"v",
		  "candidate_content_hash":"h"},
		 "evaluation":{"evidence_strength":"syntax","execution_status":"ok",
		  "supported":true,"requirements_complete":false,"closure_eligible":false},
		 "selection":{"status":"best_not_closure_eligible"}}`},
		{"unknown unrelated fields", `{"wire_version":"1.0.0","record_schema_version":"1.1.0",
		 "some_future_field":{"nested":[1,2,3]},
		 "identity":{"contract_id":"c","contract_version":"1","artifact_scope":"s",
		  "evaluation_context_hash":"ctx","adapter_id":"a","adapter_version":"v",
		  "candidate_content_hash":"h","future_id":"x"},
		 "evaluation":{"evidence_strength":"syntax","execution_status":"ok",
		  "supported":true,"requirements_complete":false,"closure_eligible":false},
		 "selection":{"status":"best_not_closure_eligible"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var e V3EvidenceEnvelope
			if err := json.Unmarshal([]byte(tc.envelope), &e); err != nil {
				t.Fatalf("the envelope does not parse: %v", err)
			}
			if e.Provenance != nil {
				t.Fatal("the envelope invented provenance")
			}
			if a, why := e.Validate(); a != EvidenceAvailable {
				t.Fatalf("the envelope does not validate: %s (%s)", a, why)
			}
		})
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
