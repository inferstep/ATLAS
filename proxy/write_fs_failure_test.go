package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two distinct filesystem failures through the real write_file dispatch.
// writeFileDirect writes `path + ".atlas.tmp"` then renames it over `path`,
// and returns (nil, err) on either failure -- so no ToolResult is built at
// the mutation site. One is synthesized generically by executeToolCall's
// error branch, which is why both cases arrive at the boundary UNCLASSIFIED.
//
// These fixtures pin present behavior. They deliberately do NOT assert a
// classification: the generic error branch cannot know whether a mutation
// began, so classifying there would be a guess. The correct owner is
// writeFileDirect, which knows which operation failed -- recorded as a
// finding rather than changed here.
func TestWriteFileFilesystemFailures(t *testing.T) {
	t.Run("temp-write-fails", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "ro")
		os.Mkdir(sub, 0o755)
		target := filepath.Join(sub, "f.py")
		prior := "A = 1\n"
		os.WriteFile(target, []byte(prior), 0o644)
		os.Chmod(sub, 0o555) // read-only dir: temp create must fail
		defer os.Chmod(sub, 0o755)

		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.PermissionMode = PermissionYolo
		ctx.StreamFn = func(string, interface{}) {}
		ctx.SessionWrites["ro/f.py"] = true
		args, _ := json.Marshal(map[string]string{"path": "ro/f.py", "content": "B = 2\n"})
		res := executeToolCall("write_file", args, ctx)

		after, _ := os.ReadFile(target)
		ents, _ := os.ReadDir(sub)
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		if res.Success {
			t.Fatal("a failed temp write must not report success")
		}
		if !strings.Contains(res.Error, "cannot write") {
			t.Fatalf("failure did not occur at the temp write: %q", res.Error)
		}
		if string(after) != prior {
			t.Fatalf("prior destination bytes changed: %q", string(after))
		}
		for _, n := range names {
			if strings.Contains(n, ".atlas.tmp") {
				t.Errorf("temporary artifact survived: %s", n)
			}
		}
		assertFailedMutation(t, res, ValidationKindSyntax, ValidationNotRun)
	})

	t.Run("rename-fails", func(t *testing.T) {
		dir := t.TempDir()
		// Destination is a NON-EMPTY DIRECTORY: temp write succeeds, rename fails.
		target := filepath.Join(dir, "f.py")
		os.Mkdir(target, 0o755)
		os.WriteFile(filepath.Join(target, "keep.txt"), []byte("x"), 0o644)

		ctx := NewAgentContext(dir, Tier2Medium)
		ctx.PermissionMode = PermissionYolo
		ctx.StreamFn = func(string, interface{}) {}
		ctx.SessionWrites["f.py"] = true
		args, _ := json.Marshal(map[string]string{"path": "f.py", "content": "B = 2\n"})
		res := executeToolCall("write_file", args, ctx)

		ents, _ := os.ReadDir(dir)
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		_, keepErr := os.Stat(filepath.Join(target, "keep.txt"))
		if res.Success {
			t.Fatal("a failed rename must not report success")
		}
		if !strings.Contains(res.Error, "cannot rename") {
			t.Fatalf("failure did not occur at the rename: %q", res.Error)
		}
		if keepErr != nil {
			t.Error("prior destination contents were destroyed")
		}
		for _, n := range names {
			if strings.Contains(n, ".atlas.tmp") {
				t.Errorf("temporary artifact survived a failed rename: %s", n)
			}
		}
		assertFailedMutation(t, res, ValidationKindSyntax, ValidationNotRun)
	})
}

func assertFailedMutation(t *testing.T, res *ToolResult, kind ValidationKind, status ValidationStatus) {
	t.Helper()
	if res.MutationStatus != MutationFailed {
		t.Errorf("MutationStatus = %q, want failed", res.MutationStatus)
	}
	if res.MutationStatus.Applied() {
		t.Error("a failed mutation must never read as applied")
	}
	if res.ValidationKind != kind || res.ValidationStatus != status {
		t.Errorf("validation = %q/%q, want %q/%q",
			res.ValidationKind, res.ValidationStatus, kind, status)
	}
	if res.ValidationStatus.Passed() {
		t.Error("a failed write must never claim validation passed")
	}
	if !res.Classified() {
		t.Errorf("result not fully classified: %+v", res)
	}
}

