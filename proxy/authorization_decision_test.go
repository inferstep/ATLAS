package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The observe-only authorization matrix.
//
// Each row is a shape a candidate can arrive in, answered by the typed decision,
// and none of them delivers anything. A row that comes back authorized
// describes what would land if a consumer existed; the guards in
// evidence_wiring_test.go prove none does.

type authWorld struct {
	*matrixWorld
}

func newAuthWorld(t *testing.T, contract, filename, code string, valid bool) *authWorld {
	t.Helper()
	w := newMatrixWorld(t, contract, filename, code, valid)
	w.ctx.HumanTask = "Create it."
	return &authWorld{matrixWorld: w}
}

// observe runs the wired producer, as production does.
func (w *authWorld) observe(t *testing.T) (proxyEvidence, candidateEvidenceIdentity, bool) {
	t.Helper()
	outcome := fallbackSyntaxOutcomeFor(w.ctx, w.path, w.code).aggregate()
	return observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, w.code, outcome)
}

func (w *authWorld) mustObserve(t *testing.T) (proxyEvidence, candidateEvidenceIdentity) {
	t.Helper()
	ev, id, ok := w.observe(t)
	if !ok {
		t.Fatal("the wired producer saw nothing")
	}
	return ev, id
}

// stage runs the client-declared producer through the real staging wiring
// against the world's executor, as production does.
func (w *authWorld) stage(id candidateEvidenceIdentity) []proxyEvidence {
	behavioral, _, _ := observeCandidateVerification(w.ctx, w.path, w.code, id, mutationScope{})
	return behavioral
}

// decide asks the decision about the candidate the producer just observed, as
// production does: the asked-for identity is the one the producer minted, and
// the evidence has to match it.
func (w *authWorld) decide(id candidateEvidenceIdentity, evidence ...proxyEvidence) AuthorizationDecision {
	return w.authorize(id, nil, evidence...).Decision
}

// authorize runs the live owner, so the matrix describes what production
// concludes rather than a parallel computation that could drift.
func (w *authWorld) authorize(id candidateEvidenceIdentity,
	envelope *V3EvidenceEnvelope, evidence ...proxyEvidence) deliveryAuthorization {
	// The real observation, so a row that refuses is refused for the reason
	// production would give rather than for a zero value.
	observed := fallbackSyntaxOutcomeFor(w.ctx, w.path, w.code).aggregate()
	entry := mintRouteEntry(w.ctx)
	return authorizeCandidateDelivery(w.ctx, entry, w.path, w.code, id, envelope,
		evidence, "selected", nil, observed,
		testMutationScope(w.ctx, entry, w.path, w.code), automaticIntent{})
}

// authorizeCandidateDeliveryDecision is the live owner's decision, for rows
// that pass their own path, code or envelope rather than the world's.
func authorizeCandidateDeliveryDecision(ctx *AgentContext, path, code string,
	id candidateEvidenceIdentity, envelope *V3EvidenceEnvelope,
	evidence []proxyEvidence) AuthorizationDecision {
	observed := fallbackSyntaxOutcomeFor(ctx, path, code).aggregate()
	entry := mintRouteEntry(ctx)
	return authorizeCandidateDelivery(ctx, entry, path, code, id, envelope,
		evidence, "selected", nil, observed,
		testMutationScope(ctx, entry, path, code), automaticIntent{}).Decision
}

// stagedWorld is a world whose client declared commands, with an executor that
// answers them.
func stagedWorld(t *testing.T, commands ...string) *authWorld {
	t.Helper()
	return stagedWorldWithCode(t, authPy, commands...)
}

func stagedWorldWithCode(t *testing.T, code string, commands ...string) *authWorld {
	t.Helper()
	quoted := make([]string, 0, len(commands))
	for _, c := range commands {
		b, _ := json.Marshal(c)
		quoted = append(quoted, string(b))
	}
	return newAuthWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":[`+
			strings.Join(quoted, ",")+`]}`,
		"solve.py", code, true)
}

func expectReason(t *testing.T, row string, d AuthorizationDecision,
	authorized bool, want AuthorizationReason) {
	t.Helper()
	if d.Authorized != authorized {
		t.Errorf("%s: authorized=%v, want %v (reason %q)", row, d.Authorized, authorized, d.Reason)
	}
	if d.Reason != want {
		t.Errorf("%s: reason %q, want %q", row, d.Reason, want)
	}
	if !authorizationReasons[d.Reason] {
		t.Errorf("%s: %q is outside the closed vocabulary", row, d.Reason)
	}
}

