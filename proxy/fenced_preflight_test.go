package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A fenced call costs a full unconstrained generation per attempt, so the
// channel opens only for a call that can execute. The preflight runs the
// checks the tool runs anyway, by calling them, so it decides nothing new.

// unusableWriteCalls are write_file arguments that cannot execute: no path, a
// path that is not one, a path outside the workspace, or a deny-listed one.
var unusableWriteCalls = []struct{ name, args string }{
	{"no arguments", ``},
	{"null arguments", `null`},
	{"malformed shape", `{"path":123,"content":"@fenced"}`},
	{"missing path", `{"content":"@fenced"}`},
	{"blank path", `{"path":"","content":"@fenced"}`},
	{"whitespace path", `{"path":"   ","content":"@fenced"}`},
	{"tab path", `{"path":"\t\n","content":"@fenced"}`},
	{"workspace escape", `{"path":"../outside.py","content":"@fenced"}`},
	{"absolute escape", `{"path":"/etc/passwd","content":"@fenced"}`},
	{"deny-listed", `{"path":".env","content":"@fenced"}`},
	{"deny-listed key", `{"path":"id_rsa.pem","content":"@fenced"}`},
}

// The property the whole design rests on: every call the preflight refuses is
// also refused by the tool, which is what makes "decline to resolve, then let
// the tool answer" safe rather than a way to write "@fenced" to disk.
func TestPreflightRefusalsAreASubsetOfTheToolsOwn(t *testing.T) {
	for _, c := range unusableWriteCalls {
		t.Run(c.name, func(t *testing.T) {
			ctx := NewAgentContext(t.TempDir(), Tier2Medium)
			ctx.PermissionMode = PermissionYolo
			ctx.StreamFn = func(string, interface{}) {}

			ok, why := fencedCallIsExecutable("write_file", json.RawMessage(c.args), ctx)
			if ok {
				t.Fatalf("preflight admitted an unusable call")
			}
			if why == "" {
				t.Error("refused without saying why")
			}
			// The tool refuses it too, with no mutation.
			res := executeToolCall("write_file", json.RawMessage(c.args), ctx)
			if res.Success {
				t.Fatalf("the tool accepted what the preflight refused: %s", c.args)
			}
			if res.MutationStatus != MutationNone {
				t.Errorf("MutationStatus = %q, want none", res.MutationStatus)
			}
		})
	}
}

// The tool's refusal of such a call is a classified one, and it leaves nothing
// behind: no file on disk and nothing in the ledger.
func TestAnUnusableWriteIsRefusedWithoutATrace(t *testing.T) {
	for _, c := range unusableWriteCalls {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.PermissionMode = PermissionYolo
			ctx.StreamFn = func(string, interface{}) {}

			// The preflight first, then the tool, as on the loop's path.
			fencedCallIsExecutable("write_file", json.RawMessage(c.args), ctx)
			res := executeToolCall("write_file", json.RawMessage(c.args), ctx)
			if !res.Classified() {
				t.Errorf("unclassified refusal: %+v", res)
			}
			var found []string
			filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
				if err == nil && info != nil && !info.IsDir() {
					found = append(found, p)
				}
				return nil
			})
			if len(found) != 0 {
				t.Errorf("an unusable call put files on disk: %v", found)
			}
			if len(ctx.Ledger) != 0 {
				t.Errorf("an unusable call entered the ledger: %v", ctx.Ledger)
			}
		})
	}
}

// A usable call is still admitted — the preflight is a gate, not a wall.
func TestPreflightAdmitsAUsableCall(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	for _, args := range []string{
		`{"path":"solve.py","content":"@fenced"}`,
		`{"path":"./solve.py","content":"@fenced"}`,
		`{"path":"pkg/solve.py","content":"@fenced"}`,
	} {
		if ok, why := fencedCallIsExecutable("write_file", json.RawMessage(args), ctx); !ok {
			t.Errorf("%s refused: %s", args, why)
		}
	}
}

// fencedCountingStub answers the model with a fixed script and counts the
// fenced sub-calls, each of which sends the block "A = 1".
func fencedCountingStub(t *testing.T, dir string, calls, fences *int, script func(int) map[string]interface{}) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLoopServices(w, r, dir) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "single fenced block") {
			mu.Lock()
			*fences++
			mu.Unlock()
			serveReply(w, "```python\nA = 1\n```")
			return
		}
		mu.Lock()
		i := *calls
		*calls++
		mu.Unlock()
		body, _ := json.Marshal(script(i))
		serveReply(w, string(body))
	}))
}

