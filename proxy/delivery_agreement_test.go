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

// Does what LANDS agree with what was DECIDED?
//
// The typed decision was computed and discarded for three slices, precisely so
// this question could be answered before it was allowed to decide anything.
// Now it decides, and the answer has to hold over every shape a real request
// takes -- not just the ones that authorize.
//
// Two invariants, one per kind of traffic:
//
//	structured    the candidate lands if and only if the delivery decision
//	              said it delivers, and the bytes on disk are exactly the
//	              ones it named
//	legacy        nothing about the route changed at all
//
// Each row drives the real write path against a real stub service. Nothing
// here reimplements the decision: it asks the owner, then looks at the disk.

// agreementRow is one shape a request can take.
type agreementRow struct {
	name     string
	contract string
	// commands scripts the staging executor for the declared commands.
	commands map[string]stubEffect
	// before runs after the world is built and before the write.
	before func(t *testing.T, w *routeWorld)
	// wantDelivered is what the row expects to end up on disk: the winner or
	// the caller's own content.
	wantWinner bool
}

func agreementRows() []agreementRow {
	const declared = `{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`
	withCommands := func(cmds ...string) string {
		quoted := make([]string, 0, len(cmds))
		for _, c := range cmds {
			b, _ := json.Marshal(c)
			quoted = append(quoted, string(b))
		}
		return `{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],` +
			`"verification_knowledge":"declared","verification":[` + strings.Join(quoted, ",") + `]}`
	}
	// The same commands, typed behavioral over assets the client owns. A
	// behavioral baseline can only be re-established by evidence that reaches
	// behavioral, and only a typed declaration does.
	withBehavioralCommands := func(cmds ...string) string {
		typed := make([]string, 0, len(cmds))
		quoted := make([]string, 0, len(cmds))
		for _, c := range cmds {
			b, _ := json.Marshal(c)
			quoted = append(quoted, string(b))
			typed = append(typed, `{"command":`+string(b)+`,"kind":"behavioral",`+
				`"expects":"exit_zero","asset_authority":"client_supplied"}`)
		}
		return `{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],` +
			`"verification_knowledge":"declared","verification":[` + strings.Join(quoted, ",") + `],` +
			`"verification_requirements_version":1,"verification_requirements":[` +
			strings.Join(typed, ",") + `]}`
	}
	pass := map[string]stubEffect{"pytest -q": {ExitCode: 0}}

	return []agreementRow{
		{name: "new declared python output with syntax evidence",
			contract: declared, wantWinner: true},

		{name: "new declared python output with exact command pass",
			contract: withCommands("pytest -q"), commands: pass, wantWinner: true},

		{name: "exact command failure",
			contract: withCommands("pytest -q"),
			commands: map[string]stubEffect{"pytest -q": {ExitCode: 1}}},

		{name: "exact command timeout",
			contract: withCommands("pytest -q"),
			commands: map[string]stubEffect{"pytest -q": {ExitCode: -1, TimedOut: true}}},

		{name: "exact command refused by the safety gate",
			contract: withCommands("rm -rf /")},

		{name: "exact command cancelled",
			contract: withCommands("pytest -q"), commands: pass,
			before: func(t *testing.T, w *routeWorld) {
				// Cancelled after the world is built: staging observes the
				// cancellation and nothing authorizes.
				cancelled, cancel := context.WithCancel(
					context.WithValue(context.Background(), requestIDKey, "req-route"))
				cancel()
				w.ctx.Ctx = cancelled
			}},

		{name: "command that mutated the target",
			contract: withCommands("pytest -q"),
			commands: map[string]stubEffect{"pytest -q": {ExitCode: 0, WriteTarget: "rewritten\n"}}},

		{name: "command that mutated the workspace",
			contract: withCommands("pytest -q"),
			commands: map[string]stubEffect{"pytest -q": {ExitCode: 0, WriteOther: "changed"}}},

		{name: "multiple commands all pass",
			contract: withCommands("pytest -q", "ruff check ."),
			commands: map[string]stubEffect{
				"pytest -q": {ExitCode: 0}, "ruff check .": {ExitCode: 0}},
			wantWinner: true},

		{name: "one of several commands failing",
			contract: withCommands("pytest -q", "ruff check ."),
			commands: map[string]stubEffect{
				"pytest -q": {ExitCode: 0}, "ruff check .": {ExitCode: 1}}},

		{name: "existing syntax baseline with equal evidence",
			contract: declared, wantWinner: true,
			before: func(t *testing.T, w *routeWorld) {
				seedBaseline(t, w, ValidationKindSyntax, "")
			}},

		{name: "existing behavioral baseline re-established by the same command",
			contract:   withBehavioralCommands("python3 solve.py"),
			commands:   map[string]stubEffect{"python3 solve.py": {ExitCode: 0}},
			wantWinner: true,
			before: func(t *testing.T, w *routeWorld) {
				seedBaseline(t, w, ValidationKindSyntax, "python3 solve.py")
			}},

		{name: "existing behavioral baseline with syntax only",
			contract: declared,
			before: func(t *testing.T, w *routeWorld) {
				seedBaseline(t, w, ValidationKindSyntax, "python3 solve.py")
			}},

		{name: "existing behavioral baseline and a different command",
			contract: withCommands("ruff check ."),
			commands: map[string]stubEffect{"ruff check .": {ExitCode: 0}},
			before: func(t *testing.T, w *routeWorld) {
				seedBaseline(t, w, ValidationKindSyntax, "python3 solve.py")
			}},

		{name: "undeclared target",
			contract: `{"task_mode":"work","output_knowledge":"declared",` +
				`"expected_outputs":["something_else.py"]}`},

		// A contract that declares zero outputs states authoritatively that
		// this request produces nothing. The typed path owns it and
		// authorizes no target: an explicit empty set is an answer, not an
		// absence of one, and it may not fall through to legacy delivery.
		{name: "declared empty outputs",
			contract: `{"task_mode":"work","output_knowledge":"declared","expected_outputs":[]}`},
		// The same shape without the declaration. The model's own call names
		// the target, so the selection basis holds and the winner lands.
		{name: "unspecified outputs",
			contract: `{"task_mode":"work","output_knowledge":"unspecified"}`, wantWinner: true},
		// Declared commands bind a target the model's call named, too: staged,
		// and a failure keeps the caller's bytes.
		{name: "unspecified outputs, command passes",
			contract: `{"task_mode":"work","verification_knowledge":"declared","verification":["pytest -q"]}`,
			commands: pass, wantWinner: true},
		{name: "unspecified outputs, command fails",
			contract: `{"task_mode":"work","verification_knowledge":"declared","verification":["pytest -q"]}`,
			commands: map[string]stubEffect{"pytest -q": {ExitCode: 1}}},
		// A question produces nothing, and no contract names nothing: the
		// caller's own bytes stand.
		{name: "a question", contract: `{"task_mode":"question"}`},
		{name: "no contract", contract: ""},
	}
}

