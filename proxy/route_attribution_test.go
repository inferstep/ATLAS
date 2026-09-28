package main

import (
	"context"
	"testing"
)

// An honest refusal must still be attributable.
//
// target_not_declared is refused before any candidate-evidence identity is
// minted, so the decision carried an empty invocation and candidate instance.
// Measured live in v6: three refusals in two requests, none of which could be
// joined to the route entry that produced it, and a reader could not tell a
// refusal from a route entry whose decision went missing.

func attrCtx(t *testing.T, request string) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, request)
	return ctx
}

func TestARefusalCarriesItsRouteEntry(t *testing.T) {
	ctx := attrCtx(t, "req-attr")
	entry := mintRouteEntry(ctx)
	in := authorizationInput{
		TargetPath:    "solve.py",
		CandidateHash: contentSHA256("candidate\n"),
		RouteEntry:    entry,
		Identity:      V3EvidenceProvenance{RequestID: "req-attr"},
	}
	recs := captureShadow(t, func() {
		recordAuthorizationDecision(ctx, in, AuthorizationDecision{
			Authorized: false, Reason: ReasonTargetNotDeclared})
	})
	got := recordsOfKind(recs, "candidate_authorization_decision")
	if len(got) != 1 {
		t.Fatalf("%d decisions recorded", len(got))
	}
	r := got[0]
	if r["route_entry_id"] != entry.ID {
		t.Fatalf("route entry %v, want %q", r["route_entry_id"], entry.ID)
	}
	if r["authorized"] != false || r["reason"] != string(ReasonTargetNotDeclared) {
		t.Fatalf("a refusal stopped being a refusal: %v %v", r["authorized"], r["reason"])
	}
	// Candidate-evidence identity was never minted here and must not be invented.
	if r["invocation_id"] != "" || r["candidate_instance_id"] != "" {
		t.Fatalf("fabricated candidate identity: %v / %v",
			r["invocation_id"], r["candidate_instance_id"])
	}
	if r["candidate_hash"] != in.CandidateHash {
		t.Error("the refusal does not name the bytes it refused")
	}
}

func TestTwoRefusalsInOneRequestStayApart(t *testing.T) {
	ctx := attrCtx(t, "req-attr")
	first, second := mintRouteEntry(ctx), mintRouteEntry(ctx)
	recs := captureShadow(t, func() {
		for _, e := range []routeEntry{first, second} {
			recordAuthorizationDecision(ctx, authorizationInput{
				TargetPath: "solve.py", CandidateHash: contentSHA256("c-" + e.ID),
				RouteEntry: e, Identity: V3EvidenceProvenance{RequestID: "req-attr"},
			}, AuthorizationDecision{Authorized: false, Reason: ReasonTargetNotDeclared})
		}
	})
	got := recordsOfKind(recs, "candidate_authorization_decision")
	seen := map[string]bool{}
	for _, r := range got {
		seen[r["route_entry_id"].(string)] = true
	}
	if !seen[first.ID] || !seen[second.ID] || len(seen) != 2 {
		t.Fatalf("two refusals did not stay apart: %v", seen)
	}
}

func TestAnUnattributableRefusalIsStillWritten(t *testing.T) {
	// Fails closed rather than silently: the record exists and names no entry,
	// so a reader can refuse it instead of mistaking it for a missing decision.
	ctx := attrCtx(t, "req-attr")
	recs := captureShadow(t, func() {
		recordAuthorizationDecision(ctx, authorizationInput{
			TargetPath: "solve.py", CandidateHash: contentSHA256("x"),
			Identity: V3EvidenceProvenance{RequestID: "req-attr"},
		}, AuthorizationDecision{Authorized: false, Reason: ReasonTargetNotDeclared})
	})
	got := recordsOfKind(recs, "candidate_authorization_decision")
	if len(got) != 1 || got[0]["route_entry_id"] != "" {
		t.Fatalf("an entry-less refusal was not written honestly: %v", got)
	}
}

func TestGrantEventsCarryTheSameRouteEntry(t *testing.T) {
	ctx := attrCtx(t, "req-attr")
	entry := mintRouteEntry(ctx)
	g := &authorizationGrant{
		ID: "g1", RequestID: "req-attr", InvocationID: entry.ID,
		CandidateInstanceID: entry.ID + ":abc", CandidateHash: contentSHA256("c"),
		TargetPath: "/w/solve.py", RouteEntryID: entry.ID,
	}
	recs := captureShadow(t, func() { recordGrantEvent(ctx, g, "minted", "") })
	got := recordsOfKind(recs, "authorization_grant_event")
	if len(got) != 1 || got[0]["route_entry_id"] != entry.ID {
		t.Fatalf("grant event lost its route entry: %v", got)
	}
}
