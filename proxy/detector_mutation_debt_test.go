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

// A write the model asked for that never landed is mutation debt, and it
// blocks completion until a later action on the same path settles it. Without
// it, a write to a.py that fails before dispatch leaves no trace, and a
// success on b.py completes the run.
//
// Debt is not kept in the deliverable ledger: the ledger records what the
// session owns on disk, and an intent that never landed owns nothing.
//
// A "@fenced" write fails before dispatch in termFixture, because the fenced
// sub-call never returns a block. That is how these tests create debt.

const debtGoodBody = "def helper():\n    return 2\n\nprint(helper())\n"

// An unrelated success must not retire a failed intent.
func TestUnrelatedSuccessDoesNotRetireAFailedIntent(t *testing.T) {
	dir := t.TempDir()
	const req = "Write a couple of small scripts."
	ctx, _, census, terminal := termFixture(t, dir, req, termCeiling,
		func(i int, _ string) map[string]interface{} {
			switch i {
			case 0, 1:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "a.py", "content": "@fenced"}}
			case 2:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "b.py", "content": debtGoodBody}}
			case 3:
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "python3 b.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "wrote what I could"}
			}
		})
	if err := runAgentLoop(ctx, req); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	t.Logf("status=%q reason=%q summary=%.140s",
		terminal["status"], terminal["reason"], terminal["summary"])

	if terminal["status"] == string(TerminalCompleted) {
		t.Fatalf("an unrelated success retired the failed intent: reason=%q", terminal["reason"])
	}
	if terminal["reason"] != "unresolved_mutation_debt" {
		t.Errorf("reason = %q, want unresolved_mutation_debt", terminal["reason"])
	}
	if !strings.Contains(terminal["summary"], "a.py") {
		t.Errorf("the summary does not name the unresolved path:\n%s", terminal["summary"])
	}
	for _, leak := range []string{"debt", "ledger", "hash", "canonical"} {
		if strings.Contains(strings.ToLower(terminal["summary"]), leak) {
			t.Errorf("the summary exposes the internal term %q:\n%s", leak, terminal["summary"])
		}
	}
	// b.py really did land and validate: debt is not a blanket block.
	if got, _ := os.ReadFile(filepath.Join(dir, "b.py")); string(got) != debtGoodBody {
		t.Errorf("b.py = %q", got)
	}
	// a.py never entered the deliverable ledger.
	if _, ok := ctx.Ledger[ledgerKey(ctx, "a.py")]; ok {
		t.Error("a path that never landed entered the deliverable ledger")
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result balance: %d vs %d", census["tool_call"], census["tool_result"])
	}
}

// A validated success on the same path clears the debt, also through an alias.
func TestSamePathValidatedSuccessClearsExactlyOneDebt(t *testing.T) {
	for _, c := range []struct{ name, failPath, fixPath string }{
		{"same spelling", "a.py", "a.py"},
		{"alias spelling", "./a.py", "a.py"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			const req = "Write a small script."
			ctx, _, _, terminal := termFixture(t, dir, req, termCeiling,
				func(i int, _ string) map[string]interface{} {
					switch i {
					case 0, 1:
						return map[string]interface{}{"type": "tool_call", "name": "write_file",
							"args": map[string]string{"path": c.failPath, "content": "@fenced"}}
					case 2:
						return map[string]interface{}{"type": "tool_call", "name": "write_file",
							"args": map[string]string{"path": c.fixPath, "content": debtGoodBody}}
					case 3:
						return map[string]interface{}{"type": "tool_call", "name": "run_command",
							"args": map[string]string{"command": "python3 " + c.fixPath}}
					default:
						return map[string]interface{}{"type": "done", "summary": "wrote it"}
					}
				})
			if err := runAgentLoop(ctx, req); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: status=%q reason=%q", c.name, terminal["status"], terminal["reason"])
			if terminal["status"] != string(TerminalCompleted) {
				t.Errorf("a resolved path did not complete: status=%q reason=%q",
					terminal["status"], terminal["reason"])
			}
		})
	}
}

