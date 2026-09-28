package main

// The typed, observe-only attribution of why an automatic candidate did not
// land: a closed vocabulary, one record per route entry, derived from the
// live owners' decisions and read by nothing.

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// --- the vocabulary -------------------------------------------------------------

var everyAutomaticRefusal = []automaticRefusal{
	automaticRefusalNone, automaticRefusalRouteNotEntered,
	automaticRefusalNoCandidate, automaticRefusalV3Unavailable, automaticRefusalV3TimedOut,
	automaticRefusalCancelled, automaticRefusalRouteGateRevoked, automaticRefusalHardVeto,
	automaticRefusalNoSelection, automaticRefusalSelectedHashMismatch,
	automaticRefusalIdentityIncomplete, automaticRefusalNoScope, automaticRefusalTargetNotGrounded,
	automaticRefusalTargetMismatch, automaticRefusalScopeExpansion, automaticRefusalStaleBaseline,
	automaticRefusalAuthorizationUnavailable, automaticRefusalGrantNotMinted,
	automaticRefusalCaptureOnlySuppressed, automaticRefusalDeliveryFailed,
	automaticRefusalUnattributed,
}

func TestAutomaticRefusalVocabularyIsClosed(t *testing.T) {
	if len(automaticRefusalVocabulary) != len(everyAutomaticRefusal) {
		t.Fatalf("vocabulary has %d entries, the test enumerates %d",
			len(automaticRefusalVocabulary), len(everyAutomaticRefusal))
	}
	for _, r := range everyAutomaticRefusal {
		if !automaticRefusalVocabulary[r] {
			t.Errorf("%q is not in the vocabulary", r)
		}
	}
	if automaticRefusalVocabulary["something_new"] {
		t.Error("an unregistered value is accepted")
	}
	// Every reason the request named is present.
	for _, want := range []automaticRefusal{
		"no_candidate_produced", "route_not_entered", "target_not_grounded", "target_mismatch",
		"scope_expansion", "hard_veto", "stale_baseline", "selected_hash_mismatch",
		"v3_unavailable", "v3_timed_out", "cancelled", "authorization_unavailable",
		"grant_not_minted", "delivery_failed",
	} {
		if !automaticRefusalVocabulary[want] {
			t.Errorf("required reason %q missing", want)
		}
	}
}

// --- the derivation, one case per value ---------------------------------------

func lifecycleWith(t *testing.T, build func(l *routeLifecycle)) *routeLifecycle {
	t.Helper()
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-attr")
	l := newRouteLifecycle(mintRouteEntry(ctx))
	build(l)
	return l
}

