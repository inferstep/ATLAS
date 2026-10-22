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
)

// The ledger records what each call did to the session's deliverables. It
// only observes: the tool's own result and the bytes on disk are what they
// would be without it.

func ledgerToolCtx(t *testing.T, dir string) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.StreamFn = func(string, interface{}) {}
	return ctx
}

func ledgerOf(t *testing.T, ctx *AgentContext, path string) *DeliverableState {
	t.Helper()
	return ctx.Ledger[ledgerKey(ctx, path)]
}

// diskHash is the ledger's only real claim: the recorded hash describes the
// bytes a reader would find right now.
func diskHash(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return hashBytes(b)
}

func TestContentMutatorsRecordTheBytesThatLanded(t *testing.T) {
	for _, c := range []struct {
		name, tool, seed string
		args             map[string]interface{}
		want             string
	}{
		{"write_file", "write_file", "",
			map[string]interface{}{"path": "solve.py", "content": "A = 1\n"},
			"A = 1\n"},
		{"edit_file", "edit_file", "A = 1\n",
			map[string]interface{}{"path": "solve.py", "old_str": "A = 1", "new_str": "A = 2"},
			"A = 2\n"},
		{"insert_after", "insert_after", "A = 1\n",
			map[string]interface{}{"path": "solve.py", "line": 1, "content": "B = 2"},
			"A = 1\nB = 2\n"},
		{"replace_lines", "replace_lines", "A = 1\nB = 2\n",
			map[string]interface{}{"path": "solve.py", "start_line": 1, "end_line": 1,
				"expected_first_line": "A = 1", "expected_last_line": "A = 1",
				"content": "A = 99"},
			"A = 99\nB = 2\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			full := filepath.Join(dir, "solve.py")
			if c.seed != "" {
				os.WriteFile(full, []byte(c.seed), 0o644)
			}
			ctx := ledgerToolCtx(t, dir)
			if c.seed != "" {
				// The edit tools require a current read; that guard is not
				// what this test is measuring.
				r, _ := json.Marshal(map[string]string{"path": "solve.py"})
				executeToolCall("read_file", r, ctx)
			}
			args, _ := json.Marshal(c.args)
			res := executeToolCall(c.tool, args, ctx)
			if !res.Success {
				t.Fatalf("%s failed: %v", c.tool, res.Error)
			}
			got, _ := os.ReadFile(full)
			if string(got) != c.want {
				t.Fatalf("disk bytes = %q, want %q", got, c.want)
			}
			d := ledgerOf(t, ctx, "solve.py")
			if d == nil {
				t.Fatal("the mutation was not recorded")
			}
			if d.CurrentHash != diskHash(t, full) {
				t.Errorf("ledger hash does not describe the bytes on disk")
			}
			if d.CurrentSize != len(c.want) {
				t.Errorf("CurrentSize = %d, want %d", d.CurrentSize, len(c.want))
			}
			// The tool's own evidence is copied, never upgraded.
			if d.ValidationStatus != res.ValidationStatus {
				t.Errorf("ledger status %q != tool status %q",
					d.ValidationStatus, res.ValidationStatus)
			}
			if d.CheckpointHash != "" && res.ValidationStatus != ValidationPassed {
				t.Error("checkpointed bytes that were never reported as passed")
			}
		})
	}
}

func TestDeleteRecordsATombstoneOnlyWhenTheFileIsGone(t *testing.T) {
	dir := t.TempDir()
	ctx := ledgerToolCtx(t, dir)
	w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": "A = 1\n"})
	executeToolCall("write_file", w, ctx)

	// Failed delete of a path this session never wrote: nothing invented.
	d1, _ := json.Marshal(map[string]string{"path": "never_existed.py"})
	if res := executeToolCall("delete_file", d1, ctx); res.Success {
		t.Fatal("deleting a missing file unexpectedly succeeded")
	}
	if d := ledgerOf(t, ctx, "never_existed.py"); d != nil {
		t.Errorf("a failed delete fabricated a tombstone: %+v", d)
	}

	d2, _ := json.Marshal(map[string]string{"path": "solve.py"})
	if res := executeToolCall("delete_file", d2, ctx); !res.Success {
		t.Fatalf("delete failed: %v", res.Error)
	}
	d := ledgerOf(t, ctx, "solve.py")
	if d == nil || !d.Tombstoned {
		t.Fatalf("delete was not tombstoned: %+v", d)
	}
	if !d.RestoreProhibited {
		t.Error("a deliberate delete must prohibit automatic restoration")
	}
	if _, s := d.CurrentValidation(); s != ValidationUnknown {
		t.Error("a deleted path still reports a current verdict")
	}
}