// Bytes on disk are not enough: the validation must be current and passed.
func TestUnvalidatedBytesDoNotClearDebt(t *testing.T) {
	dir := t.TempDir()
	const req = "Write a small script."
	const broken = "def solve():\n    return [1, 2]]\n"
	ctx, _, _, terminal := termFixture(t, dir, req, termCeiling,
		func(i int, _ string) map[string]interface{} {
			switch i {
			case 0, 1:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "a.py", "content": "@fenced"}}
			case 2:
				// Lands with a parse warning: applied, but validation failed.
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "a.py", "content": broken}}
			default:
				return map[string]interface{}{"type": "done", "summary": "wrote it"}
			}
		})
	if err := runAgentLoop(ctx, req); err != nil {
		t.Fatal(err)
	}
	t.Logf("unvalidated: status=%q reason=%q", terminal["status"], terminal["reason"])
	if terminal["status"] == string(TerminalCompleted) {
		t.Errorf("bytes that failed validation cleared the debt")
	}
}

// A file with no applicable checker settles its debt on not_applicable.
func TestNonCodeNotApplicableClearsDebt(t *testing.T) {
	dir := t.TempDir()
	const req = "Write the notes file."
	ctx, _, _, terminal := termFixture(t, dir, req, termCeiling,
		func(i int, _ string) map[string]interface{} {
			switch i {
			case 0, 1:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "notes.txt", "content": "@fenced"}}
			case 2:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "notes.txt", "content": "hello\n"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "wrote the notes"}
			}
		})
	if err := runAgentLoop(ctx, req); err != nil {
		t.Fatal(err)
	}
	t.Logf("not_applicable: status=%q reason=%q", terminal["status"], terminal["reason"])
	if terminal["reason"] == "unresolved_mutation_debt" {
		t.Errorf("a text file with no applicable checker did not settle its debt: %s", terminal["summary"])
	}
}

// Unsafe or malformed attempts create no debt and no ledger entry; they are
// still covered by the unmet-action evidence.
func TestUnsafeAttemptsCreateNoPathDebt(t *testing.T) {
	for _, c := range []struct{ name, path, content string }{
		{"deny-listed", ".env", "SECRET=2\n"},
		{"path escape", "../outside.py", "x = 1\n"},
		{"blank path", "   ", "x = 1\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			const req = "Write the config and a script."
			ctx, _, _, terminal := termFixture(t, dir, req, termCeiling,
				func(i int, _ string) map[string]interface{} {
					switch i {
					case 0:
						return map[string]interface{}{"type": "tool_call", "name": "write_file",
							"args": map[string]string{"path": c.path, "content": c.content}}
					case 1:
						return map[string]interface{}{"type": "tool_call", "name": "write_file",
							"args": map[string]string{"path": "ok.py", "content": debtGoodBody}}
					case 2:
						return map[string]interface{}{"type": "tool_call", "name": "run_command",
							"args": map[string]string{"command": "python3 ok.py"}}
					default:
						return map[string]interface{}{"type": "done", "summary": "did the work"}
					}
				})
			if err := runAgentLoop(ctx, req); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: status=%q reason=%q", c.name, terminal["status"], terminal["reason"])
			// The unsafe path must not be the reason, and must not appear as
			// an unresolved deliverable in the summary.
			if terminal["reason"] == "unresolved_mutation_debt" &&
				strings.Contains(terminal["summary"], strings.TrimSpace(c.path)) {
				t.Errorf("an unsafe/invalid path became tracked work:\n%s", terminal["summary"])
			}
			if _, ok := ctx.Ledger[ledgerKey(ctx, c.path)]; ok {
				t.Error("an unsafe path entered the deliverable ledger")
			}
		})
	}
}

// Read-only tasks are untouched.
func TestReadOnlyTaskUnaffectedByDebt(t *testing.T) {
	dir := t.TempDir()
	const q = "What does this project do?"
	ctx, _, _, terminal := termFixture(t, dir, q, termCeiling,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "done", "summary": "It is a small solver."}
		})
	if err := runAgentLoop(ctx, q); err != nil {
		t.Fatal(err)
	}
	t.Logf("read-only: status=%q reason=%q", terminal["status"], terminal["reason"])
	if terminal["status"] != string(TerminalCompleted) {
		t.Errorf("a question stopped completing: status=%q reason=%q",
			terminal["status"], terminal["reason"])
	}
}

