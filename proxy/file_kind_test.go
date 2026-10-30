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
	"syscall"
	"testing"
	"time"
)

// Issue #365. A file that is not a regular file, opened for reading, waits
// until some program opens it for writing — and in a workspace where nothing
// ever does, the wait is forever. The tool call never returns, the turn never
// ends, and a stop then waits for that turn. On dev, read_file on a named pipe
// held a whole session.
//
// The fix is in file_kind.go: the kind of the open handle is judged before
// anything is read, and the open itself is non-blocking. This is the permanent
// test the issue asks for, in its three groups. Each case puts one special
// file in a workspace, calls one entry point, and fails when nothing comes
// back in the cap — the issue's word for that is a "wait".
//
// The cap is what makes the test a test. Without it a regression does not fail
// this file; it hangs the whole run, and the failure reads as a test timeout
// rather than as the reader that waited. The cleanup on every fixture also
// opens its pipe for writing, so a reader that really is waiting is released
// and cannot leave a goroutine stuck past the case that found it.

// theSpecialFile is the name the special file takes in a workspace where only
// the kind of the file matters and not its name.
const theSpecialFile = "special.py"

// waitCap is how long one call may take before this file calls it a wait. The
// issue measured with a cap of three seconds per call.
const waitCap = 3 * time.Second

// sessionStartCap caps the one case that runs a whole session. A session start
// does real work before it asks the model anything, so it gets more room than
// a single call; the issue's own cap for this measurement was twenty seconds.
const sessionStartCap = 10 * time.Second

// --- fixtures ---------------------------------------------------------------

// makePipe creates a named pipe at dir/name. Every fixture is a pipe and not a
// socket or a device because a pipe is the kind that waits: opening one for
// reading blocks until a writer appears, where opening a socket fails at once.
func makePipe(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make the folder of %s: %v", name, err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("make a named pipe at %s: %v", name, err)
	}
	t.Cleanup(func() { releasePipe(path) })
	return path
}

// releasePipe opens the pipe for writing without blocking, which is what
// releases a reader waiting inside an open. On a pipe with no reader the open
// fails at once, and on a name that is no longer a pipe it is an ordinary
// write — both are the case this cleanup is not for, so both are ignored.
func releasePipe(path string) {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	_ = syscall.Close(fd)
}

// specialWorkspace is a workspace whose only file is the special file.
func specialWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	makePipe(t, dir, theSpecialFile)
	return dir
}

// seedFile writes a regular file into a workspace.
func seedFile(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make the folder of %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// withinCap fails the case when fn has not returned in cap. fn runs on its own
// goroutine so that a reader which waits fails this case instead of holding
// the whole file until the test timeout.
func withinCap(t *testing.T, cap time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(cap):
		t.Fatalf("%s did not return in %v — it is waiting", what, cap)
	}
}

// The fixture is a reproduction or it is nothing. A plain open of the special
// file really does wait, so every case above is asking a live question; and
// the cleanup really does release a reader, so a case that does wait cannot
// leave its goroutine behind for the rest of the run.
func TestTheFixtureReproducesTheWait(t *testing.T) {
	dir := t.TempDir()
	pipe := makePipe(t, dir, theSpecialFile)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		_, _ = os.ReadFile(pipe)
	}()
	select {
	case <-returned:
		t.Fatal("a plain os.ReadFile came back from a named pipe with no writer: " +
			"the fixture does not reproduce the wait, so the cases above prove nothing")
	case <-time.After(waitCap / 3):
	}
	releasePipe(pipe)
	select {
	case <-returned:
	case <-time.After(waitCap):
		t.Fatal("the reader was not released: the cleanup cannot free a stuck goroutine")
	}
}

// --- group 1: every file tool, driven from the tool registry ----------------

// commandTools are the entries of workspacePathFields whose path argument names
// a directory to run something in rather than a file to read. A pipe is not a
// directory, so there is no open of one for them to wait in; they are listed
// here rather than left out, so that a NEW tool is a failure of this file
// until someone decides which of the two it is.
var commandTools = map[string]bool{
	"run_command":    true,
	"run_background": true,
}

