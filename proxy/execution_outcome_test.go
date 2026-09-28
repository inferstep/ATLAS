package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// A command that was stopped is not a command that failed.
//
// The defective acquisition's staged verification was `python3 -m pytest -q`
// over a seeded loop that appends without bound. It reached 5.9 GB in seconds
// and the kernel killed the largest process on the box. Bounding the memory is
// half the fix; the other half is that a bounded kill exits non-zero exactly
// like a failing test, and reading it as one records a behavioural failure of
// a candidate that nothing observed.

func TestOnlyOneOutcomeMeansTheCommandFinished(t *testing.T) {
	for _, o := range []ExecutionOutcome{
		ExecutionCompleted, ExecutionTimedOut, ExecutionMemoryExhausted,
		ExecutionProcessLimit, ExecutionOutputLimit, ExecutionCancelled,
		ExecutionSpawnFailed, ExecutionUnclassified,
	} {
		if !knownExecutionOutcome(o) {
			t.Errorf("%q is not in the closed set", o)
		}
		if executionCompleted(o) != (o == ExecutionCompleted) {
			t.Errorf("%q answers the completion question wrongly", o)
		}
	}
	if knownExecutionOutcome("invented") {
		t.Error("an invented outcome was accepted")
	}
	// An executor that has not been taught this vocabulary, and one that grew
	// a member this build has not been taught, both fail closed.
	for _, raw := range []string{"", "invented", "COMPLETED", "completed "} {
		if got := canonicalExecutionOutcome(raw); got != ExecutionUnclassified {
			t.Errorf("%q canonicalised to %q, want unclassified", raw, got)
		}
	}
	if canonicalExecutionOutcome("completed") != ExecutionCompleted {
		t.Error("the one completion member does not survive canonicalisation")
	}
}

