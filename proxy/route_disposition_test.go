package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- the vocabularies are closed ------------------------------------------

// Every routing disposition is registered.
func TestRoutingDispositionsAreAClosedSet(t *testing.T) {
	for _, disposition := range []routingDisposition{
		routingSkippedInfeasible,
		routingProducerUnavailable,
		routingProducerTimedOut,
		routingCancelled,
		routingNoCandidate,
		routingNotClosureEligible,
		routingRevokedByGate,
		routingBaselineRetained,
		routingAuthorizationRefused,
		routingUnclassified,
		routingCandidateAuthorized,
	} {
		if !knownRoutingDisposition(disposition) {
			t.Errorf("%q is used and not registered", disposition)
		}
	}
	if knownRoutingDisposition("delivered") {
		t.Error("an unregistered disposition was accepted")
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

// How a route that kept the caller's bytes says so. Two endings keep them:
// nothing new came back, or a candidate came back and a gate withdrew it. Only
// the second has a candidate to name; the first must not name the caller's
// own bytes as one.

// retainWorld is the real write route with a producer whose answer, and whose
// structural verdict on that answer, are both fixtures.
type retainWorld struct {
	ctx  *AgentContext
	dir  string
	path string
}

func newRetainWorld(t *testing.T, produced string, unresolvedFor string) *retainWorld {
	t.Helper()
	dir := t.TempDir()
	hash := contentSHA256(produced)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v3/generate":
			body, _ := json.Marshal(map[string]interface{}{
				"code": produced, "passed": true, "phase_solved": "phase_one",
				"candidates_tested": 3, "winning_score": 0.9,
				"evidence": map[string]interface{}{
					"wire_version": "1.0.0", "record_schema_version": "1.1.0",
					"identity": map[string]interface{}{
						"contract_id": "c.v1", "contract_version": "1",
						"adapter_id": "python_compile", "adapter_version": "0.1.0-prototype",
						"artifact_scope": "solve.py", "evaluation_context_hash": "ctx",
						"candidate_content_hash": hash,
					},
					"evaluation": map[string]interface{}{
						"execution_status": "ok", "supported": true,
						"evidence_strength": "behavioral", "requirements_complete": true,
						"closure_eligible": true,
						"quality": map[string]interface{}{
							"required_coverage": 1.0, "optional_quality": 1.0, "overall": 1.0},
					},
					"coverage":  map[string]interface{}{"required": []string{}, "demonstrated": []string{}},
					"selection": map[string]interface{}{"status": "verified_winner", "reason": "highest"},
					"delivery": map[string]interface{}{
						"delivered_content_hash": hash, "describes_delivered_candidate": true},
				},
			})
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			for _, line := range []string{"event: result", "data: " + string(body), "", "data: [DONE]", ""} {
				fmt.Fprint(w, line+"\n")
				if fl != nil {
					fl.Flush()
				}
			}
		case r.URL.Path == "/internal/structural_check":
			// The gate answers about whatever source it was handed: only the
			// named bytes carry an unresolved call.
			var in struct {
				Source string `json:"source"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			bad := unresolvedFor != "" && in.Source == unresolvedFor
			out := map[string]interface{}{"ok": true, "unresolved": []string{}}
			if bad {
				out["unresolved"] = []string{"missing_helper"}
			}
			json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/internal/cyclomatic_complexity":
			json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
		}
	}))
	t.Cleanup(srv.Close)
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-retain")
	ctx.V3URL, ctx.SandboxURL = srv.URL, srv.URL
	ctx.HumanTask = "Make solve fast."
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`)
	return &retainWorld{ctx: ctx, dir: dir, path: filepath.Join(dir, "solve.py")}
}

func (w *retainWorld) disk(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func routeDispositionRecord(t *testing.T, recs []map[string]interface{}) map[string]interface{} {
	t.Helper()
	got := recordsOfKind(recs, "shadow_route_disposition")
	if len(got) != 1 {
		t.Fatalf("%d route dispositions, want 1", len(got))
	}
	return got[0]
}

// Nothing materially different came back. There is no candidate, so the record
// names none, and the ending says the caller's own bytes were kept.
func TestARetainedBaselineNamesNoCandidate(t *testing.T) {
	w := newRetainWorld(t, routeBaseline, "")
	recs := captureShadow(t, func() {
		if _, err := writeFileWithV3(w.path, routeBaseline, w.ctx); err != nil {
			t.Fatalf("write failed: %v", err)
		}
	})
	rec := routeDispositionRecord(t, recs)
	if rec["disposition"] != string(routingBaselineRetained) {
		t.Errorf("disposition %v, want %s", rec["disposition"], routingBaselineRetained)
	}
	if h, ok := rec["candidate_hash"]; ok {
		t.Errorf("a retained baseline named a candidate: %v", h)
	}
	if got := w.disk(t); got != routeBaseline {
		t.Errorf("disk holds %q", got)
	}
	// A retained baseline is not a candidate at any boundary, and the two
	// records have to agree about that.
	if n := len(recordsOfKind(recs, "candidate_mutation_scope")); n != 0 {
		t.Errorf("%d mutation scope records for a non-proposal", n)
	}
}

// A candidate WAS proposed and a gate withdrew it. Different ending, real
// candidate to name, and the record says both.
func TestAWithdrawnCandidateIsNamedAndSaysItWasRevoked(t *testing.T) {
	w := newRetainWorld(t, routeWinner, routeWinner)
	recs := captureShadow(t, func() {
		if _, err := writeFileWithV3(w.path, routeBaseline, w.ctx); err != nil {
			t.Fatalf("write failed: %v", err)
		}
	})
	rec := routeDispositionRecord(t, recs)
	if rec["disposition"] != string(routingRevokedByGate) {
		t.Fatalf("disposition %v, want %s", rec["disposition"], routingRevokedByGate)
	}
	if rec["candidate_hash"] != contentSHA256(routeWinner) {
		t.Errorf("candidate_hash %v, want the proposal's own hash", rec["candidate_hash"])
	}
	if got := w.disk(t); got != routeBaseline {
		t.Errorf("disk holds %q, want the caller's own content", got)
	}
}
