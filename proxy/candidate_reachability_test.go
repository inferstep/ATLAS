package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A mutation the producer never saw has to say so.
//
// Every predicate that turns a mutation away returns before a route entry
// exists, so without a bypass record such a mutation leaves no production
// record about the candidate route at all. Each case below is one such
// shape, reduced to a single small file.

// bypassWorld drives a real edit or write with a producer that is configured
// and must not be called. If generation runs, the fixture fails: these are
// skips, and a skip that reaches the pipeline is not the case under test.
type bypassWorld struct {
	ctx *AgentContext
	dir string
	v3  *int
}

func newBypassWorld(t *testing.T) *bypassWorld {
	t.Helper()
	dir := t.TempDir()
	v3Calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v3/"):
			v3Calls++
			http.Error(w, "the producer must not be consulted here", http.StatusTeapot)
		case r.URL.Path == "/internal/cyclomatic_complexity":
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
	return &bypassWorld{ctx: ctx, dir: dir, v3: &v3Calls}
}

func bypassRecords(recs []map[string]interface{}) []map[string]interface{} {
	return recordsOfKind(recs, "candidate_generation_bypass")
}

// A few lines of Python or Go, edited in place and never seen by the
// producer, record why the producer was skipped.
func TestAnEditTheProducerNeverSawSaysWhyItWasSkipped(t *testing.T) {
	cases := []struct {
		name, file, original, edited, want string
	}{{
		// Six lines of Python.
		name:     "a file below the tier floor",
		file:     "registry.py",
		original: "REG = {}\n\n\ndef find(name):\n    return REG.get(name)\n",
		edited:   "REG = {}\n\n\ndef find(name):\n    return REG.get(name.lower())\n",
		want:     string(bypassTierBelowThreshold),
	}, {
		// Eighteen lines of Go: over the tier floor, under the edit floor.
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

// A new Python file of a few lines, written through write_file, never reaches
// the producer and records why.
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
		// Every combination of a producer address, an iterating session and
		// an edit that warrants the pipeline.
		for _, c := range []struct {
			url                 string
			iterating, warrants bool
		}{
			{"", false, false}, {"", false, true}, {"", true, false}, {"", true, true},
			{"http://producer", false, false}, {"http://producer", false, true},
			{"http://producer", true, false}, {"http://producer", true, true},
		} {
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.V3URL = c.url
			// A session where a candidate could be delivered and no
			// deadline limits generation; the budget-ownership
			// reasons are pinned in budget_ownership_test.go.
			ctx.TaskContract = declaredOutputs("mod.py")
			wantWrite := tier >= Tier2Medium && ctx.V3URL != "" && !c.iterating
			if got := writeGenerationBypass(ctx, tier, c.iterating); (got == bypassNone) != wantWrite {
				t.Errorf("write tier=%v url=%q iter=%v: %q, want generation %v",
					tier, c.url, c.iterating, got, wantWrite)
			}
			wantEdit := tier >= Tier2Medium && c.warrants && ctx.V3URL != "" && !c.iterating
			if got := editGenerationBypass(ctx, tier, c.warrants, c.iterating); (got == bypassNone) != wantEdit {
				t.Errorf("edit tier=%v url=%q iter=%v warrants=%v: %q, want generation %v",
					tier, c.url, c.iterating, c.warrants, got, wantEdit)
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

// No byte-producing route decides activation for itself. A future route that
// re-inlines the condition would be unobservable again, and no behavioural
// test would notice.
func TestNoRouteDecidesGenerationForItself(t *testing.T) {
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
	for _, f := range []string{"tools.go", "edit_route_delivery.go", "candidate_reachability.go"} {
		for _, removed := range []string{"V3GenerationEnabled", "generationPermitted"} {
			if strings.Contains(body[f], removed) {
				t.Errorf("%s still reads %s", f, removed)
			}
		}
	}
}

// Every byte-producing route decides activation through the owners: they
// exist, the write route consults both, and every reason is emitted by
// somebody.
func TestOneActivationAuthorityPerCandidateRoute(t *testing.T) {
	body := map[string]string{}
	for _, f := range []string{"tools.go", "candidate_reachability.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		body[f] = codeWithoutComments(string(src))
	}
	owner := body["candidate_reachability.go"]
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

// Every consultation of an activation owner is recorded. A caller that asked
// whether to skip and then said nothing about the answer is the silent branch.
func TestEveryByteProducingRouteRecordsItsSkips(t *testing.T) {
	production := map[string]string{}
	for name, src := range proxySources(t) {
		production[name] = codeWithoutComments(src)
	}
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
}

// A new byte-producing route cannot ship without reachability instrumentation.
//
// The registry is the inventory of every route that may carry service-authored
// bytes toward disk. Each one decides whether to ask the producer, and a route
// that decides silently is invisible to exactly the analysis this file exists
// to support. Keyed off the registry rather than a hand-written list, so adding
// a route to one and not the other fails here.
//
// Every registered route is reached from a file that consults an owner. The
// shared delivery owner is reached FROM a route and decides no activation of
// its own, so it owes nothing here.
func TestEveryRegisteredRouteIsReachedThroughAnOwner(t *testing.T) {
	production := map[string]string{}
	for name, src := range proxySources(t) {
		production[name] = codeWithoutComments(src)
	}
	const sharedOwner = "candidate_delivery.go:deliverAuthorizedCandidate"
	for site := range v3DeliveryRoutes {
		if site == sharedOwner {
			continue
		}
		file, fn, _ := strings.Cut(site, ":")
		reached := false
		for name, other := range production {
			consultsAnOwner := strings.Contains(other, "writeGenerationBypass(") ||
				strings.Contains(other, "editGenerationBypass(")
			if consultsAnOwner && (name == file || strings.Contains(other, fn+"(")) {
				reached = true
				break
			}
		}
		if !reached {
			t.Errorf("%s can be entered without any activation owner deciding", site)
		}
	}
}

// Every registered route, except the shared delivery owner, names a production
// file and a function that file defines.
func TestTheDeliveryRegistryNamesProductionFunctions(t *testing.T) {
	production := map[string]string{}
	for name, src := range proxySources(t) {
		production[name] = codeWithoutComments(src)
	}
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
		}
	}
}

// The activation owners themselves still exist and still fail closed.
func TestTheActivationOwnersStillExist(t *testing.T) {
	src, err := os.ReadFile("candidate_reachability.go")
	if err != nil {
		t.Fatal(err)
	}
	owner := codeWithoutComments(string(src))
	for _, required := range []string{"func writeGenerationBypass(", "func editGenerationBypass(",
		"func recordCandidateGenerationBypass(", "func knownCandidateBypassReason("} {
		if !strings.Contains(owner, required) {
			t.Errorf("the reachability owner no longer defines %s", required)
		}
	}
}

// Optional candidate generation owns its budget. Under the default strict
// policy with no declared outputs, nothing the producer returns can reach
// disk, so a generation request there only spends time. Generation runs only
// where its result could be delivered and its cap leaves the declared
// allowance. The checks on the write itself run either way.

type budgetWorld struct {
	ctx *AgentContext
	dir string

	mu       sync.Mutex
	v3       int
	embedded string // compared with a previous version, a source containing this gets a finding
	unknown  string // a source containing this has an unresolved call
}

func (w *budgetWorld) v3Calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.v3
}

func newBudgetWorld(t *testing.T) *budgetWorld {
	t.Helper()
	w := &budgetWorld{dir: t.TempDir()}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in struct {
			Source, Code, Content, Previous string
		}
		_ = json.Unmarshal(raw, &in)
		body := in.Source + in.Code + in.Content
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"):
			w.mu.Lock()
			w.v3++
			w.mu.Unlock()
			http.Error(rw, "generation stub", http.StatusTeapot)
		case r.URL.Path == "/internal/cyclomatic_complexity":
			json.NewEncoder(rw).Encode(map[string]interface{}{"functions": []interface{}{}})
		case r.URL.Path == "/internal/embedded_script_check":
			out := map[string]interface{}{"ok": true, "findings": []interface{}{}}
			// A comparative finding (a loop the change stopped driving): only
			// visible against the previous version, which the final-byte syntax
			// check does not send.
			if w.embedded != "" && in.Previous != "" && strings.Contains(body, w.embedded) {
				out["findings"] = []map[string]interface{}{{"line": 3, "column": 1,
					"kind": "javascript", "where": "the <script> block", "message": "unexpected `)`"}}
			}
			json.NewEncoder(rw).Encode(out)
		case r.URL.Path == "/internal/structural_check":
			unresolved := []string{}
			if w.unknown != "" && strings.Contains(body, w.unknown) {
				unresolved = []string{w.unknown}
			}
			json.NewEncoder(rw).Encode(map[string]interface{}{"ok": true, "unresolved": unresolved})
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			valid := !strings.Contains(body, "def broken(")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: invalid syntax"}
			}
			json.NewEncoder(rw).Encode(out)
		default:
			json.NewEncoder(rw).Encode(map[string]interface{}{"ok": true})
		}
	}))
	t.Cleanup(srv.Close)
	ctx := NewAgentContext(w.dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.Ctx = context.Background()
	ctx.V3URL, ctx.SandboxURL = srv.URL, srv.URL
	ctx.Messages = []AgentMessage{{Role: "user", Content: "write the module"}}
	w.ctx = ctx
	return w
}

