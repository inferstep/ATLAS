package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A declared command and file coverage are two obligations, not one.
//
// A declared command is met by a current run of exactly that command. A code
// file is met by a current run that covered its bytes. A command that names
// no file, such as `pytest` or `make check`, meets its own obligation and
// covers nothing. If the two were one check, such a command could never be
// met and the session could never reach the settlement gate.

func sepCtx(t *testing.T, tc *TaskContract) (*AgentContext, string) {
	t.Helper()
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = tc
	return ctx, dir
}

func sepWrite(t *testing.T, ctx *AgentContext, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx.SessionWrites[name] = true
	observeDeliverable(ctx, name, []byte(body), ValidationKindSyntax, ValidationPassed, "")
	return fileSHA256(ctx, name)
}

func sepContract(outputs, commands []string) *TaskContract {
	o := append([]string{}, outputs...)
	c := append([]string{}, commands...)
	return &TaskContract{
		TaskMode:              TaskModeWork,
		OutputKnowledge:       KnowledgeDeclared,
		ExpectedOutputs:       &o,
		VerificationKnowledge: KnowledgeDeclared,
		Verification:          &c,
	}
}

// sepStamp is the record the agent loop keeps after a command has run and
// the ledger holds whatever that command changed.
func sepStamp(ctx *AgentContext, command string, covered map[string]string) VerificationRecord {
	gen, hash := workspaceIdentity(ctx)
	return VerificationRecord{
		Command: command, Covered: covered,
		WorkspaceGeneration: gen, WorkspaceStateHash: hash,
	}
}

