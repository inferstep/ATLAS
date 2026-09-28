package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A declared command and artifact coverage are two obligations, not one.
//
// They were collapsed: a declared command could only be satisfied by an
// evidence record that evidenceIsCurrent accepted, and that function refuses
// any record with empty coverage. So `pytest`, `make check` and every other
// command that names no file created an obligation nothing could ever
// discharge, and the session could not reach the settlement gate at all.

func sepCtx(t *testing.T, tc *TaskContract) (*AgentContext, string) {
	t.Helper()
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = tc
	return ctx, dir
}

func sepWrite(t *testing.T, ctx *AgentContext, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx.SessionWrites[name] = true
	observeDeliverable(ctx, name, []byte(body), ValidationKindSyntax, ValidationPassed, "")
	return fileSHA256(ctx, name)
}

func sepContract(outputs, commands []string) *TaskContract {
	o := append([]string{}, outputs...)
	c := append([]string{}, commands...)
	return &TaskContract{
		TaskMode:              TaskModeWork,
		OutputKnowledge:       KnowledgeDeclared,
		ExpectedOutputs:       &o,
		VerificationKnowledge: KnowledgeDeclared,
		Verification:          &c,
	}
}

// stamp is what the agent loop records after a command and after the ledger
// has been reconciled with whatever that command did.
func sepStamp(ctx *AgentContext, command string, covered map[string]string) VerificationRecord {
	gen, hash := workspaceIdentity(ctx)
	return VerificationRecord{
		Command: command, Covered: covered,
		WorkspaceGeneration: gen, WorkspaceStateHash: hash,
	}
}

// --- direct execution -------------------------------------------------------

func TestPathlessCommandClearsItsOwnObligationButNotCoverage(t *testing.T) {
	tc := sepContract([]string{"solve.py"}, []string{"pytest"})
	ctx, dir := sepCtx(t, tc)
	sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
	ctx.VerificationEvidence = []VerificationRecord{sepStamp(ctx, "pytest", nil)}

	d := decideVerificationDemand(ctx, tc, []string{"solve.py"})
	if !d.Required || d.Met {
		t.Fatalf("want unmet on coverage, got %+v", d)
	}
	if d.Missing != filepath.Join(dir, "solve.py") && d.Missing != "solve.py" {
		t.Fatalf("the unmet item should be the PATH, got %q", d.Missing)
	}
}

func TestPathlessCommandPlusSeparateCoverageClearsBoth(t *testing.T) {
	tc := sepContract([]string{"solve.py"}, []string{"pytest"})
	ctx, dir := sepCtx(t, tc)
	h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
	ctx.VerificationEvidence = []VerificationRecord{
		sepStamp(ctx, "pytest", nil),
		sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
	}
	if d := decideVerificationDemand(ctx, tc, []string{"solve.py"}); !d.Met {
		t.Fatalf("both obligations were discharged, got %+v", d)
	}
}

func TestOneRecordCarryingBothIdentitiesClearsBoth(t *testing.T) {
	tc := sepContract([]string{"solve.py"}, []string{"python3 solve.py"})
	ctx, dir := sepCtx(t, tc)
	h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
	ctx.VerificationEvidence = []VerificationRecord{
		sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
	}
	if d := decideVerificationDemand(ctx, tc, []string{"solve.py"}); !d.Met {
		t.Fatalf("one record named the command AND the bytes, got %+v", d)
	}
}

func TestRunAMutateRunBLeavesAStale(t *testing.T) {
	tc := sepContract([]string{"solve.py"}, []string{"pytest", "make check"})
	ctx, dir := sepCtx(t, tc)
	sepWrite(t, ctx, dir, "solve.py", "print('one')\n")
	a := sepStamp(ctx, "pytest", nil)

	// A material mutation between the two runs.
	h := sepWrite(t, ctx, dir, "solve.py", "print('two')\n")
	b := sepStamp(ctx, "make check", nil)
	cover := sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h})
	ctx.VerificationEvidence = []VerificationRecord{a, b, cover}

	d := decideVerificationDemand(ctx, tc, []string{"solve.py"})
	if d.Met {
		t.Fatal("the run from before the mutation was revived")
	}
	if d.Missing != "pytest" {
		t.Fatalf("the stale command should be the unmet one, got %q", d.Missing)
	}
}

