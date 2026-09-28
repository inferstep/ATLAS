package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- how a route that kept the caller's bytes says so -----------------------
//
// `baseline_retained` covered two different endings and named a candidate for
// both. The pilot has one of each shape, and route entry 1 of
// pilot_go_duration_label proves the naming was wrong: the hash recorded under
// `candidate_hash` is the sha256 of the main.go the MODEL wrote, which is the
// file that was on disk when the run ended.

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
