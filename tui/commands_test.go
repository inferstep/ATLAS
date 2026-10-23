// Tests for slash command dispatch (PC-062 step 4).
//
// handleSlash is a pure-ish function from input string → (state mutation,
// tea.Cmd). Tests pin the dispatch table and the local-state mutations
// (context add/drop, help/quit signaling). Shell-out commands are not
// executed here — that's covered by step 7's integration tests.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Input that starts with "/" is a command: it is consumed (never sent to the
// agent as a message), echoed as a chat row, and answered in the chat.
func TestHandleSlash(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		consumed bool
		sends    bool     // returns a command to run
		quit     bool     // asks the program to quit
		rows     []string // text each chat row contains, in order: the echo, then the answer
	}{
		{"help lists the commands", "/help", true, false, false, []string{"/help", "Slash commands"}},
		{"quit quits", "/quit", true, true, true, []string{"/quit"}},
		{"an unknown command is an error", "/diffx", true, false, false, []string{"/diffx", "unknown command"}},
		{"run needs a command", "/run", true, false, false, []string{"/run", "/run requires"}},
		{"ask needs a message", "/ask", true, false, false, []string{"/ask", "/ask requires"}},
		{"plain input is not a command", "fix the snake game", false, false, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newTUIModel("http://localhost:8090")
			consumed, cmd, quit := m.handleSlash(c.input)
			if consumed != c.consumed || (cmd != nil) != c.sends || quit != c.quit {
				t.Errorf("consumed=%v command=%v quit=%v, want %v %v %v",
					consumed, cmd != nil, quit, c.consumed, c.sends, c.quit)
			}
			if len(m.chat) != len(c.rows) {
				t.Fatalf("chat = %+v, want %d row(s)", m.chat, len(c.rows))
			}
			for i, want := range c.rows {
				if !strings.Contains(m.chat[i].Body, want) {
					t.Errorf("row %d = %q, want it to contain %q", i, m.chat[i].Body, want)
				}
			}
			// Only an /ask that sends a message selects question mode.
			if m.pendingTaskMode != "" {
				t.Errorf("pendingTaskMode = %q, want unset", m.pendingTaskMode)
			}
		})
	}
}

// /add and /drop keep the list of context files. The suffix sent with a
// message names them and is empty while the list is.
func TestSlashAddAndDropKeepTheContextFiles(t *testing.T) {
	m := newTUIModel("http://localhost:8090")
	if got := m.contextSuffix(); got != "" {
		t.Errorf("suffix with no context files = %q, want empty", got)
	}
	m.handleSlash("/add foo.go bar.go")
	m.handleSlash("/add foo.go") // already there: nothing new
	if len(m.contextFiles) != 2 || !m.contextFiles["foo.go"] || !m.contextFiles["bar.go"] {
		t.Errorf("contextFiles = %v, want foo.go and bar.go", m.contextFiles)
	}
	if got := m.contextSuffix(); !strings.Contains(got, "foo.go") || !strings.Contains(got, "atlas-tui context") {
		t.Errorf("suffix = %q, want the marker tag and foo.go", got)
	}
	m.handleSlash("/drop foo.go")
	if m.contextFiles["foo.go"] || !m.contextFiles["bar.go"] {
		t.Errorf("after /drop foo.go: contextFiles = %v, want only bar.go", m.contextFiles)
	}
}

// /ask declares one request a question and forwards only the message.
func TestAskSelectsQuestionModeOnce(t *testing.T) {
	var got map[string]interface{}
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		close(done)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"done\",\"summary\":\"ok\"}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	m := &tuiModel{workingDir: t.TempDir(), proxyURL: srv.URL,
		chatEvents: make(chan chatEvent, 64)}
	consumed, cmd, quit := m.handleSlash("/ask what does solve.py do?")
	if !consumed || quit {
		t.Fatalf("consumed=%v quit=%v", consumed, quit)
	}
	if cmd == nil {
		t.Fatal("/ask did not send anything")
	}
	go cmd()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never arrived")
	}
	tc, ok := got["task_contract"].(map[string]interface{})
	if !ok {
		t.Fatalf("no task_contract: %v", got)
	}
	if tc["task_mode"] != "question" {
		t.Errorf("task_mode=%v, want question", tc["task_mode"])
	}
	if got["message"] != "what does solve.py do?" {
		t.Errorf("the control syntax reached the model: %v", got["message"])
	}
	// One-shot: consumed by that send, so the next turn is work again.
	if m.pendingTaskMode != "" {
		t.Errorf("pendingTaskMode=%q after the send; it must be one-shot", m.pendingTaskMode)
	}
	if m.takeTaskMode() != taskModeWork {
		t.Error("the next ordinary message is not work")
	}
	// The command word is not part of what the model sees.
	if m.lastUserMsg != "what does solve.py do?" {
		t.Errorf("forwarded %q; the control syntax must not reach the model", m.lastUserMsg)
	}
	// The chat row shows the message, not the command.
	last := m.chat[len(m.chat)-1]
	if last.Body != "what does solve.py do?" || last.Echo {
		t.Errorf("chat row is %+v", last)
	}
}

// Every owned sender declares a mode, so a new one cannot silently omit it.
func TestEveryOwnedSenderDeclaresATaskMode(t *testing.T) {
	src, err := os.ReadFile("chat.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// One builder, one place the contract is attached.
	if n := strings.Count(body, "agentRequest{"); n != 1 {
		t.Errorf("%d agentRequest constructors, want one: a second sender could "+
			"omit the contract silently", n)
	}
	if n := strings.Count(body, "TaskContract:"); n != 1 {
		t.Errorf("%d contract attachments, want one", n)
	}
	// Every in-repo caller of the builder passes demoOpts, which carries the
	// mode; the model's own send path sets it from takeTaskMode.
	m, _ := os.ReadFile("model.go")
	if !strings.Contains(string(m), "taskMode: declared") {
		t.Error("the model send path no longer declares a task mode")
	}
	if !strings.Contains(string(m), "func (m *tuiModel) takeTaskMode()") {
		t.Error("the one-shot selector is gone")
	}
}

// /copy N copies the last N messages, joined by blank lines: the whole
// transcript when N is larger than it, and nothing when N is not positive or
// there are no messages.
func TestCollectLastMessagesTakesTheLastN(t *testing.T) {
	five := []chatMessage{
		{Role: roleUser, Body: "one"},
		{Role: roleAssistant, Body: "two"},
		{Role: roleUser, Body: "three"},
		{Role: roleAssistant, Body: "four"},
		{Role: roleUser, Body: "five"},
	}
	cases := []struct {
		name string
		chat []chatMessage
		n    int
		want string
	}{
		{"the last two", five, 2, "four\n\nfive"},
		{"all five", five, 5, "one\n\ntwo\n\nthree\n\nfour\n\nfive"},
		{"more than there are gives all", five, 10, "one\n\ntwo\n\nthree\n\nfour\n\nfive"},
		{"zero gives nothing", five, 0, ""},
		{"no messages gives nothing", nil, 3, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := collectLastMessages(c.chat, c.n); got != c.want {
				t.Errorf("collectLastMessages(n=%d) = %q, want %q", c.n, got, c.want)
			}
		})
	}
}
