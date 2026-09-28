package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Code emitted inside a JSON string pays escaping pressure on every dense
// line, and the served model measurably cannot sustain it: the same debounce
// solution parses 6/6 emitted in a fenced block and 0/6 emitted as a JSON
// string. "@fenced" routes the file body around the JSON channel via one
// unconstrained sub-call.

func TestFencedContentExtractsTheBlock(t *testing.T) {
	reply := "Here is the file:\n```python\nx = 1\nprint(x)\n```\nDone."
	if got := extractFencedContent(reply); got != "x = 1\nprint(x)\n" {
		t.Fatalf("extraction failed: %q", got)
	}
}

func TestFencedContentHandlesBareFence(t *testing.T) {
	reply := "```\ny = 2\n```"
	if got := extractFencedContent(reply); got != "y = 2\n" {
		t.Fatalf("extraction failed: %q", got)
	}
}

// stubInference serves /v1/chat/completions with the given replies in order,
// each stamped with tokensPer usage tokens. Other paths (the /slots prompt
// poller) get 404s and are not counted as calls.
func stubInference(t *testing.T, replies []string, tokensPer int, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		i := *calls
		*calls++
		if i >= len(replies) {
			i = len(replies) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		delta, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": replies[i]}}},
		})
		usage, _ := json.Marshal(map[string]interface{}{
			"choices": []interface{}{},
			"usage":   map[string]int{"total_tokens": tokensPer},
		})
		fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", delta, usage)
	}))
}

// Every fenced sub-call attempt is a real generation; the run totals must
// carry each one (third-party audit finding: sub-call spend was invisible).
func TestFencedFetchAccountsOneAttempt(t *testing.T) {
	calls := 0
	srv := stubInference(t, []string{"```python\nx = 1\n```"}, 100, &calls)
	defer srv.Close()
	t.Setenv("ATLAS_LLAMA_URL", srv.URL)

	ctx := &AgentContext{}
	got, err := fetchFencedContent(ctx, `{"type":"tool_call"}`, "a.py")
	if err != nil || got != "x = 1\n" {
		t.Fatalf("fetch failed: %q %v", got, err)
	}
	if calls != 1 || ctx.FencedCalls != 1 || ctx.FencedTokens != 100 || ctx.TotalTokens != 100 {
		t.Fatalf("one attempt must account one call and its tokens: calls=%d fenced=%d/%d total=%d",
			calls, ctx.FencedCalls, ctx.FencedTokens, ctx.TotalTokens)
	}
}

func TestFencedFetchAccountsFailedFirstAttempt(t *testing.T) {
	calls := 0
	srv := stubInference(t, []string{
		"no fence here, sorry",
		"```python\ny = 2\n```",
	}, 100, &calls)
	defer srv.Close()
	t.Setenv("ATLAS_LLAMA_URL", srv.URL)

	ctx := &AgentContext{}
	got, err := fetchFencedContent(ctx, `{"type":"tool_call"}`, "a.py")
	if err != nil || got != "y = 2\n" {
		t.Fatalf("fetch failed: %q %v", got, err)
	}
	if calls != 2 || ctx.FencedCalls != 2 || ctx.FencedTokens != 200 || ctx.TotalTokens != 200 {
		t.Fatalf("a failed first attempt is still a generation: calls=%d fenced=%d/%d total=%d",
			calls, ctx.FencedCalls, ctx.FencedTokens, ctx.TotalTokens)
	}
}

func TestRawResponseForFenceRoundTrips(t *testing.T) {
	parsed := ModelResponse{Type: "tool_call", Name: "write_file",
		Args: json.RawMessage(`{"path":"solve.py","content":"@fenced"}`)}
	raw := rawResponseForFence(parsed)
	var back ModelResponse
	if json.Unmarshal([]byte(raw), &back) != nil || back.Name != "write_file" {
		t.Fatalf("round trip failed: %s", raw)
	}
}

// A file that itself contains ``` (markdown, docstring examples) must not
// be cut at its first interior fence; the trailing-anchored form wins.
// A file that contains its own ``` lines is never cut at an interior fence.
// Inside a ``` wrapper those lines are indistinguishable from a second block,
// so no bytes are returned (the sub-call is re-asked with a four-backtick
// retry note); inside a ```` wrapper the whole file comes back.
func TestFencedExtractionSurvivesInteriorFences(t *testing.T) {
	body := "# readme\n\n```\nexample\n```\n\ntail line"
	if got := extractFencedContent("```markdown\n" + body + "\n```"); got != "" {
		t.Fatalf("an ambiguous ``` wrapper was resolved by guessing: %q", got)
	}
	if got := extractFencedContent("````markdown\n" + body + "\n````"); got != body+"\n" {
		t.Fatalf("four-backtick wrapper lost bytes: %q", got)
	}
}

func TestFencedExtractionAcceptsPunctuationTags(t *testing.T) {
	for _, tag := range []string{"c++", "c#", "objective-c"} {
		reply := "```" + tag + "\nint x;\n```"
		if got := extractFencedContent(reply); got == "" {
			t.Errorf("tag %q failed to open a block", tag)
		}
	}
}

func TestFenceTagFollowsExtension(t *testing.T) {
	if got := fenceTagForPath("app.html"); got != "html" {
		t.Errorf("html tag: %q", got)
	}
	if got := fenceTagForPath("main.go"); got != "go" {
		t.Errorf("go tag: %q", got)
	}
	if got := fenceTagForPath("solve.py"); got != "python" {
		t.Errorf("python tag: %q", got)
	}
}

// A file's own leading blank lines are the file's content, not part of the
// fence line. Found by fuzzing: the tag was followed by `\s*\n`, and \s
// matches newlines, so a greedy match ate every blank line at the top of the
// file before the capture began — silent mutation in the channel that
// carries every whole file ATLAS writes.
func TestFencedExtractionKeepsLeadingBlankLines(t *testing.T) {
	// Whitespace-only content is out of contract by design: an empty block
	// is what makes the sub-call ask again, so it is not a case here.
	for _, content := range []string{"\nx = 1\n", "\n\n\nx = 1\n", "\n\ndef f():\n    pass\n"} {
		reply := "```python\n" + content + "```"
		if got := extractFencedContent(reply); got != content {
			t.Errorf("leading blank lines dropped\n want=%q\n  got=%q", content, got)
		}
	}
}

// A CRLF reply must not leave the \r glued to the fence line or lose it
// from the content.
func TestFencedExtractionHandlesCRLFFenceLine(t *testing.T) {
	if got := extractFencedContent("```python\r\nx = 1\n```"); got != "x = 1\n" {
		t.Errorf("CRLF fence line mishandled: %q", got)
	}
}

// A truncated inline body must never reach disk.
//
// Found by the session hunt: the model emitted
// `"content": "@fenced\n```python\nprint("` — it began inlining the file and
// the JSON string was cut off mid-expression, which is the exact failure
// @fenced exists to avoid. The old strip declared "content arrived inline",
// the sanitizer then removed the fence line, and a one-line `print(` landed
// on disk. Six of twenty create sessions shipped an unparseable file that
// way, each repeating the identical write for five more turns.
// This used to assert that a bare body is a complete file ("bare complete
// body" -> accepted). That premise is what the sealed Stage-A acquisition
// disproved: every one of its 42 inlined bodies was bare, so the
// unterminated-fence branch never ran, and an unframed body that stopped
// mid-emission was written to disk. A bare body carries no framing evidence
// in either direction and is now resolved through the sub-call.
func TestInlineFencedBodyRequiresFramingToProveCompletion(t *testing.T) {
	cases := []struct {
		name     string
		inline   string
		wantBody string // "" means: must fall back to the sub-call
	}{
		{"truncated fence", "```python\nprint(", ""},
		{"opener only", "```python", ""},
		{"complete fence", "```python\nprint(\"hi\")\n```", "print(\"hi\")\n"},
		{"bare body, however finished it looks", "print(\"hi\")\n", ""},
		{"bare body cut mid-emission", "print(", ""},
	}
	for _, c := range cases {
		got, ok, _ := resolveInlineFencedBody(c.inline)
		if !ok {
			got = ""
		}
		if got != c.wantBody {
			t.Errorf("%s: resolved %q, want %q", c.name, got, c.wantBody)
		}
	}
}

// A fence containing only the sentinel is not a file. Measured live: asked
// for the contents of test_banner.py, the model replied
// "```python\n@fenced\n```", which extracts as non-empty and would have
// landed a file whose entire contents are the word @fenced.
func TestSentinelOnlyFenceIsNotContent(t *testing.T) {
	calls := 0
	srv := stubInference(t, []string{
		"```python\n@fenced\n```",
		"```python\nprint(\"real\")\n```",
	}, 50, &calls)
	defer srv.Close()
	t.Setenv("ATLAS_LLAMA_URL", srv.URL)

	ctx := &AgentContext{}
	got, err := fetchFencedContent(ctx, `{"type":"tool_call"}`, "a.py")
	if err != nil {
		t.Fatalf("expected the retry to succeed: %v", err)
	}
	if got != "print(\"real\")\n" {
		t.Fatalf("sentinel-only fence was accepted as content: %q", got)
	}
	if calls != 2 {
		t.Fatalf("expected a retry after the sentinel-only fence, got %d calls", calls)
	}
}

// --- Phase 2: progress-aware fenced-fetch bounds ---------------------------
//
// The zero-byte failure is a model that streams reasoning to max_tokens and
// never opens a fence. Measured on the seed-20260901 run: 9 of 17 zero-byte
// failures ran 175-311s, two attempts can consume ~10 minutes, and
// P(timeout | >=1 hang) was 8/11. The bound is on PROGRESS, not on total
// elapsed: a successful fetch legitimately ran 217s while producing content
// throughout.

func TestProductionFencedBoundsUseTheRealDefaults(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "")
	t.Setenv("ATLAS_FENCED_IDLE_SEC", "")
	if got := fencedFirstContentTimeout(); got != 60*time.Second {
		t.Errorf("first-content default = %v, want 60s", got)
	}
	if got := fencedIdleTimeout(); got != 30*time.Second {
		t.Errorf("idle default = %v, want 30s", got)
	}
	// The seam is internal and opt-in; production reads the real clock.
	t.Setenv("ATLAS_FENCED_IDLE_SEC", "2")
	if got := fencedIdleTimeout(); got != 2*time.Second {
		t.Errorf("override ignored: %v", got)
	}
	t.Setenv("ATLAS_FENCED_IDLE_SEC", "-1")
	if got := fencedIdleTimeout(); got != 30*time.Second {
		t.Errorf("invalid override must fall back to the default, got %v", got)
	}
}

// reasoningOnlyStub streams reasoning forever and never emits content: the
// exact zero-byte shape that ran to max_tokens in production.
func reasoningOnlyStub(t *testing.T, stop <-chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-stop:
				return
			default:
			}
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"reasoning_content": "thinking "}}},
			})
			fmt.Fprintf(w, "data: %s\n\n", d)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
}

func TestZeroByteFencedStreamIsCutAtTheFirstContentBound(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "1")
	stop := make(chan struct{})
	defer close(stop)
	srv := reasoningOnlyStub(t, stop)
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.Ctx = context.Background()

	start := time.Now()
	out, _, err := callLLMOnceWithGrammar(ctx, []AgentMessage{{Role: "user", Content: "x"}},
		0.2, rawEmissionSentinel)
	elapsed := time.Since(start)
	if elapsed > 20*time.Second {
		t.Fatalf("stream was not bounded: %v", elapsed)
	}
	if extractFencedContent(out) != "" {
		t.Errorf("a reasoning-only stream must not yield content: %q", out)
	}
	t.Logf("bounded in %v (err=%v)", elapsed, err)
}

// A slow but continuously progressing payload must survive: each content
// chunk resets the idle timer, so total elapsed far exceeding the idle bound
// is fine as long as the gaps do not.
func TestSlowButProgressingFencedPayloadSurvives(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "2")
	t.Setenv("ATLAS_FENCED_IDLE_SEC", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		parts := []string{"```python\n", "x = 1\n", "y = 2\n", "z = 3\n", "```"}
		for _, p := range parts {
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": p}}},
			})
			fmt.Fprintf(w, "data: %s\n\n", d)
			if fl != nil {
				fl.Flush()
			}
			// Well inside the idle bound, but five gaps sum past it.
			time.Sleep(300 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.Ctx = context.Background()
	out, _, err := callLLMOnceWithGrammar(ctx, []AgentMessage{{Role: "user", Content: "x"}},
		0.2, rawEmissionSentinel)
	if err != nil {
		t.Fatalf("progressing stream was cut: %v", err)
	}
	if body := extractFencedContent(out); !strings.Contains(body, "z = 3") {
		t.Errorf("payload truncated: %q", out)
	}
}

// Reasoning is not progress. A stream that reasons past the idle bound and
// only then emits content is still cut, because the first-content bound
// governs until real bytes arrive.
func TestReasoningDoesNotResetTheProgressTimer(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "1")
	t.Setenv("ATLAS_FENCED_IDLE_SEC", "30")
	stop := make(chan struct{})
	defer close(stop)
	srv := reasoningOnlyStub(t, stop)
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.Ctx = context.Background()
	start := time.Now()
	callLLMOnceWithGrammar(ctx, []AgentMessage{{Role: "user", Content: "x"}}, 0.2, rawEmissionSentinel)
	if el := time.Since(start); el > 20*time.Second {
		t.Fatalf("reasoning kept the stream alive past the first-content bound: %v", el)
	}
}

// Ordinary turns are untouched: the watchdog exists only for the fenced
// sub-call, so a normal grammar stream with no content and slow reasoning is
// governed by the pre-existing reasoning budget, not by these bounds.
func TestOrdinaryTurnsAreNotBoundedByTheFencedWatchdog(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		time.Sleep(1500 * time.Millisecond) // past the fenced bound
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": `{"type":"done"}`}}},
		})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.Ctx = context.Background()
	out, _, err := callLLMOnceWithGrammar(ctx, []AgentMessage{{Role: "user", Content: "x"}}, 0.2, "")
	if err != nil {
		t.Fatalf("ordinary turn was cut by the fenced watchdog: %v", err)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("ordinary turn lost its payload: %q", out)
	}
}

// Session cancellation still wins, and leaves nothing running.
func TestSessionCancellationAbortsTheFencedFetch(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "60")
	stop := make(chan struct{})
	defer close(stop)
	srv := reasoningOnlyStub(t, stop)
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = srv.URL
	cctx, cancel := context.WithCancel(context.Background())
	ctx.Ctx = cctx
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	before := runtime.NumGoroutine()
	start := time.Now()
	callLLMOnceWithGrammar(ctx, []AgentMessage{{Role: "user", Content: "x"}}, 0.2, rawEmissionSentinel)
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("cancellation did not propagate: %v", el)
	}
	time.Sleep(150 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+4 {
		t.Errorf("goroutines leaked: before=%d after=%d", before, after)
	}
}

// --- Phase 2: session/path-scoped retry budget ------------------------------
//
// fetchFencedContent's attempt counter was a local, so a new write_file call
// re-entered with a fresh allowance: a path that had already burned two ~300s
// attempts could burn two more next turn. The budget now lives in session
// state, keyed by path.

func fencedFailStub(t *testing.T, calls *int, succeedOnCall int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := *calls
		*calls++
		w.Header().Set("Content-Type", "text/event-stream")
		body := "I am thinking about it."
		if succeedOnCall >= 0 && i == succeedOnCall {
			body = "```python\nx = 1\n```"
		}
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": body}}},
		})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
}

func fencedCtx(t *testing.T, url string) *AgentContext {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = url
	ctx.Ctx = context.Background()
	return ctx
}

func TestFencedRetryBudgetSurvivesANewWriteFileCall(t *testing.T) {
	calls := 0
	srv := fencedFailStub(t, &calls, -1)
	defer srv.Close()
	ctx := fencedCtx(t, srv.URL)

	if _, err := fetchFencedContent(ctx, "raw", "solve.py"); err == nil {
		t.Fatal("expected failure")
	}
	afterFirst := calls
	if afterFirst != maxFencedFailuresPerPath {
		t.Fatalf("first call should spend the whole allowance: %d generations", afterFirst)
	}
	// A brand-new write_file for the same path re-enters the function.
	if _, err := fetchFencedContent(ctx, "raw", "solve.py"); err == nil {
		t.Fatal("expected refusal")
	}
	if calls != afterFirst {
		t.Errorf("a new write_file restarted the budget: %d generations after, want %d",
			calls, afterFirst)
	}
	if n := ctx.FencedFailures[fencedKey(ctx, "solve.py")]; n != maxFencedFailuresPerPath {
		t.Errorf("session failure count = %d", n)
	}
}

