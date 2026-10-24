package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The acquisition control, proved through the real production route.
//
// Setting the policy to advisory does not make a run capture-only: a STRICT
// authorization would still mint a grant and land a candidate in the task
// workspace. The control stops that, and these tests run the same route with
// it on.

const captureOnlyContract = `{"task_mode":"work","output_knowledge":"declared",` +
	`"expected_outputs":["solve.py"],"verification_knowledge":"declared",` +
	`"verification_requirements_version":1,"verification_requirements":[` +
	`{"command":"pytest -q","kind":"behavioral","expects":"exit_zero",` +
	`"asset_authority":"client_supplied"}],"verification":["pytest -q"]}`

func captureOnlyWorld(t *testing.T, contract string, commands map[string]stubEffect) *routeWorld {
	t.Helper()
	return newRouteWorldWithClosure(t, contract, commands, false)
}

// Capture disabled: existing strict behaviour is exactly what it was. A trusted
// candidate earns its licence and lands.
func TestCaptureDisabledStillDeliversAStrictCandidate(t *testing.T) {
	w := captureOnlyWorld(t, captureOnlyContract, map[string]stubEffect{"pytest -q": {}})
	res, err := w.write(t)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if got := w.disk(t); got != routeWinner {
		t.Fatalf("disk holds %q, want the candidate", got)
	}
	if res.AuthorizedDeliveryHash != contentSHA256(routeWinner) {
		t.Error("the delivery did not name the bytes it was authorized for")
	}
	if res.DeliveryProvenance != DeliveryFromStrictCandidate {
		t.Errorf("provenance %q", res.DeliveryProvenance)
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Error("the one-time grant was not spent")
	}
}

// Capture-only: the same route and the same evidence -- and no licence, no
// delivery, and the caller's own bytes on disk. It costs no extra model turn.
func TestCaptureOnlySuppressesAStrictDelivery(t *testing.T) {
	t.Setenv(CandidateCaptureOnlyEnv, "1")
	w := captureOnlyWorld(t, captureOnlyContract, map[string]stubEffect{"pytest -q": {}})
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})

	if got := w.disk(t); got != routeBaseline {
		t.Fatalf("candidate bytes reached the workspace: %q", got)
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Error("a grant outlived a suppressed delivery")
	}
	for _, r := range recordsOfKind(recs, "authorization_grant_event") {
		t.Errorf("a grant event survived capture-only: %v", r["event"])
	}
	for _, r := range recordsOfKind(recs, "shadow_delivery_disposition") {
		if r["disposition"] == "consumed_and_landed" {
			t.Error("a delivery landed under capture-only")
		}
	}

	// The pipeline was asked once, and the route recorded how it ended.
	if got := w.generateCalls(); got != 1 {
		t.Errorf("the pipeline was asked %d times", got)
	}
	if len(recordsOfKind(recs, "shadow_route_disposition")) == 0 {
		t.Error("the route recorded no ending")
	}

	// The declared command still ran against the exact candidate bytes.
	if w.shell.runsOf("pytest -q") != 1 {
		t.Errorf("the declared command ran %d times", w.shell.runsOf("pytest -q"))
	}
	if got := w.shell.stagedBytes("pytest -q"); got != routeWinner {
		t.Errorf("staged %q, want the candidate", got)
	}
}

// Capture-only keeps the policy answer: the policy still concludes what it
// would have concluded.
func TestCaptureOnlyKeepsThePolicyAnswer(t *testing.T) {
	t.Setenv(CandidateCaptureOnlyEnv, "1")
	w := captureOnlyWorld(t, captureOnlyContract, map[string]stubEffect{"pytest -q": {}})
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})

	policy := recordsOfKind(recs, "candidate_policy_decision")
	if len(policy) == 0 {
		t.Fatal("no policy decision was recorded")
	}
	if policy[0]["decision"] != string(PolicyCandidateAuthorizedStrict) {
		t.Fatalf("policy decided %v, want candidate_authorized_strict", policy[0]["decision"])
	}
	if policy[0]["delivers"] != false {
		t.Error("a suppressed decision still claimed delivery")
	}

	// And the suppression is its own fact, beside the answer rather than
	// instead of it.
	sup := recordsOfKind(recs, "candidate_capture_only_suppression")
	if len(sup) != 1 {
		t.Fatalf("%d suppression records, want exactly one", len(sup))
	}
	if sup[0]["would_authorize"] != true || sup[0]["grant_minted"] != false {
		t.Errorf("suppression record %v", sup[0])
	}
	disp := recordsOfKind(recs, "candidate_capture_only_disposition")
	if len(disp) != 1 {
		t.Fatalf("%d disposition records, want exactly one", len(disp))
	}
	if disp[0]["would_have"] != CaptureWouldAuthorizeStrict {
		t.Errorf("would_have %v, want would_authorize_strict", disp[0]["would_have"])
	}
	if disp[0]["delivery_suppressed"] != true || disp[0]["delivered"] != false {
		t.Errorf("disposition record %v", disp[0])
	}
}

