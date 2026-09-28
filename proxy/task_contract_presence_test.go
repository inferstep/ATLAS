package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Omitted knowledge is not authoritative emptiness.
//
// The wire could always tell `"expected_outputs": []` from an omitted key --
// encoding/json leaves a []string nil for the first and non-nil-empty for the
// second -- and validateTaskContract threw that away: it built its output by
// appending, so an explicit [] was STORED as nil, byte-identical to omitted.
//
// That matters because every owned sender -- the TUI, tests/e2e/conftest.py
// and scripts/e2e-reliability.py -- sends `{"task_mode": ...}` and nothing
// else. Reading "a contract is present, so its output list is authoritative"
// would have turned 100% of owned traffic into "the caller requires no
// outputs" and dropped the legacy obligation for all of it.
//
// So knowledge is stated, not inferred. Nothing below reads policy: this
// commit only makes the distinction survive the wire, the validator and
// storage.

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
		reject    bool
	}{
		{"task_mode only", `{"task_contract":{"task_mode":"work"}}`,
			KnowledgeUnspecified, nil, false},
		{"outputs omitted", `{"task_contract":{"task_mode":"work","verification":["go test"]}}`,
			KnowledgeUnspecified, nil, false},
		{"outputs null", `{"task_contract":{"task_mode":"work","expected_outputs":null}}`,
			KnowledgeUnspecified, nil, false},
		// Historical storage could not tell an explicit [] from omitted, so a
		// legacy [] cannot be promoted to authoritative none.
		{"legacy empty list", `{"task_contract":{"task_mode":"work","expected_outputs":[]}}`,
			KnowledgeUnspecified, nil, false},
		// A legacy non-empty list always meant "these are the outputs".
		{"legacy non-empty list", `{"task_contract":{"task_mode":"work","expected_outputs":["a.py"]}}`,
			KnowledgeDeclared, []string{"a.py"}, false},
		{"declared with paths",
			`{"task_contract":{"task_mode":"work","output_knowledge":"declared","expected_outputs":["a.py","b.py"]}}`,
			KnowledgeDeclared, []string{"a.py", "b.py"}, false},
		{"declared empty is authoritative none",
			`{"task_contract":{"task_mode":"work","output_knowledge":"declared","expected_outputs":[]}}`,
			KnowledgeDeclared, []string{}, false},
		{"explicit unspecified",
			`{"task_contract":{"task_mode":"work","output_knowledge":"unspecified"}}`,
			KnowledgeUnspecified, nil, false},

		// rejections
		{"declared with the list omitted",
			`{"task_contract":{"task_mode":"work","output_knowledge":"declared"}}`, "", nil, true},
		{"declared with a null list",
			`{"task_contract":{"task_mode":"work","output_knowledge":"declared","expected_outputs":null}}`, "", nil, true},
		{"unspecified contradicted by paths",
			`{"task_contract":{"task_mode":"work","output_knowledge":"unspecified","expected_outputs":["a.py"]}}`, "", nil, true},
		{"unknown knowledge",
			`{"task_contract":{"task_mode":"work","output_knowledge":"maybe"}}`, "", nil, true},
		{"question cannot declare outputs",
			`{"task_contract":{"task_mode":"question","output_knowledge":"declared","expected_outputs":[]}}`, "", nil, true},
		{"question cannot carry legacy outputs",
			`{"task_contract":{"task_mode":"question","expected_outputs":["a.py"]}}`, "", nil, true},
	} {
		got, err := validate(t, c.body)
		if c.reject {
			if err == nil {
				t.Errorf("%s: accepted, want rejection", c.name)
			}
			if got != nil {
				t.Errorf("%s: stored a contract despite rejecting", c.name)
			}
			continue
		}
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
		if c.knowledge == KnowledgeDeclared && c.paths != nil && len(c.paths) == 0 {
			if !got.OutputsPresent() {
				t.Errorf("%s: an authoritative none collapsed into absent", c.name)
			}
		}
		if c.knowledge == KnowledgeUnspecified && got.OutputsPresent() {
			t.Errorf("%s: unspecified knowledge claims a present list", c.name)
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

func TestEveryOwnedSenderNormalisesToUnspecified(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"tui work", `{"task_contract":{"task_mode":"work"}}`},
		{"tui question", `{"task_contract":{"task_mode":"question"}}`},
		{"e2e conftest", `{"task_contract":{"task_mode":"work"}}`},
		{"benchmark harness", `{"task_contract":{"task_mode":"work"}}`},
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
		t.Fatalf("aliases did not dedupe: %v", got.OutputPaths())
	}
}

// --- policy inertness --------------------------------------------------------

// A gate may consult the obligation DECISION. It may never read the raw
// knowledge fields, because that is a second place deciding what the caller
// meant -- and two places deciding is how the contract list and the prose
// heuristic became a union the first time.
func TestGatesReadTheDecisionNotTheRawKnowledgeFields(t *testing.T) {
	for _, file := range []string{"guardrails.go", "tools.go", "gates.go", "agent.go"} {
		src := readSourceForTest(t, file)
		for _, sym := range []string{"OutputKnowledge", "VerificationKnowledge"} {
			// validateTaskContract owns normalisation and lives in agent.go.
			if file == "agent.go" {
				continue
			}
			if strings.Contains(src, sym) {
				t.Errorf("%s reads %s directly; go through the obligation owner", file, sym)
			}
		}
	}
}