func TestALaterUnrelatedMutationStalesAPathlessCommand(t *testing.T) {
	tc := sepContract([]string{"solve.py"}, []string{"pytest"})
	ctx, dir := sepCtx(t, tc)
	h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
	ctx.VerificationEvidence = []VerificationRecord{
		sepStamp(ctx, "pytest", nil),
		sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
	}
	if d := decideVerificationDemand(ctx, tc, []string{"solve.py"}); !d.Met {
		t.Fatalf("precondition: %+v", d)
	}
	sepWrite(t, ctx, dir, "notes.py", "x = 1\n")
	if d := decideVerificationDemand(ctx, tc, []string{"solve.py"}); d.Met {
		t.Fatal("a workspace mutation left a pathless command current")
	}
}

func TestAStampFromAnotherWorkspaceCannotSatisfy(t *testing.T) {
	tc := sepContract([]string{"solve.py"}, []string{"pytest"})
	ctx, dir := sepCtx(t, tc)
	h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
	other := VerificationRecord{Command: "pytest",
		WorkspaceGeneration: 99, WorkspaceStateHash: contentSHA256("another session")}
	ctx.VerificationEvidence = []VerificationRecord{other,
		sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h})}
	if d := decideVerificationDemand(ctx, tc, []string{"solve.py"}); d.Met {
		t.Fatal("a record stamped by another workspace satisfied an obligation")
	}
}

func TestExactCommandMatchingStaysByteForByte(t *testing.T) {
	tc := sepContract([]string{"solve.py"}, []string{"python3  solve.py"})
	ctx, dir := sepCtx(t, tc)
	h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
	ctx.VerificationEvidence = []VerificationRecord{
		sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
	}
	if d := decideVerificationDemand(ctx, tc, []string{"solve.py"}); d.Met {
		t.Fatal("two spaces is a different command")
	}
}

func TestEveryDeclaredCommandIsRequiredIndependently(t *testing.T) {
	tc := sepContract([]string{"solve.py"}, []string{"pytest", "make check"})
	ctx, dir := sepCtx(t, tc)
	h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
	ctx.VerificationEvidence = []VerificationRecord{
		sepStamp(ctx, "pytest", nil),
		sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
	}
	d := decideVerificationDemand(ctx, tc, []string{"solve.py"})
	if d.Met || d.Missing != "make check" {
		t.Fatalf("want make check unmet, got %+v", d)
	}
}

func TestEveryDeclaredOutputNeedsItsOwnCoverage(t *testing.T) {
	tc := sepContract([]string{"a.py", "b.py"}, []string{"pytest"})
	ctx, dir := sepCtx(t, tc)
	ha := sepWrite(t, ctx, dir, "a.py", "print('a')\n")
	sepWrite(t, ctx, dir, "b.py", "print('b')\n")
	ctx.VerificationEvidence = []VerificationRecord{
		sepStamp(ctx, "pytest", nil),
		sepStamp(ctx, "python3 a.py", map[string]string{"a.py": ha}),
	}
	if d := decideVerificationDemand(ctx, tc, []string{"a.py", "b.py"}); d.Met {
		t.Fatal("b.py had no covering run")
	}
}

func TestDuplicateDeclaredCommandsDeduplicateDeterministically(t *testing.T) {
	tc := sepContract([]string{"solve.py"}, []string{"pytest", "pytest"})
	ctx, dir := sepCtx(t, tc)
	h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
	ctx.VerificationEvidence = []VerificationRecord{
		sepStamp(ctx, "pytest", nil),
		sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
	}
	if d := decideVerificationDemand(ctx, tc, []string{"solve.py"}); !d.Met {
		t.Fatalf("one run discharges a command declared twice, got %+v", d)
	}
}

func TestOpaqueCommandsSatisfyTheirOwnObligationWithoutPathInvention(t *testing.T) {
	for _, cmd := range []string{"pytest", "make check", "./scripts/verify.sh"} {
		tc := sepContract([]string{"solve.py"}, []string{cmd})
		ctx, dir := sepCtx(t, tc)
		h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
		rec := sepStamp(ctx, cmd, nil)
		if len(rec.Covered) != 0 {
			t.Fatalf("%q produced coverage out of nowhere: %v", cmd, rec.Covered)
		}
		ctx.VerificationEvidence = []VerificationRecord{rec,
			sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h})}
		if d := decideVerificationDemand(ctx, tc, []string{"solve.py"}); !d.Met {
			t.Fatalf("%q: %+v", cmd, d)
		}
	}
}

