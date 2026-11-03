package main

import (
	"encoding/json"
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itigges22/atlas-proxy/internal/wire"
)

// A command that was stopped is not a command that failed.
//
// A command killed at a resource ceiling exits non-zero exactly like a failing
// test. Reading it as one would record a behavioural failure of a candidate
// that nothing observed.

// A test suite killed at the memory ceiling, decoded from the executor: exit 1
// and a MemoryError on stderr, byte for byte what a failing suite looks like.
// Only the outcome field tells them apart.
func TestAMemoryKilledVerificationIsNotAFailedOne(t *testing.T) {
	body := `{"success":false,"stdout":"","stderr":"MemoryError\n","exit_code":1,` +
		`"elapsed_ms":2100,"timed_out":false,"outcome":"memory_exhausted",` +
		`"peak_memory_bytes":2100000000}`
	var out wire.RunCommandOutput
	if err := decodeShellResponse(strings.NewReader(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Outcome != wire.ExecutionMemoryExhausted {
		t.Fatalf("outcome %q", out.Outcome)
	}
	if runCommandVerifiable(out) {
		t.Error("a memory-killed command was accepted as verification")
	}
	// And the same exit code from a command that DID finish is a real failure.
	finished := wire.RunCommandOutput{ExitCode: 1, Outcome: wire.ExecutionCompleted}
	if runCommandVerifiable(finished) {
		t.Error("a genuinely failing command verified")
	}
	passed := wire.RunCommandOutput{ExitCode: 0, Outcome: wire.ExecutionCompleted}
	if !runCommandVerifiable(passed) {
		t.Error("a command that passed and finished did not verify")
	}
	// Exit zero from a command the executor SAID was stopped is not
	// verification either: a suite killed after its last assertion passed can
	// still exit zero, and it did not finish.
	for _, o := range []wire.ExecutionOutcome{
		wire.ExecutionMemoryExhausted, wire.ExecutionProcessLimit, wire.ExecutionOutputLimit,
		wire.ExecutionCancelled, wire.ExecutionSpawnFailed,
	} {
		if runCommandVerifiable(wire.RunCommandOutput{ExitCode: 0, Outcome: o}) {
			t.Errorf("exit zero under %q verified something", o)
		}
	}
}

// An older executor answers without the field. It must not read as success.
func TestAnExecutorThatDoesNotSpeakTheVocabularyFailsClosed(t *testing.T) {
	body := `{"success":true,"stdout":"ok","stderr":"","exit_code":0,"elapsed_ms":10}`
	var out wire.RunCommandOutput
	if err := decodeShellResponse(strings.NewReader(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Outcome != wire.ExecutionUnclassified {
		t.Fatalf("a missing outcome decoded as %q", out.Outcome)
	}
	// It is NOT refused: a proxy that called every command failed because its
	// executor had not been rebuilt would be worse than the risk it removes.
	// What silence cannot do is support an authorization: candidate staging
	// turns the same value into `unobservable`, which the staging table below
	// asserts.
	if !runCommandVerifiable(out) {
		t.Error("an unclassified execution was refused outright")
	}
	if wire.ExecutionKnownIncomplete(wire.ExecutionUnclassified) {
		t.Error("silence was treated as a verdict")
	}
	for _, o := range []wire.ExecutionOutcome{
		wire.ExecutionMemoryExhausted, wire.ExecutionProcessLimit, wire.ExecutionOutputLimit,
		wire.ExecutionCancelled, wire.ExecutionSpawnFailed, wire.ExecutionTimedOut,
	} {
		if !wire.ExecutionKnownIncomplete(o) {
			t.Errorf("%q did not deny verification", o)
		}
	}
	if wire.ExecutionKnownIncomplete(wire.ExecutionCompleted) {
		t.Error("a completed command denied verification")
	}
}

// What the model is told, and what it is not told.
func TestTheModelIsToldWhatHappenedAndNothingAboutTheHost(t *testing.T) {
	for _, o := range []wire.ExecutionOutcome{
		wire.ExecutionMemoryExhausted, wire.ExecutionProcessLimit, wire.ExecutionOutputLimit,
	} {
		msg := wire.ExecutionOutcomeMessage(o)
		if msg == "" {
			t.Errorf("%q tells the model nothing", o)
			continue
		}
		if !strings.Contains(msg, "did NOT fail") {
			t.Errorf("%q does not separate stopped from failed: %q", o, msg)
		}
		for _, leak := range []string{"cgroup", "/proc", "pid", "PID", "rlimit",
			"RLIMIT", "0x", "bytes", "GiB", "MiB", "container", "host"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%q leaks %q: %s", o, leak, msg)
			}
		}
	}
	// A completed command has nothing extra to say.
	if wire.ExecutionOutcomeMessage(wire.ExecutionCompleted) != "" {
		t.Error("a completed command carries a resource message")
	}
}

// --- staging: the same distinction, one layer up ----------------------------

func TestStagingSeparatesAStoppedCommandFromAFailedOne(t *testing.T) {
	const candidate = "cafebabe"
	base := stagingObservation{
		TargetBefore:    map[string]string{"solve.py": candidate},
		TargetAfter:     map[string]string{"solve.py": candidate},
		WorkspaceBefore: "w1", WorkspaceAfter: "w1", Path: "solve.py",
	}
	cases := []struct {
		name    string
		outcome wire.ExecutionOutcome
		exit    int
		success bool
		want    stagingCommandOutcome
		wantWhy AuthorizationReason
	}{
		{"memory killed", wire.ExecutionMemoryExhausted, 1, false,
			stagingResourceExhausted, ReasonEvidenceResourceExhausted},
		{"too many processes", wire.ExecutionProcessLimit, -15, false,
			stagingResourceExhausted, ReasonEvidenceResourceExhausted},
		{"output flood", wire.ExecutionOutputLimit, -13, false,
			stagingResourceExhausted, ReasonEvidenceResourceExhausted},
		{"genuinely failed", wire.ExecutionCompleted, 1, false,
			stagingExitedNonZero, ReasonEvidenceExecutionFailed},
		{"genuinely passed", wire.ExecutionCompleted, 0, true,
			stagingExitedZero, ""},
		{"executor too old", wire.ExecutionUnclassified, 0, true,
			stagingUnobservable, ReasonProducerNotRun},
		{"spawn failed", wire.ExecutionSpawnFailed, -1, false,
			stagingUnobservable, ReasonProducerNotRun},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := base
			obs.Outcome, obs.ExitCode, obs.Success = tc.outcome, tc.exit, tc.success
			var res stagingCommandResult
			staged := ""
			stagingApplyObservation(&res, obs, candidate, &staged)
			if res.Outcome != tc.want {
				t.Fatalf("outcome %q, want %q", res.Outcome, tc.want)
			}
			if !stagingCommandOutcomes[res.Outcome] {
				t.Errorf("%q is outside the closed set", res.Outcome)
			}
			if tc.wantWhy == "" {
				return
			}
			if why := stagingUnmetReason(res); why != tc.wantWhy {
				t.Errorf("reason %q, want %q", why, tc.wantWhy)
			}
		})
	}
}

// A resource kill fires the veto that says nothing was observed, never the one
// that says the candidate's own verification went against it. Neither delivery
// basis weakens this: a stopped verification is a veto whether or not the V3
// selection path named the candidate.
func TestResourceExhaustionVetoesForTheRightReason(t *testing.T) {
	for _, eligible := range []bool{false, true} {
		out := decideCandidatePolicy(policyContext(t), advisoryInput{
			Observed:         checkOutcome{Status: wire.ValidationPassed},
			TargetDeclared:   true,
			TargetAuthorized: true,
			ScopeAdmits:      true,
			Unmet: map[string]AuthorizationReason{
				"declared_command:abc": ReasonEvidenceResourceExhausted,
			},
			AutomaticEligible: eligible,
		}, false)
		if out.Decision != PolicyCandidateRejectedHardVeto {
			t.Errorf("eligible=%v: decision %q over a stopped verification", eligible, out.Decision)
		}
		if !hasVeto(out.Vetoes, VetoExecutionUnavailable) {
			t.Errorf("eligible=%v: vetoes %v do not say the execution never happened",
				eligible, out.Vetoes)
		}
		if hasVeto(out.Vetoes, VetoDeclaredVerificationFailed) {
			t.Errorf("eligible=%v: a stopped command was recorded as a failed verification", eligible)
		}
	}
}

// --- structural ownership ----------------------------------------------------

// Every production process spawn is registered, and untrusted execution has
// exactly one owner per world: the executor's bounded runner in the sandbox,
// and the one host route the operator has to opt into.
func TestEveryProcessSpawnIsRegistered(t *testing.T) {
	allowed := map[string]string{
		"tools.go:runLocally": "the opt-in host route, bounded by ulimit and its own process group",
	}
	found := processSpawnSites(proxyFiles(t))
	for site := range found {
		if _, ok := allowed[site]; !ok {
			t.Errorf("%s spawns a process and is not a registered execution owner. "+
				"Untrusted execution belongs in the sandbox's bounded runner; if this "+
				"is genuinely a new owner it needs its own resource contract and a "+
				"line here saying why.", site)
		}
	}
	for site := range allowed {
		if !found[site] {
			t.Errorf("%s is registered as an execution owner and no longer spawns "+
				"anything: remove it from the registry rather than leaving a hole", site)
		}
	}
}

// processSpawnSites names, as file:function, every function in files whose
// body refers to a way to start a process.
func processSpawnSites(files map[string]*ast.File) map[string]bool {
	found := map[string]bool{}
	for name, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				return true
			}
			ast.Inspect(fd.Body, func(inner ast.Node) bool {
				if sel, ok := inner.(*ast.SelectorExpr); ok && startsAProcess(sel) {
					found[name+":"+fd.Name.Name] = true
				}
				return true
			})
			return true
		})
	}
	return found
}

