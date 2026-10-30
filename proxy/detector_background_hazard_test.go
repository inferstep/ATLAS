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
)

// --- Live workspace hazards at completion ------------------------------------
//
// run_background raises a hazard and only a confirmed exit lowers it.
// Completion must ask about it: a server started mid-run could keep rewriting
// a tracked deliverable after the run took its hash.

// bgSandbox stands in for the sandbox's job endpoints. running and exitCode
// decide what /jobs/{id}/output reports, and stopped records each reaped job.
type bgSandbox struct {
	mu       sync.Mutex
	running  bool
	started  int
	exitCode *int
	tailed   int
	stopped  []string
	// mutate rewrites the deliverable when the job is observed as exited,
	// standing in for a process that changed the file on its way out.
	mutate func()
	// dir is the workspace the mount probe reads from; newBgSandbox sets it.
	dir string
}

func newBgSandbox(t *testing.T, dir string, bg *bgSandbox) *httptest.Server {
	t.Helper()
	bg.dir = dir
	srv := httptest.NewServer(bg)
	t.Cleanup(srv.Close)
	return srv
}

// ServeHTTP answers the sandbox endpoints the agent calls: the syntax check,
// execute, and the background job endpoints.
func (bg *bgSandbox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/syntax-check"):
		var in struct{ Code string }
		json.NewDecoder(r.Body).Decode(&in)
		valid := !strings.Contains(in.Code, "]]")
		out := map[string]interface{}{"valid": valid}
		if !valid {
			out["errors"] = []string{"SyntaxError: unmatched ']'"}
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasSuffix(r.URL.Path, "/execute"):
		var in struct{ Code string }
		json.NewDecoder(r.Body).Decode(&in)
		if strings.Contains(in.Code, ".atlas-mount-probe") {
			b, _ := os.ReadFile(filepath.Join(bg.dir, ".atlas-mount-probe"))
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": string(b), "exit_code": 0})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "stdout": "", "exit_code": 0})
	case strings.HasSuffix(r.URL.Path, "/jobs/start"):
		// Unique per start, as the real sandbox does: the hazard is
		// raised per start and lowered per reaped job, so a stub reusing
		// one id would manufacture a mismatch production does not have.
		bg.mu.Lock()
		bg.started++
		id := fmt.Sprintf("job%d", bg.started)
		bg.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"job_id": id, "pid": 4242})
	case strings.Contains(r.URL.Path, "/output"):
		bg.mu.Lock()
		bg.tailed++
		running, code, mutate := bg.running, bg.exitCode, bg.mutate
		bg.mu.Unlock()
		if !running && code != nil && mutate != nil {
			mutate() // the process changed the file on its way out
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/output")
		out := map[string]interface{}{"job_id": id, "running": running,
			"stdout": []string{}, "stderr": []string{}, "elapsed_sec": 1.0,
			"command": "python app.py"}
		if code != nil {
			out["exit_code"] = *code
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasSuffix(r.URL.Path, "/stop"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/stop")
		bg.mu.Lock()
		bg.stopped = append(bg.stopped, id)
		code := bg.exitCode
		bg.mu.Unlock()
		out := map[string]interface{}{"job_id": id, "killed": true,
			"stdout": []string{}, "stderr": []string{}}
		if code != nil {
			out["exit_code"] = *code
		}
		json.NewEncoder(w).Encode(out)
	default:
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}
}

func bgCtx(t *testing.T, dir, url string) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = url
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	return ctx
}

// validSolveSource is a solve.py that parses.
const validSolveSource = "def solve():\n    return 1\n"

func TestLiveBackgroundWorkBlocksCompletion(t *testing.T) {
	dir := t.TempDir()
	bg := &bgSandbox{running: true}
	srv := newBgSandbox(t, dir, bg)
	ctx := bgCtx(t, dir, srv.URL)
	w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": validSolveSource})
	executeToolCall("write_file", w, ctx)
	start, _ := json.Marshal(map[string]string{"command": "python app.py"})
	executeToolCall("run_background", start, ctx)
	if !workspaceHazardous(ctx) {
		t.Fatal("run_background did not raise the hazard")
	}
	st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
	status, reason := finalizeCompletion(ctx, st, "Create solve.py.", "")
	t.Logf("live: status=%q reason=%q tails=%d stops=%v",
		status, reason, bg.tailed, bg.stopped)
	if status.Completed() {
		t.Fatalf("completed with a live background job: reason=%q", reason)
	}
	if reason != "background_work_unresolved" {
		t.Errorf("reason = %q", reason)
	}
	// Nothing was killed to make a decision.
	if len(bg.stopped) != 0 {
		t.Errorf("a live job was stopped during ordinary completion: %v", bg.stopped)
	}
	if len(ctx.BackgroundJobs) != 1 {
		t.Errorf("the live job stopped being tracked")
	}
}

func TestAnExitedUnchangedJobIsReapedAndMayComplete(t *testing.T) {
	zero := 0
	dir := t.TempDir()
	// The job must be LIVE when it starts, or run_background treats it as
	// a job that died at startup and never tracks it.
	bg := &bgSandbox{running: true}
	srv := newBgSandbox(t, dir, bg)
	ctx := bgCtx(t, dir, srv.URL)
	w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": validSolveSource})
	executeToolCall("write_file", w, ctx)
	start, _ := json.Marshal(map[string]string{"command": "python app.py"})
	executeToolCall("run_background", start, ctx)
	bg.mu.Lock()
	bg.running, bg.exitCode = false, &zero // it has since exited
	bg.mu.Unlock()

	t.Logf("jobs tracked before: %v hazard=%v", ctx.BackgroundJobs, workspaceHazardous(ctx))
	st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
	status, reason := finalizeCompletion(ctx, st, "Create solve.py.", "")
	t.Logf("exited: status=%q reason=%q stops=%v tails=%d jobs=%v hazard=%v",
		status, reason, bg.stopped, bg.tailed, ctx.BackgroundJobs, workspaceHazardous(ctx))
	if !status.Completed() {
		t.Fatalf("an exited, reaped job blocked completion: reason=%q", reason)
	}
	if len(bg.stopped) != 1 {
		t.Errorf("the exited job was not reaped: %v", bg.stopped)
	}
	if workspaceHazardous(ctx) {
		t.Error("the hazard survived a confirmed exit")
	}
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if _, s := d.CurrentValidation(); s != ValidationPassed {
		t.Errorf("an untouched file lost its verdict: %v", s)
	}
}

func TestAJobThatChangedTheFileOnExitVoidsTheOldVerdict(t *testing.T) {
	zero := 0
	dir := t.TempDir()
	bg := &bgSandbox{running: true}
	srv := newBgSandbox(t, dir, bg)
	ctx := bgCtx(t, dir, srv.URL)
	w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": validSolveSource})
	executeToolCall("write_file", w, ctx)
	before := ctx.Ledger[ledgerKey(ctx, "solve.py")].CurrentHash
	start, _ := json.Marshal(map[string]string{"command": "python app.py"})
	executeToolCall("run_background", start, ctx)
	// It has since exited, and rewrote the file on its way out.
	bg.mu.Lock()
	bg.running, bg.exitCode = false, &zero
	bg.mutate = func() {
		os.WriteFile(filepath.Join(dir, "solve.py"), []byte("def solve():\n    return [1]]\n"), 0o644)
	}
	bg.mu.Unlock()

	st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
	status, reason := finalizeCompletion(ctx, st, "Create solve.py.", "")
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	_, s := d.CurrentValidation()
	t.Logf("changed-on-exit: status=%q reason=%q hash_moved=%v current=%v",
		status, reason, d.CurrentHash != before, s)
	if d.CurrentHash == before {
		t.Fatal("the rehash did not notice the change")
	}
	if s == ValidationPassed {
		t.Error("a verdict about the old bytes survived")
	}
	if status.Completed() {
		t.Errorf("completed over bytes a background job changed: reason=%q", reason)
	}
}

func TestAnUnconfirmedJobExitStaysBlocking(t *testing.T) {
	dir := t.TempDir()
	bg := &bgSandbox{running: true}
	srv := newBgSandbox(t, dir, bg)
	ctx := bgCtx(t, dir, srv.URL)
	w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": validSolveSource})
	executeToolCall("write_file", w, ctx)
	start, _ := json.Marshal(map[string]string{"command": "python app.py"})
	executeToolCall("run_background", start, ctx)
	// Signalled but never reaped: no exit code to confirm it is gone.
	bg.mu.Lock()
	bg.running, bg.exitCode = false, nil
	bg.mu.Unlock()
	st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
	status, reason := finalizeCompletion(ctx, st, "Create solve.py.", "")
	t.Logf("unconfirmed: status=%q reason=%q", status, reason)
	if status.Completed() {
		t.Errorf("an unconfirmed exit completed: reason=%q", reason)
	}
	if reason != "background_work_unresolved" {
		t.Errorf("reason = %q", reason)
	}
}

func TestAnUnobservableJobStaysBlocking(t *testing.T) {
	dir := t.TempDir()
	ctx := bgCtx(t, dir, "http://127.0.0.1:1")
	ctx.BackgroundJobs = map[string]string{"job1": "python app.py"}
	raiseWorkspaceHazard(ctx, "job1")
	st := &runState{madeProductiveChange: true}
	status, reason := finalizeCompletion(ctx, st, "Create solve.py.", "")
	if status.Completed() || reason != "background_work_unresolved" {
		t.Errorf("status=%q reason=%q", status, reason)
	}
}

func TestSessionsDoNotShareHazards(t *testing.T) {
	dir := t.TempDir()
	bg := &bgSandbox{running: true}
	srv := newBgSandbox(t, dir, bg)
	busy := bgCtx(t, dir, srv.URL)
	start, _ := json.Marshal(map[string]string{"command": "python app.py"})
	executeToolCall("run_background", start, busy)
	if !workspaceHazardous(busy) {
		t.Fatal("no hazard on the busy session")
	}
	other := bgCtx(t, t.TempDir(), srv.URL)
	if workspaceHazardous(other) {
		t.Error("a second session inherited the hazard")
	}
	if live := settleBackgroundHazard(other); len(live) != 0 {
		t.Errorf("a session with no jobs of its own saw %v", live)
	}
}

func TestForegroundOnlyWorkIsUnchangedByTheHazard(t *testing.T) {
	dir := t.TempDir()
	bg := &bgSandbox{}
	srv := newBgSandbox(t, dir, bg)
	ctx := bgCtx(t, dir, srv.URL)
	w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": validSolveSource})
	executeToolCall("write_file", w, ctx)
	st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
	status, reason := finalizeCompletion(ctx, st, "Create solve.py.", "")
	if !status.Completed() {
		t.Errorf("a run with no background work stopped completing: reason=%q", reason)
	}
	if bg.tailed != 0 {
		t.Errorf("the sandbox was polled for jobs that do not exist (%d)", bg.tailed)
	}
}

// The hazard counter and the job map must never disagree about whether work is
// outstanding. Both are mutated only from the agent-loop goroutine -- the one
// `go func` in the loop is the prompt-progress poller and touches neither --
// so this exercises the production sequence rather than manufacturing a
// concurrency pattern production does not have. Run under -race.
func TestHazardAndJobReapingStayConsistent(t *testing.T) {
	zero := 0
	dir := t.TempDir()
	bg := &bgSandbox{running: true}
	srv := newBgSandbox(t, dir, bg)
	ctx := bgCtx(t, dir, srv.URL)
	w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": "def s():\n    return 1\n"})
	executeToolCall("write_file", w, ctx)

	start, _ := json.Marshal(map[string]string{"command": "python app.py"})
	for i := 0; i < 3; i++ {
		executeToolCall("run_background", start, ctx)
	}
	st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}

	// While anything is live, the two views agree that work is outstanding.
	for i := 0; i < 3; i++ {
		live := settleBackgroundHazard(ctx)
		if len(live) == 0 || !workspaceHazardous(ctx) {
			t.Fatalf("live=%v hazardous=%v — the two views disagree", live, workspaceHazardous(ctx))
		}
		if status, _ := finalizeCompletion(ctx, st, "Create solve.py.", ""); status.Completed() {
			t.Fatal("completed while a job was live")
		}
	}
	// Once the job is confirmed gone, both views agree it is settled, and
	// repeating the settle does not double-count.
	bg.mu.Lock()
	bg.running, bg.exitCode = false, &zero
	bg.mu.Unlock()
	for i := 0; i < 3; i++ {
		if live := settleBackgroundHazard(ctx); len(live) != 0 {
			t.Errorf("settle %d still reports %v", i, live)
		}
	}
	if workspaceHazardous(ctx) {
		t.Error("the hazard outlived every confirmed exit")
	}
	if len(ctx.BackgroundJobs) != 0 {
		t.Errorf("%d jobs still tracked", len(ctx.BackgroundJobs))
	}
	if status, reason := finalizeCompletion(ctx, st, "Create solve.py.", ""); !status.Completed() {
		t.Errorf("a fully reaped session did not complete: reason=%q", reason)
	}
}

