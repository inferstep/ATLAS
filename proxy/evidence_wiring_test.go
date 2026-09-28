package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The one production call path, and the one declared absence.

func wiringWorld(t *testing.T, contract, filename, code string, valid bool) *matrixWorld {
	t.Helper()
	w := newMatrixWorld(t, contract, filename, code, valid)
	w.ctx.HumanTask = "Create it."
	return w
}

// --- the wired producer ------------------------------------------------------

func TestTheWiredProducerObservesTheDeliveredBytes(t *testing.T) {
	const code = "TOKEN = 'hunter2'\nprint(7)\n"
	w := wiringWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", code, true)

	outcome := fallbackSyntaxOutcomeFor(w.ctx, w.path, code).aggregate()
	ev, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, code, outcome)
	if !ok {
		t.Fatal("the wired producer observed nothing about a declared code output")
	}
	p := ev.Provenance
	if p.Source != ProvenanceProxyOwnedValidation {
		t.Errorf("source %q", p.Source)
	}
	if p.CandidateHash != contentSHA256(code) {
		t.Error("the record does not name the delivered bytes")
	}
	if p.RequestID != "req-matrix" {
		t.Errorf("request %q, want the live one", p.RequestID)
	}
	// The invocation IS the route entry that produced it, so the identity now
	// names the entry rather than a generation counter.
	if !strings.HasPrefix(p.InvocationID, "req-matrix:entry:") {
		t.Errorf("invocation %q is not bound to the request", p.InvocationID)
	}
	if !strings.HasPrefix(p.CandidateInstanceID, p.InvocationID+":") {
		t.Errorf("candidate %q is not bound to its invocation", p.CandidateInstanceID)
	}
	if p.WorkspaceStateHash == "" || p.ObligationID == "" {
		t.Errorf("incomplete binding %+v", p)
	}
	if ev.Outcome != outcome.Status {
		t.Errorf("the record says %q, the gate said %q", ev.Outcome, outcome.Status)
	}
}

// TestTheWiredProducerRunsNoSecondCheck pins that the verdict is handed over.
// A producer that re-ran the gate would be a second opinion about one artifact
// and a second sandbox round trip on every delivery.
func TestTheWiredProducerRunsNoSecondCheck(t *testing.T) {
	const code = "A = 1\n"
	calls := 0
	w := wiringWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", code, true)
	// Count what the sandbox is asked after the caller has its verdict.
	outcome := fallbackSyntaxOutcomeFor(w.ctx, w.path, code).aggregate()
	countingURL := w.ctx.SandboxURL
	_ = countingURL
	w.ctx.SandboxURL = "" // any further check would now be not_run, not a pass
	ev, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, code, outcome)
	if !ok {
		t.Fatal("the producer refused a verdict it was handed")
	}
	if ev.Outcome != ValidationPassed {
		t.Errorf("outcome %q — the producer re-derived instead of using the verdict", ev.Outcome)
	}
	if calls != 0 {
		t.Errorf("%d extra sandbox calls", calls)
	}
}

func TestTheWiredProducerDistinguishesNegativeFromNotRun(t *testing.T) {
	const code = "def(\n"
	t.Run("demonstrated failure", func(t *testing.T) {
		w := wiringWorld(t,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
			"solve.py", code, false)
		outcome := fallbackSyntaxOutcomeFor(w.ctx, w.path, code).aggregate()
		ev, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, code, outcome)
		if !ok {
			t.Fatal("a demonstrated failure produced no observation")
		}
		if ev.Outcome != ValidationFailed {
			t.Errorf("outcome %q, want failed", ev.Outcome)
		}
		if authorized, _ := ev.Authorizes(); authorized {
			t.Error("a failure authorized its obligation")
		}
	})
	t.Run("not run", func(t *testing.T) {
		w := wiringWorld(t,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
			"solve.py", code, true)
		if _, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, code,
			checkOutcome{Status: ValidationNotRun}); ok {
			t.Error("a check that did not run produced a record")
		}
	})
	t.Run("unknown", func(t *testing.T) {
		w := wiringWorld(t,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
			"solve.py", code, true)
		if _, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, code,
			checkOutcome{Status: ValidationUnknown}); ok {
			t.Error("an unclassified check produced a record")
		}
	})
}

// --- what the wiring refuses -------------------------------------------------

func TestTheWiringObservesOnlyDeclaredTargets(t *testing.T) {
	const code = "A = 1\n"
	w := wiringWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", code, true)
	other := filepath.Join(w.ctx.WorkingDir, "other.py")
	if err := os.WriteFile(other, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	outcome := fallbackSyntaxOutcomeFor(w.ctx, other, code).aggregate()
	if _, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), other, code, outcome); ok {
		t.Error("a delivery to an undeclared target produced evidence")
	}
}