// seedBaseline puts a validated artifact at the target before anything is
// authorized, so the row's baseline-preservation requirement is real.
func seedBaseline(t *testing.T, w *routeWorld, kind ValidationKind, command string) {
	t.Helper()
	if err := os.WriteFile(w.path, []byte(routeBaseline), 0o644); err != nil {
		t.Fatal(err)
	}
	observeDeliverable(w.ctx, w.path, []byte(routeBaseline), kind, ValidationPassed, "")
	if command != "" {
		w.ctx.VerificationEvidence = append(w.ctx.VerificationEvidence, VerificationRecord{
			Command: command,
			Covered: map[string]string{w.path: contentSHA256(routeBaseline)}, Turn: 1,
		})
	}
}

// TestWhatLandsAgreesWithWhatWasDecided is the matrix.
func TestWhatLandsAgreesWithWhatWasDecided(t *testing.T) {
	for _, row := range agreementRows() {
		t.Run(row.name, func(t *testing.T) {
			w := newRouteWorld(t, row.contract, row.commands)
			if row.before != nil {
				row.before(t, w)
			}
			recs := captureShadow(t, func() {
				if _, err := w.write(t); err != nil {
					t.Fatalf("write failed: %v", err)
				}
			})
			if strings.Contains(row.name, "cancelled") {
				// The request ended before a candidate existed. Nothing was
				// decided and nothing landed, which is the agreement.
				if got, err := os.ReadFile(w.path); err == nil && string(got) == routeWinner {
					t.Error("a cancelled request delivered a candidate")
				}
				if consumedGrants(recs) != 0 {
					t.Error("a cancelled request spent an authorization")
				}
				return
			}

			// What the owners concluded, read from their own records rather
			// than recomputed here: a second computation could agree with the
			// disk and both be wrong about what production decided.
			decisions := recordsOfKind(recs, "candidate_authorization_decision")
			if len(decisions) != 1 {
				t.Fatalf("%d authorization decisions, want exactly one", len(decisions))
			}
			influences, _ := decisions[0]["influences_live_decision"].(bool)
			policies := recordsOfKind(recs, "candidate_policy_decision")
			if len(policies) != 1 {
				t.Fatalf("%d delivery decisions, want exactly one", len(policies))
			}
			delivers, _ := policies[0]["delivers"].(bool)

			// What landed.
			onDisk, err := os.ReadFile(w.path)
			if err != nil {
				t.Fatal(err)
			}
			landedWinner := string(onDisk) == routeWinner

			// Agreement is claimed where the decision OWNS the answer, and a
			// request with no structured obligations is owned by nothing: it
			// has no target to authorize against, so the caller's own bytes
			// are what land and no winner ever does.
			if influences && landedWinner != delivers {
				t.Errorf("decision %v delivers=%v but the winner %s (reason %v)",
					policies[0]["decision"], delivers, landedOrNot(landedWinner),
					decisions[0]["reason"])
			}
			if !influences && landedWinner {
				t.Errorf("a request the typed path does not own delivered a "+
					"candidate anyway (reason %v)", decisions[0]["reason"])
			}
			if landedWinner != row.wantWinner {
				t.Errorf("the winner %s; the row expects %s (reason %v)",
					landedOrNot(landedWinner), landedOrNot(row.wantWinner),
					decisions[0]["reason"])
			}
			// Exactly one grant is consumed when and only when it landed.
			consumed := consumedGrants(recs)
			want := 0
			if delivers && influences {
				want = 1
			}
			if consumed != want {
				t.Errorf("%d grants consumed, want %d", consumed, want)
			}
			// And no live grant survives the call either way.
			if liveGrantCount(w.ctx) != 0 {
				t.Errorf("%d live grants left behind", liveGrantCount(w.ctx))
			}
		})
	}
}

