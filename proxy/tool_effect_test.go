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

// Effect classification is a property of the tool, declared on its ToolDef,
// so a new tool cannot be registered without deciding what it can do to the
// workspace. A parallel map keyed by name would drift; this cannot.
//
// The classes describe CAPABILITY, not outcome. A direct mutator still
// reports applied/refused/failed/none per branch; the effect only says the
// tool is capable of mutating and therefore owes a local classification.
func TestEveryRegisteredToolDeclaresAnEffect(t *testing.T) {
	if len(toolRegistry) == 0 {
		t.Fatal("tool registry is empty; this test would prove nothing")
	}
	var missing []string
	for name, def := range toolRegistry {
		if def.Effect == ToolEffectUnknown {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("tools registered without an effect class: %v", missing)
	}
}

// The verified distribution, pinned so a reclassification has to be
// deliberate. stop_background is CommandUnobserved rather than read-only or
// control: it sends SIGTERM/SIGKILL and performs no filesystem comparison, so
// a process killed mid-write may leave partial bytes nobody measured.
func TestEffectDistributionMatchesTheVerifiedAudit(t *testing.T) {
	want := map[string]ToolEffect{
		"read_file": ToolEffectReadOnly, "search_files": ToolEffectReadOnly,
		"list_directory": ToolEffectReadOnly, "find_file": ToolEffectReadOnly,
		"outline_file": ToolEffectReadOnly, "tail_background": ToolEffectReadOnly,

		"write_file": ToolEffectDirectMutation, "edit_file": ToolEffectDirectMutation,
		"structural_edit": ToolEffectDirectMutation, "delete_file": ToolEffectDirectMutation,
		"move_file": ToolEffectDirectMutation, "insert_after": ToolEffectDirectMutation,
		"replace_lines": ToolEffectDirectMutation,

		"run_command":     ToolEffectCommandUnobserved,
		"run_background":  ToolEffectCommandUnobserved,
		"stop_background": ToolEffectCommandUnobserved,
	}
	for name, expect := range want {
		def, ok := toolRegistry[name]
		if !ok {
			t.Errorf("expected tool %q is not registered", name)
			continue
		}
		if def.Effect != expect {
			t.Errorf("%s: effect = %q, want %q", name, def.Effect, expect)
		}
	}
	for name := range toolRegistry {
		if _, known := want[name]; !known {
			t.Errorf("tool %q is registered but absent from the verified audit; "+
				"classify it deliberately", name)
		}
	}
}

func TestDirectMutatorsAreNotClassifiableAtTheBoundary(t *testing.T) {
	for name, def := range toolRegistry {
		if def.Effect != ToolEffectDirectMutation {
			continue
		}
		// A direct mutator's outcome is only knowable at the branch that
		// performed it, so the boundary must never invent one.
		if def.Effect.BoundaryClassifiable() {
			t.Errorf("%s: a direct mutator must not be classifiable at the "+
				"shared boundary", name)
		}
	}
}

// Boundary classification through the real executeToolCall. A read-only tool
// mutates nothing, whether it succeeds or fails, and a refusal before dispatch
// proves no handler ran. A command tool that reached its handler is
// unobserved whatever its exit status: a subprocess may have written before
// it failed.
func TestTheBoundaryClassifiesReadOnlyAndCommandTools(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.StreamFn = func(string, interface{}) {}

	for _, tc := range []struct {
		name string
		tool string
		args string
		want MutationStatus
	}{
		{"read success", "read_file", `{"path":"f.txt"}`, MutationNone},
		{"read missing file", "read_file", `{"path":"nope.txt"}`, MutationNone},
		{"list", "list_directory", `{"path":"."}`, MutationNone},
		{"unknown tool", "no_such_tool", `{}`, MutationNone},
		{"missing args", "read_file", ``, MutationNone},
		{"command success", "run_command", `{"command":"true"}`, MutationUnobserved},
		{"command non-zero exit", "run_command", `{"command":"false"}`, MutationUnobserved},
		{"command writes then fails", "run_command", `{"command":"echo x > side_effect.txt; exit 3"}`, MutationUnobserved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := executeToolCall(tc.tool, json.RawMessage(tc.args), ctx)
			if res.MutationStatus != tc.want {
				t.Errorf("MutationStatus = %q, want %q", res.MutationStatus, tc.want)
			}
			if !res.Classified() {
				t.Errorf("result not fully classified: %+v", res)
			}
			if res.ValidationStatus.Passed() {
				t.Error("validation was synthesized as passed")
			}
		})
	}
}

