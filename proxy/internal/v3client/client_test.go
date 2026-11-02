package v3client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itigges22/atlas-proxy/internal/wire"
)

// defaultCallCap is the cap the proxy passes when ATLAS_V3_TIMEOUT is unset.
const defaultCallCap = 300 * time.Second

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

func TestPlanStreamingParsesResult(t *testing.T) {
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

	plan, err := PlanStreaming(context.Background(), srv.URL, "", wire.V3PlanRequest{
		UserMessage: "add a hello endpoint",
		WorkingDir:  "/workspace",
	}, cb)
	if err != nil {
		t.Fatalf("PlanStreaming: %v", err)
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

func TestPlanStreamingMissingResult(t *testing.T) {
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

	_, err := PlanStreaming(context.Background(), srv.URL, "", wire.V3PlanRequest{UserMessage: "x"}, nil)
	if err == nil {
		t.Fatal("expected error for missing result event")
	}
	if !strings.Contains(err.Error(), "without result") {
		t.Errorf("error %q doesn't mention missing result", err.Error())
	}
}

// ---------------------------------------------------------------------------
// GenerateStreaming — the generate path. All three proxy callers treat a
// client error as "fall back to the model's own content", so every failure
// path must return, not hang.
// ---------------------------------------------------------------------------

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

func TestGenerateStreamingParsesResultAndProgress(t *testing.T) {
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
	result, err := GenerateStreaming(context.Background(), srv.URL, "", defaultCallCap,
		wire.V3GenerateRequest{FilePath: "a.py"},
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

func TestGenerateStreamingNon200IsAnError(t *testing.T) {
	srv := fakeGenerateServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	})
	defer srv.Close()

	_, err := GenerateStreaming(context.Background(), srv.URL, "", defaultCallCap,
		wire.V3GenerateRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want the 503 surfaced", err)
	}
}

func TestGenerateStreamingMissingResultIsAnError(t *testing.T) {
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

	_, err := GenerateStreaming(context.Background(), srv.URL, "", defaultCallCap,
		wire.V3GenerateRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), "without sending a result event") {
		t.Errorf("err = %v, want a stream that really ended empty", err)
	}
}

func TestGenerateStreamingUndecodableResultNamesTheDecodeFailure(t *testing.T) {
	// A result that does not decode must say so, and must not read like a
	// stream that sent nothing at all.
	srv := fakeGenerateServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseLines(w, `event: result`, `data: {not json`, ``, `data: [DONE]`, ``)
	})
	defer srv.Close()

	_, err := GenerateStreaming(context.Background(), srv.URL, "", defaultCallCap,
		wire.V3GenerateRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), "could not decode") {
		t.Errorf("err = %v, want the decode failure named", err)
	}
}

// stalledGenerateServer is a V3 service that sends one progress event and then
// stalls until the client hangs up or the test ends.
func stalledGenerateServer(t *testing.T) *httptest.Server {
	t.Helper()
	// release unblocks the stalled handler after the call returns: srv.Close
	// waits for active handlers, and the server does not always see the
	// client hang up.
	release := make(chan struct{})
	srv := fakeGenerateServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseLines(w, `data: {"stage":"plan_search","detail":"stalling"}`, ``)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // cleanups run last-in first-out, so this runs before srv.Close
	return srv
}

