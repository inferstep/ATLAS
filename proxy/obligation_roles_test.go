package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Pre-delivery authorization, authorized target identity, and post-delivery
// settlement — three questions that used to be one, and could not all be
// answered at once.
//
// The premise that broke: artifact_exists was a required obligation with a
// syntax floor. Nothing evidences a file's existence before the candidate
// lands. Delivery needs authorization. Authorization needed the obligation
// met. Every structured task with a declared output was unsatisfiable by
// construction, and each step of the loop looked reasonable alone.

func rolesFixture(t *testing.T, contract string, files map[string]string) (*AgentContext, []taskObligation) {
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

func kindsPresent(obs []taskObligation) map[string]bool {
	out := map[string]bool{}
	for _, o := range obs {
		out[o.Kind] = true
	}
	return out
}

// --- the split itself --------------------------------------------------------

func TestEveryKindHasExactlyOneRole(t *testing.T) {
	for _, kind := range obligationKinds {
		role, ok := obligationRole(kind)
		if !ok {
			t.Errorf("%s has no role; nothing knows when to ask it", kind)
			continue
		}
		switch role {
		case ObligationRoleTargetIdentity,
			ObligationRoleAuthorizationPrerequisite,
			ObligationRolePostDeliverySettlement:
		default:
			t.Errorf("%s carries the unknown role %q", kind, role)
		}
	}
	if _, ok := obligationRole("something_new"); ok {
		t.Error("an unknown kind was given a role")
	}
}

func TestExistenceIsNeverAnAuthorizationPrerequisite(t *testing.T) {
	role, _ := obligationRole(ObligationArtifactExists)
	if role != ObligationRolePostDeliverySettlement {
		t.Fatalf("artifact_exists role %q, want post-delivery settlement", role)
	}
	_, obs := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		nil)
	for _, o := range authorizationPrerequisites(obs) {
		if o.Kind == ObligationArtifactExists {
			t.Error("existence is being asked for before the candidate lands")
		}
	}
	settle := postDeliverySettlement(obs)
	if len(settle) != 1 || settle[0].Kind != ObligationArtifactExists {
		t.Errorf("settlement is %v, want exactly the declared output", kindsOf(settle))
	}
}

// --- new code output: target identity plus a syntax prerequisite -------------

func TestANewCodeOutputOwesTargetIdentityAndSyntax(t *testing.T) {
	ctx, obs := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		nil)
	resolved := resolveAgentPath(ctx, "solve.py")

	if !targetIsAuthorized(obs, resolved) {
		t.Error("the declared output is not an authorized target")
	}
	pre := authorizationPrerequisites(obs)
	if len(pre) != 1 || pre[0].Kind != ObligationSyntacticValidity {
		t.Fatalf("prerequisites %v, want exactly structural validity", kindsOf(pre))
	}
	if got := authorizationFloor(obs); got != "syntax" {
		t.Errorf("authorization floor %q, want syntax", got)
	}
}

// --- new document output: identity without a fabricated requirement ---------

func TestANewDocumentOutputOwesNoFabricatedSyntax(t *testing.T) {
	ctx, obs := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["notes.md"]}`,
		nil)
	resolved := resolveAgentPath(ctx, "notes.md")

	if !targetIsAuthorized(obs, resolved) {
		t.Error("the declared document is not an authorized target")
	}
	if kindsPresent(obs)[ObligationSyntacticValidity] {
		t.Error("a class the gate does not govern was given a syntax obligation")
	}
	// No prerequisite, so no floor. That is a real answer, not an open door:
	// authorization additionally requires a satisfied prerequisite, and a task
	// with none has nothing to satisfy.
	if got := authorizationFloor(obs); got != "" {
		t.Errorf("authorization floor %q, want none stated", got)
	}
	if len(authorizationPrerequisites(obs)) != 0 {
		t.Error("a document with no declared verification owes a prerequisite")
	}
	// And it is not impossible: declaring a command gives it a path.
	_, withCmd := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["notes.md"],`+
			`"verification_knowledge":"declared","verification":["mdl notes.md"]}`, nil)
	if got := authorizationFloor(withCmd); got != VerificationKindRuntime {
		t.Errorf("a declared command gave the document floor %q, want runtime", got)
	}
}

