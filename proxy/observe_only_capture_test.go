package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

// What the observe-only machinery actually writes on a real delivery.
//
// The inertness comparison proves the production path did not change. That
// proof is only worth something if the observers RAN, so this captures what
// they emitted and checks the two properties they promise: every record is
// attributable, and no record carries content.

// captureShadow runs fn with a live shadow sink and returns the records it
// wrote.
func captureShadow(t *testing.T, fn func()) []map[string]interface{} {
	t.Helper()
	path := t.TempDir() + "/capture.jsonl"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	sink := newShadowSink(f)
	go sink.run()
	prev := activeShadowSink.Swap(sink)
	defer activeShadowSink.Store(prev)

	fn()

	// The production shutdown path, not finalize(). finalize() writes the
	// footer and releases the descriptor without closing the queue, so the
	// writer goroutine is still draining into a file that is already closed:
	// records submitted late in fn() are lost, and the goroutine never exits.
	// Every assertion in this package about what production recorded rests on
	// this helper, and one of them failed exactly that way -- a delivered
	// candidate whose consumed_authorized event was missing from the capture,
	// under full-suite load, in a test that passes in isolation.
	if err := sink.close(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("the capture did not finalise: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("undecodable record %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func recordsOfKind(recs []map[string]interface{}, kind string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, r := range recs {
		if r["record_kind"] == kind {
			out = append(out, r)
		}
	}
	return out
}

func TestTheObserversRunAndRecordAttributably(t *testing.T) {
	const secret = "TOKEN = 'hunter2'\nprint(7)\n"
	w := newAuthWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest --token=hunter2"]}`,
		"solve.py", secret, true)

	recs := captureShadow(t, func() {
		observeInvocationFeasibility(w.ctx, mintRouteEntry(w.ctx))
		outcome := fallbackSyntaxOutcomeFor(w.ctx, w.path, w.code).aggregate()
		if ev, id, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, w.code, outcome); ok {
			captureEntry := mintRouteEntry(w.ctx)
			authorizeCandidateDelivery(w.ctx, captureEntry, w.path, w.code, id, nil,
				[]proxyEvidence{ev}, "selected", nil, checkOutcome{Status: ValidationPassed},
				testMutationScope(w.ctx, captureEntry, w.path, w.code), automaticIntent{})
		} else {
			t.Error("the producer did not run on a declared code output")
		}
	})

	// Each record says truthfully whether it decided anything. Feasibility and
	// the evidence observation still decide nothing; the authorization now
	// does, for a request that declared obligations, and a record that still
	// claimed inertness would be lying about its own weight.
	for _, c := range []struct {
		kind       string
		influences bool
	}{
		{"shadow_invocation_feasibility", false},
		{"candidate_evidence_observation", false},
		{"candidate_authorization_decision", true},
	} {
		got := recordsOfKind(recs, c.kind)
		if len(got) != 1 {
			t.Fatalf("%s: %d records, want exactly one", c.kind, len(got))
		}
		rec := got[0]
		if rec["influences_live_decision"] != c.influences {
			t.Errorf("%s says influences_live_decision=%v, want %v",
				c.kind, rec["influences_live_decision"], c.influences)
		}
		if rec["request_id"] != "req-matrix" {
			t.Errorf("%s is not attributable to the request: %v", c.kind, rec["request_id"])
		}
		if rec["schema_version"] == nil || rec["build_version"] == nil {
			t.Errorf("%s carries no version", c.kind)
		}
	}

	// The evidence and authorization records join to the same candidate.
	ev := recordsOfKind(recs, "candidate_evidence_observation")[0]
	auth := recordsOfKind(recs, "candidate_authorization_decision")[0]
	for _, field := range []string{"invocation_id", "candidate_instance_id", "candidate_hash"} {
		if ev[field] == "" || ev[field] == nil {
			t.Errorf("the evidence record has no %s", field)
		}
		if ev[field] != auth[field] {
			t.Errorf("%s differs between the evidence (%v) and the decision (%v)",
				field, ev[field], auth[field])
		}
	}

	// The feasibility answer is about this invocation and says what it read.
	feas := recordsOfKind(recs, "shadow_invocation_feasibility")[0]
	if feas["generation_proceeded"] != true {
		t.Error("the feasibility record does not state that generation went ahead")
	}
	if feas["reason"] == nil || !feasibilityReasons[FeasibilityReason(feas["reason"].(string))] {
		t.Errorf("feasibility reason %v is outside the closed vocabulary", feas["reason"])
	}

	// And the decision is refused, because staging was never run for this
	// candidate so the declared command has no observation. A run that always
	// said yes would prove nothing.
	if auth["authorized"] != false {
		t.Error("the decision authorized a task whose command was never staged")
	}
	if auth["reason"] != string(ReasonEvidenceMissing) {
		t.Errorf("reason %v, want evidence_missing", auth["reason"])
	}

	// No record carries content, anywhere.
	blob, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		secret, "hunter2", "TOKEN", "print(7)", "pytest",
	} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the capture carries %q", needle)
		}
	}
}

