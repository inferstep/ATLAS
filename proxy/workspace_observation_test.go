package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// G-tool-parity#1: what a shell command wrote, changed or removed never
// reached the ledger, so a broken module written with a heredoc, or the
// removal of a user's file, ended completed.

// shellWorld is a workspace with the files a user had, a session that
// started over it, and run_command executing on this host.
func shellWorld(t *testing.T, userFiles map[string]string) (*AgentContext, string) {
	t.Helper()
	ctx, dir := sepCtx(t, nil)
	for name, body := range userFiles {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := fakeSyntaxSandbox(t, "]]")
	t.Cleanup(srv.Close)
	ctx.SandboxURL = srv.URL
	ctx.VerifyOnHost = true
	ctx.TrustMode = trustFullyTrusted
	ctx.InitialWorkspace = snapshotWorkspace(dir) // as runAgentLoop takes it
	return ctx, dir
}

func shell(t *testing.T, ctx *AgentContext, command string) {
	t.Helper()
	timeout := 20
	args, _ := json.Marshal(RunCommandInput{Command: command, Timeout: &timeout})
	if res := executeToolCall("run_command", args, ctx); res == nil || !res.Success {
		t.Fatalf("%q did not run: %+v", command, res)
	}
}

func TestAModuleTheShellWroteIsJudged(t *testing.T) {
	ctx, dir := shellWorld(t, nil)
	sepWrite(t, ctx, dir, "main.py", "import tool\nprint(tool.VALUES)\n")

	shell(t, ctx, "cat > tool.py <<'EOF'\nVALUES = [1, 2]]\nEOF")
	if ok, why := terminalCompletionAllowed(ctx, nil); ok {
		t.Fatalf("a broken module the shell wrote completed: %s", why)
	}

	shell(t, ctx, "printf 'VALUES = [1, 2]\\n' > tool.py")
	if ok, why := terminalCompletionAllowed(ctx, nil); !ok {
		t.Fatalf("the fixed module does not complete: %s", why)
	}
}

func TestAFileTheShellChangedIsRechecked(t *testing.T) {
	ctx, _ := shellWorld(t, map[string]string{"util.py": "X = 1\n"})
	shell(t, ctx, "printf 'Y = [1]]\\n' >> util.py")
	if ok, why := terminalCompletionAllowed(ctx, nil); ok {
		t.Fatalf("a user's file the shell broke was not judged: %s", why)
	}
}

func TestRemovingAUsersFileWithTheShellIsAnUnapprovedDeletion(t *testing.T) {
	ctx, dir := shellWorld(t, map[string]string{"util.py": "X = 1\n"})
	sepWrite(t, ctx, dir, "main.py", "print(1)\n")
	shell(t, ctx, "rm util.py")
	if ok, why := terminalCompletionAllowed(ctx, nil); ok || why != "delete_intent_unestablished" {
		t.Fatalf("terminal = (%v, %s), want the deletion refused", ok, why)
	}
}

func TestWhatTheRunMadeAndInstallsDoNotBlock(t *testing.T) {
	ctx, dir := shellWorld(t, map[string]string{"notes.csv": "a,b\n"})
	sepWrite(t, ctx, dir, "main.py", "print(1)\n")
	shell(t, ctx, "mkdir -p build node_modules/x && echo obj > build/out.o && echo 'module.exports=1' > node_modules/x/i.js && "+
		"echo 'print(2)' > scratch.py && echo log > run.log && printf 'c,d\\n' >> notes.csv")
	if !ledgerTracks(ctx, "scratch.py") {
		t.Fatal("a module the shell wrote did not become the session's")
	}
	// Removed by a later command: the run made it, so the run may remove it.
	shell(t, ctx, "rm scratch.py")
	if ok, why := terminalCompletionAllowed(ctx, nil); !ok {
		t.Fatalf("installs, build output, a log and a file the run made and removed blocked completion: %s", why)
	}
	for _, name := range []string{"build/out.o", "node_modules/x/i.js", "run.log", "notes.csv", "scratch.py"} {
		if ledgerTracks(ctx, name) {
			t.Errorf("%s entered the ledger as a deliverable", name)
		}
	}
}

// A walk that hit its cap cannot vouch for what the shell did: the run may
// finish, and says so.
func TestAnUnobservedShellEffectIsACaveat(t *testing.T) {
	ctx, dir := sepCtx(t, nil)
	withSyntaxSandbox(t, ctx)
	sepWrite(t, ctx, dir, "app.py", "print(1)\n")
	full := snapshotWorkspace(dir)
	partial := full
	partial.truncated = true
	applyShellChanges(ctx, full, partial, "run_command")
	if !ctx.ShellEffectsUnobserved {
		t.Fatal("a truncated walk was taken as a complete one")
	}
	st := &runState{madeProductiveChange: true, productiveChanges: 1, toolsRun: 1}
	st.observeVerification(ctx, "", 1, "python3 app.py", ranClean("1\n"))
	if gate, _ := st.exitGates(ctx, "Write app.py.", "Finished."); gate != "" {
		t.Fatalf("bounced by %s", gate)
	}
	status, reason := finalizeCompletion(ctx, st, "Write app.py.", "")
	summary := honestTerminalSummary(ctx, st, status, reason, "Finished.")
	if status != TerminalCompleted || !strings.Contains(summary, "too large to observe") {
		t.Fatalf("terminal = %s/%s summary %q", status, reason, summary)
	}
}

// A background job writes on its own schedule, so no one call brackets what
// it does. What it created or removed reaches the ledger once it can no
// longer be writing.

// bgShellWorld is a workspace with the user's files and a session that has
// started one background job, live, against bgSandbox's job endpoints.
func bgShellWorld(t *testing.T, userFiles map[string]string) (*AgentContext, string, *bgSandbox) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range userFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bg := &bgSandbox{running: true}
	ctx := bgCtx(t, dir, newBgSandbox(t, dir, bg).URL)
	ctx.InitialWorkspace = snapshotWorkspace(dir) // as runAgentLoop takes it
	start, _ := json.Marshal(map[string]string{"command": "python worker.py"})
	executeToolCall("run_background", start, ctx)
	if !workspaceHazardous(ctx) {
		t.Fatal("run_background did not leave a live job")
	}
	return ctx, dir, bg
}