// influences_live_decision must say truthfully whether the typed path decides
// delivery for this request.
//
// It is a property of the request, not of the outcome. A request that declared
// outputs, or declared work so the model's own call names the target, is one
// this path owns however the decision comes out. A question, or a request with
// no contract, is one it has nothing to say about.
func TestTheDecisionSaysTruthfullyWhetherItDecides(t *testing.T) {
	for _, tc := range []struct {
		name, contract string
		influences     bool
	}{
		{"declared outputs", `{"task_mode":"work","output_knowledge":"declared",` +
			`"expected_outputs":["solve.py"]}`, true},
		{"declared outputs and commands", `{"task_mode":"work","output_knowledge":"declared",` +
			`"expected_outputs":["solve.py"],"verification_knowledge":"declared",` +
			`"verification":["pytest -q"]}`, true},
		{"task mode only", `{"task_mode":"work"}`, true},
		{"verification declared, outputs not", `{"task_mode":"work",` +
			`"verification_knowledge":"declared","verification":["pytest -q"]}`, true},
		{"declared and authoritatively empty", `{"task_mode":"work",` +
			`"output_knowledge":"declared","expected_outputs":[]}`, true},
		{"declared nothing", `{"task_mode":"work","output_knowledge":"unspecified"}`, true},
		{"a question", `{"task_mode":"question"}`, false},
		{"no contract at all", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newAuthWorld(t, tc.contract, "solve.py", authPy, true)
			ev, evID, _ := w.observe(t)
			a := w.authorize(evID, nil, ev)
			if a.Decision.InfluencesLiveDecision != tc.influences {
				t.Errorf("influences_live_decision=%v, want %v",
					a.Decision.InfluencesLiveDecision, tc.influences)
			}
			// The typed route is exactly the set that influences: a request
			// this path does not own is left to the decision that always made it.
			if a.Typed != tc.influences {
				t.Errorf("typed=%v, want %v", a.Typed, tc.influences)
			}
		})
	}
}

const authPy = "print(7)\n"

// declaredPy is the contract most matrix rows run under: one declared Python output.
const declaredPy = `{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`

func TestDeclaredPythonOutputWithExactSyntaxEvidence(t *testing.T) {
	w := newAuthWorld(t, declaredPy, "solve.py", authPy, true)
	ev, evID := w.mustObserve(t)
	d := w.decide(evID, ev)
	expectReason(t, "exact syntax", d, true, ReasonAuthorized)
	if len(d.Satisfied) != 1 {
		t.Errorf("satisfied %v, want the one obligation", d.Satisfied)
	}
	if len(d.Missing) != 0 {
		t.Errorf("missing %v, want nothing owed", d.Missing)
	}
	if len(d.EvidenceConsumed) != 1 {
		t.Errorf("consumed %v, want the one record", d.EvidenceConsumed)
	}
	if !d.SettlementRequired {
		t.Error("existence is not yet settled and the decision does not say so")
	}
}

