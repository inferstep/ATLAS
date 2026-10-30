package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Staging a candidate and running the client's declared commands against it.
//
// The executor here is a stub, and it is a faithful one: it keeps an actual
// overlay, applies the effects the scripted command declares, and reports the
// hashes it observes either side. The real executor's own behaviour --
// snapshot, overlay, delete, observe, timeout -- is pinned separately against
// the real module in tests/infrastructure/test_candidate_staging_observation.py.

// stubEffect is what a scripted command does to the overlay when it runs.
type stubEffect struct {
	// WriteTarget replaces the staged candidate.
	WriteTarget string
	// WriteOther changes some other file in the workspace.
	WriteOther string
	ExitCode   int
	TimedOut   bool
	// Outcome overrides how the executor says the command ended. Empty means
	// the ordinary one: completed, or timed out when TimedOut is set.
	Outcome ExecutionOutcome
	// Truncated makes the executor say it could not describe the workspace.
	Truncated bool
	// HTTPStatus, when non-zero, is returned instead of a result.
	HTTPStatus int
	// SleepMS holds the executor, to burn the set's total budget.
	SleepMS int
	// StagedAs overrides what the overlay is observed to contain BEFORE the
	// command, for the case where the overlay did not take.
	StagedAs string
}

// outcome is the executor's own answer about HOW the command ended. A stub that
// omitted it would be an executor too old to speak the vocabulary, which now
// fails closed -- correct in production, and not what any of these fixtures are
// about.
func (e stubEffect) outcome() ExecutionOutcome {
	if e.Outcome != "" {
		return e.Outcome
	}
	if e.TimedOut {
		return ExecutionTimedOut
	}
	return ExecutionCompleted
}

// observation is what the executor reports about the observed path, either
// side of the command, when the overlay handed it the staged bytes.
func (e stubEffect) observation(observed, staged string) map[string]interface{} {
	if e.StagedAs != "" {
		staged = e.StagedAs
	}
	before := contentSHA256(staged)
	after := before
	if e.WriteTarget != "" {
		after = contentSHA256(e.WriteTarget)
	}
	other := "baseline"
	wsBefore := contentSHA256(observed + "\x00" + before + "\n" + other)
	if e.WriteOther != "" {
		other = e.WriteOther
	}
	wsAfter := contentSHA256(observed + "\x00" + after + "\n" + other)
	return map[string]interface{}{
		"target_before":    map[string]string{observed: before},
		"target_after":     map[string]string{observed: after},
		"workspace_before": wsBefore,
		"workspace_after":  wsAfter,
		"workspace_files":  2,
		"digest_truncated": e.Truncated,
	}
}

type stubSandbox struct {
	srv *httptest.Server
	mu  sync.Mutex
	// effects, keyed by command text.
	effects map[string]stubEffect
	// seen records the commands the executor was asked to run, in order.
	seen []string
	// overlaysDestroyed counts teardown, one per request.
	overlaysDestroyed int
	// staged is the overlay bytes each command was handed, keyed by command.
	// It is what proves a run happened against the candidate rather than
	// against whatever was on disk.
	staged map[string]string
}

// runsOf is how many times the executor was asked to run this exact command.
func (s *stubSandbox) runsOf(command string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, seen := range s.seen {
		if seen == command {
			n++
		}
	}
	return n
}

// stagedBytes is the overlay content this command last ran against.
func (s *stubSandbox) stagedBytes(command string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.staged[command]
}

