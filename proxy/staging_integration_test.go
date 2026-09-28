package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Candidate staging against the REAL executor process.
//
// Everything else about staging is pinned against a stub: faithful, fast, and
// unable to tell us whether the actual executor honours the contract. This
// runs the same `stageCandidate` against a live `sandbox/executor_server.py`,
// which is the only way to learn that its snapshot really is isolated, its
// `finally` really does delete, and its observation really does describe the
// tree either side.
//
// It needs a running executor, so it is environment-gated and skips loudly
// when one is absent -- never silently, and never counted as a pass. Two
// variables:
//
//	ATLAS_STAGING_SANDBOX_URL        base URL of a real executor (required)
//	ATLAS_STAGING_SANDBOX_WORKSPACE  the workspace it serves, which the proxy
//	                                 must see at the same path -- the same
//	                                 alignment production requires (required)
//	ATLAS_STAGING_SANDBOX_BASE       its WORKSPACE_BASE, so snapshot teardown
//	                                 can be inspected from outside (optional)
//
// `scripts/staging-integration.py` starts one, runs this, and tears it down;
// `python scripts/production-readiness.py --only staging-executor` is the
// enumerated way to run it, and reports `unavailable` rather than passing when
// the dependencies are missing.

// stagingIntegrationEnv is the gate. A missing executor is a skip with a
// reason that says exactly how to provide one.
//
// The workspace is required alongside the URL because proxy and sandbox must
// see the same one -- that alignment is a production invariant, and a test
// that pointed the proxy at a directory the executor cannot reach would be
// checking a shape nothing runs in.
func stagingIntegrationEnv(t *testing.T) (url, workspace string) {
	t.Helper()
	url = strings.TrimSpace(os.Getenv("ATLAS_STAGING_SANDBOX_URL"))
	workspace = strings.TrimSpace(os.Getenv("ATLAS_STAGING_SANDBOX_WORKSPACE"))
	if url == "" || workspace == "" {
		t.Skip("integration environment absent: set ATLAS_STAGING_SANDBOX_URL and " +
			"ATLAS_STAGING_SANDBOX_WORKSPACE to a running sandbox executor and the " +
			"workspace it serves, or run `python scripts/staging-integration.py`, " +
			"which starts one")
	}
	return url, workspace
}

// integrationWorld is a throwaway workspace with a file to be replaced and an
// input a badly-behaved command might touch. Both are re-read after every case
// to prove staging never reached them.
type integrationWorld struct {
	ctx *AgentContext
	req stagingRequest
}

const (
	integrationTarget   = "solve.py"
	integrationOriginal = "OLD = 1\n"
	integrationInput    = "7\n"
	integrationCode     = "print(7)\n"
)

// integrationSeed is what the executor's workspace holds before staging runs,
// and what it must still hold afterwards.
var integrationSeed = map[string]string{
	integrationTarget: integrationOriginal,
	"input.txt":       integrationInput,
	"baseline.py":     "BASE = 1\n",
}