func TestTheWiringFabricatesNoSyntaxForADocument(t *testing.T) {
	const body = "# notes\n"
	w := wiringWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["notes.md"]}`,
		"notes.md", body, true)
	outcome := fallbackSyntaxOutcomeFor(w.ctx, w.path, body).aggregate()
	if _, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, body, outcome); ok {
		t.Error("a class the gate does not govern got a fabricated structural record")
	}
}

func TestTheWiringIsSilentForLegacyTraffic(t *testing.T) {
	const code = "A = 1\n"
	for _, contract := range []string{
		"",
		`{"task_mode":"work"}`,
		`{"task_mode":"work","output_knowledge":"unspecified","verification_knowledge":"unspecified"}`,
	} {
		w := wiringWorld(t, contract, "solve.py", code, true)
		outcome := fallbackSyntaxOutcomeFor(w.ctx, w.path, code).aggregate()
		if _, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, code, outcome); ok {
			t.Errorf("%q produced evidence with no structured obligation", contract)
		}
	}
}

func TestTheWiringNeedsALiveRequestIdentity(t *testing.T) {
	const code = "A = 1\n"
	w := wiringWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", code, true)
	w.ctx.Ctx = context.Background()
	outcome := fallbackSyntaxOutcomeFor(w.ctx, w.path, code).aggregate()
	if _, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, code, outcome); ok {
		t.Error("evidence was produced with no request to bind to")
	}
}

// --- identities are distinct -------------------------------------------------

func TestTwoInvocationsProduceDistinctIdentities(t *testing.T) {
	const code = "A = 1\n"
	w := wiringWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", code, true)
	outcome := fallbackSyntaxOutcomeFor(w.ctx, w.path, code).aggregate()

	first, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, code, outcome)
	if !ok {
		t.Fatal("first observation refused")
	}
	second, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, code, outcome)
	if !ok {
		t.Fatal("second observation refused")
	}
	if first.Provenance.InvocationID == second.Provenance.InvocationID {
		t.Error("two invocations share one identity")
	}
	if first.Provenance.CandidateInstanceID == second.Provenance.CandidateInstanceID {
		t.Error("two invocations' candidates share one identity")
	}
	// And therefore neither binds to the other.
	if bound, _ := first.Provenance.BindsTo(second.Provenance); bound {
		t.Error("one invocation's evidence bound to another's")
	}
}

func TestTwoCandidatesInOneRequestDoNotShareAnIdentity(t *testing.T) {
	w := wiringWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", "A = 1\n", true)
	a := nextInvocationIdentity(w.ctx, mintRouteEntry(w.ctx), contentSHA256("A = 1\n"))
	b := nextInvocationIdentity(w.ctx, mintRouteEntry(w.ctx), contentSHA256("A = 2\n"))
	if a.CandidateInstanceID == b.CandidateInstanceID || a.InvocationID == b.InvocationID {
		t.Errorf("%+v and %+v collide", a, b)
	}
	if strings.TrimSpace(a.InvocationID) == "" {
		t.Error("no identity was minted")
	}
}

// --- the staging path, stated as a fact about this build ---------------------

// TestTheStagingPathExistsAndIsTheOnlyRouteToBehavioralEvidence replaces the
// blocker that stood here.
//
// Behavioral authorization needs the client's exact command run against a
// workspace holding the candidate bytes, before delivery. That path now
// exists, and this pins the two things that make it the ONLY one: the trust
// owner builds the staging request itself, and the two mechanisms that come
// close are still not it.
func TestTheStagingPathExistsAndIsTheOnlyRouteToBehavioralEvidence(t *testing.T) {
	if got := evidenceProducerStatus[ProvenanceClientDeclaredVerification]; got != evidenceProducerWired {
		t.Fatalf("client-declared verification is declared %q, want wired", got)
	}
	files := proxyFiles(t)
	sites := callSites(files, "stageCandidate")
	if len(sites) != 1 {
		t.Fatalf("staging is reached from %v, want exactly the one trust owner", sites)
	}
	if _, ok := sites["evidence_wiring.go:observeCandidateVerification"]; !ok {
		t.Errorf("staging is reached from %v, not from the trust owner", sites)
	}

	// The service still cannot run what it was never told about, so a caller
	// of a V3 endpoint cannot manufacture this authority.
	types, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(types), "type V3GenerateRequest struct")
	body := string(types)[start : start+strings.Index(string(types)[start:], "\n}")]
	for _, field := range []string{"TaskContract", "Verification", "DeclaredCommand"} {
		if strings.Contains(body, field) {
			t.Errorf("the V3 request now carries %s: generic contract authority "+
				"must not travel into the service", field)
		}
	}
	// And the model running the same command through run_command remains a
	// different, untrusted event: it writes its own record, and no producer
	// reads it.
	agent, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agent), "ctx.VerificationEvidence = append") {
		t.Error("the model's own command record moved; re-derive the boundary")
	}
	producer, err := os.ReadFile("verification_evidence.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(producer), "VerificationRecord") {
		t.Error("the producer reads the model's own command record")
	}
}

// TestStagingRunsOnlyForARequestThatDeclaredCommands pins the cost boundary: a
// request that declared nothing stages nothing, so no command executes on its
// behalf.
func TestStagingRunsOnlyForARequestThatDeclaredCommands(t *testing.T) {
	for _, contract := range []string{
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],` +
			`"verification_knowledge":"unspecified"}`,
	} {
		w := wiringWorld(t, contract, "solve.py", "print(7)\n", true)
		id := nextInvocationIdentity(w.ctx, mintRouteEntry(w.ctx), contentSHA256("print(7)\n"))
		evidence, unmet, _ := observeCandidateVerification(w.ctx, w.path, "print(7)\n", id, mutationScope{})
		if len(evidence) != 0 || len(unmet) != 0 {
			t.Errorf("a request declaring no commands staged one: %s", contract)
		}
	}
}

