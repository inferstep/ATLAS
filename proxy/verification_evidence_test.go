package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A command the client required, run exactly, against these exact bytes.
//
// Everything here is a way of getting less than that and being told so. No
// candidate is delivered: authority is computed and never consulted.

const declaredCmd = "python3 solve.py"

// verificationFixture is a workspace with one candidate on disk, a client that
// declared one command, and a green record of that command naming the file.
type verificationFixture struct {
	ctx  *AgentContext
	obl  taskObligation
	path string
	hash string
}

func newVerificationFixture(t *testing.T, commands ...string) *verificationFixture {
	t.Helper()
	if len(commands) == 0 {
		commands = []string{declaredCmd}
	}
	dir := t.TempDir()
	const code = "print(7)\n"
	if err := os.WriteFile(filepath.Join(dir, "solve.py"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-fixture")

	quoted := make([]string, 0, len(commands))
	for _, c := range commands {
		b, _ := json.Marshal(c)
		quoted = append(quoted, string(b))
	}
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","verification_knowledge":"declared","verification":[`+
			strings.Join(quoted, ",")+`]}`)

	resolved := resolveAgentPath(ctx, "solve.py")
	hash := fileSHA256(ctx, resolved)
	if hash == "" {
		t.Fatal("no hash for the candidate on disk")
	}
	obl, ok := newTaskObligation(ObligationDeclaredCommand, commands[0], VerificationKindRuntime, true)
	if !ok {
		t.Fatal("declared command obligation refused")
	}
	return &verificationFixture{ctx: ctx, obl: obl, path: resolved, hash: hash}
}

// request is the shape the wiring hands the producer: the client's obligation,
// one staged observation of that command, and the identity the staging run was
// bound to. Nothing in it is a conclusion.
func (f *verificationFixture) request() verificationEvidenceRequest {
	generation, stateHash := workspaceIdentity(f.ctx)
	return verificationEvidenceRequest{
		Obligation: f.obl,
		Result: stagingCommandResult{
			CommandIdentity:  contentSHA256(f.obl.Subject),
			ObligationID:     f.obl.ID,
			Index:            0,
			Count:            1,
			Outcome:          stagingExitedZero,
			TargetHashBefore: f.hash, TargetHashAfter: f.hash,
			WorkspaceHashBefore: "ws-before", WorkspaceHashAfter: "ws-before",
		},
		Identity: stagingIdentity{
			RequestID:           "req-fixture",
			InvocationID:        "inv-1",
			CandidateInstanceID: "cand-1",
			CandidateHash:       f.hash,
			TargetPath:          f.path,
			BaselineIdentity:    baselineIdentityFor(f.ctx, f.path),
			WorkspaceGeneration: generation,
			WorkspaceStateHash:  stateHash,
		},
	}
}

// bumpWorkspace records a change to a path the way the write path does, so
// workspaceIdentity moves. Editing the file alone does not: the identity is
// what the proxy has recorded about the workspace, not a walk of the disk.
func bumpWorkspace(ctx *AgentContext, path, hash string) {
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*DeliverableState{}
	}
	d := ctx.Ledger[path]
	if d == nil {
		d = &DeliverableState{Path: path}
		ctx.Ledger[path] = d
	}
	d.CurrentHash = hash
	d.Generation++
}

// --- the positive case -------------------------------------------------------