// A Tier 2 module: large enough that the write route consults the producer.
const budgetModule = `import math


def area(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return math.pi * radius * radius


def perimeter(radius):
    for _ in range(1):
        pass
    return 2 * math.pi * radius
`

func (w *budgetWorld) write(t *testing.T, rel, content string) *ToolResult {
	t.Helper()
	if tier := classifyFileTier(rel, content); tier < Tier2Medium {
		t.Fatalf("fixture must be Tier 2+, got %v", tier)
	}
	args, _ := json.Marshal(map[string]string{"path": rel, "content": content})
	return executeToolCall("write_file", args, w.ctx)
}

func (w *budgetWorld) seed(t *testing.T, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// (i) No contract: no target is grounded, so no generation request, and the
// model's bytes land.
func TestNoGenerationWhenNoCandidateCouldBeDelivered(t *testing.T) {
	w := newBudgetWorld(t)
	res := w.write(t, "solve.py", budgetModule)
	if !res.Success {
		t.Fatalf("the write did not land: %s", res.Error)
	}
	if n := w.v3Calls(); n != 0 {
		t.Errorf("%d generation request(s) in a session where no candidate could be delivered", n)
	}
	if got := writeGenerationBypass(w.ctx, Tier2Medium, false); got != bypassCandidateUndeliverable {
		t.Errorf("bypass reason = %q", got)
	}
	if got := editGenerationBypass(w.ctx, Tier2Medium, true, false); got != bypassCandidateUndeliverable {
		t.Errorf("edit bypass reason = %q", got)
	}
}

// (ii) Where a candidate could be delivered, generation is still requested:
// declared outputs, or declared work whose own call grounds the target.
func TestGenerationStillRunsWhereACandidateCouldBeDelivered(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contract *TaskContract
	}{
		{"declared outputs", declaredOutputs("solve.py")},
		{"declared work", &TaskContract{TaskMode: TaskModeWork,
			OutputKnowledge: KnowledgeUnspecified, VerificationKnowledge: KnowledgeUnspecified}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newBudgetWorld(t)
			w.ctx.TaskContract = tc.contract
			w.write(t, "solve.py", budgetModule)
			if w.v3Calls() == 0 {
				t.Error("no generation request although a candidate could be delivered")
			}
		})
	}
}

