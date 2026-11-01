package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Some chat templates drop role:"tool" messages, so the model never sees a
// tool result and loops re-issuing the call. Tool results go on the wire as a
// user turn with a [tool result] marker; other roles pass through unchanged.
func TestToWireMessagesRendersToolAsUser(t *testing.T) {
	in := []AgentMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "list files"},
		{Role: "assistant", Content: `{"type":"tool_call","name":"list_directory"}`},
		{Role: "tool", ToolName: "list_directory", Content: `{"entries":["app.py","static"]}`},
	}
	out := toWireMessages(in)
	if len(out) != len(in) {
		t.Fatalf("len mismatch: %d != %d", len(out), len(in))
	}
	// system / user / assistant pass through untouched.
	if out[0]["role"] != "system" || out[1]["role"] != "user" || out[2]["role"] != "assistant" {
		t.Errorf("non-tool roles altered: %+v", out[:3])
	}
	if out[2]["content"] != in[2].Content {
		t.Errorf("assistant content altered: %q", out[2]["content"])
	}
	// tool → user with marker, content preserved after the marker.
	if out[3]["role"] != "user" {
		t.Errorf("tool role not converted to user: %q", out[3]["role"])
	}
	if out[3]["content"] != "[tool result] "+in[3].Content {
		t.Errorf("tool content not marked/preserved: %q", out[3]["content"])
	}
}

// Omitted knowledge is not authoritative emptiness.
//
// The wire tells `"expected_outputs": []` from an omitted key: encoding/json
// leaves the []string non-nil and empty for the first and nil for the second.
// The validator and storage must keep that distinction, not store an explicit
// [] as nil.
//
// Every owned sender (the TUI, tests/e2e/conftest.py and
// scripts/eval/e2e-reliability.py) sends `{"task_mode": ...}` and nothing else.
// Reading "a contract is present, so its output list is authoritative" would
// turn all that traffic into "the caller requires no outputs". So knowledge is
// stated, not inferred.

