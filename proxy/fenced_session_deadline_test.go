package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The server owns each session's budget: a work deadline, then a reserve in
// which the loop still ends with a terminal. The loop tests below give the
// work a short deadline, so it passes inside production code.

func TestSessionBudgetBounds(t *testing.T) {
	for _, c := range []struct {
		raw       string
		wantTotal time.Duration
	}{
		{"", 600 * time.Second},
		{"abc", 600 * time.Second},
		{"0", 600 * time.Second},
		{"-1", 600 * time.Second},
		{"119", 600 * time.Second},
		{"3601", 600 * time.Second},
		{"999999", 600 * time.Second},
		{"120", 120 * time.Second},
		{"300", 300 * time.Second},
		{"3600", 3600 * time.Second},
	} {
		t.Run("override="+c.raw, func(t *testing.T) {
			t.Setenv("ATLAS_AGENT_SESSION_TIMEOUT_SEC", c.raw)
			total, reserve := sessionBudget()
			if total != c.wantTotal {
				t.Errorf("total = %v, want %v", total, c.wantTotal)
			}
			if reserve != 30*time.Second {
				t.Errorf("reserve = %v, want 30s", reserve)
			}
			if total-reserve <= 0 {
				t.Error("the work deadline is not positive")
			}
		})
	}
}

// timeoutFixture drives a real agent loop whose work context is already
// expiring, so the deadline lands inside production code rather than being
// simulated. hang controls where: mid-LLM-stream or mid-tool-call.
func timeoutFixture(t *testing.T, dir string, hang string) (*AgentContext, map[string]string, map[string]int) {
	t.Helper()
	stop := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/shell") {
			// A tool call that never returns until the request is cancelled.
			if hang == "tool" {
				select {
				case <-r.Context().Done():
				case <-stop:
				}
				http.Error(w, "cancelled", http.StatusGatewayTimeout)
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"stdout": "", "stderr": "", "exit_code": 0})
			return
		}
		if serveLoopServices(w, r, dir) {
			return
		}
		if hang == "llm" {
			// Stream forever: the deadline has to be what stops this.
			serveDotsForever(w, r, stop)
			return
		}
		call, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "run_command",
			"args": map[string]string{"command": "sleep 60"}})
		serveReply(w, string(call))
	}))
	// Release the hanging handlers before the server waits for them:
	// httptest.Server.Close blocks until every handler has returned, and the
	// "tool" handler returns only when its request dies or `stop` closes.
	// Cleanups run last-registered-first, so a `stop` registered on its own
	// before the server would close only after Close had blocked.
	t.Cleanup(func() {
		close(stop)
		srv.Close()
	})

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 20

	// The two lifetimes the handler builds, with a short work deadline
	// instead of ten minutes.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	t.Cleanup(cancelReq)
	workCtx, cancelWork := context.WithTimeout(reqCtx, 900*time.Millisecond)
	t.Cleanup(cancelWork)
	ctx.RequestCtx = reqCtx
	ctx.Ctx = workCtx
	ctx.cancelWork = cancelWork

	terminal := map[string]string{}
	census := map[string]int{}
	var mu sync.Mutex
	ctx.StreamFn = func(eventType string, data interface{}) {
		mu.Lock()
		defer mu.Unlock()
		census[eventType]++
		if eventType == "done" {
			b, _ := json.Marshal(data)
			json.Unmarshal(b, &terminal)
		}
	}
	return ctx, terminal, census
}

// serveDotsForever streams "." content frames until the client gives up.
func serveDotsForever(w http.ResponseWriter, r *http.Request, stop <-chan struct{}) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-stop:
			return
		default:
		}
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": "."}}}})
		fmt.Fprintf(w, "data: %s\n\n", d)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWorkDeadlineEmitsExactlyOneTimedOutTerminal(t *testing.T) {
	for _, where := range []string{"llm", "tool"} {
		t.Run("deadline during the "+where, func(t *testing.T) {
			dir := t.TempDir()
			ctx, terminal, census := timeoutFixture(t, dir, where)

			if err := runAgentLoop(ctx, "Do something long."); err != nil {
				t.Fatalf("agent loop error: %v", err)
			}

			if terminal["status"] != string(TerminalTimedOut) {
				t.Errorf("status = %q, want timed_out (summary=%q)",
					terminal["status"], terminal["summary"])
			}
			if terminal["reason"] != "work_deadline" {
				t.Errorf("reason = %q", terminal["reason"])
			}
			if census["done"] != 1 {
				t.Errorf("%d terminal events", census["done"])
			}
			if strings.Contains(terminal["summary"], "is on disk and parses") {
				t.Errorf("a timeout implied completion: %q", terminal["summary"])
			}
			t.Logf("terminal=%v", terminal)
		})
	}
}

