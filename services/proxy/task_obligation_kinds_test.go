package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itigges22/atlas-proxy/internal/taskcontract"
	"github.com/itigges22/atlas-proxy/internal/wire"
)

// What a task obliges, derived from the validated request and nothing else.
//
// The derivation reads two things the request boundary already decided and the
// ledger's own record of what is on disk. It reads no prompt, no filename
// convention, no benchmark and nothing the model emitted, and a caller that
// stated no knowledge produces no structured obligation at all.

func mustContract(t *testing.T, dir, contract string) *taskcontract.TaskContract {
	t.Helper()
	var req struct {
		TaskContract *taskcontract.TaskContract `json:"task_contract,omitempty"`
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

func obligationsFor(t *testing.T, ctx *AgentContext) []taskcontract.TaskObligation {
	t.Helper()
	return deriveTaskObligations(ctx,
		resolveOutputObligation(ctx, "Create solve.py."),
		resolveVerificationObligation(ctx))
}

func kindsOf(obs []taskcontract.TaskObligation) []string {
	out := make([]string, 0, len(obs))
	for _, o := range obs {
		out = append(out, o.Kind)
	}
	return out
}

func hasKindFor(obs []taskcontract.TaskObligation, kind, subject string) (taskcontract.TaskObligation, bool) {
	for _, o := range obs {
		if o.Kind == kind && o.Subject == subject {
			return o, true
		}
	}
	return taskcontract.TaskObligation{}, false
}

// --- the taxonomy is closed, and its floors are stated ----------------------
//
// Every kind has exactly one answer about its floor: an unsatisfiable kind has
// none, a dynamic kind takes it from a baseline, and every other kind has a
// fixed one. Each of the next three tests takes the kinds of one class, and an
// unsatisfiable kind counts as unsatisfiable even when it is also dynamic.

func TestAnUnknownObligationKindFailsClosed(t *testing.T) {
	if _, ok := taskcontract.ObligationID("something_new", "x"); ok {
		t.Error("an unknown kind produced an id")
	}
	if _, ok := taskcontract.NewTaskObligation("something_new", "x", "", true); ok {
		t.Error("an unknown kind produced an obligation")
	}
	if _, ok := taskcontract.NewTaskObligation(taskcontract.ObligationBaselinePreserved, "x", "very_strong", true); ok {
		t.Error("an unknown baseline strength produced an obligation")
	}
	if _, ok := taskcontract.NewTaskObligation(taskcontract.ObligationArtifactExists, "   ", "", true); ok {
		t.Error("an empty subject produced an obligation")
	}
}

// TestAnObligationIDNeverCarriesItsSubject pins the leak rule: a declared
// command is a subject, and a command string in an operator log is a content
// leak. The rule is uniform so no exception can leak one.
func TestAnObligationIDNeverCarriesItsSubject(t *testing.T) {
	const secret = "pytest --token=hunter2 -q"
	o, ok := taskcontract.NewTaskObligation(taskcontract.ObligationDeclaredCommand, secret, taskcontract.VerificationKindRuntime, true)
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
		{taskcontract.ObligationArtifactExists, "solve.py",
			"artifact_exists:713230e3884f80839aab2246048d5c46"},
		{taskcontract.ObligationSyntacticValidity, "solve.py",
			"syntactic_validity:713230e3884f80839aab2246048d5c46"},
		{taskcontract.ObligationDeclaredCommand, "pytest -q",
			"declared_command:c3b206874e8a7a233c1954889847b7d5"},
	} {
		got, ok := taskcontract.ObligationID(c.kind, c.subject)
		if !ok || got != c.want {
			t.Errorf("%s/%q -> %q (ok=%v), want %q", c.kind, c.subject, got, ok, c.want)
		}
	}
}

// --- derivation from the validated request ----------------------------------

// A declared output owes existence, and the syntax the live gate already
// requires. The syntax obligation uses the gate's own extension table, so a
// class the gate does not govern (notes.txt) owes existence but no syntax.
func TestADeclaredOutputOwesExistenceAndTheSyntaxTheGateAlreadyRequires(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared",`+
			`"expected_outputs":["notes.txt","solve.py"]}`)

	obs := obligationsFor(t, ctx)
	for _, p := range []string{"notes.txt", "solve.py"} {
		if _, ok := hasKindFor(obs, taskcontract.ObligationArtifactExists, resolveAgentPath(ctx, p)); !ok {
			t.Errorf("%s owes no existence obligation: %v", p, kindsOf(obs))
		}
	}
	syn, ok := hasKindFor(obs, taskcontract.ObligationSyntacticValidity, resolveAgentPath(ctx, "solve.py"))
	if !ok {
		t.Fatalf("no syntax obligation for a .py deliverable: %v", kindsOf(obs))
	}
	if syn.RequiredStrength != "syntax" {
		t.Errorf("syntax obligation floor %q", syn.RequiredStrength)
	}
	if _, ok := hasKindFor(obs, taskcontract.ObligationSyntacticValidity, resolveAgentPath(ctx, "notes.txt")); ok {
		t.Error("a class the syntax gate does not govern owes syntax")
	}
}

