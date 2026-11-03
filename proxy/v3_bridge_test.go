package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/types"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itigges22/atlas-proxy/internal/wire"
)

// fakePlanServer streams a canned SSE plan response that mirrors what
// v3-service actually emits. Useful so the bridge test doesn't depend on
// the live Python service.
func fakePlanServer(t *testing.T, sse string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/plan" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("test server doesn't support flushing")
		}
		w.WriteHeader(http.StatusOK)
		f.Flush()
		fmt.Fprint(w, sse)
		f.Flush()
	}))
}

func TestV3StageToEventCoversPlanStages(t *testing.T) {
	planStages := []string{
		"plan_start", "plan_candidate", "plan_candidate_unparseable",
		"plan_candidate_error", "plan_candidate_scored", "plan_selected",
		"plan_failed",
	}
	for _, s := range planStages {
		if got := v3StageToEvent(s); got != "v3_plan" {
			t.Errorf("v3StageToEvent(%q) = %q, want v3_plan", s, got)
		}
	}
}

// ---------------------------------------------------------------------------
// v3CallTimeout — the cap the callers pass to v3client.GenerateStreaming,
// whose own tests are in internal/v3client.
// ---------------------------------------------------------------------------

// The default is 300s because PlanSearch spends two LLM calls per candidate:
// three candidates cost about 162s at about 22s per call, before the probe and
// self-test that come first.
func TestV3CallTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want time.Duration
	}{
		{"default is 300s", "", 300 * time.Second},
		{"override in seconds", "30", 30 * time.Second},
		{"zero turns the cap off", "0", 0},
		{"a value that is not a number gives the default", "soon", 300 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ATLAS_V3_TIMEOUT", tc.env)
			if got := v3CallTimeout(); got != tc.want {
				t.Errorf("ATLAS_V3_TIMEOUT=%q gave %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// v3clientCall is one call into the V3 client: the file it is in, the function
// it calls and the expression it passes as the request id.
type v3clientCall struct{ file, fn, requestID string }

// v3clientCalls lists every call to v3client.GenerateStreaming or
// v3client.PlanStreaming in the proxy's code.
func v3clientCalls(t *testing.T) []v3clientCall {
	var out []v3clientCall
	for name, f := range proxyFiles(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || types.ExprString(sel.X) != "v3client" {
				return true
			}
			if sel.Sel.Name == "GenerateStreaming" || sel.Sel.Name == "PlanStreaming" {
				out = append(out, v3clientCall{name, sel.Sel.Name, types.ExprString(call.Args[2])})
			}
			return true
		})
	}
	return out
}

// Each production V3 call forwards its request's id. The client read the id
// from the context itself before it became a package; now a caller could
// leave it out, and the service joins a candidate pool to its request by it.
func TestEveryV3CallForwardsTheRequestID(t *testing.T) {
	calls := v3clientCalls(t)
	if len(calls) == 0 {
		t.Fatal("no call to v3client was found, so this guard reads nothing")
	}
	for _, c := range calls {
		if c.requestID != "requestIDOf(ctx)" {
			t.Errorf("%s: v3client.%s gets %s as the request id, want requestIDOf(ctx)", c.file, c.fn, c.requestID)
		}
	}
}

func sseLines(w http.ResponseWriter, lines ...string) {
	fl, _ := w.(http.Flusher)
	for _, l := range lines {
		fmt.Fprint(w, l+"\n")
		if fl != nil {
			fl.Flush()
		}
	}
}

// generatePlan is the agent loop's entry into V3 planning. The transport
// under it is covered above; this test covers the progress callback it
// installs, which drops token-level events. The planner asks for 3 candidates
// and the LLM emits about 150 token deltas each, so passing them on would put
// about 450 v3_plan rows into the TUI's pipeline pane for one plan. Only the
// structural stages belong on the stream.
func TestGeneratePlanDropsTokenNoiseFromTheStream(t *testing.T) {
	var sse []string
	add := func(line string) { sse = append(sse, line, "") }
	add(`data: {"stage":"plan_start","detail":"generating 3 candidates"}`)
	add(`data: {"stage":"llm_start","detail":"candidate 0"}`)
	for i := 0; i < 40; i++ { // stand-in for the ~450 real deltas
		add(`data: {"stage":"token","detail":"tok"}`)
	}
	add(`data: {"stage":"llm_end","detail":"candidate 0"}`)
	add(`data: {"stage":"plan_candidate_scored","detail":"candidate 1 score=0.80","data":{"index":0,"score":0.8}}`)
	add(`data: {"stage":"plan_selected","detail":"plan 1 won","data":{"index":0,"score":0.8}}`)
	sse = append(sse,
		`event: result`,
		`data: {"steps":[{"id":"s1","action":"edit_file","target":"app.py","why":"add route"}],"verify_step":"s1","rationale":"r","candidates_tested":3,"winning_score":0.8,"winning_index":0}`,
		``,
		`data: [DONE]`,
		``)
	srv := fakePlanServer(t, strings.Join(sse, "\n"))
	defer srv.Close()

	var mu sync.Mutex
	var stages []string
	ctx := &AgentContext{
		runSetup: runSetup{
			Ctx:        context.Background(),
			V3URL:      srv.URL,
			WorkingDir: t.TempDir(),
		},
	}
	ctx.StreamFn = func(kind string, data interface{}) {
		if kind != "v3_plan" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if m, ok := data.(map[string]interface{}); ok {
			stages = append(stages, fmt.Sprint(m["stage"]))
		}
	}

	plan := generatePlan(ctx, "add a hello endpoint")
	if plan == nil {
		t.Fatal("generatePlan returned nil on a well-formed plan stream")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, s := range stages {
		if s == "token" || s == "llm_start" || s == "llm_end" {
			t.Errorf("token-level stage %q reached the TUI stream", s)
		}
	}
	// The structural stages are the whole point of streaming at all.
	for _, s := range []string{"plan_start", "plan_candidate_scored", "plan_selected"} {
		if !slices.Contains(stages, s) {
			t.Errorf("structural stage %q never reached the stream (got %v)", s, stages)
		}
	}
}

// No v3-service configured means no planner. Returning nil here is what
// makes plan mode degrade to a plain agent loop instead of erroring.
func TestGeneratePlanWithoutV3URLIsNil(t *testing.T) {
	if p := generatePlan(&AgentContext{runSetup: runSetup{Ctx: context.Background()}}, "do a thing"); p != nil {
		t.Errorf("expected nil plan with no V3URL, got %+v", p)
	}
}

// V3 must generate against the human's request, never a system note. ATLAS
// sends correctives and manifests as user-role messages for chat-template
// compatibility, so "the last user turn" can be a note instead of the task.
func TestLatestUserMessagePrefersHumanTask(t *testing.T) {
	ctx := &AgentContext{
		runSetup: runSetup{
			HumanTask: "write a debounce filter over readings.txt",
		},
		modelExchange: modelExchange{
			Messages: []wire.AgentMessage{
				{Role: "user", Content: "write a debounce filter over readings.txt"},
				{Role: "assistant", Content: `{"type":"tool_call"}`},
				{Role: "user", Content: "[system note]: run the program standalone"},
				{Role: "user", Content: "[system note]: session file manifest: solve.py"},
			},
		},
	}
	if got := latestUserMessage(ctx); got != ctx.HumanTask {
		t.Fatalf("V3 task resolved to %q, want the human request", got)
	}
}

func TestLatestUserMessageFallbackSkipsSyntheticNotes(t *testing.T) {
	ctx := &AgentContext{ // no HumanTask: context built outside the loop
		modelExchange: modelExchange{
			Messages: []wire.AgentMessage{
				{Role: "user", Content: "the real task"},
				{Role: "user", Content: "[system note]: lessons from previous sessions"},
			},
		},
	}
	if got := latestUserMessage(ctx); got != "the real task" {
		t.Fatalf("fallback resolved to %q, want the real task", got)
	}
}

// `passed` authorizes nothing: it puts a compile check, a partial oracle score
// and a complete one into one boolean. Without an evidence envelope no
// response is authorized, and a response with no code leaves the caller's
// bytes.
func TestAuthorizedV3ReplacementIsTheOnlyGate(t *testing.T) {
	baseline := "def solve():\n    return 41\n"
	alternative := "def solve():\n    return 42  # best_record, not verified\n"

	for _, tc := range []struct {
		name         string
		result       *wire.V3GenerateResponse
		wantProposal string
	}{
		{"passed, no evidence", &wire.V3GenerateResponse{Passed: true, Code: alternative}, alternative},
		{"not passed, no evidence", &wire.V3GenerateResponse{Passed: false, Code: alternative}, alternative},
		{"passed, no code", &wire.V3GenerateResponse{Passed: true, Code: ""}, baseline},
		{"not passed, no code", &wire.V3GenerateResponse{Passed: false, Code: ""}, baseline},
		{"no result", nil, baseline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if authorized, _ := v3DeliveryAuthorized(tc.result, codeOf(tc.result)); authorized {
				t.Error("v3DeliveryAuthorized = true without an evidence envelope")
			}
			if got, _ := proposedV3Candidate(tc.result, baseline); got != tc.wantProposal {
				t.Errorf("proposedV3Candidate = %q, want %q", got, tc.wantProposal)
			}
		})
	}
}

// Provenance must describe the final bytes, not the first response.
func TestBaselineRestoringGatesWithdrawV3Provenance(t *testing.T) {
	// A candidate that PASSED but is refused downstream: HTML replaced by
	// JavaScript, which the language-swap gate rejects.
	htmlBaseline := "<!DOCTYPE html>\n<html><body><canvas id=\"c\"></canvas></body></html>\n"
	jsCandidate := "const c = document.getElementById('c');\nc.getContext('2d');\n"

	if v3SwappedTheLanguage("index.html", htmlBaseline, jsCandidate) == "" {
		t.Fatal("fixture invalid: the gate must reject JS replacing HTML")
	}

	code, authorized, fellBack := revokeV3(htmlBaseline, "language swap", "index.html")
	if code != htmlBaseline {
		t.Fatalf("baseline not restored: %q", code)
	}
	if authorized {
		t.Fatal("a gate that restores the baseline must withdraw authorization")
	}
	if !fellBack {
		t.Fatal("fallback must be recorded so no V3 metadata attaches")
	}
}

// Bytes and provenance must not be assignable independently: every branch
// that restores the caller's content has to go through the transition.
func TestNoGateRestoresBaselineWithoutRevoking(t *testing.T) {
	src, err := os.ReadFile("tool_write_v3.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	route := body[strings.Index(body, "func writeFileWithV3("):]
	route = route[:strings.Index(route, "\n// originalForScope")]
	for _, banned := range []string{"code = baselineContent", "code = write.baselineContent",
		"code = w.baselineContent"} {
		if strings.Contains(route, banned) {
			t.Fatal("a gate assigns baseline bytes directly; use revokeV3 so provenance follows")
		}
	}
	// The two that withdraw a proposal before it is observed, and the one
	// transition the four gates after the decision share.
	if n := strings.Count(route, "revokeV3("); n != 3 {
		t.Fatalf("revokeV3 is called %d times on the route, want 3", n)
	}
	if n := strings.Count(route, "write.restoreBaseline("); n != 4 {
		t.Fatalf("%d gates restore the baseline, want 4", n)
	}
}

// Both delivery paths must route through the one helper — a duplicated
// safety condition is how half of it goes stale, which is exactly what
// happened when write_file was fixed and improveContentWithV3 was not.
func TestBothDeliveryPathsUseTheSharedAuthorization(t *testing.T) {
	body := toolSources(t)
	if strings.Count(body, "proposedV3Candidate(") < 3 {
		t.Fatal("expected the proposal boundary plus both call sites")
	}
	for _, unsafe := range []string{"chosen := v3Result.Code", "code := v3Result.Code"} {
		if strings.Contains(body, unsafe) {
			t.Fatalf("delivery path still takes Code without authorization: %q", unsafe)
		}
	}
}

// ---------------------------------------------------------------------------
// The edit path obeys the same authorization contract as write_file
// ---------------------------------------------------------------------------
//
// Four production tools reach V3 through improveContentWithV3, three of them
// via runEditPipeline. Calling the shared helper is not enough on its own: the
// bytes that come back travel through sanitisation and two drift gates before
// anything is written, and provenance has to travel with them or fall away.

const editBaseline = "import math\n\n\ndef area(r):\n    if r < 0:\n        raise ValueError('neg')\n    return math.pi * r * r\n\n\ndef edge(r):\n    for _ in range(1):\n        pass\n    return 2 * math.pi * r\n"

// editV3Server answers /v3/generate with `candidate`, and attaches whatever
// envelope the case asks for. envelopeFor stamps the golden verified-winner
// shape onto given bytes; a nil builder sends no envelope at all.
func editV3Server(t *testing.T, candidate string, passed bool,
	envelope map[string]interface{}) *httptest.Server {
	return editV3ServerWithEdit(t, candidate, passed, envelope, "")
}

// editV3ServerWithEdit additionally answers /internal/structural_edit with
// `edited`, which is how structural_edit composes the content the pipeline
// then judges.
func editV3ServerWithEdit(t *testing.T, candidate string, passed bool,
	envelope map[string]interface{}, edited string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/generate":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			body := map[string]interface{}{
				"code": candidate, "passed": passed, "phase_solved": "phase1",
				"candidates_tested": 3, "winning_score": 0.9,
				"verification_evidence": []map[string]interface{}{
					{"verifier": "sandbox", "status": "passed"}},
			}
			if envelope != nil {
				body["evidence"] = envelope
			}
			payload, _ := json.Marshal(body)
			for _, line := range []string{"event: result", "data: " + string(payload), "", "data: [DONE]", ""} {
				fmt.Fprint(w, line+"\n")
				if fl != nil {
					fl.Flush()
				}
			}
		case "/internal/structural_edit":
			out, _ := json.Marshal(map[string]interface{}{
				"success": true, "language": "python", "new_content": edited})
			_, _ = w.Write(out)
		case "/internal/structural_check":
			_, _ = w.Write([]byte(`{"ok":true,"unresolved":[]}`))
		case "/internal/cyclomatic_complexity":
			// Complex enough that the edit warrants the pipeline; without it
			// runEditPipeline returns the caller's edit untouched and the
			// authorization boundary is never reached.
			_, _ = w.Write([]byte(`{"ok":true,"cyclomatic_complexity":12}`))
		default:
			if strings.HasSuffix(r.URL.Path, "/syntax-check") {
				_, _ = w.Write([]byte(`{"valid":true}`))
				return
			}
			http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
		}
	}))
}