func TestDeclaredDocumentWithNoFabricatedSyntaxRequirement(t *testing.T) {
	w := newAuthWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["notes.md"]}`,
		"notes.md", "# notes\n", true)
	if _, _, ok := w.observe(t); ok {
		t.Error("a structural record was fabricated for a document")
	}
	d := w.decide(candidateEvidenceIdentity{})
	// The client asked for it and its class states nothing demonstrable, so
	// nothing authorizes it. Being the declared target is not evidence
	// about the bytes, and the refusal says so rather than calling a
	// declared document undeclared.
	expectReason(t, "document", d, false, ReasonNoAuthorizationPrerequisite)
	if len(d.Missing) != 0 {
		t.Errorf("missing %v, want no fabricated obligation", d.Missing)
	}
}

func TestDeclaredCommandsLeftUnstaged(t *testing.T) {
	w := stagedWorld(t, "pytest -q", "ruff check .")
	ev, evID := w.mustObserve(t)
	// The syntax obligation is met. Staging never ran for this candidate,
	// so each command has no observation and is owed.
	d := w.decide(evID, ev)
	expectReason(t, "unstaged commands", d, false, ReasonEvidenceMissing)
	if len(d.Satisfied) != 1 {
		t.Fatalf("satisfied %v, want the syntax obligation met", d.Satisfied)
	}
	if len(d.Missing) != 2 {
		t.Fatalf("missing %v, want both commands owed", d.Missing)
	}
	for _, id := range d.Missing {
		if !strings.HasPrefix(id, ObligationDeclaredCommand+":") {
			t.Errorf("missing %v, want the declared commands", d.Missing)
		}
	}
}

func TestExactDeclaredCommandInAGenuineCandidateWorkspace(t *testing.T) {
	w := stagedWorld(t, "pytest -q")
	ev, evID := w.mustObserve(t)
	behavioral := w.stage(evID)
	if len(behavioral) != 1 {
		t.Fatalf("staging produced %d records, want the one declared command",
			len(behavioral))
	}
	if w.shellRuns != 1 {
		t.Errorf("the executor ran %d times, want once per declared command",
			w.shellRuns)
	}
	if got := behavioral[0].Provenance.ObservedStrength; got != VerificationKindRuntime {
		t.Errorf("strength %q, want behavioral", got)
	}
	d := w.decide(evID, append([]proxyEvidence{ev}, behavioral...)...)
	expectReason(t, "staged declared command", d, true, ReasonAuthorized)
	if len(d.Missing) != 0 {
		t.Errorf("missing %v, want nothing owed", d.Missing)
	}
	if len(d.Satisfied) != 2 {
		t.Errorf("satisfied %v, want the syntax and the command", d.Satisfied)
	}
}

func TestDeclaredCommandThatFailedInStaging(t *testing.T) {
	w := stagedWorld(t, "pytest -q")
	w.shellExit = 1
	ev, evID := w.mustObserve(t)
	d := w.decide(evID, append([]proxyEvidence{ev}, w.stage(evID)...)...)
	expectReason(t, "failed declared command", d, false, ReasonEvidenceMissing)
}

func TestDeclaredCommandThatRewroteTheCandidate(t *testing.T) {
	w := stagedWorld(t, "pytest -q")
	w.shellMutate = true
	ev, evID := w.mustObserve(t)
	d := w.decide(evID, append([]proxyEvidence{ev}, w.stage(evID)...)...)
	if d.Authorized {
		t.Error("a command that rewrote its own subject authorized the candidate")
	}
}

func TestDeclaredCommandThatChangedAnInput(t *testing.T) {
	w := stagedWorld(t, "pytest -q")
	w.shellInput = true
	ev, evID := w.mustObserve(t)
	behavioral := w.stage(evID)
	if len(behavioral) != 1 || w.shellRuns != 1 {
		t.Fatalf("%d records from %d runs; the row did not reach staging",
			len(behavioral), w.shellRuns)
	}
	if behavioral[0].Outcome == ValidationPassed {
		t.Error("a command that changed its own inputs was recorded as a pass")
	}
	d := w.decide(evID, append([]proxyEvidence{ev}, behavioral...)...)
	if d.Authorized {
		t.Error("a command that changed its own inputs authorized the candidate")
	}
}

func TestDeclaredCommandThatTimedOutInStaging(t *testing.T) {
	w := stagedWorld(t, "pytest -q")
	w.shellTimeout, w.shellExit = true, -1
	ev, evID := w.mustObserve(t)
	behavioral := w.stage(evID)
	if len(behavioral) != 1 {
		t.Fatalf("staging produced %d records, want one for the command", len(behavioral))
	}
	if w.shellRuns != 1 {
		t.Errorf("the executor ran %d times, want once", w.shellRuns)
	}
	if behavioral[0].Outcome == ValidationPassed {
		t.Error("a timeout was recorded as a pass")
	}
	d := w.decide(evID, append([]proxyEvidence{ev}, behavioral...)...)
	expectReason(t, "timed out", d, false, ReasonEvidenceMissing)
}

func TestDeclaredCommandRefusedByTheSafetyGate(t *testing.T) {
	// Client authority to ask for verification is not authority to do what
	// the safety gate refuses the model.
	w := stagedWorld(t, "rm -rf /")
	ev, evID := w.mustObserve(t)
	d := w.decide(evID, append([]proxyEvidence{ev}, w.stage(evID)...)...)
	if d.Authorized {
		t.Error("a refused command authorized the candidate")
	}
	if w.shellRuns != 0 {
		t.Errorf("a refused command reached the executor %d times", w.shellRuns)
	}
}

func TestDeclaredCommandWithStagingUnavailable(t *testing.T) {
	w := stagedWorld(t, "pytest -q")
	ev, evID := w.mustObserve(t)
	// The executor is gone. Unavailable is neither failed nor passed:
	// nothing observed the command, so nothing speaks for it.
	w.ctx.SandboxURL = ""
	behavioral := w.stage(evID)
	for _, b := range behavioral {
		if b.Outcome == ValidationPassed {
			t.Error("an unavailable executor produced a pass")
		}
	}
	if w.shellRuns != 0 {
		t.Errorf("the executor ran %d times after being removed", w.shellRuns)
	}
	d := w.decide(evID, append([]proxyEvidence{ev}, behavioral...)...)
	expectReason(t, "staging unavailable", d, false, ReasonEvidenceMissing)
}

func TestTwoDeclaredCommandsWhereOnlyOnePasses(t *testing.T) {
	w := stagedWorld(t, "pytest -q", "ruff check .")
	// `pytest -q` passes and `ruff check .` does not. A passing command
	// speaks for itself and for nothing else, so the other stays owed.
	w.shellFail = map[string]bool{"ruff check .": true}
	ev, evID := w.mustObserve(t)
	behavioral := w.stage(evID)
	if len(behavioral) != 2 {
		t.Fatalf("staging produced %d records, want one per command", len(behavioral))
	}
	if w.shellRuns != 2 {
		t.Errorf("the executor ran %d times, want once per command", w.shellRuns)
	}
	seen := map[string]bool{}
	passed := 0
	for _, b := range behavioral {
		if seen[b.Provenance.ObligationID] {
			t.Error("two records claim the same obligation")
		}
		seen[b.Provenance.ObligationID] = true
		if b.Outcome == ValidationPassed {
			passed++
		}
	}
	if passed != 1 {
		t.Fatalf("%d records passed, want exactly the one command that did", passed)
	}
	d := w.decide(evID, append([]proxyEvidence{ev}, behavioral...)...)
	if d.Authorized {
		t.Error("a half-met obligation set authorized")
	}
	if len(d.Satisfied) != 2 {
		t.Fatalf("satisfied %v, want the syntax and one command met", d.Satisfied)
	}
	if len(d.Missing) != 1 {
		t.Fatalf("missing %v, want the other command owed", d.Missing)
	}
	if !strings.HasPrefix(d.Missing[0], ObligationDeclaredCommand+":") {
		t.Errorf("missing %v, want the command that failed", d.Missing)
	}
}

func TestTwoDeclaredCommandsBothPassing(t *testing.T) {
	w := stagedWorld(t, "pytest -q", "ruff check .")
	ev, evID := w.mustObserve(t)
	behavioral := w.stage(evID)
	if len(behavioral) != 2 {
		t.Fatalf("staging produced %d records, want one per command", len(behavioral))
	}
	if w.shellRuns != 2 {
		t.Errorf("the executor ran %d times, want once per command", w.shellRuns)
	}
	d := w.decide(evID, append([]proxyEvidence{ev}, behavioral...)...)
	expectReason(t, "both commands", d, true, ReasonAuthorized)
	if len(d.Satisfied) != 3 {
		t.Errorf("satisfied %v, want the syntax and both commands", d.Satisfied)
	}
}

func TestADeclaredSetLargerThanTheStagingBudget(t *testing.T) {
	var commands []string
	for i := 0; i < defaultStagingBudget().MaxCommands+1; i++ {
		commands = append(commands, "check-"+string(rune('a'+i)))
	}
	w := stagedWorld(t, commands...)
	ev, evID := w.mustObserve(t)
	behavioral := w.stage(evID)
	if len(behavioral) != 0 {
		t.Errorf("an over-budget set produced %d records", len(behavioral))
	}
	if w.shellRuns != 0 {
		t.Errorf("an over-budget set ran %d commands; a partial set is not "+
			"a smaller obligation", w.shellRuns)
	}
	d := w.decide(evID, append([]proxyEvidence{ev}, behavioral...)...)
	if d.Authorized {
		t.Error("an unstaged set authorized")
	}
}

func TestBehavioralEvidenceOverASupersededWorkspace(t *testing.T) {
	w := stagedWorld(t, "pytest -q")
	ev, evID := w.mustObserve(t)
	behavioral := w.stage(evID)
	if len(behavioral) != 1 {
		t.Fatalf("staging produced %d records", len(behavioral))
	}
	// The workspace moves after staging observed it.
	bumpWorkspace(w.ctx, w.path, contentSHA256("moved on\n"))
	d := w.decide(evID, append([]proxyEvidence{ev}, behavioral...)...)
	expectReason(t, "superseded workspace", d, false, ReasonWorkspaceStale)
}

func TestBehavioralEvidenceFromAnotherInvocation(t *testing.T) {
	w := stagedWorld(t, "pytest -q")
	ev, evID := w.mustObserve(t)
	behavioral := w.stage(evID)
	if len(behavioral) != 1 {
		t.Fatalf("staging produced %d records", len(behavioral))
	}
	// A second invocation of the same request has its own identity, and
	// the earlier record may not stand in for it.
	other := nextInvocationIdentity(w.ctx, mintRouteEntry(w.ctx), w.hash)
	d := w.decide(other, append([]proxyEvidence{ev}, behavioral...)...)
	expectReason(t, "another invocation", d, false, ReasonRequestOrInvocationMismatch)
}

func TestExistingSyntaxBaselineWithEqualEvidence(t *testing.T) {
	w := newAuthWorld(t, declaredPy, "solve.py", authPy, true)
	if w.ctx.Ledger == nil {
		w.ctx.Ledger = map[string]*DeliverableState{}
	}
	w.ctx.Ledger[w.path] = &DeliverableState{
		Path: w.path, CurrentHash: w.hash, Generation: 1,
		ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
		ValidatedHash: w.hash,
	}
	ev, evID := w.mustObserve(t)
	d := w.decide(evID, ev)
	// Current syntax evidence over the exact candidate bytes makes the same
	// claim a syntax baseline holds, so nothing is owed. The alternative
	// would refuse every replacement of a validated file forever.
	expectReason(t, "syntax baseline", d, true, ReasonAuthorized)
	for _, id := range d.Missing {
		if strings.HasPrefix(id, ObligationBaselinePreserved+":") {
			t.Errorf("missing %v: equal evidence did not preserve an equal baseline",
				d.Missing)
		}
	}
}

func TestExistingBehavioralBaselineWithSyntaxOnlyEvidence(t *testing.T) {
	w := newAuthWorld(t, declaredPy, "solve.py", authPy, true)
	w.ctx.VerificationEvidence = append(w.ctx.VerificationEvidence, VerificationRecord{
		Command: "python3 solve.py",
		Covered: map[string]string{w.path: w.hash}, Turn: 1,
	})
	if got := authorizationFloor(requestObligations(w.ctx)); got != "behavioral" {
		t.Fatalf("floor %q, want behavioral", got)
	}
	ev, evID := w.mustObserve(t)
	d := w.decide(evID, ev)
	// Syntax is a weaker claim than the baseline holds. Accepting it here
	// would replace a working artifact with one that merely parses.
	expectReason(t, "behavioral baseline", d, false, ReasonBaselineNotPreserved)
}

func TestExistingBehavioralBaselineReEstablishedByItsOwnCommand(t *testing.T) {
	// The baseline is behavioral, so re-establishing it takes a command the
	// client typed as behavioral over assets it owns. The same command
	// declared untyped is a runtime fact and cannot preserve it.
	w := newAuthWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["python3 solve.py"],`+
			`"verification_requirements_version":1,`+
			`"verification_requirements":[{"command":"python3 solve.py",`+
			`"kind":"behavioral","expects":"exit_zero",`+
			`"asset_authority":"client_supplied"}]}`,
		"solve.py", authPy, true)
	w.ctx.VerificationEvidence = append(w.ctx.VerificationEvidence, VerificationRecord{
		Command: "python3 solve.py",
		Covered: map[string]string{w.path: w.hash}, Turn: 1,
	})
	ev, evID := w.mustObserve(t)
	// The client declared the command that established the baseline, and
	// staging ran it against the candidate.
	d := w.decide(evID, append([]proxyEvidence{ev}, w.stage(evID)...)...)
	expectReason(t, "behavioral baseline re-run", d, true, ReasonAuthorized)
}