// specialFileCalls is one call per file tool, with the tool's own registered
// workspace path arguments set. The fake content is deliberately plausible:
// the point is that the call reaches the read, not that the call is a good one.
func specialFileCalls() map[string]map[string]interface{} {
	return map[string]map[string]interface{}{
		"read_file":       {"path": theSpecialFile},
		"outline_file":    {"path": theSpecialFile},
		"search_files":    {"path": theSpecialFile, "pattern": "def"},
		"write_file":      {"path": theSpecialFile, "content": "x = 1\n"},
		"edit_file":       {"path": theSpecialFile, "old_str": "x = 1", "new_str": "x = 2"},
		"structural_edit": {"path": theSpecialFile, "selector": "function:solve", "content": "def solve():\n    return 1\n"},
		"insert_after":    {"path": theSpecialFile, "line": 0, "content": "y = 2\n"},
		"replace_lines": {"path": theSpecialFile, "start_line": 1, "end_line": 1,
			"expected_first_line": "x = 1", "expected_last_line": "x = 1", "content": "z = 3\n"},
		"move_file":      {"source": theSpecialFile, "destination": "moved.py"},
		"delete_file":    {"path": theSpecialFile},
		"find_file":      {"path": theSpecialFile},
		"list_directory": {"path": theSpecialFile},
	}
}

// fileToolCtx is the context a group-1 call runs in: a real workspace, no
// services, and a session that has read nothing. Every service-backed gate
// fails open without a URL, so a call reaches its own read and stops there.
func fileToolCtx(t *testing.T, dir string) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.PassID = "file-kind-" + filepath.Base(dir)
	return ctx
}

// callFileTool runs one registered tool on one workspace, straight to the
// tool, with no approval step and no agent loop — the issue's own measurement
// shape.
func callFileTool(t *testing.T, name string, dir string) (*ToolResult, error) {
	t.Helper()
	tool := getTool(name)
	if tool == nil {
		t.Fatalf("%s is not a registered tool", name)
	}
	args, err := json.Marshal(specialFileCalls()[name])
	if err != nil {
		t.Fatalf("build the call for %s: %v", name, err)
	}
	ctx := fileToolCtx(t, dir)
	var res *ToolResult
	var execErr error
	withinCap(t, waitCap, name+" on a named pipe", func() {
		res, execErr = tool.Execute(json.RawMessage(args), ctx)
	})
	return res, execErr
}

// The registry is the driver: every tool that takes a workspace path is either
// called here or named as a tool whose path is a directory. A tool added later
// fails this until it is one of the two, which is the point — it is covered on
// the day it is added, or the suite says loudly that it is not.
func TestEveryWorkspacePathToolHasANamedPipeCall(t *testing.T) {
	calls := specialFileCalls()
	for name, fields := range workspacePathFields {
		if commandTools[name] {
			continue
		}
		fixture, ok := calls[name]
		if !ok {
			t.Errorf("%s takes a workspace path (%v) and group 1 has no call for it: add one, "+
				"or name it in commandTools if its path is a directory to run in", name, fields)
			continue
		}
		for _, field := range fields {
			if value, set := fixture[field]; !set || value == "" {
				t.Errorf("the call for %s does not set its registered path argument %q", name, field)
			}
		}
		if getTool(name) == nil {
			t.Errorf("workspacePathFields names %s, which is not a registered tool", name)
		}
	}
	for name := range calls {
		if _, registered := workspacePathFields[name]; !registered {
			t.Errorf("group 1 calls %s, which is not in workspacePathFields: the registry is the driver", name)
		}
	}
}

// One subtest per tool, on its own workspace, because several of these tools
// change the workspace they are given.
func TestFileToolsOnANamedPipeReturn(t *testing.T) {
	for name := range specialFileCalls() {
		t.Run(name, func(t *testing.T) {
			dir := specialWorkspace(t)
			callFileTool(t, name, dir)
		})
	}
}

// The one tool the issue names by its outcome: read_file on a named pipe
// answers at once, and the answer names the kind so the model can act on it
// instead of retrying the same call.
func TestReadFileOnANamedPipeAnswersWithTheKind(t *testing.T) {
	dir := specialWorkspace(t)
	res, err := callFileTool(t, "read_file", dir)
	reason := ""
	switch {
	case err != nil:
		reason = err.Error()
	case res != nil && !res.Success:
		reason = res.Error
	}
	if reason == "" {
		t.Fatalf("read_file on a named pipe reported success: res=%+v err=%v", res, err)
	}
	if !strings.Contains(reason, "not a regular file") {
		t.Errorf("read_file answered %q, want the kind named in it", reason)
	}
	if !strings.Contains(reason, "named pipe") {
		t.Errorf("read_file answered %q, want the pipe named in it", reason)
	}
}

