// Tests for the /v1/agent chat client. Uses httptest.NewServer to
// stand in for atlas-proxy so the test exercises the real HTTP+SSE
// path without needing the proxy running.

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
	"testing"
	"time"
)

func mkChatEvent(typ string, data interface{}) chatEvent {
	b, _ := json.Marshal(data)
	return chatEvent{Type: typ, Data: b}
}

// sendChatOpts posts the request to /v1/agent, with the tools the user allowed
// for the session (so the proxy does not ask again), and forwards the
// streamed events in order.
func TestSendChatPostsRequestBodyAndStreamsEvents(t *testing.T) {
	var got agentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/agent" {
			http.Error(w, "want POST /v1/agent", http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"text","data":{"content":"hi"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"tool_call","data":{"name":"read_file","args":{"path":"x.go"}}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"done","data":{"summary":"ok"}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	out := make(chan chatEvent, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := sendChatOpts(ctx, srv.URL, "fix bug", "/work", "default", "sess-1", nil,
		demoOpts{allowedTools: []string{"run_command", "write_file"}}, out)
	if err != nil {
		t.Fatalf("sendChatOpts: %v", err)
	}
	close(out)

	if got.Message != "fix bug" || got.WorkingDir != "/work" || got.Mode != "default" || got.SessionID != "sess-1" {
		t.Errorf("request = %+v, want the message, working dir, mode and session id as given", got)
	}
	if strings.Join(got.SessionAllowedTools, ",") != "run_command,write_file" {
		t.Errorf("session_allowed_tools = %v, want [run_command write_file]", got.SessionAllowedTools)
	}
	var types []string
	for ev := range out {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "text,tool_call,done" {
		t.Errorf("events = %v, want text, tool_call, done", types)
	}
}

// The demo's raw side is one direct request to the model, carrying only the
// user's message: no agent endpoint and no orchestration guidance.
func TestDemoRawSideUsesOneDirectModelRequest(t *testing.T) {
	var got rawChatRequest
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"answer"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	out := make(chan chatEvent, 8)
	if err := sendRawChat(context.Background(), srv.URL, "configured-model", "build it", out); err != nil {
		t.Fatalf("sendRawChat: %v", err)
	}
	close(out)
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want the raw chat completions endpoint", gotPath)
	}
	if !got.Stream || got.Model != "configured-model" {
		t.Errorf("request = %#v, want a stream from the configured model", got)
	}
	if len(got.Messages) != 1 || got.Messages[0]["content"] != "build it" {
		t.Errorf("messages = %#v, want only the user's message", got.Messages)
	}
	var types []string
	for event := range out {
		types = append(types, event.Type)
	}
	if strings.Join(types, ",") != "llm_call_start,llm_first_token,llm_token,llm_call_end,text" {
		t.Errorf("events = %v", types)
	}
}

func TestSendChatHandlesNon200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no llama", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	out := make(chan chatEvent, 1)
	err := sendChatOpts(context.Background(), srv.URL, "hi", "/", "default", "s", nil, demoOpts{}, out)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %v, want one mentioning 503", err)
	}
}

// Cancelling the context stops the stream early and returns an error, so a
// cut-off turn never passes for a finished one.
func TestSendChatContextCancelStopsStream(t *testing.T) {
	// The server drips 100 events over two seconds unless the client goes away.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 100; i++ {
			fmt.Fprint(w, `data: {"type":"text","data":{"content":"drip"}}`+"\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()

	// Room for every event, so a client that ignores the cancel cannot block.
	out := make(chan chatEvent, 100)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(80*time.Millisecond, cancel)
	err := sendChatOpts(ctx, srv.URL, "hi", "/", "default", "s", nil, demoOpts{}, out)
	if err == nil {
		t.Error("a cancelled stream returned no error")
	}
	if len(out) == 100 {
		t.Error("all 100 events arrived: the cancel did not stop the stream")
	}
}

// cancelTurn posts the turn's session id to /cancel. Without an id it refuses
// and posts nothing.
func TestCancelTurnPostsTheSessionID(t *testing.T) {
	var posted []string // the path and session id of each request the proxy got
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		posted = append(posted, r.URL.Path+" "+body["session_id"])
	}))
	defer srv.Close()

	if err := cancelTurn(srv.URL, ""); err == nil {
		t.Error("an empty session id was not refused")
	}
	if err := cancelTurn(srv.URL, "sess-9"); err != nil {
		t.Fatalf("cancelTurn: %v", err)
	}
	if strings.Join(posted, ", ") != "/cancel sess-9" {
		t.Errorf("the proxy got %q, want only /cancel for sess-9", posted)
	}
}

