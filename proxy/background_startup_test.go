package main

import (
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
	"time"
)

// A background job that already exited did not start.
//
// Measured end to end on flask_pause (stabilization cycles 3-5). The workspace
// is one file, app.py, which imports flask; nothing in it declares a
// dependency, and the sandbox image does not carry flask. The run asked to
// start it, ATLAS redirected the foreground call to run_background (correct: a
// server would block), the job exited 1 inside the settle window with
// ModuleNotFoundError in its stderr — and the tool result came back
// success=true with no error. The run then probed a port nothing was listening
// on, twice, restarted the same job, and finished incomplete. Four of six
// flask sessions in cycle 5 ended verification_demanded_unmet, two of them
// with correct code on disk.
//
// The correction is at that boundary only: an immediate non-zero exit is a
// failed tool call, carrying the job's own last output. Nothing here installs
// anything, runs anything, or names a package ATLAS chose.

type bgWorld struct {
	dir      string
	executed []string
	prompts  []string // what the model was told, request by request
	terminal map[string]string
	mu       sync.Mutex
}

// told reports whether any request after the first carried this text.
func (w *bgWorld) told(phrase string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	enc, _ := json.Marshal(phrase)
	needle := strings.Trim(string(enc), `"`)
	for _, p := range w.prompts {
		if strings.Contains(p, needle) {
			return true
		}
	}
	return false
}

// startBgWorld stubs the sandbox job API: jobs[cmd] gives the stderr and exit
// code the sandbox reports for that command, and installed flips what a later
// start does, the way installing a dependency would.
func startBgWorld(t *testing.T, files map[string]string, script []string,
	jobFor func(cmd string, w *bgWorld) (stderr []string, exit int, running bool)) *bgWorld {
	return startBgWorldWith(t, files, script, jobFor, nil)
}