// Delete and move debt resolve only on demonstrated absence and
// demonstrated destination bytes, and separate paths stay independent.
func TestDeleteMoveAndIndependenceOfDebt(t *testing.T) {
	t.Run("successful delete resolves on confirmed absence", func(t *testing.T) {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		ctx.PermissionMode, ctx.StreamFn = PermissionYolo, func(string, interface{}) {}
		st := &runState{}
		w, _ := json.Marshal(map[string]string{"path": "gone.py", "content": "A = 1\n"})
		executeToolCall("write_file", w, ctx)
		d, _ := json.Marshal(map[string]string{"path": "gone.py"})
		noteMutationIntent(ctx, st, "delete_file", d)
		if !hasUnresolvedDebt(st) {
			t.Fatal("delete intent created no debt")
		}
		executeToolCall("delete_file", d, ctx)
		settleMutationDebt(ctx, st)
		if hasUnresolvedDebt(st) {
			paths, _ := unresolvedDebtPaths(st, 5)
			t.Errorf("a demonstrated delete did not resolve: %v", paths)
		}
	})

	t.Run("failed delete of a file that is still there stays blocking", func(t *testing.T) {
		dir := t.TempDir()
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.PermissionMode, ctx.StreamFn = PermissionYolo, func(string, interface{}) {}
		st := &runState{}
		sub := filepath.Join(dir, "locked")
		os.MkdirAll(sub, 0o755)
		os.WriteFile(filepath.Join(sub, "keep.py"), []byte("A = 1\n"), 0o644)
		if err := os.Chmod(sub, 0o555); err != nil {
			t.Skip("cannot make a directory read-only here")
		}
		t.Cleanup(func() { os.Chmod(sub, 0o755) })

		d, _ := json.Marshal(map[string]string{"path": "locked/keep.py"})
		noteMutationIntent(ctx, st, "delete_file", d)
		executeToolCall("delete_file", d, ctx) // the file survives
		settleMutationDebt(ctx, st)
		if !hasUnresolvedDebt(st) {
			t.Error("a delete that left the file in place resolved its debt")
		}
	})

	t.Run("abandoning a path that never landed settles it", func(t *testing.T) {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		ctx.PermissionMode, ctx.StreamFn = PermissionYolo, func(string, interface{}) {}
		st := &runState{}
		// The fenced case: content debt on a path that was never produced.
		w, _ := json.Marshal(map[string]string{"path": "ghost.py", "content": "@fenced"})
		noteMutationIntent(ctx, st, "write_file", w)
		d, _ := json.Marshal(map[string]string{"path": "ghost.py"})
		noteMutationIntent(ctx, st, "delete_file", d) // converts to delete debt
		executeToolCall("delete_file", d, ctx)        // fails: it was never there
		settleMutationDebt(ctx, st)
		if hasUnresolvedDebt(st) {
			t.Error("an explicitly abandoned path that is demonstrably absent stayed blocking")
		}
	})

	t.Run("successful move needs source absence and destination bytes", func(t *testing.T) {
		dir := t.TempDir()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/syntax-check") {
				json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
				return
			}
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.SandboxURL = srv.URL
		ctx.PermissionMode, ctx.StreamFn = PermissionYolo, func(string, interface{}) {}
		st := &runState{}
		w, _ := json.Marshal(map[string]string{"path": "old.py", "content": "A = 1\n"})
		executeToolCall("write_file", w, ctx)
		m, _ := json.Marshal(map[string]string{"source": "old.py", "destination": "new.py"})
		noteMutationIntent(ctx, st, "move_file", m)
		if !hasUnresolvedDebt(st) {
			t.Fatal("move intent created no debt")
		}
		executeToolCall("move_file", m, ctx)
		settleMutationDebt(ctx, st)
		// The destination is observed with an unknown verdict by design — a
		// rename earns no evidence under the new name — so the demonstration
		// is read from disk through the same contract every deliverable uses.
		// These bytes pass it, so the move is settled without the model having
		// to rewrite a file it only moved.
		if hasUnresolvedDebt(st) {
			paths, _ := unresolvedDebtPaths(st, 5)
			t.Errorf("a demonstrated move did not resolve: %v", paths)
		}
		// And a destination that cannot be demonstrated leaves it owed.
		bad, _ := json.Marshal(map[string]string{"source": "new.py", "destination": "gone.py"})
		noteMutationIntent(ctx, st, "move_file", bad)
		executeToolCall("move_file", bad, ctx)
		os.Remove(filepath.Join(dir, "gone.py"))
		settleMutationDebt(ctx, st)
		if !hasUnresolvedDebt(st) {
			t.Error("a move whose destination is not there resolved anyway")
		}
	})

	t.Run("separate paths are independent", func(t *testing.T) {
		dir := t.TempDir()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/syntax-check") {
				json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
				return
			}
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.SandboxURL = srv.URL
		ctx.PermissionMode, ctx.StreamFn = PermissionYolo, func(string, interface{}) {}
		st := &runState{}
		for _, p := range []string{"a.py", "b.py"} {
			args, _ := json.Marshal(map[string]string{"path": p, "content": "@fenced"})
			noteMutationIntent(ctx, st, "write_file", args)
		}
		if n := len(st.mutationDebt); n != 2 {
			t.Fatalf("%d debts for two paths", n)
		}
		w, _ := json.Marshal(map[string]string{"path": "a.py", "content": "A = 1\n"})
		executeToolCall("write_file", w, ctx)
		settleMutationDebt(ctx, st)
		paths, _ := unresolvedDebtPaths(st, 5)
		if len(paths) != 1 || paths[0] != "b.py" {
			t.Errorf("unresolved = %v, want only b.py", paths)
		}
	})
}

