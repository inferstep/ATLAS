package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// There is one delivery rule, in the proxy, and nothing a client sends selects
// another. The TUI used to offer /candidate-policy and send its value. The
// proxy ignores that field now, so the control would have changed nothing and
// a header naming a mode would have been false.

func TestTheTUISendsNoCandidatePolicy(t *testing.T) {
	for _, mode := range []taskMode{taskModeWork, taskModeQuestion} {
		tc := contractOf(t, captureContract(t, mode, "Make solve fast."))
		if v, ok := tc["candidate_policy"]; ok {
			t.Errorf("%s: the contract carries candidate_policy=%v", mode, v)
		}
		if tc["task_mode"] != string(mode) {
			t.Errorf("task_mode=%v, want %s", tc["task_mode"], mode)
		}
	}
}

func TestNoCandidatePolicyControlRemains(t *testing.T) {
	if strings.Contains(slashCommandHelp, "/candidate-policy") {
		t.Error("help still lists /candidate-policy")
	}
	m := &tuiModel{}
	m.handleSlash("/candidate-policy automatic")
	if last := m.chat[len(m.chat)-1]; !strings.Contains(last.Body, "unknown command") {
		t.Errorf("/candidate-policy is still a command: %q", last.Body)
	}
	h := renderHeader("http://p", "/w", "default", false, 0, 200)
	if strings.Contains(h, "candidates:") {
		t.Errorf("the header still names a candidate mode: %q", h)
	}
}

// A request the proxy refuses (the lens cannot score, for example) is shown
// as the proxy's own sentence, not as a status code and raw JSON.
func TestARefusedRequestShowsTheProxysSentence(t *testing.T) {
	const detail = "ATLAS needs the geometric lens for every request, and it is unreachable at http://lens. Run `atlas doctor`."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		body, _ := json.Marshal(map[string]string{
			"error": "dependency_unavailable", "detail": detail, "api_version": "1.0.0"})
		w.Write(body)
	}))
	defer srv.Close()
	err := sendChatOpts(context.Background(), srv.URL, "add a toggle", t.TempDir(),
		"default", "s", nil, demoOpts{}, make(chan chatEvent, 4))
	if err == nil || err.Error() != detail {
		t.Fatalf("error %v, want the proxy's sentence", err)
	}
}