// --- structural rejection of untrustworthy execution ------------------------

func TestATimedOutCommandIsRejectedStructurally(t *testing.T) {
	out := RunCommandOutput{ExitCode: 0, TimedOut: true}
	if runCommandVerifiable(out) {
		t.Fatal("a timed-out run was treated as verification")
	}
	if !runCommandVerifiable(RunCommandOutput{ExitCode: 0}) {
		t.Fatal("a clean run was refused")
	}
	if runCommandVerifiable(RunCommandOutput{ExitCode: 1}) {
		t.Fatal("a nonzero exit was treated as verification")
	}
}

func TestAMutatingCommandIsStampedWithThePostCommandState(t *testing.T) {
	tc := sepContract([]string{"solve.py"}, []string{"./build.sh"})
	ctx, dir := sepCtx(t, tc)
	sepWrite(t, ctx, dir, "solve.py", "print('one')\n")
	beforeGen, beforeState := workspaceIdentity(ctx)

	// The command rewrites a tracked artifact behind the tool layer's back --
	// which is the whole reason a shell effect is reconciled rather than
	// trusted.
	if err := os.WriteFile(filepath.Join(dir, "solve.py"), []byte("print('two')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordLedgerEffect("run_command", []byte(`{"command":"./build.sh"}`), ctx,
		&ToolResult{Success: true})

	afterGen, afterState := workspaceIdentity(ctx)
	if afterGen == beforeGen && afterState == beforeState {
		t.Fatal("the command's own effect never reached the ledger; a stamp taken " +
			"now would certify the state the command found, not the one it left")
	}
	rec := sepStamp(ctx, "./build.sh", nil)
	ctx.VerificationEvidence = []VerificationRecord{rec}
	if !commandEvidenceCurrent(ctx, rec) {
		t.Fatal("the post-command stamp is not current against the post-command state")
	}
	stale := VerificationRecord{Command: "./build.sh",
		WorkspaceGeneration: beforeGen, WorkspaceStateHash: beforeState}
	if commandEvidenceCurrent(ctx, stale) {
		t.Fatal("a pre-command stamp was accepted as current")
	}
}

func TestAnUnstampedRecordCannotSatisfyACommandObligation(t *testing.T) {
	tc := sepContract([]string{"solve.py"}, []string{"pytest"})
	ctx, dir := sepCtx(t, tc)
	h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
	ctx.VerificationEvidence = []VerificationRecord{
		{Command: "pytest"},
		sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
	}
	if d := decideVerificationDemand(ctx, tc, []string{"solve.py"}); d.Met {
		t.Fatal("a record with no workspace identity satisfied a pathless command")
	}
}

func TestCoverageFromAnUnrelatedPathDoesNotCount(t *testing.T) {
	tc := sepContract([]string{"solve.py"}, []string{"pytest"})
	ctx, dir := sepCtx(t, tc)
	sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
	other := sepWrite(t, ctx, dir, "other.py", "print('other')\n")
	ctx.VerificationEvidence = []VerificationRecord{
		sepStamp(ctx, "pytest", nil),
		sepStamp(ctx, "python3 other.py", map[string]string{"other.py": other}),
	}
	if d := decideVerificationDemand(ctx, tc, []string{"solve.py"}); d.Met {
		t.Fatal("coverage of another artifact satisfied solve.py")
	}
}

func TestTheSandboxTimeoutFlagIsDecoded(t *testing.T) {
	body := `{"success":false,"stdout":"","stderr":"Execution timed out after 5s","exit_code":-1,"elapsed_ms":5000,"timed_out":true}`
	var out RunCommandOutput
	if err := decodeShellResponse(strings.NewReader(body), &out); err != nil {
		t.Fatal(err)
	}
	if !out.TimedOut {
		t.Fatal("timed_out was not decoded; the state would have to be read out of stderr prose")
	}
}