func newIntegrationWorld(t *testing.T, url, workspace, code string) *integrationWorld {
	t.Helper()
	// The proxy's working directory IS the executor's workspace, as in
	// production. Seeded fresh so a previous case cannot explain this one.
	for name, body := range integrationSeed {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := NewAgentContext(workspace, Tier2Medium)
	ctx.SandboxURL = url
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-integration")
	return &integrationWorld{ctx: ctx, req: stagingRequest{
		WireVersion:    stagingWireVersion,
		CandidateBytes: code,
		Budget:         defaultStagingBudget(),
		Identity: stagingIdentity{
			RequestID:           "req-integration",
			InvocationID:        "req-integration:inv:1",
			CandidateInstanceID: "req-integration:inv:1:c",
			CandidateHash:       contentSHA256(code),
			TargetPath:          resolveAgentPath(ctx, integrationTarget),
			WorkspaceGeneration: 1,
			WorkspaceStateHash:  contentSHA256("ws"),
		},
	}}
}

func (w *integrationWorld) declare(commands ...string) {
	w.req.Commands = nil
	for i, c := range commands {
		w.req.Commands = append(w.req.Commands, stagingCommand{
			Text: c, Identity: contentSHA256(c),
			ObligationID: ObligationDeclaredCommand + ":" + contentSHA256(c)[:32],
			Index:        i, Count: len(commands),
		})
	}
}

// assertProductionWorkspaceUntouched is the invariant every case shares, and
// the reason staging is safe to run before anything is delivered.
func (w *integrationWorld) assertProductionWorkspaceUntouched(t *testing.T) {
	t.Helper()
	for name, want := range integrationSeed {
		got, err := os.ReadFile(resolveAgentPath(w.ctx, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("staging modified the production workspace: %s is now %q", name, string(got))
		}
	}
}

// --- one command, every way it can end ------------------------------------------

func TestRealExecutorClassifiesEveryStagedOutcome(t *testing.T) {
	url, workspace := stagingIntegrationEnv(t)
	for _, c := range []struct {
		name    string
		command string
		want    stagingCommandOutcome
	}{
		{"exact command pass", `python3 -c "import ast;ast.parse(open('solve.py').read())"`, stagingExitedZero},
		{"nonzero exit", `python3 -c "import sys;sys.exit(3)"`, stagingExitedNonZero},
		{"timeout", "sleep 120", stagingTimedOut},
		{"target mutation", "echo 'print(8)' > solve.py", stagingMutatedTarget},
		{"workspace mutation", "echo 9 > input.txt", stagingMutatedWorkspace},
		{"safety refusal", "rm -rf /", stagingRefused},
		{"backgrounding refusal", "sleep 30 &", stagingRefused},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newIntegrationWorld(t, url, workspace, integrationCode)
			w.req.Budget.PerCommandTimeoutSec = 5
			w.req.Budget.TotalTimeoutSec = 20
			w.declare(c.command)

			res, ok := stageCandidate(w.ctx, w.req)
			if !ok {
				t.Fatal("staging refused a well-formed request")
			}
			if valid, why := res.validateAgainst(w.req); !valid {
				t.Fatalf("the real executor produced an invalid result: %s", why)
			}
			got := res.Commands[0]
			if got.Outcome != c.want {
				t.Errorf("outcome %q, want %q (exit %d)", got.Outcome, c.want, got.ExitStatus)
			}
			if c.want == stagingExitedZero {
				if got.TargetHashBefore != contentSHA256(integrationCode) {
					t.Error("the real executor did not stage the exact candidate")
				}
				if len(res.authorizingOutcomes()) != 1 {
					t.Error("a clean pass authorized nothing")
				}
			} else if len(res.authorizingOutcomes()) != 0 {
				t.Errorf("%s authorized its obligation", c.name)
			}
			if !res.WorkspaceDestroyed {
				t.Error("the overlay was not reported destroyed")
			}
			w.assertProductionWorkspaceUntouched(t)
		})
	}
}

func TestRealExecutorRunIsCancellable(t *testing.T) {
	url, workspace := stagingIntegrationEnv(t)
	w := newIntegrationWorld(t, url, workspace, integrationCode)
	w.declare("sleep 30")
	cancelled, cancel := context.WithCancel(
		context.WithValue(context.Background(), requestIDKey, "req-integration"))
	w.ctx.Ctx = cancelled
	cancel()

	res, ok := stageCandidate(w.ctx, w.req)
	if !ok {
		t.Fatal("cancellation produced no result at all")
	}
	if res.Commands[0].Outcome != stagingCancelled {
		t.Errorf("outcome %q, want cancelled", res.Commands[0].Outcome)
	}
	if res.Complete {
		t.Error("a cancelled set claimed to be complete")
	}
	if len(res.authorizingOutcomes()) != 0 {
		t.Error("a cancelled run authorized")
	}
	w.assertProductionWorkspaceUntouched(t)
}

// --- sets and concurrency --------------------------------------------------------

func TestRealExecutorRunsTheWholeDeclaredSetInOrder(t *testing.T) {
	url, workspace := stagingIntegrationEnv(t)
	w := newIntegrationWorld(t, url, workspace, integrationCode)
	w.req.Budget.PerCommandTimeoutSec = 10
	w.declare(
		`python3 -c "assert open('solve.py').read().strip() == 'print(7)'"`,
		`python3 -c "import sys;sys.exit(1)"`,
		"test -f baseline.py",
	)

	res, ok := stageCandidate(w.ctx, w.req)
	if !ok || len(res.Commands) != 3 {
		t.Fatalf("%d results from a three-command set", len(res.Commands))
	}
	if !res.Complete {
		t.Error("three commands ran and the set is not complete")
	}
	for i, want := range []stagingCommandOutcome{
		stagingExitedZero, stagingExitedNonZero, stagingExitedZero,
	} {
		if res.Commands[i].Outcome != want {
			t.Errorf("command %d outcome %q, want %q", i, res.Commands[i].Outcome, want)
		}
		if res.Commands[i].ObligationID != w.req.Commands[i].ObligationID {
			t.Errorf("result %d names another command's obligation", i)
		}
	}
	// The one that failed leaves its own obligation owed, and takes neither of
	// the others down with it.
	if got := res.authorizingOutcomes(); len(got) != 2 {
		t.Errorf("authorizing %v, want the two that passed", got)
	}
	// Every command saw the same staged bytes: the second one's failure did
	// not disturb what the third was testing.
	for i, c := range res.Commands {
		if c.TargetHashBefore != contentSHA256(integrationCode) {
			t.Errorf("command %d ran against other bytes", i)
		}
	}
	w.assertProductionWorkspaceUntouched(t)
}

