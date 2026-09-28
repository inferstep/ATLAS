package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What a task obliges, derived from the validated request and nothing else.
//
// The derivation reads two things the request boundary already decided and the
// ledger's own record of what is on disk. It reads no prompt, no filename
// convention, no benchmark and nothing the model emitted, and a caller that
// stated no knowledge produces no structured obligation at all.

func mustContract(t *testing.T, dir, contract string) *TaskContract {
	t.Helper()
	var req struct {
		TaskContract *TaskContract `json:"task_contract,omitempty"`
	}
	body := `{"task_contract":` + contract + `}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if req.TaskContract == nil {
		t.Fatalf("%s decoded to no contract", body)
	}
	tc, err := validateTaskContract(req.TaskContract, dir)
	if err != nil {
		t.Fatalf("validate %s: %v", body, err)
	}
	return tc
}

func obligationsFor(t *testing.T, ctx *AgentContext) []taskObligation {
	t.Helper()
	return deriveTaskObligations(ctx,
		resolveOutputObligation(ctx, "Create solve.py."),
		resolveVerificationObligation(ctx))
}

func kindsOf(obs []taskObligation) []string {
	out := make([]string, 0, len(obs))
	for _, o := range obs {
		out = append(out, o.Kind)
	}
	return out
}

func hasKindFor(obs []taskObligation, kind, subject string) (taskObligation, bool) {
	for _, o := range obs {
		if o.Kind == kind && o.Subject == subject {
			return o, true
		}
	}
	return taskObligation{}, false
}

// --- the taxonomy is closed, and its floors are stated ----------------------

func TestEveryObligationKindHasExactlyOneAnswerAboutItsFloor(t *testing.T) {
	for _, kind := range obligationKinds {
		switch {
		case obligationUnsatisfiableKinds[kind]:
			if _, ok := obligationRequiredStrength(kind, ""); ok {
				t.Errorf("%s is unsatisfiable and must have no strength", kind)
			}
		case obligationDynamicStrengthKinds[kind]:
			if _, ok := obligationRequiredStrength(kind, ""); ok {
				t.Errorf("%s must refuse to invent a baseline strength", kind)
			}
			s, ok := obligationRequiredStrength(kind, "behavioral")
			if !ok || s != "behavioral" {
				t.Errorf("%s took %q/%v from a behavioral baseline", kind, s, ok)
			}
		default:
			s, ok := obligationRequiredStrength(kind, "")
			if !ok || strengthRank(s) < 0 {
				t.Errorf("%s has no fixed strength: %q/%v", kind, s, ok)
			}
			// A fixed floor cannot be raised by handing it a baseline.
			if _, ok := obligationRequiredStrength(kind, "oracle"); ok {
				t.Errorf("%s let a baseline raise its fixed floor", kind)
			}
		}
	}
}

func TestADeclaredCommandIsBehavioralAndNotAnOracle(t *testing.T) {
	// Exit zero says the command the client asked for ran and succeeded
	// against these bytes. It does not say an answer was checked against a
	// reference, so no declaration reaches oracle -- and the kind carries no
	// fixed floor at all, because how strongly a command counts is a statement
	// only the client can make about that command.
	if _, fixed := obligationKindRequiredStrength[ObligationDeclaredCommand]; fixed {
		t.Error("the declared-command kind carries a fixed floor")
	}
	if !obligationDynamicStrengthKinds[ObligationDeclaredCommand] {
		t.Error("the declared-command floor is not taken from the obligation")
	}
	for _, kind := range []string{VerificationKindSyntax, VerificationKindRuntime,
		VerificationKindBehavioral} {
		got, ok := obligationRequiredStrength(ObligationDeclaredCommand, kind)
		if !ok || got != kind {
			t.Errorf("declared %q resolved to %q (ok=%v)", kind, got, ok)
		}
	}
	if _, ok := obligationRequiredStrength(ObligationDeclaredCommand, "oracle"); ok {
		t.Error("a declared command reached oracle")
	}
	if got := obligationKindRequiredStrength[ObligationDeclaredExample]; got != "oracle" {
		t.Errorf("declared example floor is %q, want oracle", got)
	}
}

func TestAnUnknownObligationKindFailsClosed(t *testing.T) {
	if _, ok := obligationID("something_new", "x"); ok {
		t.Error("an unknown kind produced an id")
	}
	if _, ok := newTaskObligation("something_new", "x", "", true); ok {
		t.Error("an unknown kind produced an obligation")
	}
	if _, ok := newTaskObligation(ObligationBaselinePreserved, "x", "very_strong", true); ok {
		t.Error("an unknown baseline strength produced an obligation")
	}
	if _, ok := newTaskObligation(ObligationArtifactExists, "   ", "", true); ok {
		t.Error("an empty subject produced an obligation")
	}
}

// TestAnObligationIDNeverCarriesItsSubject pins the leak rule: a declared
// command is a subject, and a command string in an operator log is a content
// leak. The rule is uniform so no exception can leak one.
func TestAnObligationIDNeverCarriesItsSubject(t *testing.T) {
	const secret = "pytest --token=hunter2 -q"
	o, ok := newTaskObligation(ObligationDeclaredCommand, secret, VerificationKindRuntime, true)
	if !ok {
		t.Fatal("declared command obligation refused")
	}
	if o.ID == secret || strings.Contains(o.ID, "hunter2") {
		t.Errorf("obligation id %q carries its subject", o.ID)
	}
}

// TestObligationIDsArePinned pins the exact id strings for known (kind,
// subject) pairs: the kind, a colon, and the first 32 hex digits of the
// subject's SHA-256. A change to the derivation fails here rather than
// silently renaming every obligation.
func TestObligationIDsArePinned(t *testing.T) {
	for _, c := range []struct{ kind, subject, want string }{
		{ObligationArtifactExists, "solve.py",
			"artifact_exists:713230e3884f80839aab2246048d5c46"},
		{ObligationSyntacticValidity, "solve.py",
			"syntactic_validity:713230e3884f80839aab2246048d5c46"},
		{ObligationDeclaredCommand, "pytest -q",
			"declared_command:c3b206874e8a7a233c1954889847b7d5"},
	} {
		got, ok := obligationID(c.kind, c.subject)
		if !ok || got != c.want {
			t.Errorf("%s/%q -> %q (ok=%v), want %q", c.kind, c.subject, got, ok, c.want)
		}
	}
}

// --- derivation from the validated request ----------------------------------

func TestADeclaredOutputOwesExistenceAndTheSyntaxTheGateAlreadyRequires(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)

	obs := obligationsFor(t, ctx)
	resolved := resolveAgentPath(ctx, "solve.py")
	if _, ok := hasKindFor(obs, ObligationArtifactExists, resolved); !ok {
		t.Errorf("no existence obligation for the declared output: %v", kindsOf(obs))
	}
	syn, ok := hasKindFor(obs, ObligationSyntacticValidity, resolved)
	if !ok {
		t.Fatalf("no syntax obligation for a .py deliverable: %v", kindsOf(obs))
	}
	if syn.RequiredStrength != "syntax" {
		t.Errorf("syntax obligation floor %q", syn.RequiredStrength)
	}
}

// TestTheSyntaxObligationUsesTheGatesOwnTable pins that there is no second
// extension table: a class the live gate does not govern owes no syntax.
func TestTheSyntaxObligationUsesTheGatesOwnTable(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared",`+
			`"expected_outputs":["notes.txt","solve.py"]}`)

	obs := obligationsFor(t, ctx)
	if _, ok := hasKindFor(obs, ObligationSyntacticValidity,
		resolveAgentPath(ctx, "notes.txt")); ok {
		t.Error("a class the syntax gate does not govern owes syntax")
	}
	if _, ok := hasKindFor(obs, ObligationSyntacticValidity,
		resolveAgentPath(ctx, "solve.py")); !ok {
		t.Error("a class the syntax gate does govern owes none")
	}
	// Existence is owed for both: it does not depend on the gate.
	for _, p := range []string{"notes.txt", "solve.py"} {
		if _, ok := hasKindFor(obs, ObligationArtifactExists,
			resolveAgentPath(ctx, p)); !ok {
			t.Errorf("%s owes no existence obligation", p)
		}
	}
}