// improveContentWithV3 is driven with every envelope state. Materially
// different bytes are a proposal in every case: the service offers them and
// the route decides. No envelope state changes what the caller is handed.
func TestEditPathCarriesTheCandidateWhateverTheEnvelopeSays(t *testing.T) {
	candidate := "import math\n\n\ndef area(r):\n    if r < 0:\n        raise ValueError('neg')\n    return math.pi * r ** 2\n\n\ndef edge(r):\n    for _ in range(1):\n        pass\n    return math.tau * r\n"

	for _, c := range []struct {
		name   string
		passed bool
		omit   bool // send no envelope at all
		mutate func(map[string]interface{})
	}{
		{name: "verified winner with an exact hash", passed: true},
		{name: "not passed, verified winner", passed: false},
		{name: "passed, no envelope", passed: true, omit: true},
		{name: "passed, unknown wire version", passed: true, mutate: func(e map[string]interface{}) {
			e["wire_version"] = "99.0.0"
		}},
		{name: "passed, malformed identity", passed: true, mutate: func(e map[string]interface{}) {
			e["identity"].(map[string]interface{})["evaluation_context_hash"] = ""
		}},
		{name: "passed, best not closure eligible", passed: true, mutate: func(e map[string]interface{}) {
			e["evaluation"].(map[string]interface{})["closure_eligible"] = false
			e["selection"].(map[string]interface{})["status"] = "best_not_closure_eligible"
		}},
		{name: "passed, tied", passed: true, mutate: func(e map[string]interface{}) {
			e["selection"].(map[string]interface{})["status"] = "tied"
		}},
		{name: "passed, incomparable", passed: true, mutate: func(e map[string]interface{}) {
			e["selection"].(map[string]interface{})["status"] = "incomparable"
		}},
		{name: "passed, ineligible", passed: true, mutate: func(e map[string]interface{}) {
			e["selection"].(map[string]interface{})["status"] = "ineligible"
		}},
		{name: "passed, no verified winner", passed: true, mutate: func(e map[string]interface{}) {
			e["selection"].(map[string]interface{})["status"] = "no_verified_winner"
		}},
		{name: "passed, closure ineligible", passed: true, mutate: func(e map[string]interface{}) {
			e["evaluation"].(map[string]interface{})["closure_eligible"] = false
		}},
		{name: "passed, hash mismatch", passed: true, mutate: func(e map[string]interface{}) {
			e["identity"].(map[string]interface{})["candidate_content_hash"] =
				"1111111111111111111111111111111111111111111111111111111111111111"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "app.py")
			var env map[string]interface{}
			if !c.omit {
				env = envelopeFor(t, candidate, c.mutate)
			}
			srv := editV3Server(t, candidate, c.passed, env)
			defer srv.Close()
			sb := fakeSyntaxSandbox(t, "")
			defer sb.Close()
			ctx := writeGateCtx(t, srv.URL, sb.URL, dir)

			out, meta, err := improveContentWithV3(path, editBaseline, ctx)
			if err != nil {
				t.Fatalf("improveContentWithV3: %v", err)
			}
			if out != candidate {
				t.Fatalf("the proposal was not carried: %q", out)
			}
			if !meta.Used {
				t.Error("a proposal lost the pipeline that produced it")
			}
		})
	}
}