func newStubSandbox(t *testing.T) *stubSandbox {
	t.Helper()
	s := &stubSandbox{effects: map[string]stubEffect{}, staged: map[string]string{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/shell") {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Command      string            `json:"command"`
			Files        map[string]string `json:"files"`
			ObservePaths []string          `json:"observe_paths"`
			Timeout      int               `json:"timeout"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		effect := s.effects[in.Command]
		s.seen = append(s.seen, in.Command)
		s.mu.Unlock()

		if effect.SleepMS != 0 {
			time.Sleep(time.Duration(effect.SleepMS) * time.Millisecond)
		}
		if effect.HTTPStatus != 0 {
			http.Error(w, "refused", effect.HTTPStatus)
			return
		}
		if len(in.ObservePaths) == 0 || len(in.Files) == 0 {
			http.Error(w, "staging requires an overlay and an observation", http.StatusBadRequest)
			return
		}
		observed := in.ObservePaths[0]
		staged := in.Files[observed]
		s.mu.Lock()
		s.staged[in.Command] = staged
		s.mu.Unlock()

		s.mu.Lock()
		s.overlaysDestroyed++
		s.mu.Unlock()

		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":     effect.ExitCode == 0 && !effect.TimedOut,
			"stdout":      "SECRET_OUTPUT hunter2",
			"stderr":      "SECRET_DIAGNOSTIC hunter2",
			"exit_code":   effect.ExitCode,
			"elapsed_ms":  1,
			"timed_out":   effect.TimedOut,
			"outcome":     string(effect.outcome()),
			"observation": effect.observation(observed, staged),
		})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubSandbox) script(command string, effect stubEffect) { s.effects[command] = effect }

func (s *stubSandbox) commandsSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.seen...)
}

const stagedCode = "print(7)\n"

func stagingCtx(t *testing.T, stub *stubSandbox) *AgentContext {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "solve.py"), []byte("OLD = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = stub.srv.URL
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-stage")
	return ctx
}

func liveStagingReq(t *testing.T, ctx *AgentContext, commands ...string) stagingRequest {
	t.Helper()
	target := resolveAgentPath(ctx, "solve.py")
	req := stagingRequest{
		WireVersion:    stagingWireVersion,
		CandidateBytes: stagedCode,
		Budget:         defaultStagingBudget(),
		Identity: stagingIdentity{
			RequestID: "req-stage", InvocationID: "req-stage:inv:1",
			CandidateInstanceID: "req-stage:inv:1:cand",
			CandidateHash:       contentSHA256(stagedCode),
			TargetPath:          target,
			WorkspaceGeneration: 1,
			WorkspaceStateHash:  contentSHA256("ws"),
		},
	}
	for i, text := range commands {
		req.Commands = append(req.Commands, stagingCommand{
			Text: text, Identity: contentSHA256(text),
			ObligationID: ObligationDeclaredCommand + ":" + contentSHA256(text)[:32],
			Index:        i, Count: len(commands),
		})
	}
	if ok, why := req.validate(); !ok {
		t.Fatalf("fixture request is invalid: %s", why)
	}
	return req
}

// --- the passing case ----------------------------------------------------------

func TestAnExactCommandPassesAgainstTheExactCandidate(t *testing.T) {
	stub := newStubSandbox(t)
	stub.script("pytest -q", stubEffect{ExitCode: 0})
	ctx := stagingCtx(t, stub)
	req := liveStagingReq(t, ctx, "pytest -q")

	res, ok := stageCandidate(ctx, req)
	if !ok {
		t.Fatal("staging refused a well-formed request")
	}
	if valid, why := res.validateAgainst(req); !valid {
		t.Fatalf("staging produced an invalid result: %s", why)
	}
	if len(res.Commands) != 1 || res.Commands[0].Outcome != stagingExitedZero {
		t.Fatalf("outcome %+v", res.Commands)
	}
	if !res.Complete {
		t.Error("a whole set that ran is not complete")
	}
	if !res.WorkspaceDestroyed {
		t.Error("the overlay was not reported destroyed")
	}
	if got := res.authorizingOutcomes(); len(got) != 1 {
		t.Errorf("authorizing %v, want the one command", got)
	}
	// The staged bytes were the candidate, and the executor saw them.
	if res.Commands[0].TargetHashBefore != contentSHA256(stagedCode) {
		t.Error("the command did not run against the candidate")
	}
}

// --- every way it must refuse ---------------------------------------------------

func TestAFailingCommandAuthorizesNothing(t *testing.T) {
	stub := newStubSandbox(t)
	stub.script("pytest -q", stubEffect{ExitCode: 1})
	ctx := stagingCtx(t, stub)
	req := liveStagingReq(t, ctx, "pytest -q")
	res, _ := stageCandidate(ctx, req)
	if res.Commands[0].Outcome != stagingExitedNonZero {
		t.Errorf("outcome %q, want exited_nonzero", res.Commands[0].Outcome)
	}
	if res.Commands[0].ExitStatus != 1 {
		t.Errorf("exit status %d", res.Commands[0].ExitStatus)
	}
	if got := res.authorizingOutcomes(); len(got) != 0 {
		t.Errorf("a failing command authorized %v", got)
	}
}

func TestACommandThatRewritesTheCandidateAuthorizesNothing(t *testing.T) {
	stub := newStubSandbox(t)
	stub.script("pytest -q", stubEffect{ExitCode: 0, WriteTarget: "print(8)\n"})
	ctx := stagingCtx(t, stub)
	req := liveStagingReq(t, ctx, "pytest -q")
	res, _ := stageCandidate(ctx, req)
	if res.Commands[0].Outcome != stagingMutatedTarget {
		t.Errorf("outcome %q, want mutated_target", res.Commands[0].Outcome)
	}
	if !res.Commands[0].MutatedTarget {
		t.Error("the mutation was not recorded")
	}
	if len(res.authorizingOutcomes()) != 0 {
		t.Error("a command that rewrote its own subject authorized")
	}
	if valid, why := res.validateAgainst(req); !valid {
		t.Errorf("the mutation result is internally inconsistent: %s", why)
	}
}

// A command that reached the executor, but whose run shows nothing about the
// candidate, gets its own outcome and authorizes nothing.
func TestARunThatProvesNothingAuthorizesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		effect stubEffect
		want   stagingCommandOutcome
	}{
		{"it times out", stubEffect{ExitCode: -1, TimedOut: true}, stagingTimedOut},
		{"it changes an input", stubEffect{ExitCode: 0, WriteOther: "rewritten"}, stagingMutatedWorkspace},
		{"the workspace cannot be described", stubEffect{ExitCode: 0, Truncated: true}, stagingUnobservable},
		// The overlay did not take: the target holds something else.
		{"the staged bytes are not the candidate", stubEffect{ExitCode: 0, StagedAs: "SOMETHING ELSE\n"}, stagingUnobservable},
		{"the executor refuses the request", stubEffect{HTTPStatus: http.StatusBadRequest}, stagingUnobservable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubSandbox(t)
			stub.script("pytest -q", tc.effect)
			ctx := stagingCtx(t, stub)
			req := liveStagingReq(t, ctx, "pytest -q")

			res, _ := stageCandidate(ctx, req)

			if res.Commands[0].Outcome != tc.want {
				t.Errorf("outcome %q, want %q", res.Commands[0].Outcome, tc.want)
			}
			if got := res.authorizingOutcomes(); len(got) != 0 {
				t.Errorf("it authorized %v", got)
			}
		})
	}
}

// A command refused on this side is never sent to the executor.
func TestARefusedCommandNeverReachesTheExecutor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
	}{
		{"the safety gate refuses it", "rm -rf /"},
		{"it ends in an ampersand", "pytest -q &"},
		{"it ends in an ampersand and a space", "pytest -q & "},
		{"it uses nohup", "nohup pytest -q"},
		{"it uses setsid", "setsid pytest -q"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubSandbox(t)
			ctx := stagingCtx(t, stub)
			req := liveStagingReq(t, ctx, tc.command)

			res, ok := stageCandidate(ctx, req)

			if !ok {
				t.Fatal("no result")
			}
			if res.Commands[0].Outcome != stagingRefused {
				t.Errorf("outcome %q, want refused", res.Commands[0].Outcome)
			}
			if seen := stub.commandsSeen(); len(seen) != 0 {
				t.Errorf("a refused command was executed: %v", seen)
			}
			if got := res.authorizingOutcomes(); len(got) != 0 {
				t.Errorf("a refused command authorized %v", got)
			}
		})
	}
}

// A command whose staged target is outside the workspace is refused on this
// side and never sent to the executor.
func TestATargetOutsideTheWorkspaceNeverReachesTheExecutor(t *testing.T) {
	stub := newStubSandbox(t)
	ctx := stagingCtx(t, stub)
	req := liveStagingReq(t, ctx, "pytest -q")
	req.Identity.TargetPath = "/etc/passwd"

	res, ok := stageCandidate(ctx, req)

	if !ok {
		t.Fatal("no result")
	}
	if res.Commands[0].Outcome != stagingRefused {
		t.Errorf("outcome %q, want refused", res.Commands[0].Outcome)
	}
	if seen := stub.commandsSeen(); len(seen) != 0 {
		t.Errorf("a refused command was executed: %v", seen)
	}
	if got := res.authorizingOutcomes(); len(got) != 0 {
		t.Errorf("a refused command authorized %v", got)
	}
}

func TestACancelledRunStopsAndAuthorizesNothing(t *testing.T) {
	stub := newStubSandbox(t)
	stub.script("pytest -q", stubEffect{ExitCode: 0})
	ctx := stagingCtx(t, stub)
	cancelled, cancel := context.WithCancel(
		context.WithValue(context.Background(), requestIDKey, "req-stage"))
	cancel()
	ctx.Ctx = cancelled
	req := liveStagingReq(t, ctx, "pytest -q", "ruff check .")

	res, ok := stageCandidate(ctx, req)
	if !ok {
		t.Fatal("cancellation produced no result at all")
	}
	if res.Complete {
		t.Error("a cancelled set claimed to be complete")
	}
	for _, c := range res.Commands {
		if c.Outcome != stagingCancelled {
			t.Errorf("outcome %q, want cancelled", c.Outcome)
		}
	}
	if seen := stub.commandsSeen(); len(seen) != 0 {
		t.Errorf("a cancelled run executed %v", seen)
	}
}

func TestAnUnreachableExecutorIsUnavailableNotFailed(t *testing.T) {
	stub := newStubSandbox(t)
	ctx := stagingCtx(t, stub)
	ctx.SandboxURL = ""
	req := liveStagingReq(t, ctx, "pytest -q")
	res, ok := stageCandidate(ctx, req)
	if !ok {
		t.Fatal("an unreachable executor produced no result")
	}
	if res.Commands[0].Outcome != stagingUnavailable {
		t.Errorf("outcome %q, want unavailable", res.Commands[0].Outcome)
	}
	if res.Complete {
		t.Error("nothing ran and the set claimed to be complete")
	}
}

// --- sets ------------------------------------------------------------------------

func TestTwoCommandsWhereOnlyOnePasses(t *testing.T) {
	stub := newStubSandbox(t)
	stub.script("pytest -q", stubEffect{ExitCode: 0})
	stub.script("ruff check .", stubEffect{ExitCode: 1})
	ctx := stagingCtx(t, stub)
	req := liveStagingReq(t, ctx, "pytest -q", "ruff check .")

	res, _ := stageCandidate(ctx, req)
	if !res.Complete {
		t.Error("both commands ran; the set is complete even though one failed")
	}
	got := res.authorizingOutcomes()
	if len(got) != 1 || got[0] != req.Commands[0].ObligationID {
		t.Errorf("authorizing %v, want only the passing command", got)
	}
}

func TestCommandsRunInOrderAndKeepTheirOwnIdentity(t *testing.T) {
	stub := newStubSandbox(t)
	stub.script("first", stubEffect{ExitCode: 0})
	stub.script("second", stubEffect{ExitCode: 0})
	stub.script("third", stubEffect{ExitCode: 0})
	ctx := stagingCtx(t, stub)
	req := liveStagingReq(t, ctx, "first", "second", "third")

	res, _ := stageCandidate(ctx, req)
	if seen := stub.commandsSeen(); strings.Join(seen, ",") != "first,second,third" {
		t.Errorf("ran %v, want the declared order", seen)
	}
	ids := map[string]bool{}
	for _, c := range res.Commands {
		if ids[c.CommandIdentity] {
			t.Error("two results share one command identity")
		}
		ids[c.CommandIdentity] = true
	}
	if len(res.authorizingOutcomes()) != 3 {
		t.Error("three passing commands did not produce three authorizations")
	}
	// Each result names its own obligation: one command's pass never stands
	// in for another's.
	for i, c := range res.Commands {
		if c.ObligationID != req.Commands[i].ObligationID {
			t.Errorf("result %d names another command's obligation", i)
		}
	}
}

func TestAnOverBudgetSetIsRefusedBeforeAnythingRuns(t *testing.T) {
	stub := newStubSandbox(t)
	ctx := stagingCtx(t, stub)
	req := liveStagingReq(t, ctx, "a", "b")
	req.Budget.MaxCommands = 1
	if _, ok := stageCandidate(ctx, req); ok {
		t.Error("a set larger than its budget was staged")
	}
	if seen := stub.commandsSeen(); len(seen) != 0 {
		t.Errorf("an over-budget set executed %v", seen)
	}
}

// TestASetThatRanOutOfBudgetPartWayIsNotComplete is the case the budget exists
// for: the first command ran, the second had no time left, and the result
// must not read as the whole obligation discharged.
func TestASetThatRanOutOfBudgetPartWayIsNotComplete(t *testing.T) {
	stub := newStubSandbox(t)
	stub.script("slow", stubEffect{ExitCode: 0, SleepMS: 1200})
	stub.script("after", stubEffect{ExitCode: 0})
	ctx := stagingCtx(t, stub)
	req := liveStagingReq(t, ctx, "slow", "after")
	req.Budget.TotalTimeoutSec = 1
	req.Budget.PerCommandTimeoutSec = 1
	if ok, why := req.validate(); !ok {
		t.Fatalf("fixture: %s", why)
	}

	res, ok := stageCandidate(ctx, req)
	if !ok {
		t.Fatal("no result")
	}
	if res.Commands[1].Outcome != stagingBudgetExceeded {
		t.Fatalf("second outcome %q, want budget_exceeded", res.Commands[1].Outcome)
	}
	if res.Complete {
		t.Error("a set with an unrun command claimed to be complete")
	}
	if seen := stub.commandsSeen(); len(seen) != 1 || seen[0] != "slow" {
		t.Errorf("ran %v; the unbudgeted command should not have run", seen)
	}
	// The command that ran is still reported as it ended. On its own it cannot
	// discharge a two-command obligation, and Complete is what says so.
	if res.Commands[0].Outcome != stagingExitedZero {
		t.Errorf("first outcome %q", res.Commands[0].Outcome)
	}
}

// --- isolation --------------------------------------------------------------------

func TestStagingNeverWritesTheProductionWorkspace(t *testing.T) {
	stub := newStubSandbox(t)
	stub.script("pytest -q", stubEffect{ExitCode: 0, WriteTarget: "print(8)\n"})
	ctx := stagingCtx(t, stub)
	target := resolveAgentPath(ctx, "solve.py")
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	req := liveStagingReq(t, ctx, "pytest -q")

	stageCandidate(ctx, req)

	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("staging wrote %q into the real workspace", after)
	}
}

func TestParallelCandidatesDoNotShareStagingState(t *testing.T) {
	stub := newStubSandbox(t)
	stub.script("pytest -q", stubEffect{ExitCode: 0})
	ctx := stagingCtx(t, stub)
	requests := make([]stagingRequest, 4)
	for i := range requests {
		requests[i] = liveStagingReq(t, ctx, "pytest -q")
		requests[i].Identity.CandidateInstanceID = "req-stage:inv:1:cand-" + string(rune('a'+i))
	}

	var wg sync.WaitGroup
	results := make([]stagingResult, len(requests))
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = stageCandidate(ctx, requests[i])
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, res := range results {
		if len(res.Commands) != 1 || res.Commands[0].Outcome != stagingExitedZero {
			t.Errorf("candidate %d: %+v", i, res.Commands)
			continue
		}
		if seen[res.Identity.CandidateInstanceID] {
			t.Error("two parallel candidates share one identity")
		}
		seen[res.Identity.CandidateInstanceID] = true
	}
	if len(seen) != 4 {
		t.Errorf("%d distinct candidates, want 4", len(seen))
	}
}

func TestTheOverlayIsTornDownAfterSuccessAndAfterFailure(t *testing.T) {
	stub := newStubSandbox(t)
	stub.script("ok", stubEffect{ExitCode: 0})
	stub.script("bad", stubEffect{ExitCode: 2})
	stub.script("slow", stubEffect{ExitCode: -1, TimedOut: true})
	ctx := stagingCtx(t, stub)
	for _, cmd := range []string{"ok", "bad", "slow"} {
		req := liveStagingReq(t, ctx, cmd)
		res, _ := stageCandidate(ctx, req)
		if !res.WorkspaceDestroyed {
			t.Errorf("%q left its overlay standing", cmd)
		}
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.overlaysDestroyed != 3 {
		t.Errorf("%d overlays torn down, want one per run", stub.overlaysDestroyed)
	}
}

// --- no content leaves the executor ------------------------------------------------

func TestNoStdoutStderrOrCandidateBytesSurviveStaging(t *testing.T) {
	stub := newStubSandbox(t)
	// The stub returns SECRET_OUTPUT / SECRET_DIAGNOSTIC on every call.
	stub.script("pytest --token=hunter2", stubEffect{ExitCode: 0})
	ctx := stagingCtx(t, stub)
	req := liveStagingReq(t, ctx, "pytest --token=hunter2")
	res, _ := stageCandidate(ctx, req)

	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"SECRET_OUTPUT", "SECRET_DIAGNOSTIC", "hunter2", "pytest",
		stagedCode, "print(7)",
	} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the staging result carries %q", needle)
		}
	}
}

func TestTheStagingRequestSendsOnlyWhatTheInvocationNeeds(t *testing.T) {
	var body map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "exit_code": 0, "stdout": "", "stderr": "",
			"outcome": "completed",
			"observation": map[string]interface{}{
				"target_before":    map[string]string{"solve.py": contentSHA256(stagedCode)},
				"target_after":     map[string]string{"solve.py": contentSHA256(stagedCode)},
				"workspace_before": "w", "workspace_after": "w",
			},
		})
	}))
	defer srv.Close()
	ctx := stagingCtx(t, newStubSandbox(t))
	ctx.SandboxURL = srv.URL
	ctx.TaskContract = mustContract(t, ctx.WorkingDir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`)
	req := liveStagingReq(t, ctx, "pytest -q")
	stageCandidate(ctx, req)

	if body == nil {
		t.Fatal("the executor was never called")
	}
	var keys []string
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Exactly the four fields staging needs. No contract, no obligations, no
	// mode, no prose, no authority.
	if strings.Join(keys, ",") != "command,files,observe_paths,timeout" {
		t.Errorf("the executor was sent %v", keys)
	}
	blob, _ := json.Marshal(body)
	for _, banned := range []string{
		"task_contract", "task_mode", "obligation", "provenance",
		"expected_outputs", "verification_knowledge", "request_id",
	} {
		if strings.Contains(string(blob), banned) {
			t.Errorf("the executor was sent %q", banned)
		}
	}
}

// TestTheExecutorCannotDeclareItsOwnResultTrusted holds the trust boundary
// from the proxy side: whatever the executor says about authority is ignored,
// because only hashes, an exit code and two flags are read from its answer.
func TestTheExecutorCannotDeclareItsOwnResultTrusted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false, "exit_code": 7, "timed_out": false,
			// An executor trying to grant itself authority.
			"provenance":               "client_declared_verification",
			"authorized":               true,
			"trusted":                  true,
			"outcome":                  "exited_zero",
			"influences_live_decision": true,
			"observation": map[string]interface{}{
				"target_before":    map[string]string{"solve.py": contentSHA256(stagedCode)},
				"target_after":     map[string]string{"solve.py": contentSHA256(stagedCode)},
				"workspace_before": "w", "workspace_after": "w",
			},
		})
	}))
	defer srv.Close()
	ctx := stagingCtx(t, newStubSandbox(t))
	ctx.SandboxURL = srv.URL
	req := liveStagingReq(t, ctx, "pytest -q")
	res, _ := stageCandidate(ctx, req)

	// "exited_zero" is a staging word, not an execution outcome. The executor's
	// outcome is read as unclassified, so the run shows nothing and its exit
	// code is never scored.
	if res.Commands[0].Outcome != stagingUnobservable {
		t.Errorf("outcome %q; the executor's own claim was believed", res.Commands[0].Outcome)
	}
	if len(res.authorizingOutcomes()) != 0 {
		t.Error("an executor granted itself authority")
	}
	blob, _ := json.Marshal(res)
	for _, banned := range []string{"client_declared", "authorized", "trusted", "influences"} {
		if strings.Contains(string(blob), banned) {
			t.Errorf("the executor's claim survived into the result: %q", banned)
		}
	}
}
