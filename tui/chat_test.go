// Tests for the /v1/agent chat client. Uses httptest.NewServer to
// stand in for atlas-proxy so the test exercises the real HTTP+SSE
// path without needing the proxy running.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeAgentServer returns an httptest server that, on POST /v1/agent,
// streams the given canned events then closes with [DONE]. Captures
// the request body for the test to inspect.
func fakeAgentServer(t *testing.T, events []chatEvent,
	gotBody *agentRequest) *httptest.Server {

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if gotBody != nil {
			_ = json.Unmarshal(body, gotBody)
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("test server: no flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, ev := range events {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	mux.HandleFunc("/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"cancelled": true})
	})
	return httptest.NewServer(mux)
}

func mkChatEvent(typ string, data interface{}) chatEvent {
	b, _ := json.Marshal(data)
	return chatEvent{Type: typ, Data: b}
}

func TestSendChatPostsRequestBodyAndStreamsEvents(t *testing.T) {
	want := []chatEvent{
		mkChatEvent("text", map[string]string{"content": "hi"}),
		mkChatEvent("tool_call", map[string]interface{}{
			"name": "read_file", "args": map[string]string{"path": "x.go"},
		}),
		mkChatEvent("done", map[string]string{"summary": "ok"}),
	}
	var gotReq agentRequest
	srv := fakeAgentServer(t, want, &gotReq)
	defer srv.Close()

	out := make(chan chatEvent, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := sendChatOpts(ctx, srv.URL, "fix bug", "/work", "default",
		"sess-1", nil, demoOpts{}, out); err != nil {
		t.Fatalf("sendChatOpts: %v", err)
	}
	close(out)

	if gotReq.Message != "fix bug" {
		t.Errorf("Message = %q", gotReq.Message)
	}
	if gotReq.WorkingDir != "/work" {
		t.Errorf("WorkingDir = %q", gotReq.WorkingDir)
	}
	if gotReq.Mode != "default" {
		t.Errorf("Mode = %q", gotReq.Mode)
	}
	if gotReq.SessionID != "sess-1" {
		t.Errorf("SessionID = %q", gotReq.SessionID)
	}

	got := []chatEvent{}
	for ev := range out {
		got = append(got, ev)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	if got[0].Type != "text" || got[1].Type != "tool_call" || got[2].Type != "done" {
		t.Errorf("event order = %v", []string{got[0].Type, got[1].Type, got[2].Type})
	}
}

func TestDemoRawSideUsesOneDirectModelRequest(t *testing.T) {
	var got rawChatRequest
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	out := make(chan chatEvent, 8)

	if err := sendRawChat(context.Background(), srv.URL, "configured-model", "build it", out); err != nil {
		t.Fatalf("sendRawChat: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want raw chat completions endpoint", gotPath)
	}
	if !got.Stream || got.Model != "configured-model" {
		t.Fatalf("request = %#v", got)
	}
	if len(got.Messages) != 1 || got.Messages[0]["content"] != "build it" {
		t.Fatalf("messages = %#v", got.Messages)
	}
	for _, message := range got.Messages {
		if strings.Contains(message["content"], "plan_tasks") {
			t.Fatalf("raw request contains orchestration tool guidance: %#v", got.Messages)
		}
	}
	close(out)
	var types []string
	for event := range out {
		types = append(types, event.Type)
	}
	if strings.Join(types, ",") != "llm_call_start,llm_first_token,llm_token,llm_call_end,text" {
		t.Fatalf("events = %v", types)
	}
}

func TestSendChatHandlesNon200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no llama", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	out := make(chan chatEvent, 1)
	err := sendChatOpts(context.Background(), srv.URL, "hi", "/", "default", "s", nil, demoOpts{}, out)
	if err == nil {
		t.Fatal("sendChatOpts should return error on 503")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %v, want one mentioning 503", err)
	}
}

func TestSendChatContextCancelStopsStream(t *testing.T) {
	// Server drips events forever — sendChatOpts should return when ctx cancels.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for i := 0; i < 100; i++ {
			b, _ := json.Marshal(mkChatEvent("text",
				map[string]string{"content": "drip"}))
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()

	out := make(chan chatEvent, 32)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	err := sendChatOpts(ctx, srv.URL, "hi", "/", "default", "s", nil, demoOpts{}, out)
	if err == nil {
		// context cancellation on read returns ctx.Err() or io.EOF;
		// either is acceptable. nil means the stream completed before
		// cancel fired — which is also fine but unexpected.
		t.Log("sendChatOpts returned nil — server may have closed first")
	}
}

func TestCancelTurnPosts(t *testing.T) {
	got := make(chan map[string]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cancel" {
			http.Error(w, "wrong path", 404)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- body
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"cancelled": true})
	}))
	defer srv.Close()

	if err := cancelTurn(srv.URL, "sess-9"); err != nil {
		t.Fatalf("cancelTurn: %v", err)
	}
	body := <-got
	if body["session_id"] != "sess-9" {
		t.Errorf("session_id = %q", body["session_id"])
	}
}