func TestDeriveAutomaticRefusalCoversEveryValue(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-attr")
	ended := func(d routingDisposition, reason AuthorizationReason) func(*routeLifecycle) {
		return func(l *routeLifecycle) {
			l.finish(ctx, d, "", reason)
		}
	}
	refused := func(d deliveryAuthorization, vetoes ...string) func(*routeLifecycle) {
		return func(l *routeLifecycle) {
			l.noteAuthorization(d, candidateEvidenceIdentity{InvocationID: "i", CandidateInstanceID: "c"},
				"h", vetoes)
			l.finish(ctx, routingAuthorizationRefused, "h", AuthorizationReason(d.Refusal))
		}
	}
	delivered := func(out deliveryOutcome) func(*routeLifecycle) {
		return func(l *routeLifecycle) {
			l.noteAuthorization(deliveryAuthorization{AutomaticEligible: true,
				Grant: &authorizationGrant{}}, candidateEvidenceIdentity{}, "h", nil)
			l.finish(ctx, routingCandidateAuthorized, "h", "")
			l.noteDelivery(out)
		}
	}
	cases := []struct {
		name    string
		build   func(*routeLifecycle)
		outcome automaticOutcome
		refusal automaticRefusal
	}{
		{"landed", delivered(deliveryOutcome{Delivered: true}), automaticOutcomeLanded, automaticRefusalNone},
		{"no candidate", ended(routingNoCandidate, ""), automaticOutcomeNotLanded, automaticRefusalNoCandidate},
		{"producer offered the same bytes", ended(routingBaselineRetained, ""),
			automaticOutcomeNotLanded, automaticRefusalNoCandidate},
		{"v3 unavailable", ended(routingProducerUnavailable, ""), automaticOutcomeNotLanded, automaticRefusalV3Unavailable},
		{"v3 timed out", ended(routingProducerTimedOut, ""), automaticOutcomeNotLanded, automaticRefusalV3TimedOut},
		{"cancelled by the route", ended(routingCancelled, ""), automaticOutcomeNotLanded, automaticRefusalCancelled},
		{"route gate revoked", ended(routingRevokedByGate, ""), automaticOutcomeNotLanded, automaticRefusalRouteGateRevoked},
		{"not closure eligible", ended(routingNotClosureEligible, ""), automaticOutcomeNotLanded, automaticRefusalRouteGateRevoked},
		{"no closure path", ended(routingSkippedInfeasible, "no_closure_path"),
			automaticOutcomeNotLanded, automaticRefusalAuthorizationUnavailable},
		{"hard veto", refused(deliveryAuthorization{AutomaticRefusal: automaticHardVeto}, VetoSyntaxOrStructural),
			automaticOutcomeNotLanded, automaticRefusalHardVeto},
		{"veto: path expansion", refused(deliveryAuthorization{AutomaticRefusal: automaticHardVeto},
			VetoUnauthorizedPathExpansion), automaticOutcomeNotLanded, automaticRefusalScopeExpansion},
		{"veto: outside scope", refused(deliveryAuthorization{AutomaticRefusal: automaticHardVeto},
			VetoOutsideMutationScope), automaticOutcomeNotLanded, automaticRefusalScopeExpansion},
		{"veto: stale identity", refused(deliveryAuthorization{AutomaticRefusal: automaticHardVeto},
			VetoStaleIdentity), automaticOutcomeNotLanded, automaticRefusalStaleBaseline},
		{"veto: cancelled only", refused(deliveryAuthorization{AutomaticRefusal: automaticHardVeto},
			VetoCancelledOrTimedOut), automaticOutcomeNotLanded, automaticRefusalCancelled},
		{"veto: cancelled plus another", refused(deliveryAuthorization{AutomaticRefusal: automaticHardVeto},
			VetoCancelledOrTimedOut, VetoIncompleteEvidence), automaticOutcomeNotLanded, automaticRefusalHardVeto},
		{"no selection", refused(deliveryAuthorization{AutomaticRefusal: automaticNoSelection}),
			automaticOutcomeNotLanded, automaticRefusalNoSelection},
		{"not the winner", refused(deliveryAuthorization{AutomaticRefusal: automaticNotTheWinner}),
			automaticOutcomeNotLanded, automaticRefusalSelectedHashMismatch},
		{"identity incomplete", refused(deliveryAuthorization{AutomaticRefusal: automaticIdentityIncomplete}),
			automaticOutcomeNotLanded, automaticRefusalIdentityIncomplete},
		{"no scope", refused(deliveryAuthorization{AutomaticRefusal: automaticNoScope}),
			automaticOutcomeNotLanded, automaticRefusalNoScope},
		{"target not grounded", refused(deliveryAuthorization{AutomaticRefusal: automaticTargetNotGrounded}),
			automaticOutcomeNotLanded, automaticRefusalTargetNotGrounded},
		{"mint: target mismatch", refused(deliveryAuthorization{AutomaticEligible: true, Refusal: structuredTargetMismatch}),
			automaticOutcomeNotLanded, automaticRefusalTargetMismatch},
		{"mint: another request's scope", refused(deliveryAuthorization{AutomaticEligible: true, Refusal: structuredTargetNotThisRequest}),
			automaticOutcomeNotLanded, automaticRefusalTargetMismatch},
		{"mint: outside scope", refused(deliveryAuthorization{AutomaticEligible: true,
			Refusal: "the candidate is outside its mutation scope (candidate_left_the_mutation_boundary)"}),
			automaticOutcomeNotLanded, automaticRefusalScopeExpansion},
		{"mint: workspace moved", refused(deliveryAuthorization{AutomaticEligible: true, Refusal: "workspace_moved"}),
			automaticOutcomeNotLanded, automaticRefusalStaleBaseline},
		{"mint: adapter unsupported", refused(deliveryAuthorization{AutomaticEligible: true,
			Refusal: "the adapter does not support this artifact"}),
			automaticOutcomeNotLanded, automaticRefusalAuthorizationUnavailable},
		{"mint: prerequisite owed", refused(deliveryAuthorization{AutomaticEligible: true,
			Refusal: "an authorization prerequisite is still owed"}),
			automaticOutcomeNotLanded, automaticRefusalAuthorizationUnavailable},
		{"mint: other", refused(deliveryAuthorization{AutomaticEligible: true,
			Refusal: "too many live authorizations for one request"}),
			automaticOutcomeNotLanded, automaticRefusalGrantNotMinted},
		{"capture-only", refused(deliveryAuthorization{AutomaticEligible: true, CaptureOnly: true}),
			automaticOutcomeNotLanded, automaticRefusalCaptureOnlySuppressed},
		{"delivery: bytes did not settle", delivered(deliveryOutcome{Reason: "post_write_validation_failed"}),
			automaticOutcomeNotLanded, automaticRefusalDeliveryFailed},
		{"delivery: write failed", delivered(deliveryOutcome{Reason: "write_failed"}),
			automaticOutcomeNotLanded, automaticRefusalDeliveryFailed},
		{"delivery: target mismatch", delivered(deliveryOutcome{Reason: "target_mismatch"}),
			automaticOutcomeNotLanded, automaticRefusalTargetMismatch},
		{"delivery: hash mismatch", delivered(deliveryOutcome{Reason: "candidate_hash_mismatch"}),
			automaticOutcomeNotLanded, automaticRefusalSelectedHashMismatch},
		{"delivery: target changed", delivered(deliveryOutcome{Reason: "the target changed since the authorization"}),
			automaticOutcomeNotLanded, automaticRefusalStaleBaseline},
		{"delivery: workspace moved", delivered(deliveryOutcome{Reason: "the workspace moved since the authorization"}),
			automaticOutcomeNotLanded, automaticRefusalStaleBaseline},
		{"eligible and minted but never delivered", func(l *routeLifecycle) {
			l.noteAuthorization(deliveryAuthorization{AutomaticEligible: true, Grant: &authorizationGrant{}},
				candidateEvidenceIdentity{}, "h", nil)
			l.finish(ctx, routingAuthorizationRefused, "h", "")
		}, automaticOutcomeNotLanded, automaticRefusalUnattributed},
		{"never ended", func(l *routeLifecycle) {},
			automaticOutcomeNotLanded, automaticRefusalUnattributed},
		{"unclassified ending", ended(routingUnclassified, ""), automaticOutcomeNotLanded, automaticRefusalUnattributed},
	}
	seen := map[automaticRefusal]bool{}
	for _, c := range cases {
		l := lifecycleWith(t, c.build)
		outcome, refusal := deriveAutomaticRefusal(l)
		if outcome != c.outcome || refusal != c.refusal {
			t.Errorf("%s: (%s, %q), want (%s, %q)", c.name, outcome, refusal, c.outcome, c.refusal)
		}
		seen[refusal] = true
	}
	if o, r := deriveAutomaticRefusal(nil); o != automaticOutcomeNotLanded || r != automaticRefusalUnattributed {
		t.Errorf("nil lifecycle derived (%s, %q)", o, r)
	}
	// Every emitted value is reachable from some live fact pattern; the one
	// exception is route_not_entered, which is analysis-derived by design.
	for _, r := range everyAutomaticRefusal {
		if r == automaticRefusalRouteNotEntered {
			continue
		}
		if !seen[r] {
			t.Errorf("%q is in the vocabulary but no fact pattern derives it", r)
		}
	}
}