func TestAnExactDeclaredCommandAgainstExactCandidateBytesIsBehavioral(t *testing.T) {
	f := newVerificationFixture(t)
	ev, ok := produceDeclaredVerificationEvidence(f.ctx, f.request())
	if !ok {
		t.Fatal("an exact declared command against exact bytes produced nothing")
	}
	if ev.Outcome != ValidationPassed {
		t.Errorf("outcome %q, want passed", ev.Outcome)
	}
	p := ev.Provenance
	if p.Source != ProvenanceClientDeclaredVerification {
		t.Errorf("source %q, want client_declared_verification", p.Source)
	}
	// The fixture declares the command without a type, so what it demands and
	// what it demonstrates are both runtime: it ran against these exact bytes
	// and exited zero.
	if p.ObservedStrength != VerificationKindRuntime ||
		p.RequiredStrength != VerificationKindRuntime {
		t.Errorf("strengths %q/%q, want runtime/runtime",
			p.RequiredStrength, p.ObservedStrength)
	}
	if p.CandidateHash != f.hash {
		t.Error("evidence does not name the exact candidate bytes")
	}
	if p.CommandIdentity != contentSHA256(declaredCmd) {
		t.Error("evidence does not name its command")
	}
	if p.WorkspaceStateHash == "" || p.RequestID != "req-fixture" {
		t.Errorf("identity incomplete: %+v", p)
	}
	if authorized, why := ev.Authorizes(); !authorized {
		t.Errorf("a green declared command did not authorize its obligation: %s", why)
	}
}

// TestADeclaredCommandIsNeverLabelledAnOracle pins the strength ceiling: exit
// zero says the command ran and succeeded, not that an answer was checked
// against a reference.
func TestADeclaredCommandIsNeverLabelledAnOracle(t *testing.T) {
	f := newVerificationFixture(t)
	ev, ok := produceDeclaredVerificationEvidence(f.ctx, f.request())
	if !ok {
		t.Fatal("no evidence to test")
	}
	if ev.Provenance.ObservedStrength == "oracle" {
		t.Error("an arbitrary exit-zero command was labelled oracle evidence")
	}
	oracleObligation, _ := newTaskObligation(ObligationDeclaredExample, "case-1", "", true)
	forged := ev
	forged.Provenance.ObligationID = oracleObligation.ID
	forged.Provenance.RequiredStrength = "oracle"
	if authorized, _ := forged.Authorizes(); authorized {
		t.Error("behavioral evidence closed an oracle obligation")
	}
}

// --- provenance comes from the declaration, not from the text ----------------

func TestTheSameTextFromTheModelRemainsUntrusted(t *testing.T) {
	// The client declared nothing. The model ran a command that looks exactly
	// like verification and it passed against the exact bytes.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "solve.py"), []byte("print(7)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-fixture")
	ctx.TaskContract = mustContract(t, dir, `{"task_mode":"work"}`)

	resolved := resolveAgentPath(ctx, "solve.py")
	hash := fileSHA256(ctx, resolved)
	obl, _ := newTaskObligation(ObligationDeclaredCommand, declaredCmd, VerificationKindRuntime, true)
	generation, stateHash := workspaceIdentity(ctx)
	req := verificationEvidenceRequest{
		Obligation: obl,
		Result: stagingCommandResult{
			CommandIdentity: contentSHA256(declaredCmd), ObligationID: obl.ID,
			Count: 1, Outcome: stagingExitedZero,
			TargetHashBefore: hash, TargetHashAfter: hash,
			WorkspaceHashBefore: "ws", WorkspaceHashAfter: "ws",
		},
		Identity: stagingIdentity{
			RequestID: "req-fixture", InvocationID: "inv-1",
			CandidateInstanceID: "cand-1", CandidateHash: hash,
			TargetPath: resolved, BaselineIdentity: baselineIdentityFor(ctx, resolved),
			WorkspaceGeneration: generation, WorkspaceStateHash: stateHash,
		},
	}
	if _, ok := produceDeclaredVerificationEvidence(ctx, req); ok {
		t.Error("a command the client never declared produced trusted evidence")
	}
}

func TestUnspecifiedVerificationKnowledgeDeclaresNothing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "solve.py"), []byte("print(7)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-fixture")
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","verification_knowledge":"unspecified"}`)
	if requestDeclaredCommand(ctx, declaredCmd) {
		t.Error("an unspecified caller was read as declaring a command")
	}
}

