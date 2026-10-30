package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestRecoverCutCallKeepsOnlyCompleteTokens(t *testing.T) {
	for _, c := range []struct {
		name, raw string
		want      recoveredCall
	}{
		{"content cut", `{"type":"tool_call","name":"structural_edit","args":{"path":"app.py","selector":"function:index","content":"A = \"\"\"\n<html>\n<body>\n<p>x</p>`,
			recoveredCall{Tool: "structural_edit", Args: map[string]string{"path": "app.py", "selector": "function:index"},
				CutField: "content", CutLines: 4}},
		{"path cut", `{"type":"tool_call","name":"write_file","args":{"path":"src/ma`,
			recoveredCall{Tool: "write_file", Args: map[string]string{}, CutField: "path", CutLines: 1}},
		{"name cut", `{"type":"tool_call","name":"struct`,
			recoveredCall{Args: map[string]string{}}},
		{"selector cut", `{"type":"tool_call","name":"structural_edit","args":{"path":"app.py","selector":"function:ind`,
			recoveredCall{Tool: "structural_edit", Args: map[string]string{"path": "app.py"}, CutField: "selector", CutLines: 1}},
		// The cut field's key holds a quote, which the raw text escapes.
		{"cut field with a quote in its key", `{"type":"tool_call","name":"write_file","args":{"path":"a.txt","con\"tent":"line 1\nline 2\nline 3`,
			recoveredCall{Tool: "write_file", Args: map[string]string{"path": "a.txt"}, CutField: "con\"tent", CutLines: 3}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := recoverCutCall(c.raw); !reflect.DeepEqual(got, c.want) {
				t.Errorf("recoverCutCall = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestACutCallDiagnosticNeverNamesWhatDidNotArrive(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	if d := cutCallDiagnostic(ctx, `{"type":"tool_call","name":"write_file","args":{"path":"src/ma`); !strings.Contains(d, "cut before its path was complete") || strings.Contains(d, "src/ma") {
		t.Errorf("incomplete path: %q", d)
	}
	if d := cutCallDiagnostic(ctx, `{"type":"tool_call","name":"write_file","args":{"path":"../../etc/passwd","content":"x`); !strings.Contains(d, "outside the workspace") || strings.Contains(d, "passwd") {
		t.Errorf("path outside the workspace: %q", d)
	}
	if d := cutCallDiagnostic(ctx, `{"type":"tool_call","name":"read_fi`); d != "" {
		t.Errorf("no write tool named, no diagnostic: %q", d)
	}
	if d := cutCallDiagnostic(ctx, `{"type":"tool_call","name":"write_file","args":{"path":"new.py","content":"def f():\n    re`); !strings.Contains(d, "for new.py. Nothing was executed.") || !strings.Contains(d, `"@fenced"`) {
		t.Errorf("a new file: %q", d)
	}
}

// flaskCutWorkspace is the captured flask cut: a workspace holding the task's
// own app.py, that file's bytes, and the deltas the flask_pause session
// streamed for its cut structural_edit (relay capture, stabilization2/C
// session 29).
func flaskCutWorkspace(t *testing.T) (dir string, app []byte, deltas []string) {
	t.Helper()
	app, err := os.ReadFile("../scripts/fixtures/snake_app.py")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/flask_cut_response.json")
	if err != nil {
		t.Fatal(err)
	}
	var captured struct{ Deltas []string }
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), app, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, app, captured.Deltas
}

// flaskCutRun records the chat requests the agent loop sends to flaskCutServer.
type flaskCutRun struct {
	mu      sync.Mutex
	prompts []string // the body of each chat request, in order
}

// flaskCutServer stands in for the sandbox, V3 and the model while the agent
// loop works in dir. Every syntax check passes and every command succeeds
// (the mount probe is answered from dir); V3 is unavailable. Each chat request
// is recorded in run.prompts and answered by streaming, one delta each, the
// strings reply returns for its turn, counted from 0.
func flaskCutServer(dir string, run *flaskCutRun, reply func(turn int) []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			out := ""
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				out = string(b)
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "stdout": out, "exit_code": 0})
			return
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		run.mu.Lock()
		turn := len(run.prompts)
		run.prompts = append(run.prompts, string(body))
		run.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, s := range reply(turn) {
			d, _ := json.Marshal(map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]string{"content": s}}}})
			fmt.Fprintf(w, "data: %s\n\n", d)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// The captured failure, replayed through the agent loop: the flask_pause