func TestMoveTombstonesTheSourceAndObservesTheDestinationFresh(t *testing.T) {
	dir := t.TempDir()
	ctx := ledgerToolCtx(t, dir)
	w, _ := json.Marshal(map[string]string{"path": "old.py", "content": "A = 1\n"})
	executeToolCall("write_file", w, ctx)

	m, _ := json.Marshal(map[string]string{"source": "old.py", "destination": "new.py"})
	if res := executeToolCall("move_file", m, ctx); !res.Success {
		t.Fatalf("move failed: %v", res.Error)
	}
	src := ledgerOf(t, ctx, "old.py")
	if src == nil || !src.Tombstoned || !strings.HasPrefix(src.TombstoneReason, "moved:") {
		t.Fatalf("source not tombstoned as moved: %+v", src)
	}
	dst := ledgerOf(t, ctx, "new.py")
	if dst == nil {
		t.Fatal("destination not observed")
	}
	if dst.CurrentHash != diskHash(t, filepath.Join(dir, "new.py")) {
		t.Error("destination hash does not describe the bytes on disk")
	}
	if _, s := dst.CurrentValidation(); s == ValidationPassed {
		t.Error("a rename inherited a verdict earned under the old name")
	}
}

// A shell command can rewrite anything. Paths it did not touch keep their
// evidence; paths it changed lose it, because the verdict described bytes
// that are gone.
func TestRunCommandRehashesTrackedPaths(t *testing.T) {
	dir := t.TempDir()
	ctx := ledgerToolCtx(t, dir)
	for _, f := range []string{"quiet.py", "noisy.py"} {
		w, _ := json.Marshal(map[string]string{"path": f, "content": "A = 1\n"})
		executeToolCall("write_file", w, ctx)
	}
	quietBefore := ledgerOf(t, ctx, "quiet.py").CurrentHash

	c, _ := json.Marshal(map[string]string{"command": "printf 'REWRITTEN\\n' > noisy.py"})
	if res := executeToolCall("run_command", c, ctx); res.MutationStatus != MutationUnobserved {
		t.Fatalf("run_command classification changed: %q", res.MutationStatus)
	}
	if q := ledgerOf(t, ctx, "quiet.py"); q.CurrentHash != quietBefore {
		t.Error("an untouched path was re-recorded")
	}
	n := ledgerOf(t, ctx, "noisy.py")
	if n.CurrentHash != diskHash(t, filepath.Join(dir, "noisy.py")) {
		t.Error("ledger did not catch up to the command's rewrite")
	}
	if _, s := n.CurrentValidation(); s != ValidationUnknown {
		t.Error("a command-rewritten path kept a verdict about bytes that are gone")
	}
}

// The hazard describes work that may exist, not attempts that were made.
// A call the tool refused before dispatch created nothing to be hazardous
// about; a call that may have reached the sandbox and came back with no job
// id did, and nothing can reap that.
func TestBackgroundHazardFollowsDispatchNotTheAttempt(t *testing.T) {
	t.Run("refused before dispatch leaves no hazard", func(t *testing.T) {
		ctx := ledgerToolCtx(t, t.TempDir()) // VerifyOnHost: never dispatches
		ctx.SandboxURL = ""
		args, _ := json.Marshal(map[string]string{"command": "python -m http.server"})
		if res := executeToolCall("run_background", args, ctx); res.Success {
			t.Fatal("expected the start to be refused")
		}
		if workspaceHazardous(ctx) {
			t.Error("a start that never dispatched left a hazard nothing can lower")
		}
	})

	t.Run("may have dispatched, no job id: unresolvable hazard", func(t *testing.T) {
		ctx := ledgerToolCtx(t, t.TempDir())
		ctx.VerifyOnHost = false
		ctx.SandboxURL = "http://127.0.0.1:1" // reachable-looking, actually not
		args, _ := json.Marshal(map[string]string{"command": "python -m http.server"})
		if res := executeToolCall("run_background", args, ctx); res.Success {
			t.Fatal("expected the start to fail")
		}
		if !workspaceHazardous(ctx) {
			t.Error("a start that may have run left no hazard")
		}
		// Nothing to reap: reaping cannot clear what it cannot name.
		reapSessionBackgroundJobs(ctx)
		if !workspaceHazardous(ctx) {
			t.Error("an unidentified job was cleared by reaping nothing")
		}
	})
}