func TestEachDeclaredCommandIsItsOwnExactObligation(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","verification_knowledge":"declared",`+
			`"verification":["pytest -q","ruff check ."]}`)

	obs := obligationsFor(t, ctx)
	pytest, ok := hasKindFor(obs, taskcontract.ObligationDeclaredCommand, "pytest -q")
	if !ok {
		t.Fatal(`no obligation for declared command "pytest -q"`)
	}
	ruff, ok := hasKindFor(obs, taskcontract.ObligationDeclaredCommand, "ruff check .")
	if !ok {
		t.Fatal(`no obligation for declared command "ruff check ."`)
	}
	// Declared without a type, so each carries the runtime floor rather than a
	// behavioral one nobody stated.
	for _, o := range []taskcontract.TaskObligation{pytest, ruff} {
		if o.RequiredStrength != taskcontract.VerificationKindRuntime {
			t.Errorf("%q floor %q, want runtime", o.Subject, o.RequiredStrength)
		}
	}
	// Two commands are two obligations, never one satisfied twice.
	if pytest.ID == ruff.ID {
		t.Error("two declared commands share one obligation id")
	}
}

// TestSimilarCommandsAreDifferentObligations pins exactness: the client's
// string is the obligation, not the shape of it.
func TestSimilarCommandsAreDifferentObligations(t *testing.T) {
	a, _ := taskcontract.ObligationID(taskcontract.ObligationDeclaredCommand, "pytest -q")
	b, _ := taskcontract.ObligationID(taskcontract.ObligationDeclaredCommand, "pytest  -q")
	c, _ := taskcontract.ObligationID(taskcontract.ObligationDeclaredCommand, "pytest -q ")
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
		ctx.Ledger = map[string]*taskcontract.DeliverableState{}
	}
	ctx.Ledger[resolved] = &taskcontract.DeliverableState{
		Path: resolved, CurrentHash: disk, Generation: 1,
		ValidationKind: wire.ValidationKindSyntax, ValidationStatus: wire.ValidationPassed,
		ValidatedHash: disk,
	}
	obs := obligationsFor(t, ctx)
	base, ok := hasKindFor(obs, taskcontract.ObligationBaselinePreserved, resolved)
	if !ok {
		t.Fatalf("a validated baseline owes no preservation: %v", kindsOf(obs))
	}
	if base.RequiredStrength != "syntax" {
		t.Errorf("baseline floor %q, want the syntax it already has", base.RequiredStrength)
	}

	// A green run covering exactly these bytes raises it to behavioral.
	ctx.VerificationEvidence = append(ctx.VerificationEvidence, taskcontract.VerificationRecord{
		Command: "python3 solve.py", Covered: map[string]string{resolved: disk}, Turn: 1,
	})
	obs = obligationsFor(t, ctx)
	base, ok = hasKindFor(obs, taskcontract.ObligationBaselinePreserved, resolved)
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
		ctx.Ledger = map[string]*taskcontract.DeliverableState{}
	}
	ctx.Ledger[resolved] = &taskcontract.DeliverableState{
		Path: resolved, CurrentHash: "stale", Generation: 1,
		ValidationKind: wire.ValidationKindSyntax, ValidationStatus: wire.ValidationPassed,
		ValidatedHash: "some-earlier-hash",
	}
	if _, ok := hasKindFor(obligationsFor(t, ctx), taskcontract.ObligationBaselinePreserved, resolved); ok {
		t.Error("a verdict about superseded bytes became a baseline")
	}
}

func TestAnAbsentFileIsNoBaseline(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)
	resolved := resolveAgentPath(ctx, "solve.py")
	if _, ok := hasKindFor(obligationsFor(t, ctx), taskcontract.ObligationBaselinePreserved, resolved); ok {
		t.Error("a file that is not there became a baseline to preserve")
	}
}

// --- legacy traffic creates no structured authority --------------------------

