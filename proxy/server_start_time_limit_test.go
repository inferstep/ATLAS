package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A server that is started in the foreground never exits, so the sandbox
// stops it at the time limit. The answer of /shell for such a command, as
// the sandbox gives it: the error stream holds what the command wrote, and
// the time limit is in the fields `timed_out` and `outcome`.
const shellAnswerForAServerStoppedAtTheTimeLimit = `{"success":false,` +
	`"stdout":" * Serving Flask app 'app'\n * Running on http://127.0.0.1:5000\n","stderr":"",` +
	`"exit_code":-15,"elapsed_ms":30021,"timed_out":true,"outcome":"timed_out","peak_memory_bytes":31457280}`

func resultOfRunCommand(t *testing.T, shellAnswer string) *ToolResult {
	t.Helper()
	sandbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(shellAnswer))
	}))
	defer sandbox.Close()
	ctx := &AgentContext{WorkingDir: "/workspace", SandboxURL: sandbox.URL}
	result, err := runCommandTool().Execute(json.RawMessage(`{"command":"python app.py"}`), ctx)
	if err != nil {
		t.Fatalf("run_command: %v", err)
	}
	return result
}

func TestAServerStartThatTheSandboxStoppedAtTheTimeLimitIsABlockedServerStart(t *testing.T) {
	result := resultOfRunCommand(t, shellAnswerForAServerStoppedAtTheTimeLimit)
	if result.Success {
		t.Fatal("a command that was stopped at the time limit counted as a pass")
	}
	if !blockedServerStart(result) {
		t.Errorf("a server start that ran into the time limit is read as a red test: the gate then tells the model "+
			"to fix its code and to run the same command again, which can never exit. error=%q data=%s",
			result.Error, result.Data)
	}
}

// A test that failed and ended by itself, as the sandbox answers for it.
const shellAnswerForATestThatFailed = `{"success":false,"stdout":"","stderr":"AssertionError: expected 3, got 4\n",` +
	`"exit_code":1,"elapsed_ms":412,"timed_out":false,"outcome":"completed","peak_memory_bytes":20971520}`

func TestTheGateGivesAServerThatTheTimeLimitStoppedTheAdviceForAServer(t *testing.T) {
	state := &runState{}
	ctx := &AgentContext{WorkingDir: t.TempDir()}
	state.observeFailedCheck(ctx, 1, "python app.py", commandEvidence{},
		resultOfRunCommand(t, shellAnswerForAServerStoppedAtTheTimeLimit))
	if !state.serverStartBlocked {
		t.Fatal("the gate's step for a failed check did not see that the time limit stopped a server")
	}
	advice := verificationRejection(state.sawFailedVerification, state.serverStartBlocked, "")
	for _, want := range []string{"run_background", "servers do not exit"} {
		if !strings.Contains(advice, want) {
			t.Errorf("the advice for a server has no %q:\n%s", want, advice)
		}
	}
}

func TestTheGateGivesATestThatFailedTheAdviceForARedTest(t *testing.T) {
	state := &runState{}
	ctx := &AgentContext{WorkingDir: t.TempDir()}
	state.observeFailedCheck(ctx, 1, "pytest", commandEvidence{}, resultOfRunCommand(t, shellAnswerForATestThatFailed))
	if state.serverStartBlocked {
		t.Fatal("a test that failed was taken for a server that never exits")
	}
	advice := verificationRejection(state.sawFailedVerification, state.serverStartBlocked, "")
	if strings.Contains(advice, "servers do not exit") || !strings.Contains(advice, "FAILED") {
		t.Errorf("a test that failed did not get the advice for a red test:\n%s", advice)
	}
}