func TestASimilarButNotIdenticalCommandFailsBinding(t *testing.T) {
	for _, ran := range []string{
		"python3  solve.py",   // an extra space
		"python3 solve.py ",   // a trailing space
		"python3 ./solve.py",  // a different spelling of the path
		"python3 solve.py -v", // an extra flag
		"python solve.py",     // a different interpreter
	} {
		f := newVerificationFixture(t)
		req := f.request()
		req.Result.CommandIdentity = contentSHA256(ran)
		if _, ok := produceDeclaredVerificationEvidence(f.ctx, req); ok {
			t.Errorf("%q was accepted for the declared %q", ran, declaredCmd)
		}
	}
}

// --- what the run was actually about -----------------------------------------

func TestASuccessfulUnrelatedCommandProvesNothing(t *testing.T) {
	f := newVerificationFixture(t)
	t.Run("saw no bytes at the target", func(t *testing.T) {
		req := f.request()
		req.Result.TargetHashBefore = ""
		if _, ok := produceDeclaredVerificationEvidence(f.ctx, req); ok {
			t.Error("a run that observed nothing at the target authorized a candidate")
		}
	})
	t.Run("saw other bytes at the target", func(t *testing.T) {
		req := f.request()
		req.Result.TargetHashBefore = contentSHA256("print(1)\n")
		if _, ok := produceDeclaredVerificationEvidence(f.ctx, req); ok {
			t.Error("a run against other bytes authorized this candidate")
		}
	})
}