// --- Background hazard lifecycle ---------------------------------------------
//
// A hazard belongs to a job identity, so only reaping that job lowers it. A
// start that certainly dispatched nothing raises no hazard; otherwise a
// session could never finish.

// bgStartSandbox scripts the job endpoints deterministically: `startFails`
// makes /jobs/start error, `startRunning` decides what the settle-window tail
// reports, and `jobID` lets a stub return a duplicate id on purpose.
type bgStartSandbox struct {
	mu           sync.Mutex
	startFails   bool
	startRunning bool
	jobID        string
	started      int
	stopped      []string
	mutate       func()
}

func newBgStartSandbox(t *testing.T, dir string, bg *bgStartSandbox) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"valid": !strings.Contains(in.Code, "]]")})
		case strings.HasSuffix(r.URL.Path, "/jobs/start"):
			bg.mu.Lock()
			fails, id := bg.startFails, bg.jobID
			bg.started++
			if id == "" {
				id = fmt.Sprintf("job%d", bg.started)
			}
			bg.mu.Unlock()
			if fails {
				http.Error(w, "no slots", http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"job_id": id, "pid": 4242})
		case strings.Contains(r.URL.Path, "/output"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/output")
			bg.mu.Lock()
			running, mutate := bg.startRunning, bg.mutate
			bg.mu.Unlock()
			if !running && mutate != nil {
				mutate()
			}
			out := map[string]interface{}{"job_id": id, "running": running,
				"stdout": []string{}, "stderr": []string{}, "elapsed_sec": 0.1,
				"command": "python app.py"}
			if !running {
				zero := 0
				out["exit_code"] = zero
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/stop"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/stop")
			bg.mu.Lock()
			bg.stopped = append(bg.stopped, id)
			bg.mu.Unlock()
			zero := 0
			json.NewEncoder(w).Encode(map[string]interface{}{"job_id": id,
				"killed": true, "exit_code": zero, "stdout": []string{}, "stderr": []string{}})
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func bgStartCtx(t *testing.T, dir, url string) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = url
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.StreamFn = func(string, interface{}) {}
	return ctx
}