// Sanitisation rewrites the candidate AFTER the service earned its evidence,
// exactly as on the write path. Evidence for the fenced bytes does not
// describe the unwrapped ones, so the caller's own edit stands and no
// provenance is attached.
func TestEditPathRevokesWhenSanitisationChangesTheBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.py")
	inner := "import math\n\n\ndef area(r):\n    if r < 0:\n        raise ValueError('neg')\n    return math.pi * r ** 2\n\n\ndef edge(r):\n    for _ in range(1):\n        pass\n    return math.tau * r\n"
	fenced := "```python\n" + inner + "```\n"

	// The service verified what it returned: the FENCED bytes.
	srv := editV3Server(t, fenced, true, envelopeFor(t, fenced, nil))
	defer srv.Close()
	sb := fakeSyntaxSandbox(t, "")
	defer sb.Close()
	ctx := writeGateCtx(t, srv.URL, sb.URL, dir)

	out, meta, err := improveContentWithV3(path, editBaseline, ctx)
	if err != nil {
		t.Fatalf("improveContentWithV3: %v", err)
	}
	// The unwrapped bytes are carried as a proposal; what the service verified
	// was the fenced text, so it certifies nothing about them.
	if out != inner {
		t.Fatalf("the sanitised proposal was not carried: %q", out)
	}
	if meta.Envelope == nil {
		t.Error("the advisory record did not travel with the proposal")
	}
	// The same bytes, returned unwrapped and verified as such, ARE delivered:
	// this is authorization against what will be written, not a blanket
	// refusal of anything that was ever fenced. A fenced candidate stays
	// undeliverable until the service hashes the form it hands over -- the
	// same consequence the write path already carries.
	srv2 := editV3Server(t, inner, true, envelopeFor(t, inner, nil))
	defer srv2.Close()
	ctx2 := writeGateCtx(t, srv2.URL, sb.URL, dir)
	out2, meta2, err := improveContentWithV3(path, editBaseline, ctx2)
	if err != nil {
		t.Fatalf("improveContentWithV3: %v", err)
	}
	if out2 != inner || !meta2.Used {
		t.Errorf("evidence for the delivered form must authorize it, got %q used=%v",
			out2, meta2.Used)
	}
}