// --- live records through the real owners ---------------------------------------

func attributionRecords(recs []map[string]interface{}) []map[string]interface{} {
	return recordsOfKind(recs, "automatic_delivery_attribution")
}

// A landed automatic candidate: one record, outcome landed, no refusal, and
// every join identity equal to the records the other owners wrote.
func TestALandedAutomaticCandidateHasNoRefusalAndJoins(t *testing.T) {
	w := newAutomaticWorld(t, tuiAutomaticContract, routeWinner, nil, true)
	recs := captureShadow(t, func() {
		res, err := w.write(t)
		if err != nil || res == nil || !res.Success {
			t.Fatalf("delivery failed: %v %+v", err, res)
		}
	})
	attr := attributionRecords(recs)
	if len(attr) != 1 {
		t.Fatalf("%d attribution records, want 1", len(attr))
	}
	a := attr[0]
	if a["outcome"] != string(automaticOutcomeLanded) || a["refusal"] != "" {
		t.Fatalf("landed candidate attributed as %v/%v", a["outcome"], a["refusal"])
	}
	if a["policy_mode"] != string(CandidatePolicyAutomaticV3) || a["policy_source"] != string(CandidatePolicySourceFixed) {
		t.Errorf("policy %v/%v", a["policy_mode"], a["policy_source"])
	}
	if a["request_id"] != "req-automatic" {
		t.Errorf("request id %v", a["request_id"])
	}
	policies := recordsOfKind(recs, "candidate_policy_decision")
	grants := recordsOfKind(recs, "authorization_grant_event")
	if len(policies) == 0 || len(grants) == 0 {
		t.Fatal("no policy or grant records to join against")
	}
	if a["route_entry_id"] != policies[0]["route_entry_id"] || a["route_entry_id"] != grants[0]["route_entry_id"] {
		t.Errorf("route entry does not join: %v vs %v vs %v", a["route_entry_id"],
			policies[0]["route_entry_id"], grants[0]["route_entry_id"])
	}
	if a["invocation_id"] != grants[0]["invocation_id"] || a["candidate_instance_id"] != grants[0]["candidate_instance_id"] {
		t.Errorf("candidate identity does not join the grant: %v/%v vs %v/%v", a["invocation_id"],
			a["candidate_instance_id"], grants[0]["invocation_id"], grants[0]["candidate_instance_id"])
	}
	if a["candidate_hash"] != contentSHA256(routeWinner) {
		t.Errorf("candidate hash %v", a["candidate_hash"])
	}
	if a["disposition"] != string(routingCandidateAuthorized) {
		t.Errorf("disposition %v", a["disposition"])
	}
	if a["influences_live_decision"] != false {
		t.Error("the record does not declare itself inert")
	}
}