// Under capture-only, one candidate hash runs through verification, policy,
// suppression and disposition. A join that drifted would describe two
// candidates.
func TestCaptureOnlyNamesOneCandidateThroughout(t *testing.T) {
	t.Setenv(CandidateCaptureOnlyEnv, "1")
	w := captureOnlyWorld(t, captureOnlyContract, map[string]stubEffect{"pytest -q": {}})
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})

	want := contentSHA256(routeWinner)
	for _, kind := range []string{"candidate_policy_decision",
		"candidate_capture_only_suppression", "candidate_capture_only_disposition"} {
		records := recordsOfKind(recs, kind)
		if len(records) == 0 {
			t.Errorf("no %s record to join", kind)
		}
		for _, r := range records {
			if r["candidate_hash"] != want {
				t.Errorf("%s names candidate %v, want %s", kind, r["candidate_hash"], want)
			}
		}
	}
	for _, r := range recordsOfKind(recs, "candidate_evidence_observation") {
		if r["source"] == "client_declared_verification" && r["candidate_hash"] != want {
			t.Errorf("verification evidence names %v", r["candidate_hash"])
		}
	}
}

// Every edit tool goes through the same boundary.
func TestCaptureOnlyCoversEveryEditTool(t *testing.T) {
	t.Setenv(CandidateCaptureOnlyEnv, "1")
	for _, tool := range []string{"edit_file", "insert_after", "replace_lines",
		"structural_edit"} {
		t.Run(tool, func(t *testing.T) {
			w := captureOnlyWorld(t, captureOnlyContract,
				map[string]stubEffect{"pytest -q": {}})
			if err := os.WriteFile(w.path, []byte(routeBaseline), 0o644); err != nil {
				t.Fatal(err)
			}
			outcome := deliverEditCandidate(w.ctx, tool, w.path, "solve.py",
				routeBaseline, routeBaseline)
			if outcome.Delivered {
				t.Fatalf("%s delivered under capture-only", tool)
			}
			if got := w.disk(t); got != routeBaseline {
				t.Errorf("%s left %q on disk", tool, got)
			}
			if liveGrantCount(w.ctx) != 0 {
				t.Errorf("%s left a live grant", tool)
			}
		})
	}
}

// Whatever the policy answers, under the control nothing lands and no grant
// survives.
func TestCaptureOnlyAcrossEveryPolicyAnswer(t *testing.T) {
	t.Setenv(CandidateCaptureOnlyEnv, "1")
	for _, tc := range []struct {
		name   string
		effect stubEffect
	}{
		{"strict authorization", stubEffect{}},
		{"hard veto", stubEffect{ExitCode: 1}},
		{"execution unavailable", stubEffect{HTTPStatus: 503}},
		{"timeout", stubEffect{TimedOut: true}},
		{"mutated target", stubEffect{WriteTarget: "rewritten\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := captureOnlyWorld(t, captureOnlyContract, map[string]stubEffect{"pytest -q": tc.effect})
			res, err := w.write(t)
			if err != nil {
				t.Fatalf("write failed: %v", err)
			}
			if got := w.disk(t); got != routeBaseline {
				t.Fatalf("candidate bytes landed: %q", got)
			}
			if res.V3Used {
				t.Error("a suppressed route claimed V3 provenance")
			}
			if res.DeliveryProvenance != DeliveryFromModelProposal {
				t.Errorf("provenance %q", res.DeliveryProvenance)
			}
			if liveGrantCount(w.ctx) != 0 {
				t.Error("a live grant survived")
			}
		})
	}
}

// A cancelled request under capture-only leaves nothing behind either.
func TestCaptureOnlyUnderCancellation(t *testing.T) {
	t.Setenv(CandidateCaptureOnlyEnv, "1")
	w := captureOnlyWorld(t, captureOnlyContract, map[string]stubEffect{"pytest -q": {}})
	cancelled, cancel := context.WithCancel(w.ctx.Ctx)
	cancel()
	w.ctx.Ctx = cancelled
	// The write may fail or not; either way the candidate is not on disk.
	w.write(t)
	if raw, err := os.ReadFile(w.path); err == nil && string(raw) == routeWinner {
		t.Fatal("a cancelled request delivered a candidate")
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Error("cancellation left a live grant")
	}
}

// The control is operator-owned. Nothing a client, a model or a service sends
// can reach it, and an unknown value is off.
func TestCaptureOnlyIsOperatorOwnedAndFailsClosed(t *testing.T) {
	for _, raw := range []string{"", "0", "off", "no", "false", "maybe", "ADVISORY", " "} {
		t.Setenv(CandidateCaptureOnlyEnv, raw)
		if candidateCaptureOnly() {
			t.Errorf("%q enabled the acquisition control", raw)
		}
	}
	for _, raw := range []string{"1", "true", "yes", "on", " On "} {
		t.Setenv(CandidateCaptureOnlyEnv, raw)
		if !candidateCaptureOnly() {
			t.Errorf("%q did not enable the control", raw)
		}
	}
}

