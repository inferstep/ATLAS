package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A mutation the producer never saw has to say so.
//
// These reproduce the corrected eligibility pilot's own shapes. Nine of its
// twenty-four families mutated the workspace and produced not one production
// record about the candidate route, because every predicate that turns a
// mutation away returns before a route entry exists. Each case below is one
// of those families, reduced to the file that silenced it.

// bypassWorld drives a real edit or write with a producer that is configured
// and must not be called. If generation runs, the fixture fails: these are
// skips, and a skip that reaches the pipeline is not the case under test.
type bypassWorld struct {
	ctx  *AgentContext
	dir  string
	v3   *int
	sand *int
}

func newBypassWorld(t *testing.T) *bypassWorld {
	t.Helper()
	dir := t.TempDir()
	v3Calls, sandCalls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v3/"):
			v3Calls++
			http.Error(w, "the producer must not be consulted here", http.StatusTeapot)
		case r.URL.Path == "/internal/cyclomatic_complexity":
			sandCalls++
			json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
		case r.URL.Path == "/internal/structural_check":
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "unresolved": []string{}})
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
		}
	}))
	t.Cleanup(srv.Close)
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-bypass")
	ctx.V3URL, ctx.SandboxURL = srv.URL, srv.URL
	return &bypassWorld{ctx: ctx, dir: dir, v3: &v3Calls, sand: &sandCalls}
}

func bypassRecords(recs []map[string]interface{}) []map[string]interface{} {
	return recordsOfKind(recs, "candidate_generation_bypass")
}

// The pilot's own repair fixtures: a few lines of Python or Go, edited in
// place, and never seen by the producer. Nothing in production said why.
func TestAnEditTheProducerNeverSawSaysWhyItWasSkipped(t *testing.T) {
	cases := []struct {
		name, file, original, edited, want string
	}{{
		// pilot_case_insensitive_repair: registry.py, six lines.
		name:     "a file below the tier floor",
		file:     "registry.py",
		original: "REG = {}\n\n\ndef find(name):\n    return REG.get(name)\n",
		edited:   "REG = {}\n\n\ndef find(name):\n    return REG.get(name.lower())\n",
		want:     string(bypassTierBelowThreshold),
	}, {
		// pilot_go_dedupe_repair: slices.go, eighteen lines — over the tier
		// floor, under the edit floor.
		name:     "a file over the tier floor and under the edit floor",
		file:     "slices.go",
		original: "package main\n\nimport \"fmt\"\n\nfunc Dedupe(in []string) []string {\n\tseen := map[string]bool{}\n\tout := []string{}\n\tfor _, v := range in {\n\t\tif seen[v] {\n\t\t\tcontinue\n\t\t}\n\t\tout = append(out, v)\n\t}\n\treturn out\n}\n\nfunc main() {\n\tfmt.Println(Dedupe(nil))\n}\n",
		edited:   "package main\n\nimport \"fmt\"\n\nfunc Dedupe(in []string) []string {\n\tseen := map[string]bool{}\n\tout := []string{}\n\tfor _, v := range in {\n\t\tif seen[v] {\n\t\t\tcontinue\n\t\t}\n\t\tseen[v] = true\n\t\tout = append(out, v)\n\t}\n\treturn out\n}\n\nfunc main() {\n\tfmt.Println(Dedupe(nil))\n}\n",
		want:     string(bypassEditBelowComplexityFloor),
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newBypassWorld(t)
			path := filepath.Join(w.dir, tc.file)
			if err := os.WriteFile(path, []byte(tc.original), 0o644); err != nil {
				t.Fatal(err)
			}
			var out editRouteOutcome
			recs := captureShadow(t, func() {
				out = runEditPipeline(w.ctx, "edit_file", path, tc.file,
					tc.original, tc.edited)
			})
			if *w.v3 != 0 {
				t.Fatalf("the producer was consulted %d times on a skipped route", *w.v3)
			}
			if out.Content != tc.edited {
				t.Error("the caller's own edit is not what came back")
			}
			got := bypassRecords(recs)
			if len(got) != 1 {
				t.Fatalf("%d bypass records for a skipped mutation, want 1", len(got))
			}
			if got[0]["reason"] != tc.want {
				t.Errorf("reason %v, want %s", got[0]["reason"], tc.want)
			}
			if got[0]["request_id"] != "req-bypass" {
				t.Errorf("the record is not attributable: %v", got[0]["request_id"])
			}
			if got[0]["influences_live_decision"] != false {
				t.Error("a capture record claims it influenced the decision")
			}
		})
	}
}