// A client that still sends candidate_policy "strict" -- every older TUI
// does -- gets the one rule: the selected candidate lands, and the record
// names the fixed rule rather than the mode the client asked for.
func TestAnOlderClientsStrictModeIsIgnored(t *testing.T) {
	w := newAutomaticWorld(t, tuiStrictContract, routeWinner, nil, true)
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})
	if got := w.disk(t); got != routeWinner {
		t.Fatalf("disk holds %q, want the selected candidate", got)
	}
	attr := attributionRecords(recs)
	if len(attr) != 1 {
		t.Fatalf("%d attribution records, want 1", len(attr))
	}
	if attr[0]["outcome"] != string(automaticOutcomeLanded) {
		t.Fatalf("attributed as %v/%v", attr[0]["outcome"], attr[0]["refusal"])
	}
	if attr[0]["policy_mode"] != string(CandidatePolicyAutomaticV3) ||
		attr[0]["policy_source"] != string(CandidatePolicySourceFixed) {
		t.Errorf("policy %v/%v", attr[0]["policy_mode"], attr[0]["policy_source"])
	}
}

// The service names a selection that is not the bytes in hand: exactly one
// attributable reason, from the eligibility owner's own answer.
func TestASelectionMismatchIsAttributedFromTheLiveOwner(t *testing.T) {
	w := newAutomaticWorld(t, tuiAutomaticContract, routeWinner, nil, true)
	*w.selected = contentSHA256("def solve(v):\n    return 0\n")
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})
	if got := w.disk(t); got == routeWinner {
		t.Fatal("a candidate the service did not name landed")
	}
	attr := attributionRecords(recs)
	if len(attr) != 1 {
		t.Fatalf("%d attribution records, want 1", len(attr))
	}
	if attr[0]["outcome"] != string(automaticOutcomeNotLanded) ||
		attr[0]["refusal"] != string(automaticRefusalSelectedHashMismatch) {
		t.Fatalf("attributed as %v/%v", attr[0]["outcome"], attr[0]["refusal"])
	}
}