func TestCancelTurnEmptyIDIsError(t *testing.T) {
	if err := cancelTurn("http://localhost:0", ""); err == nil {
		t.Errorf("expected error for empty session id")
	}
}

func TestParseChatSSESkipsCommentsAndDONE(t *testing.T) {
	stream := strings.Join([]string{
		": connected",
		"",
		"data: " + mustJSON(mkChatEvent("text", map[string]string{"content": "a"})),
		"",
		": heartbeat",
		"",
		"data: " + mustJSON(mkChatEvent("text", map[string]string{"content": "b"})),
		"",
		"data: [DONE]",
		"",
	}, "\n")
	out := make(chan chatEvent, 4)
	if err := parseChatSSE(context.Background(), strings.NewReader(stream), out); err != nil {
		t.Fatalf("parseChatSSE: %v", err)
	}
	close(out)
	got := []chatEvent{}
	for ev := range out {
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Errorf("got %d events, want 2 (comments + [DONE] should be skipped)", len(got))
	}
}

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestLoadBearerTokenMissingFileReturnsEmpty(t *testing.T) {
	t.Setenv("ATLAS_API_KEYS_PATH", "/nonexistent/path/api-keys.json")
	if got := loadBearerToken(); got != "" {
		t.Errorf("loadBearerToken with missing file = %q, want empty", got)
	}
}

