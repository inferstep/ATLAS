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

// The fence grammar reserves four backticks for the closer (fenceBlockGrammar),
// and the model closes code with three, which the grammar takes as a body
// line. A grammar-constrained attempt then cannot end on its own: the fenced
// watchdog must cut it, and a retry without the grammar lands the file.

// serveStuckStream streams parts, then holds the stream open with frames that
// carry no content until the client gives up.
func serveStuckStream(w http.ResponseWriter, r *http.Request, stop <-chan struct{}, parts ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	frame := func(delta map[string]string) {
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": delta}},
		})
		fmt.Fprintf(w, "data: %s\n\n", d)
		if fl != nil {
			fl.Flush()
		}
	}
	for _, part := range parts {
		frame(map[string]string{"content": part})
	}
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-stop:
			return
		case <-deadline:
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		default:
		}
		frame(map[string]string{})
		time.Sleep(20 * time.Millisecond)
	}
}

// serveReasoningOnly streams reasoning_content frames and no content until
// the client gives up.
func serveReasoningOnly(w http.ResponseWriter, r *http.Request, stop <-chan struct{}) {
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
			"choices": []map[string]interface{}{{"delta": map[string]string{"reasoning_content": "thinking "}}},
		})
		fmt.Fprintf(w, "data: %s\n\n", d)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// serveReply streams body as one content frame and ends the stream.
func serveReply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/event-stream")
	d, _ := json.Marshal(map[string]interface{}{
		"choices": []map[string]interface{}{{"delta": map[string]string{"content": body}}},
	})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
}

func TestAGrammarConstrainedFencedAttemptIsWatched(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "5")
	t.Setenv("ATLAS_FENCED_IDLE_SEC", "1")
	stop := make(chan struct{})
	defer close(stop)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The block closed the way the model closes it (three backticks),
		// then frames with no content.
		serveStuckStream(w, r, stop, "````python\n", "print(1)\n", "```\n")
	}))
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.Ctx = context.Background()
	start := time.Now()
	out, _, err := callLLMOnceWithGrammar(ctx, []AgentMessage{{Role: "user", Content: "x"}},
		0.2, fenceBlockGrammar("python"))
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("the grammar-constrained attempt was not watched: %v", el)
	}
	if err == nil {
		t.Error("the stuck attempt ended without a cut")
	}
	if !strings.Contains(out, "print(1)") {
		t.Errorf("the content written before the cut was lost: %q", out)
	}
}

// End to end: the grammar attempt is cut before the block closes, the cut is
// not a stall (the model was writing), and the retry without the grammar
// lands the file.
func TestAnUnclosedFenceIsRetriedWithoutTheGrammar(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "5")
	t.Setenv("ATLAS_FENCED_IDLE_SEC", "1")
	stop := make(chan struct{})
	defer close(stop)
	var mu sync.Mutex
	var grammarUsed []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isChatCompletion(r) {
			http.NotFound(w, r)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, withGrammar := body["grammar"]
		mu.Lock()
		grammarUsed = append(grammarUsed, withGrammar)
		mu.Unlock()
		if withGrammar {
			serveStuckStream(w, r, stop, "````python\n", "print(1)\n")
			return
		}
		serveReply(w, "````python\nprint(1)\n````")
	}))
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.Ctx = context.Background()
	start := time.Now()
	content, err := fetchFencedContent(ctx,
		`{"type":"tool_call","name":"write_file","args":{"path":"app.py","content":"@fenced"}}`, "app.py")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the file was not fetched: %v", err)
	}
	if strings.TrimSpace(content) != "print(1)" {
		t.Errorf("content %q, want print(1)", content)
	}
	if elapsed > 15*time.Second {
		t.Errorf("the fetch took %v", elapsed)
	}
	if ctx.FencedStalls != 0 {
		t.Errorf("a cut after content counted as a stall (%d): the channel would be off for the session",
			ctx.FencedStalls)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(grammarUsed) != 2 || !grammarUsed[0] || grammarUsed[1] {
		t.Errorf("attempts used the grammar as %v, want [true false]", grammarUsed)
	}
}

// fencedLoopStub is the model and services for a session whose one write is
// "@fenced": every turn asks to write solve.py with the sentinel, and the
// fenced sub-call reasons forever (it never sends the file).
func fencedLoopStub(t *testing.T, stop <-chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "single fenced block") {
			serveReasoningOnly(w, r, stop)
			return
		}
		call, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": "solve.py", "content": "@fenced"},
		})
		serveReply(w, string(call))
	}))
}

// writeFileErrors runs the agent loop on task and returns the error text of
// each write_file result, in order.
func writeFileErrors(ctx *AgentContext, task string) []string {
	var mu sync.Mutex
	var errs []string
	ctx.StreamFn = func(et string, data interface{}) {
		m, ok := data.(map[string]interface{})
		if et != "tool_result" || !ok || m["tool"] != "write_file" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		e, _ := m["error"].(string)
		errs = append(errs, e)
	}
	runAgentLoop(ctx, task)
	mu.Lock()
	defer mu.Unlock()
	return errs
}

// The session stops the fetch: it is cancelled while the file is being
// fetched, or too little session time is left to fetch the file and check it.
// The model sent nothing wrong, so the write result gives that reason, not
// "no fenced block followed", and no file is written.
func TestAFetchStoppedByTheSessionSaysWhy(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "30")
	for _, tc := range []struct {
		name        string
		maxTurns    int
		cancelAfter time.Duration // cancel the session after this long
		timeout     time.Duration // or give the session this deadline
		want        string
	}{
		{"cancelled during the fetch", 4, 1500 * time.Millisecond, 0, "was cancelled"},
		{"too little time left", 2, 0, 8 * time.Second, "too little session time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop := make(chan struct{})
			defer close(stop)
			srv := fencedLoopStub(t, stop)
			defer srv.Close()

			dir := t.TempDir()
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
			ctx.PermissionMode = PermissionYolo
			ctx.MaxTurns = tc.maxTurns
			var session context.Context
			var cancel context.CancelFunc
			if tc.timeout > 0 {
				session, cancel = context.WithTimeout(context.Background(), tc.timeout)
			} else {
				session, cancel = context.WithCancel(context.Background())
				time.AfterFunc(tc.cancelAfter, cancel)
			}
			defer cancel()
			ctx.Ctx = session

			errs := writeFileErrors(ctx, "Create solve.py.")
			if len(errs) == 0 {
				t.Fatal("no write_file result")
			}
			if strings.Contains(errs[0], "no fenced block followed") || !strings.Contains(errs[0], tc.want) {
				t.Errorf("the write result does not give the true reason: %q", errs[0])
			}
			if _, err := os.Stat(filepath.Join(dir, "solve.py")); err == nil {
				t.Error("solve.py was written")
			}
		})
	}
}