// A request cancelled before the write: the route's cancellation veto fires
// through the real owners and the attribution names it.
func TestACancelledAutomaticRequestIsAttributedAsCancelled(t *testing.T) {
	w := newAutomaticWorld(t, tuiAutomaticContract, routeWinner, nil, true)
	cancelled, cancel := context.WithCancel(w.ctx.Ctx)
	cancel()
	w.ctx.Ctx = cancelled
	recs := captureShadow(t, func() { w.write(t) })
	attr := attributionRecords(recs)
	if len(attr) != 1 {
		t.Fatalf("%d attribution records, want 1", len(attr))
	}
	if attr[0]["outcome"] != string(automaticOutcomeNotLanded) ||
		attr[0]["refusal"] != string(automaticRefusalCancelled) {
		t.Fatalf("attributed as %v/%v", attr[0]["outcome"], attr[0]["refusal"])
	}
}

// V3 unavailable on the edit route under automatic: one record per entry,
// v3_unavailable, and the caller's own bytes land.
func TestAnUnavailableProducerIsAttributedOnTheEditRoute(t *testing.T) {
	var r *editLoop
	recs := captureShadow(t, func() {
		r = editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiAutomaticContract,
			"Change helper in mod.py and check it.",
			script(stepRead("mod.py"),
				stepEdit("mod.py", "    return 1\n", "    return 2\n"),
				stepRun("python3 mod.py"),
				stepDone("edited and ran mod.py")),
			editLoopOptions{})
	})
	if !strings.Contains(r.disk(t, "mod.py"), "return 2") {
		t.Fatalf("the caller's edit did not land: %s", r.describe())
	}
	attr := attributionRecords(recs)
	if len(attr) != 1 {
		t.Fatalf("%d attribution records for one route entry", len(attr))
	}
	if attr[0]["refusal"] != string(automaticRefusalV3Unavailable) || attr[0]["outcome"] != string(automaticOutcomeNotLanded) {
		t.Fatalf("attributed as %v/%v", attr[0]["outcome"], attr[0]["refusal"])
	}
	if attr[0]["policy_mode"] != string(CandidatePolicyAutomaticV3) || attr[0]["policy_consulted"] != true {
		t.Errorf("an early ending lost the policy: %v", attr[0])
	}
	dispositions := recordsOfKind(recs, "shadow_route_disposition")
	if len(dispositions) != 1 || dispositions[0]["route_entry_id"] != attr[0]["route_entry_id"] {
		t.Errorf("the attribution does not join its disposition: %v vs %v", attr[0]["route_entry_id"], dispositions)
	}
}

