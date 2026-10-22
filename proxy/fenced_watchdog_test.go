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
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fenced sub-call is bounded by progress, not by total time. A model can
// stream reasoning to max_tokens and never open a fence, while a fetch that
// sends content throughout may rightly run for minutes. So the watchdog cuts
// a stream that sends no content in time, not a stream that is slow.

// The bounds default to 60s for the first content and 30s between pieces of
// content. An override outside the safe range falls back to the default.
func TestFencedWatchdogBounds(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "")
	if got := fencedFirstContentTimeout(); got != 60*time.Second {
		t.Errorf("first-content default = %v, want 60s", got)
	}
	for _, tc := range []struct {
		idle string
		want time.Duration
	}{
		{"", 30 * time.Second},
		{"2", 2 * time.Second},
		{"45", 45 * time.Second},
		{"0", 30 * time.Second},
		{"-1", 30 * time.Second},
		{"-5", 30 * time.Second},
		{"abc", 30 * time.Second},
		{"99999", 30 * time.Second},
	} {
		t.Run("ATLAS_FENCED_IDLE_SEC="+tc.idle, func(t *testing.T) {
			t.Setenv("ATLAS_FENCED_IDLE_SEC", tc.idle)
			if got := fencedIdleTimeout(); got != tc.want {
				t.Errorf("idle bound = %v, want %v", got, tc.want)
			}
		})
	}
}

// reasoningOnlyStub streams reasoning forever and never sends content: the
// zero-byte shape.
func reasoningOnlyStub(t *testing.T, stop <-chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveReasoningOnly(w, r, stop)
	}))
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

// Reasoning is not progress. A stream that only reasons, the zero-byte shape,
// is cut by the stalled bound that its first frame arms, and gives no content.
// The idle bound is ten minutes and the reasoning budget is off, so if a
// reasoning frame re-armed the watchdog, nothing would cut the stream before
// the session's minute ends, and that end is not a watchdog cut. How fast the
// frames come does not change the result.
func TestReasoningDoesNotResetTheProgressTimer(t *testing.T) {
	t.Setenv("ATLAS_FENCED_STALL_SEC", "1")
	t.Setenv("ATLAS_FENCED_IDLE_SEC", "600")
	t.Setenv("ATLAS_REASONING_BUDGET", "0")
	stop := make(chan struct{})
	defer close(stop)
	srv := reasoningOnlyStub(t, stop)
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = srv.URL
	session, endSession := context.WithTimeout(context.Background(), time.Minute)
	defer endSession()
	ctx.Ctx = session
	out, _, _ := callLLMOnceWithGrammar(ctx, []AgentMessage{{Role: "user", Content: "x"}},
		0.2, rawEmissionSentinel)
	if cut := ctx.LastFencedStream.Cut; cut != "stalled" {
		t.Errorf("watchdog cut = %q, want %q", cut, "stalled")
	}
	if extractFencedContent(out) != "" {
		t.Errorf("a reasoning-only stream must not yield content: %q", out)
	}
}

// Ordinary turns are untouched: the watchdog exists only for the fenced
// sub-call, so a normal grammar stream that is slow to send content is bounded
// by the reasoning budget, not by these bounds.
func TestOrdinaryTurnsAreNotBoundedByTheFencedWatchdog(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond) // past the fenced bound
		serveReply(w, `{"type":"done"}`)
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

func TestWatchdogCancelsOnlyTheChildAndChargesTheAllowance(t *testing.T) {
	// The stub's first frame comes at once, so the stalled bound is the one
	// that cuts. At its default, the reasoning budget ends the stream first.
	t.Setenv("ATLAS_FENCED_STALL_SEC", "1")
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
	// One stall turns the channel off for the session, so the next
	// write_file does not start the fetch again.
	if !fencedChannelDisabledForSession(ctx) {
		t.Error("a watchdog stall did not turn the fenced channel off")
	}
}

// Through the real loop: every turn sends write_file with the @fenced
// sentinel, and the sub-call never sends a block. The fetch is bounded by
// progress and the allowance is spent once for the path, so the loop stops
// starting generations, and the file is untouched.
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
		if serveLoopServices(w, r, dir) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "single fenced block") {
			mu.Lock()
			fenceFetches++
			mu.Unlock()
			serveReasoningOnly(w, r, stop)
			return
		}
		call, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": rel, "content": "@fenced"},
		})
		serveReply(w, string(call))
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

	// However many write_file calls follow, the allowance is not restored.
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