// Only read-only and command effects get their outcome from the shared
// boundary. A direct mutator's outcome is knowable only at the branch that
// performed it, and an unknown effect has no outcome to give.
func TestOnlyReadOnlyAndCommandEffectsAreBoundaryClassifiable(t *testing.T) {
	for _, tc := range []struct {
		effect ToolEffect
		want   bool
	}{
		{ToolEffectReadOnly, true},
		{ToolEffectCommandUnobserved, true},
		{ToolEffectDirectMutation, false},
		{ToolEffectUnknown, false},
	} {
		if got := tc.effect.BoundaryClassifiable(); got != tc.want {
			t.Errorf("%q: BoundaryClassifiable() = %v, want %v", tc.effect, got, tc.want)
		}
	}
}

// A direct mutator that fails gives its own classification and the real
// error. Deleting a file that was never there removed nothing.
func TestDirectMutatorFailureCarriesItsProducersClassification(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}

	args, _ := json.Marshal(map[string]string{"path": "missing.txt"})
	res := executeToolCall("delete_file", args, ctx)
	if res.Success {
		t.Fatal("deleting a missing file unexpectedly succeeded")
	}
	if res.MutationStatus != MutationNone {
		t.Errorf("MutationStatus = %q, want none: nothing was removed",
			res.MutationStatus)
	}
	if !res.Classified() {
		t.Errorf("producer left the result unclassified: %+v", res)
	}
	if res.Error == "" {
		t.Error("the real error was suppressed")
	}
}

// --- write_file ---------------------------------------------------------------
//
// writeFileDirect is the terminal writer for the direct (non-V3) path. It
// shows the mutation but runs no syntax check, so it reports applied plus an
// honest validation state, never passed.

func TestWriteFileDirectClassification(t *testing.T) {
	for _, c := range []struct {
		name, file, content string
		wantKind            ValidationKind
		wantStatus          ValidationStatus
	}{
		{"non-code write", "notes.txt", "hello\n",
			ValidationKindNone, ValidationNotApplicable},
		{"recognized code, unvalidated at this layer", "mod.py", "A = 1\n",
			ValidationKindSyntax, ValidationNotRun},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.PermissionMode = PermissionYolo
			ctx.StreamFn = func(string, interface{}) {}

			args, _ := json.Marshal(map[string]string{"path": c.file, "content": c.content})
			res := executeToolCall("write_file", args, ctx)

			if !res.Success || res.Error != "" {
				t.Fatalf("the write was not a plain success: %+v", res)
			}
			if res.MutationStatus != MutationApplied {
				t.Errorf("MutationStatus = %q, want applied", res.MutationStatus)
			}
			if res.ValidationKind != c.wantKind {
				t.Errorf("ValidationKind = %q, want %q", res.ValidationKind, c.wantKind)
			}
			if res.ValidationStatus != c.wantStatus {
				t.Errorf("ValidationStatus = %q, want %q", res.ValidationStatus, c.wantStatus)
			}
			if res.ValidationStatus.Passed() {
				t.Error("a successful write must not synthesize validation passed")
			}
			if !res.Classified() {
				t.Errorf("result not fully classified: %+v", res)
			}
			got, err := os.ReadFile(filepath.Join(dir, c.file))
			if err != nil || string(got) != c.content {
				t.Fatalf("final bytes wrong: %q err=%v", string(got), err)
			}
		})
	}
}