func TestStopBackgroundClearsTheHazardOnlyOnAReapedExit(t *testing.T) {
	var exitCode *int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/jobs/start"):
			json.NewEncoder(w).Encode(map[string]interface{}{"job_id": "j1", "pid": 4242})
		case strings.Contains(r.URL.Path, "/output"):
			// The settle-window tail: the job is live, so it is tracked and
			// owns a hazard until something confirms its exit.
			json.NewEncoder(w).Encode(map[string]interface{}{
				"job_id": "j1", "running": true, "stdout": []string{},
				"stderr": []string{}, "elapsed_sec": 0.1, "command": "sleep 60"})
		case strings.HasSuffix(r.URL.Path, "/stop"):
			out := map[string]interface{}{"job_id": "j1", "killed": true,
				"stdout": []string{}, "stderr": []string{}}
			if exitCode != nil {
				out["exit_code"] = *exitCode
			}
			json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := ledgerToolCtx(t, t.TempDir())
	ctx.VerifyOnHost = false // so the start actually dispatches
	ctx.SandboxURL = srv.URL
	start, _ := json.Marshal(map[string]string{"command": "sleep 60"})
	executeToolCall("run_background", start, ctx)
	if !workspaceHazardous(ctx) {
		t.Fatal("run_background did not raise the hazard")
	}

	// Signalled but not reaped: the writer may still be flushing.
	stop, _ := json.Marshal(map[string]string{"job_id": "j1"})
	executeToolCall("stop_background", stop, ctx)
	if !workspaceHazardous(ctx) {
		t.Error("hazard cleared without a confirmed exit")
	}

	zero := 0
	exitCode = &zero
	executeToolCall("run_background", start, ctx)
	executeToolCall("stop_background", stop, ctx)
	executeToolCall("stop_background", stop, ctx)
	if workspaceHazardous(ctx) {
		t.Error("reaped exits did not clear the hazard")
	}
}