// Through the real loop: a fenced call that cannot execute starts no
// generation and makes no allowance key, whatever bad path it carries. The
// run ends in a few turns, does not report completion, and writes nothing.
func TestUnusableFencedCallsStartNoGeneration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths []string // the path of each turn's call, in turn; nil: no path
	}{
		{"no path", nil},
		{"unusable spellings", []string{"", "   ", "\t", "../escape.py", ".env"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			calls, fences := 0, 0
			srv := fencedCountingStub(t, dir, &calls, &fences, func(i int) map[string]interface{} {
				args := map[string]string{"content": "@fenced"}
				if tc.paths != nil {
					args["path"] = tc.paths[i%len(tc.paths)]
				}
				return map[string]interface{}{"type": "tool_call", "name": "write_file", "args": args}
			})
			defer srv.Close()

			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.InferenceURL = srv.URL
			ctx.SandboxURL = srv.URL
			ctx.V3URL = srv.URL
			ctx.PermissionMode = PermissionYolo
			ctx.MaxTurns = 20
			var terminal map[string]string
			ctx.StreamFn = func(eventType string, data interface{}) {
				if eventType == "done" {
					b, _ := json.Marshal(data)
					json.Unmarshal(b, &terminal)
				}
			}
			if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
				t.Fatalf("agent loop error: %v", err)
			}

			if fences != 0 {
				t.Errorf("%d fenced generations started for unusable calls", fences)
			}
			if len(ctx.FencedFailures) != 0 {
				t.Errorf("allowance keys created for an unusable call: %v", ctx.FencedFailures)
			}
			if calls > 12 {
				t.Errorf("%d model turns before the run ended", calls)
			}
			if terminal["status"] == "" {
				t.Fatal("no classified terminal")
			}
			if NormalizeTerminalStatus(terminal["status"]).Completed() {
				t.Errorf("a run that wrote nothing reported %q", terminal["status"])
			}
			var found []string
			filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
				if err == nil && info != nil && !info.IsDir() &&
					!strings.Contains(p, ".atlas-mount-probe") {
					found = append(found, p)
				}
				return nil
			})
			if len(found) != 0 {
				t.Errorf("files on disk after unusable calls: %v", found)
			}
			t.Logf("turns=%d status=%q reason=%q", calls, terminal["status"], terminal["reason"])
		})
	}
}

// A usable write still lands its bytes: through the fenced channel with one
// generation, or inline with none.
func TestUsableWritesStillLandThroughTheLoop(t *testing.T) {
	for _, tc := range []struct {
		name       string
		content    string
		wantFences int
		wantDisk   string
	}{
		{"fenced", "@fenced", 1, "A = 1\n"},
		{"inline", "def solve():\n    return 42\n", 0, "def solve():\n    return 42\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			calls, fences := 0, 0
			srv := fencedCountingStub(t, dir, &calls, &fences, func(i int) map[string]interface{} {
				if i == 0 {
					return map[string]interface{}{
						"type": "tool_call", "name": "write_file",
						"args": map[string]string{"path": "solve.py", "content": tc.content}}
				}
				return map[string]interface{}{"type": "done", "summary": "wrote solve.py"}
			})
			defer srv.Close()

			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.InferenceURL = srv.URL
			ctx.SandboxURL = srv.URL
			ctx.V3URL = srv.URL
			ctx.PermissionMode = PermissionYolo
			ctx.MaxTurns = 8
			ctx.StreamFn = func(string, interface{}) {}
			if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
				t.Fatal(err)
			}

			if fences != tc.wantFences {
				t.Errorf("%d fenced generations, want %d", fences, tc.wantFences)
			}
			got, err := os.ReadFile(filepath.Join(dir, "solve.py"))
			if err != nil {
				t.Fatalf("the file never landed: %v", err)
			}
			if string(got) != tc.wantDisk {
				t.Errorf("disk = %q, want %q", got, tc.wantDisk)
			}
			d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
			if d == nil || d.CurrentHash != hashBytes(got) {
				t.Error("the ledger does not describe the bytes on disk")
			}
		})
	}
}