// pilot_router_and_handlers: two new Python files of seven and nine lines,
// written through write_file, neither of which reached the producer.
func TestANewFileBelowTheTierFloorSaysWhyItWasSkipped(t *testing.T) {
	w := newBypassWorld(t)
	const body = "def route(p):\n    if p == \"/\":\n        return home\n    return miss\n"
	args, _ := json.Marshal(map[string]string{"path": "router.py", "content": body})
	var res *ToolResult
	recs := captureShadow(t, func() {
		res = executeToolCall("write_file", args, w.ctx)
	})
	if res == nil || !res.Success {
		t.Fatalf("the direct write failed: %+v", res)
	}
	if *w.v3 != 0 {
		t.Fatalf("the producer was consulted %d times on a Tier1 file", *w.v3)
	}
	got := bypassRecords(recs)
	if len(got) != 1 {
		t.Fatalf("%d bypass records, want 1", len(got))
	}
	if got[0]["reason"] != string(bypassTierBelowThreshold) {
		t.Errorf("reason %v, want %s", got[0]["reason"], bypassTierBelowThreshold)
	}
	if got[0]["tool"] != "write_file" {
		t.Errorf("tool %v", got[0]["tool"])
	}
}

// The owners answer the routing rules and nothing else: the file must be
// worth the pipeline, a producer must exist, and the session must not be
// iterating on a failure. There is no switch that turns generation off.
func TestTheBypassOwnersAreTheRoutingRules(t *testing.T) {
	dir := t.TempDir()
	for _, tier := range []Tier{Tier0Conversational, Tier1Simple, Tier2Medium, Tier3Hard} {
		for _, url := range []string{"", "http://producer"} {
			for _, iterating := range []bool{false, true} {
				for _, warrants := range []bool{false, true} {
					ctx := NewAgentContext(dir, Tier2Medium)
					ctx.V3URL = url
					// A session where a candidate could be delivered and no
					// deadline limits generation; the budget-ownership
					// reasons are pinned in budget_ownership_test.go.
					ctx.TaskContract = declaredOutputs("mod.py")
					wantWrite := tier >= Tier2Medium && ctx.V3URL != "" && !iterating
					if got := writeGenerationBypass(ctx, tier, iterating); (got == bypassNone) != wantWrite {
						t.Errorf("write tier=%v url=%q iter=%v: %q, want generation %v",
							tier, url, iterating, got, wantWrite)
					}
					wantEdit := tier >= Tier2Medium && warrants && ctx.V3URL != "" && !iterating
					if got := editGenerationBypass(ctx, tier, warrants, iterating); (got == bypassNone) != wantEdit {
						t.Errorf("edit tier=%v url=%q iter=%v warrants=%v: %q, want generation %v",
							tier, url, iterating, warrants, got, wantEdit)
					}
				}
			}
		}
	}
	// A nil context has no producer, and says that rather than panicking.
	if writeGenerationBypass(nil, Tier3Hard, false) != bypassProducerNotConfigured {
		t.Error("a nil context did not report a missing producer")
	}
	if editGenerationBypass(nil, Tier3Hard, true, false) != bypassProducerNotConfigured {
		t.Error("a nil context did not report a missing producer")
	}
}

// The vocabulary is closed and fails closed, and a consulted route writes
// nothing.
func TestTheBypassVocabularyIsClosed(t *testing.T) {
	for _, r := range []candidateBypassReason{
		bypassTierBelowThreshold, bypassEditBelowComplexityFloor,
		bypassProducerNotConfigured,
		bypassActiveDebugIteration, bypassProposalFailedSyntaxGuard,
		bypassCandidateUndeliverable, bypassWorkAllowance,
		bypassUnclassified,
	} {
		if !knownCandidateBypassReason(r) {
			t.Errorf("%q is not in the closed set", r)
		}
	}
	if knownCandidateBypassReason(bypassNone) {
		t.Error("bypassNone is a skip")
	}
	if knownCandidateBypassReason("invented") {
		t.Error("an invented reason was accepted")
	}
	recs := captureShadow(t, func() {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-closed")
		recordCandidateGenerationBypass(ctx, "write_file", bypassNone, Tier3Hard, 9)
		recordCandidateGenerationBypass(ctx, "write_file", "invented", Tier3Hard, 9)
	})
	got := bypassRecords(recs)
	if len(got) != 1 {
		t.Fatalf("%d records, want only the fail-closed one", len(got))
	}
	if got[0]["reason"] != string(bypassUnclassified) {
		t.Errorf("an unknown reason was written through as %v", got[0]["reason"])
	}
}

// The record answers the reachability question and carries nothing else. A
// capture that shipped the bytes it declined to improve would be a content
// leak on the one path that never asked the user about content at all.
func TestTheBypassRecordCarriesNoContent(t *testing.T) {
	w := newBypassWorld(t)
	const secret = "TOKEN = 'hunter2'\nprint(7)\n"
	path := filepath.Join(w.dir, "secrets.py")
	if err := os.WriteFile(path, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	recs := captureShadow(t, func() {
		runEditPipeline(w.ctx, "edit_file", path, "secrets.py", secret,
			secret+"print(8)\n")
	})
	got := bypassRecords(recs)
	if len(got) != 1 {
		t.Fatalf("%d bypass records, want 1", len(got))
	}
	blob, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"hunter2", "TOKEN", "print(7)", "secrets.py", w.dir} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the bypass record carries %q: %s", needle, blob)
		}
	}
	if got[0]["content_lines"] == nil || got[0]["file_tier"] == nil {
		t.Error("the record omits the predicate inputs that decided it")
	}
}