// A refusal before the handler can mutate. The unread-existing-file guard
// lives in the agent loop, not here, so this uses a deny-list refusal: a
// check inside executeToolCall itself, before dispatch.
func TestWriteFileRefusalBeforeMutationLeavesBytes(t *testing.T) {
	dir := t.TempDir()
	prior := "SECRET=keepme\n"
	os.WriteFile(filepath.Join(dir, ".env"), []byte(prior), 0o644)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}

	args, _ := json.Marshal(map[string]string{"path": ".env", "content": "SECRET=stolen\n"})
	res := executeToolCall("write_file", args, ctx)
	if res.Success {
		t.Fatal("expected the deny-list to refuse a write to .env")
	}
	got, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if string(got) != prior {
		t.Fatalf("prior bytes changed on a refusal: %q", string(got))
	}
	// No handler ran, so nothing could have mutated.
	if res.MutationStatus != MutationNone {
		t.Errorf("MutationStatus = %q, want none", res.MutationStatus)
	}
	if !res.Classified() {
		t.Errorf("pre-dispatch refusal not classified: %+v", res)
	}
}

// Invalid code reports syntax/failed whether it lands or not. New code lands
// with a warning: applied and failed are separate facts. Healthy code the
// session wrote is protected from an invalid overwrite: refused, and the old
// bytes stay. Both checks examine the exact bytes the write path used.
func TestAnInvalidWriteReportsSyntaxFailedWhetherItLandsOrNot(t *testing.T) {
	for _, tc := range []struct {
		name         string
		priorValid   bool
		wantSuccess  bool
		wantMutation MutationStatus
		wantOnDisk   string
	}{
		{"new file lands", false, true, MutationApplied, banLoopInvalidBody},
		{"overwrite of healthy code is refused", true, false, MutationRefused, banLoopValidBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.priorValid {
				writeWithSyntaxChecker(t, dir, "mod.py", banLoopValidBody)
			}
			got, res := writeWithSyntaxChecker(t, dir, "mod.py", banLoopInvalidBody)

			if res.Success != tc.wantSuccess {
				t.Errorf("Success = %v, want %v: %+v", res.Success, tc.wantSuccess, res)
			}
			if res.MutationStatus != tc.wantMutation {
				t.Errorf("MutationStatus = %q, want %q", res.MutationStatus, tc.wantMutation)
			}
			if res.ValidationKind != ValidationKindSyntax || res.ValidationStatus != ValidationFailed {
				t.Errorf("validation = %q/%q, want syntax/failed",
					res.ValidationKind, res.ValidationStatus)
			}
			if res.ValidationDetail == "" {
				t.Error("ValidationDetail must carry the real diagnostic")
			}
			if got != tc.wantOnDisk {
				t.Errorf("bytes on disk = %q, want %q", got, tc.wantOnDisk)
			}
		})
	}
}

// writeWithSyntaxChecker sends one write_file call through executeToolCall,
// with a syntax checker that fails on "def alpha(n:", and returns the bytes
// on disk afterwards and the result.
func writeWithSyntaxChecker(t *testing.T, dir, rel, content string) (string, *ToolResult) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "def alpha(n:")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: invalid syntax (line 4)"}
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	ctx.SandboxURL = srv.URL
	args, _ := json.Marshal(map[string]string{"path": rel, "content": content})
	res := executeToolCall("write_file", args, ctx)
	b, _ := os.ReadFile(filepath.Join(dir, rel))
	return string(b), res
}

// --- every branch of every direct mutator ---------------------------------
//
// Every branch of the seven direct mutators that a session can reach, driven
// through the real boundary. A direct mutator owes a local classification
// (the boundary refuses to invent one), so an unclassified branch is a
// producer that has not spoken, and the ledger cannot tell it apart from one
// that did nothing on purpose. Each row states what the branch does to disk,
// and the test checks that claim against the filesystem rather than against
// the tool's own words.