// The ledger after one branch of each direct mutator, driven through the real
// boundary. Four rules hold for every row:
//
//  1. a recorded current hash always describes the bytes on disk;
//  2. a checkpoint exists only where the tool reported an explicit pass;
//  3. a branch that says it changed nothing records nothing, so a file the
//     model was refused a write to never looks like the session's work;
//  4. no branch reports MutationUnknown: a direct mutator owes its own
//     classification.
func TestDirectMutatorOutcomeTable(t *testing.T) {
	type step struct {
		tool string
		args map[string]interface{}
	}
	cases := []struct {
		name  string
		seed  map[string]string
		read  []string
		steps []step
		path  string // the deliverable this row is about
	}{
		{"write_file/new non-code", nil, nil,
			[]step{{"write_file", map[string]interface{}{"path": "notes.txt", "content": "hi\n"}}}, "notes.txt"},
		{"write_file/new code", nil, nil,
			[]step{{"write_file", map[string]interface{}{"path": "m.py", "content": "A = 1\n"}}}, "m.py"},
		{"write_file/deny-list refusal", map[string]string{".env": "S=1\n"}, nil,
			[]step{{"write_file", map[string]interface{}{"path": ".env", "content": "S=2\n"}}}, ".env"},
		{"write_file/escapes workspace", nil, nil,
			[]step{{"write_file", map[string]interface{}{"path": "../out.py", "content": "A = 1\n"}}}, "../out.py"},
		{"write_file/echoed write refused", map[string]string{"m.py": strings.Repeat("A = 1\n", 40)}, nil,
			[]step{{"write_file", map[string]interface{}{"path": "m.py", "content": strings.Repeat("A = 1\n", 40)}}}, "m.py"},

		{"edit_file/applied", map[string]string{"m.py": "A = 1\n"}, []string{"m.py"},
			[]step{{"edit_file", map[string]interface{}{"path": "m.py", "old_str": "A = 1", "new_str": "A = 2"}}}, "m.py"},
		{"edit_file/old_str absent", map[string]string{"m.py": "A = 1\n"}, []string{"m.py"},
			[]step{{"edit_file", map[string]interface{}{"path": "m.py", "old_str": "ZZZ", "new_str": "Q"}}}, "m.py"},
		{"edit_file/not read first", map[string]string{"m.py": "A = 1\n"}, nil,
			[]step{{"edit_file", map[string]interface{}{"path": "m.py", "old_str": "A = 1", "new_str": "A = 2"}}}, "m.py"},

		{"structural_edit/function body", map[string]string{"m.py": "def f():\n    return 1\n"}, []string{"m.py"},
			[]step{{"structural_edit", map[string]interface{}{"path": "m.py",
				"selector": "function:f", "content": "def f():\n    return 2\n"}}}, "m.py"},
		{"structural_edit/selector missing", map[string]string{"m.py": "def f():\n    return 1\n"}, []string{"m.py"},
			[]step{{"structural_edit", map[string]interface{}{"path": "m.py",
				"selector": "function:nope", "content": "def nope():\n    pass\n"}}}, "m.py"},

		{"insert_after/applied", map[string]string{"m.py": "A = 1\n"}, []string{"m.py"},
			[]step{{"insert_after", map[string]interface{}{"path": "m.py", "line": 1, "content": "B = 2"}}}, "m.py"},
		{"insert_after/line past end", map[string]string{"m.py": "A = 1\n"}, []string{"m.py"},
			[]step{{"insert_after", map[string]interface{}{"path": "m.py", "line": 99, "content": "B = 2"}}}, "m.py"},

		{"replace_lines/applied", map[string]string{"m.py": "A = 1\nB = 2\n"}, []string{"m.py"},
			[]step{{"replace_lines", map[string]interface{}{"path": "m.py", "start_line": 1, "end_line": 1,
				"expected_first_line": "A = 1", "expected_last_line": "A = 1", "content": "A = 9"}}}, "m.py"},
		{"replace_lines/anchor mismatch", map[string]string{"m.py": "A = 1\nB = 2\n"}, []string{"m.py"},
			[]step{{"replace_lines", map[string]interface{}{"path": "m.py", "start_line": 1, "end_line": 1,
				"expected_first_line": "WRONG", "expected_last_line": "WRONG", "content": "A = 9"}}}, "m.py"},

		{"delete_file/applied", map[string]string{"m.py": "A = 1\n"}, nil,
			[]step{{"delete_file", map[string]interface{}{"path": "m.py"}}}, "m.py"},
		{"delete_file/missing", nil, nil,
			[]step{{"delete_file", map[string]interface{}{"path": "gone.py"}}}, "gone.py"},

		{"move_file/applied", map[string]string{"old.py": "A = 1\n"}, nil,
			[]step{{"move_file", map[string]interface{}{"source": "old.py", "destination": "new.py"}}}, "old.py"},
		{"move_file/missing source", nil, nil,
			[]step{{"move_file", map[string]interface{}{"source": "gone.py", "destination": "new.py"}}}, "gone.py"},
	}

	var unknownMutators []string
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for f, body := range c.seed {
				os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644)
			}
			ctx := ledgerToolCtx(t, dir)
			for _, f := range c.read {
				r, _ := json.Marshal(map[string]string{"path": f})
				executeToolCall("read_file", r, ctx)
			}
			var res *ToolResult
			for _, s := range c.steps {
				args, _ := json.Marshal(s.args)
				res = executeToolCall(s.tool, args, ctx)
			}

			d := ledgerOf(t, ctx, c.path)
			full := filepath.Join(dir, c.path)
			_, statErr := os.Stat(full)

			state := "not recorded"
			if d != nil {
				state = fmt.Sprintf("gen=%d tomb=%v ckpt=%v", d.Generation,
					d.Tombstoned, d.CheckpointHash != "")
			}
			t.Logf("mutation=%-10q validation=%s/%-14q ledger[%s]",
				res.MutationStatus, res.ValidationKind, res.ValidationStatus, state)

			// (3) a branch that says disk did not change records nothing.
			if (res.MutationStatus == MutationNone || res.MutationStatus == MutationRefused) && d != nil {
				t.Errorf("a branch that did not change disk created a ledger entry: %+v", d)
			}
			// (4) collected here, judged after all rows ran.
			if res.MutationStatus == MutationUnknown {
				unknownMutators = append(unknownMutators, c.name)
			}
			if d == nil {
				return
			}
			// (1) the hash describes what is actually there.
			if !d.Tombstoned && statErr == nil && d.CurrentHash != diskHash(t, full) {
				t.Errorf("ledger hash does not describe the bytes on disk")
			}
			if d.Tombstoned && statErr == nil {
				t.Error("tombstoned a path that is still on disk")
			}
			// (2) checkpoints come only from an explicit pass.
			if d.CheckpointHash != "" && res.ValidationStatus != ValidationPassed {
				t.Errorf("checkpointed bytes reported as %q, not passed", res.ValidationStatus)
			}
		})
	}
	if len(unknownMutators) > 0 {
		t.Errorf("branches reporting MutationUnknown: %v — a direct mutator "+
			"owes a local classification; the boundary will not invent one",
			unknownMutators)
	}
}