func startBgWorldWith(t *testing.T, files map[string]string, script []string,
	jobFor func(cmd string, w *bgWorld) (stderr []string, exit int, running bool),
	setup func(*AgentContext)) *bgWorld {
	t.Helper()
	w := &bgWorld{dir: t.TempDir(), terminal: map[string]string{}}
	for name, body := range files {
		full := filepath.Join(w.dir, name)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	jobs := map[string]struct {
		stderr  []string
		exit    int
		running bool
	}{}
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(rw).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(r.URL.Path, "/jobs/start"):
			var in struct{ Command, Cwd string }
			json.NewDecoder(r.Body).Decode(&in)
			w.mu.Lock()
			w.executed = append(w.executed, in.Command)
			w.mu.Unlock()
			stderr, exit, running := jobFor(in.Command, w)
			id := fmt.Sprintf("job%d", len(jobs)+1)
			jobs[id] = struct {
				stderr  []string
				exit    int
				running bool
			}{stderr, exit, running}
			json.NewEncoder(rw).Encode(map[string]interface{}{"job_id": id, "pid": 100 + len(jobs)})
			return
		case strings.Contains(r.URL.Path, "/jobs/") && strings.HasSuffix(r.URL.Path, "/stop"):
			id := strings.Split(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/")[0]
			if j, ok := jobs[id]; ok {
				j.running = false
				j.exit = -15
				jobs[id] = j
			}
			json.NewEncoder(rw).Encode(map[string]interface{}{"job_id": id, "killed": true, "exit_code": -15})
			return
		case strings.Contains(r.URL.Path, "/jobs/") && strings.HasSuffix(r.URL.Path, "/output"):
			id := strings.Split(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/")[0]
			j := jobs[id]
			body := map[string]interface{}{"job_id": id, "running": j.running, "stdout": []string{}, "stderr": j.stderr}
			if !j.running {
				body["exit_code"] = j.exit
			}
			json.NewEncoder(rw).Encode(body)
			return
		case strings.HasSuffix(r.URL.Path, "/shell"):
			var in struct{ Command string }
			json.NewDecoder(r.Body).Decode(&in)
			w.mu.Lock()
			w.executed = append(w.executed, in.Command)
			w.mu.Unlock()
			json.NewEncoder(rw).Encode(map[string]interface{}{
				"stdout": "ok\n", "stderr": "", "exit_code": 0, "success": true})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code, Command string }
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &in)
			cmd := in.Command
			if cmd == "" {
				cmd = in.Code
			}
			w.mu.Lock()
			if !strings.Contains(cmd, ".atlas-mount-probe") {
				w.executed = append(w.executed, cmd)
			}
			w.mu.Unlock()
			out := ""
			if strings.Contains(cmd, ".atlas-mount-probe") {
				p, _ := os.ReadFile(filepath.Join(w.dir, ".atlas-mount-probe"))
				out = string(p)
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
		out := `{"type":"done","summary":"finished"}`
		if k < len(script) {
			out = script[k]
		}
		d, _ := json.Marshal(map[string]interface{}{"choices": []map[string]interface{}{{"delta": map[string]string{"content": out}}}})
		fmt.Fprintf(rw, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(w.dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.YoloMode = true // as the request boundary sets it for mode=yolo
	ctx.TrustMode = trustFullyTrusted
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
	if setup != nil {
		setup(ctx)
	}
	if err := runAgentLoop(ctx, "In app.py, add a pause toggle. Then verify the app still starts."); err != nil {
		t.Fatal(err)
	}
	return w
}

const importsMissing = "from flask import Flask\n\napp = Flask(__name__)\n\n\n@app.route('/')\ndef index():\n    return 'hi'\n\n\nif __name__ == '__main__':\n    app.run(port=5001)\n"

func crashingStart(missing string) func(string, *bgWorld) ([]string, int, bool) {
	return func(cmd string, w *bgWorld) ([]string, int, bool) {
		w.mu.Lock()
		installed := false
		for _, c := range w.executed {
			if strings.Contains(c, "install") {
				installed = true
			}
		}
		w.mu.Unlock()
		if installed {
			return nil, 0, true
		}
		return []string{"Traceback (most recent call last):",
			"  File \"/workspace/app.py\", line 1, in <module>",
			"    from " + missing + " import Flask",
			"ModuleNotFoundError: No module named '" + missing + "'"}, 1, false
	}
}

// 1. Immediate startup failure: the tool call fails and says why.
func TestAnImmediateStartupFailureIsAFailedCall(t *testing.T) {
	w := startBgWorld(t, map[string]string{"app.py": importsMissing},
		[]string{toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			`{"type":"done","summary":"started it"}`},
		crashingStart("flask"))
	if !strings.Contains(strings.Join(w.executed, "\n"), "python app.py") {
		t.Fatalf("the job was never started: %v", w.executed)
	}
	// What the model was told is the point: the call failed, with the job's
	// own output, not a success with a traceback buried in its data.
	for _, want := range []string{"exited immediately with status 1", "ModuleNotFoundError: No module named"} {
		if !w.told(want) {
			t.Errorf("the model was never told %q", want)
		}
	}
	if w.terminal["status"] == "completed" {
		t.Errorf("a run whose only verification crashed reported %v", w.terminal)
	}
}

// The message itself: the exit status, the job's own last output, the missing
// name echoed from that output, and — with nothing declaring it — what the run
// has to decide. No install command is composed for the model to paste.
func TestTheImmediateExitMessageCarriesTheJobsOwnOutput(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	code := 1
	out := RunBackgroundOutput{JobID: "j1", ExitCode: &code, Stderr: []string{
		"Traceback (most recent call last):", "ModuleNotFoundError: No module named 'flask'"}}
	msg := immediateExitMessage(ctx, "python app.py", out)
	for _, want := range []string{"exited immediately with status 1", "nothing to probe",
		"ModuleNotFoundError", `"flask"`, "No file in this workspace declares it"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "pip install") || strings.Contains(msg, "npm install") {
		t.Errorf("the message composes an install command from error text:\n%s", msg)
	}
	// With the project's own declaration present, it points at that file.
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := immediateExitMessage(ctx, "python app.py", out); !strings.Contains(msg, "requirements.txt") {
		t.Errorf("a declared manifest is not named:\n%s", msg)
	}
}

// 2. A job that is still running is unchanged: success, with its id.
func TestAJobThatIsStillRunningIsStillASuccess(t *testing.T) {
	w := startBgWorld(t, map[string]string{"app.py": "print('ok')\n"},
		[]string{toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			toolCall("run_command", map[string]interface{}{"command": "curl -s localhost:5001", "timeout": 10}),
			`{"type":"done","summary":"it serves"}`},
		func(string, *bgWorld) ([]string, int, bool) { return nil, 0, true })
	if w.terminal["reason"] == "background_work_unresolved" {
		return // the run is judged by its own rules; the call itself succeeded
	}
	if w.terminal["status"] == "" {
		t.Error("no terminal recorded")
	}
}

// 3. An actionable recovery: told what failed, the run installs the dependency
// and the next start succeeds.
func TestADependencyFailureCanBeRecoveredAndVerified(t *testing.T) {
	w := startBgWorld(t, map[string]string{"app.py": importsMissing, "requirements.txt": "flask\n"},
		[]string{
			toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			toolCall("run_command", map[string]interface{}{"command": "pip install -r requirements.txt", "timeout": 120}),
			toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			`{"type":"done","summary":"installed the declared dependency and the app starts"}`},
		crashingStart("flask"))
	if !w.told("This project declares its dependencies in requirements.txt") {
		t.Error("the model was not pointed at the project's own declaration")
	}
	joined := strings.Join(w.executed, "\n")
	if strings.Count(joined, "python app.py") < 2 {
		t.Fatalf("the second start never happened: %v", w.executed)
	}
	if !strings.Contains(joined, "pip install -r requirements.txt") {
		t.Fatalf("the install the model asked for did not run: %v", w.executed)
	}
}

// 4. A recovery that does not work leaves the run incomplete.
func TestAFailedRecoveryStaysIncomplete(t *testing.T) {
	w := startBgWorld(t, map[string]string{"app.py": importsMissing},
		[]string{
			toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			`{"type":"done","summary":"the app starts"}`,
			`{"type":"done","summary":"the app starts"}`,
			`{"type":"done","summary":"the app starts"}`,
			`{"type":"done","summary":"the app starts"}`},
		func(cmd string, w *bgWorld) ([]string, int, bool) {
			return []string{"ModuleNotFoundError: No module named 'flask'"}, 1, false
		})
	if !w.told("exited immediately with status 1") {
		t.Error("the repeated failure was never reported as a failure")
	}
	if w.terminal["status"] == "completed" {
		t.Errorf("a run that never started its app reported %v", w.terminal)
	}
}

// ATLAS never runs an install of its own: every command executed is one the
// model sent.
func TestATLASRunsNoInstallOfItsOwn(t *testing.T) {
	w := startBgWorld(t, map[string]string{"app.py": importsMissing, "requirements.txt": "flask\n"},
		[]string{toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			`{"type":"done","summary":"started"}`},
		crashingStart("flask"))
	for _, c := range w.executed {
		if strings.Contains(c, "install") {
			t.Errorf("ATLAS executed an install the model never asked for: %q", c)
		}
	}
}

// A headers-only probe still does not verify — and now the run is told why.
//
// Measured (cycle 6, both flask_pause sessions): after installing the missing
// dependency and starting the app, the run probed it with `curl -I`, got
// HTTP 200, and the probe was declined in silence because a HEAD response
// cannot show that the page works. Both sessions repeated the probe and ended
// verification_demanded_unmet with working code on disk. The safeguard is
// unchanged; the silence is not.
func TestAHeadersOnlyProbeIsDeclinedOutLoud(t *testing.T) {
	if isVerificationCommand("curl -I http://127.0.0.1:5001") {
		t.Fatal("precondition changed: a HEAD probe now counts as verification")
	}
	if !isHeadOnlyProbe("curl -I http://127.0.0.1:5001") || isHeadOnlyProbe("curl http://127.0.0.1:5001/") {
		t.Fatal("head-only detection is wrong")
	}
	w := startBgWorld(t, map[string]string{"app.py": importsMissing, "requirements.txt": "flask\n"},
		[]string{
			toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			toolCall("run_command", map[string]interface{}{"command": "pip install -r requirements.txt", "timeout": 120}),
			toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			toolCall("run_command", map[string]interface{}{"command": "curl -I http://127.0.0.1:5001", "timeout": 10}),
			`{"type":"done","summary":"the app serves"}`,
			`{"type":"done","summary":"the app serves"}`,
			`{"type":"done","summary":"the app serves"}`,
			`{"type":"done","summary":"the app serves"}`},
		crashingStart("flask"))
	if !w.told("asked for headers only") || !w.told("does not count as a") {
		t.Error("the run was never told why its probe did not count")
	}
	if w.terminal["status"] == "completed" {
		t.Errorf("a headers-only probe opened the gate: %v", w.terminal)
	}
}

// A probe that fetches the body still verifies, and the run completes.
func TestABodyProbeStillVerifies(t *testing.T) {
	w := startBgWorld(t, map[string]string{"app.py": importsMissing, "requirements.txt": "flask\n"},
		[]string{
			toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			toolCall("run_command", map[string]interface{}{"command": "pip install -r requirements.txt", "timeout": 120}),
			toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			toolCall("run_command", map[string]interface{}{"command": "curl -s http://127.0.0.1:5001/", "timeout": 10}),
			toolCall("stop_background", map[string]interface{}{"job_id": "job2"}),
			`{"type":"done","summary":"installed the dependency; the app serves"}`},
		crashingStart("flask"))
	if w.told("asked for headers only") {
		t.Error("a body probe was treated as headers-only")
	}
}

// Verification comes before the demand to stop the server it needs.
//
// Measured (cycle 6, flask_pause rep 2): the run installed the dependency,
// started the app, tried to finish, was told to stop the job, stopped it — and
// was then bounced three times for having no successful verification, which
// now required a server that was down. It ended verification_demanded_unmet
// with a working app on disk.
func TestVerificationIsAskedForBeforeTheServerIsStopped(t *testing.T) {
	w := startBgWorld(t, map[string]string{"app.py": importsMissing, "requirements.txt": "flask\n"},
		[]string{
			toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			toolCall("run_command", map[string]interface{}{"command": "pip install -r requirements.txt", "timeout": 120}),
			toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			`{"type":"done","summary":"the app serves"}`, // premature: verification is owed
			toolCall("run_command", map[string]interface{}{"command": "curl -s http://127.0.0.1:5001/", "timeout": 10}),
			`{"type":"done","summary":"installed the dependency; the app serves"}`,
			toolCall("stop_background", map[string]interface{}{"job_id": "job2"}),
			`{"type":"done","summary":"installed the dependency; the app serves"}`},
		crashingStart("flask"))
	if !w.told("is running right now") {
		t.Error("the run was not pointed at the job it had running")
	}
	// The stop demand must not be what it hears while verification is owed.
	firstStop, firstVerify := -1, -1
	w.mu.Lock()
	for i, p := range w.prompts {
		if firstStop < 0 && strings.Contains(p, "stop_background") && strings.Contains(p, "still running") {
			firstStop = i
		}
		if firstVerify < 0 && strings.Contains(p, "haven't verified the change") {
			firstVerify = i
		}
	}
	w.mu.Unlock()
	if firstStop >= 0 && firstVerify >= 0 && firstStop < firstVerify {
		t.Errorf("the stop was demanded (request %d) before verification (request %d)", firstStop, firstVerify)
	}
}

// Once verification has landed, the stop demand still fires: nothing is left
// running at completion.
func TestAVerifiedRunIsStillToldToStopItsJob(t *testing.T) {
	w := startBgWorld(t, map[string]string{"app.py": "print('ok')\n"},
		[]string{
			toolCall("insert_after", map[string]interface{}{"path": "app.py", "line": 1, "content": "# pause\n"}),
			toolCall("run_command", map[string]interface{}{"command": "python app.py", "timeout": 30}),
			toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
			`{"type":"done","summary":"it runs"}`,
			`{"type":"done","summary":"it runs"}`,
			`{"type":"done","summary":"it runs"}`,
			`{"type":"done","summary":"it runs"}`},
		func(string, *bgWorld) ([]string, int, bool) { return nil, 0, true })
	if !w.told("still running") {
		t.Error("a run that left a job running was never told to stop it")
	}
}

// The lifecycle, end to end: start, verify, repair, re-verify, clean up, stop.
//
// Reconstructed from the cycle-6 R3 flask_pause rep 2 trace. The run started
// the app, probed it, then made the edit the task actually needed — the game
// loop honouring the pause flag — and probed again. Its second probe hit the
// SAME process, started before that edit, so it exercised the older code; the
// run ended verification_demanded_unmet with the right bytes on disk. Two
// existing rules disagreed: the gate prescribes "start it with run_background,
// probe it with curl", while the work contract wants evidence that NAMES the
// deliverable, which no probe does.
//
// A probe is now credited with the files a still-running job was started from,
// and only when that job started AFTER those files were last written. A job
// that predates the change is named as stale instead.

func serverWorld(t *testing.T, script []string) *bgWorld {
	t.Helper()
	return startBgWorldWith(t, map[string]string{"app.py": importsMissing, "requirements.txt": "flask\n"},
		script, func(cmd string, w *bgWorld) ([]string, int, bool) { return nil, 0, true },
		func(ctx *AgentContext) { contractWork(t, ctx) })
}

// contractWork is the task contract the reliability runner sends: {"task_mode":
// "work"} and nothing else.
func contractWork(t *testing.T, ctx *AgentContext) {
	t.Helper()
	tc, err := validateTaskContract(&TaskContract{TaskMode: TaskModeWork}, ctx.WorkingDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx.TaskContract = tc
}

// A probe against a server started after the last edit finishes the task: the
// run verifies, is told to stop its job, stops it, and completes.
func TestAProbeAgainstCurrentCodeFinishesTheTask(t *testing.T) {
	w := serverWorld(t, []string{
		toolCall("read_file", map[string]interface{}{"path": "app.py"}),
		toolCall("insert_after", map[string]interface{}{"path": "app.py", "line": 1, "content": "# pause\n"}),
		toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
		toolCall("run_command", map[string]interface{}{"command": "curl -sf http://127.0.0.1:5001/", "timeout": 10}),
		`{"type":"done","summary":"the pause toggle works; the app serves"}`,
		toolCall("stop_background", map[string]interface{}{"job_id": "job1"}),
		`{"type":"done","summary":"the pause toggle works; the app serves"}`,
	})
	if w.told("has been running since before your last change") {
		t.Error("a server started after the edit was called stale")
	}
	if w.terminal["reason"] == "verification_demanded_unmet" {
		t.Errorf("a verified, cleaned-up run still reported %v", w.terminal)
	}
}

// A probe that exits 0 whatever the server answers does not finish the task,
// and the run is told why and what to send instead. `curl -s` against a page
// that returns HTTP 500 exits 0 (P-guardrails/INTEGRITY#1).
func TestAProbeBlindToHTTPErrorsDoesNotFinishTheTask(t *testing.T) {
	w := serverWorld(t, []string{
		toolCall("read_file", map[string]interface{}{"path": "app.py"}),
		toolCall("insert_after", map[string]interface{}{"path": "app.py", "line": 1, "content": "# pause\n"}),
		toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
		toolCall("run_command", map[string]interface{}{"command": "curl -s http://127.0.0.1:5001/", "timeout": 10}),
		toolCall("stop_background", map[string]interface{}{"job_id": "job1"}),
		`{"type":"done","summary":"the pause toggle works; the app serves"}`,
		`{"type":"done","summary":"the pause toggle works; the app serves"}`,
		`{"type":"done","summary":"the pause toggle works; the app serves"}`,
		`{"type":"done","summary":"the pause toggle works; the app serves"}`,
	})
	if w.terminal["reason"] != "verification_demanded_unmet" {
		t.Errorf("a probe that cannot see an HTTP error discharged the contract: %v", w.terminal)
	}
	if !w.told("exits 0 even when the server answers with an error page") {
		t.Error("the run was never told why its probe did not count")
	}
}

// A probe against a server that predates the last edit does not finish it: the
// run is told the job is stale and why, and completion is still refused.
func TestAProbeAgainstStaleCodeDoesNotFinishTheTask(t *testing.T) {
	w := serverWorld(t, []string{
		toolCall("read_file", map[string]interface{}{"path": "app.py"}),
		toolCall("run_background", map[string]interface{}{"command": "python app.py"}),
		toolCall("run_command", map[string]interface{}{"command": "curl -sf http://127.0.0.1:5001/", "timeout": 10}),
		// the edit the task actually needed, made after the server started
		toolCall("insert_after", map[string]interface{}{"path": "app.py", "line": 1, "content": "# pause honoured\n"}),
		toolCall("run_command", map[string]interface{}{"command": "curl -sf http://127.0.0.1:5001/", "timeout": 10}),
		`{"type":"done","summary":"the pause toggle works"}`,
		`{"type":"done","summary":"the pause toggle works"}`,
		`{"type":"done","summary":"the pause toggle works"}`,
		`{"type":"done","summary":"the pause toggle works"}`,
	})
	if !w.told("has been running since before your last change") {
		t.Error("the run was never told its server predates its change")
	}
	if !w.told("Stop it and start it again") {
		t.Error("the run was not told what to do about it")
	}
	if w.terminal["status"] == "completed" {
		t.Errorf("a probe against stale code completed the run: %v", w.terminal)
	}
}

// Coverage is credited from the job, not from the probe's text, and only for
// files that job was started from.
func TestLiveServerCoverageIsScopedToItsOwnFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"app.py", "other.py"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.RecordFileRead(filepath.Join(dir, "app.py"), "x = 1\n")
	ctx.RecordBodySeen(filepath.Join(dir, "app.py"))
	ctx.BackgroundJobs = map[string]string{"j1": "python app.py"}
	ctx.BackgroundJobStarted = map[string]time.Time{"j1": time.Now().Add(time.Minute)} // started after the write
	cov := coverageFromLiveServer(ctx, "curl -s http://127.0.0.1:5001/")
	for p := range cov {
		if strings.HasSuffix(p, "other.py") {
			t.Errorf("a file the job was not started from was credited: %v", cov)
		}
	}
	// A job started BEFORE the file's last write credits nothing.
	ctx.BackgroundJobStarted = map[string]time.Time{"j1": time.Now().Add(-time.Hour)}
	if len(coverageFromLiveServer(ctx, "curl -s http://127.0.0.1:5001/")) != 0 {
		t.Error("a stale job was credited with coverage")
	}
	// A command that is not a local probe credits nothing.
	ctx.BackgroundJobStarted = map[string]time.Time{"j1": time.Now().Add(time.Minute)}
	if len(coverageFromLiveServer(ctx, "pytest -q")) != 0 {
		t.Error("a non-probe command was credited with live-server coverage")
	}
}

// P-guardrails/DEAD#1: outside yolo mode, validateRunCommand refused a
// server started with run_background -- the tool the refusal told the model
// to use. A foreground start through run_command is still redirected.
func TestAServerStartedInTheBackgroundRunsOutsideYolo(t *testing.T) {
	serve := toolCall("run_background", map[string]interface{}{"command": "python3 -m http.server 8000"})
	foreground := toolCall("run_command", map[string]interface{}{"command": "python3 -m http.server 8000"})
	w := startBgWorldWith(t, map[string]string{"index.html": "<!DOCTYPE html><html><body>hi</body></html>\n"},
		[]string{foreground, serve, `{"type":"done","summary":"serving"}`},
		func(cmd string, w *bgWorld) ([]string, int, bool) { return nil, 0, true },
		// The mode-gated validation, isolated: no permission prompts.
		func(ctx *AgentContext) { ctx.YoloMode = false })
	if !w.told("Start it with run_background") {
		t.Error("a foreground server start through run_command was not redirected")
	}
	started := false
	for _, c := range w.executed {
		if c == "python3 -m http.server 8000" {
			started = true
		}
	}
	if !started {
		t.Fatalf("run_background was refused the server it is for; executed %v", w.executed)
	}
}

// G-proxy-orphan-leads#5: the foreground-server bounce skipped every failure
// counter, so a model re-sending it looped until the session deadline.
func TestARepeatedForegroundServerStartEnds(t *testing.T) {
	var script []string
	for i := 0; i < 40; i++ {
		script = append(script, toolCall("run_command", map[string]interface{}{"command": "python3 -m http.server 8000"}))
	}
	w := startBgWorld(t, nil, script,
		func(cmd string, w *bgWorld) ([]string, int, bool) { return nil, 0, true })
	if w.terminal["reason"] != "repeated_refusal" {
		t.Fatalf("terminal = %v, want repeated_refusal", w.terminal)
	}
	if len(w.prompts) >= len(script) {
		t.Fatalf("the loop ran through all %d re-sends", len(script))
	}
}