// TestNoObserverEmitsAnSSEEventOrToolResult pins that none of the three
// observers can reach the caller: the private sink is the only destination.
//
// It reads the CODE, not the prose. A file may say in a comment that it emits
// no ToolResult; what it may not do is build or return one.
func TestNoObserverEmitsAnSSEEventOrToolResult(t *testing.T) {
	fset := token.NewFileSet()
	bannedTypes := map[string]bool{"ToolResult": true, "AgentMessage": true}
	bannedCalls := map[string]bool{
		"StreamFn": true, "endStream": true, "WriteFile": true,
		"Remove": true, "RemoveAll": true, "Rename": true, "Create": true,
	}
	for _, file := range []string{
		"evidence_wiring.go", "authorization_decision.go", "feasibility_decision.go",
	} {
		tree, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(tree, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if id, ok := node.Type.(*ast.Ident); ok && bannedTypes[id.Name] {
					t.Errorf("%s builds a %s", file, id.Name)
				}
			case *ast.CallExpr:
				var name string
				switch fn := node.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				}
				if bannedCalls[name] {
					t.Errorf("%s calls %s", file, name)
				}
			case *ast.IndexExpr:
				// ctx.Ledger[...] as an assignment target would be a mutation.
				if sel, ok := node.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Ledger" {
					t.Errorf("%s indexes the ledger", file)
				}
			}
			return true
		})
	}
}

// TestTheObserversAreSilentWithNoSink pins that the whole apparatus costs
// nothing when capture is off: no record, no panic, same answers.
func TestTheObserversAreSilentWithNoSink(t *testing.T) {
	prev := activeShadowSink.Swap(nil)
	defer activeShadowSink.Store(prev)

	w := newAuthWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", authPy, true)
	f := observeInvocationFeasibility(w.ctx, mintRouteEntry(w.ctx))
	if !f.Feasible {
		t.Errorf("feasibility changed with the sink off: %q", f.Reason)
	}
	outcome := fallbackSyntaxOutcomeFor(w.ctx, w.path, w.code).aggregate()
	ev, id, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, w.code, outcome)
	if !ok {
		t.Fatal("the producer went silent with the sink off")
	}
	sinkOffEntry := mintRouteEntry(w.ctx)
	a := authorizeCandidateDelivery(w.ctx, sinkOffEntry, w.path, w.code, id, nil,
		[]proxyEvidence{ev}, "selected", nil, checkOutcome{Status: ValidationPassed},
		testMutationScope(w.ctx, sinkOffEntry, w.path, w.code), automaticIntent{})
	if !a.Decision.Authorized {
		t.Errorf("the decision changed with the sink off: %q", a.Decision.Reason)
	}
	// And the grant is minted either way: capture observes, it never gates.
	if a.Grant == nil {
		t.Errorf("no grant with the sink off: %s", a.Refusal)
	}
}

// The capture helper loses nothing.
//
// A shadow sink writes from its own goroutine. Releasing the descriptor while
// that goroutine is still draining silently drops whatever was still queued,
// and every assertion in this package about what production recorded is only
// as good as the capture underneath it. The footer counts what the sink was
// handed and what it wrote, so the two have to balance.
func TestTheCaptureHelperLosesNoRecord(t *testing.T) {
	const submitted = 5000
	recs := captureShadow(t, func() {
		sink := activeShadowSink.Load()
		for i := 0; i < submitted; i++ {
			sink.submit(map[string]interface{}{
				"record_kind": "capture_probe", "seq": i,
				"influences_live_decision": false,
			})
		}
	})
	if got := len(recordsOfKind(recs, "capture_probe")); got != submitted {
		t.Errorf("%d of %d records survived the capture", got, submitted)
	}
	footers := recordsOfKind(recs, "task_contract_shadow_footer")
	if len(footers) != 1 {
		t.Fatalf("%d footers, want exactly one: a capture without one is incomplete",
			len(footers))
	}
	f := footers[0]
	accepted, _ := f["accepted"].(float64)
	written, _ := f["written"].(float64)
	dropped, _ := f["dropped"].(float64)
	errs, _ := f["errors"].(float64)
	if accepted != written+dropped {
		t.Errorf("the sink accepted %.0f records and accounted for %.0f "+
			"(%.0f written, %.0f dropped)", accepted, written+dropped, written, dropped)
	}
	if errs != 0 {
		t.Errorf("%.0f capture write errors: the descriptor closed under the writer", errs)
	}
}