// --- the ledger through the production agent loop --------------------------
//
// Two claims, proved together on one run of runAgentLoop:
//
//	the ledger's current hash describes the bytes a reader finds on disk
//	after the loop returns, including a rewrite performed by a shell
//	command rather than by an edit tool; and
//
//	the ledger is invisible from outside: the full ordered event stream is
//	pinned by its hash.
//
// The pin covers the SSE stream only. TestModelPromptBytesAreUnchangedByClassification
// covers the model-facing prompt. Set ATLAS_LEDGER_TRANSCRIPT to a file path
// to write the transcript out, so that a change in the stream can be read
// and the hash re-pinned on purpose.
const ledgerLoopTranscriptHash = "8508e55eb149f8123201993fa7646888b6d47e548f6b57cf98f4f6153f0104f7"

// prompt_tokens is elided for a fixture reason rather than a timing one: the
// workspace path is part of the system prompt and t.TempDir() varies in
// length between runs, moving the count by a token. Every other field,
// including every tool call and result, is compared exactly.
//
// volatileEventKeys are elided from the transcript: they carry wall-clock
// facts that differ between two runs of the same code, so including them
// would make the comparison meaningless rather than strict.
var volatileEventKeys = map[string]bool{
	"ms": true, "duration": true, "duration_ms": true, "elapsed": true,
	"elapsed_ms": true, "timestamp": true, "seconds": true, "wall_s": true,
	"started_at": true, "ended_at": true, "pid": true, "prompt_tokens": true,
}

func stripVolatile(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, sub := range t {
			if volatileEventKeys[k] || strings.HasSuffix(k, "_ms") {
				continue
			}
			out[k] = stripVolatile(sub)
		}
		return out
	case []interface{}:
		out := make([]interface{}, 0, len(t))
		for _, sub := range t {
			out = append(out, stripVolatile(sub))
		}
		return out
	}
	return v
}

