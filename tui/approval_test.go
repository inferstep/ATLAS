// Approval prompts are answered for what they show, deletions one file at a
// time, and every request to the proxy carries the credential it checks.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/itigges22/atlas-tui/internal/proxyclient"
)

func deleteReqMsg(callID string, oneTimeOnly bool) chatStreamMsg {
	data, _ := json.Marshal(map[string]interface{}{
		"tool_name":     "delete_file",
		"message":       "Allow this one deletion? important.db",
		"tool_call_id":  callID,
		"one_time_only": oneTimeOnly,
	})
	return chatStreamMsg{ev: proxyclient.ChatEvent{Type: "permission_request", Data: data}}
}

// One "allow for session" answered every later deletion without the user
// seeing which file. The second case is a proxy that does not mark the
// request one_time_only.
func TestADeletionIsNeverApprovedForTheSession(t *testing.T) {
	for _, marked := range []bool{true, false} {
		var got map[string]string
		srv := permServer(t, &got)
		m := sized(100, 30)
		m.proxyURL = srv.URL
		m.turnSessionID = "turn-1"

		updated, _ := m.Update(deleteReqMsg("call_1", marked))
		mu := updated.(tuiModel)
		if mu.pendingPerm == nil {
			t.Fatalf("marked=%v: no modal for a deletion", marked)
		}
		if strings.Contains(ansi.Strip(mu.View()), "allow for session") {
			t.Errorf("marked=%v: a deletion offered allow for session", marked)
		}
		updated, cmd := mu.Update(keyMsg("a"))
		mu = updated.(tuiModel)
		cmd()
		if got["decision"] != "allow" || got["scope"] != "once" {
			t.Errorf("marked=%v: 'a' on a deletion posted %v, want allow/once", marked, got)
		}
		if mu.sessionAllowedTools["delete_file"] {
			t.Errorf("marked=%v: delete_file entered the session allowlist", marked)
		}
		// Even an allowlist entry left from elsewhere answers nothing.
		mu.sessionAllowedTools["delete_file"] = true
		updated, _ = mu.Update(deleteReqMsg("call_2", marked))
		if updated.(tuiModel).pendingPerm == nil {
			t.Errorf("marked=%v: the next deletion was answered without asking", marked)
		}
		srv.Close()
	}
}

func TestTheSessionAllowlistNeverCarriesDeletions(t *testing.T) {
	got := sortedAllowedTools(map[string]bool{"run_command": true, "delete_file": true})
	if len(got) != 1 || got[0] != "run_command" {
		t.Errorf("session_allowed_tools = %v", got)
	}
}

func TestANewSessionStartsWithNoApprovals(t *testing.T) {
	m := sized(80, 30)
	m.sessionAllowedTools["run_command"] = true
	m.startNewSession()
	if len(m.sessionAllowedTools) != 0 {
		t.Errorf("approvals survived a new session: %v", m.sessionAllowedTools)
	}
}

// A prompt cut to one row per line hid the tail of a command. The whole
// command is shown; one too long for the screen keeps its last lines.
func TestTheApprovalPromptShowsTheEndOfTheCommand(t *testing.T) {
	chain := "Run command: cd tests && python3 -m pytest -q test_api.py test_models.py " +
		"test_views.py 2>&1 | tail -n 40 && cd .. && rm -rf src/legacy data/ && git checkout -- ."
	heredoc := "Run command: cat > app.py <<'EOF'\n" + strings.Repeat("print('x')\n", 80) +
		"EOF\n&& git checkout -- ."
	for name, msg := range map[string]string{"chain": chain, "heredoc": heredoc} {
		pp := &permPrompt{toolName: "run_command", message: msg, toolCallID: "c", sessionID: "s"}
		var body strings.Builder
		for _, ln := range permPromptLines(pp, 60, 15) {
			body.WriteString(ansi.Strip(ln))
		}
		shown := body.String()
		switch name {
		case "chain":
			if !strings.Contains(shown, strings.ReplaceAll(chain, "\n", "")) {
				t.Errorf("chain: the command is not shown whole: %q", shown)
			}
		case "heredoc":
			if !strings.Contains(shown, "&& git checkout -- .") || !strings.Contains(shown, "lines not shown") {
				t.Errorf("heredoc: the end or the cut is not shown: %q", shown)
			}
		}
		m := withStages(t, 100, 30, 2)
		m.pendingPerm = pp
		if h := lipgloss.Height(m.View()); h != 30 {
			t.Errorf("%s: view height with the modal = %d, want 30", name, h)
		}
	}
}

// The TUI's own requests to the proxy (calibration status, the demo's model
// lookup and its health check) carry the header the client's SetProxyAuth
// gives, so a proxy with a service token accepts them. Which token that header
// holds is checked in internal/proxyclient.
func TestEveryTUIRequestToTheProxyCarriesTheServiceToken(t *testing.T) {
	keys := filepath.Join(t.TempDir(), "api-keys.json")
	if err := os.WriteFile(keys, []byte(`{"api_key":"sk-other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATLAS_API_KEYS_PATH", keys)
	// The service token when there is one, else the key above: never empty.
	probe := httptest.NewRequest(http.MethodGet, "/", nil)
	proxyclient.SetProxyAuth(probe)
	want := probe.Header.Get("Authorization")
	if want == "" {
		t.Fatal("SetProxyAuth set no header, so this test would prove nothing")
	}

	var mu sync.Mutex
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		if r.Header.Get("Authorization") != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
	}))
	defer srv.Close()

	fetchCalibrationStatusCmd(srv.URL)()
	fetchDemoModelIdentity(srv.URL)
	proxySupportsRawDemo(srv.URL)

	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/v1/calibration/status", "/v1/models", "/health"} {
		if got := seen[path]; got != want {
			t.Errorf("%s sent %q, want %q", path, got, want)
		}
	}
}
