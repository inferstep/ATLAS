// Tests for the safety deny-list applied in executeToolCall.

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
	"sync"
	"testing"
	"time"
)

func newPermCtx(dir string) *AgentContext {
	return &AgentContext{
		WorkingDir:    dir,
		FilesRead:     map[string]string{},
		FileReadTimes: map[string]time.Time{},
		SessionWrites: map[string]bool{},
	}
}

// write_file to a sensitive target (.env) must be refused in every mode,
// and nothing may land on disk.
func TestExecuteToolCallDeniesEnvWrite(t *testing.T) {
	dir := t.TempDir()
	ctx := newPermCtx(dir)
	res := executeToolCall("write_file", json.RawMessage(`{"path":".env","content":"SECRET=1"}`), ctx)
	if res.Success {
		t.Fatalf("write_file to .env succeeded, want denial: %+v", res)
	}
	if !strings.Contains(res.Error, "blocked by safety rule") {
		t.Errorf("error %q does not mention the safety rule", res.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
		t.Errorf(".env exists on disk after denied write")
	}
}

// Sensitive key material is refused, including in subdirectories.
func TestExecuteToolCallDeniesKeyMaterialWrites(t *testing.T) {
	dir := t.TempDir()
	ctx := newPermCtx(dir)
	for _, path := range []string{
		"server.pem", "id_rsa.key", "aws_credentials.json",
		"certs/server.pem", "keys/id_rsa.key", "config/aws_credentials.json",
	} {
		input, _ := json.Marshal(map[string]string{"path": path, "content": "secret"})
		res := executeToolCall("write_file", json.RawMessage(input), ctx)
		if res.Success {
			t.Errorf("write_file to %q succeeded, want denial", path)
		}
	}
}

// Files whose names merely resemble a sensitive one must NOT be blocked.
func TestDenyWritePathAllowsLookalikes(t *testing.T) {
	for _, path := range []string{
		".env.example", ".envrc", "staging.env", "deploy/production.env",
		"src/app.envoy.yaml", "docs/environment.md", "pemphigus.txt",
	} {
		if reason := denyWritePathReason(path); reason != "" {
			t.Errorf("denyWritePathReason(%q) = %q, want allowed", path, reason)
		}
	}
	for _, path := range []string{".env", "certs/tls.pem", "a/b/c/service.key"} {
		if reason := denyWritePathReason(path); reason == "" {
			t.Errorf("denyWritePathReason(%q) allowed, want denied", path)
		}
	}
}

// Only destructive root-scoped commands are blocked; in-workspace commands
// and commands that merely mention a dangerous string are allowed.
func TestDenyCommandReason(t *testing.T) {
	denied := []string{
		"rm -rf /", "rm -rf /*", "rm -fr / ", "sudo rm -rf /",
		"mkfs.ext4 /dev/sda1", "dd if=/dev/zero of=/dev/sda",
	}
	for _, cmd := range denied {
		if denyCommandReason(cmd) == "" {
			t.Errorf("denyCommandReason(%q) allowed, want denied", cmd)
		}
	}
	allowed := []string{
		"rm -rf /workspace/build", "rm -rf ./node_modules", "rm -rf /tmp/scratch",
		"git clean -fdx", "echo 'rm -rf /' > warn.txt", "make", "npm run build",
		"grep mkfs docs.txt", "dd if=input.bin of=output.bin",
	}
	for _, cmd := range allowed {
		if reason := denyCommandReason(cmd); reason != "" {
			t.Errorf("denyCommandReason(%q) = %q, want allowed", cmd, reason)
		}
	}
}

// A normal write is not affected by the deny-list.
func TestExecuteToolCallAllowsNormalWrite(t *testing.T) {
	dir := t.TempDir()
	ctx := newPermCtx(dir)
	res := executeToolCall("write_file", json.RawMessage(`{"path":"notes.txt","content":"grocery list:\n- apples\n- flour\n"}`), ctx)
	if !res.Success {
		t.Fatalf("normal write_file failed: %+v", res)
	}
	data, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil || !strings.Contains(string(data), "apples") {
		t.Errorf("notes.txt missing or wrong content: %q err=%v", string(data), err)
	}
}

func permCtx(sessionID string) (*AgentContext, context.CancelFunc) {
	ctx := context.Background()
	reqCtx, cancel := context.WithCancel(ctx)
	return &AgentContext{
		PassID:        sessionID,
		Ctx:           reqCtx,
		FilesRead:     map[string]string{},
		FileReadTimes: map[string]time.Time{},
		SessionWrites: map[string]bool{},
	}, cancel
}

func postDecision(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/permission", strings.NewReader(body))
	w := httptest.NewRecorder()
	handlePermission(w, r)
	return w
}

// An allow decision unblocks awaitPermission and returns true.
func TestAwaitPermissionAllow(t *testing.T) {
	ctx, cancel := permCtx("sess-allow")
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- awaitPermission(ctx, "run_command", "call_1", json.RawMessage(`{"command":"ls"}`)) }()

	// Wait for the pending entry to register, then answer it.
	waitForPending(t, "sess-allow", "call_1")
	w := postDecision(t, `{"session_id":"sess-allow","tool_call_id":"call_1","decision":"allow","scope":"once"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("decision POST status = %d, want 200", w.Code)
	}
	if got := <-done; !got {
		t.Error("awaitPermission returned false for an allow decision")
	}
}

// A deny decision returns false and does not whitelist the tool.
//
// The target has to be real: a deletion is now confirmed against an inspected
// object, so a path that does not exist is refused before anyone is asked.
func TestAwaitPermissionDeny(t *testing.T) {
	ctx, cancel := permCtx("sess-deny")
	defer cancel()
	ctx.WorkingDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(ctx.WorkingDir, "x"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- awaitPermission(ctx, "delete_file", "call_2", json.RawMessage(`{"path":"x"}`)) }()

	waitForPending(t, "sess-deny", "call_2")
	postDecision(t, `{"session_id":"sess-deny","tool_call_id":"call_2","decision":"deny"}`)
	if got := <-done; got {
		t.Error("awaitPermission returned true for a deny decision")
	}
	if ctx.isToolAllowed("delete_file") {
		t.Error("deny should not whitelist the tool")
	}
}

// A session-scoped allow whitelists the tool for the rest of the turn.
func TestAwaitPermissionSessionScopeWhitelists(t *testing.T) {
	ctx, cancel := permCtx("sess-scope")
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- awaitPermission(ctx, "run_command", "call_3", json.RawMessage(`{"command":"ls"}`)) }()

	waitForPending(t, "sess-scope", "call_3")
	postDecision(t, `{"session_id":"sess-scope","tool_call_id":"call_3","decision":"allow","scope":"session"}`)
	if got := <-done; !got {
		t.Fatal("awaitPermission returned false for an allow decision")
	}
	if !ctx.isToolAllowed("run_command") {
		t.Error("session-scope allow should whitelist run_command for the turn")
	}
}

// Cancelling the request context (client disconnect or /cancel) denies.
func TestAwaitPermissionCancelDenies(t *testing.T) {
	ctx, cancel := permCtx("sess-cancel")
	done := make(chan bool, 1)
	go func() { done <- awaitPermission(ctx, "run_command", "call_4", json.RawMessage(`{}`)) }()
	waitForPending(t, "sess-cancel", "call_4")
	cancel()
	if got := <-done; got {
		t.Error("awaitPermission should deny when the context is cancelled")
	}
}

// A decision for an unknown/already-answered key is a 404 no-op.
func TestHandlePermissionUnknownKey(t *testing.T) {
	w := postDecision(t, `{"session_id":"nope","tool_call_id":"call_9","decision":"allow"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown key status = %d, want 404", w.Code)
	}
}

// A caller without a session id proceeds without prompting (non-TUI path).
func TestAwaitPermissionNoSessionDenies(t *testing.T) {
	// Fail closed: with no session_id there is no channel to answer a
	// prompt, and proceeding would make mode:"default" yolo-equivalent
	// for any client that omits the field. Unattended clients opt in
	// explicitly via mode:"yolo" or session_allowed_tools.
	ctx, cancel := permCtx("")
	defer cancel()
	if awaitPermission(ctx, "run_command", "call_5", json.RawMessage(`{}`)) {
		t.Error("awaitPermission must deny when there is no session id")
	}
}

func waitForPending(t *testing.T, sessionID, callID string) {
	t.Helper()
	key := permKey(sessionID, callID)
	for i := 0; i < 200; i++ {
		if _, ok := pendingPermissions.Load(key); ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending permission %q never registered", key)
}

// Tests for needsPermission — the mode/approval logic behind interactive
// permission prompts.

func TestNeedsPermission(t *testing.T) {
	args := json.RawMessage(`{}`)

	t.Run("yolo mode approves everything", func(t *testing.T) {
		for _, ctx := range []*AgentContext{
			{YoloMode: true},
			{PermissionMode: PermissionYolo},
		} {
			if needsPermission(ctx, "run_command", args) {
				t.Errorf("yolo context still prompts for run_command")
			}
		}
	})

	t.Run("unknown tool always prompts", func(t *testing.T) {
		if !needsPermission(&AgentContext{}, "no_such_tool", args) {
			t.Errorf("unknown tool did not prompt")
		}
	})

	t.Run("read-only tools never prompt", func(t *testing.T) {
		if needsPermission(&AgentContext{}, "read_file", args) {
			t.Errorf("read_file prompted in default mode")
		}
	})

	t.Run("destructive tools prompt in default mode", func(t *testing.T) {
		if !needsPermission(&AgentContext{}, "run_command", args) {
			t.Errorf("run_command did not prompt in default mode")
		}
		if !needsPermission(&AgentContext{}, "write_file", args) {
			t.Errorf("write_file did not prompt in default mode")
		}
	})

	t.Run("session-approved tool skips the prompt", func(t *testing.T) {
		ctx := &AgentContext{}
		ctx.allowToolForTurn("run_command")
		if needsPermission(ctx, "run_command", args) {
			t.Errorf("session-approved run_command still prompts")
		}
		// Approval is per-tool, not global.
		if !needsPermission(ctx, "write_file", args) {
			t.Errorf("write_file inherited run_command's approval")
		}
	})

	t.Run("accept-edits auto-approves edits but not commands", func(t *testing.T) {
		ctx := &AgentContext{PermissionMode: PermissionAcceptEdits}
		for _, tool := range []string{"write_file", "edit_file", "structural_edit", "move_file"} {
			if needsPermission(ctx, tool, args) {
				t.Errorf("%s prompted in accept-edits mode", tool)
			}
		}
		if !needsPermission(ctx, "run_command", args) {
			t.Errorf("run_command did not prompt in accept-edits mode")
		}
	})
}

func TestResolveTrustModeDefault(t *testing.T) {
	os.Unsetenv("ATLAS_TRUST_MODE")
	if m := resolveTrustMode(); m != trustTrusted {
		t.Fatalf("default trust mode = %q, want trusted", m)
	}
}

func TestResolveTrustModeValues(t *testing.T) {
	cases := map[string]trustMode{
		"untrusted":     trustUntrusted,
		"trusted":       trustTrusted,
		"fully-trusted": trustFullyTrusted,
		"fully_trusted": trustFullyTrusted,
		"FULLY-TRUSTED": trustFullyTrusted,
		"":              trustTrusted,
		"nonsense":      trustTrusted, // unrecognized → safe default
	}
	for in, want := range cases {
		os.Setenv("ATLAS_TRUST_MODE", in)
		if got := resolveTrustMode(); got != want {
			t.Errorf("ATLAS_TRUST_MODE=%q → %q, want %q", in, got, want)
		}
	}
	os.Unsetenv("ATLAS_TRUST_MODE")
}

func TestCommandsAllowed(t *testing.T) {
	if trustUntrusted.commandsAllowed() {
		t.Error("untrusted must not allow commands")
	}
	if !trustTrusted.commandsAllowed() {
		t.Error("trusted must allow commands")
	}
	if !trustFullyTrusted.commandsAllowed() {
		t.Error("fully-trusted must allow commands")
	}
}

func TestHostExecutionAllowed(t *testing.T) {
	if trustUntrusted.hostExecutionAllowed() {
		t.Error("untrusted must not allow host execution")
	}
	if trustTrusted.hostExecutionAllowed() {
		t.Error("trusted must NOT allow host execution (sandbox only)")
	}
	if !trustFullyTrusted.hostExecutionAllowed() {
		t.Error("fully-trusted must allow host execution")
	}
}

func TestRunCommandRefusedWhenUntrusted(t *testing.T) {
	tool := runCommandTool()
	ctx := &AgentContext{TrustMode: trustUntrusted, WorkingDir: "/tmp"}
	res, err := tool.Execute([]byte(`{"command":"echo hi"}`), ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success {
		t.Fatal("run_command should be refused under untrusted mode")
	}
	if res.Error != untrustedRefusal {
		t.Fatalf("expected untrusted refusal, got: %q", res.Error)
	}
}

// --- delete_file confirmation: path- and identity-bound ----------------------
//
// The permission flow exists and fails closed, but it confirms a CALL, not a
// TARGET. For a deletion that is not enough: the user is shown a bare tool
// name, the question is asked before the path is canonicalised or bounded, one
// session-scoped answer authorises every later deletion, and nothing detects
// the file changing while the prompt sits on screen.

func deletePermCtx(t *testing.T, sessionID, dir string) (*AgentContext, context.CancelFunc) {
	t.Helper()
	ctx, cancel := permCtx(sessionID)
	ctx.WorkingDir = dir
	return ctx, cancel
}

// captureRequest runs awaitPermission and hands back the emitted event.
func captureRequest(t *testing.T, ctx *AgentContext, callID, args string,
	answer func()) (PermissionRequest, bool, int) {
	t.Helper()
	var got PermissionRequest
	requests := 0
	var mu sync.Mutex
	ctx.StreamFn = func(et string, data interface{}) {
		if et != "permission_request" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		requests++
		if pr, ok := data.(PermissionRequest); ok {
			got = pr
		}
	}
	done := make(chan bool, 1)
	go func() {
		done <- awaitPermission(ctx, "delete_file", callID, json.RawMessage(args))
	}()
	waitForPending(t, ctx.PassID, callID)
	answer()
	allowed := <-done
	mu.Lock()
	defer mu.Unlock()
	return got, allowed, requests
}

// 1: the prompt must name the exact target, not the tool.
func TestDeletePermissionNamesTheTarget(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "obsolete.py"), []byte("A = 1\n"), 0o644)
	ctx, cancel := deletePermCtx(t, "sess-name", dir)
	defer cancel()
	req, _, _ := captureRequest(t, ctx, "call_n", `{"path":"obsolete.py"}`, func() {
		postDecision(t, `{"session_id":"sess-name","tool_call_id":"call_n","decision":"deny"}`)
	})
	t.Logf("message=%q", req.Message)
	if !strings.Contains(req.Message, "obsolete.py") {
		t.Errorf("the user is asked about %q, which never names the file", req.Message)
	}
	if !strings.Contains(strings.ToLower(req.Message), "delete") {
		t.Errorf("the message does not say what will happen: %q", req.Message)
	}
}

// 3/5/6/7: unsafe targets must never reach the user, and an approval must be
// bound to what was inspected.
func TestDeletePermissionPreflightAndBinding(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "keep.py"), []byte("A = 1\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "mod.py"), []byte("A = 1\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "emptydir"), 0o755)

	for _, c := range []struct {
		name, args string
		wantAsk    bool
	}{
		{"path escape", `{"path":"../../etc/passwd"}`, false},
		{"absolute escape", `{"path":"/etc/passwd"}`, false},
		{"blank path", `{"path":""}`, false},
		{"malformed args", `{"path":123}`, false},
		{"missing target", `{"path":"gone.py"}`, false},
		{"non-empty directory", `{"path":"pkg"}`, false},
		{"regular file", `{"path":"keep.py"}`, true},
		{"empty directory", `{"path":"emptydir"}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := deletePermCtx(t, "sess-pf-"+strings.ReplaceAll(c.name, " ", ""), dir)
			defer cancel()
			asked := 0
			ctx.StreamFn = func(et string, _ interface{}) {
				if et == "permission_request" {
					asked++
				}
			}
			if !c.wantAsk {
				// Nothing should block; the call must be refused without a
				// prompt. A short fail-safe keeps the parent's blocking
				// behaviour observable instead of hanging the suite.
				t.Setenv("ATLAS_PERMISSION_TIMEOUT_SEC", "1")
				allowed := awaitPermission(ctx, "delete_file", "call_pf", json.RawMessage(c.args))
				if allowed {
					t.Errorf("%s was allowed without a user decision", c.name)
				}
				if asked != 0 {
					t.Errorf("%s emitted %d permission request(s); the user must never be "+
						"asked about a target the proxy would refuse", c.name, asked)
				}
				return
			}
			done := make(chan bool, 1)
			go func() {
				done <- awaitPermission(ctx, "delete_file", "call_pf", json.RawMessage(c.args))
			}()
			waitForPending(t, ctx.PassID, "call_pf")
			postDecision(t, `{"session_id":"`+ctx.PassID+`","tool_call_id":"call_pf","decision":"deny"}`)
			<-done
			if asked != 1 {
				t.Errorf("%s emitted %d permission requests, want 1", c.name, asked)
			}
		})
	}
}

// 4/8/17/18: one answer must never authorise a second deletion.
func TestDeleteApprovalIsNeverSessionWide(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.py"), []byte("A = 1\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.py"), []byte("B = 2\n"), 0o644)
	ctx, cancel := deletePermCtx(t, "sess-scope-del", dir)
	defer cancel()

	_, allowed, _ := captureRequest(t, ctx, "call_a", `{"path":"a.py"}`, func() {
		postDecision(t, `{"session_id":"sess-scope-del","tool_call_id":"call_a","decision":"allow","scope":"session"}`)
	})
	if !allowed {
		t.Fatal("the allow decision did not come through")
	}
	if ctx.isToolAllowed("delete_file") {
		t.Error("a session-scoped answer put delete_file on the session allowlist; " +
			"one approval now authorises every later deletion")
	}
	if needsPermission(ctx, "delete_file", json.RawMessage(`{"path":"b.py"}`)) == false {
		t.Error("deleting b.py no longer needs permission after approving a.py")
	}
}

// 12/14/15/16: the target changing while the prompt is open must invalidate it.
func TestDeleteApprovalGoesStaleWhenTheTargetChanges(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(dir string) string // returns the path argument
		churn func(dir string)
	}{
		{"contents change", func(dir string) string {
			os.WriteFile(filepath.Join(dir, "f.py"), []byte("A = 1\n"), 0o644)
			return "f.py"
		}, func(dir string) {
			os.WriteFile(filepath.Join(dir, "f.py"), []byte("A = 999\n"), 0o644)
		}},
		{"type changes", func(dir string) string {
			os.WriteFile(filepath.Join(dir, "t.py"), []byte("A = 1\n"), 0o644)
			return "t.py"
		}, func(dir string) {
			os.Remove(filepath.Join(dir, "t.py"))
			os.MkdirAll(filepath.Join(dir, "t.py"), 0o755)
		}},
		{"empty directory gains a child", func(dir string) string {
			os.MkdirAll(filepath.Join(dir, "d"), 0o755)
			return "d"
		}, func(dir string) {
			os.WriteFile(filepath.Join(dir, "d", "new.py"), []byte("x\n"), 0o644)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			rel := c.setup(dir)
			ctx, cancel := deletePermCtx(t, "sess-stale-"+strings.ReplaceAll(c.name, " ", ""), dir)
			defer cancel()
			args := `{"path":"` + rel + `"}`
			_, allowed, _ := captureRequest(t, ctx, "call_s", args, func() {
				c.churn(dir) // the world moves while the prompt is open
				postDecision(t, `{"session_id":"`+ctx.PassID+`","tool_call_id":"call_s","decision":"allow"}`)
			})
			if !allowed {
				t.Skip("approval did not return; the stale check is downstream of it")
			}
			res := executeToolCall("delete_file", json.RawMessage(args), ctx)
			if res.Success {
				t.Errorf("%s: the deletion went ahead on an approval for different bytes", c.name)
			}
			if _, err := os.Lstat(filepath.Join(dir, rel)); os.IsNotExist(err) {
				t.Errorf("%s: the target is gone despite a stale approval", c.name)
			}
		})
	}
}

// The outcome matrix, driven through the real endpoint and the real tool.
func TestDeleteConfirmationOutcomeMatrix(t *testing.T) {
	type step struct {
		rel      string
		decision string // "allow", "deny", "" = never answered
		scope    string
		churn    func(dir, rel string)
		wantGone bool
		wantOK   bool
	}
	for _, c := range []struct {
		name  string
		setup func(dir string)
		steps []step
	}{
		{"1 regular file approved", func(dir string) {
			os.WriteFile(filepath.Join(dir, "f.py"), []byte("A\n"), 0o644)
		}, []step{{rel: "f.py", decision: "allow", wantGone: true, wantOK: true}}},

		{"2 denied", func(dir string) {
			os.WriteFile(filepath.Join(dir, "f.py"), []byte("A\n"), 0o644)
		}, []step{{rel: "f.py", decision: "deny"}}},

		{"10 empty directory approved", func(dir string) {
			os.MkdirAll(filepath.Join(dir, "d"), 0o755)
		}, []step{{rel: "d", decision: "allow", wantGone: true, wantOK: true}}},

		{"11 symlink approved removes the link only", func(dir string) {
			os.WriteFile(filepath.Join(dir, "real.py"), []byte("A\n"), 0o644)
			os.Symlink("real.py", filepath.Join(dir, "link.py"))
		}, []step{{rel: "link.py", decision: "allow", wantGone: true, wantOK: true}}},

		{"15 symlink retarget goes stale", func(dir string) {
			os.WriteFile(filepath.Join(dir, "real.py"), []byte("A\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "other.py"), []byte("B\n"), 0o644)
			os.Symlink("real.py", filepath.Join(dir, "link.py"))
		}, []step{{rel: "link.py", decision: "allow", churn: func(dir, rel string) {
			os.Remove(filepath.Join(dir, rel))
			os.Symlink("other.py", filepath.Join(dir, rel))
		}}}},

		{"20 two deletions need two confirmations", func(dir string) {
			os.WriteFile(filepath.Join(dir, "a.py"), []byte("A\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "b.py"), []byte("B\n"), 0o644)
		}, []step{
			{rel: "a.py", decision: "allow", scope: "session", wantGone: true, wantOK: true},
			{rel: "b.py", decision: "allow", wantGone: true, wantOK: true},
		}},

		{"18 approval for A does not delete B", func(dir string) {
			os.WriteFile(filepath.Join(dir, "a.py"), []byte("A\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "b.py"), []byte("B\n"), 0o644)
		}, []step{{rel: "a.py", decision: "allow", wantGone: true, wantOK: true}}},

		{"19 alias spelling is the same target", func(dir string) {
			os.WriteFile(filepath.Join(dir, "solve.py"), []byte("A\n"), 0o644)
		}, []step{{rel: "./solve.py", decision: "allow", wantGone: true, wantOK: true}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			c.setup(dir)
			sess := "sess-m-" + strings.Map(func(r rune) rune {
				if r == ' ' {
					return '-'
				}
				return r
			}, c.name)
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			ctx.StreamFn = func(string, interface{}) {}

			for i, s := range c.steps {
				callID := fmt.Sprintf("call_%d", i)
				args := `{"path":"` + s.rel + `"}`
				if needsPermission(ctx, "delete_file", json.RawMessage(args)) == false {
					t.Fatalf("step %d: delete_file did not require permission", i)
				}
				done := make(chan bool, 1)
				go func() {
					done <- awaitPermission(ctx, "delete_file", callID, json.RawMessage(args))
				}()
				waitForPending(t, sess, callID)
				if s.churn != nil {
					s.churn(dir, s.rel)
				}
				scope := s.scope
				if scope == "" {
					scope = "once"
				}
				postDecision(t, fmt.Sprintf(
					`{"session_id":%q,"tool_call_id":%q,"decision":%q,"scope":%q}`,
					sess, callID, s.decision, scope))
				allowed := <-done
				if s.decision == "deny" && allowed {
					t.Fatalf("step %d: a deny returned allowed", i)
				}
				if !allowed {
					continue
				}
				res := executeToolCall("delete_file", json.RawMessage(args), ctx)
				if res.Success != s.wantOK {
					t.Errorf("step %d: success=%v want %v (err=%.90s)", i, res.Success, s.wantOK, res.Error)
				}
				_, statErr := os.Lstat(filepath.Join(dir, filepath.Clean(s.rel)))
				gone := os.IsNotExist(statErr)
				if gone != s.wantGone {
					t.Errorf("step %d: gone=%v want %v", i, gone, s.wantGone)
				}
			}
			// The link's target always survives.
			if strings.Contains(c.name, "symlink") {
				if _, err := os.Stat(filepath.Join(dir, "real.py")); err != nil {
					t.Errorf("the symlink's target was removed: %v", err)
				}
			}
			// No grant is ever left behind.
			if ctx.approvedDelete != nil {
				t.Errorf("a pending approval leaked: %+v", ctx.approvedDelete)
			}
			if ctx.isToolAllowed("delete_file") {
				t.Error("delete_file reached the session allowlist")
			}
		})
	}
}

// 4/22: no pending state survives a cancel, and an unknown id deletes nothing.
func TestDeleteConfirmationLeavesNoPendingState(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.py"), []byte("A\n"), 0o644)
	ctx, cancel := deletePermCtx(t, "sess-clean", dir)
	ctx.StreamFn = func(string, interface{}) {}
	done := make(chan bool, 1)
	go func() {
		done <- awaitPermission(ctx, "delete_file", "call_c", json.RawMessage(`{"path":"f.py"}`))
	}()
	waitForPending(t, "sess-clean", "call_c")
	cancel()
	if <-done {
		t.Error("a cancelled request was allowed")
	}
	if _, ok := pendingPermissions.Load(permKey("sess-clean", "call_c")); ok {
		t.Error("the pending permission entry leaked after cancellation")
	}
	if ctx.approvedDelete != nil {
		t.Error("a cancelled request granted an approval")
	}
	if w := postDecision(t, `{"session_id":"sess-clean","tool_call_id":"call_c","decision":"allow"}`); w.Code != 404 {
		t.Errorf("a decision for a gone request returned %d, want 404", w.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "f.py")); err != nil {
		t.Errorf("the file was removed: %v", err)
	}
}

// 24: a client with no way to answer fails closed, and is never asked.
func TestDeleteConfirmationFailsClosedWithoutAClient(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.py"), []byte("A\n"), 0o644)
	ctx, cancel := permCtx("") // no session id: nothing can answer
	defer cancel()
	ctx.WorkingDir = dir
	asked := 0
	ctx.StreamFn = func(et string, _ interface{}) {
		if et == "permission_request" {
			asked++
		}
	}
	if awaitPermission(ctx, "delete_file", "call_x", json.RawMessage(`{"path":"f.py"}`)) {
		t.Error("a non-interactive client was allowed to delete")
	}
	if asked != 0 {
		t.Errorf("%d prompts emitted with nobody to answer them", asked)
	}
	if _, err := os.Stat(filepath.Join(dir, "f.py")); err != nil {
		t.Errorf("the file was removed: %v", err)
	}
}

// No broad mode answers for a deletion. Every mode asks, including the ones
// that exist precisely to stop asking -- and unrelated tools keep theirs.
func TestDeletePermissionModes(t *testing.T) {
	del := json.RawMessage(`{"path":"f.py"}`)
	for _, c := range []struct {
		name string
		set  func(*AgentContext)
	}{
		{"default", func(c *AgentContext) { c.PermissionMode = PermissionDefault }},
		{"accept-edits", func(c *AgentContext) { c.PermissionMode = PermissionAcceptEdits }},
		{"yolo mode", func(c *AgentContext) { c.PermissionMode = PermissionYolo }},
		{"yolo flag", func(c *AgentContext) { c.YoloMode = true }},
		{"preauthorized", func(c *AgentContext) {
			c.AllowedTools = map[string]bool{"delete_file": true}
		}},
		{"yolo plus preauthorized", func(c *AgentContext) {
			c.PermissionMode = PermissionYolo
			c.AllowedTools = map[string]bool{"delete_file": true}
		}},
	} {
		ctx := &AgentContext{}
		c.set(ctx)
		if !needsPermission(ctx, "delete_file", del) {
			t.Errorf("%s: a deletion did not require permission", c.name)
		}
	}
	// The same modes still answer for everything else.
	for _, c := range []struct {
		name, tool string
		set        func(*AgentContext)
		args       string
	}{
		{"yolo/run_command", "run_command",
			func(c *AgentContext) { c.PermissionMode = PermissionYolo }, `{"command":"ls"}`},
		{"preauthorized/run_command", "run_command",
			func(c *AgentContext) { c.AllowedTools = map[string]bool{"run_command": true} },
			`{"command":"ls"}`},
		{"accept-edits/write_file", "write_file",
			func(c *AgentContext) { c.PermissionMode = PermissionAcceptEdits },
			`{"path":"a.py","content":"x"}`},
		{"accept-edits/move_file", "move_file",
			func(c *AgentContext) { c.PermissionMode = PermissionAcceptEdits },
			`{"source":"a.py","destination":"b.py"}`},
	} {
		ctx := &AgentContext{}
		c.set(ctx)
		if needsPermission(ctx, c.tool, json.RawMessage(c.args)) {
			t.Errorf("%s: an unrelated tool lost its existing permission semantics", c.name)
		}
	}
}

// A symlink whose target is written as an absolute path is refused by the
// workspace resolver before this confirmation is reached -- existing
// containment policy, reused rather than restated. Pinned so the reuse is
// visible and a future resolver change is noticed here.
func TestAbsoluteSymlinkIsRefusedByContainment(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "real.py"), []byte("A\n"), 0o644)
	if err := os.Symlink(filepath.Join(dir, "real.py"), filepath.Join(dir, "abs.py")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ctx := &AgentContext{WorkingDir: dir}
	_, refusal := inspectDeleteTarget(ctx, json.RawMessage(`{"path":"abs.py"}`))
	t.Logf("refusal=%q", refusal)
	if refusal == "" {
		t.Error("an absolute symlink was accepted for confirmation")
	}
	// A relative link inside the workspace is the supported shape.
	os.Symlink("real.py", filepath.Join(dir, "rel.py"))
	target, refusal2 := inspectDeleteTarget(ctx, json.RawMessage(`{"path":"rel.py"}`))
	if refusal2 != "" {
		t.Fatalf("a relative in-workspace symlink was refused: %s", refusal2)
	}
	if target.Kind != deleteTargetSymlink || target.LinkText != "real.py" {
		t.Errorf("kind=%q link=%q, want a symlink bound to its link text",
			target.Kind, target.LinkText)
	}
	msg := describeDeleteTarget(target)
	if !strings.Contains(msg, "symlink") || !strings.Contains(msg, "not what it points at") {
		t.Errorf("the prompt does not say the link itself is removed: %q", msg)
	}
}

// Structural: one handshake, no verb list, and deletion cannot be allowlisted.
func TestDeleteConfirmationStructure(t *testing.T) {
	read := func(f string) string { b, _ := os.ReadFile(f); return string(b) }

	// The rejected extractor and every verb table stay gone.
	entries, _ := os.ReadDir(".")
	self := "permissions_test.go" // this file necessarily spells the banned names
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || e.Name() == self {
			continue
		}
		body := read(e.Name())
		for _, banned := range []string{"explicitDeleteIntent", "deleteVerbs",
			"deleteClauseUnsupported", "deleteHedgeWords", "maxDeleteIntentPaths"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s references %s; the verb-inference approach was removed",
					e.Name(), banned)
			}
		}
	}

	// The confirmation path consults no natural-language helper.
	perms := read("permissions.go")
	i := strings.Index(perms, "func inspectDeleteTarget")
	j := strings.Index(perms, "func takeDeleteApproval")
	if i < 0 || j < 0 || j < i {
		t.Fatal("the delete confirmation helpers moved; this guard is stale")
	}
	region := perms[i:j]
	for _, nl := range []string{"actionIntentWords", "fixIntentWords", "isActionIntentMessage",
		"isReadOnlyRequest", "isExplainOnlyMessage", "expectedOutputPaths", "negatedAt",
		"claimWords", "UserMessage", "Messages"} {
		if strings.Contains(region, nl) {
			t.Errorf("the delete confirmation path consults %s; authorisation must be "+
				"structural, never linguistic", nl)
		}
	}

	// Exactly one handshake: one emitter, one endpoint, one pending store.
	if n := strings.Count(perms, `ctx.Stream("permission_request"`); n != 1 {
		t.Errorf("%d permission_request emitters, want exactly one", n)
	}
	if n := strings.Count(read("main.go"), `"/v1/permission"`); n != 1 {
		t.Errorf("%d permission endpoints registered, want exactly one", n)
	}

	// delete_file can never be added to the turn allowlist by an approval.
	if strings.Contains(perms, `allowToolForTurn(toolName)`) {
		before := perms[:strings.Index(perms, `allowToolForTurn(toolName)`)]
		if !strings.Contains(before, `toolName == "delete_file"`) {
			t.Error("the session-scope allowlist is reachable without a delete_file guard")
		}
	}

	// Revalidation lives next to the removal, not somewhere hopeful. The
	// revalidation is the identity comparison; the take happens earlier, on
	// purpose, so that every attempt spends the approval (below).
	tools := read("tools.go")
	reval := strings.Index(tools, "approved.identityMatches(now)")
	rm := strings.Index(tools, "if rmErr := os.Remove(path)")
	if reval < 0 || rm < 0 || reval > rm || rm-reval > 1600 {
		t.Errorf("the identity revalidation is not adjacent to the removal (revalidation=%d rm=%d)",
			reval, rm)
	}
	// The approval is spent by the attempt before any preflight can refuse,
	// so a refusal cannot leave a grant -- and the object reference it holds
	// -- waiting for a later call.
	tool := strings.Index(tools, "func deleteFileTool()")
	if tool < 0 {
		t.Fatal("deleteFileTool moved; this guard is stale")
	}
	body := tools[tool:]
	take := strings.Index(body, "takeDeleteApproval(ctx, path)")
	preflight := strings.Index(body, "info, err := os.Stat(path)")
	if take < 0 || preflight < 0 || take > preflight {
		t.Errorf("the approval is taken after the preflight (take=%d preflight=%d)", take, preflight)
	}
}

// Completion behaviour is deliberately untouched by this slice: an APPROVED,
// successful deletion still cannot finish a run.
func TestApprovedDeletionStillCannotComplete(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.py"), []byte("A\n"), 0o644)
	ctx, cancel := deletePermCtx(t, "sess-complete", dir)
	defer cancel()
	ctx.StreamFn = func(string, interface{}) {}
	args := json.RawMessage(`{"path":"a.py"}`)

	done := make(chan bool, 1)
	go func() { done <- awaitPermission(ctx, "delete_file", "call_0", args) }()
	waitForPending(t, "sess-complete", "call_0")
	postDecision(t, `{"session_id":"sess-complete","tool_call_id":"call_0","decision":"allow"}`)
	if !<-done {
		t.Fatal("the approval did not come through")
	}
	res := executeToolCall("delete_file", args, ctx)
	if !res.Success {
		t.Fatalf("the approved deletion failed: %s", res.Error)
	}
	recordLedgerEffect("delete_file", args, ctx, res)

	// The tombstone is real, and it still blocks: no obligation exists yet.
	d := ctx.Ledger[ledgerKey(ctx, "a.py")]
	if d == nil || !d.Tombstoned || !d.RestoreProhibited {
		t.Fatalf("the deletion did not record a prohibited tombstone: %+v", d)
	}
	if !blockingTombstone(ctx) {
		t.Error("an approved deletion stopped blocking completion; the obligation " +
			"slice has not landed and approval alone must not authorise a terminal")
	}
	ok, why := terminalCompletionAllowed(ctx, nil)
	if ok || why != "delete_intent_unestablished" {
		t.Errorf("completion says ok=%v why=%q, want the unchanged refusal", ok, why)
	}
}

// --- No broad mode may authorise a deletion ----------------------------------
//
// yolo and session_allowed_tools are blanket answers to "may this tool run".
// For a deletion that is the wrong question: the decision is about a specific
// object, so a blanket yes cannot stand in for it. A yolo session with nobody
// to ask therefore cannot delete, which is the intended cost.

// deleteReachesDisk runs the real gate then the real tool, counting prompts.
func deleteReachesDisk(t *testing.T, set func(*AgentContext), rel string) (int, bool, bool) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, rel)
	os.WriteFile(path, []byte("A\n"), 0o644)
	ctx, cancel := permCtx("")
	defer cancel()
	ctx.WorkingDir = dir
	set(ctx)
	if ctx.YoloMode {
		// Production installs this in yolo mode; the fixture must carry it or
		// it is not testing the yolo path that actually ships.
		ctx.PermissionFn = func(string, json.RawMessage) bool { return true }
	}
	prompts := 0
	ctx.StreamFn = func(et string, _ interface{}) {
		if et == "permission_request" {
			prompts++
		}
	}
	args := json.RawMessage(`{"path":"` + rel + `"}`)
	allowed := true
	if needsPermission(ctx, "delete_file", args) {
		t.Setenv("ATLAS_PERMISSION_TIMEOUT_SEC", "1")
		// Mirrors the loop: PermissionFn is skipped for a deletion, so even
		// a yolo session reaches the handshake.
		allowed = awaitPermission(ctx, "delete_file", "call_0", args)
	}
	if allowed {
		executeToolCall("delete_file", args, ctx)
	}
	_, err := os.Lstat(path)
	return prompts, allowed, os.IsNotExist(err)
}

var deleteModes = []struct {
	name string
	set  func(*AgentContext)
}{
	{"yolo mode", func(c *AgentContext) { c.PermissionMode = PermissionYolo }},
	{"yolo flag", func(c *AgentContext) { c.YoloMode = true }},
	{"preauthorized delete_file", func(c *AgentContext) {
		c.AllowedTools = map[string]bool{"delete_file": true}
	}},
	{"accept-edits", func(c *AgentContext) { c.PermissionMode = PermissionAcceptEdits }},
	{"default", func(c *AgentContext) { c.PermissionMode = PermissionDefault }},
}

// With nobody to answer, every mode fails closed and nobody is asked.
func TestNoBroadModeAuthorisesDeletion(t *testing.T) {
	for _, c := range deleteModes {
		t.Run(c.name, func(t *testing.T) {
			prompts, allowed, gone := deleteReachesDisk(t, c.set, "a.py")
			t.Logf("%s (no client): prompts=%d allowed=%v deleted=%v",
				c.name, prompts, allowed, gone)
			if gone {
				t.Errorf("%s deleted the file without an exact confirmation", c.name)
			}
			if allowed {
				t.Errorf("%s allowed a deletion with nobody to approve it", c.name)
			}
			if prompts != 0 {
				t.Errorf("%s emitted %d prompts with no client to answer them", c.name, prompts)
			}
		})
	}
}

// With a client, every mode asks exactly once — including the broad ones.
func TestEveryModeAsksExactlyOnceForADeletion(t *testing.T) {
	for _, c := range deleteModes {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "a.py"), []byte("A\n"), 0o644)
			sess := "sess-mode-" + strings.ReplaceAll(c.name, " ", "-")
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			c.set(ctx)
			prompts := 0
			ctx.StreamFn = func(et string, _ interface{}) {
				if et == "permission_request" {
					prompts++
				}
			}
			args := json.RawMessage(`{"path":"a.py"}`)
			if !needsPermission(ctx, "delete_file", args) {
				t.Fatalf("%s: deletion did not require permission", c.name)
			}
			done := make(chan bool, 1)
			go func() { done <- awaitPermission(ctx, "delete_file", "call_0", args) }()
			waitForPending(t, sess, "call_0")
			postDecision(t, `{"session_id":"`+sess+`","tool_call_id":"call_0","decision":"deny"}`)
			if <-done {
				t.Errorf("%s: a deny was reported as allowed", c.name)
			}
			t.Logf("%s (with client): prompts=%d", c.name, prompts)
			if prompts != 1 {
				t.Errorf("%s emitted %d permission requests, want exactly 1", c.name, prompts)
			}
			if _, err := os.Stat(filepath.Join(dir, "a.py")); err != nil {
				t.Errorf("%s: the file was removed after a deny", c.name)
			}
		})
	}
}

// Exact prompt counts per outcome: the answered paths each begin with one
// request; only the refused-before-asking shapes produce none.
func TestDeletePromptCounts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ok.py"), []byte("A\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "m.py"), []byte("A\n"), 0o644)

	answered := []struct{ name, decision string }{
		{"approve", "allow"}, {"deny", "deny"},
	}
	for _, c := range answered {
		t.Run(c.name, func(t *testing.T) {
			d := t.TempDir()
			os.WriteFile(filepath.Join(d, "f.py"), []byte("A\n"), 0o644)
			sess := "sess-count-" + c.name
			ctx, cancel := deletePermCtx(t, sess, d)
			defer cancel()
			prompts := 0
			ctx.StreamFn = func(et string, _ interface{}) {
				if et == "permission_request" {
					prompts++
				}
			}
			done := make(chan bool, 1)
			go func() {
				done <- awaitPermission(ctx, "delete_file", "c", json.RawMessage(`{"path":"f.py"}`))
			}()
			waitForPending(t, sess, "c")
			postDecision(t, `{"session_id":"`+sess+`","tool_call_id":"c","decision":"`+c.decision+`"}`)
			<-done
			if prompts != 1 {
				t.Errorf("%s: %d prompts, want 1", c.name, prompts)
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "f.py"), []byte("A\n"), 0o644)
		ctx, cancel := deletePermCtx(t, "sess-count-timeout", d)
		defer cancel()
		prompts := 0
		ctx.StreamFn = func(et string, _ interface{}) {
			if et == "permission_request" {
				prompts++
			}
		}
		t.Setenv("ATLAS_PERMISSION_TIMEOUT_SEC", "1")
		if awaitPermission(ctx, "delete_file", "c", json.RawMessage(`{"path":"f.py"}`)) {
			t.Error("a timeout was allowed")
		}
		if prompts != 1 {
			t.Errorf("timeout: %d prompts, want 1", prompts)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "f.py"), []byte("A\n"), 0o644)
		ctx, cancel := deletePermCtx(t, "sess-count-cancel", d)
		prompts := 0
		ctx.StreamFn = func(et string, _ interface{}) {
			if et == "permission_request" {
				prompts++
			}
		}
		done := make(chan bool, 1)
		go func() {
			done <- awaitPermission(ctx, "delete_file", "c", json.RawMessage(`{"path":"f.py"}`))
		}()
		waitForPending(t, "sess-count-cancel", "c")
		cancel()
		<-done
		if prompts != 1 {
			t.Errorf("cancel: %d prompts, want 1", prompts)
		}
	})
	// Refused before asking: zero prompts.
	for _, c := range []struct{ name, args string }{
		{"escape", `{"path":"../../etc/passwd"}`},
		{"blank", `{"path":""}`},
		{"malformed", `{"path":123}`},
		{"missing", `{"path":"gone.py"}`},
		{"non-empty directory", `{"path":"pkg"}`},
	} {
		t.Run("zero/"+c.name, func(t *testing.T) {
			ctx, cancel := deletePermCtx(t, "sess-zero-"+strings.ReplaceAll(c.name, " ", "-"), dir)
			defer cancel()
			prompts := 0
			ctx.StreamFn = func(et string, _ interface{}) {
				if et == "permission_request" {
					prompts++
				}
			}
			if awaitPermission(ctx, "delete_file", "c", json.RawMessage(c.args)) {
				t.Errorf("%s was allowed", c.name)
			}
			if prompts != 0 {
				t.Errorf("%s: %d prompts, want 0", c.name, prompts)
			}
		})
	}
}

// Preauthorising delete_file must not carry from one target to another.
func TestPreauthorizedDeleteDoesNotCarryToAnotherPath(t *testing.T) {
	ctx := &AgentContext{AllowedTools: map[string]bool{"delete_file": true}}
	for _, rel := range []string{"a.py", "b.py"} {
		if !needsPermission(ctx, "delete_file", json.RawMessage(`{"path":"`+rel+`"}`)) {
			t.Errorf("%s was pre-authorised; deletion needs its own decision", rel)
		}
	}
	// Unrelated tools keep their preauthorization semantics.
	ctx2 := &AgentContext{AllowedTools: map[string]bool{"run_command": true}}
	if needsPermission(ctx2, "run_command", json.RawMessage(`{"command":"ls"}`)) {
		t.Error("preauthorization stopped working for run_command")
	}
	ctx3 := &AgentContext{PermissionMode: PermissionYolo}
	if needsPermission(ctx3, "run_command", json.RawMessage(`{"command":"ls"}`)) {
		t.Error("yolo stopped working for run_command")
	}
}

// --- Approval binds to the object, not only to its bytes ---------------------
//
// A different file with the same contents is a different thing to delete.

func TestApprovalBindsToTheFilesystemObject(t *testing.T) {
	for _, c := range []struct {
		name     string
		setup    func(dir string) string
		churn    func(dir, rel string)
		wantGone bool
	}{
		{"replaced with identical bytes", func(dir string) string {
			os.WriteFile(filepath.Join(dir, "f.py"), []byte("A\n"), 0o644)
			return "f.py"
		}, func(dir, rel string) {
			os.Remove(filepath.Join(dir, rel))
			os.WriteFile(filepath.Join(dir, rel), []byte("A\n"), 0o644)
		}, false},
		{"symlink replaced with the same target text", func(dir string) string {
			os.WriteFile(filepath.Join(dir, "real.py"), []byte("A\n"), 0o644)
			os.Symlink("real.py", filepath.Join(dir, "l.py"))
			return "l.py"
		}, func(dir, rel string) {
			os.Remove(filepath.Join(dir, rel))
			os.Symlink("real.py", filepath.Join(dir, rel))
		}, false},
		{"empty directory replaced by another", func(dir string) string {
			os.MkdirAll(filepath.Join(dir, "d"), 0o755)
			return "d"
		}, func(dir, rel string) {
			os.Remove(filepath.Join(dir, rel))
			os.MkdirAll(filepath.Join(dir, rel), 0o755)
		}, false},
		{"untouched", func(dir string) string {
			os.WriteFile(filepath.Join(dir, "f.py"), []byte("A\n"), 0o644)
			return "f.py"
		}, func(string, string) {}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			rel := c.setup(dir)
			sess := "sess-obj-" + strings.ReplaceAll(c.name, " ", "-")
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			ctx.StreamFn = func(string, interface{}) {}
			args := json.RawMessage(`{"path":"` + rel + `"}`)
			done := make(chan bool, 1)
			go func() { done <- awaitPermission(ctx, "delete_file", "call_o", args) }()
			waitForPending(t, sess, "call_o")
			c.churn(dir, rel)
			postDecision(t, `{"session_id":"`+sess+`","tool_call_id":"call_o","decision":"allow"}`)
			if !<-done {
				t.Fatal("the approval did not come through")
			}
			res := executeToolCall("delete_file", args, ctx)
			_, err := os.Lstat(filepath.Join(dir, rel))
			gone := os.IsNotExist(err)
			t.Logf("%s: success=%v gone=%v err=%.80s", c.name, res.Success, gone, res.Error)
			if gone != c.wantGone {
				t.Errorf("%s: deleted=%v want %v", c.name, gone, c.wantGone)
			}
		})
	}
}

// --- Fulfilled deletion authority --------------------------------------------
//
// Four facts stay apart: the user approved this exact object, the model
// attempted the removal, the path is absent, and the task is finished. A
// record exists only when the first three are all true on the same route.

// approveAndDelete runs the real handshake, the real tool, and the real ledger
// effect, returning whether a fulfilled record was produced.
func approveAndDelete(t *testing.T, ctx *AgentContext, sess, callID, rel string,
	decision string, churn func()) (*fulfilledDeletion, *ToolResult) {
	t.Helper()
	args := json.RawMessage(`{"path":"` + rel + `"}`)
	allowed := true
	if needsPermission(ctx, "delete_file", args) {
		done := make(chan bool, 1)
		go func() { done <- awaitPermission(ctx, "delete_file", callID, args) }()
		waitForPending(t, sess, callID)
		if churn != nil {
			churn()
		}
		postDecision(t, fmt.Sprintf(
			`{"session_id":%q,"tool_call_id":%q,"decision":%q}`, sess, callID, decision))
		allowed = <-done
	}
	if !allowed {
		return nil, nil
	}
	res := executeToolCall("delete_file", args, ctx)
	recordLedgerEffect("delete_file", args, ctx, res)
	f, _ := fulfilledDeletionFor(ctx, filepath.Join(ctx.WorkingDir, filepath.Clean(rel)))
	return f, res
}

func TestFulfilledDeletionRecord(t *testing.T) {
	for _, c := range []struct {
		name     string
		setup    func(dir string) string
		decision string
		churn    func(dir, rel string)
		want     bool
	}{
		{"approved and removed", func(dir string) string {
			os.WriteFile(filepath.Join(dir, "a.py"), []byte("A\n"), 0o644)
			return "a.py"
		}, "allow", nil, true},
		{"denied", func(dir string) string {
			os.WriteFile(filepath.Join(dir, "a.py"), []byte("A\n"), 0o644)
			return "a.py"
		}, "deny", nil, false},
		{"stale identity", func(dir string) string {
			os.WriteFile(filepath.Join(dir, "a.py"), []byte("A\n"), 0o644)
			return "a.py"
		}, "allow", func(dir, rel string) {
			os.WriteFile(filepath.Join(dir, rel), []byte("CHANGED\n"), 0o644)
		}, false},
		{"empty directory approved", func(dir string) string {
			os.MkdirAll(filepath.Join(dir, "d"), 0o755)
			return "d"
		}, "allow", nil, true},
		{"symlink approved", func(dir string) string {
			os.WriteFile(filepath.Join(dir, "real.py"), []byte("A\n"), 0o644)
			os.Symlink("real.py", filepath.Join(dir, "l.py"))
			return "l.py"
		}, "allow", nil, true},
		{"alias spelling", func(dir string) string {
			os.WriteFile(filepath.Join(dir, "a.py"), []byte("A\n"), 0o644)
			return "./a.py"
		}, "allow", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			rel := c.setup(dir)
			sess := "sess-ful-" + strings.ReplaceAll(c.name, " ", "-")
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			ctx.StreamFn = func(string, interface{}) {}
			var churn func()
			if c.churn != nil {
				churn = func() { c.churn(dir, rel) }
			}
			f, _ := approveAndDelete(t, ctx, sess, "call_0", rel, c.decision, churn)
			t.Logf("%s: fulfilled=%v", c.name, f != nil)
			if (f != nil) != c.want {
				t.Errorf("fulfilled=%v want %v", f != nil, c.want)
			}
			if f != nil {
				if f.Canonical != filepath.Join(dir, filepath.Clean(rel)) {
					t.Errorf("record names %q", f.Canonical)
				}
				if f.Generation == 0 {
					t.Error("the record is not bound to a ledger generation")
				}
			}
			// The symlink's target is never fulfilled by the link's removal.
			if strings.Contains(c.name, "symlink") {
				if _, ok := fulfilledDeletionFor(ctx, filepath.Join(dir, "real.py")); ok {
					t.Error("removing the link fulfilled its target")
				}
				if _, err := os.Stat(filepath.Join(dir, "real.py")); err != nil {
					t.Errorf("the target was removed: %v", err)
				}
			}
		})
	}
}

// An unapproved removal, and an absence caused by something else, are not
// authority however the ledger ends up looking.
func TestUnapprovedAbsenceIsNotFulfilled(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.py"), []byte("A\n"), 0o644)
	ctx := &AgentContext{WorkingDir: dir, Ledger: map[string]*DeliverableState{}}
	// Something else removes it; the ledger is told about a delete call.
	os.Remove(filepath.Join(dir, "a.py"))
	args := json.RawMessage(`{"path":"a.py"}`)
	recordLedgerEffect("delete_file", args, ctx,
		&ToolResult{Success: true, MutationStatus: MutationApplied})
	if _, ok := fulfilledDeletionFor(ctx, filepath.Join(dir, "a.py")); ok {
		t.Error("an absence with no user approval became fulfilled authority")
	}
	// A move-source tombstone is never a fulfilled deletion either.
	os.WriteFile(filepath.Join(dir, "old.py"), []byte("A\n"), 0o644)
	os.Rename(filepath.Join(dir, "old.py"), filepath.Join(dir, "new.py"))
	recordLedgerEffect("move_file",
		json.RawMessage(`{"source":"old.py","destination":"new.py"}`), ctx,
		&ToolResult{Success: true, MutationStatus: MutationApplied})
	if _, ok := fulfilledDeletionFor(ctx, filepath.Join(dir, "old.py")); ok {
		t.Error("a move source became a fulfilled deletion")
	}
}

// Capacity is reserved before the mutation, so an untrackable deletion never
// happens.
func TestDeletionTrackingCapacity(t *testing.T) {
	dir := t.TempDir()
	ctx := &AgentContext{WorkingDir: dir, fulfilledDeletions: map[string]*fulfilledDeletion{}}
	for i := 0; i < maxTrackedDeletions; i++ {
		ctx.fulfilledDeletions[fmt.Sprintf("/x/%d", i)] = &fulfilledDeletion{}
	}
	if reserveDeletionSlot(ctx, filepath.Join(dir, "one-more.py")) {
		t.Error("a slot was handed out past the ceiling")
	}
	// And the tool refuses rather than removing something it cannot account for.
	os.WriteFile(filepath.Join(dir, "one-more.py"), []byte("A\n"), 0o644)
	sess := "sess-cap"
	pctx, cancel := deletePermCtx(t, sess, dir)
	defer cancel()
	pctx.StreamFn = func(string, interface{}) {}
	pctx.fulfilledDeletions = ctx.fulfilledDeletions
	_, res := approveAndDelete(t, pctx, sess, "call_0", "one-more.py", "allow", nil)
	if res != nil && res.Success {
		t.Error("a deletion succeeded with no room to account for it")
	}
	if _, err := os.Stat(filepath.Join(dir, "one-more.py")); err != nil {
		t.Errorf("the file was removed anyway: %v", err)
	}
}

// The fulfilled record has exactly one consumer: the tombstone decision.
func TestFulfilledDeletionHasOneConsumer(t *testing.T) {
	entries, _ := os.ReadDir(".")
	var callers []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(n)
		body := strings.Replace(string(b), "func fulfilledDeletionFor(", "", 1)
		if c := strings.Count(body, "fulfilledDeletionFor("); c > 0 {
			callers = append(callers, fmt.Sprintf("%s x%d", n, c))
		}
	}
	t.Logf("consumers: %v", callers)
	if len(callers) != 1 || !strings.HasPrefix(callers[0], "agent.go") {
		t.Errorf("fulfilled deletions are consumed from %v, want only the tombstone "+
			"decision in agent.go", callers)
	}
}

func TestDeletionCompletionHasNoLanguageDependency(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// Each function that decides or renders deletion completion, and the
	// language helpers none of them may consult.
	for _, fn := range []string{"blockingTombstone", "fulfilledApprovedDeletion",
		"undoneApprovedDeletion", "approvedDeletionPaths", "terminalCompletionAllowed"} {
		i := strings.Index(body, "func "+fn)
		if i < 0 {
			t.Errorf("%s is gone; this guard is pinning something that moved", fn)
			continue
		}
		end := strings.Index(body[i+1:], "\nfunc ")
		if end < 0 {
			end = len(body) - i - 1
		}
		region := body[i : i+1+end]
		for _, nl := range []string{"actionIntentWords", "fixIntentWords",
			"reOutputWriteVerb", "expectedOutputPaths", "isReadOnlyRequest",
			"isExplainOnlyMessage", "isActionIntentMessage", "negatedAt",
			"claimWords", "explicitDeleteIntent", "userMessage", "parsed.Summary"} {
			if strings.Contains(region, nl) {
				t.Errorf("%s consults %s; deletion authority must be structural", fn, nl)
			}
		}
	}

	// Provenance: the grant is written in one place, and only the endpoint can
	// reach it.
	perms, _ := os.ReadFile("permissions.go")
	if n := strings.Count(string(perms), "ctx.approvedDelete = &approvedDeletion{"); n != 1 {
		t.Errorf("%d writers of the approval grant, want exactly one", n)
	}
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(n)
		s := string(b)
		if n != "permissions.go" {
			for _, sym := range []string{"grantDeleteApproval(", "noteDeletionAttempt(",
				"reserveDeletionSlot("} {
				if strings.Contains(s, sym) && n != "tools.go" {
					t.Errorf("%s writes deletion authority via %s", n, sym)
				}
			}
		}
		// Promotion happens in exactly one place.
		if c := strings.Count(s, "promoteFulfilledDeletion("); c > 0 && n != "gates.go" && n != "permissions.go" {
			t.Errorf("%s promotes fulfilled deletions; only the ledger may", n)
		}
	}
	// The record is internal: no wire type carries it.
	types, _ := os.ReadFile("types.go")
	for _, leak := range []string{"`json:\"fulfilled", "`json:\"approved_delete",
		"`json:\"deletion_attempt"} {
		if strings.Contains(string(types), leak) {
			t.Errorf("the internal deletion record reached the wire: %s", leak)
		}
	}
}