// The other half of the fix. A check that refuses everything would pass every
// case above, so a regular file must still read.
func TestARegularFileStillReads(t *testing.T) {
	dir := t.TempDir()
	seedFile(t, dir, theSpecialFile, "x = 1\n")
	res, err := callFileTool(t, "read_file", dir)
	if err != nil {
		t.Fatalf("read_file on a regular file: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("read_file on a regular file = %+v, want the content", res)
	}
	if !strings.Contains(string(res.Data), "x = 1") {
		t.Errorf("read_file returned %q, want the file's lines", res.Data)
	}
}

// --- group 2: the readers of the session start, by the names they open ------

// samplePlanStartNames are the names the start's first pass takes by name,
// plus the .py its source-file pass reaches. A pipe under any of them is a
// session that never reaches the model, so each name is a case of its own.
var samplePlanStartNames = []string{
	"README.md", "package.json", "go.mod", "pyproject.toml", "requirements.txt", theSpecialFile,
}

func TestSamplePlanContextSkipsANamedPipe(t *testing.T) {
	for _, name := range samplePlanStartNames {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			makePipe(t, dir, name)
			withinCap(t, waitCap, "samplePlanContext with a pipe named "+name, func() {
				if got := samplePlanContext(dir, 10, 4096); len(got) != 0 {
					t.Errorf("samplePlanContext read the pipe: %v", keysOf(got))
				}
			})
		})
	}
}

// The rest of the start's readers, each named by the file it opens, because
// that is what decides whether the reader meets the pipe: a pipe named
// notes.txt starts a session that a pipe named README.md holds.
func TestTheSessionStartSkipsANamedPipe(t *testing.T) {
	for _, tc := range []struct {
		what  string
		name  string
		setup func(t *testing.T, dir string)
		read  func(t *testing.T, dir string)
	}{
		{"walkPythonFiles walks the tree", theSpecialFile, nil, func(t *testing.T, dir string) {
			if got := walkPythonFiles(dir); len(got) != 0 {
				t.Errorf("walkPythonFiles read the pipe: %v", keysOf(got))
			}
		}},
		{"walkPythonFiles reaches a nested .py", filepath.Join("pkg", "special.py"), func(t *testing.T, dir string) {
			seedFile(t, dir, "app.py", "x = 1\n")
		}, func(t *testing.T, dir string) {
			got := walkPythonFiles(dir)
			if _, walkedThePipe := got[filepath.Join("pkg", "special.py")]; walkedThePipe {
				t.Errorf("walkPythonFiles read the pipe: %v", keysOf(got))
			}
			if got["app.py"] != "x = 1\n" {
				t.Errorf("walkPythonFiles skipped the regular file beside the pipe: %v", keysOf(got))
			}
		}},
		{"detectNodeJS reads package.json", "package.json", func(t *testing.T, dir string) {
			// The control, in a folder beside the pipe: a regular package.json
			// of the same name is still parsed. Seeding it over the pipe would
			// be a write to a pipe with no reader, which is the wait again.
			seedFile(t, dir, filepath.Join("control", "package.json"), `{"scripts":{"build":"vite build"}}`)
		}, func(t *testing.T, dir string) {
			files := map[string]bool{"package.json": true}
			parsed := detectNodeJS(dir, files)
			if parsed == nil {
				parsed = &ProjectInfo{}
			}
			if parsed.Framework != "" {
				t.Errorf("detectNodeJS took the framework from the pipe: %+v", parsed)
			}
			if parsed.BuildCommand != "" {
				t.Errorf("detectNodeJS took the build command from the pipe: %+v", parsed)
			}
			if info := detectNodeJS(filepath.Join(dir, "control"), files); info == nil || info.BuildCommand == "" {
				t.Errorf("detectNodeJS skipped a regular package.json: %+v", info)
			}
		}},
		{"detectPython reads requirements.txt", "requirements.txt", func(t *testing.T, dir string) {
			seedFile(t, dir, filepath.Join("control", "requirements.txt"), "flask==3.0.0\n")
		}, func(t *testing.T, dir string) {
			files := map[string]bool{"requirements.txt": true}
			if info := detectPython(dir, files); info != nil && info.Framework != "" {
				t.Errorf("detectPython parsed the pipe: %+v", info)
			}
			if info := detectPython(filepath.Join(dir, "control"), files); info == nil || info.Framework != "flask" {
				t.Errorf("detectPython skipped a regular requirements.txt: %+v", info)
			}
		}},
		{"resolveVerifyTarget reads .atlas/config.toml", filepath.Join(".atlas", "config.toml"), func(t *testing.T, dir string) {
			t.Setenv("ATLAS_VERIFY_IN", "")
		}, func(t *testing.T, dir string) {
			if got := resolveVerifyTarget(dir); got != "sandbox" {
				t.Errorf("resolveVerifyTarget = %q, want the default: the pipe named a target", got)
			}
		}},
		{"probeASAStatus reads the marker beside the vector", "vector.gguf.model", func(t *testing.T, dir string) {
			seedFile(t, dir, "vector.gguf", "weights")
			t.Setenv("ATLAS_CONTROL_VECTOR", filepath.Join(dir, "vector.gguf"))
		}, func(t *testing.T, dir string) {
			if status := probeASAStatus(); strings.Contains(status.Hint, "control vector active") {
				t.Errorf("probeASAStatus read the pipe: %+v", status)
			}
		}},
		{"probeASAStatus reads the marker inside the workspace", filepath.Join("models", "vector.gguf.model"), func(t *testing.T, dir string) {
			seedFile(t, dir, filepath.Join("models", "vector.gguf"), "weights")
			t.Setenv("ATLAS_CONTROL_VECTOR", "/models/vector.gguf")
			t.Setenv("ATLAS_WORKSPACE_DIR", dir)
		}, func(t *testing.T, dir string) {
			if status := probeASAStatus(); strings.Contains(status.Hint, "control vector active") {
				t.Errorf("probeASAStatus read the pipe: %+v", status)
			}
		}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			dir := t.TempDir()
			makePipe(t, dir, tc.name)
			// Writing a file and setting a variable fail the test the wrong
			// way from inside the goroutine the cap is watching.
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			withinCap(t, waitCap, tc.what, func() { tc.read(t, dir) })
		})
	}
}