// A contract that states no knowledge, or declares empty lists, owes nothing:
// never everything, and never a structured obligation guessed from elsewhere.
func TestAContractThatDeclaresNoOutputOrCommandOwesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contract string
	}{
		{"work, no knowledge stated", `{"task_mode":"work"}`},
		{"knowledge unspecified",
			`{"task_mode":"work","output_knowledge":"unspecified","verification_knowledge":"unspecified"}`},
		{"question", `{"task_mode":"question"}`},
		{"declared empty", `{"task_mode":"work","output_knowledge":"declared","expected_outputs":[],` +
			`"verification_knowledge":"declared","verification":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.TaskContract = mustContract(t, dir, tc.contract)
			if obs := obligationsFor(t, ctx); len(obs) != 0 {
				t.Errorf("structured obligations %v, want none", kindsOf(obs))
			}
		})
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
	if legacy.Source != taskcontract.ObligationSourceLegacy {
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

// Pre-delivery authorization, authorized target identity, and post-delivery
// settlement are three separate questions, asked at different times.
//
// Nothing can show that a file exists before the candidate lands, and
// delivery needs authorization. If authorization also needed existence, every
// task with a declared output could never be satisfied.

func rolesFixture(t *testing.T, contract string, files map[string]string) (*AgentContext, []taskcontract.TaskObligation) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	if contract != "" {
		ctx.TaskContract = mustContract(t, dir, contract)
	}
	obs := deriveTaskObligations(ctx,
		resolveOutputObligation(ctx, "Create it."),
		resolveVerificationObligation(ctx))
	return ctx, obs
}

// --- the split itself --------------------------------------------------------

func TestExistenceIsNeverAnAuthorizationPrerequisite(t *testing.T) {
	role, _ := taskcontract.ObligationRole(taskcontract.ObligationArtifactExists)
	if role != taskcontract.ObligationRolePostDeliverySettlement {
		t.Fatalf("artifact_exists role %q, want post-delivery settlement", role)
	}
	_, obs := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		nil)
	for _, o := range taskcontract.AuthorizationPrerequisites(obs) {
		if o.Kind == taskcontract.ObligationArtifactExists {
			t.Error("existence is being asked for before the candidate lands")
		}
	}
	settle := taskcontract.PostDeliverySettlement(obs)
	if len(settle) != 1 || settle[0].Kind != taskcontract.ObligationArtifactExists {
		t.Errorf("settlement is %v, want exactly the declared output", kindsOf(settle))
	}
}

// --- new code output: target identity plus a syntax prerequisite -------------

func TestANewCodeOutputOwesTargetIdentityAndSyntax(t *testing.T) {
	ctx, obs := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		nil)
	resolved := resolveAgentPath(ctx, "solve.py")

	if !taskcontract.TargetIsAuthorized(obs, resolved) {
		t.Error("the declared output is not an authorized target")
	}
	pre := taskcontract.AuthorizationPrerequisites(obs)
	if len(pre) != 1 || pre[0].Kind != taskcontract.ObligationSyntacticValidity {
		t.Fatalf("prerequisites %v, want exactly structural validity", kindsOf(pre))
	}
	if got := taskcontract.AuthorizationFloor(obs); got != "syntax" {
		t.Errorf("authorization floor %q, want syntax", got)
	}
}

// --- new document output: identity without a fabricated requirement ---------

func TestANewDocumentOutputOwesNoFabricatedSyntax(t *testing.T) {
	ctx, obs := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["notes.md"]}`,
		nil)
	resolved := resolveAgentPath(ctx, "notes.md")

	if !taskcontract.TargetIsAuthorized(obs, resolved) {
		t.Error("the declared document is not an authorized target")
	}
	for _, o := range obs {
		if o.Kind == taskcontract.ObligationSyntacticValidity {
			t.Error("a class the gate does not govern was given a syntax obligation")
		}
	}
	// No prerequisite, so no floor. That is a real answer, not an open door:
	// authorization additionally requires a satisfied prerequisite, and a task
	// with none has nothing to satisfy.
	if got := taskcontract.AuthorizationFloor(obs); got != "" {
		t.Errorf("authorization floor %q, want none stated", got)
	}
	if len(taskcontract.AuthorizationPrerequisites(obs)) != 0 {
		t.Error("a document with no declared verification owes a prerequisite")
	}
	// Naming a path says what may be replaced, never that any bytes belong in
	// it: being a target completes no settlement while the file is absent.
	if ok, _ := settlementIsComplete(ctx, obs, ""); ok {
		t.Error("an absent artifact settled its own existence")
	}
	// And it is not impossible: declaring a command gives it a path.
	_, withCmd := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["notes.md"],`+
			`"verification_knowledge":"declared","verification":["mdl notes.md"]}`, nil)
	if got := taskcontract.AuthorizationFloor(withCmd); got != taskcontract.VerificationKindRuntime {
		t.Errorf("a declared command gave the document floor %q, want runtime", got)
	}
}

// --- baselines ---------------------------------------------------------------

func TestAnExistingSyntaxValidatedArtifactStillOwesPreservation(t *testing.T) {
	ctx, _ := rolesFixture(t, "", map[string]string{"solve.py": "A = 1\n"})
	resolved := resolveAgentPath(ctx, "solve.py")
	disk := fileSHA256(ctx, resolved)
	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*taskcontract.DeliverableState{}
	}
	ctx.Ledger[resolved] = &taskcontract.DeliverableState{
		Path: resolved, CurrentHash: disk, Generation: 1,
		ValidationKind: wire.ValidationKindSyntax, ValidationStatus: wire.ValidationPassed,
		ValidatedHash: disk,
	}
	ctx.TaskContract = mustContract(t, ctx.WorkingDir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)
	obs := deriveTaskObligations(ctx,
		resolveOutputObligation(ctx, "Improve it."),
		resolveVerificationObligation(ctx))

	var base taskcontract.TaskObligation
	for _, o := range taskcontract.AuthorizationPrerequisites(obs) {
		if o.Kind == taskcontract.ObligationBaselinePreserved {
			base = o
		}
	}
	if base.ID == "" {
		t.Fatalf("a validated baseline owes no preservation: %v", kindsOf(obs))
	}
	if base.RequiredStrength != "syntax" {
		t.Errorf("baseline floor %q, want the syntax it has", base.RequiredStrength)
	}
	if got := taskcontract.AuthorizationFloor(obs); got != "syntax" {
		t.Errorf("authorization floor %q, want syntax", got)
	}
}

func TestASyntaxOnlyReplacementCannotDisplaceABehavioralBaseline(t *testing.T) {
	ctx, _ := rolesFixture(t, "", map[string]string{"solve.py": "print(7)\n"})
	resolved := resolveAgentPath(ctx, "solve.py")
	disk := fileSHA256(ctx, resolved)
	ctx.VerificationEvidence = append(ctx.VerificationEvidence, taskcontract.VerificationRecord{
		Command: "python3 solve.py", Covered: map[string]string{resolved: disk}, Turn: 1,
	})
	ctx.TaskContract = mustContract(t, ctx.WorkingDir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)
	obs := deriveTaskObligations(ctx,
		resolveOutputObligation(ctx, "Improve it."),
		resolveVerificationObligation(ctx))

	if got := taskcontract.AuthorizationFloor(obs); got != "behavioral" {
		t.Fatalf("authorization floor %q, want the behavioral the baseline has", got)
	}
	// A syntax verifier cannot reach that floor, which is the refusal.
	if wire.StrengthRank("syntax") >= wire.StrengthRank(taskcontract.AuthorizationFloor(obs)) {
		t.Error("a compile was allowed to displace a behaviourally covered baseline")
	}
}

// --- settlement --------------------------------------------------------------

func TestExistenceIsNotFulfilledUntilDiskAndLedgerAgree(t *testing.T) {
	ctx, obs := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		nil)
	resolved := resolveAgentPath(ctx, "solve.py")

	t.Run("nothing on disk", func(t *testing.T) {
		if ok, why := settlementIsComplete(ctx, obs, ""); ok {
			t.Errorf("settled with no artifact: %s", why)
		}
	})

	if err := os.WriteFile(resolved, []byte("A = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	disk := fileSHA256(ctx, resolved)

	t.Run("disk without a ledger entry", func(t *testing.T) {
		if ok, why := settlementIsComplete(ctx, obs, disk); ok {
			t.Errorf("settled on a file the session does not own: %s", why)
		}
	})

	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*taskcontract.DeliverableState{}
	}
	ctx.Ledger[resolved] = &taskcontract.DeliverableState{
		Path: resolved, CurrentHash: disk, Generation: 1}

	t.Run("disk and ledger agree", func(t *testing.T) {
		if ok, why := settlementIsComplete(ctx, obs, disk); !ok {
			t.Errorf("did not settle when both agree: %s", why)
		}
	})

	t.Run("ledger describes other bytes", func(t *testing.T) {
		ctx.Ledger[resolved].CurrentHash = "some-other-hash"
		if ok, _ := settlementIsComplete(ctx, obs, disk); ok {
			t.Error("settled while the ledger described different bytes")
		}
		ctx.Ledger[resolved].CurrentHash = disk
	})

	t.Run("delivered bytes are not the bytes on disk", func(t *testing.T) {
		if ok, _ := settlementIsComplete(ctx, obs, contentSHA256("something else")); ok {
			t.Error("settled on bytes that never landed")
		}
	})

	t.Run("tombstoned", func(t *testing.T) {
		ctx.Ledger[resolved].Tombstoned = true
		if ok, _ := settlementIsComplete(ctx, obs, disk); ok {
			t.Error("a removed artifact settled its own existence")
		}
		ctx.Ledger[resolved].Tombstoned = false
	})
}

// --- target identity is exact ------------------------------------------------

func TestAnUndeclaredTargetCannotBorrowAnotherOutputsAuthority(t *testing.T) {
	ctx, obs := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["src/solve.py"]}`,
		nil)
	for _, other := range []string{
		"src/other.py", "solve.py", "src/solve.pyc", "src", "src/solve.py.bak",
	} {
		if taskcontract.TargetIsAuthorized(obs, resolveAgentPath(ctx, other)) {
			t.Errorf("%q borrowed the declared target's authority", other)
		}
	}
	if !taskcontract.TargetIsAuthorized(obs, resolveAgentPath(ctx, "src/solve.py")) {
		t.Error("the declared target is not authorized")
	}
	if taskcontract.TargetIsAuthorized(obs, "") {
		t.Error("the empty path is an authorized target")
	}
}