// A delivered edit candidate through the real loop: landed, no refusal, and
// exactly one record for the entry.
func TestADeliveredEditCandidateIsAttributedAsLanded(t *testing.T) {
	c := editCases()[0]
	recs := captureShadow(t, func() {
		r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiAutomaticContract,
			"Change helper in mod.py and check it.",
			script(stepRead("mod.py"), c.step(t), stepRun("python3 mod.py"), stepDone("done")),
			editLoopOptions{v3Winner: c.winner})
		if r.disk(t, "mod.py") != c.winner {
			t.Fatalf("the winner did not land: %s", r.describe())
		}
	})
	attr := attributionRecords(recs)
	if len(attr) != 1 {
		t.Fatalf("%d attribution records, want 1", len(attr))
	}
	if attr[0]["outcome"] != string(automaticOutcomeLanded) || attr[0]["refusal"] != "" {
		t.Fatalf("attributed as %v/%v", attr[0]["outcome"], attr[0]["refusal"])
	}
	if attr[0]["candidate_hash"] != contentSHA256(c.winner) {
		t.Errorf("candidate hash %v", attr[0]["candidate_hash"])
	}
}

// --- content and keys --------------------------------------------------------------

var attributionKeys = []string{
	"build_version", "candidate_hash", "candidate_instance_id", "disposition",
	"influences_live_decision", "invocation_id", "outcome", "policy_consulted",
	"policy_mode", "policy_source", "record_kind", "refusal", "request_id",
	"route_entry_id", "schema_version",
}

func TestAttributionRecordsCarryClosedKeysAndNoContent(t *testing.T) {
	w := newRouteWorldWithClosure(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q --secret hunter2"],`+
			`"candidate_policy":"automatic_v3"}`,
		map[string]stubEffect{"pytest -q --secret hunter2": {}}, false)
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})
	attr := attributionRecords(recs)
	if len(attr) == 0 {
		t.Fatal("no attribution record")
	}
	allowed := map[string]bool{}
	for _, k := range attributionKeys {
		allowed[k] = true
	}
	for _, a := range attr {
		for k, v := range a {
			if !allowed[k] {
				t.Errorf("unexpected key %q", k)
			}
			switch v.(type) {
			case string, bool, float64:
			default:
				t.Errorf("key %q carries a %T, not a scalar", k, v)
			}
		}
		raw, _ := json.Marshal(a)
		blob := string(raw)
		for _, secret := range []string{routeWinner, routeBaseline, "hunter2", "pytest",
			w.path, w.dir, "Make solve fast.", "/tmp/"} {
			if strings.Contains(blob, secret) {
				t.Errorf("attribution carries %q", secret)
			}
		}
		if !automaticRefusalVocabulary[automaticRefusal(a["refusal"].(string))] {
			t.Errorf("refusal %v outside the vocabulary", a["refusal"])
		}
	}
}

// --- inertness -------------------------------------------------------------------