// Every production edit tool, driven through its real handler. The request
// declares no outputs, so no envelope can license a replacement: the caller's
// own edit stays on disk whatever V3 offers, and an unauthorized candidate
// leaves no V3 provenance on the result.
func TestEveryEditToolObeysTheAuthorizationBoundary(t *testing.T) {
	// The file every tool edits.
	const original = "import math\n\n\ndef area(r):\n    if r < 0:\n        raise ValueError('neg')\n    return math.pi * r * r\n\n\ndef edge(r):\n    for _ in range(1):\n        pass\n    return 2 * math.pi * r\n"

	type editTool struct {
		name string
		args map[string]interface{}
		// the bytes the tool itself produces, before V3 is asked
		edited string
		// what V3 offers instead: a variant of the same span, so the drift
		// gate has nothing to object to and authorization is what decides
		candidate string
	}
	editFile := editTool{name: "edit_file",
		edited:    strings.Replace(original, "raise ValueError('neg')", "raise ValueError('negative')", 1),
		candidate: strings.Replace(original, "raise ValueError('neg')", "raise ValueError('negative radius')", 1),
		args: map[string]interface{}{"path": "app.py",
			"old_str": "raise ValueError('neg')",
			"new_str": "raise ValueError('negative')"}}
	insertAfter := editTool{name: "insert_after",
		edited:    strings.Replace(original, "import math\n", "import math\nimport sys\n", 1),
		candidate: strings.Replace(original, "import math\n", "import math\nimport sys  # v3\n", 1),
		args: map[string]interface{}{"path": "app.py", "line": 1,
			"content": "import sys"}}
	replaceLines := editTool{name: "replace_lines",
		edited:    strings.Replace(original, "    return math.pi * r * r", "    return math.pi * r ** 2", 1),
		candidate: strings.Replace(original, "    return math.pi * r * r", "    return math.pi * pow(r, 2)", 1),
		args: map[string]interface{}{"path": "app.py",
			"start_line": 7, "end_line": 7,
			"expected_first_line": "    return math.pi * r * r",
			"expected_last_line":  "    return math.pi * r * r",
			"content":             "    return math.pi * r ** 2"}}
	structuralEdit := editTool{name: "structural_edit",
		edited: strings.Replace(original,
			"def edge(r):\n    for _ in range(1):\n        pass\n    return 2 * math.pi * r\n",
			"def edge(r):\n    for _ in range(1):\n        pass\n    return math.tau * r\n", 1),
		candidate: strings.Replace(original,
			"def edge(r):\n    for _ in range(1):\n        pass\n    return 2 * math.pi * r\n",
			"def edge(r):\n    for _ in range(1):\n        pass\n    return 2.0 * math.pi * r\n", 1),
		args: map[string]interface{}{"path": "app.py",
			"selector": "function:edge",
			"content":  "def edge(r):\n    for _ in range(1):\n        pass\n    return math.tau * r\n"}}

	for _, tc := range []struct {
		tool       editTool
		mode       string
		authorized bool
	}{
		{editFile, "authorized candidate", true}, {editFile, "unauthorized candidate", false},
		{insertAfter, "authorized candidate", true}, {insertAfter, "unauthorized candidate", false},
		{replaceLines, "authorized candidate", true}, {replaceLines, "unauthorized candidate", false},
		{structuralEdit, "authorized candidate", true}, {structuralEdit, "unauthorized candidate", false},
	} {
		t.Run(tc.tool.name+"/"+tc.mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "app.py")
			writeInWorkspace(t, dir, "app.py", original)
			var authorizesNothing func(map[string]interface{})
			if !tc.authorized {
				// passed=true, and an envelope that authorizes nothing.
				authorizesNothing = func(e map[string]interface{}) {
					e["evaluation"].(map[string]interface{})["closure_eligible"] = false
					e["selection"].(map[string]interface{})["status"] = "best_not_closure_eligible"
				}
			}
			env := envelopeFor(t, tc.tool.candidate, authorizesNothing)
			srv := editV3ServerWithEdit(t, tc.tool.candidate, true, env, tc.tool.edited)
			defer srv.Close()

			ctx := writeGateCtx(t, srv.URL, srv.URL, dir)
			ctx.PermissionMode = PermissionYolo
			ctx.StreamFn = func(string, interface{}) {}
			ctx.SessionWrites["app.py"] = true
			ctx.RecordFileRead(path, original)

			args, _ := json.Marshal(tc.tool.args)
			res := executeToolCall(tc.tool.name, args, ctx)
			if !res.Success {
				t.Fatalf("%s failed: %s", tc.tool.name, res.Error)
			}
			onDisk, _ := os.ReadFile(path)
			if string(onDisk) != tc.tool.edited {
				t.Fatalf("V3's candidate replaced the caller's edit:\n got %q\nwant %q",
					onDisk, tc.tool.edited)
			}
			// The "V3 verified this edit" nudge reads these fields, so
			// empty fields keep it quiet.
			if !tc.authorized && (res.V3Used || res.CandidatesTested != 0 || res.WinningScore != 0 ||
				res.PhaseSolved != "" || len(res.VerificationEvidence) != 0) {
				t.Errorf("baseline edit carries V3 provenance: %+v", res)
			}
		})
	}
}