// Every byte-producing route decides activation through the owners, and
// nowhere else. A future route that re-inlines the condition would be
// unobservable again, and no behavioural test would notice.
func TestOneActivationAuthorityPerCandidateRoute(t *testing.T) {
	body := map[string]string{}
	for _, f := range []string{"tools.go", "edit_route_delivery.go", "candidate_reachability.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		body[f] = codeWithoutComments(string(src))
	}
	// No route may re-inline the activation decision. The two shapes below
	// are the conditions the owners replaced, and finding either one back in
	// a route file means a mutation can be turned away silently again.
	for _, f := range []string{"tools.go", "edit_route_delivery.go"} {
		for _, inlined := range []string{
			"fileTier >= Tier2Medium && ctx.V3URL",
			"fileTier < Tier2Medium || !editWarrantsV3",
			"editWarrantsV3(finalContent, cc, ccOK) && ctx.V3URL",
		} {
			if strings.Contains(body[f], inlined) {
				t.Errorf("%s decides generation for itself: %s", f, inlined)
			}
		}
	}
	// No site reads a switch to decide whether to generate: there is none, and
	// the owners decide from the routing rules alone.
	owner := body["candidate_reachability.go"]
	for _, f := range []string{"tools.go", "edit_route_delivery.go", "candidate_reachability.go"} {
		for _, removed := range []string{"V3GenerationEnabled", "generationPermitted"} {
			if strings.Contains(body[f], removed) {
				t.Errorf("%s still reads %s", f, removed)
			}
		}
	}
	if !strings.Contains(owner, "func writeGenerationBypass(") ||
		!strings.Contains(owner, "func editGenerationBypass(") {
		t.Error("the owners are gone; repoint this test at their replacement")
	}
	// Both owners are reached, and every reason is emitted by somebody.
	for _, call := range []string{"writeGenerationBypass(", "editGenerationBypass("} {
		if !strings.Contains(body["tools.go"], call) {
			t.Errorf("tools.go does not consult %s", call)
		}
	}
	for _, reason := range []string{"bypassProposalFailedSyntaxGuard"} {
		if !strings.Contains(body["tools.go"], reason) {
			t.Errorf("no route reports %s", reason)
		}
	}
	// And every registered delivery route's own file records a skip.
	if !strings.Contains(body["tools.go"], "recordCandidateGenerationBypass(") {
		t.Error("the write route records no skip")
	}
}

// A new byte-producing route cannot ship without reachability instrumentation.
//
// The registry is the inventory of every route that may carry service-authored
// bytes toward disk. Each one decides whether to ask the producer, and a route
// that decides silently is invisible to exactly the analysis this file exists
// to support. Keyed off the registry rather than a hand-written list, so adding
// a route to one and not the other fails here.
func TestEveryByteProducingRouteRecordsItsSkips(t *testing.T) {
	production := map[string]string{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		src, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		production[n] = codeWithoutComments(string(src))
	}

	// Every consultation of an owner is recorded. A caller that asked whether
	// to skip and then said nothing about the answer is the silent branch.
	consultations := 0
	for name, body := range production {
		if name == "candidate_reachability.go" {
			continue
		}
		lines := strings.Split(body, "\n")
		for i, line := range lines {
			if !strings.Contains(line, "writeGenerationBypass(") &&
				!strings.Contains(line, "editGenerationBypass(") {
				continue
			}
			consultations++
			window := strings.Join(lines[i:min(i+7, len(lines))], "\n")
			if !strings.Contains(window, "recordCandidateGenerationBypass(") {
				t.Errorf("%s:%d consults an activation owner and records nothing",
					name, i+1)
			}
		}
	}
	if consultations < 3 {
		t.Errorf("%d activation consultations; want the write route and both "+
			"edit entry points", consultations)
	}

	// And every registered route is reached from a file that consults one.
	// The shared delivery owner is reached FROM a route and decides no
	// activation of its own, so it owes nothing here.
	const sharedOwner = "candidate_delivery.go:deliverAuthorizedCandidate"
	for site := range v3DeliveryRoutes {
		if site == sharedOwner {
			continue
		}
		file, fn, _ := strings.Cut(site, ":")
		body, ok := production[file]
		if !ok {
			t.Errorf("%s: the registry names a file that is not production", site)
			continue
		}
		if !strings.Contains(body, "func "+fn+"(") {
			t.Errorf("%s: the registry names a function this file does not define", site)
			continue
		}
		reached := false
		for name, other := range production {
			if !strings.Contains(other, "writeGenerationBypass(") &&
				!strings.Contains(other, "editGenerationBypass(") {
				continue
			}
			if name == file || strings.Contains(other, fn+"(") {
				reached = true
				break
			}
		}
		if !reached {
			t.Errorf("%s can be entered without any activation owner deciding", site)
		}
	}

	// The owners themselves still exist and still fail closed.
	owner := production["candidate_reachability.go"]
	for _, required := range []string{"func writeGenerationBypass(", "func editGenerationBypass(",
		"func recordCandidateGenerationBypass(", "func knownCandidateBypassReason("} {
		if !strings.Contains(owner, required) {
			t.Errorf("the reachability owner no longer defines %s", required)
		}
	}
}