func TestFencedBudgetIsPerPath(t *testing.T) {
	calls := 0
	srv := fencedFailStub(t, &calls, -1)
	defer srv.Close()
	ctx := fencedCtx(t, srv.URL)

	fetchFencedContent(ctx, "raw", "a.py")
	spent := calls
	// A different path must have its own allowance.
	if _, err := fetchFencedContent(ctx, "raw", "b.py"); err == nil {
		t.Fatal("expected failure on b.py too")
	}
	if calls <= spent {
		t.Errorf("b.py was denied a.py's budget: %d generations", calls-spent)
	}
	if ctx.FencedFailures[fencedKey(ctx, "a.py")] != maxFencedFailuresPerPath ||
		ctx.FencedFailures[fencedKey(ctx, "b.py")] != maxFencedFailuresPerPath {
		t.Errorf("per-path counts wrong: %v", ctx.FencedFailures)
	}
}

func TestOneConstrainedRetryMayFollowAZeroByteFailure(t *testing.T) {
	calls := 0
	srv := fencedFailStub(t, &calls, 1) // first attempt empty, retry succeeds
	defer srv.Close()
	ctx := fencedCtx(t, srv.URL)

	out, err := fetchFencedContent(ctx, "raw", "solve.py")
	if err != nil {
		t.Fatalf("the constrained retry should have succeeded: %v", err)
	}
	if !strings.Contains(out, "x = 1") {
		t.Errorf("retry payload lost: %q", out)
	}
	if n := ctx.FencedFailures[fencedKey(ctx, "solve.py")]; n != 0 {
		t.Errorf("success must clear the consecutive-failure state, got %d", n)
	}
	if calls != 2 {
		t.Errorf("expected one failure plus one retry, got %d generations", calls)
	}
}

func TestSuccessRestoresTheFullAllowance(t *testing.T) {
	calls := 0
	srv := fencedFailStub(t, &calls, 1)
	defer srv.Close()
	ctx := fencedCtx(t, srv.URL)
	fetchFencedContent(ctx, "raw", "solve.py") // fail then succeed -> cleared
	if fencedBudgetExhausted(ctx, "solve.py") {
		t.Fatal("a healthy path must not stay exhausted")
	}
}

// Deadline present: a retry that cannot finish and still leave the reserve is
// not started.
func TestReserveAwareRetryAdmissionWithADeadline(t *testing.T) {
	calls := 0
	srv := fencedFailStub(t, &calls, -1)
	defer srv.Close()
	ctx := fencedCtx(t, srv.URL)
	dctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	ctx.Ctx = dctx

	if _, err := fetchFencedContent(ctx, "raw", "solve.py"); err == nil {
		t.Fatal("expected failure")
	}
	// first-content bound (60s) + reserve (20s) cannot fit in 500ms, so the
	// fetch is refused before any generation.
	if calls != 0 {
		t.Errorf("started %d generation(s) with no room to validate", calls)
	}
	if !strings.Contains(func() string {
		_, err := fetchFencedContent(ctx, "raw", "other.py")
		return err.Error()
	}(), "session budget") {
		t.Error("refusal should name the budget as the reason")
	}
}

// Deadline absent — no longer production (Phase 2B gives every session one),
// but still the shape a direct caller or an older embedder produces. Fetching
// stays bounded by the progress watchdog; this makes no claim about total
// session duration.
func TestNoDeadlineStillBoundsFetchingWithoutClaimingSessionBudget(t *testing.T) {
	calls := 0
	srv := fencedFailStub(t, &calls, -1)
	defer srv.Close()
	ctx := fencedCtx(t, srv.URL) // context.Background(): no deadline
	if _, ok := ctx.Ctx.Deadline(); ok {
		t.Fatal("fixture should have no deadline")
	}
	if !fencedFitsRemainingBudget(ctx) {
		t.Error("with no deadline there is no budget to fail against")
	}
	if _, err := fetchFencedContent(ctx, "raw", "solve.py"); err == nil {
		t.Fatal("expected failure")
	}
	// Bounded by the per-path allowance, not by any session budget.
	if calls != maxFencedFailuresPerPath {
		t.Errorf("unbounded: %d generations", calls)
	}
}

// A failed resolution must not touch the file.
func TestFailedFencedResolutionLeavesDiskBytesUnchanged(t *testing.T) {
	calls := 0
	srv := fencedFailStub(t, &calls, -1)
	defer srv.Close()
	ctx := fencedCtx(t, srv.URL)
	target := filepath.Join(ctx.WorkingDir, "solve.py")
	original := "original = True\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	fetchFencedContent(ctx, "raw", "solve.py")
	after, _ := os.ReadFile(target)
	if string(after) != original {
		t.Errorf("failed resolution modified disk: %q", string(after))
	}
}

// --- Phase 2: black-box proof through the production agent loop ------------
//
// Drives runAgentLoop against a model that answers every write_file with the
// @fenced sentinel and then never emits a fence — the shape behind 17
// zero-byte resolutions and 10 timeouts in the seed-20260901 run.
//
// Against the parent each write_file starts a fresh unbounded fetch, so the
// generation count grows with the turn count. Against current the fetch is
// bounded by progress and the allowance is spent once per path, so the loop
// stops issuing generations and the file is untouched.
//
// Uses no symbol the fix introduced except the allowance constant, which is
// read as a bound rather than asserted, so the same fixture compiles and runs
// against the parent.
func TestFencedHangIsBoundedThroughTheAgentLoop(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "1")
	dir := t.TempDir()
	rel := "solve.py"
	original := "untouched = True\n"
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	fenceFetches := 0
	stop := make(chan struct{})
	defer close(stop)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v3/") || strings.HasPrefix(r.URL.Path, "/internal/") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/execute") {
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		if strings.Contains(string(raw), "single fenced block") {
			mu.Lock()
			fenceFetches++
			mu.Unlock()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-stop:
					return
				default:
				}
				d, _ := json.Marshal(map[string]interface{}{
					"choices": []map[string]interface{}{
						{"delta": map[string]string{"reasoning_content": "thinking "}}},
				})
				fmt.Fprintf(w, "data: %s\n\n", d)
				if fl != nil {
					fl.Flush()
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		call, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": rel, "content": "@fenced"},
		})
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}},
		})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 8

	start := time.Now()
	if err := runAgentLoop(ctx, "Create solve.py."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}
	elapsed := time.Since(start)

	mu.Lock()
	got := fenceFetches
	mu.Unlock()

	// The allowance is spent once for this path and never restored, however
	// many write_file calls follow. The parent restarts it every turn, so its
	// count scales with MaxTurns.
	if got > 2 {
		t.Errorf("fenced generations = %d across %d turns; the per-path session "+
			"allowance is 2 and a new write_file must not restore it",
			got, ctx.MaxTurns)
	}
	if elapsed > 60*time.Second {
		t.Errorf("loop was not bounded: %v", elapsed)
	}
	after, _ := os.ReadFile(filepath.Join(dir, rel))
	if string(after) != original {
		t.Errorf("a failed fenced resolution must not touch disk: %q", string(after))
	}
	t.Logf("fenced_generations=%d turns<=%d elapsed=%v", got, ctx.MaxTurns, elapsed)
}

func TestFencedAllowanceIsKeyedOnTheCanonicalPath(t *testing.T) {
	calls := 0
	srv := fencedFailStub(t, &calls, -1)
	defer srv.Close()
	ctx := fencedCtx(t, srv.URL)

	fetchFencedContent(ctx, "raw", "solve.py")
	spent := calls
	// Same file, different spelling: must share the allowance, not restart it.
	if _, err := fetchFencedContent(ctx, "raw", "./solve.py"); err == nil {
		t.Fatal("expected refusal for the equivalent spelling")
	}
	if calls != spent {
		t.Errorf("./solve.py got a fresh allowance: %d extra generations", calls-spent)
	}
	if len(ctx.FencedFailures) != 1 {
		t.Errorf("equivalent spellings created %d keys: %v",
			len(ctx.FencedFailures), ctx.FencedFailures)
	}
	// A genuinely different path keeps its own.
	if _, err := fetchFencedContent(ctx, "raw", "other.py"); err == nil {
		t.Fatal("expected other.py to fail on its own budget")
	}
	if calls <= spent {
		t.Error("other.py was denied its independent allowance")
	}
}

func TestWatchdogCancelsOnlyTheChildAndChargesTheAllowance(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "1")
	stop := make(chan struct{})
	defer close(stop)
	srv := reasoningOnlyStub(t, stop)
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = srv.URL
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	ctx.Ctx = parent

	fetchFencedContent(ctx, "raw", "solve.py")
	// The agent session must survive its own watchdog.
	if err := parent.Err(); err != nil {
		t.Fatalf("watchdog killed the parent session context: %v", err)
	}
	// A cancelled zero-content attempt is a failure and must consume budget,
	// or the loop restarts it on the next write_file.
	if n := ctx.FencedFailures[fencedKey(ctx, "solve.py")]; n == 0 {
		t.Error("watchdog cancellation consumed no allowance")
	}
	if !fencedBudgetExhausted(ctx, "solve.py") {
		t.Error("two cancelled attempts should exhaust the path's allowance")
	}
}

func TestEnvOverridesRejectUnsafeValues(t *testing.T) {
	for _, bad := range []string{"0", "-5", "abc", "", "99999"} {
		t.Setenv("ATLAS_FENCED_IDLE_SEC", bad)
		if got := fencedIdleTimeout(); got != 30*time.Second {
			t.Errorf("override %q produced %v; unsafe values must fall back to 30s",
				bad, got)
		}
	}
	t.Setenv("ATLAS_FENCED_IDLE_SEC", "45")
	if got := fencedIdleTimeout(); got != 45*time.Second {
		t.Errorf("a reasonable override was ignored: %v", got)
	}
}

func TestNoPersistentStateOrWireFieldWasAdded(t *testing.T) {
	types, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatal(err)
	}
	// Session-scoped, in-memory only: no file, no DB, no wire field.
	if !strings.Contains(string(types), "FencedFailures map[string]int") {
		t.Fatal("the allowance is not session state")
	}
	for _, f := range []string{"types.go", "agent.go"} {
		b, _ := os.ReadFile(f)
		for _, leak := range []string{`json:"fenced_failures`, "os.WriteFile(fencedState"} {
			if strings.Contains(string(b), leak) {
				t.Errorf("%s: %q suggests a wire field or persistent state", f, leak)
			}
		}
	}
}

// --- Phase 2B commit B: the server-owned session budget ---------------------

func TestSessionBudgetBounds(t *testing.T) {
	for _, c := range []struct {
		raw       string
		wantTotal time.Duration
	}{
		{"", 600 * time.Second},
		{"abc", 600 * time.Second},
		{"0", 600 * time.Second},
		{"-1", 600 * time.Second},
		{"119", 600 * time.Second},
		{"3601", 600 * time.Second},
		{"999999", 600 * time.Second},
		{"120", 120 * time.Second},
		{"300", 300 * time.Second},
		{"3600", 3600 * time.Second},
	} {
		t.Run("override="+c.raw, func(t *testing.T) {
			if c.raw == "" {
				t.Setenv("ATLAS_AGENT_SESSION_TIMEOUT_SEC", "")
			} else {
				t.Setenv("ATLAS_AGENT_SESSION_TIMEOUT_SEC", c.raw)
			}
			total, reserve := sessionBudget()
			if total != c.wantTotal {
				t.Errorf("total = %v, want %v", total, c.wantTotal)
			}
			if reserve != 30*time.Second {
				t.Errorf("reserve = %v, want 30s", reserve)
			}
			if total-reserve <= 0 {
				t.Error("the work deadline is not positive")
			}
		})
	}
}

// timeoutFixture drives a real agent loop whose work context is already
// expiring, so the deadline lands inside production code rather than being
// simulated. hang controls where: mid-LLM-stream or mid-tool-call.
func timeoutFixture(t *testing.T, dir string, hang string, onLLM func()) (*AgentContext, map[string]string, map[string]int) {
	t.Helper()
	stop := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "]]")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError"}
			}
			json.NewEncoder(w).Encode(out)
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case strings.HasSuffix(r.URL.Path, "/shell"):
			// A tool call that never returns until the request is cancelled.
			if hang == "tool" {
				select {
				case <-r.Context().Done():
				case <-stop:
				}
				http.Error(w, "cancelled", http.StatusGatewayTimeout)
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"stdout": "", "stderr": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		if onLLM != nil {
			onLLM()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		if hang == "llm" {
			// Stream forever: the deadline has to be what stops this.
			for {
				select {
				case <-r.Context().Done():
					return
				case <-stop:
					return
				default:
				}
				d, _ := json.Marshal(map[string]interface{}{
					"choices": []map[string]interface{}{
						{"delta": map[string]string{"content": "."}}}})
				fmt.Fprintf(w, "data: %s\n\n", d)
				if fl != nil {
					fl.Flush()
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		call, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "run_command",
			"args": map[string]string{"command": "sleep 60"}})
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	// Release the hanging handlers BEFORE the server waits for them.
	// httptest.Server.Close blocks until every handler has returned, and the
	// "tool" handler returns when the request context dies or `stop` closes.
	// Cleanups run last-registered-first, so a `stop` registered before the
	// server would close only after Close had already blocked on the handler
	// -- a deadlock this suite never saw while the slot-erase retries spent
	// the whole 900ms deadline before turn 0 could reach the tool at all.
	t.Cleanup(func() {
		close(stop)
		srv.Close()
	})

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 20

	// The two lifetimes the handler builds, with a deterministically short
	// work deadline instead of ten minutes.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	t.Cleanup(cancelReq)
	workCtx, cancelWork := context.WithTimeout(reqCtx, 900*time.Millisecond)
	t.Cleanup(cancelWork)
	ctx.RequestCtx = reqCtx
	ctx.Ctx = workCtx
	ctx.cancelWork = cancelWork

	terminal := map[string]string{}
	census := map[string]int{}
	var mu sync.Mutex
	ctx.StreamFn = func(eventType string, data interface{}) {
		mu.Lock()
		defer mu.Unlock()
		census[eventType]++
		if eventType == "done" {
			b, _ := json.Marshal(data)
			json.Unmarshal(b, &terminal)
		}
	}
	return ctx, terminal, census
}

func TestWorkDeadlineEmitsExactlyOneTimedOutTerminal(t *testing.T) {
	for _, where := range []string{"llm", "tool"} {
		t.Run("deadline during the "+where, func(t *testing.T) {
			dir := t.TempDir()
			ctx, terminal, census := timeoutFixture(t, dir, where, nil)

			start := time.Now()
			if err := runAgentLoop(ctx, "Do something long."); err != nil {
				t.Fatalf("agent loop error: %v", err)
			}
			elapsed := time.Since(start)

			if terminal["status"] != string(TerminalTimedOut) {
				t.Errorf("status = %q, want timed_out (summary=%q)",
					terminal["status"], terminal["summary"])
			}
			if terminal["reason"] != "work_deadline" {
				t.Errorf("reason = %q", terminal["reason"])
			}
			if census["done"] != 1 {
				t.Errorf("%d terminal events", census["done"])
			}
			// The terminal arrives AFTER the work context is dead, which is
			// the whole point of the split.
			if ctx.Ctx.Err() == nil {
				t.Error("work context outlived the deadline")
			}
			if ctx.RequestCtx.Err() != nil {
				t.Error("the response lifetime died with the work")
			}
			// Well inside the reserve.
			if elapsed > 20*time.Second {
				t.Errorf("finalisation took %v", elapsed)
			}
			if strings.Contains(terminal["summary"], "is on disk and parses") {
				t.Errorf("a timeout implied completion: %q", terminal["summary"])
			}
			t.Logf("elapsed=%v terminal=%v", elapsed, terminal)
		})
	}
}

// A timeout is not a completion, whatever survives on disk.
func TestTimeoutNeverClaimsCompletion(t *testing.T) {
	dir := t.TempDir()
	// A perfectly valid artifact is present the whole time.
	os.WriteFile(filepath.Join(dir, "solve.py"), []byte("A = 1\n"), 0o644)
	ctx, terminal, _ := timeoutFixture(t, dir, "llm", nil)
	if err := runAgentLoop(ctx, "Do something long."); err != nil {
		t.Fatal(err)
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("a timeout with a valid artifact read as completed: %v", terminal)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "solve.py")); string(got) != "A = 1\n" {
		t.Errorf("the timeout changed an untracked file: %q", got)
	}
}

// The timeout path reuses the Phase 3B rules unchanged: a demonstrably safer
// checkpoint is restored, and anything short of that is left alone.
func TestTimeoutRestorationObeysTheSameEligibilityRules(t *testing.T) {
	for _, c := range []struct {
		name        string
		seed        func(t *testing.T, ctx *AgentContext, dir string)
		wantBytes   string
		wantRestore bool
	}{
		{name: "demonstrably safer checkpoint is restored",
			seed: func(t *testing.T, ctx *AgentContext, dir string) {
				args, _ := json.Marshal(map[string]string{
					"path": "solve.py", "content": "def f():\n    return [1]\n"})
				executeToolCall("write_file", args, ctx)
				os.WriteFile(filepath.Join(dir, "solve.py"),
					[]byte("def f():\n    return [1]]\n"), 0o644)
			},
			wantBytes: "def f():\n    return [1]\n", wantRestore: true},
		{name: "invalid bytes with no checkpoint are kept",
			seed: func(t *testing.T, ctx *AgentContext, dir string) {
				// Written broken from the start, so nothing was ever valid.
				args, _ := json.Marshal(map[string]string{
					"path": "solve.py", "content": "def f():\n    return [1]]\n"})
				executeToolCall("write_file", args, ctx)
			},
			wantBytes: "def f():\n    return [1]]\n"},
		{name: "unknown validation is not a reason to act",
			seed: func(t *testing.T, ctx *AgentContext, dir string) {
				args, _ := json.Marshal(map[string]string{
					"path": "solve.py", "content": "def f():\n    return [1]\n"})
				executeToolCall("write_file", args, ctx)
				os.WriteFile(filepath.Join(dir, "solve.py"),
					[]byte("def f():\n    return [2]\n"), 0o644)
				// Checker gone: the current bytes have no verdict at all.
				ctx.SandboxURL = "http://127.0.0.1:1"
			},
			wantBytes: "def f():\n    return [2]\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx, terminal, _ := timeoutFixture(t, dir, "llm", nil)
			c.seed(t, ctx, dir)

			if err := runAgentLoop(ctx, "Do something long."); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
			if string(got) != c.wantBytes {
				t.Errorf("disk = %q, want %q", got, c.wantBytes)
			}
			if c.wantRestore {
				if !strings.Contains(terminal["summary"], "Put back the last version") {
					t.Errorf("restore was not disclosed: %q", terminal["summary"])
				}
				d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
				if d == nil || d.CurrentHash != hashBytes(got) {
					t.Error("the ledger does not describe the restored bytes")
				}
			}
			// Restored or not, a timeout is still a timeout.
			if terminal["status"] != string(TerminalTimedOut) {
				t.Errorf("status = %q", terminal["status"])
			}
		})
	}
}

