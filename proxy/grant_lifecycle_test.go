package main

import (
	"context"
	"testing"
)

// Every grant ends once, and says why.
//
// A grant that ends without a record cannot be told apart from a dropped
// record: "minted, never seen again" leaves a lifecycle that cannot be
// reconciled from the capture.

func lifeCtx(t *testing.T, request string) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, request)
	if ctx.grants == nil {
		ctx.grants = map[string]*authorizationGrant{}
	}
	return ctx
}

func lifeGrant(ctx *AgentContext, entry routeEntry, target, hash string) *authorizationGrant {
	return &authorizationGrant{
		ID:                  grantKey("req-life", entry.ID, entry.ID+":c", target),
		RequestID:           "req-life",
		RouteEntryID:        entry.ID,
		InvocationID:        entry.ID,
		CandidateInstanceID: entry.ID + ":c",
		CandidateHash:       hash,
		TargetPath:          target,
	}
}

func TestASupersededGrantRecordsItsRetirement(t *testing.T) {
	ctx := lifeCtx(t, "req-life")
	first := mintRouteEntry(ctx)
	second := mintRouteEntry(ctx)
	a := lifeGrant(ctx, first, "/w/solve.py", contentSHA256("a"))
	b := lifeGrant(ctx, second, "/w/solve.py", contentSHA256("b"))

	recs := captureShadow(t, func() {
		ctx.grants[a.ID] = a
		recordGrantEvent(ctx, a, "minted", "")
		supersedeGrantsForTarget(ctx, b)
		ctx.grants[b.ID] = b
		recordGrantEvent(ctx, b, "minted", "")
	})
	events := recordsOfKind(recs, "authorization_grant_event")
	var retired []map[string]interface{}
	for _, r := range events {
		if r["event"] == "retired" {
			retired = append(retired, r)
		}
	}
	if len(retired) != 1 {
		t.Fatalf("%d retirement events for a superseded grant, want 1", len(retired))
	}
	if retired[0]["detail"] != string(grantSuperseded) {
		t.Fatalf("retirement reason %v, want %q", retired[0]["detail"], grantSuperseded)
	}
	if retired[0]["route_entry_id"] != first.ID {
		t.Fatalf("the retirement names entry %v, want the superseded one %q",
			retired[0]["route_entry_id"], first.ID)
	}
	if a.retired != grantSuperseded {
		t.Fatalf("state %q, want %q", a.retired, grantSuperseded)
	}
}

func TestEveryRetirementCauseIsRepresentable(t *testing.T) {
	// Each cause the code can reach has a distinct token, so a lifecycle can
	// be reconciled without inferring anything.
	for _, want := range []grantRetirement{
		grantSuperseded, grantTerminal, grantCancelled, grantConsumed,
		grantAttempted, grantSessionEnd,
	} {
		if !knownGrantRetirement(want) {
			t.Errorf("%q is reachable in the code and not in the closed set", want)
		}
	}
	if knownGrantRetirement(grantLive) {
		t.Error("live is not a retirement")
	}
	if knownGrantRetirement("something_new") {
		t.Error("an unregistered cause was accepted")
	}
}

func TestAGrantEndsExactlyOnce(t *testing.T) {
	ctx := lifeCtx(t, "req-life")
	entry := mintRouteEntry(ctx)
	g := lifeGrant(ctx, entry, "/w/solve.py", contentSHA256("a"))
	ctx.grants[g.ID] = g
	recs := captureShadow(t, func() {
		retireAuthorizationGrants(ctx, grantTerminal)
		// A second sweep must find nothing left to retire.
		retireAuthorizationGrants(ctx, grantSessionEnd)
	})
	n := 0
	for _, r := range recordsOfKind(recs, "authorization_grant_event") {
		if r["event"] == "retired" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d retirements for one grant, want exactly 1", n)
	}
}