// seedValidSolve writes validSolveSource to solve.py through write_file, and
// stops the test unless the write validated.
func seedValidSolve(t *testing.T, ctx *AgentContext) {
	t.Helper()
	w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": validSolveSource})
	if res := executeToolCall("write_file", w, ctx); res.ValidationStatus != ValidationPassed {
		t.Fatalf("seed did not validate: %s", res.ValidationStatus)
	}
}

// startPythonApp starts `python app.py` through run_background.
func startPythonApp(ctx *AgentContext) *ToolResult {
	args, _ := json.Marshal(map[string]string{"command": "python app.py"})
	return executeToolCall("run_background", args, ctx)
}

// solveRunState is a run that made a productive change and owes solve.py.
func solveRunState() *runState {
	return &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
}

// Definitely no job: a refusal the tool makes before dispatch.
func TestADefinitiveFailedStartLeavesNoHazard(t *testing.T) {
	dir := t.TempDir()
	bg := &bgStartSandbox{}
	srv := newBgStartSandbox(t, dir, bg)
	ctx := bgStartCtx(t, dir, srv.URL)
	seedValidSolve(t, ctx)
	// An empty command is refused locally: nothing is dispatched.
	args, _ := json.Marshal(map[string]string{"command": "   "})
	if res := executeToolCall("run_background", args, ctx); res.Success {
		t.Fatal("an empty command was accepted")
	}
	if workspaceHazardous(ctx) {
		t.Fatal("a refusal that never dispatched left a hazard")
	}
	if len(ctx.BackgroundJobs) != 0 {
		t.Error("a job was invented")
	}
	if len(bg.stopped) != 0 {
		t.Error("something was reaped")
	}
	status, reason := finalizeCompletion(ctx, solveRunState(), "Create solve.py.", "")
	if !status.Completed() {
		t.Errorf("a valid deliverable could not complete: reason=%q", reason)
	}
}

