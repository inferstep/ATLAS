package main

import (
	"context"
	"encoding/json"
	"go/ast"
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

// The record names the delivered bytes, the live request and its route entry,
// and carries none of the content.
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
	// The invocation is the route entry that produced it.
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
	blob, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{code, "hunter2", "TOKEN", "print(7)"} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the binding carries %q", needle)
		}
	}
}

// The producer records the verdict it is handed. A producer that ran the
// gate again would be a second opinion about one file and a second sandbox
// round trip on every delivery.
func TestTheWiredProducerRunsNoSecondCheck(t *testing.T) {
	const code = "A = 1\n"
	w := wiringWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", code, true)
	outcome := fallbackSyntaxOutcomeFor(w.ctx, w.path, code).aggregate()
	w.ctx.SandboxURL = "" // a check run now could only be not_run, never a pass
	ev, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, code, outcome)
	if !ok {
		t.Fatal("the producer refused a verdict it was handed")
	}
	if ev.Outcome != ValidationPassed {
		t.Errorf("outcome %q: the producer checked again instead of using the verdict", ev.Outcome)
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

// The wiring records nothing unless a live request declared this code file as
// an output.
func TestTheWiringObservesOnlyADeclaredCodeOutputOfALiveRequest(t *testing.T) {
	const declared = `{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`
	for _, tc := range []struct {
		name     string
		contract string
		filename string
		body     string
		// observed is the file the wiring is asked about, when it is not the
		// delivered one.
		observed  string
		noRequest bool
	}{
		{name: "an undeclared target", contract: declared,
			filename: "solve.py", body: "A = 1\n", observed: "other.py"},
		{name: "a document the syntax gate does not govern",
			contract: `{"task_mode":"work","output_knowledge":"declared","expected_outputs":["notes.md"]}`,
			filename: "notes.md", body: "# notes\n"},
		{name: "no contract", contract: "",
			filename: "solve.py", body: "A = 1\n"},
		{name: "a contract with only a task mode", contract: `{"task_mode":"work"}`,
			filename: "solve.py", body: "A = 1\n"},
		{name: "a contract with unspecified knowledge",
			contract: `{"task_mode":"work","output_knowledge":"unspecified","verification_knowledge":"unspecified"}`,
			filename: "solve.py", body: "A = 1\n"},
		{name: "no live request to bind to", contract: declared,
			filename: "solve.py", body: "A = 1\n", noRequest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := wiringWorld(t, tc.contract, tc.filename, tc.body, true)
			path := w.path
			if tc.observed != "" {
				path = filepath.Join(w.ctx.WorkingDir, tc.observed)
				if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.noRequest {
				w.ctx.Ctx = context.Background()
			}
			outcome := fallbackSyntaxOutcomeFor(w.ctx, path, tc.body).aggregate()
			if _, _, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), path, tc.body, outcome); ok {
				t.Error("the wiring produced a record")
			}
		})
	}
}

// --- identities are distinct -------------------------------------------------