// The terminal arrives AFTER the work context is dead, which is the whole
// point of the split, while the response lifetime lives on, and well inside
// the reserve.
func TestTheWorkDeadlineEndsTheWorkButNotTheResponse(t *testing.T) {
	for _, where := range []string{"llm", "tool"} {
		t.Run("deadline during the "+where, func(t *testing.T) {
			dir := t.TempDir()
			ctx, _, _ := timeoutFixture(t, dir, where)

			start := time.Now()
			if err := runAgentLoop(ctx, "Do something long."); err != nil {
				t.Fatalf("agent loop error: %v", err)
			}
			elapsed := time.Since(start)

			if ctx.Ctx.Err() == nil {
				t.Error("work context outlived the deadline")
			}
			if ctx.RequestCtx.Err() != nil {
				t.Error("the response lifetime died with the work")
			}
			if elapsed > 20*time.Second {
				t.Errorf("finalisation took %v", elapsed)
			}
			t.Logf("elapsed=%v", elapsed)
		})
	}
}

// A timeout restores a checkpoint only by the usual rules: a checkpoint that
// is shown to be safer is put back, and anything short of that is left alone.
// Whatever is on disk, a timeout is never a completion.
func TestTimeoutRestorationObeysTheSameEligibilityRules(t *testing.T) {
	for _, c := range []struct {
		name      string
		seed      func(t *testing.T, ctx *AgentContext, dir string)
		wantBytes string
	}{
		{name: "demonstrably safer checkpoint is restored",
			seed: func(t *testing.T, ctx *AgentContext, dir string) {
				args, _ := json.Marshal(map[string]string{
					"path": "solve.py", "content": "def f():\n    return [1]\n"})
				executeToolCall("write_file", args, ctx)
				os.WriteFile(filepath.Join(dir, "solve.py"),
					[]byte("def f():\n    return [1]]\n"), 0o644)
			},
			wantBytes: "def f():\n    return [1]\n"},
		{name: "invalid bytes with no checkpoint are kept",
			seed: func(t *testing.T, ctx *AgentContext, dir string) {
				// Written broken from the start, so nothing was ever valid.
				args, _ := json.Marshal(map[string]string{
					"path": "solve.py", "content": "def f():\n    return [1]]\n"})
				executeToolCall("write_file", args, ctx)
			},
			wantBytes: "def f():\n    return [1]]\n"},
		{name: "unknown validation is not a reason to act",
			seed: func(t *testing.T, ctx *AgentContext, dir string) {
				args, _ := json.Marshal(map[string]string{
					"path": "solve.py", "content": "def f():\n    return [1]\n"})
				executeToolCall("write_file", args, ctx)
				os.WriteFile(filepath.Join(dir, "solve.py"),
					[]byte("def f():\n    return [2]\n"), 0o644)
				// Checker gone: the current bytes have no verdict at all.
				ctx.SandboxURL = "http://127.0.0.1:1"
			},
			wantBytes: "def f():\n    return [2]\n"},
		{name: "a valid file the session never wrote is left alone",
			seed: func(t *testing.T, ctx *AgentContext, dir string) {
				os.WriteFile(filepath.Join(dir, "solve.py"), []byte("A = 1\n"), 0o644)
			},
			wantBytes: "A = 1\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx, terminal, _ := timeoutFixture(t, dir, "llm")
			c.seed(t, ctx, dir)

			if err := runAgentLoop(ctx, "Do something long."); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
			if string(got) != c.wantBytes {
				t.Errorf("disk = %q, want %q", got, c.wantBytes)
			}
			if terminal["status"] != string(TerminalTimedOut) {
				t.Errorf("status = %q, want timed_out", terminal["status"])
			}
		})
	}
}

// When a timeout puts back a demonstrably safer checkpoint, the terminal says
// so, and the ledger describes the bytes it put back.
func TestATimeoutRestoreIsDisclosedAndInTheLedger(t *testing.T) {
	dir := t.TempDir()
	ctx, terminal, _ := timeoutFixture(t, dir, "llm")
	args, _ := json.Marshal(map[string]string{
		"path": "solve.py", "content": "def f():\n    return [1]\n"})
	executeToolCall("write_file", args, ctx)
	os.WriteFile(filepath.Join(dir, "solve.py"),
		[]byte("def f():\n    return [1]]\n"), 0o644)

	if err := runAgentLoop(ctx, "Do something long."); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	if !strings.Contains(terminal["summary"], "Put back the last version") {
		t.Errorf("restore was not disclosed: %q", terminal["summary"])
	}
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Error("the ledger does not describe the restored bytes")
	}
}

