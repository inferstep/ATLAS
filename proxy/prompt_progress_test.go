package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// While the model has given no first piece of its reply, the proxy sends
// llm_prompt_progress on a clock of its own, every 100 ms. The replay tests
// leave that event out of what they compare, because how many of them come
// follows the speed of the model. So this test holds what a replay does not
// show: the event comes while the model is silent, it stops with the first
// piece of the reply, and none comes after the call has ended.
func TestPromptProgressComesOnlyWhileTheModelHasGivenNoFirstPiece(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		time.Sleep(350 * time.Millisecond) // silent: the model reads the prompt
		sseWrite(w, `data: {"choices":[{"index":0,"delta":{"content":"{\"type\": \"done\","}}]}`)
		time.Sleep(450 * time.Millisecond) // the reply has begun, and goes on
		sseWrite(w, `data: {"choices":[{"index":0,"delta":{"content":" \"summary\": \"ok\"}"}}]}`, `data: [DONE]`)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var events []string
	ctx := llmTestCtx(srv.URL)
	ctx.StreamFn = func(event string, _ interface{}) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	if _, _, err := callLLMOnce(ctx, ctx.Messages, 0.3); err != nil {
		t.Fatalf("the call of the model failed: %v", err)
	}
	mu.Lock()
	atTheEnd := slices.Clone(events)
	mu.Unlock()

	firstPiece := slices.Index(atTheEnd, "llm_first_token")
	if firstPiece < 0 {
		t.Fatalf("no llm_first_token among the events: %v", atTheEnd)
	}
	whileSilent, afterTheFirstPiece := 0, 0
	for at, event := range atTheEnd {
		if event == "llm_prompt_progress" && at < firstPiece {
			whileSilent++
		}
		if event == "llm_prompt_progress" && at > firstPiece {
			afterTheFirstPiece++
		}
	}
	if whileSilent < 2 {
		t.Errorf("the model was silent for 350 ms: llm_prompt_progress came %d time(s), want 2 or more (events: %v)", whileSilent, atTheEnd)
	}
	// One event can be on its way when the first piece comes. A poller that
	// is not stopped then would send four more in the 450 ms that follow.
	if afterTheFirstPiece > 1 {
		t.Errorf("after the first piece of the reply llm_prompt_progress came %d time(s), want 1 at the most (events: %v)", afterTheFirstPiece, atTheEnd)
	}

	time.Sleep(300 * time.Millisecond) // a poller that outlived the call would send three more now
	mu.Lock()
	defer mu.Unlock()
	if len(events) != len(atTheEnd) {
		t.Errorf("after the call had ended %d more event(s) came: %v", len(events)-len(atTheEnd), events[len(atTheEnd):])
	}
}