// Each route entry mints its own identities, even for the same bytes in the
// same request, so one invocation's evidence never binds to another's.
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
	if bound, _ := first.Provenance.BindsTo(second.Provenance); bound {
		t.Error("one invocation's evidence bound to another's")
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
	gates, err := os.ReadFile("gates_exit.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gates), "ctx.VerificationEvidence = append") {
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

// The delivery graph, enumerated.
//
// Two producers compute provenance and the authority it carries. Every
// production consumer of provenance is named here, and a new one fails these
// tests rather than shipping quietly. One owner turns provenance into a
// decision about what lands on disk, and the guards below pin that it stays
// the only one.

// deliveryDecisions are the functions that decide whether a generated
// candidate may replace the caller's content. Adding a caller to any of them
// changes what lands, so the set of callers is pinned.
var deliveryDecisions = []string{
	"EvidenceSupportsProvenanceFor",
	"proposedV3Candidate",
	"v3DeliveryAuthorized",
}

// provenanceReaders are the ways a piece of provenance can be turned into a
// yes or a no. A production caller of one of these is a consumer of trust.
var provenanceReaders = []string{
	"MayAuthorize", "BindsTo", "Authorizes",
	"produceSyntaxEvidence", "produceDeclaredVerificationEvidence",
	"declaredVerificationCoverage", "observeCandidateVerification",
	"decideAuthorization", "authorizeCandidateDelivery",
	"deliverAuthorizedCandidate", "consumeAuthorizationGrant",
	"evidenceRefusalFor",
}

// TestEveryProductionConsumerOfProvenanceIsEnumerated fails when a call to a
// provenance reader appears anywhere in production code.
//
// A producer may build a binding, and the wiring may call a producer. What
// none of them may do is turn the answer into a delivery, which the guards
// below pin separately.
func TestEveryProductionConsumerOfProvenanceIsEnumerated(t *testing.T) {
	allowed := map[string]bool{
		// The coverage helper reports which declared commands are still owed.
		// Its answer is returned to its caller and, in this build, has none.
		"verification_evidence.go:declaredVerificationCoverage": true,
		// THE production call paths for the two producers, and the only places
		// a record reaches private telemetry.
		"evidence_wiring.go:observeDeliveredCandidateSyntax": true,
		"evidence_wiring.go:observeCandidateVerification":    true,
		// The grant check re-checks declared-command coverage rather than
		// taking the decision's word for it. It reads the answer and builds a
		// grant or refuses; it reaches no delivery, which the guards below pin.
		"authorization_grant.go:grantFor": true,
		// THE live authorization owner, and THE consumer of the grant it
		// mints. Exactly one of each, pinned by name below.
		"candidate_delivery.go:authorizeCandidateDelivery": true,
		"candidate_delivery.go:deliverAuthorizedCandidate": true,
		// THE shared delivery owner: it spends the grant a route minted and
		// is the only implementation of the exact-byte write, the post-write
		// check, settlement and restoration.
		"candidate_delivery.go:deliverCandidateBytes": true,
		// The protected edit route. It asks the same owners the write route
		// asks, and observes feasibility without consulting the answer --
		// pinned separately by the feasibility guard. Its evidence and its
		// decision are two of its named steps (v3DeliveryRouteSteps).
		"edit_route_delivery.go:deliverEditCandidate":         true,
		"edit_route_delivery.go:collectEditCandidateEvidence": true,
		"edit_route_delivery.go:decideEditCandidateDelivery":  true,
		// The decision's own refusal classifier, called from the decision.
		"authorization_decision.go:decideAuthorization": true,
		// The one site that computes the shadow decision beside the live one,
		// and the one that asks the feasibility question before generation.
		"tool_write_v3.go:writeFileWithV3":               true,
		"tool_write_v3.go:collectWriteCandidateEvidence": true,
		"tool_write_v3.go:decideWriteCandidateDelivery":  true,
		// The feasibility owner reads only its own closed input set.
		"feasibility_decision.go:observeInvocationFeasibility": true,
	}
	for _, fn := range append(append([]string{}, provenanceReaders...),
		"decideInvocationFeasibility", "observeInvocationFeasibility") {
		for site := range callSites(proxyFiles(t), fn) {
			if allowed[site] {
				continue
			}
			t.Errorf("%s calls %s: every production consumer of provenance must "+
				"be enumerated here, and this one is not", site, fn)
		}
	}
}

// TestTheDeliveryGraphGainedNoCaller pins the callers of the three functions
// that decide what lands. The set is exactly what it was at dc9172c.
func TestTheDeliveryGraphGainedNoCaller(t *testing.T) {
	allowed := map[string]bool{
		"v3_bridge.go:v3DeliveryAuthorized":          true,
		"v3_bridge.go:EvidenceSupportsProvenanceFor": true,
		"tool_write_v3.go:proposedV3Candidate":       true,
		"tool_write_v3.go:proposeWriteCandidate":     true,
		"tool_write_v3.go:emitV3WriteStageEnd":       true,
		"tool_write_v3.go:improveContentWithV3":      true,
	}
	for _, fn := range deliveryDecisions {
		for site := range callSites(proxyFiles(t), fn) {
			if !allowed[site] {
				t.Errorf("%s gained a new caller of %s", site, fn)
			}
		}
	}
}

// evidenceProducers are the functions that build evidence and obligations.
// The two guards below pin the direction between them and the delivery
// decisions.
var evidenceProducers = map[string]bool{
	"produceSyntaxEvidence":               true,
	"produceDeclaredVerificationEvidence": true,
	"declaredVerificationCoverage":        true,
	"deriveTaskObligations":               true,
	"newTaskObligation":                   true,
}

// TestNoProducerReachesTheDeliveryGraph pins one direction: a producer may
// never call a delivery decision.
func TestNoProducerReachesTheDeliveryGraph(t *testing.T) {
	files := proxyFiles(t)
	for _, fn := range deliveryDecisions {
		for site := range callSites(files, fn) {
			caller := site[strings.Index(site, ":")+1:]
			if evidenceProducers[caller] {
				t.Errorf("producer %s calls the delivery decision %s", caller, fn)
			}
		}
	}
}

// TestNoDeliveryDecisionReachesAProducer pins the other: a delivery decision
// may never call a producer.
func TestNoDeliveryDecisionReachesAProducer(t *testing.T) {
	files := proxyFiles(t)
	for fn := range evidenceProducers {
		for site := range callSites(files, fn) {
			for _, d := range deliveryDecisions {
				if strings.HasSuffix(site, ":"+d) {
					t.Errorf("delivery decision %s calls the producer %s", d, fn)
				}
			}
		}
	}
}

// TestTheWritePathReadsNoObligationOrEvidence pins that the functions which
// actually DECIDE what lands consult none of this slice's vocabulary.
//
// writeFileWithV3 is off this list on purpose: it is where the observation and
// the shadow decision are computed, beside the live decision and after it.
// What it may not do is let either answer reach the delivery, which is what
// TestExactlyOneLiveAuthorizationOwnerAnswersForACandidate replaces the
// inertness statement that stood here.
//
// The typed decision used to be computed beside the live one and discarded.
// It now decides, so what has to hold is different and stricter: one function
// reaches the decision, one function spends the grant it mints, and each is
// called from exactly one place on the write path. Two live authorization
// paths for one candidate is the state this pins against.
func TestExactlyOneLiveAuthorizationOwnerAnswersForACandidate(t *testing.T) {
	files := proxyFiles(t)
	// One owner each, whatever the route. There are now two protected routes
	// -- the new file and the edit -- and they share every owner below rather
	// than each keeping a copy: two grant implementations or two settlement
	// writers is the state this pins against.
	for _, c := range []struct{ fn, site string }{
		{"consumeAuthorizationGrant", "candidate_delivery.go:deliverCandidateBytes"},
		{"mintAuthorizationGrant", "candidate_delivery.go:authorizeCandidateDelivery"},
		{"decideAuthorization", "candidate_delivery.go:authorizeCandidateDelivery"},
		{"recordDeliverySettlement", "candidate_delivery.go:deliverCandidateBytes"},
	} {
		sites := callSites(files, c.fn)
		if len(sites) != 1 {
			t.Errorf("%s is called from %d places %v, want exactly one", c.fn, len(sites), sites)
			continue
		}
		if _, ok := sites[c.site]; !ok {
			t.Errorf("%s is called from %v, want %s", c.fn, sites, c.site)
		}
	}
	// The routes that may ask for authorization at all are exactly the
	// registered V3 byte-delivery routes, or their named steps. A new caller
	// is a new way for service bytes to reach disk and has to be registered
	// to exist.
	for _, fn := range []string{"authorizeCandidateDelivery", "deliverCandidateBytes"} {
		for site := range callSites(files, fn) {
			if !registeredV3DeliveryRoute(routeOf(site)) {
				t.Errorf("%s is called from unregistered route %s", fn, site)
			}
		}
	}
	// And the superseded observe-only owner is gone rather than left beside
	// the live one, where the two could drift.
	if sites := callSites(files, "observeCandidateAuthorization"); len(sites) != 0 {
		t.Errorf("the superseded observe-only owner is still called from %v", sites)
	}
	src, err := os.ReadFile("authorization_decision.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "func observeCandidateAuthorization(") {
		t.Error("the superseded observe-only owner is still defined")
	}
}

// TestATypedRefusalNeverFallsBackToACandidate pins the routing rule that makes
// the typed answer binding: when it refuses, the caller's own content is what
// is kept. Falling through to the envelope's winner would mean the typed path
// could be overruled by the thing it exists to check.
func TestATypedRefusalNeverFallsBackToACandidate(t *testing.T) {
	src, err := os.ReadFile("tool_write_v3.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func withdrawRefusedCandidate(")
	if i < 0 {
		t.Fatal("the policy refusal is no longer on the production path")
	}
	// The refusal restores the caller's own baseline, through the one
	// transition that does, and does not reach for the candidate.
	end := strings.Index(body[i:], "\n}\n")
	if end < 0 {
		t.Fatal("could not bound the refusal")
	}
	branch := body[i : i+end]
	if !strings.Contains(branch, "write.restoreBaseline(") {
		t.Error("a typed refusal does not restore the caller's own content")
	}
	if strings.Contains(branch, "v3Result.Code") || strings.Contains(branch, "proposedV3Candidate") {
		t.Error("a typed refusal reaches for a candidate")
	}
	// And the grant is only ever spent on the typed route.
	j := strings.Index(body, "deliverAuthorizedCandidate(")
	if j < 0 {
		t.Fatal("the grant consumer is not on the production path")
	}
	before := body[:j]
	if !strings.Contains(before[strings.LastIndex(before, "\n\tif "):], "delivery.Typed") {
		t.Error("the grant consumer is not guarded by the typed route")
	}
}

// TestEachProducerHasExactlyOneEnumeratedCallPath is the wiring invariant.
//
// A producer with two callers has two places its rules can drift apart. A
// producer with none is either a wiring bug or a mechanism that does not
// exist, and those are not the same thing -- so the second case must be
// declared unavailable rather than silently absent.
func TestEachProducerHasExactlyOneEnumeratedCallPath(t *testing.T) {
	files := proxyFiles(t)
	for _, c := range []struct {
		fn, source, site string
	}{
		{"produceSyntaxEvidence", ProvenanceProxyOwnedValidation,
			"evidence_wiring.go:observeDeliveredCandidateSyntax"},
		{"produceDeclaredVerificationEvidence", ProvenanceClientDeclaredVerification,
			"evidence_wiring.go:observeCandidateVerification"},
	} {
		sites := callSites(files, c.fn)
		switch evidenceProducerStatus[c.source] {
		case evidenceProducerWired:
			if len(sites) != 1 {
				t.Errorf("%s is declared wired but has %d call paths %v, want exactly one",
					c.fn, len(sites), sites)
				continue
			}
			if _, ok := sites[c.site]; !ok {
				t.Errorf("%s is called from %v, want %s", c.fn, sites, c.site)
			}
		case evidenceProducerUnavailable:
			if len(sites) != 0 {
				t.Errorf("%s is declared unavailable but is called from %v", c.fn, sites)
			}
		default:
			t.Errorf("%s has no availability declaration", c.source)
		}
	}
}

// TestBothProducersAreWiredAndNeitherGainedASecondPath is what stood here as a
// declared blocker. Both producers now have a live path; what has to stay true
// is that each has exactly one, and that no third source of provenance
// appeared alongside them.
func TestBothProducersAreWiredAndNeitherGainedASecondPath(t *testing.T) {
	for _, source := range []string{
		ProvenanceProxyOwnedValidation, ProvenanceClientDeclaredVerification,
	} {
		if got := evidenceProducerStatus[source]; got != evidenceProducerWired {
			t.Errorf("%s is declared %q, want wired", source, got)
		}
	}
	if len(evidenceProducerStatus) != 2 {
		t.Errorf("%d declared producers; a new source needs its own wiring "+
			"invariant here", len(evidenceProducerStatus))
	}
	// Staging executes, so its own reachability is pinned too: one caller, and
	// that caller is the trust owner.
	sites := callSites(proxyFiles(t), "stageCandidate")
	if len(sites) != 1 {
		t.Fatalf("staging is reached from %v, want exactly one path", sites)
	}
	if _, ok := sites["evidence_wiring.go:observeCandidateVerification"]; !ok {
		t.Errorf("staging is reached from %v, not from the trust owner", sites)
	}
}

// TestTerminalCompletionReadsNoEvidence pins that the terminal decision is
// untouched: it consults the obligation DECISION that landed at dc9172c and
// none of this slice's evidence vocabulary.
func TestTerminalCompletionReadsNoEvidence(t *testing.T) {
	files := proxyFiles(t)
	terminal := map[string]bool{
		"finalizeCompletion":        true,
		"terminalCompletionAllowed": true,
		"decideVerificationDemand":  true,
		"missingExpectedOutputs":    true,
	}
	banned := append(append([]string{}, provenanceReaders...),
		"deriveTaskObligations", "obligationID",
		"baselineIdentityFor", "workspaceIdentity")
	for _, fn := range banned {
		for site := range callSites(files, fn) {
			caller := site[strings.Index(site, ":")+1:]
			if terminal[caller] {
				t.Errorf("the terminal decision %s calls %s", caller, fn)
			}
		}
	}
}

// TestNoProductionCodeConstructsAProvenanceOutsideTheProducers pins that a
// binding can only be built where the rules for building one live.
func TestNoProductionCodeConstructsAProvenanceOutsideTheProducers(t *testing.T) {
	allowed := map[string]bool{
		"syntax_evidence.go":       true,
		"verification_evidence.go": true,
		// The type and its methods live here.
		"types.go": true,
		// The wire decoder materialises what the service sent.
		"v3_bridge.go": true,
		// The live owner builds the identity evidence must MATCH, which is the
		// opposite of producing evidence. It may not give that identity a
		// source -- one with a source would read as a record.
		"candidate_delivery.go": true,
	}
	for name, f := range proxyFiles(t) {
		if allowed[name] {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "V3EvidenceProvenance" {
				t.Errorf("%s constructs a V3EvidenceProvenance outside the producers", name)
			}
			return true
		})
	}
}