// A stalled V3 stream must end with an error at the first of: the configured
// cap, half of the session's remaining time, or the user's cancel
// (TestCancelAbortsAStalledV3Stream).
func TestGenerateStreamingEndsAStalledStream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		callCap time.Duration // the configured cap of one call
		session time.Duration // time left in the session; 0 means no deadline
		within  time.Duration // the call must return before this
	}{
		{name: "the configured cap fires",
			callCap: time.Second, within: 5 * time.Second},
		// Half of a long session is far above the cap, so the cap applies.
		{name: "the cap bounds a long session",
			callCap: time.Second, session: 10 * time.Minute, within: 5 * time.Second},
		// With 6s left, V3 may have 3s, not 300s, so the run keeps as much
		// again for the work still to do.
		{name: "half of a short session",
			callCap: 300 * time.Second, session: 6 * time.Second, within: 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := stalledGenerateServer(t)
			sessionCtx := context.Background()
			if tc.session > 0 {
				var cancel context.CancelFunc
				sessionCtx, cancel = context.WithTimeout(sessionCtx, tc.session)
				defer cancel()
			}

			start := time.Now()
			_, err := GenerateStreaming(sessionCtx, srv.URL, "", tc.callCap, wire.V3GenerateRequest{}, nil)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("a stalled V3 run returned no error")
			}
			if elapsed > tc.within {
				t.Errorf("the call took %v, want under %v", elapsed.Round(time.Millisecond), tc.within)
			}
			if sessionCtx.Err() != nil {
				t.Errorf("V3 used up the whole session: %v", sessionCtx.Err())
			}
			// The cap firing and V3 finishing empty are different events with
			// different fixes, so they must not share one message.
			if strings.Contains(err.Error(), "without sending a result event") {
				t.Errorf("the cap firing is reported as V3 finishing empty:\n%s", err)
			}
			if !strings.Contains(err.Error(), "ATLAS_V3_TIMEOUT") {
				t.Errorf("the error does not name the cap that fired:\n%s", err)
			}
		})
	}
}

// The user presses Ctrl-C during a long PlanSearch.
func TestCancelAbortsAStalledV3Stream(t *testing.T) {
	srv := stalledGenerateServer(t)
	callCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(150*time.Millisecond, cancel)

	start := time.Now()
	_, err := GenerateStreaming(callCtx, srv.URL, "", defaultCallCap, wire.V3GenerateRequest{}, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a stalled V3 run returned no error")
	}
	if elapsed > 3*time.Second {
		t.Errorf("the call took %v, want under %v", elapsed.Round(time.Millisecond), 3*time.Second)
	}
}

// The service plans its phases against the cap this call actually has. Told
// nothing, it plans a 300s run inside a call the bridge may cut to half of
// the session's remaining time.
func TestGenerateStreamingSendsTheCapItApplies(t *testing.T) {
	var got []map[string]interface{}
	srv := fakeGenerateServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		w.Header().Set("Content-Type", "text/event-stream")
		sseLines(w, `event: result`, `data: {"code":"x = 1\n","passed":true}`, ``, `data: [DONE]`, ``)
	})
	defer srv.Close()

	// No session deadline: the configured ceiling.
	if _, err := GenerateStreaming(context.Background(), srv.URL, "", 300*time.Second, wire.V3GenerateRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	// 6s of session left: half of it, whatever a caller put in the field.
	sessionCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := GenerateStreaming(sessionCtx, srv.URL, "", 300*time.Second, wire.V3GenerateRequest{BudgetMs: 999999}, nil); err != nil {
		t.Fatal(err)
	}
	// Uncapped: no cap is sent, and the service keeps its own reading.
	if _, err := GenerateStreaming(context.Background(), srv.URL, "", 0, wire.V3GenerateRequest{BudgetMs: 5}, nil); err != nil {
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

// The request id a caller passes travels to the service as a header, and a
// call without one sends none: it joins a candidate pool to the request that
// caused it.
func TestTheRequestIDTravelsAsAHeader(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(wire.RequestIDHeader))
		w.Header().Set("Content-Type", "text/event-stream")
		if r.URL.Path == "/v3/plan" {
			sseLines(w, `event: result`, `data: {"steps":[{"id":"s1"}]}`, ``, `data: [DONE]`, ``)
			return
		}
		sseLines(w, `event: result`, `data: {"code":"x = 1\n"}`, ``, `data: [DONE]`, ``)
	}))
	defer srv.Close()

	ctx := context.Background()
	if _, err := GenerateStreaming(ctx, srv.URL, "req-generate", 0, wire.V3GenerateRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateStreaming(ctx, srv.URL, "", 0, wire.V3GenerateRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanStreaming(ctx, srv.URL, "req-plan", wire.V3PlanRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanStreaming(ctx, srv.URL, "", wire.V3PlanRequest{}, nil); err != nil {
		t.Fatal(err)
	}

	want := []string{"req-generate", "", "req-plan", ""}
	if !slices.Equal(got, want) {
		t.Errorf("the service saw request ids %q, want %q", got, want)
	}
}
