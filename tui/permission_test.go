// Tests for the interactive permission approval modal — the
// "permission_request" SSE handler, the y/a/n input gating, and the
// /v1/permission post-back.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// A permission_request for a tool NOT on the session allowlist raises the
// modal and captures the turn's session id.
func TestPermissionRequestRaisesModal(t *testing.T) {
	m := sized(80, 30)
	m.turnSessionID = "turn-abc"
	updated, _ := m.Update(chatStreamMsg{ev: chatEvent{Type: "permission_request", Data: json.RawMessage(
		`{"tool_name": "run_command", "message": "Run command: npm install", "tool_call_id": "call_3",
		  "args": {"command": "npm install"}}`)}})
	got := updated.(tuiModel).pendingPerm
	if got == nil {
		t.Fatal("pendingPerm is nil — modal should be up")
	}
	if got.toolName != "run_command" || got.message != "Run command: npm install" ||
		got.toolCallID != "call_3" || got.sessionID != "turn-abc" {
		t.Errorf("modal = %+v, want the request's tool, message and call id, and the turn's session id", *got)
	}
}

// A tool already approved "for session" auto-answers allow without a modal.
func TestPermissionRequestAutoAllowsWhitelistedTool(t *testing.T) {
	m := sized(80, 30)
	// The auto-answer is posted in the background; a closed port on this
	// machine keeps it off the network.
	m.proxyURL = "http://127.0.0.1:1"
	m.turnSessionID = "turn-abc"
	m.sessionAllowedTools["run_command"] = true
	updated, _ := m.Update(chatStreamMsg{ev: chatEvent{Type: "permission_request", Data: json.RawMessage(
		`{"tool_name": "run_command", "message": "Run command: npm install", "tool_call_id": "call_3",
		  "args": {"command": "npm install"}}`)}})
	mu := updated.(tuiModel)
	if mu.pendingPerm != nil {
		t.Fatal("whitelisted tool should auto-allow without raising the modal")
	}
	if len(mu.chat) != 1 || !strings.Contains(mu.chat[0].Body, "auto-allowed") {
		t.Fatalf("chat = %+v, want an auto-allowed system row", mu.chat)
	}
	if !mu.chat[0].Echo {
		t.Error("decision record should be Echo=true so it stays out of agent history")
	}
}

// permServer captures the last /v1/permission body for assertions.
func permServer(t *testing.T, got *map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/permission" {
			http.Error(w, "wrong path", 404)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		*got = body
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"delivered": true})
	}))
}

// Each decision key posts its answer for this turn and call: 'y' allows once,
// 'a' allows for the session, 'n' and esc deny.
func TestPermissionModalKeyDecisions(t *testing.T) {
	cases := []struct {
		name         string
		key          tea.KeyMsg
		wantDecision string
		wantScope    string
	}{
		{"y", keyMsg("y"), "allow", "once"},
		{"a", keyMsg("a"), "allow", "session"},
		{"n", keyMsg("n"), "deny", "once"},
		{"esc", tea.KeyMsg(tea.Key{Type: tea.KeyEsc}), "deny", "once"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]string
			srv := permServer(t, &got)
			defer srv.Close()

			m := sized(80, 30)
			m.proxyURL = srv.URL
			m.pendingPerm = &permPrompt{
				toolName: "run_command", message: "Run command: npm install",
				toolCallID: "call_3", sessionID: "turn-abc",
			}
			_, cmd := m.Update(tc.key)
			if cmd == nil {
				t.Fatal("decision must return a post-back Cmd")
			}
			cmd() // fire the POST synchronously
			if got == nil {
				t.Fatal("no /v1/permission POST received")
			}
			if got["decision"] != tc.wantDecision || got["scope"] != tc.wantScope {
				t.Errorf("decision/scope = %q/%q, want %q/%q", got["decision"], got["scope"], tc.wantDecision, tc.wantScope)
			}
			if got["session_id"] != "turn-abc" || got["tool_call_id"] != "call_3" {
				t.Errorf("correlation = %+v, want session turn-abc / call_3", got)
			}
		})
	}
}