// (iii) Deliverable, but the cap would leave less than the allowance.
func TestNoGenerationWhenItsCapWouldBreachTheWorkAllowance(t *testing.T) {
	t.Setenv("ATLAS_V3_TIMEOUT", "300")
	allowance := workAllowance()
	if total, reserve := sessionBudget(); allowance != time.Duration(float64(total-reserve)*0.35) &&
		allowance != 120*time.Second {
		t.Fatalf("allowance %s is not max(120s, 35%% of the work budget)", allowance)
	}
	for _, c := range []struct {
		left     time.Duration
		generate bool
	}{
		// cap = min(300s, left/2); generation needs left - cap >= allowance
		{2*allowance + 20*time.Second, true},
		{2*allowance - 20*time.Second, false},
		{90 * time.Second, false},
	} {
		w := newBudgetWorld(t)
		w.ctx.TaskContract = declaredOutputs("solve.py")
		work, cancel := context.WithTimeout(context.Background(), c.left)
		w.ctx.Ctx = work
		got := writeGenerationBypass(w.ctx, Tier2Medium, false)
		if (got == bypassNone) != c.generate {
			t.Errorf("%s left (allowance %s): bypass %q, want generate=%v", c.left, allowance, got, c.generate)
		}
		if !c.generate {
			if got != bypassWorkAllowance {
				t.Errorf("%s left: reason %q, want %q", c.left, got, bypassWorkAllowance)
			}
			w.write(t, "solve.py", budgetModule)
			if n := w.v3Calls(); n != 0 {
				t.Errorf("%s left: %d generation request(s) breached the allowance", c.left, n)
			}
		}
		cancel()
	}
	// Uncapped generation can run to the deadline, so it never leaves one.
	t.Setenv("ATLAS_V3_TIMEOUT", "0")
	w := newBudgetWorld(t)
	w.ctx.TaskContract = declaredOutputs("solve.py")
	work, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	w.ctx.Ctx = work
	if got := writeGenerationBypass(w.ctx, Tier2Medium, false); got != bypassWorkAllowance {
		t.Errorf("uncapped generation with a deadline: %q, want %q", got, bypassWorkAllowance)
	}
}