func TestRealExecutorKeepsConcurrentCandidatesIsolated(t *testing.T) {
	url, workspace := stagingIntegrationEnv(t)
	const candidates = 4
	var wg sync.WaitGroup
	outcomes := make([]stagingCommandOutcome, candidates)
	instances := make([]string, candidates)

	for i := 0; i < candidates; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := string(rune('0' + i))
			body := "MARKER = " + marker + "\n"
			w := newIntegrationWorld(t, url, workspace, body)
			w.req.Identity.CandidateInstanceID = "req-integration:inv:1:c" + marker
			w.req.Budget.PerCommandTimeoutSec = 15
			// Exits zero only if THIS candidate's bytes are what it sees.
			w.declare(`python3 -c "import sys;sys.exit(0 if open('solve.py').read()==` +
				`'MARKER = ` + marker + `\n' else 9)"`)
			res, _ := stageCandidate(w.ctx, w.req)
			if len(res.Commands) == 1 {
				outcomes[i] = res.Commands[0].Outcome
			}
			instances[i] = res.Identity.CandidateInstanceID
			w.assertProductionWorkspaceUntouched(t)
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i := 0; i < candidates; i++ {
		if outcomes[i] != stagingExitedZero {
			t.Errorf("candidate %d saw another candidate's bytes (outcome %q)", i, outcomes[i])
		}
		if seen[instances[i]] {
			t.Errorf("candidate %d shares an identity with another", i)
		}
		seen[instances[i]] = true
	}
	if len(seen) != candidates {
		t.Errorf("%d distinct candidate identities, want %d", len(seen), candidates)
	}
}

// --- teardown --------------------------------------------------------------------

// TestRealExecutorDestroysEveryStagingSnapshot inspects the executor's own
// base directory from outside. It needs ATLAS_STAGING_SANDBOX_BASE, so it
// skips separately from the rest: a remote executor is still worth testing
// even when its filesystem is not reachable here.
func TestRealExecutorDestroysEveryStagingSnapshot(t *testing.T) {
	url, workspace := stagingIntegrationEnv(t)
	base := strings.TrimSpace(os.Getenv("ATLAS_STAGING_SANDBOX_BASE"))
	if base == "" {
		t.Skip("set ATLAS_STAGING_SANDBOX_BASE to the executor's WORKSPACE_BASE to " +
			"inspect snapshot teardown from outside")
	}
	before, err := filepath.Glob(filepath.Join(base, "shell-*"))
	if err != nil {
		t.Fatal(err)
	}
	// One of each way a staged command can end, including the two that leave
	// the executor unwinding rather than returning normally.
	for _, cmd := range []string{
		"true", "false", "sleep 120", "echo 'print(8)' > solve.py", "echo 9 > input.txt",
	} {
		w := newIntegrationWorld(t, url, workspace, integrationCode)
		w.req.Budget.PerCommandTimeoutSec = 3
		w.req.Budget.TotalTimeoutSec = 10
		w.declare(cmd)
		if _, ok := stageCandidate(w.ctx, w.req); !ok {
			t.Fatalf("%q produced no result", cmd)
		}
		w.assertProductionWorkspaceUntouched(t)
	}
	after, err := filepath.Glob(filepath.Join(base, "shell-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("%d staging snapshots survived: %v", len(after)-len(before), after)
	}
}

// --- nothing about the candidate leaves the executor ------------------------------

func TestRealExecutorLeaksNoContent(t *testing.T) {
	url, workspace := stagingIntegrationEnv(t)
	const secret = "TOKEN = 'hunter2'\nprint(7)\n"
	w := newIntegrationWorld(t, url, workspace, secret)
	w.req.Budget.PerCommandTimeoutSec = 10
	// A command whose text, whose output and whose subject all carry something
	// that must not survive.
	w.declare("grep -c TOKEN solve.py")

	res, ok := stageCandidate(w.ctx, w.req)
	if !ok {
		t.Fatal("no result")
	}
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"hunter2", "TOKEN", "print(7)", "grep", secret,
	} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the staging result carries %q", needle)
		}
	}
	w.assertProductionWorkspaceUntouched(t)
}