func TestDeclaredEmptyOutputsAuthorizeNoTarget(t *testing.T) {
	ctx, obs := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":[],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`, nil)
	if got := taskcontract.AuthorizedTargets(obs); len(got) != 0 {
		t.Errorf("authoritative none authorized %v", got)
	}
	if taskcontract.TargetIsAuthorized(obs, resolveAgentPath(ctx, "anything.py")) {
		t.Error("authoritative none authorized a target")
	}
	// The declared command is still owed: the classes are independent. Its
	// floor is runtime, because the client declared the command without
	// saying what it demonstrates.
	if got := taskcontract.AuthorizationFloor(obs); got != taskcontract.VerificationKindRuntime {
		t.Errorf("floor %q, want the declared command's runtime", got)
	}
}

// --- contractless traffic authorizes nothing ---------------------------------

func TestContractlessAndUnspecifiedTrafficAuthorizeNothing(t *testing.T) {
	for _, body := range []string{
		"",
		`{"task_mode":"work"}`,
		`{"task_mode":"work","output_knowledge":"unspecified","verification_knowledge":"unspecified"}`,
		`{"task_mode":"question"}`,
	} {
		ctx, obs := rolesFixture(t, body, nil)
		if len(obs) != 0 {
			t.Errorf("%q derived %v", body, kindsOf(obs))
		}
		if got := taskcontract.AuthorizedTargets(obs); len(got) != 0 {
			t.Errorf("%q authorized %v", body, got)
		}
		if got := taskcontract.AuthorizationFloor(obs); got != "" {
			t.Errorf("%q produced floor %q", body, got)
		}
		if taskcontract.TargetIsAuthorized(obs, resolveAgentPath(ctx, "solve.py")) {
			t.Errorf("%q authorized a target", body)
		}
	}
}