// A failed attempt beside a live job changes nothing about the live one.
func TestAFailedStartBesideALiveJobNeitherAddsNorRemoves(t *testing.T) {
	dir := t.TempDir()
	bg := &bgStartSandbox{startRunning: true}
	srv := newBgStartSandbox(t, dir, bg)
	ctx := bgStartCtx(t, dir, srv.URL)
	seedValidSolve(t, ctx)
	startPythonApp(ctx) // job1, live
	if len(ctx.WorkspaceHazards) != 1 {
		t.Fatalf("hazards after one live start: %v", ctx.WorkspaceHazards)
	}
	args, _ := json.Marshal(map[string]string{"command": ""})
	executeToolCall("run_background", args, ctx)
	if len(ctx.WorkspaceHazards) != 1 {
		t.Errorf("a refused attempt changed the hazard set: %v", ctx.WorkspaceHazards)
	}
	if status, reason := finalizeCompletion(ctx, solveRunState(), "Create solve.py.", ""); status.Completed() {
		t.Fatalf("completed with job1 live: reason=%q", reason)
	}
	// job1 exits; nothing else is outstanding.
	bg.mu.Lock()
	bg.startRunning = false
	bg.mu.Unlock()
	if status, reason := finalizeCompletion(ctx, solveRunState(), "Create solve.py.", ""); !status.Completed() {
		t.Errorf("after job1 exited and settled: reason=%q", reason)
	}
}