// Capture off and capture on: identical prompt bytes, tool sequence, results,
// disk and terminal. The observer changes nothing it observes.
func TestAttributionCaptureIsInert(t *testing.T) {
	c := editCases()[0]
	run := func() (*editLoop, string) {
		r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiAutomaticContract,
			"Change helper in mod.py and check it.",
			script(stepRead("mod.py"), c.step(t), stepRun("python3 mod.py"), stepDone("done")),
			editLoopOptions{v3Winner: c.winner})
		return r, r.disk(t, "mod.py")
	}
	off, offDisk := run()
	var on *editLoop
	var onDisk string
	recs := captureShadow(t, func() { on, onDisk = run() })
	if len(attributionRecords(recs)) != 1 {
		t.Fatalf("capture on wrote %d attribution records", len(attributionRecords(recs)))
	}
	if offDisk != onDisk {
		t.Error("disk differs between capture off and on")
	}
	if strings.Join(off.seq, ",") != strings.Join(on.seq, ",") {
		t.Errorf("tool sequence differs: %v vs %v", off.seq, on.seq)
	}
	if off.terminal["status"] != on.terminal["status"] || off.terminal["reason"] != on.terminal["reason"] {
		t.Errorf("terminal differs: %v vs %v", off.terminal, on.terminal)
	}
	if len(off.results) != len(on.results) {
		t.Fatalf("%d vs %d tool results", len(off.results), len(on.results))
	}
	for i := range off.results {
		if off.results[i]["success"] != on.results[i]["success"] || off.results[i]["tool"] != on.results[i]["tool"] {
			t.Errorf("result %d differs: %v vs %v", i, off.results[i], on.results[i])
		}
	}
	if a, b := conversationBytes(t, off.prompts), conversationBytes(t, on.prompts); a != b {
		t.Error("model-facing prompt bytes differ between capture off and on")
	}
}

// With the sink off the recorder builds nothing: the guard precedes every
// allocation, and the derivation is never called.
func TestAttributionBuildsNothingWithTheSinkOff(t *testing.T) {
	src, err := os.ReadFile("automatic_attribution.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	fn := body[strings.Index(body, "func (l *routeLifecycle) recordAttribution("):]
	fn = fn[:strings.Index(fn, "\n}")]
	guard := strings.Index(fn, "if !sink.enabled() {")
	derive := strings.Index(fn, "deriveAutomaticRefusal(")
	build := strings.Index(fn, "map[string]interface{}{")
	submit := strings.Index(fn, "sink.submit(")
	if guard < 0 || derive < 0 || build < 0 || submit < 0 || guard > derive || guard > build || build > submit {
		t.Error("recordAttribution derives or builds before checking the sink")
	}
	if !strings.Contains(fn, `"influences_live_decision": false`) {
		t.Error("the attribution does not declare itself inert")
	}
	// And the live path with the sink off still delivers.
	w := newAutomaticWorld(t, tuiAutomaticContract, routeWinner, nil, true)
	res, err := w.write(t)
	if err != nil || res == nil || !res.Success || w.disk(t) != routeWinner {
		t.Fatalf("the route stopped delivering with the sink off: %v %+v", err, res)
	}
}

// --- ownership guards -----------------------------------------------------------------

// attributionOwners are the only production files that may name the
// attribution: its owner, the lifecycle it hangs off, and the two routes that
// hand it facts. Policy, authorization, grant, delivery, settlement,
// obligation, verification, feasibility, completion, permission, Lens and tool
// execution owners never mention it, so no shadow identifier can reach a
// decision about what lands, what is owed, or what is verified.
var attributionOwners = map[string]bool{
	"automatic_attribution.go": true,
	"route_disposition.go":     true,
	"edit_route_delivery.go":   true,
	"tools.go":                 true,
}

// attributionIdentifiers are every top-level name the owner file declares,
// read from its AST so a rename cannot silently narrow the guard.
func attributionIdentifiers(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, "automatic_attribution.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{"automatic_delivery_attribution": true}
	for _, d := range tree.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			names[decl.Name.Name] = true
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					names[sp.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						names[n.Name] = true
					}
				}
			}
		}
	}
	if len(names) < 10 {
		t.Fatalf("only %d attribution names found; the owner file moved", len(names))
	}
	return names
}

// identifiersIn returns every identifier and selector name a Go file uses,
// comments excluded: a mention in prose is not a read.
func identifiersIn(t *testing.T, path string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	ast.Inspect(tree, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.Ident:
			used[e.Name] = true
		case *ast.BasicLit:
			used[strings.Trim(e.Value, "`\"")] = true
		}
		return true
	})
	return used
}