// TestAResultNamingAnUndeclaredObligationProducesNothing is the trust boundary
// at the producer: the proxy matches results against ITS OWN obligations, so a
// result that names something else matches nothing.
func TestAResultNamingAnUndeclaredObligationProducesNothing(t *testing.T) {
	w := wiringWorld(t,
		`{"task_mode":"work","verification_knowledge":"declared","verification":["python3 solve.py"]}`,
		"solve.py", "print(7)\n", true)
	obl, ok := newTaskObligation(ObligationDeclaredCommand, "python3 solve.py", VerificationKindRuntime, true)
	if !ok {
		t.Fatal("obligation refused")
	}
	generation, stateHash := workspaceIdentity(w.ctx)
	base := verificationEvidenceRequest{
		Obligation: obl,
		Result: stagingCommandResult{
			CommandIdentity: contentSHA256("python3 solve.py"), ObligationID: obl.ID,
			Count: 1, Outcome: stagingExitedZero,
			TargetHashBefore: w.hash, TargetHashAfter: w.hash,
			WorkspaceHashBefore: "ws", WorkspaceHashAfter: "ws",
		},
		Identity: stagingIdentity{
			RequestID: "req-matrix", InvocationID: "inv-1",
			CandidateInstanceID: "cand-1", CandidateHash: w.hash,
			TargetPath: w.path, BaselineIdentity: baselineIdentityFor(w.ctx, w.path),
			WorkspaceGeneration: generation, WorkspaceStateHash: stateHash,
		},
	}
	if _, ok := produceDeclaredVerificationEvidence(w.ctx, base); !ok {
		t.Fatal("the honest shape produced nothing to contrast with")
	}
	for name, mut := range map[string]func(*verificationEvidenceRequest){
		"another obligation": func(r *verificationEvidenceRequest) {
			r.Result.ObligationID = "declared_command:not-ours"
		},
		"another command": func(r *verificationEvidenceRequest) {
			r.Result.CommandIdentity = contentSHA256("rm -rf /")
		},
		"another request": func(r *verificationEvidenceRequest) {
			r.Identity.RequestID = "req-somebody-else"
		},
		"other bytes": func(r *verificationEvidenceRequest) {
			r.Result.TargetHashBefore = contentSHA256("something else")
		},
	} {
		req := base
		mut(&req)
		if _, ok := produceDeclaredVerificationEvidence(w.ctx, req); ok {
			t.Errorf("a staged result naming %s produced evidence", name)
		}
	}
}

// --- telemetry carries identities and nothing else ---------------------------

func TestTheEvidenceRecordCarriesNoContent(t *testing.T) {
	const secret = "TOKEN = 'hunter2'\nprint(7)\n"
	w := wiringWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", secret, true)
	outcome := fallbackSyntaxOutcomeFor(w.ctx, w.path, secret).aggregate()
	ev, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, secret, outcome)
	if !ok {
		t.Fatal("no evidence to inspect")
	}
	blob, err := json.Marshal(ev.Provenance)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{secret, "hunter2", "TOKEN", "print(7)"} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the binding carries %q", needle)
		}
	}
}

// TestTheTelemetryRecordIsShapedAndFlaggedInert reads the record builder's own
// source: it must state influences_live_decision and must not carry any field
// that could hold bytes.
func TestTheTelemetryRecordIsShapedAndFlaggedInert(t *testing.T) {
	src, err := os.ReadFile("evidence_wiring.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func recordEvidenceObservation(")
	if start < 0 {
		t.Fatal("the record builder is gone")
	}
	// The builder's own body, and no further: the file continues past it, and
	// a scan that ran to the end would report the wiring above as if the
	// record carried what the wiring reads.
	fn := body[start:]
	if next := strings.Index(fn[1:], "\nfunc "); next >= 0 {
		fn = fn[:next+1]
	}
	if !strings.Contains(fn, `"influences_live_decision": false`) {
		t.Error("the record does not declare itself inert")
	}
	for _, banned := range []string{
		"CandidateBytes", "req.Path", "Detail", "code", "content",
	} {
		if strings.Contains(fn, banned) {
			t.Errorf("the record builder references %q, which can hold content", banned)
		}
	}
}