// startsAProcess reports whether sel names a way to start a process:
// exec.Command and its variants, os.StartProcess or syscall.Exec.
func startsAProcess(sel *ast.SelectorExpr) bool {
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return (pkg.Name == "exec" && strings.HasPrefix(sel.Sel.Name, "Command")) ||
		(pkg.Name == "os" && sel.Sel.Name == "StartProcess") ||
		(pkg.Name == "syscall" && sel.Sel.Name == "Exec")
}

// The host route carries the same ceiling, from the same operator value, and
// takes its own process group so the kill reaches what it started.
func TestTheHostRouteIsBoundedByTheSameNumbers(t *testing.T) {
	src, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	body := codeWithoutComments(string(src))
	fn := body[strings.Index(body, "func runLocally("):]
	fn = fn[:strings.Index(fn, "\nfunc ")]
	for _, required := range []string{"ulimit -v", "hostAddressSpaceKiB()",
		"Setpgid: true", "syscall.Kill(-cmd.Process.Pid"} {
		if !strings.Contains(fn, required) {
			t.Errorf("the host route no longer carries %s", required)
		}
	}
	// One source for the number: the host route may not name a size of its own.
	if strings.Contains(fn, "1024*1024") || strings.Contains(fn, "<< 20") {
		t.Error("the host route computes a memory size of its own")
	}
	// And the number it uses comes from the operator variable the executor reads.
	owner, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(owner), "ATLAS_EXEC_MEMORY_BYTES") {
		t.Error("the host ceiling no longer reads the operator's value")
	}
}