// The reaper stops this session's jobs and leaves everything else alone.
func TestTimeoutReapsOnlyThisSessionsBackgroundJobs(t *testing.T) {
	var stopped []string
	var mu sync.Mutex
	zero := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stop") {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/stop")
			mu.Lock()
			stopped = append(stopped, id)
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"job_id": id, "killed": true, "exit_code": zero,
				"stdout": []string{}, "stderr": []string{}})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.SandboxURL = srv.URL
	ctx.StreamFn = func(string, interface{}) {}
	ctx.BackgroundJobs = map[string]string{
		"mine-1": "python app.py", "mine-2": "npm start",
	}
	// Hazards are owned by the job identity they came from.
	raiseWorkspaceHazard(ctx, "mine-1")
	raiseWorkspaceHazard(ctx, "mine-2")

	reapSessionBackgroundJobs(ctx)

	mu.Lock()
	got := append([]string(nil), stopped...)
	mu.Unlock()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "mine-1" || got[1] != "mine-2" {
		t.Errorf("reaped %v, want exactly this session's two jobs", got)
	}
	if len(ctx.BackgroundJobs) != 0 {
		t.Errorf("%d jobs still tracked after reaping", len(ctx.BackgroundJobs))
	}
	// Confirmed exits clear the hazard, which is what lets restoration run.
	if workspaceHazardous(ctx) {
		t.Error("hazard still raised after two confirmed exits")
	}
}

// A job that cannot be confirmed gone keeps the hazard raised, and the hazard
// is what stops restoration touching a file something may still be writing.
func TestUnconfirmedJobKeepsTheHazardAndBlocksRestore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No exit_code: signalled, not reaped.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"job_id": "j1", "killed": true, "stdout": []string{}, "stderr": []string{}})
	}))
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.SandboxURL = srv.URL
	ctx.StreamFn = func(string, interface{}) {}
	ctx.BackgroundJobs = map[string]string{"j1": "python app.py"}
	raiseWorkspaceHazard(ctx, "j1")

	reapSessionBackgroundJobs(ctx)
	if !workspaceHazardous(ctx) {
		t.Fatal("an unconfirmed exit cleared the hazard")
	}
	if len(restoreSaferDeliverables(ctx)) != 0 {
		t.Error("restoration ran while a job may still be writing")
	}
}

// A client that goes away is not a server timeout.
func TestClientDisconnectIsNotATimeout(t *testing.T) {
	dir := t.TempDir()
	ctx, terminal, census := timeoutFixture(t, dir, "llm", nil)
	// Kill the response lifetime first, as a dropped connection does.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	workCtx, cancelWork := context.WithCancel(reqCtx)
	ctx.RequestCtx = reqCtx
	ctx.Ctx = workCtx
	ctx.cancelWork = cancelWork
	cancelReq()

	err := runAgentLoop(ctx, "Do something long.")
	if err == nil {
		t.Error("a disconnect should surface as the context error")
	}
	if terminal["status"] == string(TerminalTimedOut) {
		t.Error("a disconnect was reported as a server timeout")
	}
	if census["done"] != 0 {
		t.Errorf("%d terminal events emitted into a closed response", census["done"])
	}
}

// The bounded-vs-unbounded contrast, written so it runs on the parent tree
// too: only ctx.Ctx is set, which is the one lifetime the parent has. The
// parent's loop returns the context error and emits nothing, so a client sees
// a stream that simply stops. Here the server owns the deadline and says so.
func TestASessionThatRunsOutOfTimeStillEndsWithATerminal(t *testing.T) {
	dir := t.TempDir()
	stop := make(chan struct{})
	defer close(stop)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v3/") || strings.HasPrefix(r.URL.Path, "/internal/") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/execute") {
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-stop:
				return
			default:
			}
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": "."}}}})
			fmt.Fprintf(w, "data: %s\n\n", d)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer srv.Close()

	before := runtime.NumGoroutine()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 20
	workCtx, cancelWork := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancelWork()
	ctx.Ctx = workCtx

	var mu sync.Mutex
	terminals := 0
	var payload map[string]string
	ctx.StreamFn = func(eventType string, data interface{}) {
		if eventType != "done" {
			return
		}
		b, _ := json.Marshal(data)
		mu.Lock()
		terminals++
		json.Unmarshal(b, &payload)
		mu.Unlock()
	}

	start := time.Now()
	runAgentLoop(ctx, "Do something long.")
	elapsed := time.Since(start)

	mu.Lock()
	n, got := terminals, payload
	mu.Unlock()

	if n != 1 {
		t.Fatalf("%d terminal events; a session that runs out of time must "+
			"still tell the client what happened", n)
	}
	if got["status"] != "timed_out" {
		t.Errorf("status = %q, want timed_out", got["status"])
	}
	if elapsed > 20*time.Second {
		t.Errorf("the deadline did not bound the run: %v", elapsed)
	}

	// Nothing left running behind it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("goroutines leaked: %d before, %d after", before, after)
	}
	t.Logf("elapsed=%v terminals=%d status=%q", elapsed, n, got["status"])
}

// Phase 2's reserve-aware admission needed no change to observe the new
// deadline: it already reserves against ctx.Ctx, which now always has one.
// This pins the wiring, because "it works automatically" is the kind of claim
// that stops being true silently.
func TestFencedAdmissionObservesTheWorkDeadline(t *testing.T) {
	calls := 0
	srv := fencedFailStub(t, &calls, -1)
	defer srv.Close()

	// A session with plenty of budget admits the fetch.
	roomy := fencedCtx(t, srv.URL)
	deep, cancelDeep := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelDeep()
	roomy.Ctx = deep
	if !fencedFitsRemainingBudget(roomy) {
		t.Error("a session with ten minutes left refused a fenced fetch")
	}

	// One whose work deadline is closer than the fetch plus its reserve does
	// not: resolving would consume the time needed to validate the result.
	tight := fencedCtx(t, srv.URL)
	shallow, cancelShallow := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShallow()
	tight.Ctx = shallow
	if fencedFitsRemainingBudget(tight) {
		t.Fatal("a fetch was admitted with no room left to validate it")
	}
	before := calls
	if _, err := fetchFencedContent(tight, "raw", "solve.py"); err == nil {
		t.Fatal("expected the admission refusal to surface")
	} else if !strings.Contains(err.Error(), "session budget") {
		t.Errorf("refusal did not name the budget: %v", err)
	}
	if calls != before {
		t.Errorf("a refused fetch still spent %d generations", calls-before)
	}

	// And the real budget leaves room: 600s total, 30s reserve.
	t.Setenv("ATLAS_AGENT_SESSION_TIMEOUT_SEC", "")
	total, reserve := sessionBudget()
	if total-reserve <= fencedFirstContentTimeout()+fencedReserve {
		t.Errorf("the default work deadline (%v) cannot admit a single fenced "+
			"fetch", total-reserve)
	}
}

// --- Phase 2C: the malformed fenced call ------------------------------------
//
// The fenced channel costs a full unconstrained generation per attempt, and it
// was opened before anyone asked whether the call could execute. A model
// re-sending `write_file {"content":"@fenced"}` with no path spent the whole
// session that way: the 300s canary reached turn 36 with nothing on disk.
//
// The preflight runs the checks the call has to survive anyway, by calling
// them, so nothing new decides anything.