// The map is bounded, and past the ceiling it fails closed: no naming, still
// blocking.
func TestMutationDebtCeilingFailsClosed(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	st := &runState{}
	for i := 0; i < maxTrackedMutationDebt+25; i++ {
		args, _ := json.Marshal(map[string]string{
			"path": fmt.Sprintf("f%03d.py", i), "content": "@fenced"})
		noteMutationIntent(ctx, st, "write_file", args)
	}
	if len(st.mutationDebt) > maxTrackedMutationDebt {
		t.Errorf("map grew to %d, ceiling is %d", len(st.mutationDebt), maxTrackedMutationDebt)
	}
	if !st.debtOverflow {
		t.Error("the ceiling was reached without recording that it was")
	}
	if !hasUnresolvedDebt(st) {
		t.Fatal("overflow stopped blocking completion")
	}
	paths, more := unresolvedDebtPaths(st, 5)
	if len(paths) != 5 || !more {
		t.Errorf("disclosure = %d paths, more=%v; want a bounded list flagged as partial",
			len(paths), more)
	}
	if status, reason := finalizeCompletion(ctx, st, "Write the files.", ""); status.Completed() {
		t.Errorf("overflow allowed completion: reason=%q", reason)
	}
	if s := unresolvedDebtSummary(st); !strings.Contains(s, "Other files are in the same state") {
		t.Errorf("overflow is not disclosed:\n%s", s)
	}
}

// run_command and run_background keep their unobserved semantics and must not
// be approximated as one-path debt.
func TestCommandToolsCreateNoPathDebt(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	st := &runState{}
	for _, c := range []struct{ name, args string }{
		{"run_command", `{"command":"rm -rf build"}`},
		{"run_background", `{"command":"python app.py"}`},
		{"read_file", `{"path":"a.py"}`},
		{"search_files", `{"pattern":"def "}`},
	} {
		noteMutationIntent(ctx, st, c.name, json.RawMessage(c.args))
	}
	if hasUnresolvedDebt(st) {
		paths, _ := unresolvedDebtPaths(st, 5)
		t.Errorf("non-path-targeted tools created debt: %v", paths)
	}
}

// --- Bounded recovery for unresolved work -----------------------------------

const debtRecoveryMark = "never reached a state this run could check"