// An allow key closes the modal and leaves an "allowed" record in the
// transcript. Only 'a' adds the tool to the session allowlist.
func TestAnAllowKeyClosesTheModalWithAnAllowedRecord(t *testing.T) {
	cases := []struct {
		name          string
		key           tea.KeyMsg
		wantAllowlist bool
	}{
		{"y", keyMsg("y"), false},
		{"a", keyMsg("a"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(80, 30)
			m.pendingPerm = &permPrompt{
				toolName: "run_command", message: "Run command: npm install",
				toolCallID: "call_3", sessionID: "turn-abc",
			}
			updated, _ := m.Update(tc.key)
			mu := updated.(tuiModel)
			if mu.pendingPerm != nil {
				t.Fatal("pendingPerm should be cleared after a decision")
			}
			if mu.sessionAllowedTools["run_command"] != tc.wantAllowlist {
				t.Errorf("run_command on the session allowlist = %v, want %v",
					mu.sessionAllowedTools["run_command"], tc.wantAllowlist)
			}
			if len(mu.chat) == 0 || !strings.HasPrefix(mu.chat[len(mu.chat)-1].Body, "allowed") {
				t.Errorf("chat tail = %+v, want a %q record", mu.chat, "allowed")
			}
		})
	}
}

// A deny key ('n' or esc) closes the modal and adds no local row: the proxy's
// permission_denied event renders it. A deny adds nothing to the session
// allowlist.
func TestADenyKeyClosesTheModalWithoutALocalRow(t *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyMsg
	}{
		{"n", keyMsg("n")},
		{"esc", tea.KeyMsg(tea.Key{Type: tea.KeyEsc})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(80, 30)
			m.pendingPerm = &permPrompt{
				toolName: "run_command", message: "Run command: npm install",
				toolCallID: "call_3", sessionID: "turn-abc",
			}
			updated, _ := m.Update(tc.key)
			mu := updated.(tuiModel)
			if mu.pendingPerm != nil {
				t.Fatal("pendingPerm should be cleared after a decision")
			}
			if mu.sessionAllowedTools["run_command"] {
				t.Errorf("run_command on the session allowlist = %v, want false",
					mu.sessionAllowedTools["run_command"])
			}
			if len(mu.chat) != 0 {
				t.Errorf("expected no local row, got %+v", mu.chat)
			}
		})
	}
}

// While the modal is up, non-decision keys are swallowed and never reach the
// textarea.
func TestPermissionModalSwallowsOtherKeys(t *testing.T) {
	m := sized(80, 30)
	m.pendingPerm = &permPrompt{toolName: "run_command", toolCallID: "call_3", sessionID: "s1"}
	updated, _ := m.Update(keyMsg("q"))
	mu := updated.(tuiModel)
	if mu.pendingPerm == nil {
		t.Error("a non-decision key must not resolve the modal")
	}
	if mu.input.Value() != "" {
		t.Errorf("input = %q, want empty (key swallowed, not typed)", mu.input.Value())
	}
}

func TestSortedAllowedTools(t *testing.T) {
	got := sortedAllowedTools(map[string]bool{"zed": true, "abc": true, "off": false})
	if strings.Join(got, ",") != "abc,zed" {
		t.Errorf("sortedAllowedTools = %v, want [abc zed] (false entries dropped)", got)
	}
	if sortedAllowedTools(nil) != nil {
		t.Error("empty allowlist should yield nil")
	}
}

// Ctrl+C while the modal is up cancels the turn (clearing the modal) rather
// than being swallowed, so the user can always abort mid-prompt.
func TestPermissionModalCtrlCCancelsTurn(t *testing.T) {
	m := sized(80, 30)
	_, cancel := context.WithCancel(context.Background())
	m.turnActive = true
	m.turnCancel = cancel
	m.pendingPerm = &permPrompt{
		toolName: "run_command", toolCallID: "call_1", sessionID: "turn-x",
	}
	updated, _ := m.Update(keyMsg("ctrl+c"))
	mu := updated.(tuiModel)
	if mu.pendingPerm != nil {
		t.Error("Ctrl+C should clear the permission modal")
	}
	if mu.turnActive {
		t.Error("Ctrl+C should cancel the in-flight turn")
	}
}