func TestLedgerTracksFinalDiskBytesThroughTheAgentLoop(t *testing.T) {
	dir := t.TempDir()
	rel := "solve.py"

	var turnMu sync.Mutex
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		io.Copy(io.Discard, r.Body)
		turnMu.Lock()
		turn++
		n := turn
		turnMu.Unlock()

		var payload map[string]interface{}
		switch n {
		case 1:
			payload = map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": rel, "content": "A = 1\n"}}
		case 2:
			payload = map[string]interface{}{"type": "tool_call", "name": "read_file",
				"args": map[string]string{"path": rel}}
		case 3:
			payload = map[string]interface{}{"type": "tool_call", "name": "edit_file",
				"args": map[string]string{"path": rel, "old_str": "A = 1", "new_str": "A = 2"}}
		case 4:
			// The case the ledger exists for: bytes changing outside the
			// edit tools, so the last recorded verdict stops being about
			// what is on disk.
			payload = map[string]interface{}{"type": "tool_call", "name": "run_command",
				"args": map[string]string{"command": "printf 'A = 3\\n' > " + rel}}
		default:
			payload = map[string]interface{}{"type": "done",
				"summary": "wrote solve.py"}
		}
		call, _ := json.Marshal(payload)
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	defer srv.Close()

	var evMu sync.Mutex
	var transcript strings.Builder
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 8
	ctx.StreamFn = func(eventType string, data interface{}) {
		raw, _ := json.Marshal(data)
		var generic interface{}
		json.Unmarshal(raw, &generic)
		clean, _ := json.Marshal(stripVolatile(generic))
		evMu.Lock()
		fmt.Fprintf(&transcript, "%s %s\n", eventType, clean)
		evMu.Unlock()
	}

	if err := runAgentLoop(ctx, "Create solve.py."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	full := filepath.Join(dir, rel)
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if string(got) != "A = 3\n" {
		t.Fatalf("final disk bytes = %q, want the command's rewrite", got)
	}

	d := ledgerOf(t, ctx, rel)
	if d == nil {
		t.Fatal("the loop's deliverable was never recorded")
	}
	if d.CurrentHash != hashBytes(got) {
		t.Errorf("ledger hash %s does not describe the final bytes %s",
			d.CurrentHash[:12], hashBytes(got)[:12])
	}
	if _, s := d.CurrentValidation(); s == ValidationPassed {
		t.Error("a shell rewrite inherited the edit tool's verdict")
	}
	for key, entry := range ctx.Ledger {
		if entry.Tombstoned {
			continue
		}
		b, err := os.ReadFile(key)
		if err != nil {
			continue
		}
		if entry.CurrentHash != hashBytes(b) {
			t.Errorf("%s: ledger hash disagrees with disk", key)
		}
	}

	evMu.Lock()
	text := transcript.String()
	evMu.Unlock()
	if out := os.Getenv("ATLAS_LEDGER_TRANSCRIPT"); out != "" {
		os.WriteFile(out, []byte(text), 0o600)
	}
	if h := hashBytes([]byte(text)); h != ledgerLoopTranscriptHash {
		t.Errorf("event stream changed (%s, want %s); the ledger must be "+
			"invisible from outside:\n%s", h[:12], ledgerLoopTranscriptHash[:12], text)
	}
	t.Logf("transcript: %d events, %d bytes, sha256 %s",
		strings.Count(text, "\n"), len(text), hashBytes([]byte(text)))
}

// SessionWrites is keyed on the raw path the model gave, so two spellings of
// one file are two entries; the ledger keys on the resolved path. Pinned so
// the difference stays visible, and changing it is a deliberate step.
func TestSessionWritesKeyingIsRawWhileTheLedgerIsCanonical(t *testing.T) {
	dir := t.TempDir()
	ctx := ledgerToolCtx(t, dir)
	for _, spelling := range []string{"solve.py", "./solve.py"} {
		args, _ := json.Marshal(map[string]string{"path": spelling, "content": "A = 1\n"})
		if res := executeToolCall("write_file", args, ctx); !res.Success {
			t.Fatalf("write %q failed: %v", spelling, res.Error)
		}
	}
	if len(ctx.SessionWrites) != 2 {
		t.Errorf("SessionWrites keying changed: %v — if this was fixed "+
			"deliberately, update the note on the field", ctx.SessionWrites)
	}
	if len(ctx.Ledger) != 1 {
		t.Errorf("the ledger split one file across %d entries: %v",
			len(ctx.Ledger), ctx.Ledger)
	}
}

// Checkpoint promotion on a production route rather than a direct call. A new
// code file routes through the syntax gate, so with a sandbox answering the
// write earns an explicit pass and those exact bytes become the path's
// checkpoint. The follow-up write is deliberately broken: it must be recorded
// and must NOT displace the checkpoint the passing bytes hold.
func TestCheckpointPromotionThroughTheWriteRoute(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/syntax-check") {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Code string `json:"code"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		bodies = append(bodies, in.Code)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"valid": !strings.Contains(in.Code, "def broken(")})
	}))
	defer srv.Close()

	dir := t.TempDir()
	ctx := ledgerToolCtx(t, dir)
	ctx.SandboxURL = srv.URL

	good := "def f():\n    return 1\n"
	args, _ := json.Marshal(map[string]string{"path": "solve.py", "content": good})
	res := executeToolCall("write_file", args, ctx)
	if res.ValidationStatus != ValidationPassed {
		t.Fatalf("the write route did not produce a pass (%s/%s); this test "+
			"proves nothing without one", res.ValidationKind, res.ValidationStatus)
	}
	d := ledgerOf(t, ctx, "solve.py")
	if d.CheckpointHash != hashBytes([]byte(good)) {
		t.Fatalf("passing bytes were not checkpointed: %+v", d)
	}

	// The syntax gate refuses a broken rewrite outright, so disk and the
	// checkpoint both stand.
	args2, _ := json.Marshal(map[string]string{"path": "solve.py", "content": "def broken(\n"})
	if r := executeToolCall("write_file", args2, ctx); r.MutationStatus != MutationRefused {
		t.Fatalf("expected the gate to refuse the broken rewrite, got %q", r.MutationStatus)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "solve.py")); string(got) != good {
		t.Fatalf("a refused write reached disk: %q", got)
	}

	// Bytes that DO land without a verdict -- a shell rewrite -- are recorded
	// and must not become the checkpoint either.
	cmd, _ := json.Marshal(map[string]string{"command": "printf 'x = (\\n' > solve.py"})
	executeToolCall("run_command", cmd, ctx)
	d = ledgerOf(t, ctx, "solve.py")
	if d.CheckpointHash != hashBytes([]byte(good)) {
		t.Errorf("a failing write displaced the checkpoint: %+v", d)
	}
	if string(d.CheckpointBytes) != good {
		t.Errorf("checkpoint bytes = %q", d.CheckpointBytes)
	}
	if d.CurrentHash == d.CheckpointHash {
		t.Error("the shell rewrite was not recorded as the current bytes")
	}
	if d.CurrentHash != diskHash(t, filepath.Join(dir, "solve.py")) {
		t.Error("current hash does not describe the bytes on disk")
	}
	if _, s := d.CurrentValidation(); s == ValidationPassed {
		t.Error("the failing bytes read as passed")
	}
}