// Structural sentinel: the edit path authorizes through the one gate, and
// never from the legacy fields.
func TestEditPathAuthorizesOnlyThroughTheSharedGate(t *testing.T) {
	src, err := os.ReadFile("tool_write_v3.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	improve := body[strings.Index(body, "func improveContentWithV3("):]
	improve = improve[:strings.Index(improve, "\n// runEditPipeline")]
	pipeline := body[strings.Index(body, "func runEditPipeline("):]
	pipeline = pipeline[:strings.Index(pipeline, "\n// attachV3")]

	for name, fn := range map[string]string{
		"improveContentWithV3": improve, "runEditPipeline": pipeline,
	} {
		for _, banned := range []string{"v3Result.Passed", "result.Passed",
			".PhaseSolved != \"\"", "v3Result.WinningScore >", "VerificationEvidence) >"} {
			if strings.Contains(fn, banned) {
				t.Errorf("%s authorizes from a legacy field: %s", name, banned)
			}
		}
	}
	// One positive gate, and the post-sanitisation recheck that keeps it
	// honest. Nothing else may hand back a candidate.
	if strings.Count(improve, "proposedV3Candidate(") != 1 {
		t.Errorf("improveContentWithV3 must take the proposal exactly once, found %d",
			strings.Count(improve, "proposedV3Candidate("))
	}
	// It authorizes nothing at all: the route that receives the proposal
	// stages it and applies the policy.
	if strings.Contains(improve, "v3DeliveryAuthorized(") {
		t.Error("improveContentWithV3 authorizes instead of proposing")
	}
	// And the callers never build provenance of their own.
	if strings.Contains(pipeline, "V3EditMetadata{\n\t\tUsed: true") ||
		strings.Contains(pipeline, "Used:                 true") {
		t.Error("runEditPipeline manufactures provenance instead of carrying it")
	}
}

// codeOf is the bytes a response offered, or "" for none.
func codeOf(result *wire.V3GenerateResponse) string {
	if result == nil {
		return ""
	}
	return result.Code
}