// A start reader that refused everything would pass the case above, so the
// control: the same reader with a regular file of the same name reads it.
func TestTheSessionStartStillReadsARegularFile(t *testing.T) {
	dir := t.TempDir()
	seedFile(t, dir, "README.md", "the project")
	seedFile(t, dir, "solve.py", "x = 1\n")
	if got := samplePlanContext(dir, 10, 4096); got["README.md"] != "the project" {
		t.Errorf("samplePlanContext returned %v, want the README's text", keysOf(got))
	}
	if got := walkPythonFiles(dir); got["solve.py"] != "x = 1\n" {
		t.Errorf("walkPythonFiles returned %v, want solve.py's text", keysOf(got))
	}
}

// The whole session, not just the reader: a workspace holding a pipe under a
// name the start reads must still reach the model. The event watched for is
// the proxy's first model request, which is the issue's own definition of a
// session that starts.
func TestASessionStartsInAWorkspaceHoldingANamedPipe(t *testing.T) {
	dir := t.TempDir()
	makePipe(t, dir, "README.md")
	makePipe(t, dir, "special.py")

	asked := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasPrefix(req.URL.Path, "/slots/"):
			http.NotFound(w, req) // this model server has no slots to erase
			return
		case !strings.HasSuffix(req.URL.Path, "/v1/chat/completions"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		io.Copy(io.Discard, req.Body)
		select {
		case asked <- struct{}{}:
		default:
		}
		call, _ := json.Marshal(map[string]interface{}{
			"type": "done", "summary": "nothing to do"})
		delta, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
	}))
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.MaxTurns = 1
	ctx.PassID = "start-" + filepath.Base(dir)
	reqCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx.Ctx = reqCtx

	ended := make(chan struct{})
	go func() {
		defer close(ended)
		runAgentLoop(ctx, "say hello")
	}()
	select {
	case <-asked:
	case <-time.After(sessionStartCap):
		t.Fatal("no model request arrived: the session start is waiting in a named pipe")
	case <-ended:
		t.Fatal("the run ended without asking the model anything")
	}
	select {
	case <-ended:
	case <-time.After(sessionStartCap):
		t.Error("the session did not end after the model answered")
	}
}

// --- group 3: the two readers at a gate later in a session -----------------