// The scripted model retires the mistaken path only after it is told what is
// outstanding, so the recovery message is what makes the run complete.
func TestDebtRecoveryRetiresAMistakenPathAndCompletes(t *testing.T) {
	dir := t.TempDir()
	const req = "Write a couple of small scripts."
	var recoveries, step int
	var mu sync.Mutex

	ctx, _, census, terminal := termFixture(t, dir, req, termCeiling,
		func(i int, prompt string) map[string]interface{} {
			sawRecovery := strings.Contains(prompt, debtRecoveryMark)
			switch {
			case i == 0 || i == 1:
				// a.py: the mistaken path, never lands.
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "a.py", "content": "@fenced"}}
			case i == 2:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "b.py", "content": debtGoodBody}}
			case i == 3:
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "python3 b.py"}}
			case !sawRecovery:
				return map[string]interface{}{"type": "done", "summary": "wrote what I could"}
			default:
				// Steps after the recovery are scripted, because the delete of
				// a path that never landed FAILS and leaves no marker in the
				// prompt to key on. The conditional part -- that the model
				// only abandons the path once it has been told -- is above.
				mu.Lock()
				step++
				n := step
				mu.Unlock()
				if n == 1 {
					return map[string]interface{}{"type": "tool_call", "name": "delete_file",
						"args": map[string]string{"path": "a.py"}}
				}
				return map[string]interface{}{"type": "done", "summary": "b.py is written and runs"}
			}
		})
	inner := ctx.StreamFn
	ctx.StreamFn = func(et string, data interface{}) {
		if et == "gate" {
			if b, _ := json.Marshal(data); strings.Contains(string(b), debtRecoveryMark) {
				mu.Lock()
				recoveries++
				mu.Unlock()
			}
		}
		inner(et, data)
	}
	if err := runAgentLoop(ctx, req); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	bBytes, _ := os.ReadFile(filepath.Join(dir, "b.py"))
	_, aErr := os.Stat(filepath.Join(dir, "a.py"))
	t.Logf("recoveries=%d status=%q reason=%q a.py_absent=%v",
		recoveries, terminal["status"], terminal["reason"], os.IsNotExist(aErr))

	if recoveries != 1 {
		t.Fatalf("recovery fired %d times, want exactly 1", recoveries)
	}
	if terminal["status"] != string(TerminalCompleted) {
		t.Fatalf("structured retirement did not complete: status=%q reason=%q summary=%.140s",
			terminal["status"], terminal["reason"], terminal["summary"])
	}
	if !os.IsNotExist(aErr) {
		t.Error("a.py was not confirmed absent")
	}
	if string(bBytes) != debtGoodBody {
		t.Errorf("b.py = %q", bBytes)
	}
	d := ctx.Ledger[ledgerKey(ctx, "b.py")]
	if d == nil || d.CurrentHash != hashBytes(bBytes) {
		t.Error("b.py's ledger hash does not match disk")
	}
	if k, s := d.CurrentValidation(); s != ValidationPassed || k != ValidationKindSyntax {
		t.Errorf("completion over %v/%v", k, s)
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result balance: %d vs %d", census["tool_call"], census["tool_result"])
	}
}

// A user-required path stays required even when the model deletes it.
func TestDeletingAUserRequiredPathDoesNotComplete(t *testing.T) {
	dir := t.TempDir()
	const req = "Write a.py and b.py."
	ctx, _, _, terminal := termFixture(t, dir, req, termCeiling,
		func(i int, prompt string) map[string]interface{} {
			sawRecovery := strings.Contains(prompt, debtRecoveryMark)
			retired := strings.Contains(prompt, `"deleted":true`)
			switch {
			case i == 0 || i == 1:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "a.py", "content": "@fenced"}}
			case i == 2:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "b.py", "content": debtGoodBody}}
			case !sawRecovery:
				return map[string]interface{}{"type": "done", "summary": "did what I could"}
			case !retired:
				return map[string]interface{}{"type": "tool_call", "name": "delete_file",
					"args": map[string]string{"path": "a.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "b.py is written"}
			}
		})
	if err := runAgentLoop(ctx, req); err != nil {
		t.Fatal(err)
	}
	t.Logf("required-path deletion: status=%q reason=%q", terminal["status"], terminal["reason"])
	if terminal["status"] == string(TerminalCompleted) {
		t.Fatal("deleting a user-required output completed the run")
	}
	if terminal["reason"] != "deliverables_not_demonstrated" {
		t.Errorf("reason = %q, want the prompt obligation to block first", terminal["reason"])
	}
}