// TestTargetIdentityIsNeverEvidence pins the converse rule: naming a path says
// what may be replaced, never that any particular bytes belong in it.
func TestTargetIdentityIsNeverEvidence(t *testing.T) {
	ctx, obs := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["notes.md"]}`,
		nil)
	resolved := resolveAgentPath(ctx, "notes.md")
	if !targetIsAuthorized(obs, resolved) {
		t.Fatal("fixture is wrong")
	}
	// Being a target satisfies nothing: there is no prerequisite it meets and
	// no settlement it completes while the file is absent.
	if len(authorizationPrerequisites(obs)) != 0 {
		t.Error("target identity produced a prerequisite")
	}
	if ok, _ := settlementIsComplete(ctx, obs, ""); ok {
		t.Error("an absent artifact settled its own existence")
	}
}

// --- baselines ---------------------------------------------------------------

func TestAnExistingSyntaxValidatedArtifactStillOwesPreservation(t *testing.T) {
	ctx, _ := rolesFixture(t, "", map[string]string{"solve.py": "A = 1\n"})
	resolved := resolveAgentPath(ctx, "solve.py")
	disk := fileSHA256(ctx, resolved)
	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*DeliverableState{}
	}
	ctx.Ledger[resolved] = &DeliverableState{
		Path: resolved, CurrentHash: disk, Generation: 1,
		ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
		ValidatedHash: disk,
	}
	ctx.TaskContract = mustContract(t, ctx.WorkingDir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)
	obs := deriveTaskObligations(ctx,
		resolveOutputObligation(ctx, "Improve it."),
		resolveVerificationObligation(ctx))

	var base taskObligation
	for _, o := range authorizationPrerequisites(obs) {
		if o.Kind == ObligationBaselinePreserved {
			base = o
		}
	}
	if base.ID == "" {
		t.Fatalf("a validated baseline owes no preservation: %v", kindsOf(obs))
	}
	if base.RequiredStrength != "syntax" {
		t.Errorf("baseline floor %q, want the syntax it has", base.RequiredStrength)
	}
	if got := authorizationFloor(obs); got != "syntax" {
		t.Errorf("authorization floor %q, want syntax", got)
	}
}

func TestASyntaxOnlyReplacementCannotDisplaceABehavioralBaseline(t *testing.T) {
	ctx, _ := rolesFixture(t, "", map[string]string{"solve.py": "print(7)\n"})
	resolved := resolveAgentPath(ctx, "solve.py")
	disk := fileSHA256(ctx, resolved)
	ctx.VerificationEvidence = append(ctx.VerificationEvidence, VerificationRecord{
		Command: "python3 solve.py", Covered: map[string]string{resolved: disk}, Turn: 1,
	})
	ctx.TaskContract = mustContract(t, ctx.WorkingDir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)
	obs := deriveTaskObligations(ctx,
		resolveOutputObligation(ctx, "Improve it."),
		resolveVerificationObligation(ctx))

	if got := authorizationFloor(obs); got != "behavioral" {
		t.Fatalf("authorization floor %q, want the behavioral the baseline has", got)
	}
	// A syntax verifier cannot reach that floor, which is the refusal.
	if strengthRank("syntax") >= strengthRank(authorizationFloor(obs)) {
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
		ctx.Ledger = map[string]*DeliverableState{}
	}
	ctx.Ledger[resolved] = &DeliverableState{
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
		if targetIsAuthorized(obs, resolveAgentPath(ctx, other)) {
			t.Errorf("%q borrowed the declared target's authority", other)
		}
	}
	if !targetIsAuthorized(obs, resolveAgentPath(ctx, "src/solve.py")) {
		t.Error("the declared target is not authorized")
	}
	if targetIsAuthorized(obs, "") {
		t.Error("the empty path is an authorized target")
	}
}

func TestDeclaredEmptyOutputsAuthorizeNoTarget(t *testing.T) {
	ctx, obs := rolesFixture(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":[],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`, nil)
	if got := authorizedTargets(obs); len(got) != 0 {
		t.Errorf("authoritative none authorized %v", got)
	}
	if targetIsAuthorized(obs, resolveAgentPath(ctx, "anything.py")) {
		t.Error("authoritative none authorized a target")
	}
	// The declared command is still owed: the classes are independent. Its
	// floor is runtime, because the client declared the command without
	// saying what it demonstrates.
	if got := authorizationFloor(obs); got != VerificationKindRuntime {
		t.Errorf("floor %q, want the declared command's runtime", got)
	}
}

// --- legacy traffic is untouched ---------------------------------------------

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
		if got := authorizedTargets(obs); len(got) != 0 {
			t.Errorf("%q authorized %v", body, got)
		}
		if got := authorizationFloor(obs); got != "" {
			t.Errorf("%q produced floor %q", body, got)
		}
		if targetIsAuthorized(obs, resolveAgentPath(ctx, "solve.py")) {
			t.Errorf("%q authorized a target", body)
		}
	}
}

// --- unknown fails closed ----------------------------------------------------

func TestAnUnsupportedPrerequisitePutsTheFloorOutOfReach(t *testing.T) {
	unsup, ok := newTaskObligation(ObligationUnsupported, "a thing we cannot name", "", true)
	if !ok {
		t.Fatal("an unsupported obligation could not be represented")
	}
	obs := []taskObligation{unsup}
	if role, _ := obligationRole(ObligationUnsupported); role != ObligationRoleAuthorizationPrerequisite {
		t.Errorf("unsupported role %q, want prerequisite so it blocks", role)
	}
	if got := authorizationFloor(obs); got != "oracle" {
		t.Errorf("floor %q, want the top of the order", got)
	}
}

func TestAKindWithNoRoleIsRefusedRatherThanAssumed(t *testing.T) {
	forged := taskObligation{
		ID: "forged:0", Kind: "forged", Subject: "x",
		RequiredStrength: "syntax", Required: true,
	}
	obs := []taskObligation{forged}
	if len(authorizationPrerequisites(obs)) != 0 {
		t.Error("a kind with no role became a prerequisite")
	}
	if len(postDeliverySettlement(obs)) != 0 {
		t.Error("a kind with no role became settlement")
	}
	if len(authorizedTargets(obs)) != 0 {
		t.Error("a kind with no role named a target")
	}
}
