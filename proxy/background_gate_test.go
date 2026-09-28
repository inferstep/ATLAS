package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A model that verified its work by starting a server and then declared done
// used to get its summary replaced by "Still running in the sandbox" and the
// run reported as unfinished (dev scenario, 2026-09-14). The exit gate now
// names the job and asks for stop_background; a model that complies finishes
// with its own account of the work.
func TestBackgroundGateAsksForTheStopBeforeFinishing(t *testing.T) {
	const good = "def solve():\n    return 7\n\n\nprint(solve())\n"
	zero := 0
	dir := t.TempDir()
	bg := &bgSandbox{running: true}
	sandbox := newBgSandbox(t, dir, bg)

	var mu sync.Mutex
	turns := 0
	plan := func(i int) map[string]interface{} {
		switch i {
		case 0:
			return writeCall("solve.py", good)
		case 1:
			return map[string]interface{}{"type": "tool_call", "name": "run_background",
				"args": map[string]string{"command": "python3 solve.py"}}
		case 2:
			return map[string]interface{}{"type": "done", "summary": "Built solve.py and verified it prints 7."}
		case 3:
			// The model complies; the sandbox reports a reaped exit.
			bg.mu.Lock()
			bg.running, bg.exitCode = false, &zero
			bg.mu.Unlock()
			return map[string]interface{}{"type": "tool_call", "name": "stop_background",
				"args": map[string]string{"job_id": "job1"}}
		}
		return map[string]interface{}{"type": "done", "summary": "Built solve.py and verified it prints 7."}
	}
	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= 12 {
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(call)}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(inference.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL = inference.URL, sandbox.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.MaxTurns = 0
	var gates []string
	terminal := map[string]string{}
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		if et == "gate" {
			var g struct{ Gate, Reason string }
			if json.Unmarshal(b, &g) == nil && g.Gate == "background_gate" {
				gates = append(gates, g.Reason)
			}
		}
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	runAgentLoop(ctx, "Create solve.py so it prints 7.")

	t.Logf("gates=%d status=%q reason=%q summary=%q stops=%v",
		len(gates), terminal["status"], terminal["reason"], terminal["summary"], bg.stopped)
	if len(gates) != 1 {
		t.Fatalf("expected one background_gate bounce, got %d", len(gates))
	}
	if !strings.Contains(gates[0], "job1") || !strings.Contains(gates[0], "stop_background") {
		t.Errorf("the gate must name the job and the call that resolves it: %s", gates[0])
	}
	if bg.stopped == nil || bg.stopped[0] != "job1" {
		t.Errorf("the model's stop_background did not reach the sandbox: %v", bg.stopped)
	}
	if terminal["reason"] == "background_work_unresolved" {
		t.Errorf("a stopped job still counted as unresolved: %q", terminal["summary"])
	}
	if !strings.Contains(terminal["summary"], "Built solve.py") {
		t.Errorf("the model's own summary was replaced: %q", terminal["summary"])
	}
	if strings.Contains(terminal["summary"], "Still running") {
		t.Errorf("a stopped job is still announced as running: %q", terminal["summary"])
	}
}