func TestDeclaredCommandsAndFileCoverageAreSeparateObligations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outputs  []string
		commands []string
		// evidence writes the files and returns the records the session holds.
		evidence func(t *testing.T, ctx *AgentContext, dir string) []VerificationRecord
		wantMet  bool
		// When the demand is not met, one of these names what is missing.
		wantMissingCommand string
		wantMissingFile    string
	}{
		{
			name:    "a pathless command alone leaves the file uncovered",
			outputs: []string{"solve.py"}, commands: []string{"pytest"},
			evidence: func(t *testing.T, ctx *AgentContext, dir string) []VerificationRecord {
				sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
				return []VerificationRecord{sepStamp(ctx, "pytest", nil)}
			},
			wantMissingFile: "solve.py",
		},
		{
			name:    "a pathless command plus a covering run meets both",
			outputs: []string{"solve.py"}, commands: []string{"pytest"},
			evidence: func(t *testing.T, ctx *AgentContext, dir string) []VerificationRecord {
				h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
				return []VerificationRecord{
					sepStamp(ctx, "pytest", nil),
					sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
				}
			},
			wantMet: true,
		},
		{
			name:    "one record that is the command and covers the file meets both",
			outputs: []string{"solve.py"}, commands: []string{"python3 solve.py"},
			evidence: func(t *testing.T, ctx *AgentContext, dir string) []VerificationRecord {
				h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
				return []VerificationRecord{
					sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
				}
			},
			wantMet: true,
		},
		{
			name:    "a command run before a change to the file is stale",
			outputs: []string{"solve.py"}, commands: []string{"pytest", "make check"},
			evidence: func(t *testing.T, ctx *AgentContext, dir string) []VerificationRecord {
				sepWrite(t, ctx, dir, "solve.py", "print('one')\n")
				before := sepStamp(ctx, "pytest", nil)
				h := sepWrite(t, ctx, dir, "solve.py", "print('two')\n")
				return []VerificationRecord{
					before,
					sepStamp(ctx, "make check", nil),
					sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
				}
			},
			wantMissingCommand: "pytest",
		},
		{
			// notes.txt is not code, so it adds no coverage demand of its
			// own: only the stale command can be what is missing.
			name:    "a later change to an unrelated file stales a pathless command",
			outputs: []string{"solve.py"}, commands: []string{"pytest"},
			evidence: func(t *testing.T, ctx *AgentContext, dir string) []VerificationRecord {
				h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
				records := []VerificationRecord{
					sepStamp(ctx, "pytest", nil),
					sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
				}
				sepWrite(t, ctx, dir, "notes.txt", "x = 1\n")
				return records
			},
			wantMissingCommand: "pytest",
		},
		{
			name:    "a stamp from another workspace does not count",
			outputs: []string{"solve.py"}, commands: []string{"pytest"},
			evidence: func(t *testing.T, ctx *AgentContext, dir string) []VerificationRecord {
				h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
				return []VerificationRecord{
					{Command: "pytest", WorkspaceGeneration: 99, WorkspaceStateHash: contentSHA256("another session")},
					sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
				}
			},
			wantMissingCommand: "pytest",
		},
		{
			name:    "a record with no workspace stamp does not count",
			outputs: []string{"solve.py"}, commands: []string{"pytest"},
			evidence: func(t *testing.T, ctx *AgentContext, dir string) []VerificationRecord {
				h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
				return []VerificationRecord{
					{Command: "pytest"},
					sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
				}
			},
			wantMissingCommand: "pytest",
		},
		{
			name:    "commands match byte for byte, so two spaces is another command",
			outputs: []string{"solve.py"}, commands: []string{"python3  solve.py"},
			evidence: func(t *testing.T, ctx *AgentContext, dir string) []VerificationRecord {
				h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
				return []VerificationRecord{
					sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
				}
			},
			wantMissingCommand: "python3  solve.py",
		},
		{
			name:    "every declared command is required",
			outputs: []string{"solve.py"}, commands: []string{"pytest", "make check"},
			evidence: func(t *testing.T, ctx *AgentContext, dir string) []VerificationRecord {
				h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
				return []VerificationRecord{
					sepStamp(ctx, "pytest", nil),
					sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
				}
			},
			wantMissingCommand: "make check",
		},
		{
			name:    "a command declared twice is met by one run",
			outputs: []string{"solve.py"}, commands: []string{"pytest", "pytest"},
			evidence: func(t *testing.T, ctx *AgentContext, dir string) []VerificationRecord {
				h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
				return []VerificationRecord{
					sepStamp(ctx, "pytest", nil),
					sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
				}
			},
			wantMet: true,
		},
		{
			name:    "every declared output needs its own coverage",
			outputs: []string{"a.py", "b.py"}, commands: []string{"pytest"},
			evidence: func(t *testing.T, ctx *AgentContext, dir string) []VerificationRecord {
				ha := sepWrite(t, ctx, dir, "a.py", "print('a')\n")
				sepWrite(t, ctx, dir, "b.py", "print('b')\n")
				return []VerificationRecord{
					sepStamp(ctx, "pytest", nil),
					sepStamp(ctx, "python3 a.py", map[string]string{"a.py": ha}),
				}
			},
			wantMissingFile: "b.py",
		},
		{
			name:    "coverage of another file does not cover the output",
			outputs: []string{"solve.py"}, commands: []string{"pytest"},
			evidence: func(t *testing.T, ctx *AgentContext, dir string) []VerificationRecord {
				sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")
				other := sepWrite(t, ctx, dir, "other.py", "print('other')\n")
				return []VerificationRecord{
					sepStamp(ctx, "pytest", nil),
					sepStamp(ctx, "python3 other.py", map[string]string{"other.py": other}),
				}
			},
			wantMissingFile: "solve.py",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contract := sepContract(tc.outputs, tc.commands)
			ctx, dir := sepCtx(t, contract)
			ctx.VerificationEvidence = tc.evidence(t, ctx, dir)

			d := decideVerificationDemand(ctx, contract, tc.outputs)

			wantMissing := tc.wantMissingCommand
			if tc.wantMissingFile != "" {
				wantMissing = filepath.Join(dir, tc.wantMissingFile)
			}
			if !d.Required || d.Met != tc.wantMet || d.Missing != wantMissing {
				t.Fatalf("got %+v, want Required, Met=%v, Missing=%q", d, tc.wantMet, wantMissing)
			}
		})
	}
}