// The reaper stops this session's jobs and leaves everything else alone.
func TestTimeoutReapsOnlyThisSessionsBackgroundJobs(t *testing.T) {
	var stopped []string
	var mu sync.Mutex
	zero := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stop") {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/stop")
			mu.Lock()
			stopped = append(stopped, id)
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"job_id": id, "killed": true, "exit_code": zero,
				"stdout": []string{}, "stderr": []string{}})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.SandboxURL = srv.URL
	ctx.StreamFn = func(string, interface{}) {}
	ctx.BackgroundJobs = map[string]string{
		"mine-1": "python app.py", "mine-2": "npm start",
	}
	// Hazards are owned by the job identity they came from.
	raiseWorkspaceHazard(ctx, "mine-1")
	raiseWorkspaceHazard(ctx, "mine-2")

	reapSessionBackgroundJobs(ctx)

	mu.Lock()
	got := append([]string(nil), stopped...)
	mu.Unlock()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "mine-1" || got[1] != "mine-2" {
		t.Errorf("reaped %v, want exactly this session's two jobs", got)
	}
	if len(ctx.BackgroundJobs) != 0 {
		t.Errorf("%d jobs still tracked after reaping", len(ctx.BackgroundJobs))
	}
	// Confirmed exits clear the hazard, which is what lets restoration run.
	if workspaceHazardous(ctx) {
		t.Error("hazard still raised after two confirmed exits")
	}
}

// A job that cannot be confirmed gone keeps the hazard raised, and the hazard
// is what stops restoration touching a file something may still be writing.
func TestUnconfirmedJobKeepsTheHazardAndBlocksRestore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No exit_code: signalled, not reaped.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"job_id": "j1", "killed": true, "stdout": []string{}, "stderr": []string{}})
	}))
	defer srv.Close()

	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.SandboxURL = srv.URL
	ctx.StreamFn = func(string, interface{}) {}
	ctx.BackgroundJobs = map[string]string{"j1": "python app.py"}
	raiseWorkspaceHazard(ctx, "j1")

	reapSessionBackgroundJobs(ctx)
	if !workspaceHazardous(ctx) {
		t.Fatal("an unconfirmed exit cleared the hazard")
	}
	if len(restoreSaferDeliverables(ctx)) != 0 {
		t.Error("restoration ran while a job may still be writing")
	}
}

// A client that goes away is not a server timeout.
func TestClientDisconnectIsNotATimeout(t *testing.T) {
	dir := t.TempDir()
	ctx, terminal, census := timeoutFixture(t, dir, "llm")
	// Kill the response lifetime first, as a dropped connection does.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	workCtx, cancelWork := context.WithCancel(reqCtx)
	ctx.RequestCtx = reqCtx
	ctx.Ctx = workCtx
	ctx.cancelWork = cancelWork
	cancelReq()

	err := runAgentLoop(ctx, "Do something long.")
	if err == nil {
		t.Error("a disconnect should surface as the context error")
	}
	if terminal["status"] == string(TerminalTimedOut) {
		t.Error("a disconnect was reported as a server timeout")
	}
	if census["done"] != 0 {
		t.Errorf("%d terminal events emitted into a closed response", census["done"])
	}
}

// A caller that sets only ctx.Ctx, with no separate response lifetime, still
// gets one terminal when the work deadline passes, and nothing is left
// running behind it.
func TestASessionThatRunsOutOfTimeStillEndsWithATerminal(t *testing.T) {
	dir := t.TempDir()
	stop := make(chan struct{})
	defer close(stop)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLoopServices(w, r, dir) {
			return
		}
		serveDotsForever(w, r, stop)
	}))
	defer srv.Close()

	before := runtime.NumGoroutine()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 20
	workCtx, cancelWork := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancelWork()
	ctx.Ctx = workCtx

	var mu sync.Mutex
	terminals := 0
	var payload map[string]string
	ctx.StreamFn = func(eventType string, data interface{}) {
		if eventType != "done" {
			return
		}
		b, _ := json.Marshal(data)
		mu.Lock()
		terminals++
		json.Unmarshal(b, &payload)
		mu.Unlock()
	}

	start := time.Now()
	runAgentLoop(ctx, "Do something long.")
	elapsed := time.Since(start)

	mu.Lock()
	n, got := terminals, payload
	mu.Unlock()

	if n != 1 {
		t.Fatalf("%d terminal events; a session that runs out of time must "+
			"still tell the client what happened", n)
	}
	if got["status"] != "timed_out" {
		t.Errorf("status = %q, want timed_out", got["status"])
	}
	if elapsed > 20*time.Second {
		t.Errorf("the deadline did not bound the run: %v", elapsed)
	}

	// Nothing left running behind it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("goroutines leaked: %d before, %d after", before, after)
	}
	t.Logf("elapsed=%v terminals=%d status=%q", elapsed, n, got["status"])
}