// session's cut structural_edit (relay capture, stabilization2/C session 29,
// streamed delta by delta) against the task's own app.py. The loop must refuse
// it without touching app.py, tell the model what the prefix and the file
// establish, and a retry of the kind it names must land.
func TestTheCapturedFlaskCutIsRefusedAndTheNamedRecoveryLands(t *testing.T) {
	dir, app, deltas := flaskCutWorkspace(t)
	lines := strings.Split(string(app), "\n")
	// The retry inserts after the line that sets the score, counted from 1.
	target := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "let score = 0;") }) + 1
	if target == 0 {
		t.Fatal("fixture changed: no `let score = 0;` line")
	}

	run := &flaskCutRun{}
	srv := flaskCutServer(dir, run, func(turn int) []string {
		switch turn {
		case 0:
			return []string{toolCall("read_file", map[string]interface{}{"path": "app.py"})}
		case 1:
			return deltas
		case 2:
			return []string{toolCall("insert_after", map[string]interface{}{"path": "app.py", "line": target,
				"content": "        let isPaused = false;\n"})}
		default:
			return []string{`{"type":"done","summary":"added a pause flag"}`}
		}
	})
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	var diskAfterCut string
	ctx.StreamFn = func(event string, data interface{}) {
		if (event == "error" || event == "agent_loop_recovery") && diskAfterCut == "" {
			b, _ := os.ReadFile(filepath.Join(dir, "app.py"))
			diskAfterCut = string(b)
		}
	}
	if err := runAgentLoop(ctx, "Add a pause toggle to the snake game."); err != nil {
		t.Fatal(err)
	}
	if diskAfterCut != string(app) {
		t.Fatal("app.py changed when the cut call was refused")
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.prompts) < 3 {
		t.Fatalf("the loop stopped after %d requests", len(run.prompts))
	}
	told := run.prompts[2]
	enc := func(s string) string { b, _ := json.Marshal(s); return strings.Trim(string(b), `"`) }
	for _, want := range []string{
		"unfinished structural_edit call on app.py",
		"Nothing was executed and app.py is unchanged",
		"cut inside `content`",
		"`function:index` is line 189",
		"`HTML_TEMPLATE` (lines 6-186), a module-level string",
		"replace_lines with the line numbers read_file printed",
		"insert_after",
	} {
		if !strings.Contains(told, enc(want)) {
			t.Errorf("the refusal the model saw lacks %q", want)
		}
	}
	if strings.Contains(told, enc("Write the CODE that processes the data")) {
		t.Error("the generic data-retyping advice was sent instead of the grounded refusal")
	}
	want := strings.Join(append(append(append([]string{}, lines[:target]...), "        let isPaused = false;"), lines[target:]...), "\n")
	if got, _ := os.ReadFile(filepath.Join(dir, "app.py")); string(got) != want {
		t.Error("the recovery insert_after did not land as sent")
	}
}

// Prints the refusal the product sends for the captured flask cut, using a real
// v3-service outline, so a replay can use the exact bytes. Runs only when
// ATLAS_PROBE_PRINT_V3 names a v3-service URL.
func TestPrintTheCapturedFlaskCutRefusal(t *testing.T) {
	v3 := os.Getenv("ATLAS_PROBE_PRINT_V3")
	if v3 == "" {
		t.Skip("set ATLAS_PROBE_PRINT_V3 to a v3-service URL")
	}
	dir, _, deltas := flaskCutWorkspace(t)
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.V3URL = v3
	_, feedback := parseFailureFeedback(ctx, strings.Join(deltas, ""), "content_loop")
	out := os.Getenv("ATLAS_PROBE_PRINT_OUT")
	if out != "" {
		if err := os.WriteFile(out, []byte(feedback), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Log(feedback)
}

// The cut attempt goes back into the conversation, so the retry does not
// resume from the prefix that produced it.
//
// Probe L (stabilization cycle 3): with the attempt present and the same
// grounded refusal after it, 10 of 10 generations from the captured context
// made a valid small edit; with the refusal alone, 1 of 10.
func TestTheCutAttemptIsPutBackIntoTheConversation(t *testing.T) {
	dir, app, deltas := flaskCutWorkspace(t)
	run := &flaskCutRun{}
	srv := flaskCutServer(dir, run, func(turn int) []string {
		if turn == 0 {
			return deltas
		}
		return []string{`{"type":"done","summary":"stopping here"}`}
	})
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	if err := runAgentLoop(ctx, "Add a pause toggle to the snake game."); err != nil {
		t.Fatal(err)
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.prompts) < 2 {
		t.Fatalf("the loop stopped after %d requests", len(run.prompts))
	}
	var req struct {
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	if err := json.Unmarshal([]byte(run.prompts[1]), &req); err != nil {
		t.Fatal(err)
	}
	last := req.Messages[len(req.Messages)-2:]
	if last[0].Role != "assistant" {
		t.Fatalf("the turn before the refusal is %q, not the attempt", last[0].Role)
	}
	sent := strings.Join(deltas, "")
	if !strings.HasPrefix(sent, string([]rune(last[0].Content)[:cutAttemptKeepRunes])) {
		t.Error("the echoed attempt is not what the model sent")
	}
	if !strings.Contains(last[0].Content, "cut here") {
		t.Error("the echoed attempt does not say it was cut")
	}
	if last[1].Role != "user" || !strings.Contains(last[1].Content, "app.py is unchanged") {
		t.Errorf("the grounded refusal does not follow the attempt: %q", last[1].Content)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "app.py")); string(got) != string(app) {
		t.Error("app.py changed")
	}
}

// The echo of the attempt carries only what arrived: a long attempt keeps its
// first cutAttemptKeepRunes runes, and says it was cut only when it was.
func TestTheCutAttemptEchoCarriesOnlyWhatArrived(t *testing.T) {
	short := `{"type":"tool_call","name":"write_file"`
	if got := attemptEcho(short+"\n", true); got != short {
		t.Errorf("a short attempt was changed: %q", got)
	}
	long := strings.Repeat("x", cutAttemptKeepRunes+50)
	got := attemptEcho(long, true)
	if !strings.HasPrefix(long, string([]rune(got)[:cutAttemptKeepRunes])) {
		t.Error("the echo is not a prefix of what arrived")
	}
	if !strings.Contains(got, "cut here") || !strings.Contains(got, "nothing was executed") {
		t.Errorf("the elision is not stated: %q", got[len(got)-90:])
	}
	if n := len([]rune(attemptEcho(long, false))); n <= cutAttemptKeepRunes {
		t.Errorf("an elided reply lost its marker: got %v", n)
	}
	if strings.Contains(attemptEcho(long, false), "cut here") {
		t.Error("a reply that was not cut is described as cut")
	}
}