// A task contract cannot carry the control: the field does not exist, so a
// caller that sends one has it dropped rather than obeyed. And the model
// never sees it.
func TestNoTaskContractOrToolResultCarriesTheCaptureControl(t *testing.T) {
	var in TaskContract
	if err := json.Unmarshal([]byte(
		`{"task_mode":"work","candidate_capture_only":true}`), &in); err != nil {
		t.Fatal(err)
	}
	out, err := validateTaskContract(&in, t.TempDir())
	if err != nil {
		t.Fatalf("the contract was rejected for the wrong reason: %v", err)
	}
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "capture_only") {
		t.Error("the stored contract carries an acquisition control")
	}

	r := &ToolResult{Success: true}
	facing, err := json.Marshal(r.ModelFacing())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(facing), "capture") {
		t.Error("the model sees the acquisition control")
	}
}

// The control's reader touches the operator's environment and nothing else.
func TestTheCaptureControlReaderReadsOnlyTheEnvironment(t *testing.T) {
	src, err := os.ReadFile("candidate_capture_only.go")
	if err != nil {
		t.Fatal(err)
	}
	body := codeWithoutComments(string(src))
	reader := body[strings.Index(body, "func candidateCaptureOnly("):]
	reader = reader[:strings.Index(reader, "\n}")]
	if !strings.Contains(reader, "os.Getenv(CandidateCaptureOnlyEnv)") {
		t.Error("the control is not read from operator configuration")
	}
	for _, banned := range []string{"TaskContract", "V3Generate", "Envelope", "ctx",
		"Message", "header", "Header"} {
		if strings.Contains(reader, banned) {
			t.Errorf("the control reader reaches %q", banned)
		}
	}
}

// Structural: every grant in this build is minted at one site, and the control
// is checked before it. A second minting path would be a way around the
// boundary that no behavioural test could see.
func TestEveryGrantCreationCrossesTheCaptureBoundary(t *testing.T) {
	files := proxyFiles(t)
	sites := callSites(files, "mintAuthorizationGrant")
	delete(sites, "authorization_grant.go:mintAuthorizationGrant")
	if len(sites) != 1 {
		t.Fatalf("mintAuthorizationGrant is called from %v, want exactly one site", sites)
	}
	if _, ok := sites["candidate_delivery.go:authorizeCandidateDelivery"]; !ok {
		t.Fatalf("the one minting caller is %v", sites)
	}
	src, err := os.ReadFile("candidate_delivery.go")
	if err != nil {
		t.Fatal(err)
	}
	body := codeWithoutComments(string(src))
	fn := body[strings.Index(body, "func authorizeCandidateDelivery("):]
	guard := strings.Index(fn, "if candidateCaptureOnly() {")
	mint := strings.Index(fn, "mintAuthorizationGrant(")
	if guard < 0 {
		t.Fatal("the acquisition boundary is not on the minting path")
	}
	if mint < 0 || guard > mint {
		t.Fatal("a grant can be minted before the boundary is consulted")
	}
	if !strings.Contains(fn[guard:mint], "return auth") {
		t.Error("the boundary does not stop before minting")
	}
}

// The suppression keeps the causal information rather than flattening it.
func TestSuppressionDoesNotRewriteTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		decision candidatePolicyDecision
		want     string
	}{
		{PolicyCandidateAuthorizedStrict, CaptureWouldAuthorizeStrict},
		{PolicyCandidateAutomaticV3, CaptureWouldDeliverAutomaticV3},
		{PolicyCandidateRejectedHardVeto, CaptureRejectedHardVeto},
		{PolicyBaselineRetained, CaptureBaselineRetained},
	} {
		if got := captureOnlyDispositionFor(tc.decision); got != tc.want {
			t.Errorf("%s mapped to %q, want %q", tc.decision, got, tc.want)
		}
	}
	// The suppression fact is its own member and is never a policy decision.
	if candidatePolicyDecisions[candidatePolicyDecision(CaptureSuppressedDelivery)] {
		t.Error("the suppression became a policy decision")
	}
}

// Path, identity and permission rules are untouched by the control.
func TestCaptureOnlyWeakensNothingElse(t *testing.T) {
	t.Setenv(CandidateCaptureOnlyEnv, "1")
	ctx := policyContext(t)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-capture-only")
	entry := mintRouteEntry(ctx)
	path := filepath.Join(ctx.WorkingDir, "solve.py")
	if scope := testMutationScope(ctx, entry, path, "x = 1\n"); !scope.valid() {
		t.Fatal("no scope")
	}
	// An out-of-workspace target still has no scope, control or no control.
	if _, ok := deriveMutationScope(ctx, entry, "write_file", "/etc/passwd", "", "x\n"); ok {
		t.Error("the control widened the path boundary")
	}
	// And a vetoed candidate is still vetoed.
	out := decideCandidatePolicy(ctx, advisoryInput{
		Observed: checkOutcome{Status: ValidationFailed}, TargetDeclared: true,
		TargetAuthorized: true, ScopeAdmits: true, CaptureOnlySuppressed: true,
	}, true)
	if out.Decision != PolicyCandidateRejectedHardVeto || out.Delivers {
		t.Errorf("veto precedence changed under the control: %+v", out)
	}
}