// Nothing about the candidate lifecycle may key off a resource outcome except
// by refusing. No grant, no settlement, no debt retirement.
func TestResourceExhaustionReachesNoGrant(t *testing.T) {
	for _, f := range []string{"authorization_grant.go", "candidate_delivery.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		body := codeWithoutComments(string(src))
		for _, banned := range []string{"ExecutionMemoryExhausted", "ExecutionOutputLimit",
			"ExecutionProcessLimit", "stagingResourceExhausted"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s reads %s: a resource outcome must reach the grant path "+
					"only as an unmet obligation", f, banned)
			}
		}
	}
}

// The wire shape the executor promises, pinned so a rename on either side is
// a test failure rather than a silent unclassified.
func TestTheExecutorWireCarriesTheOutcome(t *testing.T) {
	body, err := os.ReadFile("../sandbox/executor_server.py")
	if err != nil {
		// A skip here would pass in silence once the sandbox moves.
		t.Fatalf("the executor this test pins is not at ../sandbox: %v", err)
	}
	models, err := os.ReadFile("../sandbox/executor_models.py")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, models...)
	for _, required := range []string{`outcome: str = "internal_unclassified"`,
		"peak_memory_bytes", "EXEC_CONTRACT", "run_bounded("} {
		if !strings.Contains(string(body), required) {
			t.Errorf("the executor no longer carries %q", required)
		}
	}
	// And a round trip through the field name this side decodes.
	var probe struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(`{"outcome":"memory_exhausted"}`), &probe); err != nil {
		t.Fatal(err)
	}
	if wire.CanonicalExecutionOutcome(probe.Outcome) != wire.ExecutionMemoryExhausted {
		t.Error("the wire name and the vocabulary have drifted apart")
	}
}

// main logs the envelope's overruns once, at start.
func TestMainLogsTheEnvelopeOverrunsAtStart(t *testing.T) {
	if !strings.Contains(readSourceForTest(t, "main.go"), "memoryenvelope.LogOverruns(declaredMemoryEnvelope())") {
		t.Error("main no longer logs the overruns at start")
	}
}

// A deployment whose declared memory limits add up to more than the host
// refuses to run a command, and the command does not run.
func TestACommandIsRefusedWhenTheMemoryEnvelopeCannotHold(t *testing.T) {
	ctx, dir := shellWorld(t, nil)
	t.Setenv("ATLAS_HOST_MEMORY_BYTES", "4g")
	t.Setenv("ATLAS_HOST_RESERVE_BYTES", "1g")
	t.Setenv("ATLAS_SANDBOX_MEM", "11g")
	t.Setenv("ATLAS_EXEC_MEMORY_BYTES", "1g")

	args, _ := json.Marshal(wire.RunCommandInput{Command: "touch ran.txt"})
	res := executeToolCall("run_command", args, ctx)

	if res == nil || res.Success {
		t.Fatalf("a command ran in an envelope that cannot hold: %+v", res)
	}
	if !strings.Contains(res.Error, "memory limits add up to more than the machine has") {
		t.Errorf("refused for another reason: %q", res.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran.txt")); err == nil {
		t.Error("the refused command ran")
	}
}