func decodeContract(t *testing.T, body string) *TaskContract {
	t.Helper()
	var req struct {
		TaskContract *TaskContract `json:"task_contract,omitempty"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return req.TaskContract
}

func validate(t *testing.T, body string) (*TaskContract, error) {
	t.Helper()
	return validateTaskContract(decodeContract(t, body), t.TempDir())
}

// --- the wire truth table ---------------------------------------------------

func TestOutputKnowledgeTruthTable(t *testing.T) {
	for _, c := range []struct {
		name      string
		body      string
		knowledge ObligationKnowledge
		paths     []string
	}{
		{"task_mode only", `{"task_contract":{"task_mode":"work"}}`,
			KnowledgeUnspecified, nil},
		{"outputs omitted", `{"task_contract":{"task_mode":"work","verification":["go test"]}}`,
			KnowledgeUnspecified, nil},
		{"outputs null", `{"task_contract":{"task_mode":"work","expected_outputs":null}}`,
			KnowledgeUnspecified, nil},
		// Without output_knowledge, an explicit [] cannot be told from an
		// omitted list in stored contracts, so it is not authoritative none.
		{"legacy empty list", `{"task_contract":{"task_mode":"work","expected_outputs":[]}}`,
			KnowledgeUnspecified, nil},
		// A legacy non-empty list always meant "these are the outputs".
		{"legacy non-empty list", `{"task_contract":{"task_mode":"work","expected_outputs":["a.py"]}}`,
			KnowledgeDeclared, []string{"a.py"}},
		{"declared with paths",
			`{"task_contract":{"task_mode":"work","output_knowledge":"declared","expected_outputs":["a.py","b.py"]}}`,
			KnowledgeDeclared, []string{"a.py", "b.py"}},
		{"declared empty is authoritative none",
			`{"task_contract":{"task_mode":"work","output_knowledge":"declared","expected_outputs":[]}}`,
			KnowledgeDeclared, []string{}},
		{"explicit unspecified",
			`{"task_contract":{"task_mode":"work","output_knowledge":"unspecified"}}`,
			KnowledgeUnspecified, nil},
	} {
		got, err := validate(t, c.body)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.OutputKnowledge != c.knowledge {
			t.Errorf("%s: knowledge=%q want %q", c.name, got.OutputKnowledge, c.knowledge)
		}
		paths := got.OutputPaths()
		if strings.Join(paths, "|") != strings.Join(c.paths, "|") {
			t.Errorf("%s: paths=%v want %v", c.name, paths, c.paths)
		}
		// Presence must survive: declared-empty is not the same as unspecified.
		declaredEmpty := c.knowledge == KnowledgeDeclared && c.paths != nil && len(c.paths) == 0
		if declaredEmpty && !got.OutputsPresent() {
			t.Errorf("%s: an authoritative none collapsed into absent", c.name)
		}
		if c.knowledge == KnowledgeUnspecified && got.OutputsPresent() {
			t.Errorf("%s: unspecified knowledge claims a present list", c.name)
		}
	}
}

func TestOutputKnowledgeTruthTableRejections(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"declared with the list omitted",
			`{"task_contract":{"task_mode":"work","output_knowledge":"declared"}}`},
		{"declared with a null list",
			`{"task_contract":{"task_mode":"work","output_knowledge":"declared","expected_outputs":null}}`},
		{"unspecified contradicted by paths",
			`{"task_contract":{"task_mode":"work","output_knowledge":"unspecified","expected_outputs":["a.py"]}}`},
		{"unknown knowledge",
			`{"task_contract":{"task_mode":"work","output_knowledge":"maybe"}}`},
		{"question cannot declare outputs",
			`{"task_contract":{"task_mode":"question","output_knowledge":"declared","expected_outputs":[]}}`},
		{"question cannot carry legacy outputs",
			`{"task_contract":{"task_mode":"question","expected_outputs":["a.py"]}}`},
	} {
		got, err := validate(t, c.body)
		if err == nil {
			t.Errorf("%s: accepted, want rejection", c.name)
		}
		if got != nil {
			t.Errorf("%s: stored a contract despite rejecting", c.name)
		}
	}
}

func TestVerificationKnowledgeTruthTable(t *testing.T) {
	for _, c := range []struct {
		name      string
		body      string
		knowledge ObligationKnowledge
		cmds      []string
		reject    bool
	}{
		{"task_mode only", `{"task_contract":{"task_mode":"work"}}`,
			KnowledgeUnspecified, nil, false},
		{"legacy empty", `{"task_contract":{"task_mode":"work","verification":[]}}`,
			KnowledgeUnspecified, nil, false},
		{"legacy non-empty", `{"task_contract":{"task_mode":"work","verification":["pytest -q"]}}`,
			KnowledgeDeclared, []string{"pytest -q"}, false},
		{"declared empty", `{"task_contract":{"task_mode":"work","verification_knowledge":"declared","verification":[]}}`,
			KnowledgeDeclared, []string{}, false},
		{"declared with commands",
			`{"task_contract":{"task_mode":"work","verification_knowledge":"declared","verification":["go test ./...","pytest"]}}`,
			KnowledgeDeclared, []string{"go test ./...", "pytest"}, false},
		{"declared with the list omitted",
			`{"task_contract":{"task_mode":"work","verification_knowledge":"declared"}}`, "", nil, true},
		{"unknown knowledge",
			`{"task_contract":{"task_mode":"work","verification_knowledge":"sure"}}`, "", nil, true},
		{"question cannot declare verification",
			`{"task_contract":{"task_mode":"question","verification_knowledge":"declared","verification":[]}}`, "", nil, true},
	} {
		got, err := validate(t, c.body)
		if c.reject {
			if err == nil || got != nil {
				t.Errorf("%s: accepted (%v)", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.VerificationKnowledge != c.knowledge {
			t.Errorf("%s: knowledge=%q want %q", c.name, got.VerificationKnowledge, c.knowledge)
		}
		if strings.Join(got.VerificationCommands(), "|") != strings.Join(c.cmds, "|") {
			t.Errorf("%s: commands=%v want %v", c.name, got.VerificationCommands(), c.cmds)
		}
	}
}

// The two are independent: one may be declared while the other is not.
func TestOutputAndVerificationKnowledgeAreIndependent(t *testing.T) {
	got, err := validate(t, `{"task_contract":{"task_mode":"work",
		"output_knowledge":"declared","expected_outputs":["a.py"],
		"verification":["pytest"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.OutputKnowledge != KnowledgeDeclared {
		t.Errorf("output knowledge %q", got.OutputKnowledge)
	}
	if got.VerificationKnowledge != KnowledgeDeclared {
		t.Errorf("legacy non-empty verification did not normalise to declared")
	}
	got, err = validate(t, `{"task_contract":{"task_mode":"work",
		"output_knowledge":"declared","expected_outputs":[]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.OutputKnowledge != KnowledgeDeclared || got.VerificationKnowledge != KnowledgeUnspecified {
		t.Errorf("knowledge leaked across classes: %q / %q",
			got.OutputKnowledge, got.VerificationKnowledge)
	}
}

// --- owned senders stay unspecified and wire-compatible ---------------------

// The TUI, the e2e conftest and the benchmark harness send task_mode alone.
func TestEveryOwnedSenderNormalisesToUnspecified(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"work", `{"task_contract":{"task_mode":"work"}}`},
		{"question", `{"task_contract":{"task_mode":"question"}}`},
	} {
		got, err := validate(t, c.body)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.OutputKnowledge != KnowledgeUnspecified ||
			got.VerificationKnowledge != KnowledgeUnspecified {
			t.Errorf("%s: %q / %q", c.name, got.OutputKnowledge, got.VerificationKnowledge)
		}
		if got.OutputsPresent() || got.VerificationPresent() {
			t.Errorf("%s: claims a present list it never sent", c.name)
		}
	}
}

func TestNoContractStaysNoContract(t *testing.T) {
	got, err := validate(t, `{"message":"hi"}`)
	if err != nil || got != nil {
		t.Fatalf("a request without a contract produced %v (%v)", got, err)
	}
}

// --- round trip through the real encoder ------------------------------------

func TestStoredContractRoundTripsWithPresenceIntact(t *testing.T) {
	for _, body := range []string{
		`{"task_contract":{"task_mode":"work"}}`,
		`{"task_contract":{"task_mode":"work","output_knowledge":"declared","expected_outputs":[]}}`,
		`{"task_contract":{"task_mode":"work","output_knowledge":"declared","expected_outputs":["a.py"]}}`,
		`{"task_contract":{"task_mode":"work","verification_knowledge":"declared","verification":[]}}`,
	} {
		got, err := validate(t, body)
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var back TaskContract
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back.OutputKnowledge != got.OutputKnowledge ||
			back.VerificationKnowledge != got.VerificationKnowledge ||
			back.OutputsPresent() != got.OutputsPresent() ||
			back.VerificationPresent() != got.VerificationPresent() ||
			strings.Join(back.OutputPaths(), "|") != strings.Join(got.OutputPaths(), "|") ||
			strings.Join(back.VerificationCommands(), "|") != strings.Join(got.VerificationCommands(), "|") {
			t.Errorf("%s: round trip lost presence: %s", body, b)
		}
	}
}

// A stored unspecified contract states its knowledge and invents no list.
//
// The knowledge fields DO appear here, and should: a stored contract is the
// validator's output, not the caller's request. What must never appear is a
// list the caller did not send -- that is the "silently upgraded to
// authoritative none" failure. The request side is covered by
// TestEveryOwnedSenderNormalisesToUnspecified, which drives the real bodies.
func TestAStoredUnspecifiedContractInventsNoList(t *testing.T) {
	got, err := validate(t, `{"task_contract":{"task_mode":"work"}}`)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	for _, k := range []string{`"expected_outputs"`, `"verification"`} {
		if strings.Contains(string(b), k) {
			t.Errorf("an unspecified contract serialised %s: %s", k, b)
		}
	}
	if !strings.Contains(string(b), `"output_knowledge":"unspecified"`) ||
		!strings.Contains(string(b), `"verification_knowledge":"unspecified"`) {
		t.Errorf("a stored contract does not state its knowledge: %s", b)
	}
}

// --- bounds still enforced ---------------------------------------------------

func TestEntryBoundsStillApply(t *testing.T) {
	var paths []string
	for i := 0; i <= maxTaskContractEntries; i++ {
		paths = append(paths, "f"+string(rune('a'+i%26))+".py")
	}
	b, _ := json.Marshal(paths)
	body := `{"task_contract":{"task_mode":"work","output_knowledge":"declared","expected_outputs":` + string(b) + `}}`
	if _, err := validate(t, body); err == nil {
		t.Fatal("the entry bound stopped applying")
	}
}

func TestCanonicalAliasesStillDedupe(t *testing.T) {
	got, err := validate(t, `{"task_contract":{"task_mode":"work",
		"output_knowledge":"declared","expected_outputs":["a.py","./a.py"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got.OutputPaths()); n != 1 {
		t.Fatalf("aliases did not dedupe: %v: got %v", got.OutputPaths(), n)
	}
}

// --- policy inertness --------------------------------------------------------

// A gate may consult the obligation DECISION. It may never read the raw
// knowledge fields, because that is a second place deciding what the caller
// meant -- and two places deciding is how the contract list and the prose
// heuristic became a union the first time.
func TestGatesReadTheDecisionNotTheRawKnowledgeFields(t *testing.T) {
	for _, file := range append(append([]string{"guardrails.go", "gates.go",
		"gates_asset_lint.go", "gates_background_jobs.go", "shell_wrapper.go",
		"gates_completion.go", "gates_edit_requests.go", "gates_ledger.go", "gates_plan.go",
		"gates_syntax_rejection.go", "gates_workspace_alignment.go",
		"guardrails_write.go", "guardrails_tool_hints.go", "guardrails_shell.go",
		"guardrails_reply.go", "guardrails_verification_gate.go", "guardrails_summaries.go"}, agentFiles...), toolFiles...) {
		src := readSourceForTest(t, file)
		for _, sym := range []string{"OutputKnowledge", "VerificationKnowledge"} {
			// validateTaskContract owns normalisation and lives in task_intake.go.
			if file == "task_intake.go" {
				continue
			}
			if strings.Contains(src, sym) {
				t.Errorf("%s reads %s directly; go through the obligation owner", file, sym)
			}
		}
	}
}

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
	for _, file := range append(append([]string{"v3_bridge.go", "gates.go",
		"gates_asset_lint.go", "gates_background_jobs.go", "shell_wrapper.go",
		"gates_completion.go", "gates_edit_requests.go", "gates_ledger.go", "gates_plan.go",
		"gates_syntax_rejection.go", "gates_workspace_alignment.go"}, agentFiles...), toolFiles...) {
		src := readSourceForTest(t, file)
		for _, fn := range []string{"MayAuthorize(", "BindsTo("} {
			if strings.Contains(src, fn) {
				t.Errorf("%s calls %s; wiring authorization is the next slice", file, fn)
			}
		}
	}
}