func TestEachDeclaredCommandIsItsOwnExactObligation(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","verification_knowledge":"declared",`+
			`"verification":["pytest -q","ruff check ."]}`)

	obs := obligationsFor(t, ctx)
	for _, cmd := range []string{"pytest -q", "ruff check ."} {
		o, ok := hasKindFor(obs, ObligationDeclaredCommand, cmd)
		if !ok {
			t.Fatalf("no obligation for declared command %q", cmd)
		}
		// Declared without a type, so each carries the runtime floor rather
		// than a behavioral one nobody stated.
		if o.RequiredStrength != VerificationKindRuntime {
			t.Errorf("%q floor %q, want runtime", cmd, o.RequiredStrength)
		}
	}
	// Two commands are two obligations, never one satisfied twice.
	if a, _ := hasKindFor(obs, ObligationDeclaredCommand, "pytest -q"); true {
		b, _ := hasKindFor(obs, ObligationDeclaredCommand, "ruff check .")
		if a.ID == b.ID {
			t.Error("two declared commands share one obligation id")
		}
	}
}

// TestSimilarCommandsAreDifferentObligations pins exactness: the client's
// string is the obligation, not the shape of it.
func TestSimilarCommandsAreDifferentObligations(t *testing.T) {
	a, _ := obligationID(ObligationDeclaredCommand, "pytest -q")
	b, _ := obligationID(ObligationDeclaredCommand, "pytest  -q")
	c, _ := obligationID(ObligationDeclaredCommand, "pytest -q ")
	if a == b || a == c {
		t.Error("a command that differs by whitespace became the same obligation")
	}
}