func TestEveryDirectMutatorOutcomeIsClassified(t *testing.T) {
	oneLine := map[string]string{"m.py": "A = 1\n"}
	twoLines := map[string]string{"m.py": "A = 1\nB = 2\n"}
	hundredLines := map[string]string{"m.py": strings.Repeat("X = 0\n", 100)}
	functionSrc := "def f():\n    return 1\n"
	function := map[string]string{"m.py": functionSrc}
	spliced := "def f():\n    return 2\n"
	for _, c := range []struct {
		name string
		tool string
		// Files written before the call, and files to read_file first.
		seed map[string]string
		read []string
		args string
		// Runs last, after seeding, and may break the filesystem.
		setup func(t *testing.T, dir string)
		// Answers structural_edit's splice request when set.
		v3 func(w http.ResponseWriter, r *http.Request)

		wantMutation MutationStatus
		// The file this row is about, and its bytes afterwards. An empty
		// want means it must not exist.
		path string
		want string
		// A branch that exists in production but cannot be reached from a
		// test process, with the reason.
		skip string
	}{
		{name: "malformed args", tool: "write_file", args: `{"path":123}`,
			wantMutation: MutationNone},
		{name: "empty path", tool: "write_file", args: `{"path":" ","content":"x"}`,
			wantMutation: MutationNone},
		{name: "deny-list target", tool: "write_file",
			seed: map[string]string{".env": "S=1\n"}, args: `{"path":".env","content":"S=2\n"}`,
			wantMutation: MutationNone, path: ".env", want: "S=1\n"},
		{name: "escapes the workspace", tool: "write_file",
			args: `{"path":"../out.py","content":"A = 1\n"}`, wantMutation: MutationNone},
		{name: "echoed write", tool: "write_file",
			seed:         map[string]string{"m.py": strings.Repeat("A = 1\n", 40)},
			args:         `{"path":"m.py","content":"` + strings.Repeat("A = 1\\n", 40) + `"}`,
			wantMutation: MutationRefused, path: "m.py", want: strings.Repeat("A = 1\n", 40)},
		{name: "written", tool: "write_file",
			args:         `{"path":"m.py","content":"A = 1\n"}`,
			wantMutation: MutationApplied, path: "m.py", want: "A = 1\n"},
		{name: "malformed args", tool: "edit_file", args: `{"path":123}`,
			wantMutation: MutationNone},
		{name: "empty path", tool: "edit_file", args: `{"path":" ","old_str":"a","new_str":"b"}`,
			wantMutation: MutationNone},
		{name: "old_str empty", tool: "edit_file", seed: twoLines, read: []string{"m.py"},
			args:         `{"path":"m.py","old_str":"","new_str":"b"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "file not read", tool: "edit_file", seed: twoLines,
			args:         `{"path":"m.py","old_str":"A = 1","new_str":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "file vanished after the read", tool: "edit_file", seed: twoLines, read: []string{"m.py"},
			setup:        func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "m.py")) },
			args:         `{"path":"m.py","old_str":"A = 1","new_str":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py"},
		{name: "old_str absent", tool: "edit_file", seed: twoLines, read: []string{"m.py"},
			args:         `{"path":"m.py","old_str":"ZZZ","new_str":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "ambiguous match", tool: "edit_file",
			seed: map[string]string{"m.py": "A = 1\nA = 1\n"}, read: []string{"m.py"},
			args:         `{"path":"m.py","old_str":"A = 1","new_str":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nA = 1\n"},
		{name: "no-op edit", tool: "edit_file", seed: twoLines, read: []string{"m.py"},
			args:         `{"path":"m.py","old_str":"A = 1","new_str":"A = 1"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "applied", tool: "edit_file", seed: twoLines, read: []string{"m.py"},
			args:         `{"path":"m.py","old_str":"A = 1","new_str":"A = 9"}`,
			wantMutation: MutationApplied, path: "m.py", want: "A = 9\nB = 2\n"},
		{name: "malformed args", tool: "insert_after", args: `{"line":"x"}`,
			wantMutation: MutationNone},
		{name: "path missing", tool: "insert_after", args: `{"line":1,"content":"B = 2"}`,
			wantMutation: MutationNone},
		{name: "content empty", tool: "insert_after", seed: oneLine, read: []string{"m.py"},
			args:         `{"path":"m.py","line":1,"content":""}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\n"},
		{name: "file not read", tool: "insert_after", seed: oneLine,
			args:         `{"path":"m.py","line":1,"content":"B = 2"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\n"},
		{name: "file vanished after the read", tool: "insert_after", seed: oneLine, read: []string{"m.py"},
			setup:        func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "m.py")) },
			args:         `{"path":"m.py","line":1,"content":"B = 2"}`,
			wantMutation: MutationNone, path: "m.py"},
		{name: "line past end", tool: "insert_after", seed: oneLine, read: []string{"m.py"},
			args:         `{"path":"m.py","line":99,"content":"B = 2"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\n"},
		{name: "inserted", tool: "insert_after", seed: oneLine, read: []string{"m.py"},
			args:         `{"path":"m.py","line":1,"content":"B = 2"}`,
			wantMutation: MutationApplied, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "inserted at the top", tool: "insert_after", seed: oneLine, read: []string{"m.py"},
			args:         `{"path":"m.py","line":0,"content":"B = 2"}`,
			wantMutation: MutationApplied, path: "m.py", want: "B = 2\nA = 1\n"},
		{name: "malformed args", tool: "replace_lines", args: `{"start_line":"x"}`,
			wantMutation: MutationNone},
		{name: "path missing", tool: "replace_lines", args: `{"start_line":1,"end_line":1}`,
			wantMutation: MutationNone},
		{name: "file not read", tool: "replace_lines", seed: twoLines,
			args:         `{"path":"m.py","start_line":1,"end_line":1,"expected_first_line":"A = 1","expected_last_line":"A = 1","content":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "file vanished after the read", tool: "replace_lines", seed: twoLines, read: []string{"m.py"},
			setup:        func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "m.py")) },
			args:         `{"path":"m.py","start_line":1,"end_line":1,"expected_first_line":"A = 1","expected_last_line":"A = 1","content":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py"},
		{name: "range invalid", tool: "replace_lines", seed: twoLines, read: []string{"m.py"},
			args:         `{"path":"m.py","start_line":5,"end_line":9,"expected_first_line":"A = 1","expected_last_line":"A = 1","content":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "span too large", tool: "replace_lines", seed: hundredLines, read: []string{"m.py"},
			args:         `{"path":"m.py","start_line":1,"end_line":99,"expected_first_line":"X = 0","expected_last_line":"X = 0","content":"X = 1"}`,
			wantMutation: MutationNone, path: "m.py", want: strings.Repeat("X = 0\n", 100)},
		{name: "anchor mismatch", tool: "replace_lines", seed: twoLines, read: []string{"m.py"},
			args:         `{"path":"m.py","start_line":1,"end_line":1,"expected_first_line":"WRONG","expected_last_line":"WRONG","content":"A = 9"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "replacement identical", tool: "replace_lines", seed: twoLines, read: []string{"m.py"},
			args:         `{"path":"m.py","start_line":1,"end_line":1,"expected_first_line":"A = 1","expected_last_line":"A = 1","content":"A = 1"}`,
			wantMutation: MutationNone, path: "m.py", want: "A = 1\nB = 2\n"},
		{name: "replaced", tool: "replace_lines", seed: twoLines, read: []string{"m.py"},
			args:         `{"path":"m.py","start_line":1,"end_line":1,"expected_first_line":"A = 1","expected_last_line":"A = 1","content":"A = 9"}`,
			wantMutation: MutationApplied, path: "m.py", want: "A = 9\nB = 2\n"},
		{name: "malformed args", tool: "structural_edit", args: `{"path":123}`,
			wantMutation: MutationNone},
		{name: "empty path", tool: "structural_edit", args: `{"path":" ","selector":"function:f","content":"x"}`,
			wantMutation: MutationNone},
		{name: "empty selector", tool: "structural_edit", seed: function, read: []string{"m.py"},
			args:         `{"path":"m.py","selector":" ","content":"x"}`,
			wantMutation: MutationNone, path: "m.py", want: functionSrc},
		{name: "file not read", tool: "structural_edit", seed: function,
			args:         `{"path":"m.py","selector":"function:f","content":"def f():\n    return 2\n"}`,
			wantMutation: MutationNone, path: "m.py", want: functionSrc},
		{name: "empty content deletes the node", tool: "structural_edit", seed: function, read: []string{"m.py"},
			args:         `{"path":"m.py","selector":"function:f","content":"   "}`,
			wantMutation: MutationNone, path: "m.py", want: functionSrc},
		{name: "runaway content", tool: "structural_edit", seed: function, read: []string{"m.py"},
			args:         `{"path":"m.py","selector":"function:f","content":"` + strings.Repeat("x", 9000) + `"}`,
			wantMutation: MutationNone, path: "m.py", want: functionSrc},
		{name: "v3 unreachable", tool: "structural_edit", seed: function, read: []string{"m.py"},
			args:         `{"path":"m.py","selector":"function:f","content":"def f():\n    return 2\n"}`,
			wantMutation: MutationNone, path: "m.py", want: functionSrc},
		{name: "selector matched nothing", tool: "structural_edit", seed: function, read: []string{"m.py"},
			v3:           v3SpliceStub(t, "", false, "selector function:nope not found"),
			args:         `{"path":"m.py","selector":"function:nope","content":"def nope():\n    pass\n"}`,
			wantMutation: MutationNone, path: "m.py", want: functionSrc},
		{name: "splice is a no-op", tool: "structural_edit", seed: function, read: []string{"m.py"},
			v3:           v3SpliceStub(t, "\x00same", true, ""),
			args:         `{"path":"m.py","selector":"function:f","content":"def f():\n    return 1\n"}`,
			wantMutation: MutationNone, path: "m.py", want: functionSrc},
		{name: "spliced", tool: "structural_edit", seed: function, read: []string{"m.py"},
			v3:           v3SpliceStub(t, spliced, true, ""),
			args:         `{"path":"m.py","selector":"function:f","content":"def f():\n    return 2\n"}`,
			wantMutation: MutationApplied, path: "m.py", want: spliced},
		{name: "malformed args", tool: "delete_file", args: `{"path":123}`,
			wantMutation: MutationNone},
		{name: "empty path", tool: "delete_file", args: `{"path":"  "}`,
			wantMutation: MutationNone},
		{name: "target absent", tool: "delete_file", args: `{"path":"gone.py"}`,
			wantMutation: MutationNone, path: "gone.py"},
		{name: "directory not empty", tool: "delete_file",
			seed: map[string]string{"pkg/a.py": "A = 1\n"}, args: `{"path":"pkg"}`,
			wantMutation: MutationRefused, path: "pkg/a.py", want: "A = 1\n"},
		{name: "removal fails", tool: "delete_file",
			seed: map[string]string{"locked/a.py": "A = 1\n"}, args: `{"path":"locked/a.py"}`,
			setup: func(t *testing.T, dir string) {
				if err := os.Chmod(filepath.Join(dir, "locked"), 0o555); err != nil {
					t.Skip("cannot make a directory read-only here")
				}
				t.Cleanup(func() { os.Chmod(filepath.Join(dir, "locked"), 0o755) })
			},
			wantMutation: MutationFailed, path: "locked/a.py", want: "A = 1\n"},
		{name: "removed", tool: "delete_file",
			seed: map[string]string{"a.py": "A = 1\n"}, args: `{"path":"a.py"}`,
			wantMutation: MutationApplied, path: "a.py"},
		{name: "empty directory removed", tool: "delete_file",
			setup: func(t *testing.T, dir string) { os.Mkdir(filepath.Join(dir, "empty"), 0o755) },
			args:  `{"path":"empty"}`, wantMutation: MutationApplied, path: "empty"},
		{name: "malformed args", tool: "move_file", args: `{"source":123}`,
			wantMutation: MutationNone},
		{name: "missing destination", tool: "move_file", args: `{"source":"a.py","destination":" "}`,
			wantMutation: MutationNone},
		{name: "source absent", tool: "move_file", args: `{"source":"gone.py","destination":"new.py"}`,
			wantMutation: MutationNone, path: "new.py"},
		{name: "same path", tool: "move_file",
			seed: map[string]string{"a.py": "A = 1\n"}, args: `{"source":"a.py","destination":"./a.py"}`,
			wantMutation: MutationNone, path: "a.py", want: "A = 1\n"},
		{name: "destination occupied", tool: "move_file",
			seed:         map[string]string{"a.py": "A = 1\n", "b.py": "B = 2\n"},
			args:         `{"source":"a.py","destination":"b.py"}`,
			wantMutation: MutationRefused, path: "b.py", want: "B = 2\n"},
		{name: "cannot create destination dir", tool: "move_file",
			seed: map[string]string{"a.py": "A = 1\n", "ro/keep": "x"},
			setup: func(t *testing.T, dir string) {
				if err := os.Chmod(filepath.Join(dir, "ro"), 0o555); err != nil {
					t.Skip("cannot make a directory read-only here")
				}
				t.Cleanup(func() { os.Chmod(filepath.Join(dir, "ro"), 0o755) })
			},
			args:         `{"source":"a.py","destination":"ro/sub/a.py"}`,
			wantMutation: MutationFailed, path: "a.py", want: "A = 1\n"},
		{name: "renamed", tool: "move_file",
			seed: map[string]string{"a.py": "A = 1\n"}, args: `{"source":"a.py","destination":"new.py"}`,
			wantMutation: MutationApplied, path: "new.py", want: "A = 1\n"},
		{name: "moved into a directory", tool: "move_file",
			seed: map[string]string{"a.py": "A = 1\n"},
			setup: func(t *testing.T, dir string) {
				os.Mkdir(filepath.Join(dir, "sub"), 0o755)
			},
			args: `{"source":"a.py","destination":"sub"}`,
			// The tool resolves the directory to sub/a.py.
			wantMutation: MutationApplied, path: "sub/a.py", want: "A = 1\n"},
		{name: "cross-device copy path", tool: "move_file",
			skip: "os.Rename only fails across filesystems; one temp dir cannot straddle two"},
	} {
		t.Run(c.tool+"/"+c.name, func(t *testing.T) {
			if c.skip != "" {
				t.Skip(c.skip)
			}
			dir := t.TempDir()
			for f, body := range c.seed {
				full := filepath.Join(dir, f)
				os.MkdirAll(filepath.Dir(full), 0o755)
				if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ctx := ledgerToolCtx(t, dir)
			if c.v3 != nil {
				srv := httptest.NewServer(http.HandlerFunc(c.v3))
				defer srv.Close()
				ctx.V3URL = srv.URL
				ctx.SandboxURL = srv.URL
			}
			for _, f := range c.read {
				r, _ := json.Marshal(map[string]string{"path": f})
				executeToolCall("read_file", r, ctx)
			}
			if c.setup != nil {
				c.setup(t, dir)
			}

			res := executeToolCall(c.tool, json.RawMessage(c.args), ctx)

			if !res.Classified() {
				t.Errorf("unclassified branch: mutation=%q kind=%q status=%q err=%q",
					res.MutationStatus, res.ValidationKind, res.ValidationStatus, res.Error)
			}
			if res.MutationStatus != c.wantMutation {
				t.Errorf("MutationStatus = %q, want %q (err=%q)",
					res.MutationStatus, c.wantMutation, res.Error)
			}
			if c.path == "" {
				return
			}
			got, err := os.ReadFile(filepath.Join(dir, c.path))
			switch {
			case c.want == "" && err == nil:
				t.Errorf("%s still exists with %q", c.path, got)
			case c.want != "" && err != nil:
				t.Errorf("%s missing: %v", c.path, err)
			case c.want != "" && string(got) != c.want:
				t.Errorf("%s = %q, want %q", c.path, got, c.want)
			}
			// A pass must survive into the ledger for the bytes that are there.
			if d := ledgerOf(t, ctx, c.path); d != nil && res.ValidationStatus == ValidationPassed {
				if k, s := d.CurrentValidation(); s != ValidationPassed || k == ValidationKindUnknown {
					t.Errorf("a pass did not survive into the ledger: %v/%v", k, s)
				}
			}
		})
	}
}

// v3SpliceStub answers /internal/structural_edit by replacing the named
// function's body, and /syntax-check as valid, so structural_edit's branches
// are reachable without the real service.
func v3SpliceStub(t *testing.T, newContent string, ok bool, errMsg string) func(http.ResponseWriter, *http.Request) {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/internal/structural_edit"):
			var in struct{ Source, Selector, Content string }
			json.NewDecoder(r.Body).Decode(&in)
			body := map[string]interface{}{"success": ok, "error": errMsg,
				"language": "python", "old_size": len(in.Source), "new_size": len(newContent)}
			if ok {
				out := newContent
				if out == "\x00same" {
					out = in.Source
				}
				body["new_content"] = out
				body["new_size"] = len(out)
			}
			json.NewEncoder(w).Encode(body)
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}
}

// --- the bytes the next model turn receives --------------------------------
//
// Classification reaches the conversation, not the SSE stream, so this pins
// the request bodies the proxy sends to the inference endpoint: literally what
// the next turn is conditioned on. The fixture drives two classified branches,
// edit_file with an absent old_str (none) and move_file on a file that exists
// (applied), and no classification key may reach the prompt. Set
// ATLAS_PROMPT_BYTES to a file path to write the conversation out, so that a
// change can be read and the hash re-pinned on purpose.
const modelPromptBytesHash = "47e39feb5a8be623e1e1bdaf420dc1727f705b563dbd7436dad126cee98fa3ad"

// conversationBytes keeps every message except the system prompt, whose tool
// descriptions are rendered in Go map order and so differ between two runs of
// the same code. The scan for classification keys still covers the system
// prompt in full.
func conversationBytes(t *testing.T, prompts []string) string {
	t.Helper()
	var out strings.Builder
	for i, raw := range prompts {
		var req struct {
			Messages []AgentMessage `json:"messages"`
		}
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			t.Fatalf("request %d is not decodable: %v", i, err)
		}
		fmt.Fprintf(&out, "--- request %d ---\n", i)
		for _, m := range req.Messages {
			if m.Role == "system" {
				fmt.Fprintf(&out, "system <%d bytes elided>\n", len(m.Content))
				continue
			}
			fmt.Fprintf(&out, "%s|%s|%s|%s\n", m.Role, m.ToolName, m.ToolCallID, m.Content)
		}
	}
	return out.String()
}

func TestModelPromptBytesAreUnchangedByClassification(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "solve.py"), []byte("A = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	turn := 0
	var prompts []string
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
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		// The workspace path is baked into the prompt and differs per run.
		prompts = append(prompts, strings.ReplaceAll(string(body), dir, "<WORKSPACE>"))
		turn++
		n := turn
		mu.Unlock()

		var payload map[string]interface{}
		switch n {
		case 1:
			payload = map[string]interface{}{"type": "tool_call", "name": "read_file",
				"args": map[string]string{"path": "solve.py"}}
		case 2:
			// Classified as none.
			payload = map[string]interface{}{"type": "tool_call", "name": "edit_file",
				"args": map[string]string{"path": "solve.py",
					"old_str": "NOT PRESENT", "new_str": "B = 2"}}
		case 3:
			// Classified as applied.
			payload = map[string]interface{}{"type": "tool_call", "name": "move_file",
				"args": map[string]string{"source": "solve.py", "destination": "final.py"}}
		default:
			payload = map[string]interface{}{"type": "done", "summary": "renamed solve.py"}
		}
		call, _ := json.Marshal(payload)
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 8
	ctx.StreamFn = func(string, interface{}) {}

	if err := runAgentLoop(ctx, "Rename solve.py."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	mu.Lock()
	joined := strings.Join(prompts, "\n\x1e\n")
	conversation := conversationBytes(t, prompts)
	mu.Unlock()
	if len(prompts) < 3 {
		t.Fatalf("only %d model requests captured; the fixture did not reach "+
			"the branches it is meant to exercise", len(prompts))
	}

	// The direct claim, independent of the pinned hash: no classification key
	// reached the prompt on any turn.
	for _, key := range []string{"mutation_status", "validation_kind",
		"validation_status", "validation_detail"} {
		if strings.Contains(joined, key) {
			t.Errorf("%q reached the model prompt", key)
		}
	}
	// The tool messages are still there, in their plain shape.
	if !strings.Contains(joined, `string to replace not found in file`) {
		t.Error("the refused edit_file result never reached the prompt at all")
	}
	if !strings.Contains(joined, `{\"moved\":true`) {
		t.Error("the applied move_file result never reached the prompt")
	}

	if out := os.Getenv("ATLAS_PROMPT_BYTES"); out != "" {
		os.WriteFile(out, []byte(conversation), 0o600)
	}
	h := hashBytes([]byte(conversation))
	if h != modelPromptBytesHash {
		t.Errorf("conversation bytes changed (%s, want %s):\n%s",
			h[:12], modelPromptBytesHash[:12], conversation)
	}
	t.Logf("conversation: %d requests, %d bytes, sha256 %s",
		len(prompts), len(conversation), h)
}