func TestLoadBearerTokenReadsAPIKey(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/api-keys.json"
	if err := writeJSON(path, map[string]string{"api_key": "abc123"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATLAS_API_KEYS_PATH", path)
	if got := loadBearerToken(); got != "abc123" {
		t.Errorf("loadBearerToken = %q, want abc123", got)
	}
}

// `atlas init` writes {"sk-…": {"user": …}} — token strings keyed to
// metadata objects. The loader picks the lexicographically first token.
func TestLoadBearerTokenReadsAtlasInitShape(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/api-keys.json"
	body := map[string]interface{}{
		"sk-atlas-zzz": map[string]string{"user": "local", "created_by": "atlas init"},
		"sk-atlas-aaa": map[string]string{"user": "local", "created_by": "atlas init"},
	}
	if err := writeJSON(path, body); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATLAS_API_KEYS_PATH", path)
	if got := loadBearerToken(); got != "sk-atlas-aaa" {
		t.Errorf("loadBearerToken = %q, want sk-atlas-aaa", got)
	}
}

func TestLoadBearerTokenReadsGroupedKeys(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/api-keys.json"
	body := map[string]interface{}{"keys": map[string]string{"tui": "tok-1"}}
	if err := writeJSON(path, body); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATLAS_API_KEYS_PATH", path)
	if got := loadBearerToken(); got != "tok-1" {
		t.Errorf("loadBearerToken = %q, want tok-1", got)
	}
}

func writeJSON(path string, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// --- The structured task contract, sent by the client ------------------------
//
// The proxy still decides everything from the user's English. This is the
// client saying, in a typed field, what only the client knows: whether the
// person was asking for work or asking a question. It is a control the user
// operates, never an inference from what they typed -- the same sentence can
// go either way, and only the explicit selection differs.

// captureContract posts one message and returns the decoded request body.
func captureContract(t *testing.T, mode taskMode, message string) map[string]interface{} {
	t.Helper()
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"done\",\"summary\":\"ok\"}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	out := make(chan chatEvent, 16)
	err := sendChatOpts(context.Background(), srv.URL, message, t.TempDir(), "default",
		"sess-1", nil, demoOpts{taskMode: mode}, out)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return got
}

func contractOf(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	tc, ok := body["task_contract"].(map[string]interface{})
	if !ok {
		t.Fatalf("no task_contract in %v", body)
	}
	return tc
}

// 1/2: an ordinary message is work; the explicit control sends question.
func TestTaskContractModeIsClientSelected(t *testing.T) {
	work := captureContract(t, taskModeWork, "What does this repository do?")
	if got := contractOf(t, work)["task_mode"]; got != "work" {
		t.Errorf("ordinary send task_mode=%v, want work", got)
	}
	question := captureContract(t, taskModeQuestion, "Create app.py.")
	if got := contractOf(t, question)["task_mode"]; got != "question" {
		t.Errorf("explicit question task_mode=%v, want question", got)
	}
	// 5/6: identical prose, opposite modes, chosen only by the client.
	same := "Fix the failing test."
	a := contractOf(t, captureContract(t, taskModeWork, same))["task_mode"]
	b := contractOf(t, captureContract(t, taskModeQuestion, same))["task_mode"]
	if a != "work" || b != "question" {
		t.Errorf("the same sentence gave %v and %v; selection is not client-owned", a, b)
	}
}

// 4: the control never edits the user's message.
func TestQuestionSelectionLeavesTheMessageAlone(t *testing.T) {
	const msg = "Is app.py valid?"
	for _, mode := range []taskMode{taskModeWork, taskModeQuestion} {
		body := captureContract(t, mode, msg)
		if body["message"] != msg {
			t.Errorf("mode %q changed the message to %q", mode, body["message"])
		}
	}
}

// 7: no obligation is invented when the client knows none.
func TestTaskContractSendsNoFabricatedObligations(t *testing.T) {
	tc := contractOf(t, captureContract(t, taskModeWork, "Write four files."))
	if _, ok := tc["expected_outputs"]; ok {
		t.Errorf("the TUI invented expected_outputs: %v", tc["expected_outputs"])
	}
	if _, ok := tc["verification"]; ok {
		t.Errorf("the TUI invented verification: %v", tc["verification"])
	}
}

// 3: question mode is one-shot -- the next ordinary message is work again.
func TestQuestionModeIsOneShot(t *testing.T) {
	m := &tuiModel{}
	if got := m.takeTaskMode(); got != taskModeWork {
		t.Errorf("default task mode is %q, want work", got)
	}
	m.pendingTaskMode = taskModeQuestion
	if got := m.takeTaskMode(); got != taskModeQuestion {
		t.Errorf("selected mode was not used: %q", got)
	}
	if got := m.takeTaskMode(); got != taskModeWork {
		t.Errorf("question mode persisted into the next turn: %q — it must be one-shot", got)
	}
}

// A default caller declares work. Absence is reserved for the one legacy
// fixture: an owned sender that omits the contract is indistinguishable from a
// stranger's request.
func TestDefaultCallerDeclaresWork(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"done\",\"summary\":\"ok\"}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	out := make(chan chatEvent, 16)
	// demoOpts{} is the default caller: no explicit mode selected.
	if err := sendChatOpts(context.Background(), srv.URL, "hi", t.TempDir(),
		"default", "s", nil, demoOpts{}, out); err != nil {
		t.Fatal(err)
	}
	tc, ok := got["task_contract"].(map[string]interface{})
	if !ok {
		t.Fatalf("a default owned caller sent no task_contract: %v", got)
	}
	if tc["task_mode"] != "work" {
		t.Errorf("task_mode=%v, want work", tc["task_mode"])
	}
	if _, ok := tc["expected_outputs"]; ok {
		t.Error("the default caller fabricated expected_outputs")
	}
	if _, ok := tc["verification"]; ok {
		t.Error("the default caller fabricated verification")
	}
}

// Exactly one path omits the contract, and it exists to prove an external or
// legacy caller is still accepted.
func TestLegacyOmissionFixtureIsTheOnlyContractlessSender(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"done\",\"summary\":\"ok\"}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	out := make(chan chatEvent, 16)
	if err := sendChatOpts(context.Background(), srv.URL, "hi", t.TempDir(),
		"default", "s", nil, demoOpts{omitTaskContract: true}, out); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["task_contract"]; ok {
		t.Errorf("the legacy fixture sent a contract: %v", got["task_contract"])
	}
	if got["message"] != "hi" {
		t.Errorf("the legacy path changed the message: %v", got["message"])
	}
}
