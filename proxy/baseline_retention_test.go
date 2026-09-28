package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A retained caller baseline is not a candidate delivery.
//
// When nothing materially different comes back, the route keeps the caller's
// own bytes and writes them directly. It must not stage those bytes as a
// candidate, take an authorization decision over them, or mint a one-time
// licence it then retires unused: a reader could not otherwise tell a retained
// baseline from an undelivered candidate.
//
// Retention is about the BYTES, not about the service's opinion of them. A
// proposal the service declined to certify is still a proposal, and what
// happens to it is decided by the evidence this machine produces about it --
// TestAnUncertifiedProposalReachesStaging is that case.

// retentionWorld is a route whose service returns the caller's own content,
// which is the ordinary shape when the pipeline finds nothing better.
func retentionWorld(t *testing.T, contract string) *routeWorld {
	t.Helper()
	// The declared command passes in staging, which is what made the live run
	// mint a grant over the caller's own bytes.
	w := newRouteWorld(t, contract, map[string]stubEffect{"pytest": {ExitCode: 0}})
	dir := w.dir
	stub := w.shell
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v3/generate":
			// A well-formed answer that offers the caller's own bytes back,
			// with a record saying nothing closed. There is no candidate here.
			body, _ := json.Marshal(map[string]interface{}{
				"code": routeBaseline, "passed": false, "phase_solved": "",
				"candidates_tested": 3, "winning_score": 0.4,
				"evidence": map[string]interface{}{
					"wire_version": "1.0.0", "record_schema_version": "1.1.0",
					"identity": map[string]interface{}{
						"contract_id": "c.v1", "contract_version": "1",
						"adapter_id": "python_compile", "adapter_version": "0.1.0-prototype",
						"artifact_scope": "solve.py", "evaluation_context_hash": "ctx",
						"candidate_content_hash": contentSHA256(routeBaseline),
					},
					"evaluation": map[string]interface{}{
						"execution_status": "ok", "supported": true,
						"evidence_strength": "syntax", "requirements_complete": false,
						"closure_eligible": false,
						"quality": map[string]interface{}{
							"required_coverage": 0.0, "optional_quality": 0.0, "overall": 0.0},
					},
					"coverage":  map[string]interface{}{"required": []string{}, "demonstrated": []string{}},
					"selection": map[string]interface{}{"status": "best_not_closure_eligible", "reason": "no closure"},
					"delivery": map[string]interface{}{
						"delivered_content_hash": "", "describes_delivered_candidate": false},
				},
			})
			rw.Header().Set("Content-Type", "text/event-stream")
			fl, _ := rw.(http.Flusher)
			for _, line := range []string{"event: result", "data: " + string(body), "", "data: [DONE]", ""} {
				fmt.Fprint(rw, line+"\n")
				if fl != nil {
					fl.Flush()
				}
			}
		case r.URL.Path == "/internal/structural_check":
			json.NewEncoder(rw).Encode(map[string]interface{}{"ok": true, "unresolved": []string{}})
		case r.URL.Path == "/internal/cyclomatic_complexity":
			json.NewEncoder(rw).Encode(map[string]interface{}{"functions": []interface{}{}})
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(rw).Encode(map[string]interface{}{"valid": true})
		default:
			stub.srv.Config.Handler.ServeHTTP(rw, r)
		}
	}))
	t.Cleanup(srv.Close)
	w.ctx.V3URL, w.ctx.SandboxURL = srv.URL, srv.URL
	_ = dir
	return w
}

const retentionContract = `{"task_mode":"work","output_knowledge":"declared",` +
	`"expected_outputs":["solve.py"],"verification_knowledge":"declared",` +
	`"verification":["pytest"]}`

func TestARetainedBaselineMintsNoGrant(t *testing.T) {
	w := retentionWorld(t, retentionContract)
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})
	for _, r := range recordsOfKind(recs, "authorization_grant_event") {
		t.Errorf("a retained baseline minted a grant: %v", r["event"])
	}
	if n := len(recordsOfKind(recs, "shadow_delivery_disposition")); n != 0 {
		t.Errorf("%d delivery dispositions for a route that delivered nothing", n)
	}
	// The route was contemplated and said so.
	ends := recordsOfKind(recs, "shadow_route_disposition")
	if len(ends) != 1 || ends[0]["disposition"] != string(routingBaselineRetained) {
		t.Fatalf("routing endings %v", ends)
	}
	if len(recordsOfKind(recs, "shadow_invocation_feasibility")) != 1 {
		t.Error("the route no longer records that it was contemplated")
	}
}

func TestARetainedBaselineLandsTheCallersOwnBytes(t *testing.T) {
	w := retentionWorld(t, retentionContract)
	res, err := w.write(t)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("the caller's own content did not land: %v", res.Error)
	}
	if got := w.disk(t); got != routeBaseline {
		t.Fatal("disk does not hold the caller's bytes")
	}
	if res.AuthorizedDeliveryHash != "" {
		t.Error("a retained baseline claimed an authorized delivery")
	}
	if res.MutationStatus != MutationApplied {
		t.Errorf("mutation status %q", res.MutationStatus)
	}
	// The ledger observed the landed bytes exactly once, as before.
	if h := fileSHA256(w.ctx, w.path); h != contentSHA256(routeBaseline) {
		t.Error("the ledger does not describe the landed bytes")
	}
}

func TestARetainedBaselineStagesNothing(t *testing.T) {
	w := retentionWorld(t, retentionContract)
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})
	if seen := w.shell.commandsSeen(); len(seen) != 0 {
		t.Errorf("%d staging executions for bytes nobody proposed replacing: %v",
			len(seen), seen)
	}
	for _, r := range recordsOfKind(recs, "candidate_evidence_observation") {
		if r["source"] == "client_declared_verification" {
			t.Error("the caller's own bytes were staged as a candidate")
		}
	}
}

func TestAMateriallyDifferentCandidateStillMintsAndConsumesOneGrant(t *testing.T) {
	w := newRouteWorld(t, retentionContract, map[string]stubEffect{"pytest": {ExitCode: 0}})
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})
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
		t.Fatalf("minted %d consumed %d, want exactly one of each", minted, consumed)
	}
	if got := w.disk(t); got != routeWinner {
		t.Error("the authorized candidate did not land")
	}
	ends := recordsOfKind(recs, "shadow_delivery_disposition")
	if len(ends) != 1 || ends[0]["disposition"] != string(deliveryConsumedAndLanded) {
		t.Fatalf("delivery endings %v", ends)
	}
}

func TestARetainedBaselineLeavesNoStrayFile(t *testing.T) {
	w := retentionWorld(t, retentionContract)
	if _, err := w.write(t); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "solve.py" && e.Name() != "task_contract.json" {
			t.Errorf("stray %s", e.Name())
		}
	}
}