func TestExistingBehavioralBaselineWithADifferentCommandPassing(t *testing.T) {
	w := stagedWorld(t, "ruff check .")
	w.ctx.VerificationEvidence = append(w.ctx.VerificationEvidence, VerificationRecord{
		Command: "python3 solve.py",
		Covered: map[string]string{w.path: w.hash}, Turn: 1,
	})
	ev, evID := w.mustObserve(t)
	// A different command exiting zero is behavioural evidence about
	// something, but not evidence that what the baseline showed still holds.
	d := w.decide(evID, append([]proxyEvidence{ev}, w.stage(evID)...)...)
	expectReason(t, "behavioral baseline, other command", d, false,
		ReasonBaselineNotPreserved)
}

func TestANewFileOwesNoPreservation(t *testing.T) {
	w := newAuthWorld(t, declaredPy, "solve.py", authPy, true)
	// Nothing was ever validated at this path, so there is no baseline to
	// preserve and no obligation to fabricate.
	for _, o := range requestObligations(w.ctx) {
		if o.Kind == ObligationBaselinePreserved {
			t.Fatal("a path with no validated baseline owed preservation")
		}
	}
	ev, evID := w.mustObserve(t)
	expectReason(t, "new file", w.decide(evID, ev), true, ReasonAuthorized)
}

