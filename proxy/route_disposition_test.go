package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// --- the vocabularies are closed ------------------------------------------

func TestRoutingDispositionsAreAClosedSet(t *testing.T) {
	for _, d := range []routingDisposition{
		routingSkippedInfeasible, routingProducerUnavailable, routingProducerTimedOut,
		routingCancelled, routingNoCandidate, routingNotClosureEligible,
		routingRevokedByGate, routingBaselineRetained, routingAuthorizationRefused,
		routingCandidateAuthorized, routingUnclassified,
	} {
		if !knownRoutingDisposition(d) {
			t.Errorf("%q is used and not registered", d)
		}
	}
	if knownRoutingDisposition("delivered") {
		t.Error("an unregistered disposition was accepted")
	}
}

func TestOnlyAnAuthorizedCandidateCountsAsRoutingSuccess(t *testing.T) {
	if !routingDispositionSucceeded(routingCandidateAuthorized) {
		t.Error("an authorized candidate is the success case")
	}
	for _, d := range []routingDisposition{
		routingUnclassified, routingBaselineRetained, routingNotClosureEligible,
		routingProducerUnavailable, routingCancelled, routingAuthorizationRefused,
		routingSkippedInfeasible, routingNoCandidate, routingRevokedByGate,
		routingProducerTimedOut,
	} {
		if routingDispositionSucceeded(d) {
			t.Errorf("%q was read as success", d)
		}
	}
}

func TestOnlyOneDeliveryDispositionMeansLanded(t *testing.T) {
	if !deliveryDispositionLanded(deliveryConsumedAndLanded) {
		t.Error("consumed_and_landed is the landing case")
	}
	for _, d := range []deliveryDisposition{
		deliveryNotAttemptedBaseline, deliveryNotAttemptedSuperseded,
		deliveryConsumedDidNotSettle, deliveryRefusedAtConsumption,
		deliveryRetiredAtTerminal, deliveryCancelled, deliveryUnclassified,
	} {
		if deliveryDispositionLanded(d) {
			t.Errorf("%q was read as a landing", d)
		}
	}
}

// --- exactly once ----------------------------------------------------------

func TestARouteEntryEndsExactlyOnce(t *testing.T) {
	ctx := lifeCtx(t, "req-disp")
	entry := mintRouteEntry(ctx)
	l := newRouteLifecycle(entry)
	recs := captureShadow(t, func() {
		l.finish(ctx, routingBaselineRetained, contentSHA256("a"), "")
		l.finish(ctx, routingCandidateAuthorized, contentSHA256("b"), "")
		l.finalizeDefault(ctx)
	})
	got := recordsOfKind(recs, "shadow_route_disposition")
	if len(got) != 1 {
		t.Fatalf("%d dispositions for one entry, want 1", len(got))
	}
	if got[0]["disposition"] != string(routingBaselineRetained) {
		t.Fatalf("the first ending did not win: %v", got[0]["disposition"])
	}
	if got[0]["route_entry_id"] != entry.ID {
		t.Fatal("the disposition does not name its entry")
	}
}

func TestAnUnclassifiedExitStillEnds(t *testing.T) {
	ctx := lifeCtx(t, "req-disp")
	l := newRouteLifecycle(mintRouteEntry(ctx))
	recs := captureShadow(t, func() { l.finalizeDefault(ctx) })
	got := recordsOfKind(recs, "shadow_route_disposition")
	if len(got) != 1 || got[0]["disposition"] != string(routingUnclassified) {
		t.Fatalf("a silent exit was not caught: %v", got)
	}
}

func TestAGrantDeliveryEndsExactlyOnce(t *testing.T) {
	ctx := lifeCtx(t, "req-disp")
	entry := mintRouteEntry(ctx)
	g := lifeGrant(ctx, entry, "/w/solve.py", contentSHA256("a"))
	recs := captureShadow(t, func() {
		recordDeliveryDisposition(ctx, g, deliveryNotAttemptedBaseline)
		recordDeliveryDisposition(ctx, g, deliveryConsumedAndLanded)
	})
	got := recordsOfKind(recs, "shadow_delivery_disposition")
	if len(got) != 1 || got[0]["disposition"] != string(deliveryNotAttemptedBaseline) {
		t.Fatalf("a grant ended more than once, or the wrong ending won: %v", got)
	}
	if got[0]["route_entry_id"] != entry.ID || got[0]["grant_id"] != g.ID {
		t.Fatal("the delivery ending is not bound to its grant and entry")
	}
}

