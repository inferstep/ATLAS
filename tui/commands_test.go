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

func newTestModel() *tuiModel {
	m := newTUIModel("http://localhost:8090")
	return &m
}

func TestSlashHelpEchoesHelpText(t *testing.T) {
	m := newTestModel()
	consumed, cmd, quit := m.handleSlash("/help")
	if !consumed {
		t.Fatal("consumed = false, want true")
	}
	if cmd != nil {
		t.Errorf("/help should not return a tea.Cmd")
	}
	if quit {
		t.Errorf("/help should not signal quit")
	}
	// Two messages: the echo of "/help" and the help-body.
	if len(m.chat) != 2 {
		t.Fatalf("chat length = %d, want 2", len(m.chat))
	}
	if !strings.Contains(m.chat[1].Body, "Slash commands") {
		t.Errorf("help body missing header: %q", m.chat[1].Body)
	}
}

func TestSlashQuitSignalsQuit(t *testing.T) {
	m := newTestModel()
	consumed, cmd, quit := m.handleSlash("/quit")
	if !consumed || !quit {
		t.Errorf("consumed=%v quit=%v, want both true", consumed, quit)
	}
	if cmd == nil {
		t.Errorf("expected tea.Quit cmd")
	}
}

func TestSlashAddPopulatesContext(t *testing.T) {
	m := newTestModel()
	m.handleSlash("/add foo.go bar.go")
	if !m.contextFiles["foo.go"] || !m.contextFiles["bar.go"] {
		t.Errorf("contextFiles = %v", m.contextFiles)
	}
	// Adding the same files again should report "no new files added"
	// without duplicating entries.
	m.handleSlash("/add foo.go")
	count := 0
	for range m.contextFiles {
		count++
	}
	if count != 2 {
		t.Errorf("contextFiles size = %d, want 2 (no dup)", count)
	}
}

func TestSlashDropRemovesContext(t *testing.T) {
	m := newTestModel()
	m.handleSlash("/add foo.go bar.go")
	m.handleSlash("/drop foo.go")
	if m.contextFiles["foo.go"] {
		t.Errorf("foo.go should be dropped")
	}
	if !m.contextFiles["bar.go"] {
		t.Errorf("bar.go should remain")
	}
}

func TestContextSuffixOmitsHintWhenEmpty(t *testing.T) {
	m := newTestModel()
	if got := m.contextSuffix(); got != "" {
		t.Errorf("empty context suffix = %q, want empty", got)
	}
	m.handleSlash("/add foo.go")
	got := m.contextSuffix()
	if !strings.Contains(got, "foo.go") {
		t.Errorf("suffix = %q, missing foo.go", got)
	}
	if !strings.Contains(got, "atlas-tui context") {
		t.Errorf("suffix = %q, missing marker tag", got)
	}
}

func TestSlashUnknownReportsErrorNotPassthrough(t *testing.T) {
	m := newTestModel()
	consumed, _, _ := m.handleSlash("/diffx")
	if !consumed {
		t.Fatal("unknown slash should still be consumed (not passed to agent)")
	}
	if len(m.chat) < 2 {
		t.Fatalf("expected echo + error, got %d msgs", len(m.chat))
	}
	if !strings.Contains(m.chat[1].Body, "unknown command") {
		t.Errorf("missing unknown-command notice: %q", m.chat[1].Body)
	}
}

func TestNonSlashInputNotConsumed(t *testing.T) {
	m := newTestModel()
	consumed, _, _ := m.handleSlash("fix the snake game")
	if consumed {
		t.Errorf("plain input should not be consumed by slash handler")
	}
	if len(m.chat) != 0 {
		t.Errorf("plain input should not append to chat from slash handler")
	}
}

func TestSlashRunRequiresArgument(t *testing.T) {
	m := newTestModel()
	consumed, cmd, _ := m.handleSlash("/run")
	if !consumed {
		t.Fatal("consumed = false")
	}
	if cmd != nil {
		t.Errorf("/run with no arg should not run anything")
	}
	// echo + error message
	if len(m.chat) != 2 || !strings.Contains(m.chat[1].Body, "/run requires") {
		t.Errorf("chat = %v, want error about missing arg", m.chat)
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

// /ask with no message asks for one and sends nothing.
func TestAskWithoutAMessageSendsNothing(t *testing.T) {
	m := &tuiModel{workingDir: t.TempDir()}
	consumed, cmd, _ := m.handleSlash("/ask")
	if !consumed {
		t.Fatal("not consumed")
	}
	if cmd != nil {
		t.Error("/ask with no message sent a request")
	}
	if m.pendingTaskMode != "" {
		t.Errorf("pendingTaskMode=%q, want unset", m.pendingTaskMode)
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