func TestACommandThatPassedBeforeInsertionCannotAuthorizeTheCandidate(t *testing.T) {
	f := newVerificationFixture(t)
	// The command passed against the file as it was, then the candidate
	// replaced it. The record still names the older bytes.
	oldHash := f.hash
	if err := os.WriteFile(f.path, []byte("print(8)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newHash := fileSHA256(f.ctx, f.path)
	if newHash == oldHash {
		t.Fatal("the candidate did not change")
	}
	req := f.request()
	// The staged run observed the OLD bytes; the candidate is the new ones.
	req.Result.TargetHashBefore = oldHash
	req.Result.TargetHashAfter = oldHash
	req.Identity.CandidateHash = newHash
	if _, ok := produceDeclaredVerificationEvidence(f.ctx, req); ok {
		t.Error("a run from before the candidate landed authorized it")
	}
}

func TestAWorkspaceMutationAfterExecutionInvalidatesEvidence(t *testing.T) {
	f := newVerificationFixture(t)
	// The staging run was bound to the workspace as it stood when it started.
	req := f.request()
	// It moved while the command was running.
	if err := os.WriteFile(f.path, []byte("print(9)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bumpWorkspace(f.ctx, f.path, contentSHA256("print(9)\n"))
	liveGen, liveHash := workspaceIdentity(f.ctx)
	if liveHash == req.Identity.WorkspaceStateHash {
		t.Fatal("the workspace did not actually move")
	}

	ev, ok := produceDeclaredVerificationEvidence(f.ctx, req)
	if !ok {
		t.Fatal("no evidence to test")
	}
	// The record names the workspace the run SAW, not the one it was written
	// in. A producer that re-read the live state here would leave every record
	// self-consistent and make staleness invisible to every reader.
	if ev.Provenance.WorkspaceStateHash != req.Identity.WorkspaceStateHash {
		t.Error("the record followed the workspace instead of recording it")
	}
	if ev.Provenance.WorkspaceGeneration != req.Identity.WorkspaceGeneration {
		t.Error("the record's generation followed the workspace")
	}
	asked := ev.Provenance
	asked.WorkspaceGeneration, asked.WorkspaceStateHash = liveGen, liveHash
	if bound, _ := ev.Provenance.BindsTo(asked); bound {
		t.Error("evidence survived a mutation of the workspace it vouched for")
	}
}

func TestACandidateHashMismatchInvalidatesEvidence(t *testing.T) {
	f := newVerificationFixture(t)
	req := f.request()
	req.Identity.CandidateHash = contentSHA256("some other candidate")
	if _, ok := produceDeclaredVerificationEvidence(f.ctx, req); ok {
		t.Error("evidence was produced about a candidate the run never covered")
	}
}

func TestABaselineMismatchInvalidatesEvidence(t *testing.T) {
	f := newVerificationFixture(t)
	req := f.request()
	req.Identity.BaselineIdentity = "behavioral:baseline-a"
	ev, ok := produceDeclaredVerificationEvidence(f.ctx, req)
	if !ok {
		t.Fatal("no evidence to test")
	}
	asked := ev.Provenance
	asked.BaselineIdentity = "behavioral:baseline-b"
	if bound, _ := ev.Provenance.BindsTo(asked); bound {
		t.Error("evidence earned against one baseline bound to another")
	}
}

// --- every way a command can end without authorizing -------------------------

func TestOnlyACleanForegroundSuccessAuthorizes(t *testing.T) {
	for _, outcome := range []stagingCommandOutcome{
		stagingExitedNonZero, stagingTimedOut, stagingCancelled, stagingRefused,
		stagingMutatedTarget, stagingMutatedWorkspace, stagingUnobservable,
		stagingBudgetExceeded, stagingUnavailable,
	} {
		f := newVerificationFixture(t)
		req := f.request()
		req.Result.Outcome = outcome
		ev, ok := produceDeclaredVerificationEvidence(f.ctx, req)
		if !ok {
			continue // declining outright is also a correct answer
		}
		if authorized, _ := ev.Authorizes(); authorized {
			t.Errorf("%s authorized its obligation", outcome)
		}
		if ev.Outcome == ValidationPassed {
			t.Errorf("%s was recorded as a pass", outcome)
		}
	}
}

// TestAnExitZeroThatChangedSomethingIsNotAPass is the case only staging can
// see: the command succeeded, and it succeeded by rewriting the thing it was
// meant to be testing, or something else it could reach.
func TestAnExitZeroThatChangedSomethingIsNotAPass(t *testing.T) {
	for _, c := range []struct {
		name                     string
		target, workspaceChanged bool
	}{
		{"rewrote the candidate", true, true},
		{"changed an input", false, true},
	} {
		f := newVerificationFixture(t)
		req := f.request()
		req.Result.Outcome = stagingExitedZero
		req.Result.MutatedTarget = c.target
		req.Result.MutatedWorkspace = c.workspaceChanged
		if c.target {
			req.Result.TargetHashAfter = contentSHA256("rewritten\n")
		}
		ev, ok := produceDeclaredVerificationEvidence(f.ctx, req)
		if !ok {
			continue
		}
		if ev.Outcome == ValidationPassed {
			t.Errorf("a command that %s was recorded as a pass", c.name)
		}
		if authorized, _ := ev.Authorizes(); authorized {
			t.Errorf("a command that %s authorized its obligation", c.name)
		}
	}
}

func TestAnUnknownOutcomeValueFailsClosed(t *testing.T) {
	f := newVerificationFixture(t)
	req := f.request()
	req.Result.Outcome = stagingCommandOutcome("something_new")
	if _, ok := produceDeclaredVerificationEvidence(f.ctx, req); ok {
		t.Error("an unrecognised outcome produced evidence")
	}
}

func TestACancelledRunProducesNothing(t *testing.T) {
	f := newVerificationFixture(t)
	cancelled, cancel := context.WithCancel(
		context.WithValue(context.Background(), requestIDKey, "req-fixture"))
	cancel()
	f.ctx.Ctx = cancelled
	if _, ok := produceDeclaredVerificationEvidence(f.ctx, f.request()); ok {
		t.Error("a cancelled run produced evidence")
	}
}

func TestAMissingIdentityProducesNothing(t *testing.T) {
	for _, mut := range []func(*verificationEvidenceRequest){
		func(r *verificationEvidenceRequest) { r.Identity.InvocationID = "" },
		func(r *verificationEvidenceRequest) { r.Identity.CandidateInstanceID = "" },
		func(r *verificationEvidenceRequest) { r.Identity.TargetPath = "" },
		func(r *verificationEvidenceRequest) { r.Identity.CandidateHash = "" },
		func(r *verificationEvidenceRequest) { r.Identity.RequestID = "" },
		func(r *verificationEvidenceRequest) { r.Identity.WorkspaceStateHash = "" },
		func(r *verificationEvidenceRequest) { r.Result.ObligationID = "other" },
	} {
		f := newVerificationFixture(t)
		req := f.request()
		mut(&req)
		if _, ok := produceDeclaredVerificationEvidence(f.ctx, req); ok {
			t.Error("evidence was produced with an incomplete identity")
		}
	}
}

func TestTheProducerRefusesAnyOtherObligationKind(t *testing.T) {
	f := newVerificationFixture(t)
	for _, kind := range []string{
		ObligationSyntacticValidity, ObligationArtifactExists,
		ObligationDeclaredExample, ObligationBaselinePreserved,
		ObligationUnsupported,
	} {
		baseline := ""
		if kind == ObligationBaselinePreserved {
			baseline = "behavioral"
		}
		o, ok := newTaskObligation(kind, declaredCmd, baseline, true)
		if !ok {
			continue
		}
		req := f.request()
		req.Obligation = o
		req.Result.ObligationID = o.ID
		if _, ok := produceDeclaredVerificationEvidence(f.ctx, req); ok {
			t.Errorf("a declared command described a %s obligation", kind)
		}
	}
}

// --- obligations stay independent --------------------------------------------

func TestOneOfTwoCommandsPassingDoesNotCompleteBoth(t *testing.T) {
	f := newVerificationFixture(t, declaredCmd, "ruff check .")
	second, ok := newTaskObligation(ObligationDeclaredCommand, "ruff check .", VerificationKindRuntime, true)
	if !ok {
		t.Fatal("second obligation refused")
	}
	ev, ok := produceDeclaredVerificationEvidence(f.ctx, f.request())
	if !ok {
		t.Fatal("the first command produced no evidence")
	}
	met, missing := declaredVerificationCoverage(
		[]taskObligation{f.obl, second}, []proxyEvidence{ev})
	if len(met) != 1 || met[0] != f.obl.ID {
		t.Errorf("met %v, want only the command that ran", met)
	}
	if len(missing) != 1 || missing[0] != second.ID {
		t.Errorf("missing %v, want the command that did not run", missing)
	}
}

func TestAllRequiredCommandsMustBeRepresented(t *testing.T) {
	f := newVerificationFixture(t, declaredCmd, "ruff check .")
	second, _ := newTaskObligation(ObligationDeclaredCommand, "ruff check .", VerificationKindRuntime, true)

	secondReq := f.request()
	secondReq.Obligation = second
	secondReq.Result.ObligationID = second.ID
	secondReq.Result.CommandIdentity = contentSHA256("ruff check .")
	secondEv, ok := produceDeclaredVerificationEvidence(f.ctx, secondReq)
	if !ok {
		t.Fatal("the second command produced no evidence")
	}
	firstEv, _ := produceDeclaredVerificationEvidence(f.ctx, f.request())

	met, missing := declaredVerificationCoverage(
		[]taskObligation{f.obl, second}, []proxyEvidence{firstEv, secondEv})
	if len(met) != 2 || len(missing) != 0 {
		t.Errorf("met %v missing %v, want both met", met, missing)
	}
}

func TestAFailedObservationDoesNotCoverItsObligation(t *testing.T) {
	f := newVerificationFixture(t)
	req := f.request()
	req.Result.Outcome = stagingExitedNonZero
	ev, ok := produceDeclaredVerificationEvidence(f.ctx, req)
	if !ok {
		t.Skip("the producer declined outright, which is also correct")
	}
	_, missing := declaredVerificationCoverage([]taskObligation{f.obl}, []proxyEvidence{ev})
	if len(missing) != 1 {
		t.Error("a failed run covered its obligation")
	}
}

// --- evidence cannot cross a request or an invocation ------------------------

func TestConcurrentRequestsAndInvocationsCannotExchangeEvidence(t *testing.T) {
	a := newVerificationFixture(t)
	b := newVerificationFixture(t)
	b.ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-other")

	evA, ok := produceDeclaredVerificationEvidence(a.ctx, a.request())
	if !ok {
		t.Fatal("fixture A produced no evidence")
	}
	reqB := b.request()
	reqB.Identity.RequestID = "req-other"
	reqB.Identity.InvocationID = "inv-2"
	reqB.Identity.CandidateInstanceID = "cand-2"
	evB, ok := produceDeclaredVerificationEvidence(b.ctx, reqB)
	if !ok {
		t.Fatal("fixture B produced no evidence")
	}
	if bound, _ := evA.Provenance.BindsTo(evB.Provenance); bound {
		t.Error("one request's evidence bound to another's")
	}
	if bound, _ := evB.Provenance.BindsTo(evA.Provenance); bound {
		t.Error("one invocation's evidence bound to another's")
	}
}

// --- no leaks, no execution, no hidden evaluator -----------------------------

func TestNeitherCommandStringsNorSourceBytesTravel(t *testing.T) {
	const secret = "pytest --token=hunter2 -q"
	f := newVerificationFixture(t, secret)
	ev, ok := produceDeclaredVerificationEvidence(f.ctx, f.request())
	if !ok {
		t.Fatal("no evidence to test")
	}
	blob, err := json.Marshal(ev.Provenance)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{secret, "hunter2", "print(7)"} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the serialised binding carries %q", needle)
		}
	}
}

// TestTheProducerExecutesNothing pins that running a command stays the tool
// path's job, with its own safety checks, permission endpoint and sandbox. A
// producer that executed would be a second route with a second set of rules.
func TestTheProducerExecutesNothing(t *testing.T) {
	src, err := os.ReadFile("verification_evidence.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "verification_evidence.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{
		"executeCommand": true, "runCommand": true, "runInSandbox": true,
		"exec": true, "Command": true, "CommandContext": true, "Start": true,
		"awaitPermission": true, "requestPermission": true,
		"deleteFile": true, "removeAll": true, "RemoveAll": true, "Remove": true,
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if banned[name] {
			t.Errorf("the producer calls %s: execution, permission and deletion "+
				"belong to the tool path", name)
		}
		return true
	})
}

// TestNoHiddenEvaluatorIsReachableFromTheProducers pins that the benchmark's
// own grader has no representation any producer could name.
func TestNoHiddenEvaluatorIsReachableFromTheProducers(t *testing.T) {
	for _, path := range []string{
		"verification_evidence.go", "syntax_evidence.go", "obligation_kinds.go",
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		low := strings.ToLower(string(body))
		for _, banned := range []string{
			"holdout", "hidden_evaluator", "benchmark_grader", "reference_answer",
			"expected_output", "goldenanswer",
		} {
			if strings.Contains(low, banned) {
				t.Errorf("%s names %q", path, banned)
			}
		}
	}
}

// TestTheOnlyProductionWriterOfVerificationEvidenceIsTheGreenBranch pins where
// the consumed records come from: one append, in the branch that already
// requires a successful foreground run.
func TestTheOnlyProductionWriterOfVerificationEvidenceIsTheGreenBranch(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	writers := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		writers[name] = strings.Count(string(body), "ctx.VerificationEvidence = append")
	}
	total := 0
	for name, n := range writers {
		total += n
		if n > 0 && name != "agent.go" {
			t.Errorf("%s writes verification evidence; only the agent loop's green "+
				"branch may", name)
		}
	}
	if total != 1 {
		t.Errorf("%d production writers of verification evidence, want exactly 1", total)
	}
}