// Dispatched and already gone by the settle window.
func TestAnImmediateExitWithoutMutationSettlesAtOnce(t *testing.T) {
	dir := t.TempDir()
	bg := &bgStartSandbox{startRunning: false}
	srv := newBgStartSandbox(t, dir, bg)
	ctx := bgStartCtx(t, dir, srv.URL)
	seedValidSolve(t, ctx)
	before := ctx.Ledger[ledgerKey(ctx, "solve.py")].CurrentHash
	startPythonApp(ctx)
	if workspaceHazardous(ctx) {
		t.Fatalf("an already-exited job left a lasting hazard: %v", ctx.WorkspaceHazards)
	}
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d.CurrentHash != before {
		t.Error("an untouched file was re-recorded")
	}
	if _, s := d.CurrentValidation(); s != ValidationPassed {
		t.Errorf("an untouched file lost its verdict: %v", s)
	}
	if status, reason := finalizeCompletion(ctx, solveRunState(), "Create solve.py.", ""); !status.Completed() {
		t.Errorf("could not complete after an immediate exit: reason=%q", reason)
	}
}

// Dispatched and already gone by the settle window, but it changed the file on
// its way out.
func TestAnImmediateExitAfterMutationInvalidates(t *testing.T) {
	dir := t.TempDir()
	bg := &bgStartSandbox{startRunning: false}
	srv := newBgStartSandbox(t, dir, bg)
	ctx := bgStartCtx(t, dir, srv.URL)
	seedValidSolve(t, ctx)
	before := ctx.Ledger[ledgerKey(ctx, "solve.py")].CurrentHash
	bg.mu.Lock()
	bg.mutate = func() {
		os.WriteFile(filepath.Join(dir, "solve.py"), []byte("def solve():\n    return [1]]\n"), 0o644)
	}
	bg.mu.Unlock()
	startPythonApp(ctx)
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d.CurrentHash == before {
		t.Fatal("the rehash did not notice the change")
	}
	if _, s := d.CurrentValidation(); s == ValidationPassed {
		t.Error("a verdict about the old bytes survived")
	}
	if status, reason := finalizeCompletion(ctx, solveRunState(), "Create solve.py.", ""); status.Completed() {
		t.Errorf("completed over changed bytes: reason=%q", reason)
	}
}

