package main

import (
	"go/ast"
	"os"
	"strings"
	"testing"
)

// The delivery graph, enumerated.
//
// Two producers now exist and each computes authority. Nothing consults it.
// This file is the proof: every production consumer of provenance is named
// here, and a new one fails the build's tests rather than quietly shipping.
//
// The rule is not "provenance is unused" -- it is computed, serialised and
// compared. The rule is that no result of it reaches a decision about what
// lands on disk.

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
		// pinned separately by the feasibility guard.
		"edit_route_delivery.go:deliverEditCandidate": true,
		// The decision's own refusal classifier, called from the decision.
		"authorization_decision.go:decideAuthorization": true,
		// The one site that computes the shadow decision beside the live one,
		// and the one that asks the feasibility question before generation.
		"tools.go:writeFileWithV3": true,
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
		"tools.go:proposedV3Candidate":               true,
		"tools.go:writeFileWithV3":                   true,
		"tools.go:improveContentWithV3":              true,
	}
	for _, fn := range deliveryDecisions {
		for site := range callSites(proxyFiles(t), fn) {
			if !allowed[site] {
				t.Errorf("%s gained a new caller of %s", site, fn)
			}
		}
	}
}

// TestNoProducerReachesTheDeliveryGraph pins the direction: a producer may
// never call a delivery decision, and a delivery decision may never call a
// producer.
func TestNoProducerReachesTheDeliveryGraph(t *testing.T) {
	files := proxyFiles(t)
	producers := map[string]bool{
		"produceSyntaxEvidence":               true,
		"produceDeclaredVerificationEvidence": true,
		"declaredVerificationCoverage":        true,
		"deriveTaskObligations":               true,
		"newTaskObligation":                   true,
	}
	for _, fn := range deliveryDecisions {
		for site := range callSites(files, fn) {
			caller := site[strings.Index(site, ":")+1:]
			if producers[caller] {
				t.Errorf("producer %s calls the delivery decision %s", caller, fn)
			}
		}
	}
	for fn := range producers {
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
	// registered V3 byte-delivery routes. A new caller is a new way for
	// service bytes to reach disk and has to be registered to exist.
	for _, fn := range []string{"authorizeCandidateDelivery", "deliverCandidateBytes"} {
		for site := range callSites(files, fn) {
			if !registeredV3DeliveryRoute(site) {
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
	src, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "if candidateProposed && !authorizedV3 {")
	if i < 0 {
		t.Fatal("the policy refusal is no longer on the production path")
	}
	// The refusal branch restores the caller's own baseline. It must name
	// baselineContent and must not reach for the candidate.
	end := strings.Index(body[i:], "\n\t}\n")
	if end < 0 {
		t.Fatal("could not bound the refusal branch")
	}
	branch := body[i : i+end]
	if !strings.Contains(branch, "revokeV3(") || !strings.Contains(branch, "baselineContent") {
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