// --- unknown fails closed ----------------------------------------------------

func TestAnUnsupportedPrerequisitePutsTheFloorOutOfReach(t *testing.T) {
	unsup, ok := taskcontract.NewTaskObligation(taskcontract.ObligationUnsupported, "a thing we cannot name", "", true)
	if !ok {
		t.Fatal("an unsupported obligation could not be represented")
	}
	obs := []taskcontract.TaskObligation{unsup}
	if role, _ := taskcontract.ObligationRole(taskcontract.ObligationUnsupported); role != taskcontract.ObligationRoleAuthorizationPrerequisite {
		t.Errorf("unsupported role %q, want prerequisite so it blocks", role)
	}
	if got := taskcontract.AuthorizationFloor(obs); got != "oracle" {
		t.Errorf("floor %q, want the top of the order", got)
	}
}

func TestAKindWithNoRoleIsRefusedRatherThanAssumed(t *testing.T) {
	forged := taskcontract.TaskObligation{
		ID: "forged:0", Kind: "forged", Subject: "x",
		RequiredStrength: "syntax", Required: true,
	}
	obs := []taskcontract.TaskObligation{forged}
	if len(taskcontract.AuthorizationPrerequisites(obs)) != 0 {
		t.Error("a kind with no role became a prerequisite")
	}
	if len(taskcontract.PostDeliverySettlement(obs)) != 0 {
		t.Error("a kind with no role became settlement")
	}
	if len(taskcontract.AuthorizedTargets(obs)) != 0 {
		t.Error("a kind with no role named a target")
	}
}