// (iv) Every mandatory check still refuses its case on the direct path, with no
// generation request.
func TestMandatoryChecksRefuseOnTheDirectPath(t *testing.T) {
	healthyPage := strings.Repeat("<!-- layout -->\n", 60) +
		"<html><body>\n<script>\nlet x = 1;\n</script>\n</body></html>\n"
	for _, c := range []struct {
		name, rel, before, after string
		unknown, embedded        string // the budgetWorld fields of the same name
		want                     string
	}{
		{"healthy to broken syntax", "solve.py", budgetModule,
			strings.Replace(budgetModule, "def area(radius):", "def broken(radius:", 1), "", "", "syntax error"},
		{"unresolved name introduced", "solve.py", budgetModule,
			strings.Replace(budgetModule, "return math.pi * radius * radius", "return frobnicate(radius)", 1),
			"frobnicate", "", "frobnicate"},
		{"embedded script broken", "index.html", healthyPage,
			strings.Replace(healthyPage, "let x = 1;", "let x = 1;);", 1),
			"", "let x = 1;);", "<script>"},
		{"duplicate entrypoint", "solve.py",
			budgetModule + "\n\nif __name__ == \"__main__\":\n    print(area(1))\n",
			budgetModule + "\n\nif __name__ == \"__main__\":\n    print(area(1))\n\nif __name__ == \"__main__\":\n    print(area(2))\n",
			"", "", "__main__"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newBudgetWorld(t)
			w.unknown, w.embedded = c.unknown, c.embedded
			w.seed(t, c.rel, c.before)
			res := w.write(t, c.rel, c.after)
			if res.Success {
				t.Fatalf("the direct path landed a write its mandatory check refuses")
			}
			if !strings.Contains(res.Error, c.want) {
				t.Errorf("refusal does not name the problem (%q):\n%s", c.want, res.Error)
			}
			if b, _ := os.ReadFile(filepath.Join(w.dir, c.rel)); string(b) != c.before {
				t.Error("the file on disk changed")
			}
			if n := w.v3Calls(); n != 0 {
				t.Errorf("%d generation request(s)", n)
			}
		})
	}
}

// Where generation does run and the producer fails, the fallback writer applies
// the same comparative checks before landing the model's bytes.
func TestTheProducerFallbackAppliesTheComparativeChecks(t *testing.T) {
	w := newBudgetWorld(t)
	w.ctx.TaskContract = declaredOutputs("solve.py")
	before := budgetModule + "\n\nif __name__ == \"__main__\":\n    print(area(1))\n"
	w.seed(t, "solve.py", before)
	w.ctx.RecordFileRead(filepath.Join(w.dir, "solve.py"), before)
	after := before + "\nif __name__ == \"__main__\":\n    print(area(2))\n"
	res := w.write(t, "solve.py", after)
	if w.v3Calls() == 0 {
		t.Fatal("fixture did not reach the producer")
	}
	if res.Success || !strings.Contains(res.Error, "__main__") {
		t.Errorf("the fallback landed a duplicated entrypoint: success=%v %s", res.Success, res.Error)
	}
	if b, _ := os.ReadFile(filepath.Join(w.dir, "solve.py")); string(b) != before {
		t.Error("the file on disk changed")
	}
}