func landedOrNot(landed bool) string {
	if landed {
		return "landed"
	}
	return "did not land"
}

// --- the shapes the matrix cannot drive through the route -------------------------

func TestBorrowingAndReplayAgreeWithTheDecision(t *testing.T) {
	t.Run("cross-request", func(t *testing.T) {
		a := newGrantWorld(t)
		a.mint(t)
		claim := a.claim()
		claim.RequestID = "req-somebody-else"
		if _, why := consumeAuthorizationGrant(a.ctx, claim); why == "" {
			t.Error("one request's authorization delivered another's candidate")
		}
	})
	t.Run("cross-invocation", func(t *testing.T) {
		w := newGrantWorld(t)
		w.mint(t)
		claim := w.claim()
		claim.InvocationID = w.in.Identity.InvocationID + ":another"
		if _, why := consumeAuthorizationGrant(w.ctx, claim); why == "" {
			t.Error("one invocation's authorization delivered another's candidate")
		}
	})
	t.Run("candidate hash mismatch", func(t *testing.T) {
		w := newGrantWorld(t)
		w.mint(t)
		claim := w.claim()
		claim.CandidateHash = contentSHA256("other bytes\n")
		if _, why := consumeAuthorizationGrant(w.ctx, claim); why == "" {
			t.Error("an authorization was spent on bytes it was not about")
		}
	})
	t.Run("stale workspace", func(t *testing.T) {
		w := newGrantWorld(t)
		w.mint(t)
		bumpWorkspace(w.ctx, filepath.Join(w.ctx.WorkingDir, "elsewhere.py"),
			contentSHA256("moved on\n"))
		if _, why := consumeAuthorizationGrant(w.ctx, w.claim()); why == "" {
			t.Error("an authorization outlived the workspace it was about")
		}
	})
	t.Run("stale baseline", func(t *testing.T) {
		w := newGrantWorld(t)
		w.mint(t)
		if err := os.WriteFile(w.path, []byte("REPLACED = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, why := consumeAuthorizationGrant(w.ctx, w.claim()); why == "" {
			t.Error("an authorization outlived the baseline it was about")
		}
	})
	t.Run("replay", func(t *testing.T) {
		w := newGrantWorld(t)
		w.mint(t)
		if _, why := consumeAuthorizationGrant(w.ctx, w.claim()); why != "" {
			t.Fatalf("the honest consumption failed: %s", why)
		}
		if _, why := consumeAuthorizationGrant(w.ctx, w.claim()); why == "" {
			t.Error("a replayed authorization delivered twice")
		}
	})
	t.Run("concurrent consumption", func(t *testing.T) {
		w := newGrantWorld(t)
		w.mint(t)
		claim := w.claim()
		var wg sync.WaitGroup
		wins := make([]bool, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, why := consumeAuthorizationGrant(w.ctx, claim)
				wins[i] = why == ""
			}(i)
		}
		wg.Wait()
		n := 0
		for _, ok := range wins {
			if ok {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d concurrent consumers succeeded, want exactly one", n)
		}
	})
}

func TestAModelGeneratedSelfTestNeverDelivers(t *testing.T) {
	w := newAuthWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", authPy, true)
	ev, evID := w.mustObserve(t)
	// The model ran its own test and it passed. Relabelled as what it is.
	forged := ev
	forged.Provenance.Source = ProvenanceModelGenerated
	a := w.authorize(evID, nil, forged)
	if a.Decision.Authorized {
		t.Error("a model-generated record authorized a candidate")
	}
	if a.Grant != nil {
		t.Error("a model-generated record minted a grant")
	}
	if a.mayDeliver() {
		t.Error("a model-generated record was allowed to deliver")
	}
}

func TestALegacyRecordNeitherAuthorizesNorBlocks(t *testing.T) {
	w := newAuthWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", authPy, true)
	ev, evID := w.mustObserve(t)
	// An envelope from before provenance existed.
	legacy := &V3EvidenceEnvelope{WireVersion: "0.0.1"}
	a := w.authorize(evID, legacy, ev)
	if a.Decision.Reason != ReasonLegacyRecord {
		t.Errorf("reason %q, want legacy_record", a.Decision.Reason)
	}
	if a.Grant != nil {
		t.Error("a legacy record minted a grant")
	}
	// The request declared obligations, so the typed path still owns it and
	// an unusable record is a refusal rather than a pass-through.
	if !a.Typed || a.mayDeliver() {
		t.Error("an unusable record was allowed to deliver")
	}
}

func TestARepairCandidateIsJudgedOnItsOwnBytes(t *testing.T) {
	// A refinement of an artifact this run already delivered. It gets its own
	// invocation, its own candidate instance and its own decision; the
	// earlier delivery's authorization does not carry over.
	w := newSettledWorld(t, settledContract, nil)
	first := deliverySettlementFor(w.ctx, w.path)
	if first == nil {
		t.Fatal("the fixture did not deliver")
	}
	const repaired = "def solve(values):\n    return sum(values)  # repaired\n"
	ev, evID, ok := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, repaired,
		fallbackSyntaxOutcomeFor(w.ctx, w.path, repaired).aggregate())
	if !ok {
		t.Fatal("the producer did not observe the repair")
	}
	if evID.CandidateInstanceID == first.CandidateInstanceID {
		t.Error("a repair reused the first delivery's candidate identity")
	}
	repairEntry := mintRouteEntry(w.ctx)
	a := authorizeCandidateDelivery(w.ctx, repairEntry, w.path, repaired, evID, nil,
		[]proxyEvidence{ev}, "selected-repair", nil,
		fallbackSyntaxOutcomeFor(w.ctx, w.path, repaired).aggregate(),
		testMutationScope(w.ctx, repairEntry, w.path, repaired), automaticIntent{})
	if a.Grant != nil && a.Grant.CandidateHash != contentSHA256(repaired) {
		t.Error("the repair's grant is about other bytes")
	}
	// Whatever the decision, it is about the repair -- never inherited.
	if a.Grant != nil && a.Grant.CandidateInstanceID == first.CandidateInstanceID {
		t.Error("the repair inherited the first delivery's authorization")
	}
}