// The property the whole design rests on: every call the preflight refuses is
// also refused by the tool, which is what makes "decline to resolve, then let
// the tool answer" safe rather than a way to write "@fenced" to disk.
func TestPreflightRefusalsAreASubsetOfTheToolsOwn(t *testing.T) {
	for _, c := range []struct{ name, args string }{
		{"no arguments", ``},
		{"null arguments", `null`},
		{"malformed shape", `{"path":123,"content":"@fenced"}`},
		{"missing path", `{"content":"@fenced"}`},
		{"blank path", `{"path":"","content":"@fenced"}`},
		{"whitespace path", `{"path":"   ","content":"@fenced"}`},
		{"tab path", `{"path":"\t\n","content":"@fenced"}`},
		{"workspace escape", `{"path":"../outside.py","content":"@fenced"}`},
		{"absolute escape", `{"path":"/etc/passwd","content":"@fenced"}`},
		{"deny-listed", `{"path":".env","content":"@fenced"}`},
		{"deny-listed key", `{"path":"id_rsa.pem","content":"@fenced"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.PermissionMode = PermissionYolo
			ctx.StreamFn = func(string, interface{}) {}

			ok, why := fencedCallIsExecutable("write_file", json.RawMessage(c.args), ctx)
			if ok {
				t.Fatalf("preflight admitted an unusable call")
			}
			if why == "" {
				t.Error("refused without saying why")
			}
			// The tool refuses it too, with no mutation and nothing on disk.
			res := executeToolCall("write_file", json.RawMessage(c.args), ctx)
			if res.Success {
				t.Fatalf("the tool accepted what the preflight refused: %s", c.args)
			}
			if res.MutationStatus != MutationNone {
				t.Errorf("MutationStatus = %q, want none", res.MutationStatus)
			}
			if !res.Classified() {
				t.Errorf("unclassified refusal: %+v", res)
			}
			var found []string
			filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
				if err == nil && info != nil && !info.IsDir() {
					found = append(found, p)
				}
				return nil
			})
			if len(found) != 0 {
				t.Errorf("an unusable call put files on disk: %v", found)
			}
			if len(ctx.Ledger) != 0 {
				t.Errorf("an unusable call entered the ledger: %v", ctx.Ledger)
			}
		})
	}
}

// A usable call is still admitted — the preflight is a gate, not a wall.
func TestPreflightAdmitsAUsableCall(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	for _, args := range []string{
		`{"path":"solve.py","content":"@fenced"}`,
		`{"path":"./solve.py","content":"@fenced"}`,
		`{"path":"pkg/solve.py","content":"@fenced"}`,
	} {
		if ok, why := fencedCallIsExecutable("write_file", json.RawMessage(args), ctx); !ok {
			t.Errorf("%s refused: %s", args, why)
		}
	}
}

// fencedCountingStub answers the model with a fixed script and counts how many
// unconstrained fenced generations were requested. Free of Phase 2C symbols so
// the same fixture runs on the parent tree.
func fencedCountingStub(t *testing.T, dir string, calls, fences *int, script func(int) map[string]interface{}) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")

		// The fenced sub-call is the one that asks for a single fenced block.
		if strings.Contains(string(raw), "single fenced block") {
			mu.Lock()
			*fences++
			mu.Unlock()
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": "```python\nA = 1\n```"}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
			return
		}
		mu.Lock()
		i := *calls
		*calls++
		mu.Unlock()
		body, _ := json.Marshal(script(i))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(body)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
}

// The production claim, through the real loop: a malformed fenced call starts
// ZERO generations, and the run reaches its terminal in a handful of turns
// instead of burning the session. On the parent every one of those turns opens
// the channel first.
func TestMalformedFencedCallStartsNoGeneration(t *testing.T) {
	dir := t.TempDir()
	calls, fences := 0, 0
	srv := fencedCountingStub(t, dir, &calls, &fences, func(i int) map[string]interface{} {
		// The exact shape the canary produced: content set, path absent.
		return map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"content": "@fenced"}}
	})
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 20

	var terminal map[string]string
	ctx.StreamFn = func(eventType string, data interface{}) {
		if eventType == "done" {
			b, _ := json.Marshal(data)
			json.Unmarshal(b, &terminal)
		}
	}
	start := time.Now()
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}
	elapsed := time.Since(start)

	if fences != 0 {
		t.Errorf("%d fenced generations started for a call with no path", fences)
	}
	// Bounded escalation: the repeat detector sees identical malformed calls
	// and stops the run quickly.
	if calls > 12 {
		t.Errorf("%d model turns before terminating on a malformed call", calls)
	}
	if terminal["status"] == "" {
		t.Fatal("no classified terminal")
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("a run that wrote nothing reported %q", terminal["status"])
	}
	// No allowance state for a path that never existed.
	if len(ctx.FencedFailures) != 0 {
		t.Errorf("allowance keys created for an unusable call: %v", ctx.FencedFailures)
	}
	var found []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() &&
			!strings.Contains(p, ".atlas-mount-probe") {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Errorf("files on disk after a malformed run: %v", found)
	}
	t.Logf("turns=%d fenced_generations=%d elapsed=%v status=%q reason=%q",
		calls, fences, elapsed, terminal["status"], terminal["reason"])
}

// Equivalent bad spellings cannot each buy their own allowance, because none
// of them reaches the allowance at all.
func TestNoAllowanceKeyForAnyUnusableSpelling(t *testing.T) {
	dir := t.TempDir()
	calls, fences := 0, 0
	spellings := []string{"", "   ", "\t", "../escape.py", ".env"}
	srv := fencedCountingStub(t, dir, &calls, &fences, func(i int) map[string]interface{} {
		return map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{
				"path": spellings[i%len(spellings)], "content": "@fenced"}}
	})
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 12
	ctx.StreamFn = func(string, interface{}) {}

	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatal(err)
	}
	if fences != 0 {
		t.Errorf("%d generations started across unusable spellings", fences)
	}
	if len(ctx.FencedFailures) != 0 {
		t.Errorf("allowance map is not empty: %v", ctx.FencedFailures)
	}
}

// The channel still works for a call that can execute, and the bytes it
// carries still land.
func TestValidFencedPathStillResolvesThroughTheLoop(t *testing.T) {
	dir := t.TempDir()
	calls, fences := 0, 0
	srv := fencedCountingStub(t, dir, &calls, &fences, func(i int) map[string]interface{} {
		if i == 0 {
			return map[string]interface{}{
				"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
		}
		return map[string]interface{}{"type": "done", "summary": "wrote solve.py"}
	})
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 8
	ctx.StreamFn = func(string, interface{}) {}

	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatal(err)
	}
	if fences != 1 {
		t.Errorf("%d fenced generations for one valid call, want 1", fences)
	}
	got, err := os.ReadFile(filepath.Join(dir, "solve.py"))
	if err != nil {
		t.Fatalf("the resolved file never landed: %v", err)
	}
	if string(got) != "A = 1\n" {
		t.Errorf("disk = %q, want the fenced body", got)
	}
}

// An inline write never involved the channel and must be untouched by this.
func TestValidInlineWriteIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	calls, fences := 0, 0
	const body = "def solve():\n    return 42\n"
	srv := fencedCountingStub(t, dir, &calls, &fences, func(i int) map[string]interface{} {
		if i == 0 {
			return map[string]interface{}{
				"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "solve.py", "content": body}}
		}
		return map[string]interface{}{"type": "done", "summary": "wrote solve.py"}
	})
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 8
	ctx.StreamFn = func(string, interface{}) {}

	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatal(err)
	}
	if fences != 0 {
		t.Errorf("an inline write opened the fenced channel %d times", fences)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	if string(got) != body {
		t.Errorf("disk = %q, want %q", got, body)
	}
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d == nil || d.CurrentHash != hashBytes([]byte(body)) {
		t.Error("the ledger does not describe the inline write")
	}
}

// --- Phase 4B-M1: the signature describes what the model sent ---------------
//
// Fenced resolution rewrites parsed.Args with the fetched file body before the
// repetition detector fingerprints it. The model's call is byte-identical every
// turn; the bytes the detector sees are different every turn. In the frozen
// run that blindness is why debounce2 sent the same write_file seven times and
// reached the 600 s harness cap with no intervention at all.
//
// m1Stub answers every fenced sub-call with a DIFFERENT body, which is what
// makes the two builds diverge: same intent, different resolved bytes.
func m1Stub(t *testing.T, dir string, turns, fences *int, script func(int) map[string]interface{}) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")

		if strings.Contains(string(raw), "single fenced block") {
			mu.Lock()
			n := *fences
			*fences++
			mu.Unlock()
			// A different body every time — the model iterating on the file.
			body := fmt.Sprintf("```python\nVALUE = %d\n%s\n```", n, strings.Repeat("# pad\n", n))
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": body}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
			return
		}
		mu.Lock()
		i := *turns
		*turns++
		mu.Unlock()
		call, _ := json.Marshal(script(i))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
}

func m1Ctx(t *testing.T, dir, url string, census map[string]int, terminal map[string]string) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = url, url, url
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 14
	var mu sync.Mutex
	ctx.StreamFn = func(eventType string, data interface{}) {
		mu.Lock()
		defer mu.Unlock()
		census[eventType]++
		if eventType == "done" {
			b, _ := json.Marshal(data)
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	return ctx
}

// Seven byte-identical raw calls, seven different fetched bodies. The parent
// records seven distinct signatures and intervenes zero times; here the intent
// is one signature and the existing threshold is reached.
func TestIdenticalFencedIntentIsOneSignature(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	srv := m1Stub(t, dir, &turns, &fences, func(i int) map[string]interface{} {
		return map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
	})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	ctx := m1Ctx(t, dir, srv.URL, census, terminal)
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	t.Logf("turns=%d fenced=%d interventions=%d status=%q reason=%q",
		turns, fences, census["agent_repeat_intervention"],
		terminal["status"], terminal["reason"])

	if census["agent_repeat_intervention"] < 2 {
		t.Errorf("%d repeat interventions across %d identical calls; the detector "+
			"is still reading the resolved bytes rather than the model's call",
			census["agent_repeat_intervention"], turns)
	}
	if turns > 10 {
		t.Errorf("%d turns before the breaker engaged", turns)
	}
	// Requirement 6: the fetched bytes still did their job.
	got, err := os.ReadFile(filepath.Join(dir, "solve.py"))
	if err != nil {
		t.Fatalf("the resolved file never landed: %v", err)
	}
	if !strings.HasPrefix(string(got), "VALUE = ") {
		t.Errorf("disk does not hold a fetched body: %q", got)
	}
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Error("the ledger does not describe the bytes on disk")
	}
	// Requirement 8: the terminal contract is untouched.
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("a repeat-broken run reported %q", terminal["status"])
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result invariant broken: %d vs %d",
			census["tool_call"], census["tool_result"])
	}
}

// Spelling the same target two ways must not buy a fresh budget.
//
// Owned by the repeat detector, which is where the name says it belongs: the
// detector reads the RAW intent, and `@fenced` on solve.py and on ./solve.py
// are one signature there. It briefly showed up in the failure window instead,
// because the resend ban was also reading the raw intent and fired first; now
// that an executed write is identified by its resolved proposal, the ban stops
// answering for a channel that returns different bytes each time and the
// detector answers again. Both spellings still have to collapse to one target
// for it to fire at all, which is the property.
// TestDifferentFencedTargetsStayIndependent is the other half: distinct
// targets must not collapse the same way.
func TestCanonicalSpellingsShareTheIntentSignature(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	spellings := []string{"solve.py", "./solve.py", "solve.py", "./solve.py"}
	srv := m1Stub(t, dir, &turns, &fences, func(i int) map[string]interface{} {
		return map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{
				"path": spellings[i%len(spellings)], "content": "@fenced"}}
	})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	ctx := m1Ctx(t, dir, srv.URL, census, terminal)
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatal(err)
	}
	t.Logf("turns=%d interventions=%d status=%q reason=%q fences=%d", turns,
		census["agent_repeat_intervention"], terminal["status"], terminal["reason"], fences)
	if census["agent_repeat_intervention"] < 2 {
		t.Errorf("alternating spellings evaded repetition tracking (%d interventions in %d turns)",
			census["agent_repeat_intervention"], turns)
	}
	if turns >= 30 {
		t.Fatalf("%d turns without a terminal", turns)
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("a run that only ever re-sent one call reported %q", terminal["status"])
	}
	_ = ctx
}

// Two different files are two different problems and keep their own budgets.
func TestDifferentFencedTargetsStayIndependent(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	paths := []string{"a.py", "b.py", "c.py", "d.py", "e.py", "f.py"}
	srv := m1Stub(t, dir, &turns, &fences, func(i int) map[string]interface{} {
		if i >= len(paths) {
			return map[string]interface{}{"type": "done", "summary": "wrote them"}
		}
		return map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": paths[i], "content": "@fenced"}}
	})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	ctx := m1Ctx(t, dir, srv.URL, census, terminal)
	if err := runAgentLoop(ctx, "Write six files."); err != nil {
		t.Fatal(err)
	}
	t.Logf("turns=%d interventions=%d status=%q reason=%q summary=%.120s",
		turns, census["agent_repeat_intervention"], terminal["status"],
		terminal["reason"], terminal["summary"])
	if census["agent_repeat_intervention"] != 0 {
		t.Errorf("%d interventions for six distinct targets", census["agent_repeat_intervention"])
	}
	// Not every file lands: once a.py and b.py exist, the sibling-pattern hint
	// starts asking the model to read one before creating another. That guard
	// is unrelated to the signature, so this checks only what M1 owns — the
	// files that DID land hold their own fetched body, and no two targets
	// collided into one budget.
	landed := 0
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			continue
		}
		landed++
		if !strings.HasPrefix(string(b), "VALUE = ") {
			t.Errorf("%s does not hold a fetched body: %q", p, b)
		}
	}
	if landed < 2 {
		t.Fatalf("only %d of the distinct targets landed; the fixture proves nothing", landed)
	}
}

// A model genuinely revising a file inline must not be read as repetition.
func TestMateriallyDifferentInlineWritesAreNotRepetition(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	srv := m1Stub(t, dir, &turns, &fences, func(i int) map[string]interface{} {
		if i >= 6 {
			return map[string]interface{}{"type": "done", "summary": "done revising"}
		}
		body := fmt.Sprintf("def solve():\n    total = %d\n    return total\n", i*7+1)
		return map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": "solve.py", "content": body}}
	})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	ctx := m1Ctx(t, dir, srv.URL, census, terminal)
	if err := runAgentLoop(ctx, "Revise solve.py."); err != nil {
		t.Fatal(err)
	}
	if fences != 0 {
		t.Errorf("an inline write opened the fenced channel %d times", fences)
	}
	if census["agent_repeat_intervention"] != 0 {
		t.Errorf("%d interventions for six materially different revisions",
			census["agent_repeat_intervention"])
	}
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	if !strings.Contains(string(got), "total = 36") {
		t.Errorf("the last revision did not land: %q", got)
	}
}

// The signature is intent-only, so a call the preflight refuses records
// nothing and starts nothing.
func TestMalformedIntentRecordsNoSignatureAndStartsNoFetch(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	srv := m1Stub(t, dir, &turns, &fences, func(i int) map[string]interface{} {
		return map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"content": "@fenced"}}
	})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	ctx := m1Ctx(t, dir, srv.URL, census, terminal)
	if err := runAgentLoop(ctx, "Write it."); err != nil {
		t.Fatal(err)
	}
	if fences != 0 {
		t.Errorf("%d fenced generations for a call with no path", fences)
	}
	if len(ctx.FencedFailures) != 0 {
		t.Errorf("allowance keys created: %v", ctx.FencedFailures)
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("a run that wrote nothing reported %q", terminal["status"])
	}
}

// The retry ban had the same blindness the repeat detector had: it compares
// the args left behind by fenced resolution, so a re-sent `@fenced` call whose
// body came back different is not recognised as the same rejected call. The
// lookup, the record and the clear all key on the model's intent now, or they
// key on three different things and never meet.
func TestRejectedFencedIntentIsRecognisedOnResend(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	stop := make(chan struct{})
	defer close(stop)

	// Every fenced body is invalid AND different, which is exactly the shape
	// that defeated the ban: the call is byte-identical as the model wrote it,
	// and the args it is judged on change every turn.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "]]")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: unmatched ']'"}
			}
			json.NewEncoder(w).Encode(out)
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(raw), "single fenced block") {
			n := fences
			fences++
			body := fmt.Sprintf("```python\nVALUE = %d]]\n```", n)
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": body}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
			return
		}
		i := turns
		turns++
		args := map[string]string{"path": "solve.py", "content": "@fenced"}
		if i == 0 {
			// Establish a healthy, session-owned file first. Without it the
			// broken writes are NEW-file writes, which land with a warning
			// rather than being refused — and a call that did not fail is not
			// the ban's business.
			args = map[string]string{"path": "solve.py",
				"content": "def solve():\n    return 1\n"}
		}
		call, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "write_file", "args": args})
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	ctx := m1Ctx(t, dir, srv.URL, census, terminal)
	// The ban's own signal. It fires one occurrence earlier than the repeat
	// detector, so it is the only way to observe it separately.
	banBounces := 0
	inner := ctx.StreamFn
	ctx.StreamFn = func(eventType string, data interface{}) {
		if b, _ := json.Marshal(data); strings.Contains(string(b), "byte for byte") {
			banBounces++
		}
		inner(eventType, data)
	}
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}
	t.Logf("turns=%d fenced=%d ban_bounces=%d status=%q reason=%q", turns, fences,
		banBounces, terminal["status"], terminal["reason"])

	// The behaviour under test is the ECONOMY, not which mechanism enforces
	// it: a model re-sending one fenced intent must not spend a fresh
	// generation every turn, and must not run on indefinitely.
	//
	// M1 made the retry ban read the raw intent so it would recognise the
	// re-send. That answer could not survive C4: an executed write is now
	// identified by the bytes it proposes, because a channel returning
	// DIFFERENT bytes each turn was having those bytes discarded unevaluated.
	// Here every fetched body fails the same way, so the rejection class never
	// changes, the consecutive-failure streak is never reset, and the
	// path-aware breaker ends the run on the same turn the ban used to --
	// which is the discrimination that was wanted all along: one failure
	// repeated is stuck, three different failures are progress.
	if banBounces != 0 {
		t.Logf("the ban also fired %d time(s)", banBounces)
	}
	switch terminal["reason"] {
	case "same_target_failures", "repeated_refusal", "repeat_detector", "failure_ceiling":
	default:
		t.Errorf("reason=%q — repeated channel use is no longer bounded by any "+
			"convergence mechanism", terminal["reason"])
	}

	// The run does not spend its whole budget re-sending it.
	if turns > 10 {
		t.Errorf("%d turns of an identical rejected intent before the run ended", turns)
	}
	// Fewer generations than turns: the later re-sends are answered without
	// opening the channel again. This is M1's payload and it is unchanged.
	if fences >= turns {
		t.Errorf("%d generations for %d turns — the re-send still paid for a "+
			"fresh generation every time", fences, turns)
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("a run whose every write was refused reported %q", terminal["status"])
	}
	// The healthy bytes written on the first turn are still there: every
	// broken rewrite after them was refused before disk.
	got, err := os.ReadFile(filepath.Join(dir, "solve.py"))
	if err != nil {
		t.Fatalf("the healthy write did not land: %v", err)
	}
	if string(got) != "def solve():\n    return 1\n" {
		t.Errorf("a refused rewrite reached disk: %q", got)
	}
}

// The three sites that make up the ban must agree on the key, or a recorded
// rejection can never be found again.
func TestRetryBanKeysOnIntentAcrossRecordLookupAndClear(t *testing.T) {
	ctx := &AgentContext{}
	intent := json.RawMessage(`{"path":"solve.py","content":"@fenced"}`)
	resolved := json.RawMessage(`{"path":"solve.py","content":"VALUE = 1\n"}`)
	alias := json.RawMessage(`{"path":"./solve.py","content":"@fenced"}`)

	recordFailedToolCall(ctx, "write_file", intent, "rejected: would not parse")
	if identicalRetryRefusal(ctx, "write_file", intent) == "" {
		t.Fatal("a recorded rejection is not found under its own intent")
	}
	if identicalRetryRefusal(ctx, "write_file", alias) == "" {
		t.Error("./solve.py evaded the ban recorded for solve.py")
	}
	if identicalRetryRefusal(ctx, "write_file", resolved) != "" {
		t.Error("the resolved body matched an intent-keyed record; the two " +
			"representations must not be interchangeable")
	}
	clearFailedToolCall(ctx, "write_file", alias)
	if identicalRetryRefusal(ctx, "write_file", intent) != "" {
		t.Error("clearing under an equivalent spelling did not clear the record")
	}

	// A materially different inline write is a different call.
	a := json.RawMessage(`{"path":"solve.py","content":"def f():\n    return 1\n"}`)
	b := json.RawMessage(`{"path":"solve.py","content":"def f():\n    return 2\n"}`)
	recordFailedToolCall(ctx, "write_file", a, "rejected")
	if identicalRetryRefusal(ctx, "write_file", b) != "" {
		t.Error("a genuine revision was refused as an identical re-send")
	}
	// And a different target keeps its own record.
	other := json.RawMessage(`{"path":"other.py","content":"@fenced"}`)
	if identicalRetryRefusal(ctx, "write_file", other) != "" {
		t.Error("a different path inherited another path's ban")
	}
}

// --- Phase 4B: the C5 recovery transition -----------------------------------
//
// The observed state: a warned file on disk, the run-first gate demanding it be
// run, and the model answering with the identical raw @fenced write. The gate
// used to repeat its demand until the bounce budget ran out. Now the recurrence
// hands back the file as it actually is, once, and holds the useless call back.

// c5Stub scripts a model through the C5 state. `plan` decides what the model
// sends on each turn; the fenced sub-call returns `body`, which the caller
// changes to make the broken and fixed versions.
func c5Stub(t *testing.T, dir string, turns, fences *int, ran *int,
	plan func(i int, prompt string) map[string]interface{}, body func(n int) string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "]]")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: unmatched ']' (line 2)"}
			}
			json.NewEncoder(w).Encode(out)
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(raw), "single fenced block") {
			n := *fences
			*fences++
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": body(n)}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
			return
		}
		i := *turns
		*turns++
		call, _ := json.Marshal(plan(i, string(raw)))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
}

const (
	c5Broken = "def solve():\n    return [1, 2]]\n"
	c5Fixed  = "def solve():\n    return [1, 2]  # fixed_marker\n"
)

func c5Ctx(t *testing.T, dir, url string, census map[string]int,
	terminal map[string]string, bounces *[]string) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = url, url, url
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 16
	var mu sync.Mutex
	ctx.StreamFn = func(eventType string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		census[eventType]++
		// Both, tagged. bounceToolCall emits a gate AND the tool_result that
		// answers the call: the gate's reason is truncated for the event
		// stream, so counting happens on the gate and the text is read from
		// the result the model actually receives.
		if eventType == "gate" || eventType == "tool_result" || eventType == "tool_call" {
			*bounces = append(*bounces, eventType+"|"+string(b))
		}
		if eventType == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	return ctx
}

// THE MANDATORY FIXTURE. The model enters the C5 state, is given the file, then
// does something useful with it and finishes with bytes that validate.
func TestC5RecoveryReachesAVerifiedCompletion(t *testing.T) {
	dir := t.TempDir()
	turns, fences, ran := 0, 0, 0
	srv := c5Stub(t, dir, &turns, &fences, &ran,
		// The scripted model is CONDITIONAL, and that is the whole point. It
		// repeats the same whole-file write until it can actually see the
		// file; once the source is in front of it, it runs the code and then
		// sends a targeted correction. A model that would have corrected
		// itself anyway proves nothing about recovery.
		func(i int, prompt string) map[string]interface{} {
			sawSource := strings.Contains(prompt, "twice without anything changing on disk")
			ranIt := strings.Contains(prompt, "ran_solve_marker")
			// A token rather than the source text: the model's own tool call is
			// embedded as a JSON string inside the request body, so the content
			// arrives double-escaped and matching it literally is escape-depth
			// guesswork.
			sentFix := strings.Contains(prompt, "fixed_marker")
			reran := strings.Contains(prompt, "reran_marker")
			switch {
			case !sawSource:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
			case !ranIt:
				// The command the recovery itself suggested. Pass or fail,
				// running it is what clears the run-first demand.
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "echo ran_solve_marker; python3 solve.py"}}
			case !sentFix:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "solve.py", "content": c5Fixed}}
			case !reran:
				// The last run of solve.py failed. Until the corrected file
				// runs, nothing shows it works, and completion waits for that.
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "echo reran_marker; python3 solve.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "fixed the bracket in solve.py"}
			}
		},
		func(n int) string { return "```python\n" + c5Broken + "```" })
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	var bounces []string
	ctx := c5Ctx(t, dir, srv.URL, census, terminal, &bounces)
	if err := runAgentLoop(ctx, "Write solve.py that returns the list."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	recoveries, recovery := 0, ""
	for _, b := range bounces {
		if !strings.Contains(b, "twice without anything changing on disk") {
			continue
		}
		if strings.HasPrefix(b, "gate|") {
			recoveries++
		} else {
			recovery = b // the untruncated text the model receives
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	t.Logf("turns=%d fenced=%d recoveries=%d status=%q reason=%q disk=%q",
		turns, fences, recoveries, terminal["status"], terminal["reason"], string(got))
	for _, b := range bounces {
		if strings.HasPrefix(b, "tool_call|") {
			t.Logf("  %.130s", b)
		}
	}

	// 1. Recovery fired, exactly once.
	if recoveries != 1 {
		t.Fatalf("recovery fired %d times, want exactly 1", recoveries)
	}
	// 2. The bounded source was supplied, numbered, with the guidance.
	if recovery == "" {
		t.Fatal("the recovery never reached the model as a tool result")
	}
	for _, want := range []string{"solve.py", "return [1, 2]]", "run_command", "read_file"} {
		if !strings.Contains(recovery, want) {
			t.Errorf("the recovery context is missing %q", want)
		}
	}
	// 3. THE POINT: a genuinely completed terminal over validated bytes.
	if terminal["status"] != string(TerminalCompleted) {
		t.Fatalf("recovery did not reach a verified completion: status=%q reason=%q summary=%s",
			terminal["status"], terminal["reason"], terminal["summary"])
	}
	if string(got) != c5Fixed {
		t.Errorf("disk = %q, want the corrected file", got)
	}
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Fatal("the ledger does not describe the final bytes")
	}
	if k, s := d.CurrentValidation(); s != ValidationPassed || k != ValidationKindSyntax {
		t.Errorf("completion authorized over %v/%v rather than a current syntax pass", k, s)
	}
	// 4. Nothing was run for the model: the one command was its own.
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result invariant broken: %d vs %d",
			census["tool_call"], census["tool_result"])
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

// A model that answers the recovery with the same blocked intent gets an
// honest stop, and the bytes that were there are still there.
func TestC5RecoveryThatIsIgnoredStopsHonestly(t *testing.T) {
	dir := t.TempDir()
	turns, fences, ran := 0, 0, 0
	srv := c5Stub(t, dir, &turns, &fences, &ran,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
		},
		func(n int) string { return "```python\n" + c5Broken + "```" })
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	var bounces []string
	ctx := c5Ctx(t, dir, srv.URL, census, terminal, &bounces)
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatal(err)
	}
	fencedAtRecovery := fences
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	t.Logf("turns=%d fenced=%d status=%q reason=%q", turns, fencedAtRecovery,
		terminal["status"], terminal["reason"])

	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("an ignored recovery reported %q", terminal["status"])
	}
	if completionClaimIn(terminal["summary"]) != "" {
		t.Errorf("the terminal claimed success:\n%s", terminal["summary"])
	}
	// The blocked repeats opened no channel: fewer generations than turns.
	if fences >= turns {
		t.Errorf("%d generations across %d turns — blocked repeats still paid for one",
			fences, turns)
	}
	if string(got) != c5Broken {
		t.Errorf("the bytes on disk changed under an ignored recovery: %q", got)
	}
}

// One recovery per canonical path: a different spelling is the same file.
func TestC5RecoveryIsNotResetByAliasOrTurn(t *testing.T) {
	dir := t.TempDir()
	turns, fences, ran := 0, 0, 0
	spellings := []string{"solve.py", "./solve.py"}
	srv := c5Stub(t, dir, &turns, &fences, &ran,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{
					"path": spellings[i%len(spellings)], "content": "@fenced"}}
		},
		func(n int) string { return "```python\n" + c5Broken + "```" })
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	var bounces []string
	ctx := c5Ctx(t, dir, srv.URL, census, terminal, &bounces)
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatal(err)
	}
	recoveries := 0
	for _, b := range bounces {
		if strings.HasPrefix(b, "gate|") &&
			strings.Contains(b, "twice without anything changing on disk") {
			recoveries++
		}
	}
	t.Logf("turns=%d recoveries=%d status=%q", turns, recoveries, terminal["status"])
	if recoveries > 1 {
		t.Errorf("alias spellings bought %d recoveries", recoveries)
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("status %q", terminal["status"])
	}
}

// Two files are two problems and keep independent recovery state.
func TestC5RecoveryIsPerPath(t *testing.T) {
	dir := t.TempDir()
	turns, fences, ran := 0, 0, 0
	// Three attempts each: land a warned version, take the gate's bounce,
	// then reach the recurrence that recovery answers.
	paths := []string{"a.py", "a.py", "a.py", "b.py", "b.py", "b.py"}
	srv := c5Stub(t, dir, &turns, &fences, &ran,
		func(i int, _ string) map[string]interface{} {
			if i >= len(paths) {
				return map[string]interface{}{"type": "done", "summary": "stopping"}
			}
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": paths[i], "content": "@fenced"}}
		},
		func(n int) string { return "```python\n" + c5Broken + "```" })
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	var bounces []string
	ctx := c5Ctx(t, dir, srv.URL, census, terminal, &bounces)
	if err := runAgentLoop(ctx, "Write two files."); err != nil {
		t.Fatal(err)
	}
	perPath := map[string]int{}
	for _, b := range bounces {
		if !strings.HasPrefix(b, "gate|") ||
			!strings.Contains(b, "twice without anything changing on disk") {
			continue
		}
		for _, p := range []string{"a.py", "b.py"} {
			if strings.Contains(b, p) {
				perPath[p]++
			}
		}
	}
	t.Logf("recoveries per path: %v", perPath)
	for _, p := range []string{"a.py", "b.py"} {
		if perPath[p] != 1 {
			t.Errorf("%s got %d recoveries, want exactly 1", p, perPath[p])
		}
	}
}

// With the budget nearly gone, recovery is skipped and the run stops honestly
// rather than spending what is left on context nobody can act on.
func TestC5RecoverySkippedWhenTheBudgetIsGone(t *testing.T) {
	dir := t.TempDir()
	turns, fences, ran := 0, 0, 0
	srv := c5Stub(t, dir, &turns, &fences, &ran,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
		},
		func(n int) string { return "```python\n" + c5Broken + "```" })
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	var bounces []string
	ctx := c5Ctx(t, dir, srv.URL, census, terminal, &bounces)
	// A work deadline inside the recovery floor.
	workCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ctx.Ctx = workCtx

	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatal(err)
	}
	for _, b := range bounces {
		if strings.HasPrefix(b, "gate|") &&
			strings.Contains(b, "twice without anything changing on disk") {
			t.Fatal("recovery ran with less budget than it needs to be acted on")
		}
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("status %q", terminal["status"])
	}
}

// --- The turn-level fenced loop ---------------------------------------------
//
// debounce2 in the frozen run made 149 main-loop LLM calls and 147 identical
// write_file attempts for one path, and opened only 4 fenced sub-generations:
// the session/path allowance worked, refusing 144 of them before any
// generation. What nothing bounded was the TURNS. The refusal at the fenced
// bounce continues past the failed-call counters, the retry ban and the
// repetition window, so no breaker ever saw the resend and the run spent its
// whole budget on it.
//
// The same defect was found twice before, at the per-path tool ban and at the
// retry ban; both branches were repaired by counting the bounce as the failure
// it is. This is the third instance.

// fencedLoopFixture drives the real loop with MaxTurns=0, as production runs
// it. The server itself enforces the bound, so nothing here sleeps.
func fencedLoopFixture(t *testing.T, dir string, maxTurns int,
	plan func(i int, prompt string) map[string]interface{}) (*AgentContext, *int, *int, map[string]int, map[string]string) {
	t.Helper()
	turns, subcalls := 0, 0
	census := map[string]int{}
	terminal := map[string]string{}
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "]]")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: unmatched ']'"}
			}
			json.NewEncoder(w).Encode(out)
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(raw), "single fenced block") {
			// The exact production shape: a fast reply with NO fenced block.
			mu.Lock()
			subcalls++
			mu.Unlock()
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": "Sure, here is the file."}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
			return
		}
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= maxTurns {
			// The bound lives in the server so the test never sleeps: past it
			// the loop is unbounded and the fixture says so immediately.
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i, string(raw)))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 0 // production
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		census[et]++
		if et == "gate" || et == "tool_result" {
			census["_text:"+string(b[:min(len(b), 0)])] += 0
		}
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	return ctx, &turns, &subcalls, census, terminal
}

const fencedTurnCeiling = 30

func TestRepeatedFencedFailureReachesABoundedTerminal(t *testing.T) {
	dir := t.TempDir()
	ctx, turns, subcalls, census, terminal := fencedLoopFixture(t, dir, fencedTurnCeiling,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
		})
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	t.Logf("main-loop turns=%d fenced sub-generations=%d (ctx.FencedCalls=%d) status=%q reason=%q",
		*turns, *subcalls, ctx.FencedCalls, terminal["status"], terminal["reason"])

	if *turns >= fencedTurnCeiling {
		t.Fatalf("the loop consumed %d main-loop turns without a terminal; production "+
			"runs uncapped and would spend the whole session budget here", *turns)
	}
	if census["done"] != 1 {
		t.Fatalf("%d terminal events", census["done"])
	}
	st := NormalizeTerminalStatus(terminal["status"])
	if st != TerminalStopped && st != TerminalIncomplete {
		t.Errorf("terminal status = %q, want an honest stop", terminal["status"])
	}
	if st.Completed() {
		t.Error("a run that wrote nothing reported completion")
	}
	// The allowance is untouched by the fix: still at most two sub-generations.
	if ctx.FencedCalls > maxFencedFailuresPerPath {
		t.Errorf("%d fenced sub-generations, allowance is %d",
			ctx.FencedCalls, maxFencedFailuresPerPath)
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result invariant broken: %d vs %d",
			census["tool_call"], census["tool_result"])
	}
	if _, err := os.Stat(filepath.Join(dir, "solve.py")); err == nil {
		t.Error("a run whose every resolution failed still mutated the target")
	}
}

// Two spellings of one file are one failure identity.
func TestFencedFailureAliasesShareOneIdentity(t *testing.T) {
	dir := t.TempDir()
	spellings := []string{"solve.py", "./solve.py"}
	ctx, turns, _, census, terminal := fencedLoopFixture(t, dir, fencedTurnCeiling,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{
					"path": spellings[i%len(spellings)], "content": "@fenced"}}
		})
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatal(err)
	}
	t.Logf("alias run: turns=%d status=%q reason=%q", *turns, terminal["status"], terminal["reason"])
	if *turns >= fencedTurnCeiling {
		t.Errorf("alternating spellings evaded the bound: %d turns", *turns)
	}
	if census["done"] != 1 || NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("terminal: %d events, status=%q", census["done"], terminal["status"])
	}
}

// Distinct targets keep independent budgets and must not be collapsed.
func TestFencedFailurePathsStayIndependent(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"a.py", "b.py", "c.py", "d.py"}
	ctx, turns, subcalls, _, terminal := fencedLoopFixture(t, dir, fencedTurnCeiling,
		func(i int, _ string) map[string]interface{} {
			if i >= len(paths) {
				return map[string]interface{}{"type": "done", "summary": "gave up"}
			}
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": paths[i], "content": "@fenced"}}
		})
	if err := runAgentLoop(ctx, "Write four files."); err != nil {
		t.Fatal(err)
	}
	t.Logf("independent paths: turns=%d sub-generations=%d status=%q",
		*turns, *subcalls, terminal["status"])
	// Each distinct path is entitled to its own allowance, so four paths must
	// not be stopped as if they were one target repeated.
	if *turns < len(paths) {
		t.Errorf("only %d turns for %d distinct targets — the paths were collapsed",
			*turns, len(paths))
	}
	// Each path is entitled to its own allowance: four targets, two
	// sub-generations each, and no target starved by another's failures.
	if *subcalls != len(paths)*maxFencedFailuresPerPath {
		t.Errorf("%d sub-generations for %d paths, want %d",
			*subcalls, len(paths), len(paths)*maxFencedFailuresPerPath)
	}
	// The terminal here is whatever the existing completion policy decides for
	// a run that declared nothing and wrote nothing; this slice does not
	// touch that rule, so it is logged rather than asserted.
	t.Logf("terminal for the four-path run: status=%q reason=%q",
		terminal["status"], terminal["reason"])
}

// --- Recovery for the exhausted fenced channel ------------------------------
//
// The allowance stops the generations and tells the model nothing it can act
// on. This offers the way out once, before anything tries another resolution.

const fencedRecoveryMark = "fenced-content channel for"

func TestFencedChannelRecoveryReachesAVerifiedCompletion(t *testing.T) {
	dir := t.TempDir()
	const fixed = "def solve():\n    return 7  # inline_fix\n\nprint(solve())\n"
	var bounces []string
	var mu sync.Mutex

	ctx, turns, subcalls, census, terminal := fencedLoopFixture(t, dir, fencedTurnCeiling,
		// The scripted model is CONDITIONAL: it keeps asking for the fenced
		// channel until it is told the channel is spent, and only then picks
		// an allowed alternative. A model that would have switched anyway
		// proves nothing about recovery.
		func(i int, prompt string) map[string]interface{} {
			sawRecovery := strings.Contains(prompt, fencedRecoveryMark)
			sentInline := strings.Contains(prompt, "inline_fix")
			ranIt := strings.Contains(prompt, "ran_marker")
			switch {
			case !sawRecovery:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
			case !sentInline:
				// The alternative the recovery named: the whole file inline.
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "solve.py", "content": fixed}}
			case !ranIt:
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "echo ran_marker; python3 solve.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "wrote solve.py inline and ran it"}
			}
		})
	inner := ctx.StreamFn
	ctx.StreamFn = func(et string, data interface{}) {
		if et == "gate" || et == "tool_result" {
			b, _ := json.Marshal(data)
			mu.Lock()
			bounces = append(bounces, et+"|"+string(b))
			mu.Unlock()
		}
		inner(et, data)
	}
	if err := runAgentLoop(ctx, "Create solve.py."); err != nil {
		t.Fatalf("loop error: %v", err)
	}

	recoveries, recoveryText := 0, ""
	for _, b := range bounces {
		if !strings.Contains(b, fencedRecoveryMark) {
			continue
		}
		if strings.HasPrefix(b, "gate|") {
			recoveries++
		} else {
			recoveryText = b
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	t.Logf("turns=%d sub-generations=%d recoveries=%d status=%q reason=%q disk=%q",
		*turns, *subcalls, recoveries, terminal["status"], terminal["reason"], string(got))

	if recoveries != 1 {
		t.Fatalf("recovery fired %d times, want exactly 1", recoveries)
	}
	if recoveryText == "" {
		t.Fatal("the recovery never reached the model as a tool result")
	}
	for _, want := range []string{"solve.py", "write_file", "edit_file", "read_file", "run_command"} {
		if !strings.Contains(recoveryText, want) {
			t.Errorf("the recovery context never names %q", want)
		}
	}
	// THE POINT: a genuinely completed terminal over validated bytes.
	if terminal["status"] != string(TerminalCompleted) {
		t.Fatalf("recovery did not reach a verified completion: status=%q reason=%q summary=%s",
			terminal["status"], terminal["reason"], terminal["summary"])
	}
	if terminal["reason"] != "deliverables_demonstrated" {
		t.Errorf("completion reason = %q", terminal["reason"])
	}
	if string(got) != fixed {
		t.Errorf("disk = %q, want the inline correction", got)
	}
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Fatal("the ledger does not describe the final bytes")
	}
	if k, s := d.CurrentValidation(); s != ValidationPassed || k != ValidationKindSyntax {
		t.Errorf("completion authorized over %v/%v rather than a current syntax pass", k, s)
	}
	// The allowance is unchanged and recovery started nothing.
	if *subcalls > maxFencedFailuresPerPath {
		t.Errorf("%d sub-generations, allowance is %d", *subcalls, maxFencedFailuresPerPath)
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result invariant broken: %d vs %d", census["tool_call"], census["tool_result"])
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

func TestFencedChannelRecoveryIgnoredStopsHonestly(t *testing.T) {
	dir := t.TempDir()
	ctx, turns, subcalls, census, terminal := fencedLoopFixture(t, dir, fencedTurnCeiling,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
		})
	if err := runAgentLoop(ctx, "Create solve.py."); err != nil {
		t.Fatal(err)
	}
	t.Logf("ignored: turns=%d sub-generations=%d status=%q reason=%q",
		*turns, *subcalls, terminal["status"], terminal["reason"])
	if *turns >= fencedTurnCeiling {
		t.Errorf("%d turns — recovery did not stay bounded", *turns)
	}
	st := NormalizeTerminalStatus(terminal["status"])
	if st.Completed() {
		t.Errorf("an ignored recovery reported %q", terminal["status"])
	}
	if completionClaimIn(terminal["summary"]) != "" {
		t.Errorf("the terminal claimed success:\n%s", terminal["summary"])
	}
	// Recovery launched no generation of its own.
	if *subcalls > maxFencedFailuresPerPath {
		t.Errorf("%d sub-generations after recovery, allowance is %d",
			*subcalls, maxFencedFailuresPerPath)
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

// The offer is made once per canonical path, whatever spelling arrives, and
// two different files get their own.
func TestFencedChannelRecoveryIdentityAndScope(t *testing.T) {
	t.Run("aliases cannot reset it", func(t *testing.T) {
		dir := t.TempDir()
		spellings := []string{"solve.py", "./solve.py"}
		var seen int
		var mu sync.Mutex
		ctx, _, _, _, _ := fencedLoopFixture(t, dir, fencedTurnCeiling,
			func(i int, _ string) map[string]interface{} {
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{
						"path": spellings[i%len(spellings)], "content": "@fenced"}}
			})
		inner := ctx.StreamFn
		ctx.StreamFn = func(et string, data interface{}) {
			if et == "gate" {
				if b, _ := json.Marshal(data); strings.Contains(string(b), fencedRecoveryMark) {
					mu.Lock()
					seen++
					mu.Unlock()
				}
			}
			inner(et, data)
		}
		if err := runAgentLoop(ctx, "Create solve.py."); err != nil {
			t.Fatal(err)
		}
		if seen != 1 {
			t.Errorf("alias spellings produced %d recoveries, want 1", seen)
		}
	})

	t.Run("separate paths get their own", func(t *testing.T) {
		dir := t.TempDir()
		paths := []string{"a.py", "a.py", "a.py", "b.py", "b.py", "b.py"}
		perPath := map[string]int{}
		var mu sync.Mutex
		ctx, _, _, _, _ := fencedLoopFixture(t, dir, fencedTurnCeiling,
			func(i int, _ string) map[string]interface{} {
				if i >= len(paths) {
					return map[string]interface{}{"type": "done", "summary": "stopping"}
				}
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": paths[i], "content": "@fenced"}}
			})
		inner := ctx.StreamFn
		ctx.StreamFn = func(et string, data interface{}) {
			if et == "gate" {
				b, _ := json.Marshal(data)
				if strings.Contains(string(b), fencedRecoveryMark) {
					mu.Lock()
					for _, p := range []string{"a.py", "b.py"} {
						if strings.Contains(string(b), p) {
							perPath[p]++
						}
					}
					mu.Unlock()
				}
			}
			inner(et, data)
		}
		if err := runAgentLoop(ctx, "Create two files."); err != nil {
			t.Fatal(err)
		}
		t.Logf("recoveries per path: %v", perPath)
		for _, p := range []string{"a.py", "b.py"} {
			if perPath[p] != 1 {
				t.Errorf("%s got %d recoveries, want exactly 1", p, perPath[p])
			}
		}
	})

	t.Run("absent target invents no source", func(t *testing.T) {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		st := &runState{}
		ctx.FencedFailures = map[string]int{fencedKey(ctx, "missing.py"): maxFencedFailuresPerPath}
		msg := fencedChannelRecovery(ctx, st, "missing.py")
		if msg == "" {
			t.Fatal("no recovery offered for an exhausted path")
		}
		if !strings.Contains(msg, "not on disk yet") {
			t.Errorf("absent target not disclosed:\n%s", msg)
		}
		if strings.Contains(msg, "currently contains") {
			t.Errorf("source was invented for a file that is not there:\n%s", msg)
		}
	})

	t.Run("budget too small to act on", func(t *testing.T) {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		st := &runState{}
		ctx.FencedFailures = map[string]int{fencedKey(ctx, "solve.py"): maxFencedFailuresPerPath}
		workCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		ctx.Ctx = workCtx
		if msg := fencedChannelRecovery(ctx, st, "solve.py"); msg != "" {
			t.Error("recovery ran with less budget than it needs to be acted on")
		}
	})

	t.Run("not offered while the allowance remains", func(t *testing.T) {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		st := &runState{}
		if msg := fencedChannelRecovery(ctx, st, "solve.py"); msg != "" {
			t.Error("recovery offered before the channel was spent")
		}
		ctx.FencedFailures = map[string]int{fencedKey(ctx, "solve.py"): 1}
		if msg := fencedChannelRecovery(ctx, st, "solve.py"); msg != "" {
			t.Error("recovery offered with allowance remaining")
		}
	})
}

// The alternatives the recovery names must actually work after the channel is
// spent: a targeted edit, and simply verifying an artifact that is already
// correct rather than rewriting it.
func TestAllowedAlternativesAfterFencedExhaustion(t *testing.T) {
	t.Run("a materially different edit lands", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "solve.py"),
			[]byte("def solve():\n    return 1\n\nprint(solve())\n"), 0o644)
		// Steps are scripted rather than sniffed from the prompt: the model
		// still only STARTS them once it has been told the channel is spent,
		// which is the conditional part that matters.
		step := 0
		ctx, turns, subcalls, _, terminal := fencedLoopFixture(t, dir, fencedTurnCeiling,
			func(i int, prompt string) map[string]interface{} {
				if !strings.Contains(prompt, fencedRecoveryMark) {
					return map[string]interface{}{"type": "tool_call", "name": "write_file",
						"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
				}
				step++
				switch step {
				case 1:
					return map[string]interface{}{"type": "tool_call", "name": "read_file",
						"args": map[string]string{"path": "solve.py"}}
				case 2:
					return map[string]interface{}{"type": "tool_call", "name": "edit_file",
						"args": map[string]string{"path": "solve.py",
							"old_str": "return 1", "new_str": "return 42"}}
				case 3:
					return map[string]interface{}{"type": "tool_call", "name": "run_command",
						"args": map[string]string{"command": "python3 solve.py"}}
				default:
					return map[string]interface{}{"type": "done", "summary": "changed the return value"}
				}
			})
		if err := runAgentLoop(ctx, "Change solve.py to return 42."); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
		t.Logf("edit route: turns=%d sub-generations=%d status=%q disk=%q",
			*turns, *subcalls, terminal["status"], string(got))
		if !strings.Contains(string(got), "return 42") {
			t.Errorf("the targeted edit never landed: %q", got)
		}
		if *subcalls > maxFencedFailuresPerPath {
			t.Errorf("%d sub-generations after exhaustion", *subcalls)
		}
	})

	t.Run("an already-correct artifact is verified, not rewritten", func(t *testing.T) {
		dir := t.TempDir()
		const good = "def solve():\n    return 3\n\nprint(solve())\n"
		os.WriteFile(filepath.Join(dir, "solve.py"), []byte(good), 0o644)
		before := hashBytes([]byte(good))
		ctx, turns, subcalls, _, terminal := fencedLoopFixture(t, dir, fencedTurnCeiling,
			func(i int, prompt string) map[string]interface{} {
				saw := strings.Contains(prompt, fencedRecoveryMark)
				ran := strings.Contains(prompt, "ran_marker")
				switch {
				case !saw:
					return map[string]interface{}{"type": "tool_call", "name": "write_file",
						"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
				case !ran:
					// The recovery showed the file; it is already right, so
					// the model verifies instead of rewriting.
					return map[string]interface{}{"type": "tool_call", "name": "run_command",
						"args": map[string]string{"command": "echo ran_marker; python3 solve.py"}}
				default:
					return map[string]interface{}{"type": "done", "summary": "solve.py already returns 3; ran it"}
				}
			})
		if err := runAgentLoop(ctx, "Make sure solve.py prints 3."); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
		t.Logf("verify route: turns=%d sub-generations=%d status=%q reason=%q",
			*turns, *subcalls, terminal["status"], terminal["reason"])
		if hashBytes(got) != before {
			t.Errorf("a correct artifact was rewritten: %q", got)
		}
		if *subcalls > maxFencedFailuresPerPath {
			t.Errorf("%d sub-generations after exhaustion", *subcalls)
		}
		if NormalizeTerminalStatus(terminal["status"]) == TerminalUnknown {
			t.Error("no classified terminal")
		}
	})
}

// --- C4: retry identity for an executed write is the proposal, not the intent
//
// `content:"@fenced"` is the same seven bytes every time the model sends it.
// The sub-call that resolves it returns DIFFERENT bytes each time, and the
// resend ban reads the raw intent, so the second and third proposals were
// fetched -- a generation each -- and discarded before any syntax gate looked
// at them. Measured on a session-owned file whose current bytes are exact-hash
// syntax/passed: three fenced fetches, one proposal evaluated, and a valid
// fourth body queued behind a ban it never reached.
//
// Channel identity stays raw: the fenced allowance and the repeat detector both
// exist to bound a model re-sending one call, and that is what the raw bytes
// say. Only the identity of an EXECUTED write_file moves to the proposal.

const (
	c4Valid = "def solve():\n    total = 0\n    for i in range(3):\n        total += i\n    return total\n\n\nprint(solve())\n"
	c4BadA  = "def solve():\n    total = 0\n    for i in range(3:\n        total += i\n    return total\n\n\nprint(solve())\n"
	c4BadB  = "def solve():\n    vals = [1, 2]]\n    return sum(vals)\n\n\nprint(solve())\n"
	c4BadC  = "def solve():\n    d = {'k': 1\n    return d\n\n\nprint(solve())\n"
	c4Fixed = "def solve():\n    return 7\n\n\nprint(solve())\n"
)

func c4Syntax(code string) string {
	cmd := exec.Command("python3", "-c",
		"import ast,sys\ntry:\n ast.parse(sys.stdin.read())\nexcept SyntaxError as e:\n sys.stdout.write('%s (line %d)' % (e.msg, e.lineno or 0))")
	cmd.Stdin = strings.NewReader(code)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// syntaxCheck is one call the write path made to the checker, identified by
// the exact bytes it asked about. Proposal checks and baseline checks both
// arrive here, and the hash is what tells them apart.
type syntaxCheck struct {
	Hash   string
	Valid  bool
	Detail string
}

type c4Run struct {
	ctx        *AgentContext
	dir        string
	turns      int
	fetches    int
	census     map[string]int
	terminal   map[string]string
	refusals   []string // syntax-gate diagnostics, in order
	gates      []string
	calls      []string
	writes     int // successful write_file results
	checks     []syntaxCheck
	recoveries []string
}

// c4Fixture runs the real loop with a genuine Python syntax check. fencedBodies
// feeds the @fenced sub-call in order.
func c4Fixture(t *testing.T, fencedBodies []string,
	plan func(i int, r *c4Run) map[string]interface{}) *c4Run {
	t.Helper()
	r := &c4Run{dir: t.TempDir(), census: map[string]int{}, terminal: map[string]string{}}
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasPrefix(req.URL.Path, "/v3/"), strings.HasPrefix(req.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(req.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(req.Body).Decode(&in)
			d := c4Syntax(in.Code)
			mu.Lock()
			r.checks = append(r.checks, syntaxCheck{
				Hash: hashBytes([]byte(in.Code))[:12], Valid: d == "", Detail: d})
			mu.Unlock()
			if d != "" {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"valid": false, "errors": []string{d}})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(req.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(req.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(r.dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(req.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, req)
			return
		}
		raw, _ := io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(raw), "single fenced block") {
			mu.Lock()
			n := r.fetches
			r.fetches++
			mu.Unlock()
			body := "no block"
			if n < len(fencedBodies) {
				body = "```python\n" + fencedBodies[n] + "```"
			}
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{{"delta": map[string]string{"content": body}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
			return
		}
		mu.Lock()
		i := r.turns
		r.turns++
		mu.Unlock()
		if i >= 30 {
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		mu.Lock()
		call, _ := json.Marshal(plan(i, r))
		mu.Unlock()
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(srv.Close)

	r.ctx = NewAgentContext(r.dir, Tier2Medium)
	r.ctx.InferenceURL, r.ctx.SandboxURL, r.ctx.V3URL = srv.URL, srv.URL, srv.URL
	r.ctx.PermissionMode = PermissionYolo
	r.ctx.TrustMode = trustFullyTrusted
	r.ctx.VerifyOnHost = true
	r.ctx.MaxTurns = 0
	r.ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		r.census[et]++
		switch et {
		case "tool_result":
			var ok struct {
				Success bool
				Tool    string
			}
			if json.Unmarshal(b, &ok) == nil && ok.Success && ok.Tool == "write_file" {
				r.writes++
			}
			var tr struct{ Error string }
			if json.Unmarshal(b, &tr) == nil && strings.Contains(tr.Error, c4Marker) {
				r.recoveries = append(r.recoveries, tr.Error)
			}
			// Only a real syntax-gate evaluation counts. The resend ban
			// quotes the original refusal back, so it carries the same
			// phrase without any proposal having been looked at.
			if json.Unmarshal(b, &tr) == nil && strings.Contains(tr.Error, "it was NOT written") &&
				!strings.Contains(tr.Error, "byte for byte") &&
				!strings.Contains(tr.Error, "no longer available") {
				r.refusals = append(r.refusals, tr.Error)
			}
		case "gate":
			var g struct{ Gate, Reason string }
			if json.Unmarshal(b, &g) == nil {
				r.gates = append(r.gates, g.Gate+": "+g.Reason)
			}
		case "done":
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				r.terminal[k] = v
			}
		case "tool_call":
			r.calls = append(r.calls, string(b))
		}
	}
	runAgentLoop(r.ctx, "Write solve.py so it prints 7, then run it.")
	return r
}

// subject names the bytes a check was about, so a proposal check can be told
// from the baseline check the write path makes of what is already on disk.
func (r *c4Run) subject(hash string) string {
	for name, body := range map[string]string{
		"valid(known-good)": c4Valid, "BadA": c4BadA, "BadB": c4BadB,
		"BadC": c4BadC, "Fixed": c4Fixed,
	} {
		if hashBytes([]byte(body))[:12] == hash {
			return name
		}
	}
	return "other"
}

func (r *c4Run) disk(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(r.dir, "solve.py"))
	return string(b)
}

func (r *c4Run) log(t *testing.T, name string) {
	t.Helper()
	d := r.ctx.Ledger[ledgerKey(r.ctx, "solve.py")]
	kind, status := ValidationKindUnknown, ValidationUnknown
	hashOK := false
	if d != nil {
		kind, status = d.CurrentValidation()
		hashOK = d.CurrentHash == hashBytes([]byte(r.disk(t)))
	}
	pass, fail := 0, 0
	for _, c := range r.checks {
		if c.Valid {
			pass++
		} else {
			fail++
		}
	}
	t.Logf("%s: turns=%d fetches=%d checks=%d (pass=%d fail=%d) refusals=%d recoveries=%d status=%q reason=%q hash_matches=%v validation=%s/%s",
		name, r.turns, r.fetches, len(r.checks), pass, fail, len(r.refusals), len(r.recoveries),
		r.terminal["status"], r.terminal["reason"], hashOK, kind, status)
	for i, c := range r.checks {
		t.Logf("   check#%d %s %-5v %s [%s]", i+1, c.Hash, c.Valid, c.Detail, r.subject(c.Hash))
	}
}

func c4Write(path, content string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "write_file",
		"args": map[string]string{"path": path, "content": content}}
}

// Variant B: the raw intent never changes; the resolved proposals all differ.
// Every one of them must reach the syntax gate.
func TestFencedProposalsAreEvaluatedNotDeduplicatedByIntent(t *testing.T) {
	r := c4Fixture(t, []string{c4BadA, c4BadB, c4BadC, c4Fixed},
		func(i int, r *c4Run) map[string]interface{} {
			switch {
			case i == 0:
				return c4Write("solve.py", c4Valid)
			case r.writes < 2: // still trying to replace the known-good file
				return c4Write("solve.py", "@fenced")
			}
			return map[string]interface{}{"type": "done", "summary": "solve.py prints 7"}
		})
	r.log(t, "B")

	// Accounting, stated exactly. `refusals` counts REJECTED proposals only;
	// the checker sees more than that, and the distinction is the point.
	seen := map[string]bool{}
	var proposalFail, proposalPass int
	for _, c := range r.checks {
		if r.subject(c.Hash) == "valid(known-good)" {
			continue // the baseline the write path compares against
		}
		seen[c.Hash] = true
		if c.Valid {
			proposalPass++
		} else {
			proposalFail++
		}
	}
	if len(seen) != 4 {
		t.Errorf("%d distinct proposals reached the checker, want 4 (BadA, BadB, BadC, Fixed); "+
			"the rest were fetched and discarded on raw-intent identity", len(seen))
	}
	if proposalFail != 3 {
		t.Errorf("%d failed proposal checks, want 3", proposalFail)
	}
	if proposalPass == 0 {
		t.Error("the valid proposal was never checked")
	}
	if len(r.refusals) != 3 {
		t.Fatalf("%d rejected proposals, want 3", len(r.refusals))
	}
	// The valid proposal was checked BEFORE it was written: its passing check
	// precedes the successful write, and no write succeeded between the last
	// failing check and it.
	lastFail, firstPass := -1, -1
	for i, c := range r.checks {
		if r.subject(c.Hash) == "valid(known-good)" {
			continue
		}
		if !c.Valid {
			lastFail = i
		} else if firstPass < 0 {
			firstPass = i
		}
	}
	if firstPass < 0 || firstPass <= lastFail {
		t.Errorf("the accepted proposal was not checked after the last rejection "+
			"(lastFail=%d firstPass=%d)", lastFail, firstPass)
	}
	if hashBytes([]byte(r.disk(t)))[:12] != r.checks[firstPass].Hash {
		t.Error("the bytes on disk are not the bytes that passed the check")
	}
	// This fixture is also, incidentally, the C4 shape -- valid bytes on disk,
	// more than one replacement refused against them -- so the refused-
	// replacement recovery fires once here too. It is not what makes this run
	// complete: the model above advances on r.writes and has no dependence on
	// the recovery text at all, and the run completed identically before that
	// recovery existed. Bounded to one, as everywhere else.
	if n := len(r.recoveries); n > 1 {
		t.Errorf("%d recoveries in one generation, want at most one", n)
	}
	// Each evaluation describes its OWN bytes, as the checker reported them.
	for i, body := range []string{c4BadA, c4BadB, c4BadC} {
		want := c4Syntax(body)
		if want == "" {
			t.Fatalf("fixture body %d is not actually invalid", i+1)
		}
		if i < len(r.refusals) && !strings.Contains(r.refusals[i], want) {
			t.Errorf("evaluation %d does not describe its own proposal (want %q): %.140s",
				i+1, want, r.refusals[i])
		}
	}
	// No rejected proposal ever reached disk: what is there is either the
	// original known-good bytes or the valid correction.
	got := r.disk(t)
	if got != c4Valid && got != c4Fixed {
		t.Errorf("disk holds neither the known-good bytes nor the valid correction: %q", got)
	}
	if c4Syntax(got) != "" {
		t.Errorf("invalid bytes reached disk: %q", got)
	}
}

// Identity is the proposal: the same file bytes sent inline and through the
// fenced channel are one call; different bytes are not.
func TestProposalIdentityIsTheResolvedBytes(t *testing.T) {
	inline := json.RawMessage(`{"path":"solve.py","content":` + c4JSON(c4BadA) + `}`)
	resolved := json.RawMessage(`{"path":"solve.py","content":` + c4JSON(c4BadA) + `}`)
	alias := json.RawMessage(`{"path":"./solve.py","content":` + c4JSON(c4BadA) + `}`)
	other := json.RawMessage(`{"path":"solve.py","content":` + c4JSON(c4BadB) + `}`)
	bare := json.RawMessage(`{"path":"solve.py","content":"@fenced"}`)

	same := retryIdentityArgs("write_file", bare, resolved)
	if toolCallSignature("write_file", same) != toolCallSignature("write_file", inline) {
		t.Error("the same bytes inline and through the channel are not one call")
	}
	if toolCallSignature("write_file", same) != toolCallSignature("write_file", alias) {
		t.Error("path aliases do not share proposal identity")
	}
	if toolCallSignature("write_file", same) == toolCallSignature("write_file", other) {
		t.Error("materially different bytes share identity")
	}
	// Channel identity is still the raw intent, and other tools are untouched.
	if got := retryIdentityArgs("write_file", bare, nil); string(got) != string(bare) {
		t.Errorf("with nothing resolved the identity must stay the intent: %s", got)
	}
	ed := json.RawMessage(`{"path":"solve.py","old_str":"a","new_str":"b"}`)
	if got := retryIdentityArgs("edit_file", ed, resolved); string(got) != string(ed) {
		t.Errorf("edit_file identity moved: %s", got)
	}
}

func c4JSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The two bounds that must survive the identity change, each owned by a
// different mechanism.
func TestInvalidProposalsStayBounded(t *testing.T) {
	t.Run("identical resolved bytes hit the resend ban", func(t *testing.T) {
		r := c4Fixture(t, nil, func(i int, _ *c4Run) map[string]interface{} {
			if i == 0 {
				return c4Write("solve.py", c4Valid)
			}
			return c4Write("solve.py", c4BadA)
		})
		r.log(t, "A")
		if r.turns >= 30 {
			t.Fatalf("%d turns without a terminal", r.turns)
		}
		if r.terminal["reason"] != "repeated_refusal" {
			t.Errorf("reason=%q, want repeated_refusal", r.terminal["reason"])
		}
		if len(r.refusals) != 1 {
			t.Errorf("%d syntax evaluations of one unchanging proposal, want 1", len(r.refusals))
		}
		if r.disk(t) != c4Valid {
			t.Errorf("the known-good bytes did not survive: %q", r.disk(t))
		}
	})

	t.Run("endlessly changing invalid proposals hit the failure ceiling", func(t *testing.T) {
		bad := []string{c4BadA, c4BadB, c4BadC}
		r := c4Fixture(t, nil, func(i int, _ *c4Run) map[string]interface{} {
			if i == 0 {
				return c4Write("solve.py", c4Valid)
			}
			// Never repeats a proposal: a fresh trailing comment each time.
			return c4Write("solve.py",
				bad[(i-1)%len(bad)]+fmt.Sprintf("# attempt %d\n", i))
		})
		r.log(t, "B2")
		if r.turns >= 30 {
			t.Fatalf("%d turns without a terminal", r.turns)
		}
		if r.terminal["reason"] != "failure_ceiling" {
			t.Errorf("reason=%q, want failure_ceiling — the bound for proposals that never repeat",
				r.terminal["reason"])
		}
		if NormalizeTerminalStatus(r.terminal["status"]).Completed() {
			t.Errorf("a run that never landed a valid replacement reported %q", r.terminal["status"])
		}
		if r.disk(t) != c4Valid {
			t.Errorf("the known-good bytes did not survive: %q", r.disk(t))
		}
	})

	t.Run("invalid then valid still completes without ceremony", func(t *testing.T) {
		r := c4Fixture(t, nil, func(i int, _ *c4Run) map[string]interface{} {
			switch i {
			case 0:
				return c4Write("solve.py", c4Valid)
			case 1:
				return c4Write("solve.py", c4BadA)
			case 2:
				return c4Write("solve.py", c4Fixed)
			case 3:
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "python3 solve.py"}}
			}
			return map[string]interface{}{"type": "done", "summary": "solve.py prints 7"}
		})
		r.log(t, "C")
		if r.terminal["status"] != string(TerminalCompleted) {
			t.Errorf("terminal %q/%q, want completed",
				r.terminal["status"], r.terminal["reason"])
		}
		if r.disk(t) != c4Fixed {
			t.Errorf("the correction did not land: %q", r.disk(t))
		}
	})
}

// The repeat detector and the fenced allowance both count the model re-sending
// ONE call, and both still read the raw intent. Moving them to the proposal
// would make a channel that returns different bytes look like progress.
func TestChannelIdentityStaysRaw(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(src), "\n") {
		l := strings.TrimSpace(line)
		if strings.Contains(l, "recordToolCall(ctx,") && !strings.Contains(l, "intentArgs") {
			t.Errorf("the repeat detector no longer reads the raw intent: %s", l)
		}
		if strings.Contains(l, "fencedKey(ctx,") && strings.Contains(l, "retryIdentityArgs") {
			t.Errorf("the fenced allowance was moved off the raw target: %s", l)
		}
	}
	// And the ban's three operations agree with each other.
	var lookup, record, clear int
	for _, line := range strings.Split(string(src), "\n") {
		l := strings.TrimSpace(line)
		switch {
		case strings.Contains(l, "identicalRetryRefusal(ctx"):
			lookup++
		case strings.Contains(l, "recordFailedToolCall(ctx, parsed.Name,"):
			record++
		case strings.Contains(l, "clearFailedToolCall(ctx"):
			clear++
		}
	}
	t.Logf("FailedToolCalls operations: %d lookup, %d record, %d clear", lookup, record, clear)
	if lookup != 1 || clear != 1 {
		t.Errorf("expected one lookup and one clear, got %d/%d", lookup, clear)
	}
}

// Nothing about the bounds moved. The identity change is an identity change:
// no threshold, no counter reset, no allowance was introduced with it.
func TestC4IdentityChangeIntroducedNoAllowance(t *testing.T) {
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"maxTotalFailures", maxTotalFailures, 12},
		{"toolRepeatThreshold", toolRepeatThreshold, 3},
		{"toolRepeatWindow", toolRepeatWindow, 8},
		{"maxFencedFailuresPerPath", maxFencedFailuresPerPath, 2},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d — the C4 identity change must not move a bound",
				c.name, c.got, c.want)
		}
	}
	// The streak reset that lets a converging model keep going predates this
	// commit and is the existing changed-rejection-kind rule, not a new
	// epoch. It has exactly one site.
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	// Four, all predating this commit: the changed-rejection-kind reset, the
	// two path-aware-breaker resets, and the one on a successful call.
	resets := strings.Count(string(src), "consecutiveErrors = 0")
	t.Logf("consecutiveErrors reset sites: %d", resets)
	if resets != 4 {
		t.Errorf("%d reset sites; a new one would be an undeclared allowance", resets)
	}
}

// --- C4 recovery: refused replacements over surviving known-good bytes -------
//
// The conditional models below correct themselves ONLY after the recovery text
// arrives. Removing the marker restores the parent's failure, which is the
// whole point: nothing here is a model that fixes itself because turns passed.

const c4Marker = "The working version is still there"

// c4RecoveryPlan repeats invalid replacements until it sees the recovery, then
// sends one valid correction and verifies it. `bodies` is cycled; a single
// entry reproduces the identical-proposal shape.
func c4RecoveryPlan(marker string, bodies []string) func(int, *c4Run) map[string]interface{} {
	corrected, verified := false, false
	return func(i int, r *c4Run) map[string]interface{} {
		switch {
		case i == 0:
			return c4Write("solve.py", c4Valid)
		case marker != "" && r.sawRecovery(marker) && !corrected:
			corrected = true
			return c4Write("solve.py", c4Fixed)
		case corrected && !verified:
			verified = true
			return map[string]interface{}{"type": "tool_call", "name": "run_command",
				"args": map[string]string{"command": "python3 solve.py"}}
		case corrected:
			return map[string]interface{}{"type": "done", "summary": "solve.py prints 7"}
		default:
			return c4Write("solve.py", bodies[(i-1)%len(bodies)])
		}
	}
}

func (r *c4Run) sawRecovery(marker string) bool {
	for _, e := range r.recoveries {
		if strings.Contains(e, marker) {
			return true
		}
	}
	return false
}

func (r *c4Run) recoveryCount(marker string) int {
	n := 0
	for _, e := range r.recoveries {
		if strings.Contains(e, marker) {
			n++
		}
	}
	return n
}

func TestRefusedReplacementRecovery(t *testing.T) {
	for _, c := range []struct {
		name   string
		bodies []string
	}{
		{"the same invalid proposal, repeated", []string{c4BadA}},
		{"two distinct invalid proposals", []string{c4BadA, c4BadB}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := c4Fixture(t, nil, c4RecoveryPlan(c4Marker, c.bodies))
			r.log(t, c.name)

			if n := r.recoveryCount(c4Marker); n != 1 {
				t.Fatalf("%d recoveries, want exactly one", n)
			}
			if r.terminal["status"] != string(TerminalCompleted) ||
				r.terminal["reason"] != "deliverables_demonstrated" {
				t.Fatalf("terminal %q/%q, want completed/deliverables_demonstrated",
					r.terminal["status"], r.terminal["reason"])
			}
			if r.disk(t) != c4Fixed {
				t.Errorf("the correction did not land: %q", r.disk(t))
			}
			d := r.ctx.Ledger[ledgerKey(r.ctx, "solve.py")]
			if d == nil || d.CurrentHash != hashBytes([]byte(r.disk(t))) {
				t.Fatal("the ledger does not describe the bytes on disk")
			}
			if kind, status := d.CurrentValidation(); status != ValidationPassed ||
				kind != ValidationKindSyntax {
				t.Errorf("validation on the current hash is %s/%s, want syntax/passed", kind, status)
			}
			if r.census["done"] != 1 {
				t.Errorf("%d terminal events", r.census["done"])
			}
			if r.census["tool_call"] != r.census["tool_result"] {
				t.Errorf("call/result balance: %d vs %d",
					r.census["tool_call"], r.census["tool_result"])
			}
			// The recovery quotes the diagnostic for the bytes just refused.
			for _, e := range r.recoveries {
				if !strings.Contains(e, c4Marker) {
					continue
				}
				last := c4Syntax(c.bodies[len(c.bodies)-1])
				if !strings.Contains(e, last) {
					t.Errorf("the recovery does not carry the diagnostic for the "+
						"proposal that triggered it (want %q):\n%.400s", last, e)
				}
			}
		})

		t.Run(c.name+" — without the recovery", func(t *testing.T) {
			// Same fixture, marker removed: the model never corrects itself.
			r := c4Fixture(t, nil, c4RecoveryPlan("", c.bodies))
			r.log(t, c.name+"/ignored")
			if r.turns >= 30 {
				t.Fatalf("%d turns without a terminal", r.turns)
			}
			if NormalizeTerminalStatus(r.terminal["status"]).Completed() {
				t.Errorf("a run that never landed a valid replacement reported %q",
					r.terminal["status"])
			}
			if r.disk(t) != c4Valid {
				t.Errorf("the known-good bytes did not survive: %q", r.disk(t))
			}
			if r.census["done"] != 1 {
				t.Errorf("%d terminal events", r.census["done"])
			}
			if r.census["tool_call"] != r.census["tool_result"] {
				t.Errorf("call/result balance: %d vs %d",
					r.census["tool_call"], r.census["tool_result"])
			}
		})
	}
}

// c4State builds the one state C4 reads: a session-written file on disk whose
// exact current bytes are syntax/passed, plus one refused proposal recorded
// against them. Each case then breaks exactly one clause.
func c4State(t *testing.T, mutate func(*AgentContext, *DeliverableState, string)) (
	*AgentContext, *runState, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "solve.py")
	os.WriteFile(path, []byte(c4Valid), 0o644)
	ctx := NewAgentContext(dir, Tier2Medium)
	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*DeliverableState{}
	}
	h := hashBytes([]byte(c4Valid))
	d := &DeliverableState{
		Path: "solve.py", CurrentHash: h, CurrentSize: len(c4Valid), Generation: 1,
		ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
		ValidatedHash: h,
	}
	ctx.Ledger[ledgerKey(ctx, "solve.py")] = d
	if mutate != nil {
		mutate(ctx, d, path)
	}
	return ctx, &runState{}, hashBytes([]byte(c4BadA))
}

func c4Refusal(args string) (json.RawMessage, *ToolResult) {
	return json.RawMessage(args), &ToolResult{
		Success:          false,
		MutationStatus:   MutationRefused,
		ValidationKind:   ValidationKindSyntax,
		ValidationStatus: ValidationFailed,
		ValidationDetail: "invalid syntax (line 3)",
	}
}

// Eligibility is a conjunction. Each case removes one clause and must record
// nothing and recover nothing.
func TestRefusedReplacementEligibility(t *testing.T) {
	badA := `{"path":"solve.py","content":` + c4JSON(c4BadA) + `}`
	for _, c := range []struct {
		name   string
		mutate func(*AgentContext, *DeliverableState, string)
		result func(*ToolResult)
	}{
		{"surviving validation unknown", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus, d.ValidationKind = ValidationUnknown, ValidationKindUnknown
		}, nil},
		{"surviving validation not_run", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus = ValidationNotRun
		}, nil},
		{"surviving validation not_applicable", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus = ValidationNotApplicable
		}, nil},
		{"surviving validation failed", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus = ValidationFailed
		}, nil},
		{"surviving verdict describes older bytes", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidatedHash = hashBytes([]byte("other"))
		}, nil},
		{"disk moved under the ledger", func(_ *AgentContext, _ *DeliverableState, path string) {
			os.WriteFile(path, []byte(c4Fixed), 0o644)
		}, nil},
		{"path unreadable", func(_ *AgentContext, _ *DeliverableState, path string) {
			os.Remove(path)
		}, nil},
		{"no known-good baseline was ever kept", func(ctx *AgentContext, _ *DeliverableState, _ string) {
			delete(ctx.Ledger, ledgerKey(ctx, "solve.py"))
		}, nil},
		{"the write was not refused", nil, func(r *ToolResult) { r.MutationStatus = MutationApplied }},
		{"the failure was not a syntax failure", nil, func(r *ToolResult) {
			r.ValidationKind = ValidationKindUnknown
		}},
		{"no diagnostic for those bytes", nil, func(r *ToolResult) { r.ValidationDetail = "" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, st, _ := c4State(t, c.mutate)
			args, res := c4Refusal(badA)
			if c.result != nil {
				c.result(res)
			}
			key, sha, n := noteRejectedProposal(ctx, st, "write_file", args, res)
			if key != "" || n != 0 {
				t.Fatalf("recorded evidence on %q: key=%q n=%d", c.name, key, n)
			}
			if msg := rejectedProposalRecovery(ctx, st, "solve.py", key, sha); msg != "" {
				t.Errorf("recovered on %q: %.100s", c.name, msg)
			}
			if len(st.c4Rejected) != 0 {
				t.Errorf("state was written anyway: %v", st.c4Rejected)
			}
		})
	}
}

// The positive path, and everything the state must and must not do.
func TestRefusedReplacementStateBounds(t *testing.T) {
	badA := `{"path":"solve.py","content":` + c4JSON(c4BadA) + `}`
	badB := `{"path":"solve.py","content":` + c4JSON(c4BadB) + `}`
	aliasA := `{"path":"./solve.py","content":` + c4JSON(c4BadA) + `}`

	t.Run("a diagnostic never describes a different proposal", func(t *testing.T) {
		ctx, st, _ := c4State(t, nil)
		a, ra := c4Refusal(badA)
		noteRejectedProposal(ctx, st, "write_file", a, ra)
		b, rb := c4Refusal(badB)
		rb.ValidationDetail = "unmatched ']' (line 2)"
		key, shaB, n := noteRejectedProposal(ctx, st, "write_file", b, rb)
		if n != 2 {
			t.Fatalf("distinct proposals = %d, want 2", n)
		}
		msg := rejectedProposalRecovery(ctx, st, "solve.py", key, shaB)
		if !strings.Contains(msg, "unmatched ']' (line 2)") {
			t.Errorf("the recovery does not carry this proposal's diagnostic:\n%s", msg)
		}
		if strings.Contains(msg, "invalid syntax (line 3)") {
			t.Errorf("the recovery carried the OTHER proposal's diagnostic:\n%s", msg)
		}
	})

	t.Run("an unknown proposal hash gets no diagnostic and no recovery", func(t *testing.T) {
		ctx, st, _ := c4State(t, nil)
		a, ra := c4Refusal(badA)
		key, _, _ := noteRejectedProposal(ctx, st, "write_file", a, ra)
		if msg := rejectedProposalRecovery(ctx, st, "solve.py", key,
			hashBytes([]byte(c4BadC))); msg != "" {
			t.Errorf("bytes that were never refused drew a recovery:\n%.150s", msg)
		}
	})

	t.Run("aliases share one generation and one recovery", func(t *testing.T) {
		ctx, st, _ := c4State(t, nil)
		a, ra := c4Refusal(badA)
		k1, _, _ := noteRejectedProposal(ctx, st, "write_file", a, ra)
		al, ral := c4Refusal(aliasA)
		k2, sha, n := noteRejectedProposal(ctx, st, "write_file", al, ral)
		if k1 != k2 {
			t.Errorf("alias spellings built separate generations:\n%q\n%q", k1, k2)
		}
		if n != 1 {
			t.Errorf("the same bytes under another spelling counted as a new proposal (n=%d)", n)
		}
		if rejectedProposalRecovery(ctx, st, "solve.py", k2, sha) == "" {
			t.Fatal("no recovery offered")
		}
		if again := rejectedProposalRecovery(ctx, st, "./solve.py", k2, sha); again != "" {
			t.Error("the alias bought a second recovery")
		}
	})

	t.Run("new surviving bytes are a new generation", func(t *testing.T) {
		ctx, st, _ := c4State(t, nil)
		a, ra := c4Refusal(badA)
		k1, sha, _ := noteRejectedProposal(ctx, st, "write_file", a, ra)
		if rejectedProposalRecovery(ctx, st, "solve.py", k1, sha) == "" {
			t.Fatal("no recovery offered")
		}
		// The model lands something else valid; the old evidence is unreachable.
		os.WriteFile(filepath.Join(ctx.WorkingDir, "solve.py"), []byte(c4Fixed), 0o644)
		h := hashBytes([]byte(c4Fixed))
		d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
		d.CurrentHash, d.ValidatedHash = h, h
		k2, sha2, n := noteRejectedProposal(ctx, st, "write_file", a, ra)
		// One entry per path, so the key is stable; what must change is the
		// generation inside it. The old hashes and diagnostics are released
		// with the bytes they described.
		if k2 != k1 {
			t.Fatalf("the path's entry moved: %q -> %q", k1, k2)
		}
		ev := st.c4Rejected[k2]
		if ev == nil || ev.diskHash != h {
			t.Fatal("the entry still describes the previous surviving bytes")
		}
		if n != 1 || len(ev.order) != 1 {
			t.Errorf("the new generation started at %d proposals, want 1", n)
		}
		if _, stale := ev.diagnostics[sha]; stale && sha != sha2 {
			t.Error("a diagnostic from the previous generation survived")
		}
		if rejectedProposalRecovery(ctx, st, "solve.py", k2, sha2) == "" {
			t.Error("the new generation was denied its own recovery")
		}
	})

	t.Run("separate paths are independent", func(t *testing.T) {
		ctx, st, _ := c4State(t, nil)
		other := filepath.Join(ctx.WorkingDir, "helper.py")
		os.WriteFile(other, []byte(c4Valid), 0o644)
		h := hashBytes([]byte(c4Valid))
		ctx.Ledger[ledgerKey(ctx, "helper.py")] = &DeliverableState{
			Path: "helper.py", CurrentHash: h, Generation: 1,
			ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
			ValidatedHash: h,
		}
		a, ra := c4Refusal(badA)
		k1, sha1, _ := noteRejectedProposal(ctx, st, "write_file", a, ra)
		rejectedProposalRecovery(ctx, st, "solve.py", k1, sha1)

		o, ro := c4Refusal(`{"path":"helper.py","content":` + c4JSON(c4BadA) + `}`)
		k2, sha2, n := noteRejectedProposal(ctx, st, "write_file", o, ro)
		if k1 == k2 || n != 1 {
			t.Errorf("helper.py inherited solve.py's generation (n=%d)", n)
		}
		if rejectedProposalRecovery(ctx, st, "helper.py", k2, sha2) == "" {
			t.Error("solve.py's recovery spent helper.py's")
		}
	})

	t.Run("a run that is ending or out of budget recovers nothing", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			set  func(*AgentContext)
		}{
			{"cancelled", func(ctx *AgentContext) {
				c, cancel := context.WithCancel(context.Background())
				cancel()
				ctx.Ctx = c
			}},
			{"under the recovery floor", func(ctx *AgentContext) {
				c, cancel := context.WithDeadline(context.Background(),
					time.Now().Add(5*time.Second))
				t.Cleanup(cancel)
				ctx.Ctx = c
			}},
		} {
			ctx, st, _ := c4State(t, nil)
			a, ra := c4Refusal(badA)
			key, sha, _ := noteRejectedProposal(ctx, st, "write_file", a, ra)
			tc.set(ctx)
			if msg := rejectedProposalRecovery(ctx, st, "solve.py", key, sha); msg != "" {
				t.Errorf("%s: recovered anyway: %.100s", tc.name, msg)
			}
		}
	})

	t.Run("the state is bounded", func(t *testing.T) {
		ctx, st, _ := c4State(t, nil)
		for i := 0; i < maxC4Proposals+4; i++ {
			body := c4BadA + fmt.Sprintf("# %d\n", i)
			a, ra := c4Refusal(`{"path":"solve.py","content":` + c4JSON(body) + `}`)
			noteRejectedProposal(ctx, st, "write_file", a, ra)
		}
		ev := st.c4Rejected[ledgerKey(ctx, "solve.py")]
		if ev == nil || len(ev.order) > maxC4Proposals {
			t.Errorf("proposal list is unbounded: %d", len(ev.order))
		}
		// Every retained hash still has its own diagnostic.
		for _, h := range ev.order {
			if ev.diagnostics[h] == "" {
				t.Errorf("proposal %.12s is retained without its diagnostic", h)
			}
		}
	})
}

// Lifecycle: a path's own history must never cost it a recovery.
//
// The state was first keyed on path AND surviving hash, under a session-wide
// ceiling. Every correction a path lands is a new surviving hash, so one file
// iterating eight times filled the map with obsolete entries and the ninth
// generation -- the live one -- was refused a recovery by its own past.
func TestRefusedReplacementGenerationsDoNotStarve(t *testing.T) {
	ctx, st, _ := c4State(t, nil)
	path := filepath.Join(ctx.WorkingDir, "solve.py")
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]

	// Nine successive generations of the SAME path: land new valid bytes,
	// have a replacement refused against them, take the recovery.
	for gen := 0; gen < 9; gen++ {
		body := fmt.Sprintf("%s# generation %d\n", c4Valid, gen)
		os.WriteFile(path, []byte(body), 0o644)
		h := hashBytes([]byte(body))
		d.CurrentHash, d.ValidatedHash = h, h

		args, res := c4Refusal(`{"path":"solve.py","content":` +
			c4JSON(c4BadA+fmt.Sprintf("# try %d\n", gen)) + `}`)
		canon, sha, n := noteRejectedProposal(ctx, st, "write_file", args, res)
		if canon == "" {
			t.Fatalf("generation %d recorded nothing", gen+1)
		}
		if n != 1 {
			t.Errorf("generation %d started at %d proposals, want 1 — the previous "+
				"generation's hashes were carried over", gen+1, n)
		}
		if msg := rejectedProposalRecovery(ctx, st, "solve.py", canon, sha); msg == "" {
			t.Fatalf("generation %d was refused a recovery by the path's own history", gen+1)
		}
		if len(st.c4Rejected) != 1 {
			t.Errorf("generation %d left %d entries for one path", gen+1, len(st.c4Rejected))
		}
	}
}

// The ceiling bounds live paths, and reaching it fails closed.
func TestRefusedReplacementCeilingFailsClosed(t *testing.T) {
	ctx, st, _ := c4State(t, nil)
	mk := func(name string) {
		p := filepath.Join(ctx.WorkingDir, name)
		os.WriteFile(p, []byte(c4Valid), 0o644)
		h := hashBytes([]byte(c4Valid))
		ctx.Ledger[ledgerKey(ctx, name)] = &DeliverableState{
			Path: name, CurrentHash: h, Generation: 1,
			ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
			ValidatedHash: h,
		}
		args, res := c4Refusal(`{"path":"` + name + `","content":` + c4JSON(c4BadA) + `}`)
		noteRejectedProposal(ctx, st, "write_file", args, res)
	}
	// solve.py plus seven more fills the ceiling with LIVE paths.
	mk("solve.py")
	for i := 0; i < maxC4Generations-1; i++ {
		mk(fmt.Sprintf("m%d.py", i))
	}
	if len(st.c4Rejected) != maxC4Generations {
		t.Fatalf("%d tracked paths, want %d", len(st.c4Rejected), maxC4Generations)
	}
	// A ninth LIVE path records nothing and recovers nothing — no diagnostic
	// is borrowed from any of the eight.
	mk("ninth.py")
	if len(st.c4Rejected) != maxC4Generations {
		t.Errorf("the ceiling was exceeded: %d", len(st.c4Rejected))
	}
	args, res := c4Refusal(`{"path":"ninth.py","content":` + c4JSON(c4BadA) + `}`)
	canon, sha, n := noteRejectedProposal(ctx, st, "write_file", args, res)
	if canon != "" || n != 0 {
		t.Errorf("the ninth live path was recorded past the ceiling: %q n=%d", canon, n)
	}
	if msg := rejectedProposalRecovery(ctx, st, "ninth.py", canon, sha); msg != "" {
		t.Errorf("a path with no evidence of its own drew a recovery:\n%.150s", msg)
	}

	// A tracked path whose bytes are gone is provably stale, and only that
	// makes room.
	os.Remove(filepath.Join(ctx.WorkingDir, "m0.py"))
	canon, sha, n = noteRejectedProposal(ctx, st, "write_file", args, res)
	if canon == "" || n != 1 {
		t.Errorf("a provably stale entry did not make room: canon=%q n=%d", canon, n)
	}
	if len(st.c4Rejected) > maxC4Generations {
		t.Errorf("eviction exceeded the ceiling: %d", len(st.c4Rejected))
	}
	if msg := rejectedProposalRecovery(ctx, st, "ninth.py", canon, sha); msg == "" {
		t.Error("the ninth path was still denied after room was made")
	}
}

// Evidence recorded against bytes that have since changed is never offered.
func TestRefusedReplacementEvidenceMustMatchCurrentBytes(t *testing.T) {
	ctx, st, _ := c4State(t, nil)
	args, res := c4Refusal(`{"path":"solve.py","content":` + c4JSON(c4BadA) + `}`)
	canon, sha, _ := noteRejectedProposal(ctx, st, "write_file", args, res)
	// Disk moves without the recovery being taken.
	os.WriteFile(filepath.Join(ctx.WorkingDir, "solve.py"), []byte(c4Fixed), 0o644)
	h := hashBytes([]byte(c4Fixed))
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	d.CurrentHash, d.ValidatedHash = h, h
	if msg := rejectedProposalRecovery(ctx, st, "solve.py", canon, sha); msg != "" {
		t.Errorf("evidence about older bytes was offered anyway:\n%.200s", msg)
	}
}