func TestPreservationNeverPromotesTheEvidenceItCompares(t *testing.T) {
	syntax, _ := newTaskObligation(ObligationBaselinePreserved, "/w/solve.py", "syntax", true)
	behavioral, _ := newTaskObligation(ObligationBaselinePreserved, "/w/solve.py", "behavioral", true)
	oracle, _ := newTaskObligation(ObligationBaselinePreserved, "/w/solve.py", "oracle", true)
	syntaxEv := proxyEvidence{Outcome: ValidationPassed,
		Provenance: V3EvidenceProvenance{ObservedStrength: "syntax"}}
	behavioralEv := proxyEvidence{Outcome: ValidationPassed,
		Provenance: V3EvidenceProvenance{ObservedStrength: "behavioral",
			CommandIdentity: contentSHA256("python3 solve.py")}}
	witness := contentSHA256("python3 solve.py")

	for _, tc := range []struct {
		name    string
		o       taskObligation
		ev      []proxyEvidence
		witness string
		want    bool
	}{
		{"syntax by syntax", syntax, []proxyEvidence{syntaxEv}, "", true},
		{"syntax by behavioral", syntax, []proxyEvidence{behavioralEv}, witness, true},
		{"behavioral by syntax", behavioral, []proxyEvidence{syntaxEv}, witness, false},
		{"behavioral by its own command", behavioral, []proxyEvidence{behavioralEv}, witness, true},
		{"behavioral by another command", behavioral, []proxyEvidence{behavioralEv},
			contentSHA256("ruff check ."), false},
		{"behavioral with no witness", behavioral, []proxyEvidence{behavioralEv}, "", false},
		{"oracle by behavioral", oracle, []proxyEvidence{behavioralEv}, witness, false},
		{"nothing by nothing", behavioral, nil, witness, false},
	} {
		if got, _ := baselinePreservedBy(tc.o, tc.witness, tc.ev); got != tc.want {
			t.Errorf("%s: preserved=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestEvidenceWithOneFieldChanged(t *testing.T) {
	w := newAuthWorld(t, declaredPy, "solve.py", authPy, true)
	ev, evID := w.mustObserve(t)
	for _, tc := range []struct {
		name   string
		change func(*proxyEvidence)
		want   AuthorizationReason
	}{
		// The model ran its own test and it passed, labelled as what it is.
		{"model-generated self-test", func(e *proxyEvidence) {
			e.Provenance.Source = ProvenanceModelGenerated
		}, ReasonProvenanceUntrusted},
		{"unknown source", func(e *proxyEvidence) {
			e.Provenance.Source = "something_new"
		}, ReasonProvenanceUntrusted},
		// Something mutated the workspace after the observation.
		{"stale workspace", func(e *proxyEvidence) {
			e.Provenance.WorkspaceGeneration += 3
		}, ReasonWorkspaceStale},
		{"other candidate bytes", func(e *proxyEvidence) {
			e.Provenance.CandidateHash = contentSHA256("print(8)\n")
		}, ReasonCandidateMismatch},
		{"another request", func(e *proxyEvidence) {
			e.Provenance.RequestID = "req-other"
		}, ReasonRequestOrInvocationMismatch},
		{"another invocation", func(e *proxyEvidence) {
			e.Provenance.InvocationID = "req-matrix:inv:99"
		}, ReasonRequestOrInvocationMismatch},
	} {
		changed := ev
		tc.change(&changed)
		expectReason(t, tc.name, w.decide(evID, changed), false, tc.want)
	}
	unknownStrength := ev
	unknownStrength.Provenance.RequiredStrength = "very_strong"
	if d := w.decide(evID, unknownStrength); d.Authorized {
		t.Errorf("an unknown strength authorized: %q", d.Reason)
	}
}

func TestLegacyOrUnknownEnvelope(t *testing.T) {
	w := newAuthWorld(t, declaredPy, "solve.py", authPy, true)
	ev, evID := w.mustObserve(t)
	for _, tc := range []struct {
		name     string
		envelope *V3EvidenceEnvelope
	}{
		{"legacy envelope", &V3EvidenceEnvelope{}},
		{"unknown wire version", &V3EvidenceEnvelope{WireVersion: "99.0.0"}},
	} {
		d := authorizeCandidateDeliveryDecision(w.ctx, w.path, w.code, evID,
			tc.envelope, []proxyEvidence{ev})
		expectReason(t, tc.name, d, false, ReasonLegacyRecord)
	}
}

func TestUndeclaredTarget(t *testing.T) {
	w := newAuthWorld(t, declaredPy, "solve.py", authPy, true)
	ev, evID := w.mustObserve(t)
	d := authorizeCandidateDeliveryDecision(w.ctx, resolveAgentPath(w.ctx, "other.py"),
		w.code, evID, nil, []proxyEvidence{ev})
	expectReason(t, "undeclared target", d, false, ReasonTargetNotDeclared)
}

func TestRepairOrRefinementCandidate(t *testing.T) {
	// A later candidate for the same target in the same request gets its
	// own identity, and the earlier candidate's evidence does not carry
	// over to it.
	w := newAuthWorld(t, declaredPy, "solve.py", authPy, true)
	first, _ := w.mustObserve(t)
	second, secondID := w.mustObserve(t)
	if first.Provenance.CandidateInstanceID == second.Provenance.CandidateInstanceID {
		t.Fatal("the repair candidate reused the first candidate's identity")
	}
	// The repair candidate carries the same obligation record as a
	// generation candidate, and authorizes on its own evidence.
	d := w.decide(secondID, second)
	expectReason(t, "repair candidate", d, true, ReasonAuthorized)
	// Given both records, only the current candidate's is consumed.
	both := authorizeCandidateDeliveryDecision(w.ctx, w.path, w.code, secondID, nil,
		[]proxyEvidence{first, second})
	if !both.Authorized {
		t.Errorf("the current candidate's own evidence stopped working: %q", both.Reason)
	}
	if len(both.EvidenceConsumed) != 1 {
		t.Errorf("consumed %v, want only the current candidate's record",
			both.EvidenceConsumed)
	}
}

func TestUnknownObligationKind(t *testing.T) {
	w := newAuthWorld(t, declaredPy, "solve.py", authPy, true)
	in := authorizationInput{
		Obligations: []taskObligation{
			{ID: "artifact_exists:x", Kind: ObligationArtifactExists,
				Subject: w.path, RequiredStrength: "syntax", Required: true},
			{ID: "forged:x", Kind: "forged", Subject: w.path,
				RequiredStrength: "syntax", Required: true},
		},
		TargetPath: w.path, CandidateHash: w.hash,
	}
	// A kind with no role is neither a prerequisite nor settlement, so
	// nothing can be shown about it: the task states no satisfiable
	// prerequisite at all.
	if d := decideAuthorization(w.ctx, in); d.Authorized {
		t.Errorf("an unclassified obligation authorized: %q", d.Reason)
	}
}

func TestUnsupportedObligation(t *testing.T) {
	w := newAuthWorld(t, declaredPy, "solve.py", authPy, true)
	unsupported, _ := newTaskObligation(ObligationUnsupported, "a thing", "", true)
	exists, _ := newTaskObligation(ObligationArtifactExists, w.path, "", true)
	d := decideAuthorization(w.ctx, authorizationInput{
		Obligations:             []taskObligation{exists, unsupported},
		TargetPath:              w.path,
		CandidateHash:           w.hash,
		OutputKnowledgeDeclared: true,
	})
	expectReason(t, "unsupported obligation", d, false, ReasonObligationUnknown)
}

// --- the decision is inert ---------------------------------------------------

func TestTheAuthorizationDecisionCarriesNoContent(t *testing.T) {
	const secret = "TOKEN = 'hunter2'\nprint(7)\n"
	w := newAuthWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", secret, true)
	ev, evID := w.mustObserve(t)
	blob, err := json.Marshal(w.decide(evID, ev))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{secret, "hunter2", "TOKEN", "print(7)"} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the decision carries %q", needle)
		}
	}
}

func TestEveryReasonIsInTheClosedVocabulary(t *testing.T) {
	required := []AuthorizationReason{
		ReasonTargetNotDeclared, ReasonObligationUnknown, ReasonAdapterUnsupported,
		ReasonEvidenceMissing, ReasonEvidenceTooWeak, ReasonProvenanceUntrusted,
		ReasonCandidateMismatch, ReasonRequestOrInvocationMismatch,
		ReasonWorkspaceStale, ReasonBaselineNotPreserved, ReasonCommandMismatch,
		ReasonLegacyRecord, ReasonPostDeliverySettlementPending,
	}
	for _, r := range required {
		if !authorizationReasons[r] {
			t.Errorf("%q is required by the contract and not in the set", r)
		}
	}
	if authorizationReasons["something_new"] {
		t.Error("an unclassified reason is accepted")
	}
}

func TestAContradictoryStateNeverAuthorizes(t *testing.T) {
	w := newAuthWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", authPy, true)
	// No obligations at all: nothing to authorize on.
	d := decideAuthorization(w.ctx, authorizationInput{})
	if d.Authorized {
		t.Error("an empty input authorized")
	}
	if d.Reason != ReasonLegacyRecord {
		t.Errorf("reason %q, want legacy_record", d.Reason)
	}
	// A nil context with obligations still cannot settle.
	exists, _ := newTaskObligation(ObligationArtifactExists, w.path, "", true)
	d = decideAuthorization(nil, authorizationInput{
		Obligations: []taskObligation{exists}, TargetPath: w.path, CandidateHash: w.hash})
	if d.Authorized {
		t.Errorf("a nil context authorized: %q", d.Reason)
	}
}

// TestTheDecisionReachesNoLiveWrite inspects the owner's calls, not its prose.
// A doc comment may name the live decision it sits beside; the code may not
// call it, nor anything that delivers, mutates, completes or generates.
func TestTheDecisionReachesNoLiveWrite(t *testing.T) {
	const file = "authorization_decision.go"
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{
		"proposedV3Candidate": true, "v3DeliveryAuthorized": true,
		"writeFileRecorded": true, "finalizeCompletion": true,
		"terminalCompletionAllowed": true, "callV3Generate": true,
		"callV3GenerateStreaming": true, "revokeV3": true,
		"WriteFile": true, "Remove": true, "RemoveAll": true,
		"StreamFn": true, "awaitPermission": true,
	}
	ast.Inspect(tree, func(n ast.Node) bool {
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
			t.Errorf("the authorization owner calls %s", name)
		}
		return true
	})
}