func TestASupersededGrantEndsItsDeliveryLifecycle(t *testing.T) {
	ctx := lifeCtx(t, "req-disp")
	a := lifeGrant(ctx, mintRouteEntry(ctx), "/w/solve.py", contentSHA256("a"))
	b := lifeGrant(ctx, mintRouteEntry(ctx), "/w/solve.py", contentSHA256("b"))
	recs := captureShadow(t, func() {
		ctx.grants[a.ID] = a
		supersedeGrantsForTarget(ctx, b)
	})
	got := recordsOfKind(recs, "shadow_delivery_disposition")
	if len(got) != 1 || got[0]["disposition"] != string(deliveryNotAttemptedSuperseded) {
		t.Fatalf("supersession did not end the delivery lifecycle: %v", got)
	}
}

func TestBaselineRetentionIsNotADelivery(t *testing.T) {
	ctx := lifeCtx(t, "req-disp")
	g := lifeGrant(ctx, mintRouteEntry(ctx), "/w/solve.py", contentSHA256("a"))
	g.baselineRetained = true
	ctx.grants[g.ID] = g
	recs := captureShadow(t, func() { retireAuthorizationGrants(ctx, grantTerminal) })
	got := recordsOfKind(recs, "shadow_delivery_disposition")
	if len(got) != 1 || got[0]["disposition"] != string(deliveryNotAttemptedBaseline) {
		t.Fatalf("baseline retention was not named: %v", got)
	}
	if deliveryDispositionLanded(deliveryNotAttemptedBaseline) {
		t.Fatal("baseline retention read as a landing")
	}
}

// --- structural ------------------------------------------------------------

func dispositionSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("route_disposition.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGuardTheRouteHasADeferredBackstop(t *testing.T) {
	b, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "func writeFileWithV3(")
	if i < 0 {
		t.Fatal("the route is gone")
	}
	body := s[i:]
	if j := strings.Index(body[1:], "\nfunc "); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "defer lifecycle.finalizeDefault(ctx)") {
		t.Error("no deferred backstop: a new exit could end the route silently")
	}
	if !strings.Contains(body, "newRouteLifecycle(") {
		t.Error("the route no longer owns a lifecycle")
	}
}

func TestGuardRoutingAndDeliveryAreNotCollapsed(t *testing.T) {
	s := dispositionSource(t)
	if strings.Contains(s, `"target_path"`) {
		t.Error("a canonical path in a shadow record; identity is the join")
	}
	for _, want := range []string{"shadow_route_disposition", "shadow_delivery_disposition"} {
		if !strings.Contains(s, want) {
			t.Errorf("%s is gone; routing and delivery would be one answer", want)
		}
	}
	// No boolean success field in either record.
	for _, banned := range []string{`"success"`, `"delivered":`, `"ok"`} {
		if strings.Contains(s, banned) {
			t.Errorf("a boolean %s collapses the lifecycle", banned)
		}
	}
}

func TestGuardNoCandidateContentInDispositions(t *testing.T) {
	s := dispositionSource(t)
	for _, banned := range []string{
		`"code"`, `"content"`, `"stdout"`, `"stderr"`, `"command"`, `"prompt"`,
		`"source"`, `"detail_text"`,
	} {
		if strings.Contains(s, banned) {
			t.Errorf("%s would put content in telemetry", banned)
		}
	}
}

func TestGuardDispositionsUseIdentityNotPositionOrTime(t *testing.T) {
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, "route_disposition.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	ast.Inspect(tree, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.Ident:
			used[e.Name] = true
		case *ast.SelectorExpr:
			used[e.Sel.Name] = true
		}
		return true
	})
	for _, banned := range []string{"Now", "Since", "Unix", "enumerate", "index", "Sort"} {
		if used[banned] {
			t.Errorf("dispositions use %s; identity is the only join", banned)
		}
	}
}

func TestGuardDispositionsChangeNoLivePolicy(t *testing.T) {
	s := dispositionSource(t)
	if !strings.Contains(s, `"influences_live_decision": false`) {
		t.Error("a disposition does not declare itself inert")
	}
	for _, banned := range []string{"StreamFn(", "ctx.Messages", "MutationStatus", "ValidationStatus"} {
		if strings.Contains(s, banned) {
			t.Errorf("a disposition reaches %s", banned)
		}
	}
}