// Comment lines (": ...") are not events, and [DONE] ends the turn: nothing
// after it is read.
func TestParseChatSSESkipsCommentsAndDONE(t *testing.T) {
	stream := ": connected\n\n" +
		`data: {"type":"text","data":{"content":"a"}}` + "\n\n" +
		": heartbeat\n\n" +
		`data: {"type":"text","data":{"content":"b"}}` + "\n\n" +
		"data: [DONE]\n\n" +
		`data: {"type":"text","data":{"content":"after the end"}}` + "\n\n"
	out := make(chan chatEvent, 4)
	if err := parseChatSSE(context.Background(), strings.NewReader(stream), out); err != nil {
		t.Fatalf("parseChatSSE: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("got %d events, want 2 (comments are skipped, nothing after [DONE] is read)", len(out))
	}
}

// The token comes from the api-keys file in any of its three shapes; with no
// file there is no token.
func TestLoadBearerToken(t *testing.T) {
	cases := []struct {
		name string
		file string // the content of the api-keys file; "" means there is no file
		want string
	}{
		{"no file", "", ""},
		{"api_key field", `{"api_key": "abc123"}`, "abc123"},
		{"grouped keys", `{"keys": {"tui": "tok-1"}}`, "tok-1"},
		// `atlas init` writes token strings keyed to metadata; the first in
		// sorted order wins.
		{"atlas init shape", `{"sk-atlas-zzz": {"user": "local", "created_by": "atlas init"},
			"sk-atlas-aaa": {"user": "local", "created_by": "atlas init"}}`, "sk-atlas-aaa"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "api-keys.json")
			if c.file != "" {
				if err := os.WriteFile(path, []byte(c.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("ATLAS_API_KEYS_PATH", path)
			if got := loadBearerToken(); got != c.want {
				t.Errorf("loadBearerToken = %q, want %q", got, c.want)
			}
		})
	}
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

// The contract's task_mode is the client's selection, and an unselected
// caller declares work. The message is sent as typed, and the TUI invents no
// expected outputs or verification: it knows none.
func TestTaskContractModeIsClientSelected(t *testing.T) {
	cases := []struct {
		name    string
		mode    taskMode
		message string
		want    string
	}{
		{"no selection is work", "", "hi", "work"},
		{"an ordinary message is work", taskModeWork, "What does this repository do?", "work"},
		{"the question control sends question", taskModeQuestion, "Create app.py.", "question"},
		// The same sentence goes either way; only the selection differs.
		{"a sentence sent as work", taskModeWork, "Fix the failing test.", "work"},
		{"the same sentence sent as a question", taskModeQuestion, "Fix the failing test.", "question"},
		{"work that names files", taskModeWork, "Write four files.", "work"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := captureContract(t, c.mode, c.message)
			contract := contractOf(t, body)
			if contract["task_mode"] != c.want {
				t.Errorf("task_mode = %v, want %s", contract["task_mode"], c.want)
			}
			if body["message"] != c.message {
				t.Errorf("message = %q, want it unchanged", body["message"])
			}
			if _, ok := contract["expected_outputs"]; ok {
				t.Errorf("the TUI invented expected_outputs: %v", contract["expected_outputs"])
			}
			if _, ok := contract["verification"]; ok {
				t.Errorf("the TUI invented verification: %v", contract["verification"])
			}
		})
	}
}

// Question mode is one-shot: the next ordinary message is work again.
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