// A command that names no file covers nothing, and still meets its own
// declared obligation.
func TestOpaqueCommandsSatisfyTheirOwnObligationWithoutPathInvention(t *testing.T) {
	for _, cmd := range []string{"pytest", "make check", "./scripts/verify.sh"} {
		t.Run(cmd, func(t *testing.T) {
			tc := sepContract([]string{"solve.py"}, []string{cmd})
			ctx, dir := sepCtx(t, tc)
			h := sepWrite(t, ctx, dir, "solve.py", "print('ok')\n")

			// The same coverage the agent loop computes for a green command.
			covered := coverageForGreenCommand(ctx, cmd)
			if len(covered) != 0 {
				t.Fatalf("%q produced coverage out of nowhere: %v", cmd, covered)
			}
			ctx.VerificationEvidence = []VerificationRecord{
				sepStamp(ctx, cmd, covered),
				sepStamp(ctx, "python3 solve.py", map[string]string{"solve.py": h}),
			}
			if d := decideVerificationDemand(ctx, tc, []string{"solve.py"}); !d.Met {
				t.Fatalf("%q: %+v", cmd, d)
			}
		})
	}
}

// A run is verification only when it ran to its own end and exited zero.
func TestATimedOutCommandIsRejectedStructurally(t *testing.T) {
	if runCommandVerifiable(RunCommandOutput{ExitCode: 0, TimedOut: true}) {
		t.Fatal("a timed-out run was treated as verification")
	}
	if !runCommandVerifiable(RunCommandOutput{ExitCode: 0}) {
		t.Fatal("a clean run was refused")
	}
	if runCommandVerifiable(RunCommandOutput{ExitCode: 1}) {
		t.Fatal("a nonzero exit was treated as verification")
	}
}

func TestAMutatingCommandIsStampedWithThePostCommandState(t *testing.T) {
	tc := sepContract([]string{"solve.py"}, []string{"./build.sh"})
	ctx, dir := sepCtx(t, tc)
	sepWrite(t, ctx, dir, "solve.py", "print('one')\n")
	beforeGen, beforeState := workspaceIdentity(ctx)

	// The command rewrites a tracked file outside the tool layer. This is
	// why a shell effect is reconciled with the ledger rather than trusted.
	if err := os.WriteFile(filepath.Join(dir, "solve.py"), []byte("print('two')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordLedgerEffect("run_command", []byte(`{"command":"./build.sh"}`), ctx,
		&ToolResult{Success: true})

	afterGen, afterState := workspaceIdentity(ctx)
	if afterGen == beforeGen && afterState == beforeState {
		t.Fatal("the command's own effect never reached the ledger; a stamp taken " +
			"now would certify the state the command found, not the one it left")
	}
	rec := sepStamp(ctx, "./build.sh", nil)
	ctx.VerificationEvidence = []VerificationRecord{rec}
	if !commandEvidenceCurrent(ctx, rec) {
		t.Fatal("the post-command stamp is not current against the post-command state")
	}
	stale := VerificationRecord{Command: "./build.sh",
		WorkspaceGeneration: beforeGen, WorkspaceStateHash: beforeState}
	if commandEvidenceCurrent(ctx, stale) {
		t.Fatal("a pre-command stamp was accepted as current")
	}
}

func TestTheSandboxTimeoutFlagIsDecoded(t *testing.T) {
	body := `{"success":false,"stdout":"","stderr":"Execution timed out after 5s","exit_code":-1,"elapsed_ms":5000,"timed_out":true}`
	var out RunCommandOutput
	if err := decodeShellResponse(strings.NewReader(body), &out); err != nil {
		t.Fatal(err)
	}
	if !out.TimedOut {
		t.Fatal("timed_out was not decoded; the state would have to be read out of stderr prose")
	}
}