func TestWhichMutatorsCanEverPromoteACheckpoint(t *testing.T) {
	src := "def f():\n    return 1\n"
	sandbox := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
		case strings.HasSuffix(r.URL.Path, "/internal/structural_edit"):
			var in struct{ Source string }
			json.NewDecoder(r.Body).Decode(&in)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true,
				"language": "python", "new_content": "def f():\n    return 2\n",
				"old_size": len(in.Source), "new_size": 22})
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}
	for _, c := range []struct {
		tool, args string
		read       bool
	}{
		{"write_file", `{"path":"m.py","content":"def f():\n    return 9\n"}`, false},
		{"edit_file", `{"path":"m.py","old_str":"return 1","new_str":"return 2"}`, true},
		{"insert_after", `{"path":"m.py","line":2,"content":"G = 1"}`, true},
		{"replace_lines", `{"path":"m.py","start_line":2,"end_line":2,"expected_first_line":"    return 1","expected_last_line":"    return 1","content":"    return 2"}`, true},
		{"structural_edit", `{"path":"m.py","selector":"function:f","content":"def f():\n    return 2\n"}`, true},
		{"delete_file", `{"path":"m.py"}`, false},
		{"move_file", `{"source":"m.py","destination":"n.py"}`, false},
	} {
		t.Run(c.tool, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "m.py"), []byte(src), 0o644)
			srv := httptest.NewServer(http.HandlerFunc(sandbox))
			defer srv.Close()
			ctx := ledgerToolCtx(t, dir)
			ctx.SandboxURL = srv.URL
			ctx.V3URL = srv.URL
			if c.read {
				r, _ := json.Marshal(map[string]string{"path": "m.py"})
				executeToolCall("read_file", r, ctx)
			}
			res := executeToolCall(c.tool, json.RawMessage(c.args), ctx)
			promoted := false
			for _, d := range ctx.Ledger {
				if d.CheckpointHash != "" {
					promoted = true
				}
			}
			t.Logf("%-16s validation=%s/%-14s checkpoint=%v", c.tool,
				res.ValidationKind, res.ValidationStatus, promoted)
			// The invariant, whatever the answer: a checkpoint exists only
			// where the producer said passed for the bytes that landed.
			if promoted != (res.ValidationStatus == ValidationPassed) {
				t.Errorf("checkpoint=%v but validation=%q", promoted, res.ValidationStatus)
			}
		})
	}
}