func TestALaterMutationAfterSettlementIsVisible(t *testing.T) {
	w := newSettledWorld(t, settledContract, nil)
	if owed, _ := postDeliverySettlementOwed(w.ctx); owed {
		t.Fatal("the fixture did not settle")
	}
	if err := os.WriteFile(w.path, []byte("SOMETHING ELSE = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	owed, why := postDeliverySettlementOwed(w.ctx)
	if !owed {
		t.Error("a mutation after settlement went unnoticed")
	}
	if !strings.Contains(why, "bytes on disk") {
		t.Errorf("reason %q does not name what changed", why)
	}
}

// --- feasibility stays out of it ---------------------------------------------------

// TestFeasibilityIsReadAtExactlyOnePlace replaces the inertness statement that
// stood here.
//
// The answer used to be computed and discarded. It now decides, under enforce
// only, so what has to hold is different: one production call site, one reader
// of that answer, and no other function anywhere reading a feasibility verdict
// to decide something of its own.
func TestFeasibilityIsReadAtExactlyOnePlace(t *testing.T) {
	files := proxyFiles(t)
	sites := callSites(files, "observeInvocationFeasibility")
	// Two routes observe it, and only one may act on it. The edit route
	// records the answer and never consults it: adding protection there must
	// not quietly start skipping generation, and any compute saving on that
	// route needs its own measurement and its own authorization.
	want := map[string]bool{
		"tools.go:writeFileWithV3":                    true,
		"edit_route_delivery.go:deliverEditCandidate": true,
	}
	for site := range sites {
		if !want[site] {
			t.Errorf("feasibility is observed from unexpected %s", site)
		}
	}
	for site := range want {
		if _, ok := sites[site]; !ok {
			t.Errorf("%s no longer observes feasibility", site)
		}
	}
	// The edit route observes and does not act: it never turns the answer
	// into a decision.
	if _, acts := callSites(files, "generationSkipped")["edit_route_delivery.go:deliverEditCandidate"]; acts {
		t.Error("the edit route consults the feasibility answer; that is a policy change")
	}
	// Exactly one function turns the answer into an action, and it is the
	// mode owner.
	skipSites := callSites(files, "generationSkipped")
	for site := range skipSites {
		switch site {
		case "tools.go:writeFileWithV3", "feasibility_decision.go:recordFeasibilityDecision":
		default:
			t.Errorf("%s acts on the feasibility answer", site)
		}
	}
	// And nothing else reads a verdict field to decide anything.
	for name := range files {
		if name == "feasibility_decision.go" || name == "tools.go" {
			continue
		}
		if strings.Contains(fileText(t, name), ".Feasible") {
			t.Errorf("%s reads a feasibility verdict", name)
		}
	}
}

func fileText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGenerationStillHappensForEveryShape(t *testing.T) {
	// Feasibility is observe-only, so a shape it would call infeasible still
	// generates. If that ever stops being true, the count changes here first.
	//
	// Cancellation is the one exception and not a skip: the request ended, so
	// the generation call is abandoned for a reason that has nothing to do
	// with feasibility.
	for _, row := range agreementRows() {
		if strings.Contains(row.name, "cancelled") {
			continue
		}
		w := newRouteWorld(t, row.contract, row.commands)
		if row.before != nil {
			row.before(t, w)
		}
		if _, err := w.write(t); err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		if w.generateCalls() != 1 {
			t.Errorf("%s: %d generation calls, want exactly one",
				row.name, w.generateCalls())
		}
	}
}

func consumedGrants(recs []map[string]interface{}) int {
	n := 0
	for _, r := range recordsOfKind(recs, "authorization_grant_event") {
		if r["event"] == string(grantConsumedAuthorized) {
			n++
		}
	}
	return n
}

// TestAnUnavailableStructuralGateRefusesRatherThanDelivers is the operational
// consequence of the typed path owning delivery, stated so it cannot be
// discovered in production.
//
// When the sandbox is unreachable the structural gate reports `not_run`, which
// is not a pass and not a failure: nothing was checked. No syntax evidence is
// produced, so a structured request has nothing that speaks for the candidate
// and keeps the caller's own content.
//
// That is a change. The previous behaviour delivered the winner with the gate
// down, on the envelope's word alone. Fail-closed is the correct direction for
// a decision about replacing someone's file, and a sandbox outage now costs
// candidate delivery for structured requests rather than costing the check.
func TestAnUnavailableStructuralGateRefusesRatherThanDelivers(t *testing.T) {
	w := newRouteWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`, nil)
	// The V3 service still answers; only the structural gate is unreachable.
	// A separate URL keeps the two failures distinguishable.
	w.ctx.SandboxURL = "http://127.0.0.1:1"

	out := fallbackSyntaxOutcomeFor(w.ctx, w.path, routeWinner).aggregate()
	if out.Status != ValidationNotRun {
		t.Fatalf("gate reported %q with the sandbox down, want not_run", out.Status)
	}
	if _, _, produced := observeDeliveredCandidateSyntax(w.ctx, mintRouteEntry(w.ctx), w.path, routeWinner, out); produced {
		t.Error("a check that did not run produced evidence")
	}

	res, err := w.write(t)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	onDisk, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) == routeWinner {
		t.Error("a candidate landed with nothing able to speak for it")
	}
	if res.AuthorizedDeliveryHash != "" {
		t.Error("an unauthorized delivery named an authorization")
	}
	// Contractless traffic answers the same way whether the gate is up or
	// down: it declared no target, so nothing can be authorized against it and
	// the model's own bytes are what land.
	u := newRouteWorld(t, `{"task_mode":"work"}`, nil)
	u.ctx.SandboxURL = "http://127.0.0.1:1"
	if _, err := u.write(t); err != nil {
		t.Fatalf("contractless write failed: %v", err)
	}
	got, err := os.ReadFile(u.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != routeBaseline {
		t.Errorf("contractless traffic delivered %q with the gate down", string(got))
	}
}