// Ignoring the recovery, budget, boundedness, and prose.
func TestDebtRecoveryBoundsAndRefusals(t *testing.T) {
	// b.py lands so the unmet-action clause is satisfied and debt is the only
	// thing left blocking: that isolates what this test is about.
	ignore := func(i int, prompt string) map[string]interface{} {
		switch i {
		case 0, 1:
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "a.py", "content": "@fenced"}}
		case 2:
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "b.py", "content": debtGoodBody}}
		}
		return map[string]interface{}{"type": "done",
			"summary": "I no longer need a.py, so the work is complete."}
	}

	t.Run("ignored recovery stops honestly", func(t *testing.T) {
		dir := t.TempDir()
		ctx, _, census, terminal := termFixture(t, dir, "Write a couple of scripts.", termCeiling, ignore)
		if err := runAgentLoop(ctx, "Write a couple of scripts."); err != nil {
			t.Fatal(err)
		}
		t.Logf("ignored: status=%q reason=%q", terminal["status"], terminal["reason"])
		if terminal["status"] == string(TerminalCompleted) {
			t.Fatal("prose retired the work")
		}
		if terminal["reason"] != "unresolved_mutation_debt" {
			t.Errorf("reason = %q", terminal["reason"])
		}
		if completionClaimIn(terminal["summary"]) != "" {
			t.Errorf("the terminal claimed success:\n%s", terminal["summary"])
		}
		if census["done"] != 1 {
			t.Errorf("%d terminal events", census["done"])
		}
		// Nothing was run or mutated on the model's behalf.
		if _, err := os.Stat(filepath.Join(dir, "a.py")); err == nil {
			t.Error("recovery created the file itself")
		}
	})

	t.Run("offered once per generation and bounded overall", func(t *testing.T) {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		st := &runState{}
		args, _ := json.Marshal(map[string]string{"path": "a.py", "content": "@fenced"})
		noteMutationIntent(ctx, st, "write_file", args)
		if offerDebtRecovery(ctx, st) == "" {
			t.Fatal("no first offer")
		}
		if offerDebtRecovery(ctx, st) != "" {
			t.Error("a second offer in the same generation")
		}
		// New unresolved work opens the next generation.
		args2, _ := json.Marshal(map[string]string{"path": "b.py", "content": "@fenced"})
		noteMutationIntent(ctx, st, "write_file", args2)
		if offerDebtRecovery(ctx, st) == "" {
			t.Error("new unresolved work earned no offer")
		}
		// ...but not without bound.
		args3, _ := json.Marshal(map[string]string{"path": "c.py", "content": "@fenced"})
		noteMutationIntent(ctx, st, "write_file", args3)
		if offerDebtRecovery(ctx, st) != "" {
			t.Errorf("offers exceeded the cap of %d", maxDebtRecoveries)
		}
	})

	t.Run("low budget skips recovery", func(t *testing.T) {
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		workCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		ctx.Ctx = workCtx
		st := &runState{}
		args, _ := json.Marshal(map[string]string{"path": "a.py", "content": "@fenced"})
		noteMutationIntent(ctx, st, "write_file", args)
		if offerDebtRecovery(ctx, st) != "" {
			t.Error("recovery ran with less budget than it needs to be acted on")
		}
		if status, _ := finalizeCompletion(ctx, st, "Write a script.", ""); status.Completed() {
			t.Error("skipping recovery allowed completion")
		}
	})

	t.Run("same-path correction settles without deletion", func(t *testing.T) {
		dir := t.TempDir()
		var step2 int
		var stepMu sync.Mutex
		const req = "Write a couple of small scripts."
		ctx, _, _, terminal := termFixture(t, dir, req, termCeiling,
			func(i int, prompt string) map[string]interface{} {
				sawRecovery := strings.Contains(prompt, debtRecoveryMark)
				switch {
				case i == 0 || i == 1:
					return map[string]interface{}{"type": "tool_call", "name": "write_file",
						"args": map[string]string{"path": "a.py", "content": "@fenced"}}
				case i == 2:
					// Something has to land, or the action gate owns the loop
					// and the debt recovery is never reached — that gate is
					// the more specific failure and takes precedence.
					return map[string]interface{}{"type": "tool_call", "name": "write_file",
						"args": map[string]string{"path": "b.py", "content": debtGoodBody}}
				case !sawRecovery:
					return map[string]interface{}{"type": "done", "summary": "tried"}
				default:
					// Scripted after the recovery, for the same reason as in
					// TestDebtRecoveryRetiresAMistakenPathAndCompletes.
					stepMu.Lock()
					step2++
					n := step2
					stepMu.Unlock()
					if n == 1 {
						return map[string]interface{}{"type": "tool_call", "name": "write_file",
							"args": map[string]string{"path": "a.py", "content": debtGoodBody}}
					}
					return map[string]interface{}{"type": "done", "summary": "a.py is written"}
				}
			})
		if err := runAgentLoop(ctx, req); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "a.py"))
		t.Logf("corrected: status=%q reason=%q a.py=%q",
			terminal["status"], terminal["reason"], string(got))
		if terminal["status"] != string(TerminalCompleted) {
			t.Errorf("a corrected path did not complete: status=%q reason=%q",
				terminal["status"], terminal["reason"])
		}
	})
}