// The exact shape that took the host down, decoded from the executor.
func TestAMemoryKilledVerificationIsNotAFailedOne(t *testing.T) {
	// pytest over `stepped(0, 5, -1)`: killed at the ceiling, exit 1, and a
	// MemoryError on stderr -- byte-for-byte what a failing suite looks like.
	body := `{"success":false,"stdout":"","stderr":"MemoryError\n","exit_code":1,` +
		`"elapsed_ms":2100,"timed_out":false,"outcome":"memory_exhausted",` +
		`"peak_memory_bytes":2100000000}`
	var out RunCommandOutput
	if err := decodeShellResponse(strings.NewReader(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Outcome != ExecutionMemoryExhausted {
		t.Fatalf("outcome %q", out.Outcome)
	}
	if runCommandVerifiable(out) {
		t.Error("a memory-killed command was accepted as verification")
	}
	// And the same exit code from a command that DID finish is a real failure.
	finished := RunCommandOutput{ExitCode: 1, Outcome: ExecutionCompleted}
	if runCommandVerifiable(finished) {
		t.Error("a genuinely failing command verified")
	}
	passed := RunCommandOutput{ExitCode: 0, Outcome: ExecutionCompleted}
	if !runCommandVerifiable(passed) {
		t.Error("a command that passed and finished did not verify")
	}
	// Exit zero from a command the executor SAID was stopped is not
	// verification either: a suite killed after its last assertion passed can
	// still exit zero, and it did not finish.
	for _, o := range []ExecutionOutcome{
		ExecutionMemoryExhausted, ExecutionProcessLimit, ExecutionOutputLimit,
		ExecutionCancelled, ExecutionSpawnFailed,
	} {
		if runCommandVerifiable(RunCommandOutput{ExitCode: 0, Outcome: o}) {
			t.Errorf("exit zero under %q verified something", o)
		}
	}
}

// An older executor answers without the field. It must not read as success.
func TestAnExecutorThatDoesNotSpeakTheVocabularyFailsClosed(t *testing.T) {
	body := `{"success":true,"stdout":"ok","stderr":"","exit_code":0,"elapsed_ms":10}`
	var out RunCommandOutput
	if err := decodeShellResponse(strings.NewReader(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Outcome != ExecutionUnclassified {
		t.Fatalf("a missing outcome decoded as %q", out.Outcome)
	}
	// It is NOT refused. Silence from an executor older than this vocabulary
	// is the state every command was in before the contract existed, and a
	// proxy that called every command failed because its executor had not been
	// rebuilt would be worse than the risk it removes. What silence cannot do
	// is support an authorization: candidate staging turns the same value into
	// `unobservable`, which is asserted in the staging table below.
	if !runCommandVerifiable(out) {
		t.Error("an unclassified execution was refused outright")
	}
	if executionKnownIncomplete(ExecutionUnclassified) {
		t.Error("silence was treated as a verdict")
	}
	for _, o := range []ExecutionOutcome{
		ExecutionMemoryExhausted, ExecutionProcessLimit, ExecutionOutputLimit,
		ExecutionCancelled, ExecutionSpawnFailed, ExecutionTimedOut,
	} {
		if !executionKnownIncomplete(o) {
			t.Errorf("%q did not deny verification", o)
		}
	}
	if executionKnownIncomplete(ExecutionCompleted) {
		t.Error("a completed command denied verification")
	}
}

// What the model is told, and what it is not told.
func TestTheModelIsToldWhatHappenedAndNothingAboutTheHost(t *testing.T) {
	for _, o := range []ExecutionOutcome{
		ExecutionMemoryExhausted, ExecutionProcessLimit, ExecutionOutputLimit,
	} {
		msg := executionOutcomeMessage(o)
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
	if executionOutcomeMessage(ExecutionCompleted) != "" {
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
		outcome ExecutionOutcome
		exit    int
		success bool
		want    stagingCommandOutcome
		wantWhy AuthorizationReason
	}{
		{"memory killed", ExecutionMemoryExhausted, 1, false,
			stagingResourceExhausted, ReasonEvidenceResourceExhausted},
		{"too many processes", ExecutionProcessLimit, -15, false,
			stagingResourceExhausted, ReasonEvidenceResourceExhausted},
		{"output flood", ExecutionOutputLimit, -13, false,
			stagingResourceExhausted, ReasonEvidenceResourceExhausted},
		{"genuinely failed", ExecutionCompleted, 1, false,
			stagingExitedNonZero, ReasonEvidenceExecutionFailed},
		{"genuinely passed", ExecutionCompleted, 0, true,
			stagingExitedZero, ""},
		{"executor too old", ExecutionUnclassified, 0, true,
			stagingUnobservable, ReasonProducerNotRun},
		{"spawn failed", ExecutionSpawnFailed, -1, false,
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
// that says the candidate's own verification went against it.
func TestResourceExhaustionVetoesForTheRightReason(t *testing.T) {
	out := decideCandidatePolicy(policyContext(t), advisoryInput{
		Observed:         checkOutcome{Status: ValidationPassed},
		TargetDeclared:   true,
		TargetAuthorized: true,
		ScopeAdmits:      true,
		Unmet: map[string]AuthorizationReason{
			"declared_command:abc": ReasonEvidenceResourceExhausted,
		},
	}, false)
	if out.Decision != PolicyCandidateRejectedHardVeto {
		t.Fatalf("decision %q", out.Decision)
	}
	if !hasVeto(out.Vetoes, VetoExecutionUnavailable) {
		t.Errorf("vetoes %v do not say the execution never happened", out.Vetoes)
	}
	if hasVeto(out.Vetoes, VetoDeclaredVerificationFailed) {
		t.Error("a stopped command was recorded as a failed verification")
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
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				return true
			}
			ast.Inspect(fd.Body, func(inner ast.Node) bool {
				sel, ok := inner.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				spawns := (pkg.Name == "exec" && strings.HasPrefix(sel.Sel.Name, "Command")) ||
					(pkg.Name == "os" && sel.Sel.Name == "StartProcess") ||
					(pkg.Name == "syscall" && sel.Sel.Name == "Exec")
				if spawns {
					found[name+":"+fd.Name.Name] = true
				}
				return true
			})
			return true
		})
	}
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
	owner, err := os.ReadFile("execution_outcome.go")
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

// Neither delivery basis weakens it: a stopped verification is a veto
// whether or not the V3 selection path named the candidate.
func TestNoDeliveryBasisWeakensResourceSafety(t *testing.T) {
	for _, eligible := range []bool{false, true} {
		out := decideCandidatePolicy(policyContext(t), advisoryInput{
			Observed:         checkOutcome{Status: ValidationPassed},
			TargetDeclared:   true,
			TargetAuthorized: true,
			ScopeAdmits:      true,
			Unmet: map[string]AuthorizationReason{
				"declared_command:abc": ReasonEvidenceResourceExhausted,
			},
			AutomaticEligible: eligible,
		}, false)
		if out.Decision != PolicyCandidateRejectedHardVeto {
			t.Errorf("eligible=%v decided %q over a stopped verification", eligible, out.Decision)
		}
	}
}

// The wire shape the executor promises, pinned so a rename on either side is
// a test failure rather than a silent unclassified.
func TestTheExecutorWireCarriesTheOutcome(t *testing.T) {
	body, err := os.ReadFile("../sandbox/executor_server.py")
	if err != nil {
		t.Skip("sandbox source not present")
	}
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
	if canonicalExecutionOutcome(probe.Outcome) != ExecutionMemoryExhausted {
		t.Error("the wire name and the vocabulary have drifted apart")
	}
}

// --- the declared envelope ---------------------------------------------------

// The deployment that killed the model was one where every process was inside
// its own limit and the sum was not.
func TestAnEnvelopeThatDoesNotFitTheHostIsRefused(t *testing.T) {
	const GiB = int64(1) << 30
	fits := memoryEnvelope{
		HostBytes: 15*GiB + GiB/3, ReserveBytes: 3 * GiB / 2,
		Budgets: []memoryBudget{
			{"lens", 1792 * (1 << 20), true}, {"v3-service", 512 * (1 << 20), true},
			{"proxy", 256 * (1 << 20), true}, {"sandbox", 1536 * (1 << 20), true},
		},
		PerCommandBytes: GiB, SandboxBytes: 1536 * (1 << 20), Concurrency: 1,
	}
	if problems := fits.validate(); len(problems) != 0 {
		t.Fatalf("the shipped envelope does not validate: %v", problems)
	}
	if executionEnvelopeRefusal(fits) != "" {
		t.Error("a fitting envelope refused execution")
	}

	// The deployment as it actually stood: an 11 GiB sandbox beside an
	// unbounded 9 GiB model on a 15 GiB host.
	//
	// The enforced maxima ALONE do not catch it, and that is the lesson. With
	// the model unbounded there is no inference maximum to add, so eleven
	// gigabytes of sandbox plus the small services plus the reserve fits --
	// which is exactly how every process came to be inside its own limit at
	// the moment the kernel went looking for something to kill. What catches
	// it is the remainder: what the enforced budgets leave over for the one
	// component nothing holds.
	asItWas := fits
	asItWas.Budgets = []memoryBudget{
		{"sandbox", 11 * GiB, true}, {"inference", 11 * GiB, false},
	}
	asItWas.SandboxBytes = 11 * GiB
	if len(asItWas.validate()) != 0 {
		t.Fatal("the enforced maxima are expected to fit here; the point is that " +
			"fitting is not sufficient")
	}
	over := asItWas.unenforcedOverruns()
	if len(over) != 1 || !strings.Contains(over[0], "inference") {
		t.Fatalf("the configuration that killed the model was not reported: %v", over)
	}
	// The overrun is reported WITH its size, which is what separates the two
	// configurations: the historical one misses by more than ten gigabytes,
	// today's by about one.
	if !strings.Contains(over[0], "11.00 GiB") {
		t.Errorf("the report does not say what was expected: %q", over[0])
	}

	// A refusal still exists, for the case it was built for: enforced maxima
	// that over-commit. Setting an inference limit AND an eleven-gigabyte
	// sandbox is a number an operator can edit, and it is refused.
	enforcedOver := fits
	enforcedOver.Budgets = append(append([]memoryBudget{}, fits.Budgets...),
		memoryBudget{"inference", 9728 * (1 << 20), true})
	for i := range enforcedOver.Budgets {
		if enforcedOver.Budgets[i].Name == "sandbox" {
			enforcedOver.Budgets[i] = memoryBudget{"sandbox", 11 * GiB, true}
		}
	}
	enforcedOver.SandboxBytes = 11 * GiB
	if !strings.Contains(executionEnvelopeRefusal(enforcedOver), "over by") {
		t.Errorf("an over-committed enforced set was not refused: %q",
			executionEnvelopeRefusal(enforcedOver))
	}

	// Concurrency has to fit too, or raising it is a silent over-commit.
	crowded := fits
	crowded.Concurrency = 2
	if len(crowded.validate()) == 0 {
		t.Error("two concurrent commands fit in a container that holds one")
	}

	// Nothing declared is not the same as everything fine: no refusal, and no
	// claim either.
	if executionEnvelopeRefusal(memoryEnvelope{}) != "" {
		t.Error("an undeclared envelope refused execution")
	}
	var undeclared memoryEnvelope
	if undeclared.declared() {
		t.Error("an empty envelope reports itself as declared")
	}
}

func TestTheEnvelopeReadsTheSizesComposeWrites(t *testing.T) {
	for raw, want := range map[string]int64{
		"1536m": 1536 << 20, "9728M": 9728 << 20, "11g": 11 << 30,
		"2G": 2 << 30, "1073741824": 1073741824, "512k": 512 << 10,
		"": 0, "lots": 0, "-5": 0,
	} {
		t.Setenv("ATLAS_PROBE_SIZE", raw)
		if got := envBytes("ATLAS_PROBE_SIZE"); got != want {
			t.Errorf("%q read as %d, want %d", raw, got, want)
		}
	}
}

// A budget that is missing, zero or negative is a declaration nobody can act
// on, and it fails closed rather than being treated as unlimited.
func TestAMalformedEnvelopeFailsClosed(t *testing.T) {
	const GiB = int64(1) << 30
	base := memoryEnvelope{
		HostBytes: 16 * GiB, ReserveBytes: GiB,
		Budgets:      []memoryBudget{{"inference", 8 * GiB, true}, {"sandbox", GiB, true}},
		SandboxBytes: GiB, PerCommandBytes: GiB / 2, Concurrency: 1,
	}
	if len(base.validate()) != 0 {
		t.Fatalf("the base envelope does not validate: %v", base.validate())
	}
	noReserve := base
	noReserve.ReserveBytes = 0
	if len(noReserve.validate()) == 0 {
		t.Error("an envelope with no host reserve validated")
	}
	zeroBudget := base
	zeroBudget.Budgets = []memoryBudget{{"inference", 0, true}}
	if len(zeroBudget.validate()) == 0 {
		t.Error("a component with no budget validated")
	}
	noContainer := base
	noContainer.SandboxBytes = 0
	if len(noContainer.validate()) == 0 {
		t.Error("a per-command ceiling with no container to hold it validated")
	}
	zeroConcurrency := base
	zeroConcurrency.Concurrency = 0
	if len(zeroConcurrency.validate()) == 0 {
		t.Error("zero concurrency validated")
	}
}

// An unbounded component is not a maximum, and pretending otherwise is the
// mistake that made the first draft of this envelope dangerous.
//
// The inference service has no cgroup limit: its measured peak resident set is
// above the value that was proposed for it, and a hard limit under the peak
// produces a certain kill rather than reclaim. So its budget is an
// EXPECTATION. It is excluded from the maxima sum -- an unbounded component
// has no maximum to add -- and reported separately when the expectation does
// not fit what the enforced budgets leave.
func TestAnUnenforcedBudgetIsReportedAndNotSummed(t *testing.T) {
	const GiB = int64(1) << 30
	e := memoryEnvelope{
		HostBytes: 15*GiB + GiB/3, ReserveBytes: 3 * GiB / 2,
		Budgets: []memoryBudget{
			{"lens", 1792 * (1 << 20), true}, {"v3-service", 512 * (1 << 20), true},
			{"proxy", 256 * (1 << 20), true}, {"sandbox", 1536 * (1 << 20), true},
			{"inference", 11 * GiB, false},
		},
		PerCommandBytes: GiB, SandboxBytes: 1536 * (1 << 20), Concurrency: 1,
	}
	// The enforced maxima fit, so execution is not refused: a configuration an
	// operator can fix by editing a number is what the refusal is for.
	if problems := e.validate(); len(problems) != 0 {
		t.Fatalf("the enforced maxima do not fit: %v", problems)
	}
	if executionEnvelopeRefusal(e) != "" {
		t.Error("an unbounded component refused execution")
	}
	// But it IS reported: 11 GiB expected against what four enforced budgets
	// and the reserve leave.
	over := e.unenforcedOverruns()
	if len(over) != 1 || !strings.Contains(over[0], "inference") {
		t.Fatalf("the unbounded overrun is not reported: %v", over)
	}
	if !strings.Contains(over[0], "not held to a limit") {
		t.Errorf("the report does not say it is unbounded: %q", over[0])
	}
	// Enforcing it instead moves it into the sum, where it does not fit.
	enforced := e
	enforced.Budgets = append([]memoryBudget{}, e.Budgets...)
	enforced.Budgets[4].Enforced = true
	if len(enforced.validate()) == 0 {
		t.Error("enforcing an 11 GiB inference budget on a 15 GiB host validated")
	}
	if len(enforced.unenforcedOverruns()) != 0 {
		t.Error("an enforced budget is still reported as an overrun")
	}
	// And a fitting expectation is silent.
	small := e
	small.Budgets = append([]memoryBudget{}, e.Budgets...)
	small.Budgets[4].Bytes = 5 * GiB
	if len(small.unenforcedOverruns()) != 0 {
		t.Errorf("a fitting expectation was reported: %v", small.unenforcedOverruns())
	}
}

// ATLAS_LLAMA_MEM enforces; ATLAS_LLAMA_BUDGET_BYTES only accounts. Setting
// the first must not leave the second silently shadowing it.
func TestTheInferenceBudgetIsEnforcedOnlyWhenTheLimitIsSet(t *testing.T) {
	t.Setenv("ATLAS_HOST_MEMORY_BYTES", "16495915008")
	t.Setenv("ATLAS_LLAMA_BUDGET_BYTES", "11811160064")
	t.Setenv("ATLAS_LLAMA_MEM", "")
	e := envelopeFromEnv()
	var inference *memoryBudget
	for i := range e.Budgets {
		if e.Budgets[i].Name == "inference" {
			inference = &e.Budgets[i]
		}
	}
	if inference == nil {
		t.Fatal("the inference term is missing from the envelope entirely")
	}
	if inference.Enforced {
		t.Error("an unset ATLAS_LLAMA_MEM produced an enforced budget")
	}
	if inference.Bytes != 11811160064 {
		t.Errorf("expectation %d", inference.Bytes)
	}
	t.Setenv("ATLAS_LLAMA_MEM", "9728m")
	e = envelopeFromEnv()
	for _, b := range e.Budgets {
		if b.Name == "inference" {
			if !b.Enforced {
				t.Error("a set ATLAS_LLAMA_MEM did not enforce")
			}
			if b.Bytes != 9728*(1<<20) {
				t.Errorf("the expectation shadowed the limit: %d", b.Bytes)
			}
		}
	}
}