// Skipping generation must not remove a delivery that could have happened.
// For every contract/capture configuration the skip predicate calls
// undeliverable, the real candidate route is forced to run with the strongest
// candidate the fixture can offer; the caller's baseline must be what lands.
// Where the predicate says deliverable, the same fixture must be able to
// deliver in at least one configuration, or the matrix proves nothing.
func TestSkippingGenerationLosesNoDelivery(t *testing.T) {
	contracts := map[string]string{
		"no contract":                        "",
		"question":                           `{"task_mode":"question"}`,
		"work only (the reliability runner)": `{"task_mode":"work"}`,
		"work, an older client's policy":     `{"task_mode":"work","candidate_policy":"advisory"}`,
		"declared output":                    workContract,
		"verification declared, no outputs":  `{"task_mode":"work","verification_knowledge":"declared","verification":["python3 solve.py"]}`,
	}
	delivered := 0
	for name, contract := range contracts {
		for _, supported := range []bool{true, false} {
			for _, capture := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/supported=%v/capture=%v", name, supported, capture), func(t *testing.T) {
					if capture {
						t.Setenv(CandidateCaptureOnlyEnv, "1")
					}
					w := newAutomaticWorld(t, workContract, routeWinner, nil, supported)
					w.ctx.TaskContract = nil
					if contract != "" {
						w.ctx.TaskContract = mustContract(t, w.dir, contract)
					}
					deliverable := candidateDeliverable(w.ctx)
					if _, err := w.write(t); err != nil {
						t.Fatalf("write failed: %v", err)
					}
					got := w.disk(t)
					if !deliverable && got != routeBaseline {
						t.Fatalf("the skip predicate said undeliverable, but the forced route delivered %q", got)
					}
					if got == routeWinner {
						delivered++
					}
					t.Logf("deliverable=%v delivered=%v", deliverable, got == routeWinner)
				})
			}
		}
	}
	if delivered == 0 {
		t.Fatal("no configuration delivered: the fixture cannot show that a skip loses nothing")
	}
}

// With generation skipped, an unreachable check service is still visible: the
// client is told once, and every write's gate log carries the count.
func TestACheckServiceOutageIsVisibleWhenGenerationIsSkipped(t *testing.T) {
	w := newBudgetWorld(t)
	var notices []string
	w.ctx.StreamFn = func(event string, data interface{}) {
		if m, ok := data.(map[string]string); ok && event == "text" {
			notices = append(notices, m["content"])
		}
	}
	w.seed(t, "solve.py", budgetModule)
	// The check service is down; the sandbox (syntax) still answers.
	sandbox := w.ctx.SandboxURL
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	w.ctx.V3URL = dead.URL
	w.ctx.SandboxURL = sandbox
	for i := 0; i < 2; i++ {
		changed := strings.Replace(budgetModule, "return 2 * math.pi * radius", fmt.Sprintf("return %d * math.pi * radius", 3+i), 1)
		if res := w.write(t, "solve.py", changed); !res.Success {
			t.Fatalf("write %d did not land: %s", i, res.Error)
		}
	}
	if got := writeGenerationBypass(w.ctx, Tier2Medium, false); got != bypassCandidateUndeliverable {
		t.Fatalf("fixture must skip generation, got %q", got)
	}
	shown := 0
	for _, n := range notices {
		if strings.Contains(n, "unresolved-name check unavailable") {
			shown++
		}
	}
	if shown != 1 {
		t.Errorf("the outage was shown %d times, want exactly once: %v", shown, notices)
	}
	if s := w.ctx.checkServiceFailureSummary(); !strings.Contains(s, "unresolved-name=") {
		t.Errorf("the session does not count the outage: %q", s)
	}
}

// A check the session itself cancelled is not an outage.
func TestACancelledCheckIsNotReportedAsAnOutage(t *testing.T) {
	w := newBudgetWorld(t)
	var notices int
	w.ctx.StreamFn = func(event string, _ interface{}) {
		if event == "text" {
			notices++
		}
	}
	c, cancel := context.WithCancel(context.Background())
	cancel()
	w.ctx.Ctx = c
	noteCheckServiceUnavailable(w.ctx, "unresolved-name", "service unreachable")
	if notices != 0 || w.ctx.checkServiceFailureSummary() != "" {
		t.Errorf("a cancelled request was reported as an outage (notices=%d, summary=%q)",
			notices, w.ctx.checkServiceFailureSummary())
	}
}