// A path whose parent component is a regular file is refused by workspace
// path validation BEFORE the handler runs, so writeFileDirect never executes
// and no mutation is attempted. MutationNone is correct here; calling it a
// failed mutation would claim an attempt that never happened.
func TestParentDirPathIsRefusedPreDispatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	args, _ := json.Marshal(map[string]string{
		"path": "blocker/sub/f.py", "content": "A = 1\n"})
	res := executeToolCall("write_file", args, ctx)

	if res.Success {
		t.Fatal("expected a pre-dispatch refusal")
	}
	if res.MutationStatus != MutationNone {
		t.Errorf("MutationStatus = %q, want none (no handler ran)", res.MutationStatus)
	}
	if res.MutationStatus.Applied() {
		t.Error("a pre-dispatch refusal must never read as applied")
	}
	if !res.Classified() {
		t.Errorf("pre-dispatch refusal not classified: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "blocker", "sub")); err == nil {
		t.Error("a directory was created despite the refusal")
	}
}

// Producer-level proof for the MkdirAll site. Dispatch refuses this shape
// earlier (see above), so the branch is exercised at the layer that owns the
// evidence. Transport of the same carrier is proven independently by the
// temp-write and rename tests, which do run end-to-end.
func TestWriteFileDirectParentDirFailureCarriesClassification(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(blocker, "sub", "f.py")

	res, err := writeFileDirect(target, "A = 1\n")
	if res != nil {
		t.Fatalf("writeFileDirect must keep its (nil, err) contract, got %+v", res)
	}
	if err == nil {
		t.Fatal("expected an error when the parent component is a regular file")
	}
	if !strings.Contains(err.Error(), "cannot create parent dir") {
		t.Fatalf("failure did not occur at parent-dir creation: %v", err)
	}
	var ce *classifiedError
	if !errors.As(err, &ce) {
		t.Fatalf("error does not carry classification: %v", err)
	}
	if ce.mutationStatus != MutationFailed {
		t.Errorf("mutationStatus = %q, want failed", ce.mutationStatus)
	}
	if ce.validationKind != ValidationKindSyntax || ce.validationStatus != ValidationNotRun {
		t.Errorf("validation = %q/%q, want syntax/not_run",
			ce.validationKind, ce.validationStatus)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Error("target exists after a failed parent-dir creation")
	}
	if _, statErr := os.Stat(target + ".atlas.tmp"); statErr == nil {
		t.Error("temporary artifact exists after a failed parent-dir creation")
	}
}

// V3 callers branch on err != nil. A failed write must stay an error there so
// success metadata can never be attached to it.
func TestFailedWriteStaysAnErrorForV3Callers(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	os.WriteFile(blocker, []byte("x"), 0o644)

	res, err := writeFileDirect(filepath.Join(blocker, "sub", "f.py"), "A = 1\n")
	// This is the exact shape every caller tests: `if err != nil || res == nil
	// || !res.Success`. All three must hold so no caller can proceed.
	if err == nil || res != nil {
		t.Fatalf("a failed write must surface as (nil, err); got res=%+v err=%v", res, err)
	}
	var ce *classifiedError
	if errors.As(err, &ce) && ce.mutationStatus.Applied() {
		t.Fatal("a failed write must never carry an applied mutation status")
	}
}

// A generic (untyped) error from a read-only tool must stay Unknown-derived:
// the boundary classifies it by EFFECT (none), never as MutationFailed.
func TestUntypedErrorDoesNotBecomeFailedMutation(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	args, _ := json.Marshal(map[string]string{"path": "nope.txt"})
	res := executeToolCall("read_file", args, ctx)
	if res.MutationStatus == MutationFailed {
		t.Fatal("a read-only tool's failure must never be MutationFailed")
	}
	if res.MutationStatus != MutationNone {
		t.Errorf("MutationStatus = %q, want none (effect-derived)", res.MutationStatus)
	}
}