// Two live jobs, two owners.
func TestMultipleBackgroundJobsAreIndependentlyOwned(t *testing.T) {
	dir := t.TempDir()
	bg := &bgStartSandbox{startRunning: true}
	srv := newBgStartSandbox(t, dir, bg)
	ctx := bgStartCtx(t, dir, srv.URL)
	seedValidSolve(t, ctx)
	startPythonApp(ctx)
	startPythonApp(ctx)
	if len(ctx.WorkspaceHazards) != 2 {
		t.Fatalf("hazards: %v", ctx.WorkspaceHazards)
	}
	stop, _ := json.Marshal(map[string]string{"job_id": "job1"})
	executeToolCall("stop_background", stop, ctx)
	if len(ctx.WorkspaceHazards) != 1 {
		t.Errorf("reaping one cleared %v", ctx.WorkspaceHazards)
	}
	if !workspaceHazardous(ctx) {
		t.Error("the second job stopped blocking")
	}
	// Idempotent.
	executeToolCall("stop_background", stop, ctx)
	if len(ctx.WorkspaceHazards) != 1 {
		t.Errorf("a repeated reap changed the set: %v", ctx.WorkspaceHazards)
	}
	stop2, _ := json.Marshal(map[string]string{"job_id": "job2"})
	executeToolCall("stop_background", stop2, ctx)
	if workspaceHazardous(ctx) {
		t.Errorf("hazards after reaping both: %v", ctx.WorkspaceHazards)
	}
}

// A duplicate id is one job, however often it is seen.
func TestADuplicateJobIDCannotDoubleRaise(t *testing.T) {
	dir := t.TempDir()
	bg := &bgStartSandbox{startRunning: true, jobID: "same"}
	srv := newBgStartSandbox(t, dir, bg)
	ctx := bgStartCtx(t, dir, srv.URL)
	seedValidSolve(t, ctx)
	startPythonApp(ctx)
	startPythonApp(ctx)
	startPythonApp(ctx)
	if len(ctx.WorkspaceHazards) != 1 {
		t.Fatalf("a duplicate id raised %v", ctx.WorkspaceHazards)
	}
	stop, _ := json.Marshal(map[string]string{"job_id": "same"})
	executeToolCall("stop_background", stop, ctx)
	if workspaceHazardous(ctx) {
		t.Errorf("one reap did not settle the duplicate: %v", ctx.WorkspaceHazards)
	}
}

// Dispatch may have happened and cannot be named.
func TestAnAmbiguousDispatchFailsClosed(t *testing.T) {
	dir := t.TempDir()
	bg := &bgStartSandbox{startFails: true}
	srv := newBgStartSandbox(t, dir, bg)
	ctx := bgStartCtx(t, dir, srv.URL)
	seedValidSolve(t, ctx)
	if res := startPythonApp(ctx); res.Success {
		t.Fatal("the start unexpectedly succeeded")
	}
	if !workspaceHazardous(ctx) {
		t.Fatal("a possibly-dispatched start left no hazard")
	}
	if !ctx.WorkspaceHazards[hazardUnidentifiedJob] {
		t.Errorf("the hazard is not the unidentified one: %v", ctx.WorkspaceHazards)
	}
	// Nothing can reap it, and it keeps blocking.
	reapSessionBackgroundJobs(ctx)
	settleBackgroundHazard(ctx)
	if !workspaceHazardous(ctx) {
		t.Error("reaping nothing cleared an unidentified job")
	}
	status, reason := finalizeCompletion(ctx, solveRunState(), "Create solve.py.", "")
	if status.Completed() {
		t.Fatal("completed with a possibly-live unidentified process")
	}
	if reason != "background_work_unresolved" {
		t.Errorf("reason = %q", reason)
	}
}

// Sessions that never start anything do no job work at all.
func TestNoBackgroundWorkMeansNoJobTraffic(t *testing.T) {
	dir := t.TempDir()
	bg := &bgStartSandbox{}
	srv := newBgStartSandbox(t, dir, bg)
	ctx := bgStartCtx(t, dir, srv.URL)
	seedValidSolve(t, ctx)
	status, reason := finalizeCompletion(ctx, solveRunState(), "Create solve.py.", "")
	if !status.Completed() {
		t.Errorf("reason=%q", reason)
	}
	bg.mu.Lock()
	defer bg.mu.Unlock()
	if bg.started != 0 || len(bg.stopped) != 0 {
		t.Errorf("job endpoints were touched: started=%d stopped=%v", bg.started, bg.stopped)
	}
}