// exit ends the job. onExit runs when the sandbox is next asked about it,
// standing in for what the process did on its way out.
func (bg *bgSandbox) exit(onExit func()) {
	zero := 0
	bg.mu.Lock()
	bg.running, bg.exitCode, bg.mutate = false, &zero, onExit
	bg.mu.Unlock()
}

func TestAModuleABackgroundJobWroteIsJudged(t *testing.T) {
	for _, tc := range []struct {
		body      string
		completes bool
	}{{"VALUES = [1, 2]]\n", false}, {"VALUES = [1, 2]\n", true}} {
		ctx, dir, bg := bgShellWorld(t, nil)
		w, _ := json.Marshal(map[string]string{"path": "main.py", "content": "import gen\nprint(gen.VALUES)\n"})
		executeToolCall("write_file", w, ctx)
		bg.exit(func() { os.WriteFile(filepath.Join(dir, "gen.py"), []byte(tc.body), 0o644) })

		st := &runState{madeProductiveChange: true, expectedOutputs: []string{"main.py"}}
		status, reason := finalizeCompletion(ctx, st, "Create main.py.", "")
		if !ledgerTracks(ctx, "gen.py") {
			t.Fatalf("%q: the module the job wrote never reached the ledger", tc.body)
		}
		if status.Completed() != tc.completes {
			t.Errorf("%q: terminal = %s/%s", tc.body, status, reason)
		}
	}
}

func TestABackgroundJobRemovingAUsersFileIsAnUnapprovedDeletion(t *testing.T) {
	ctx, dir, bg := bgShellWorld(t, map[string]string{"util.py": "X = 1\n"})
	w, _ := json.Marshal(map[string]string{"path": "main.py", "content": "print(1)\n"})
	executeToolCall("write_file", w, ctx)
	bg.exit(func() { os.Remove(filepath.Join(dir, "util.py")) })

	st := &runState{madeProductiveChange: true, expectedOutputs: []string{"main.py"}}
	status, reason := finalizeCompletion(ctx, st, "Create main.py.", "")
	if status.Completed() || reason != "delete_intent_unestablished" {
		t.Fatalf("terminal = %s/%s, want the deletion refused", status, reason)
	}
}

// The comparison spans every call made while the job ran. A deletion one of
// them already recorded is not the job's.
func TestADeletionTheLedgerRecordedIsNotChargedToTheJob(t *testing.T) {
	ctx, _, bg := bgShellWorld(t, map[string]string{"old.py": "X = 1\n"})
	del, _ := json.Marshal(map[string]string{"path": "old.py"})
	executeToolCall("delete_file", del, ctx)
	d := ctx.Ledger[ledgerKey(ctx, "old.py")]
	if d == nil || !d.Tombstoned {
		t.Fatalf("delete_file left no tombstone: %+v", d)
	}
	was := d.TombstoneReason
	bg.exit(nil)
	settleBackgroundHazard(ctx)
	if got := ctx.Ledger[ledgerKey(ctx, "old.py")].TombstoneReason; got != was {
		t.Fatalf("tombstone reason %q became %q", was, got)
	}
}

func TestWhatAStoppedJobWroteIsJudged(t *testing.T) {
	ctx, dir, bg := bgShellWorld(t, nil)
	// Written while it ran, before stop_background was called.
	os.WriteFile(filepath.Join(dir, "gen.py"), []byte("VALUES = [1]]\n"), 0o644)
	bg.exit(nil)
	stop, _ := json.Marshal(map[string]string{"job_id": "job1"})
	executeToolCall("stop_background", stop, ctx)
	if workspaceHazardous(ctx) {
		t.Fatal("a confirmed exit left the hazard up")
	}
	if !ledgerTracks(ctx, "gen.py") {
		t.Fatal("what the stopped job wrote never reached the ledger")
	}
}

func TestWhatAReapedJobLeftIsRecorded(t *testing.T) {
	ctx, dir, bg := bgShellWorld(t, map[string]string{"util.py": "X = 1\n"})
	os.Remove(filepath.Join(dir, "util.py"))
	bg.exit(nil)
	reapSessionBackgroundJobs(ctx) // the deadline and cancel paths
	if d := ctx.Ledger[ledgerKey(ctx, "util.py")]; d == nil || d.TombstoneReason != "deleted:shell" {
		t.Fatalf("the user's file the job removed is not recorded: %+v", d)
	}
}
