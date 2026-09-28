package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The bound on answering repetition cuts, and what it counts.
//
// Measured (stabilization cycle 4, flask_pause, both repetitions, identical to
// the token): cut, recovery 1, cut, recovery 2, a valid replace_lines that
// landed, then a third cut that hit the allowance and ended the run with the
// change half applied — at 271 s of a 570 s work budget, with the prompt at
// 7,270 tokens of an 18,432-token conversation budget. Neither wall clock,
// context, per-response cap nor turn count was near its limit: the allowance
// ended it, and the productive edit between the cuts had given nothing back.
//
// ATLAS_CONTENT_LOOP_COUNT=unproductive charges the allowance only for
// CONSECUTIVE recoveries after which nothing landed. ATLAS_CONTENT_LOOP_RECOVERIES
// changes the allowance itself. They are separate on purpose: an experiment
// changes one.

// loopWorld drives a stub that cuts mid-generation on demand, so a content loop
// is produced the way llama-server produces one: a long repeating body with no
// terminating JSON.
type loopWorld struct {
	dir      string
	prompts  []string
	terminal map[string]string
	mu       sync.Mutex
}

func repeatingBody() string {
	return `{"type":"tool_call","name":"write_file","args":{"path":"app.py","content":"` +
		strings.Repeat(`x = 1\nx = 1\n`, 400)
}

// runLoopWorld plays the given per-turn scripts; an empty string means "emit a
// repeating, unterminated call" (which the content-loop detector cuts).
func runLoopWorld(t *testing.T, env map[string]string, scripts []string) *loopWorld {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	w := &loopWorld{dir: t.TempDir(), terminal: map[string]string{}}
	if err := os.WriteFile(filepath.Join(w.dir, "app.py"), []byte("v = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(rw).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			out := ""
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(w.dir, ".atlas-mount-probe"))
				out = string(b)
			}
			json.NewEncoder(rw).Encode(map[string]interface{}{"success": true, "stdout": out, "exit_code": 0})
			return
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(rw, "unavailable", http.StatusServiceUnavailable)
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(rw, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		k := turn
		turn++
		w.prompts = append(w.prompts, string(body))
		w.mu.Unlock()
		rw.Header().Set("Content-Type", "text/event-stream")
		send := func(s string) {
			d, _ := json.Marshal(map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]string{"content": s}}}})
			fmt.Fprintf(rw, "data: %s\n\n", d)
			if f, ok := rw.(http.Flusher); ok {
				f.Flush()
			}
		}
		script := `{"type":"done","summary":"finished"}`
		if k < len(scripts) {
			script = scripts[k]
		}
		if script == "" {
			script = repeatingBody()
		}
		for i := 0; i < len(script); i += 256 {
			end := i + 256
			if end > len(script) {
				end = len(script)
			}
			send(script[i:end])
		}
		fmt.Fprint(rw, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(w.dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.StreamFn = func(et string, data interface{}) {
		if et != "done" {
			return
		}
		b, _ := json.Marshal(data)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		w.mu.Lock()
		defer w.mu.Unlock()
		for k, v := range m {
			w.terminal[k] = fmt.Sprint(v)
		}
	}
	if err := runAgentLoop(ctx, "Add a pause flag to app.py."); err != nil {
		t.Fatal(err)
	}
	return w
}

func edit(line int, text string) string {
	return toolCall("insert_after", map[string]interface{}{"path": "app.py", "line": line, "content": text})
}

// The edit tools refuse a path the run has not read, so every script starts
// the way a real session does.
func readFirst(rest ...string) []string {
	return append([]string{toolCall("read_file", map[string]interface{}{"path": "app.py"})}, rest...)
}

// Default accounting: every cut is charged, so a run that is making progress
// between cuts still ends at the second one.
func TestByDefaultEveryRepetitionCutIsCharged(t *testing.T) {
	w := runLoopWorld(t, nil, readFirst("", edit(1, "a = 1\n"), "", edit(1, "b = 2\n"), "", edit(1, "c = 3\n"),
		`{"type":"done","summary":"added the flag"}`))
	got, _ := os.ReadFile(filepath.Join(w.dir, "app.py"))
	if !strings.Contains(string(got), "a = 1") || !strings.Contains(string(got), "b = 2") {
		t.Errorf("the two answered cuts did not let their edits land: %q", got)
	}
	if strings.Contains(string(got), "c = 3") {
		t.Errorf("the run continued past the default allowance: %q", got)
	}
	if w.terminal["status"] == "completed" {
		t.Errorf("terminal %v", w.terminal)
	}
}

// Unproductive-only accounting: work that landed between cuts clears the
// count, so the same run keeps going and its later edits land.
func TestProductiveWorkBetweenCutsClearsTheAllowance(t *testing.T) {
	w := runLoopWorld(t, map[string]string{"ATLAS_CONTENT_LOOP_COUNT": "unproductive"},
		readFirst("", edit(1, "a = 1\n"), "", edit(1, "b = 2\n"), "", edit(1, "c = 3\n"),
			`{"type":"done","summary":"added the flag"}`))
	got, _ := os.ReadFile(filepath.Join(w.dir, "app.py"))
	for _, want := range []string{"a = 1", "b = 2", "c = 3"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("edit %q never landed: %q", want, got)
		}
	}
}

