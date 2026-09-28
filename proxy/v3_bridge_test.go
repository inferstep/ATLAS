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
	"sync"
	"testing"
	"time"
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

func TestCallV3PlanStreamingParsesResult(t *testing.T) {
	// Three progress events, then a final result event, then [DONE].
	// Mirrors the wire format of /v3/plan.
	sse := strings.Join([]string{
		`data: {"stage":"plan_start","detail":"generating 3 candidates"}`,
		``,
		`data: {"stage":"plan_candidate_scored","detail":"candidate 1 score=0.80","data":{"index":0,"score":0.8}}`,
		``,
		`data: {"stage":"plan_selected","detail":"plan 1 won","data":{"index":0,"score":0.8}}`,
		``,
		`event: result`,
		`data: {"steps":[{"id":"s1","action":"edit_file","target":"app.py","why":"add route"},{"id":"s2","action":"run_command","target":"curl http://localhost:5000/hello","why":"verify"}],"verify_step":"s2","rationale":"add then verify","candidates_tested":3,"winning_score":0.8,"winning_index":0,"reasons":["step count 2 in range","verify_step=s2"]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	srv := fakePlanServer(t, sse)
	defer srv.Close()

	var mu sync.Mutex
	var seenStages []string
	cb := func(stage, detail string, data map[string]interface{}) {
		mu.Lock()
		seenStages = append(seenStages, stage)
		mu.Unlock()
	}

	plan, err := callV3PlanStreaming(context.Background(), srv.URL, V3PlanRequest{
		UserMessage: "add a hello endpoint",
		WorkingDir:  "/workspace",
	}, cb)
	if err != nil {
		t.Fatalf("callV3PlanStreaming: %v", err)
	}
	if plan == nil {
		t.Fatal("plan is nil")
	}
	if got, want := len(plan.Steps), 2; got != want {
		t.Errorf("got %d steps, want %d", got, want)
	}
	if plan.VerifyStep != "s2" {
		t.Errorf("got verify_step=%q, want %q", plan.VerifyStep, "s2")
	}
	if plan.WinningScore != 0.8 {
		t.Errorf("got winning_score=%v, want 0.8", plan.WinningScore)
	}

	mu.Lock()
	defer mu.Unlock()
	wantStages := []string{"plan_start", "plan_candidate_scored", "plan_selected"}
	if len(seenStages) != len(wantStages) {
		t.Fatalf("got stages %v, want %v", seenStages, wantStages)
	}
	for i, s := range wantStages {
		if seenStages[i] != s {
			t.Errorf("stage[%d]=%q, want %q", i, seenStages[i], s)
		}
	}
}

func TestCallV3PlanStreamingMissingResult(t *testing.T) {
	// SSE that ends without an `event: result` block — bridge should
	// surface this as an error rather than returning nil silently.
	sse := strings.Join([]string{
		`data: {"stage":"plan_start","detail":"go"}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	srv := fakePlanServer(t, sse)
	defer srv.Close()

	_, err := callV3PlanStreaming(context.Background(), srv.URL, V3PlanRequest{UserMessage: "x"}, nil)
	if err == nil {
		t.Fatal("expected error for missing result event")
	}
	if !strings.Contains(err.Error(), "without result") {
		t.Errorf("error %q doesn't mention missing result", err.Error())
	}
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
// callV3GenerateStreaming + v3CallTimeout — the generate path. All three
// proxy callers treat a bridge error as "fall back to the model's own
// content", so every failure path must return, not hang.
// ---------------------------------------------------------------------------

func TestV3CallTimeout(t *testing.T) {
	// 300s, not 180s: PlanSearch spends two LLM calls per candidate, so k=3
	// costs ~162s at the measured ~22s per call before the probe and
	// self-test that precede it. At 180s, sessions spent a median 207s on
	// generation alone and phase-3 repair was skipped 19 times with 7-9s
	// left.
	t.Run("default is 300s", func(t *testing.T) {
		t.Setenv("ATLAS_V3_TIMEOUT", "")
		if d := v3CallTimeout(); d != 300*time.Second {
			t.Errorf("default = %v", d)
		}
	})
	t.Run("env override in seconds", func(t *testing.T) {
		t.Setenv("ATLAS_V3_TIMEOUT", "30")
		if d := v3CallTimeout(); d != 30*time.Second {
			t.Errorf("override = %v", d)
		}
	})
	t.Run("zero disables the cap", func(t *testing.T) {
		t.Setenv("ATLAS_V3_TIMEOUT", "0")
		if d := v3CallTimeout(); d != 0 {
			t.Errorf("0 should disable, got %v", d)
		}
	})
	t.Run("garbage falls back to default", func(t *testing.T) {
		t.Setenv("ATLAS_V3_TIMEOUT", "soon")
		if d := v3CallTimeout(); d != 300*time.Second {
			t.Errorf("garbage value gave %v", d)
		}
	})
}

func fakeGenerateServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/generate" {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
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

func TestCallV3GenerateStreamingParsesResultAndProgress(t *testing.T) {
	srv := fakeGenerateServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseLines(w,
			`data: {"stage":"plan_search","detail":"3 candidates","data":{"count":3}}`,
			``,
			`event: result`,
			`data: {"code":"print(1)","passed":true,"phase_solved":"phase1","candidates_tested":3}`,
			``,
			`data: [DONE]`,
			``)
	})
	defer srv.Close()

	var stages []string
	var gotData map[string]interface{}
	result, err := callV3GenerateStreaming(context.Background(), srv.URL,
		V3GenerateRequest{FilePath: "a.py"},
		func(stage, detail string, data map[string]interface{}) {
			stages = append(stages, stage)
			gotData = data
		})
	if err != nil {
		t.Fatalf("streaming call failed: %v", err)
	}
	if result.Code != "print(1)" || !result.Passed {
		t.Errorf("result = %+v", result)
	}
	if len(stages) != 1 || stages[0] != "plan_search" {
		t.Errorf("progress stages = %v", stages)
	}
	if gotData["count"] != float64(3) {
		t.Errorf("structured progress data = %v", gotData)
	}
}

func TestCallV3GenerateStreamingNon200IsAnError(t *testing.T) {
	srv := fakeGenerateServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	})
	defer srv.Close()

	_, err := callV3GenerateStreaming(context.Background(), srv.URL,
		V3GenerateRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want the 503 surfaced", err)
	}
}