// A V3 that answers the structural check, so the case exercises the read in
// front of it rather than the connectivity of a service.
func structuralCheckStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"unresolved":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// checkStructuralUnresolved reads the session's other .py files off disk
// through structuralProjectContext, then addSessionWrites. Reaching the pipe
// is what this case holds: the call must come back, and the check must still
// have run rather than being skipped for a reason of its own.
func TestCheckStructuralUnresolvedReturnsWhenASessionWriteIsANamedPipe(t *testing.T) {
	dir := specialWorkspace(t)
	srv := structuralCheckStub(t)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.PassID = "gate-" + filepath.Base(dir)
	ctx.V3URL = srv.URL
	// The gate reads the files the session wrote, so the pipe is given as one
	// of them: that is the read this case puts in front of the check.
	ctx.SessionWrites[theSpecialFile] = true

	var introduced []string
	var ok bool
	withinCap(t, waitCap, "checkStructuralUnresolved with a pipe where the session wrote", func() {
		introduced, ok = checkStructuralUnresolved(ctx, filepath.Join(dir, "elsewhere.py"), "def f():\n    return 1\n")
	})
	if len(introduced) != 0 {
		t.Errorf("the gate reported %v introduced, want nothing", introduced)
	}
	if !ok {
		t.Error("checkStructuralUnresolved = false, want the check to have run: the pipe should not stop it")
	}
}

// readOriginalForGate is the read the structural gate's comparison is built
// on. Its contract is fail-open, and a pipe is not a missing file: treating it
// as an empty original would count every pre-existing unresolved call as newly
// introduced and block an edit that is fine.
func TestReadOriginalForGateSkipsANamedPipe(t *testing.T) {
	dir := specialWorkspace(t)
	pipe := filepath.Join(dir, theSpecialFile)
	var body string
	var ok bool
	withinCap(t, waitCap, "readOriginalForGate on a named pipe", func() {
		body, ok = readOriginalForGate(pipe)
	})
	if ok {
		t.Errorf("readOriginalForGate = (%q, true), want the gate skipped: a pipe is not an empty file", body)
	}

	seedFile(t, dir, "regular.py", "x = 1\n")
	body, ok = readOriginalForGate(filepath.Join(dir, "regular.py"))
	if !ok {
		t.Error("readOriginalForGate refused a regular file")
	}
	if body != "x = 1\n" {
		t.Errorf("readOriginalForGate = %q, want its content", body)
	}
	missing, ok := readOriginalForGate(filepath.Join(dir, "absent.py"))
	if !ok {
		t.Error("readOriginalForGate refused a missing file: want the first-write answer")
	}
	if missing != "" {
		t.Errorf("readOriginalForGate = %q for a missing file, want no original", missing)
	}
}

// editViewIsCurrent is the read that made five tools wait on dev only: they
// ask it before the "file not read yet" answer. A pipe there is simply a file
// this session knows nothing about.
func TestEditViewIsCurrentReturnsFalseForANamedPipe(t *testing.T) {
	dir := specialWorkspace(t)
	ctx := fileToolCtx(t, dir)
	path := filepath.Join(dir, theSpecialFile)
	var current bool
	withinCap(t, waitCap, "editViewIsCurrent on a named pipe", func() {
		current = editViewIsCurrent(ctx, path)
	})
	if current {
		t.Error("editViewIsCurrent = true for a named pipe, want false: nothing was read")
	}
}

// readLedgerBytes hashes what a tool wrote. It answers (nil, false) for bytes
// it cannot speak about, and a pipe is one of those: the ledger verdict
// becomes unknown rather than a pass.
func TestReadLedgerBytesSkipsANamedPipe(t *testing.T) {
	dir := specialWorkspace(t)
	var body []byte
	var ok bool
	withinCap(t, waitCap, "readLedgerBytes on a named pipe", func() {
		body, ok = readLedgerBytes(filepath.Join(dir, theSpecialFile))
	})
	if ok {
		t.Error("readLedgerBytes spoke about a named pipe: want the unknown verdict")
	}
	if body != nil {
		t.Errorf("readLedgerBytes returned %q, want no bytes", body)
	}
}

// The asset lint walks the whole project and reads every source file it finds.
// A pipe in the tree is one file it does not read, not a project it cannot
// judge.
func TestAssetLintSkipsANamedPipeAndKeepsGoing(t *testing.T) {
	dir := t.TempDir()
	makePipe(t, dir, theSpecialFile)
	seedFile(t, dir, "app.py", "def f():\n    return 1\n")
	var files []lintFile
	var ok bool
	withinCap(t, waitCap, "assetLintFiles with a named pipe in the tree", func() {
		files, ok = assetLintFiles(dir)
	})
	if !ok {
		t.Fatal("assetLintFiles gave up on the project: a pipe is not a project too large to judge")
	}
	if len(files) != 1 || files[0].rel != "app.py" {
		t.Errorf("assetLintFiles = %+v, want only the regular file", files)
	}
}

// keysOf names the keys of a reader's answer, for a failure message that says
// what was read rather than only that something was.
func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
