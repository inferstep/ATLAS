package main

import (
	"os"
	"path/filepath"
	"testing"
)

// V3-authored bytes on an existing file go through the same chain as a new
// file, or they do not land.
//
// Four tools could land a service-authored replacement with no route identity,
// no candidate evidence, no authorization, no one-time grant, no exact-byte
// consumption and no settlement: the only gate was the service's own envelope,
// which is a proposal, not proxy authority.

const editOriginal = "def solve(values):\n    total = 0\n    for v in values:\n" +
	"        total += v\n    return total\n"

func editWorld(t *testing.T, contract string, commands map[string]stubEffect) *routeWorld {
	t.Helper()
	w := newRouteWorld(t, contract, commands)
	if err := os.WriteFile(w.path, []byte(editOriginal), 0o644); err != nil {
		t.Fatal(err)
	}
	observeDeliverable(w.ctx, "solve.py", []byte(editOriginal),
		ValidationKindSyntax, ValidationPassed, "")
	return w
}

const editContract = `{"task_mode":"work","output_knowledge":"declared",` +
	`"expected_outputs":["solve.py"],"verification_knowledge":"declared",` +
	`"verification":["pytest"]}`

// editAttempt is the post-edit full-file bytes a tool computes before V3.
// Deliberately different from the service's winner, so the route has a
// materially different candidate to decide about.
const editAttempt = "def solve(values):\n    return sum(v for v in values)\n"

func TestAnEditCandidateLandsThroughAConsumedGrant(t *testing.T) {
	w := editWorld(t, editContract, map[string]stubEffect{"pytest": {ExitCode: 0}})
	var landed string
	recs := captureShadow(t, func() {
		res := deliverEditCandidate(w.ctx, "edit_file", w.path, "solve.py",
			editOriginal, editAttempt)
		if res.Result == nil || !res.Result.Success {
			t.Fatalf("delivery did not complete: %+v", res)
		}
		landed = w.disk(t)
	})
	if landed != routeWinner {
		t.Fatal("the authorized candidate did not land")
	}
	minted, consumed := 0, 0
	for _, r := range recordsOfKind(recs, "authorization_grant_event") {
		switch r["event"] {
		case "minted":
			minted++
		case "consumed_authorized":
			consumed++
		}
	}
	if minted != 1 || consumed != 1 {
		t.Fatalf("minted %d consumed %d, want one each", minted, consumed)
	}
	if n := len(recordsOfKind(recs, "shadow_route_disposition")); n != 1 {
		t.Fatalf("%d routing endings, want exactly 1", n)
	}
	ends := recordsOfKind(recs, "shadow_delivery_disposition")
	if len(ends) != 1 || ends[0]["disposition"] != string(deliveryConsumedAndLanded) {
		t.Fatalf("delivery endings %v", ends)
	}
	if len(recordsOfKind(recs, "candidate_evidence_observation")) == 0 {
		t.Error("no candidate evidence was produced for an edit candidate")
	}
}

func TestAnEditCandidateSettles(t *testing.T) {
	w := editWorld(t, editContract, map[string]stubEffect{"pytest": {ExitCode: 0}})
	res := deliverEditCandidate(w.ctx, "edit_file", w.path, "solve.py",
		editOriginal, editAttempt)
	if res.Result == nil || !res.Result.Success {
		t.Fatalf("%+v", res)
	}
	if deliverySettlementFor(w.ctx, w.path) == nil {
		t.Fatal("an edit-route delivery wrote no settlement record")
	}
	if h := fileSHA256(w.ctx, w.path); h != contentSHA256(routeWinner) {
		t.Fatal("the ledger does not describe the landed bytes")
	}
}

func TestAnUnspecifiedClientCannotReceiveAnEditCandidate(t *testing.T) {
	w := editWorld(t, "", nil)
	recs := captureShadow(t, func() {
		res := deliverEditCandidate(w.ctx, "edit_file", w.path, "solve.py",
			editOriginal, editAttempt)
		if res.Delivered {
			t.Fatal("a contractless request received a V3 replacement")
		}
		if res.Content != editAttempt {
			t.Fatal("the model's own edit was not preserved")
		}
	})
	for _, r := range recordsOfKind(recs, "authorization_grant_event") {
		t.Errorf("a contractless request minted a grant: %v", r["event"])
	}
}

func TestARejectedEditCandidateKeepsTheModelsOwnEdit(t *testing.T) {
	// The staging command fails, so no behavioural evidence exists and the
	// authorization refuses.
	w := editWorld(t, editContract, map[string]stubEffect{"pytest": {ExitCode: 1}})
	recs := captureShadow(t, func() {
		res := deliverEditCandidate(w.ctx, "edit_file", w.path, "solve.py",
			editOriginal, editAttempt)
		if res.Delivered {
			t.Fatal("an unauthorized candidate landed")
		}
		if res.Content != editAttempt {
			t.Fatal("the model's own edit was not returned")
		}
	})
	for _, r := range recordsOfKind(recs, "authorization_grant_event") {
		if r["event"] == "consumed_authorized" {
			t.Error("a refused candidate consumed a grant")
		}
	}
	if got := w.disk(t); got != editOriginal {
		t.Error("the route wrote something before its caller decided to")
	}
}

func TestEveryEditToolUsesTheProtectedRoute(t *testing.T) {
	for _, tool := range []string{"edit_file", "structural_edit", "insert_after",
		"replace_lines"} {
		t.Run(tool, func(t *testing.T) {
			w := editWorld(t, editContract, map[string]stubEffect{"pytest": {ExitCode: 0}})
			recs := captureShadow(t, func() {
				res := deliverEditCandidate(w.ctx, tool, w.path, "solve.py",
					editOriginal, editAttempt)
				if res.Result == nil || !res.Result.Success {
					t.Fatalf("%s: %+v", tool, res)
				}
			})
			if len(recordsOfKind(recs, "shadow_route_disposition")) != 1 {
				t.Errorf("%s: route did not end exactly once", tool)
			}
			consumed := 0
			for _, r := range recordsOfKind(recs, "authorization_grant_event") {
				if r["event"] == "consumed_authorized" {
					consumed++
				}
			}
			if consumed != 1 {
				t.Errorf("%s: %d consumptions, want 1", tool, consumed)
			}
		})
	}
}

func TestAnEditRouteEndsExactlyOnceOnRefusal(t *testing.T) {
	w := editWorld(t, editContract, map[string]stubEffect{"pytest": {ExitCode: 1}})
	recs := captureShadow(t, func() {
		deliverEditCandidate(w.ctx, "edit_file", w.path, "solve.py",
			editOriginal, editAttempt)
	})
	ends := recordsOfKind(recs, "shadow_route_disposition")
	if len(ends) != 1 {
		t.Fatalf("%d routing endings", len(ends))
	}
	if ends[0]["disposition"] == string(routingCandidateAuthorized) {
		t.Error("a refusal claimed an authorized candidate")
	}
}

func TestTheEditRouteWritesNothingWhenItDeliversNothing(t *testing.T) {
	w := editWorld(t, editContract, map[string]stubEffect{"pytest": {ExitCode: 1}})
	before, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatal(err)
	}
	deliverEditCandidate(w.ctx, "edit_file", w.path, "solve.py", editOriginal, editAttempt)
	after, err := os.ReadFile(filepath.Clean(w.path))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the route mutated disk on a path that delivers nothing")
	}
}