// The record's claim about its own weight must be derived from the wiring,
// not written in as a constant.
func TestTheDecisionDerivesItsOwnWeight(t *testing.T) {
	src, err := os.ReadFile("authorization_decision.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, hardcoded := range []string{
		`"influences_live_decision": false`, `"influences_live_decision": true`,
	} {
		if strings.Contains(body, hardcoded) {
			t.Errorf("the record hardcodes %s instead of deriving it", hardcoded)
		}
	}
	// Derived once, from the request, and read by both the decision and its
	// record, so the two cannot disagree. The derivation names the two facts
	// that give a decision weight: declared output knowledge, or a structured
	// mutation target under automatic_v3.
	if strings.Count(body, "in.influencesLiveDecision()") != 2 {
		t.Error("the decision and its record do not both derive their weight from the one derivation")
	}
	i := strings.Index(body, "func (in authorizationInput) influencesLiveDecision() bool {")
	if i < 0 {
		t.Fatal("the weight derivation is gone")
	}
	derivation := body[i:]
	derivation = derivation[:strings.Index(derivation, "\n}")]
	for _, must := range []string{"in.OutputKnowledgeDeclared", "in.StructuredTargetGrounded"} {
		if !strings.Contains(derivation, must) {
			t.Errorf("the weight derivation does not read %s", must)
		}
	}
}