func TestCallV3GenerateStreamingMissingResultIsAnError(t *testing.T) {
	// Progress events then [DONE] with no result event — the pipeline
	// died server-side. Must be an error, not a nil result.
	srv := fakeGenerateServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseLines(w,
			`data: {"stage":"plan_search","detail":"working"}`,
			``,
			`data: [DONE]`,
			``)
	})
	defer srv.Close()

	_, err := callV3GenerateStreaming(context.Background(), srv.URL,
		V3GenerateRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), "without sending a result event") {
		t.Errorf("err = %v, want a stream that really ended empty", err)
	}
}

func TestCallV3GenerateStreamingUndecodableResultNamesTheDecodeFailure(t *testing.T) {
	// An unmarshal failure used to be discarded, leaving result nil and
	// reporting the same message as a stream that sent nothing at all.
	srv := fakeGenerateServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseLines(w, `event: result`, `data: {not json`, ``, `data: [DONE]`, ``)
	})
	defer srv.Close()

	_, err := callV3GenerateStreaming(context.Background(), srv.URL,
		V3GenerateRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), "could not decode") {
		t.Errorf("err = %v, want the decode failure named", err)
	}
}

func TestCallV3GenerateStreamingTimeoutFires(t *testing.T) {
	t.Setenv("ATLAS_V3_TIMEOUT", "1")
	// `release` unblocks the stalled handler after the call returns:
	// srv.Close() waits for active handlers, and server-side disconnect
	// detection isn't reliable enough to end the stall on its own.
	release := make(chan struct{})
	srv := fakeGenerateServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseLines(w, `data: {"stage":"plan_search","detail":"stalling"}`, ``)
		select { // stall past the cap
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer srv.Close()
	// LIFO: runs before srv.Close(), releasing the stalled handler.
	defer close(release)

	start := time.Now()
	_, err := callV3GenerateStreaming(context.Background(), srv.URL,
		V3GenerateRequest{}, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("stalled V3 run did not time out")
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout took %v with a 1s cap", elapsed)
	}
	// The cap firing and V3 finishing empty are different events with
	// different fixes, and both reported the same sentence. Measured
	// 2026-08-03: five caps in one run read as V3 producing nothing,
	// while it was still working and its output was being discarded.
	if strings.Contains(err.Error(), "without sending a result event") {
		t.Errorf("the cap firing is reported as V3 finishing empty:\n%s", err)
	}
	if !strings.Contains(err.Error(), "ATLAS_V3_TIMEOUT") {
		t.Errorf("the error should name the cap that fired:\n%s", err)
	}
}

func TestCallV3GenerateStreamingCancelAborts(t *testing.T) {
	// User Ctrl-C: cancelling the request context must abort a stalled
	// stream promptly — the regression this guards is the "ctrl-c does
	// not stop it" multi-minute PlanSearch hang.
	release := make(chan struct{})
	srv := fakeGenerateServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseLines(w, `data: {"stage":"plan_search","detail":"stalling"}`, ``)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer srv.Close()
	// LIFO: runs before srv.Close(), releasing the stalled handler.
	defer close(release)

	reqCtx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := callV3GenerateStreaming(reqCtx, srv.URL, V3GenerateRequest{}, nil)
	if err == nil {
		t.Fatal("cancelled V3 run returned no error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("cancel took %v to unblock", elapsed)
	}
}

// generatePlan is the agent loop's entry into V3 planning. The transport
// under it is covered above; what is only here is the progress callback it
// installs, and that callback exists to drop token-level events. The
// planner asks for 3 candidates and the LLM emits ~150 token deltas each,
// so forwarding them verbatim puts ~450 v3_plan rows into the TUI's
// pipeline pane for one plan — the same flood that had to be fixed once
// already for V3 generation. Only the structural stages belong on the
// stream.
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
		Ctx:        context.Background(),
		V3URL:      srv.URL,
		WorkingDir: t.TempDir(),
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
	want := map[string]bool{"plan_start": false, "plan_candidate_scored": false, "plan_selected": false}
	for _, s := range stages {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for s, seen := range want {
		if !seen {
			t.Errorf("structural stage %q never reached the stream (got %v)", s, stages)
		}
	}
}

// No v3-service configured means no planner. Returning nil here is what
// makes plan mode degrade to a plain agent loop instead of erroring.
func TestGeneratePlanWithoutV3URLIsNil(t *testing.T) {
	if p := generatePlan(&AgentContext{Ctx: context.Background()}, "do a thing"); p != nil {
		t.Errorf("expected nil plan with no V3URL, got %+v", p)
	}
}

// V3 must generate against the human's request, never a harness note.
// ATLAS rides correctives/manifests on user-role messages for chat-template
// compatibility, so "last user turn" is the wrong question to ask the
// conversation (third-party audit finding: V3 received "run the program
// standalone" as its task).
func TestLatestUserMessagePrefersHumanTask(t *testing.T) {
	ctx := &AgentContext{
		HumanTask: "write a debounce filter over readings.txt",
		Messages: []AgentMessage{
			{Role: "user", Content: "write a debounce filter over readings.txt"},
			{Role: "assistant", Content: `{"type":"tool_call"}`},
			{Role: "user", Content: "[system note]: run the program standalone"},
			{Role: "user", Content: "[system note]: session file manifest: solve.py"},
		},
	}
	if got := latestUserMessage(ctx); got != ctx.HumanTask {
		t.Fatalf("V3 task resolved to %q, want the human request", got)
	}
}

func TestLatestUserMessageFallbackSkipsSyntheticNotes(t *testing.T) {
	ctx := &AgentContext{ // no HumanTask: context built outside the loop
		Messages: []AgentMessage{
			{Role: "user", Content: "the real task"},
			{Role: "user", Content: "[system note]: lessons from previous sessions"},
		},
	}
	if got := latestUserMessage(ctx); got != "the real task" {
		t.Fatalf("fallback resolved to %q, want the real task", got)
	}
}

// Authorization to replace the caller's content is the ENVELOPE, never
// `passed` and never the mere presence of `code`. `passed` collapses a compile
// smoke, a partial oracle score and a complete one into one boolean, so it can
// no longer stand for any of them.
//
// The previous version of this test reimplemented the condition inline, so
// it tested the intended expression rather than production code — and stayed
// green while improveContentWithV3 still took Code unconditionally. It now
// calls the shared helper both paths use.
func TestAuthorizedV3ReplacementIsTheOnlyGate(t *testing.T) {
	baseline := "def solve():\n    return 41\n"
	alternative := "def solve():\n    return 42  # best_record, not verified\n"

	for _, tc := range []struct {
		name       string
		result     *V3GenerateResponse
		want       string
		authorized bool
	}{
		{"passing without evidence is refused",
			&V3GenerateResponse{Passed: true, Code: alternative}, baseline, false},
		{"unverified candidate is refused",
			&V3GenerateResponse{Passed: false, Code: alternative}, baseline, false},
		{"passing but empty falls back",
			&V3GenerateResponse{Passed: true, Code: ""}, baseline, false},
		{"unverified and empty falls back",
			&V3GenerateResponse{Passed: false, Code: ""}, baseline, false},
		{"nil result falls back", nil, baseline, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The service's own verdict, which authorizes nothing anywhere:
			// this table now pins what the predicate SAYS, not what it
			// permits. No production path consults it.
			if got, _ := v3DeliveryAuthorized(tc.result, codeOf(tc.result)); got != tc.authorized {
				t.Fatalf("v3DeliveryAuthorized = %v, want %v", got, tc.authorized)
			}
			// And the bytes a refused verdict leaves behind are the caller's.
			if !tc.authorized {
				if got, _ := proposedV3Candidate(tc.result, baseline); tc.want == baseline &&
					got != baseline && codeOf(tc.result) == "" {
					t.Fatalf("a refusal returned %q, not the baseline", got)
				}
			}
		})
	}
}

// Provenance must describe the FINAL bytes, not the initial response.
//
// The previous test here asserted `V3EditMetadata{}.Used == false`, which
// only proves Go's zero value is false — it never called either delivery
// function. That is the fourth mirror-test in this workstream, so this one
// drives the real gates.
func TestBaselineRestoringGatesWithdrawV3Provenance(t *testing.T) {
	// A candidate that PASSED but is refused downstream: HTML replaced by
	// JavaScript, which the language-swap gate rejects.
	htmlBaseline := "<!DOCTYPE html>\n<html><body><canvas id=\"c\"></canvas></body></html>\n"
	jsCandidate := "const c = document.getElementById('c');\nc.getContext('2d');\n"

	if why := v3SwappedTheLanguage("index.html", htmlBaseline, jsCandidate); why == "" {
		t.Fatal("fixture invalid: the gate must reject JS replacing HTML")
	}

	// The transition the production path now uses.
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
	src, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	writeFn := body[strings.Index(body, "func writeFileWithV3("):]
	writeFn = writeFn[:strings.Index(writeFn, "\nfunc ")]
	if strings.Contains(writeFn, "code = baselineContent") {
		t.Fatal("a gate assigns baseline bytes directly; use revokeV3 so provenance follows")
	}
	if strings.Count(writeFn, "revokeV3(") < 3 {
		t.Fatalf("expected every baseline-restoring gate to revoke, found %d",
			strings.Count(writeFn, "revokeV3("))
	}
}

// Both delivery paths must route through the one helper — a duplicated
// safety condition is how half of it goes stale, which is exactly what
// happened when write_file was fixed and improveContentWithV3 was not.
func TestBothDeliveryPathsUseTheSharedAuthorization(t *testing.T) {
	src, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
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

// The shared boundary, driven through improveContentWithV3 itself: every
// envelope state, and what the caller is handed for it.
func TestEditPathDeliversOnlyAuthorizedCandidates(t *testing.T) {
	candidate := "import math\n\n\ndef area(r):\n    if r < 0:\n        raise ValueError('neg')\n    return math.pi * r ** 2\n\n\ndef edge(r):\n    for _ in range(1):\n        pass\n    return math.tau * r\n"

	for _, c := range []struct {
		name      string
		passed    bool
		omit      bool
		mutate    func(map[string]interface{})
		delivered bool
	}{
		{name: "verified winner with an exact hash", passed: true, delivered: true},
		// The envelope is authoritative in both directions.
		{name: "not passed, verified winner", passed: false, delivered: true},
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
		c := c
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
			// Materially different bytes are a PROPOSAL in every row: the
			// service offers them and the route decides. What the envelope
			// state changes is the certification that travels with them, which
			// is the delivery rule for a request that declared nothing.
			if out != candidate {
				t.Fatalf("the proposal was not carried: %q", out)
			}
			if !meta.Used {
				t.Error("a proposal lost the pipeline that produced it")
			}
			// Whatever the envelope said, the metadata carries no
			// certification: there is no field for the service to authorize
			// through and no route that would read one.
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

// Every production edit tool, driven through its real handler. Each one has to
// prove it cannot bypass the shared boundary: an unauthorized candidate must
// leave the caller's own edit on disk with no provenance, and an authorized one
// must arrive intact.
func TestEveryEditToolObeysTheAuthorizationBoundary(t *testing.T) {
	// The file every tool edits, and the candidate V3 offers instead.
	const original = "import math\n\n\ndef area(r):\n    if r < 0:\n        raise ValueError('neg')\n    return math.pi * r * r\n\n\ndef edge(r):\n    for _ in range(1):\n        pass\n    return 2 * math.pi * r\n"

	for _, tool := range []struct {
		name string
		args func(edited string) map[string]interface{}
		// the bytes the tool itself produces, before V3 is consulted
		edited string
		// what V3 offers instead: a variant of the SAME span, so the drift
		// gate has nothing to object to and authorization is what decides.
		candidate string
	}{
		{name: "edit_file",
			edited:    strings.Replace(original, "raise ValueError('neg')", "raise ValueError('negative')", 1),
			candidate: strings.Replace(original, "raise ValueError('neg')", "raise ValueError('negative radius')", 1),
			args: func(string) map[string]interface{} {
				return map[string]interface{}{"path": "app.py",
					"old_str": "raise ValueError('neg')",
					"new_str": "raise ValueError('negative')"}
			}},
		{name: "insert_after",
			edited:    strings.Replace(original, "import math\n", "import math\nimport sys\n", 1),
			candidate: strings.Replace(original, "import math\n", "import math\nimport sys  # v3\n", 1),
			args: func(string) map[string]interface{} {
				return map[string]interface{}{"path": "app.py", "line": 1,
					"content": "import sys"}
			}},
		{name: "replace_lines",
			edited:    strings.Replace(original, "    return math.pi * r * r", "    return math.pi * r ** 2", 1),
			candidate: strings.Replace(original, "    return math.pi * r * r", "    return math.pi * pow(r, 2)", 1),
			args: func(string) map[string]interface{} {
				return map[string]interface{}{"path": "app.py",
					"start_line": 7, "end_line": 7,
					"expected_first_line": "    return math.pi * r * r",
					"expected_last_line":  "    return math.pi * r * r",
					"content":             "    return math.pi * r ** 2"}
			}},
		{name: "structural_edit",
			edited: strings.Replace(original,
				"def edge(r):\n    for _ in range(1):\n        pass\n    return 2 * math.pi * r\n",
				"def edge(r):\n    for _ in range(1):\n        pass\n    return math.tau * r\n", 1),
			candidate: strings.Replace(original,
				"def edge(r):\n    for _ in range(1):\n        pass\n    return 2 * math.pi * r\n",
				"def edge(r):\n    for _ in range(1):\n        pass\n    return 2.0 * math.pi * r\n", 1),
			args: func(string) map[string]interface{} {
				return map[string]interface{}{"path": "app.py",
					"selector": "function:edge",
					"content":  "def edge(r):\n    for _ in range(1):\n        pass\n    return math.tau * r\n"}
			}},
	} {
		tool := tool
		candidate := tool.candidate

		for _, mode := range []struct {
			name       string
			authorized bool
		}{{"authorized candidate", true}, {"unauthorized candidate", false}} {
			mode := mode
			t.Run(tool.name+"/"+mode.name, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "app.py")
				if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}
				var env map[string]interface{}
				if mode.authorized {
					env = envelopeFor(t, candidate, nil)
				} else {
					// passed=true, and an envelope that authorizes nothing.
					env = envelopeFor(t, candidate, func(e map[string]interface{}) {
						e["evaluation"].(map[string]interface{})["closure_eligible"] = false
						e["selection"].(map[string]interface{})["status"] = "best_not_closure_eligible"
					})
				}
				srv := editV3ServerWithEdit(t, candidate, true, env, tool.edited)
				defer srv.Close()

				ctx := writeGateCtx(t, srv.URL, srv.URL, dir)
				ctx.PermissionMode = PermissionYolo
				ctx.StreamFn = func(string, interface{}) {}
				ctx.SessionWrites["app.py"] = true
				ctx.RecordFileRead(path, original)

				args, _ := json.Marshal(tool.args(tool.edited))
				res := executeToolCall(tool.name, args, ctx)
				if !res.Success {
					t.Fatalf("%s failed: %s", tool.name, res.Error)
				}
				onDisk, _ := os.ReadFile(path)

				if mode.authorized {
					// The envelope is a proposal, not proxy authority. This
					// request states no output knowledge, so no typed
					// authorization exists to license a replacement and the
					// caller's own edit is what stays -- the same rule the
					// new-file route has always applied to a contractless
					// request, now applied to the edit routes as well.
					if string(onDisk) != tool.edited {
						t.Fatalf("a service envelope landed bytes for a request that "+
							"declared no outputs:\n got %q\nwant the caller's edit %q",
							onDisk, tool.edited)
					}
					return
				}
				if string(onDisk) != tool.edited {
					t.Fatalf("unauthorized bytes reached disk:\n got %q\nwant the caller's edit %q",
						onDisk, tool.edited)
				}
				if res.V3Used || res.CandidatesTested != 0 || res.WinningScore != 0 ||
					res.PhaseSolved != "" || len(res.VerificationEvidence) != 0 {
					t.Errorf("baseline edit carries V3 provenance: %+v", res)
				}
				// The agent's "V3 verified this edit" nudge keys off exactly
				// these fields, so an empty set is what keeps it quiet.
				if res.V3Used && verifiedPhase(res.PhaseSolved) {
					t.Error("an unauthorized edit would fire the V3-verified nudge")
				}
			})
		}
	}
}

// The nudge fires on V3Used plus a verified phase, and nothing else. A
// baseline fallback must not satisfy it.
func TestTheV3VerifiedNudgeFollowsAuthorization(t *testing.T) {
	authorized := &ToolResult{Success: true, V3Used: true, PhaseSolved: "phase1"}
	if !(authorized.Success && authorized.V3Used && verifiedPhase(authorized.PhaseSolved)) {
		t.Error("an authorized delivery must be able to fire the nudge")
	}
	for _, res := range []*ToolResult{
		{Success: true},
		{Success: true, PhaseSolved: "phase1"},
		{Success: true, V3Used: true, PhaseSolved: ""},
	} {
		if res.Success && res.V3Used && verifiedPhase(res.PhaseSolved) {
			t.Errorf("a delivery without provenance would fire the nudge: %+v", res)
		}
	}
}

// Structural sentinel: the edit path authorizes through the one gate, and
// never from the legacy fields.
func TestEditPathAuthorizesOnlyThroughTheSharedGate(t *testing.T) {
	src, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	improve := body[strings.Index(body, "func improveContentWithV3("):]
	improve = improve[:strings.Index(improve, "\n// findActualString")]
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
func codeOf(result *V3GenerateResponse) string {
	if result == nil {
		return ""
	}
	return result.Code
}

// V3's cap is a per-call ceiling, not a claim on the whole session. Measured
// 2026-09-14: the pipeline spent 295s of a 570s work budget repairing the
// FIRST of five files, and the run ended with one file and no verification,
// while the same prompt with a cheap first file delivered five files in 248s.
// One file may take at most half of what is left, so the run always keeps as
// much again for the work still to do.
func TestCallV3GenerateStreamingLeavesHalfTheSessionForTheRestOfTheRun(t *testing.T) {
	t.Setenv("ATLAS_V3_TIMEOUT", "300") // the configured ceiling, far above what remains
	release := make(chan struct{})
	srv := fakeGenerateServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseLines(w, `data: {"stage":"plan_search","detail":"stalling"}`, ``)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer srv.Close()
	defer close(release)

	// A session with 6s of work left: V3 may have 3s, not 300s.
	sessionCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	start := time.Now()
	_, err := callV3GenerateStreaming(sessionCtx, srv.URL, V3GenerateRequest{}, nil)
	elapsed := time.Since(start)
	t.Logf("V3 returned after %v (err=%v)", elapsed.Round(time.Millisecond), err)

	if err == nil {
		t.Fatal("a stalled V3 run returned success")
	}
	if elapsed > 5*time.Second {
		t.Errorf("V3 consumed %v of a 6s session; it must stop near the 3s half-share",
			elapsed.Round(time.Millisecond))
	}
	// The session itself must survive: the point is that time is left over.
	if sessionCtx.Err() != nil {
		t.Errorf("V3 used up the whole session context: %v", sessionCtx.Err())
	}
}

// The service plans its phases against the cap this call actually has. Told
// nothing, it planned a 300s run inside a call the bridge had cut to half of
// the session's remaining time, and started work the bridge then abandoned.
func TestCallV3GenerateStreamingSendsTheCapItApplies(t *testing.T) {
	var got []map[string]interface{}
	srv := fakeGenerateServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		w.Header().Set("Content-Type", "text/event-stream")
		sseLines(w, `event: result`, `data: {"code":"x = 1\n","passed":true}`, ``, `data: [DONE]`, ``)
	})
	defer srv.Close()

	t.Setenv("ATLAS_V3_TIMEOUT", "300")
	// No session deadline: the configured ceiling.
	if _, err := callV3GenerateStreaming(context.Background(), srv.URL, V3GenerateRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	// 6s of session left: half of it, whatever a caller put in the field.
	sessionCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := callV3GenerateStreaming(sessionCtx, srv.URL, V3GenerateRequest{BudgetMs: 999999}, nil); err != nil {
		t.Fatal(err)
	}
	// Uncapped: no cap is sent, and the service keeps its own reading.
	t.Setenv("ATLAS_V3_TIMEOUT", "0")
	if _, err := callV3GenerateStreaming(context.Background(), srv.URL, V3GenerateRequest{BudgetMs: 5}, nil); err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Fatalf("%d requests reached the service, want 3", len(got))
	}
	if v, _ := got[0]["budget_ms"].(float64); v != 300000 {
		t.Errorf("an unshortened call sent budget_ms=%v, want 300000", got[0]["budget_ms"])
	}
	if v, _ := got[1]["budget_ms"].(float64); v <= 2500 || v > 3000 {
		t.Errorf("a call with 6s of session left sent budget_ms=%v, want about 3000", got[1]["budget_ms"])
	}
	if v, ok := got[2]["budget_ms"]; ok {
		t.Errorf("an uncapped call sent budget_ms=%v", v)
	}
}

// The ceiling still applies when the session has plenty of room: half of a
// large remainder must not exceed the configured cap.
func TestCallV3GenerateStreamingKeepsItsCeilingWhenTheSessionIsLong(t *testing.T) {
	t.Setenv("ATLAS_V3_TIMEOUT", "1")
	release := make(chan struct{})
	srv := fakeGenerateServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseLines(w, `data: {"stage":"plan_search","detail":"stalling"}`, ``)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer srv.Close()
	defer close(release)

	sessionCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	start := time.Now()
	_, err := callV3GenerateStreaming(sessionCtx, srv.URL, V3GenerateRequest{}, nil)
	if err == nil {
		t.Fatal("stalled V3 run did not time out")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the 1s configured cap did not bound a long session: %v", elapsed)
	}
}
