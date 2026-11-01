package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The direct new-file gate: a new file that is below Tier2 does not go to V3,
// and it does not skip validation either. It reaches this gate.

const smallOK = "A = 1\nB = 2\n"
const smallBad = writeRouteSyntaxErrorMark + "\n"

func TestNewFileRoute(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rel         string
		content     string
		noSandbox   bool // no sandbox is configured, so no check can run
		readOnlyDir bool // the folder is read-only, so the write itself fails

		wantSuccess     bool
		wantDisk        string
		wantMutation    MutationStatus
		wantKind        ValidationKind
		wantStatus      ValidationStatus
		wantSyntaxCalls int
		warns           bool // the result tells the model the file does not parse
	}{
		{name: "syntax passes",
			rel: "m.py", content: smallOK,
			wantSuccess: true, wantDisk: smallOK,
			wantMutation: MutationApplied, wantKind: ValidationKindSyntax, wantStatus: ValidationPassed,
			wantSyntaxCalls: 1},
		{name: "sandbox unavailable",
			rel: "m.py", content: smallOK, noSandbox: true,
			wantSuccess: true, wantDisk: smallOK,
			wantMutation: MutationApplied, wantKind: ValidationKindSyntax, wantStatus: ValidationNotRun,
			wantSyntaxCalls: 0},
		{name: "no check applies",
			rel: "notes.txt", content: "hello\n",
			wantSuccess: true, wantDisk: "hello\n",
			wantMutation: MutationApplied, wantKind: ValidationKindNone, wantStatus: ValidationNotApplicable,
			wantSyntaxCalls: 0},
		// A new file has nothing on disk to protect, so it lands with a
		// warning. The failure is not checked a second time.
		{name: "syntax fails, file lands with a warning",
			rel: "m.py", content: smallBad,
			wantSuccess: true, wantDisk: smallBad,
			wantMutation: MutationApplied, wantKind: ValidationKindSyntax, wantStatus: ValidationFailed,
			wantSyntaxCalls: 1, warns: true},
		// The check on these bytes still stands when the write fails.
		{name: "syntax passes, then the write fails",
			rel: "ro/new.py", content: smallOK, readOnlyDir: true,
			wantSuccess: false, wantDisk: "",
			wantMutation: MutationFailed, wantKind: ValidationKindSyntax, wantStatus: ValidationPassed,
			wantSyntaxCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			services := newWriteRouteServices(t, "", "")
			dir := t.TempDir()
			target := filepath.Join(dir, tc.rel)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.PermissionMode = PermissionYolo
			ctx.StreamFn = func(string, interface{}) {}
			// V3 is configured and the outputs are declared, so the tier
			// alone keeps V3 off.
			ctx.V3URL = services.URL
			ctx.TaskContract = declaredOutputs(tc.rel)
			if !tc.noSandbox {
				ctx.SandboxURL = services.URL
			}
			if tc.readOnlyDir {
				folder := filepath.Dir(target)
				if err := os.Chmod(folder, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(folder, 0o755) })
			}
			if tier := classifyFileTier(tc.rel, tc.content); tier >= Tier2Medium {
				t.Fatalf("the fixture must be below Tier2 to reach the direct gate, got %v", tier)
			}

			args, _ := json.Marshal(map[string]string{"path": tc.rel, "content": tc.content})
			res := executeToolCall("write_file", args, ctx)

			calls := services.calls()
			if len(calls.unexpected) > 0 {
				t.Fatalf("unexpected endpoints were called: %v", calls.unexpected)
			}
			if calls.v3 != 0 {
				t.Fatalf("V3 generation was reached (%d calls); wrong route", calls.v3)
			}
			if res.Success != tc.wantSuccess {
				t.Fatalf("success = %v, want %v (error %q)", res.Success, tc.wantSuccess, res.Error)
			}
			if !tc.wantSuccess {
				if _, err := os.Stat(target); err == nil {
					t.Error("the target exists despite the failed write")
				}
			}
			disk, _ := os.ReadFile(target)
			if string(disk) != tc.wantDisk {
				t.Errorf("disk holds %q, want %q", disk, tc.wantDisk)
			}
			entries, _ := os.ReadDir(filepath.Dir(target))
			for _, e := range entries {
				if strings.Contains(e.Name(), ".atlas.tmp") {
					t.Errorf("a temporary file survived: %s", e.Name())
				}
			}
			if res.MutationStatus != tc.wantMutation ||
				res.ValidationKind != tc.wantKind ||
				res.ValidationStatus != tc.wantStatus {
				t.Errorf("got %q/%q/%q, want %q/%q/%q",
					res.MutationStatus, res.ValidationKind, res.ValidationStatus,
					tc.wantMutation, tc.wantKind, tc.wantStatus)
			}
			if !res.Classified() {
				t.Error("result not fully classified")
			}
			if calls.syntax != tc.wantSyntaxCalls {
				t.Errorf("syntax-check calls = %d, want %d", calls.syntax, tc.wantSyntaxCalls)
			}
			warned := strings.Contains(string(res.Data), "does not parse")
			if warned != tc.warns {
				t.Errorf("the result warns that the file does not parse: %v, want %v (%s)",
					warned, tc.warns, res.Data)
			}
		})
	}
}

// Structural proof that the direct gate migrated and the V3 preflight did not.
func TestDirectNewFileGateUsesStructuredChecker(t *testing.T) {
	structured, legacy := syntaxCallCounts(t)
	if structured < 1 {
		t.Fatal("the direct new-file gate does not call fallbackSyntaxOutcomeFor")
	}
	// When this was written the direct new-file gate was the only migrated
	// route and the wrapper still had 14 callers. They are all gone now, which
	// is the end state this test was steering toward rather than a regression.
	if legacy != 0 {
		t.Errorf("legacy call sites remaining in the tool files = %d, want 0", legacy)
	}
	t.Logf("structured call sites=%d legacy call sites remaining=%d", structured, legacy)
}