func TestReplacingAValidatedBaselineOwesAtLeastWhatItAlreadyHas(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "solve.py")
	if err := os.WriteFile(path, []byte("A = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)

	resolved := resolveAgentPath(ctx, "solve.py")
	disk := fileSHA256(ctx, resolved)
	if disk == "" {
		t.Fatal("no hash for the baseline on disk")
	}

	// A ledger pass about exactly these bytes is syntax-strength evidence.
	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*DeliverableState{}
	}
	ctx.Ledger[resolved] = &DeliverableState{
		Path: resolved, CurrentHash: disk, Generation: 1,
		ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
		ValidatedHash: disk,
	}
	obs := obligationsFor(t, ctx)
	base, ok := hasKindFor(obs, ObligationBaselinePreserved, resolved)
	if !ok {
		t.Fatalf("a validated baseline owes no preservation: %v", kindsOf(obs))
	}
	if base.RequiredStrength != "syntax" {
		t.Errorf("baseline floor %q, want the syntax it already has", base.RequiredStrength)
	}

	// A green run covering exactly these bytes raises it to behavioral.
	ctx.VerificationEvidence = append(ctx.VerificationEvidence, VerificationRecord{
		Command: "python3 solve.py", Covered: map[string]string{resolved: disk}, Turn: 1,
	})
	obs = obligationsFor(t, ctx)
	base, ok = hasKindFor(obs, ObligationBaselinePreserved, resolved)
	if !ok {
		t.Fatal("a behaviourally covered baseline owes no preservation")
	}
	if base.RequiredStrength != "behavioral" {
		t.Errorf("baseline floor %q, want the behavioral it already has",
			base.RequiredStrength)
	}
}

// TestAStaleValidationIsNotABaseline pins that a verdict about superseded
// bytes is history, not evidence about what is there now.
func TestAStaleValidationIsNotABaseline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "solve.py")
	if err := os.WriteFile(path, []byte("A = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)
	resolved := resolveAgentPath(ctx, "solve.py")
	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*DeliverableState{}
	}
	ctx.Ledger[resolved] = &DeliverableState{
		Path: resolved, CurrentHash: "stale", Generation: 1,
		ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
		ValidatedHash: "some-earlier-hash",
	}
	if _, ok := hasKindFor(obligationsFor(t, ctx), ObligationBaselinePreserved, resolved); ok {
		t.Error("a verdict about superseded bytes became a baseline")
	}
}

func TestAnAbsentFileIsNoBaseline(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)
	resolved := resolveAgentPath(ctx, "solve.py")
	if _, ok := hasKindFor(obligationsFor(t, ctx), ObligationBaselinePreserved, resolved); ok {
		t.Error("a file that is not there became a baseline to preserve")
	}
}

// --- legacy traffic creates no structured authority --------------------------

func TestUnspecifiedKnowledgeCreatesNoStructuredObligation(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{
		`{"task_mode":"work"}`,
		`{"task_mode":"work","output_knowledge":"unspecified","verification_knowledge":"unspecified"}`,
		`{"task_mode":"question"}`,
	} {
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.TaskContract = mustContract(t, dir, body)
		if obs := obligationsFor(t, ctx); len(obs) != 0 {
			t.Errorf("%s produced structured obligations %v", body, kindsOf(obs))
		}
	}
}

// TestContractlessProseNeverBecomesAStructuredObligation is the promotion this
// file must not make. The prose heuristic may still govern proxy completion;
// converting its guesses into trusted structured authority is what turns
// "Write solve.py that reads input.txt" into an obligation about input.txt.
func TestContractlessProseNeverBecomesAStructuredObligation(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = nil

	legacy := resolveOutputObligation(ctx, "Write solve.py that reads input.txt")
	if legacy.Source != ObligationSourceLegacy {
		t.Fatalf("source %q, want legacy", legacy.Source)
	}
	if obs := deriveTaskObligations(ctx, legacy, resolveVerificationObligation(ctx)); len(obs) != 0 {
		t.Errorf("prose produced structured obligations %v", kindsOf(obs))
	}
	// The heuristic itself is untouched: legacy completion still sees it.
	if len(legacy.Items) == 0 {
		t.Error("the legacy heuristic stopped producing its own items")
	}
}

func TestDeclaredEmptyKnowledgeOwesNothingRatherThanEverything(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":[],`+
			`"verification_knowledge":"declared","verification":[]}`)
	if obs := obligationsFor(t, ctx); len(obs) != 0 {
		t.Errorf("authoritative none produced %v", kindsOf(obs))
	}
}

func TestDerivationIsCanonicalAndRepeatable(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared",`+
			`"expected_outputs":["b.py","a.py"],`+
			`"verification_knowledge":"declared","verification":["z -q","a -q"]}`)
	first := obligationsFor(t, ctx)
	for i := 0; i < 5; i++ {
		again := obligationsFor(t, ctx)
		if len(again) != len(first) {
			t.Fatalf("derivation length drifted: %d then %d", len(first), len(again))
		}
		for j := range first {
			if first[j].ID != again[j].ID {
				t.Fatalf("derivation order drifted at %d: %q then %q",
					j, first[j].ID, again[j].ID)
			}
		}
	}
}