func TestNoDecisionOwnerReadsTheAttribution(t *testing.T) {
	names := attributionIdentifiers(t)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || attributionOwners[f] {
			continue
		}
		used := identifiersIn(t, f)
		for name := range names {
			if used[name] {
				t.Errorf("%s uses %s; the attribution is read by nothing", f, name)
			}
		}
	}
	// The two routes only hand facts in; nothing reads the facts back.
	for _, f := range []string{"edit_route_delivery.go", "tools.go", "route_disposition.go"} {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), ".auto.") || strings.Contains(string(body), "deriveAutomaticRefusal(") {
			t.Errorf("%s reads the attribution facts", f)
		}
	}
}

// The attribution API returns nothing to a caller: every method on the
// lifecycle that takes facts has no result, and the only functions that
// return a reason are the pure derivation helpers in the owner file.
func TestAttributionMethodsReturnNothingToTheRoutes(t *testing.T) {
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, "automatic_attribution.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range tree.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			continue
		}
		if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
			t.Errorf("%s returns a value to its caller", fn.Name.Name)
		}
	}
}

// No route reads a delivery mode: there is one rule, and nothing a client or
// an operator sends selects another.
func TestNoRouteReadsADeliveryMode(t *testing.T) {
	for _, f := range []string{"edit_route_delivery.go", "tools.go", "candidate_delivery.go",
		"advisory_policy.go", "automatic_delivery.go", "structured_mutation_target.go"} {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		code := codeWithoutComments(string(body))
		for _, banned := range []string{".CandidatePolicy", "ATLAS_CANDIDATE_POLICY"} {
			if strings.Contains(code, banned) {
				t.Errorf("%s reads %s", f, banned)
			}
		}
	}
}

// The attribution is not an obligation kind, a verification kind, an evidence
// kind or a completion reason: none of those owners knows the word.
func TestTheAttributionCannotBecomeAnObligationOrAVerificationResult(t *testing.T) {
	for _, f := range []string{"obligation_kinds.go", "verification_requirements.go",
		"verification_evidence.go", "staged_command_evidence.go", "evidence_wiring.go",
		"authorization_decision.go", "authorization_grant.go", "candidate_delivery.go",
		"delivery_settlement.go", "feasibility_decision.go", "candidate_policy.go",
		"advisory_policy.go", "agent.go", "gates.go", "guardrails.go"} {
		used := identifiersIn(t, f)
		for name := range attributionIdentifiers(t) {
			if used[name] {
				t.Errorf("%s knows %s", f, name)
			}
		}
	}
	keys := append([]string(nil), attributionKeys...)
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasPrefix(k, "obligation") || strings.HasPrefix(k, "verification") {
			t.Errorf("attribution key %q reads like an obligation or verification field", k)
		}
	}
}

// An edit V3 replaced reached the model as its own. Only write_file, whose
// whole-file content compares with disk, got the note that the bytes differ;
// the model then kept editing a file it had never seen. Every edit tool now
// carries its whole-file proposal, so the model is told for all four, and its
// read state follows what landed.
func TestTheModelIsToldWhenACandidateReplacedItsEdit(t *testing.T) {
	for _, c := range editCases() {
		t.Run(c.tool, func(t *testing.T) {
			r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiAutomaticContract,
				"Change helper in mod.py and check it.",
				script(stepRead("mod.py"), c.step(t), stepRun("python3 mod.py"), stepDone("done")),
				editLoopOptions{v3Winner: c.winner})
			if r.disk(t, "mod.py") != c.winner {
				t.Fatalf("the winner did not land: %s", r.describe())
			}
			told := false
			for _, p := range r.prompts {
				if strings.Contains(p, "NOT the ones you sent") {
					told = true
				}
			}
			if !told {
				t.Errorf("the model was never told the bytes on disk are not its edit")
			}
			if seen := r.ctx.FilesRead[filepath.Join(r.dir, "mod.py")]; seen != c.winner {
				t.Errorf("read state does not follow what landed")
			}
		})
	}
}