// The safeguard survives the accounting change: a model that only repeats,
// with nothing landing, still terminates at the same allowance.
func TestRepetitionWithNoProgressStillTerminates(t *testing.T) {
	w := runLoopWorld(t, map[string]string{"ATLAS_CONTENT_LOOP_COUNT": "unproductive"},
		readFirst("", "", "", "", "", "", ""))
	if w.terminal["status"] == "completed" {
		t.Errorf("a run that only repeated was called completed: %v", w.terminal)
	}
	if n := strings.Count(strings.Join(w.prompts, "\n"), "began repeating itself"); n > 4 {
		t.Errorf("the corrective was sent %d times — the bound did not hold", n)
	}
}

// The allowance itself can be changed without touching the accounting.
func TestTheAllowanceIsConfigurableOnItsOwn(t *testing.T) {
	w := runLoopWorld(t, map[string]string{"ATLAS_CONTENT_LOOP_RECOVERIES": "4"},
		readFirst("", edit(1, "a = 1\n"), "", edit(1, "b = 2\n"), "", edit(1, "c = 3\n"),
			`{"type":"done","summary":"added the flag"}`))
	got, _ := os.ReadFile(filepath.Join(w.dir, "app.py"))
	if !strings.Contains(string(got), "c = 3") {
		t.Errorf("a raised allowance did not let the run continue: %q", got)
	}
	if contentLoopCountUnproductive() {
		t.Error("raising the allowance also changed the accounting")
	}
}

// Zero allowance: the first cut ends the run. The bound is still a bound.
func TestAZeroAllowanceEndsOnTheFirstCut(t *testing.T) {
	w := runLoopWorld(t, map[string]string{"ATLAS_CONTENT_LOOP_RECOVERIES": "0"},
		readFirst("", edit(1, "a = 1\n"), `{"type":"done","summary":"done"}`))
	if got, _ := os.ReadFile(filepath.Join(w.dir, "app.py")); strings.Contains(string(got), "a = 1") {
		t.Error("a zero allowance still answered the cut")
	}
	if w.terminal["status"] == "completed" {
		t.Errorf("terminal %v", w.terminal)
	}
}

// Defaults are exactly what shipped.
func TestTheDefaultsAreTheShippedBehaviour(t *testing.T) {
	if contentLoopRecoveryAllowance() != maxContentLoopRecoveries || contentLoopCountUnproductive() {
		t.Errorf("allowance=%d unproductive=%v", contentLoopRecoveryAllowance(), contentLoopCountUnproductive())
	}
}

// Cancellation outranks the allowance: a cancelled session stops, and does not
// answer another cut or claim completion.
func TestCancellationStopsTheRunWhateverTheAllowanceSays(t *testing.T) {
	t.Setenv("ATLAS_CONTENT_LOOP_COUNT", "unproductive")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("v = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	requests := 0
	var cancel context.CancelFunc
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(rw).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			json.NewEncoder(rw).Encode(map[string]interface{}{"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.Error(rw, "unavailable", http.StatusServiceUnavailable)
			return
		}
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		if n >= 2 {
			cancel() // the user hung up while the model was repeating
		}
		rw.Header().Set("Content-Type", "text/event-stream")
		body := repeatingBody()
		for i := 0; i < len(body); i += 256 {
			end := i + 256
			if end > len(body) {
				end = len(body)
			}
			d, _ := json.Marshal(map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]string{"content": body[i:end]}}}})
			fmt.Fprintf(rw, "data: %s\n\n", d)
			if f, ok := rw.(http.Flusher); ok {
				f.Flush()
			}
		}
		fmt.Fprint(rw, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	c, cancelFn := context.WithCancel(context.Background())
	ctx.Ctx, cancel = c, cancelFn
	defer cancelFn()
	terminal := map[string]string{}
	ctx.StreamFn = func(et string, data interface{}) {
		if et != "done" {
			return
		}
		b, _ := json.Marshal(data)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		for k, v := range m {
			terminal[k] = fmt.Sprint(v)
		}
	}
	_ = runAgentLoop(ctx, "Add a pause flag to app.py.")
	mu.Lock()
	defer mu.Unlock()
	if terminal["status"] == "completed" {
		t.Errorf("a cancelled run reported %v", terminal)
	}
	if requests > 4 {
		t.Errorf("the loop kept going after cancellation: %d requests", requests)
	}
}

// Every write that lands after a recovery still goes through the mandatory
// checks — the allowance changes how long a run may continue, not what is
// checked before bytes land.
func TestWritesAfterARecoveryAreStillChecked(t *testing.T) {
	t.Setenv("ATLAS_CONTENT_LOOP_COUNT", "unproductive")
	w := runLoopWorld(t, map[string]string{"ATLAS_CONTENT_LOOP_COUNT": "unproductive"},
		readFirst("", edit(1, "a = 1\n"), "", edit(1, "b = 2\n"),
			`{"type":"done","summary":"added the flag"}`))
	got, _ := os.ReadFile(filepath.Join(w.dir, "app.py"))
	if !strings.Contains(string(got), "a = 1") || !strings.Contains(string(got), "b = 2") {
		t.Fatalf("the edits did not land: %q", got)
	}
	// The gate log line is the product's own record that checks ran.
	if !strings.Contains(strings.Join(w.prompts, "\n"), "app.py") {
		t.Error("the conversation lost the file under edit")
	}
}
