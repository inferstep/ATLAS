package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The candidate V3 selected lands, and only that candidate.
//
// Strict asks whether trusted evidence meets a floor the client declared. A
// request that declared none has no floor, so strict keeps the baseline -- and
// a pipeline that generates K candidates and picks a winner never reaches the
// artifact. automatic_v3 separates the evidence question from the safety
// question: nothing below claims a candidate is correct, and everything that
// was never about evidence still has to hold.

// automaticWorld drives the real write route under automatic_v3, with a
// service that names exactly which candidate it selected.
type automaticWorld struct {
	ctx      *AgentContext
	dir      string
	path     string
	winner   string
	selected *string
	shell    *stubSandbox
}

func newAutomaticWorld(t *testing.T, contract, winner string,
	commands map[string]stubEffect, supported bool) *automaticWorld {
	t.Helper()
	dir := t.TempDir()
	selected := contentSHA256(winner)
	stub := newStubSandbox(t)
	for cmd, effect := range commands {
		stub.script(cmd, effect)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v3/generate":
			body, _ := json.Marshal(map[string]interface{}{
				"code": winner, "passed": true, "phase_solved": "phase_one",
				"candidates_tested": 5, "winning_score": 0.9,
				"evidence": map[string]interface{}{
					"wire_version": "1.0.0", "record_schema_version": "1.1.0",
					"identity": map[string]interface{}{
						"contract_id": "c.v1", "contract_version": "1",
						"adapter_id": "python_compile", "adapter_version": "0.1.0-prototype",
						"artifact_scope": "solve.py", "evaluation_context_hash": "ctx",
						// THE selected candidate, named by content hash. The
						// proxy hashes what it holds and the two must agree.
						"candidate_content_hash": selected,
					},
					"evaluation": map[string]interface{}{
						"execution_status": "ok", "supported": supported,
						"evidence_strength": "syntax", "requirements_complete": true,
						"closure_eligible": false,
						"quality": map[string]interface{}{
							"required_coverage": 1.0, "optional_quality": 1.0, "overall": 1.0},
					},
					"coverage":  map[string]interface{}{"required": []string{}, "demonstrated": []string{}},
					"selection": map[string]interface{}{"status": "best_not_closure_eligible", "reason": "highest"},
					"delivery": map[string]interface{}{
						"delivered_content_hash": selected, "describes_delivered_candidate": true},
				},
			})
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			for _, line := range []string{"event: result", "data: " + string(body), "", "data: [DONE]", ""} {
				fmt.Fprint(w, line+"\n")
				if fl != nil {
					fl.Flush()
				}
			}
		case r.URL.Path == "/internal/structural_check":
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "unresolved": []string{}})
		case r.URL.Path == "/internal/cyclomatic_complexity":
			json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
		case strings.HasSuffix(r.URL.Path, "/shell"):
			stub.srv.Config.Handler.ServeHTTP(w, r)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-automatic")
	ctx.V3URL, ctx.SandboxURL = srv.URL, srv.URL
	ctx.HumanTask = "Make solve fast."
	ctx.TaskContract = mustContract(t, dir, contract)
	return &automaticWorld{ctx: ctx, dir: dir, path: filepath.Join(dir, "solve.py"),
		winner: winner, selected: &selected, shell: stub}
}

func (w *automaticWorld) write(t *testing.T) (*ToolResult, error) {
	t.Helper()
	return writeFileWithV3(w.path, routeBaseline, w.ctx)
}

func (w *automaticWorld) disk(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// automaticTestScope is the structured intent a write_file call would derive
// for the fixture target.
func automaticTestScope(t *testing.T) mutationScope {
	t.Helper()
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-scope")
	return testMutationScope(ctx, mintRouteEntry(ctx), filepath.Join(dir, "solve.py"), routeWinner)
}

const workContract = `{"task_mode":"work","output_knowledge":"declared",` +
	`"expected_outputs":["solve.py"]}`

// --- one rule over one candidate --------------------------------------------

// No declared verification, a safe selected candidate: the exact bytes land,
// terminator and all.
//
// Every hash in the chain -- the service's selected hash, the authorization
// identity, the grant, the disk read -- is computed from the same string, so a
// candidate that ends in two newlines lands with two, and one that ends in
// none lands with none. This is the delivery half of the exact-byte contract
// whose extraction half lives in the V3 service.
func TestAutomaticDeliversTheSelectedCandidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		winner string
	}{
		{"one final newline", routeWinner},
		{"two trailing newlines", routeWinner + "\n"},
		{"no final newline", strings.TrimSuffix(routeWinner, "\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newAutomaticWorld(t, workContract, tc.winner, nil, true)

			res, err := w.write(t)

			if err != nil {
				t.Fatalf("write failed: %v", err)
			}
			if res == nil || !res.Success {
				t.Fatalf("delivery failed: %+v", res)
			}
			if got := w.disk(t); got != tc.winner {
				t.Fatalf("disk holds %q, want the exact candidate %q", got, tc.winner)
			}
			if res.AuthorizedDeliveryHash != contentSHA256(tc.winner) {
				t.Errorf("authorization names %s, disk bytes hash to %s",
					res.AuthorizedDeliveryHash, contentSHA256(tc.winner))
			}
			if got := contentSHA256(w.disk(t)); got != *w.selected {
				t.Errorf("disk hash %s is not the selected hash %s", got, *w.selected)
			}
			if liveGrantCount(w.ctx) != 0 {
				t.Error("the grant was not spent")
			}
		})
	}
}

// An older client may still send a candidate_policy. Every spelling gets the
// same rule and lands the same bytes: nothing a request says selects a mode.
// TestAnUnsupportedAdapterDoesNotBlockAutomaticDelivery is the same request
// without the field.
func TestEveryOlderPolicySpellingLandsTheSameBytes(t *testing.T) {
	for _, policy := range []string{"strict", "advisory", "automatic_v3"} {
		t.Run(policy, func(t *testing.T) {
			contract := strings.TrimSuffix(workContract, "}") + `,"candidate_policy":"` + policy + `"}`
			w := newAutomaticWorld(t, contract, routeWinner, nil, false)
			if _, err := w.write(t); err != nil {
				t.Fatalf("write failed: %v", err)
			}
			if got := w.disk(t); got != routeWinner {
				t.Errorf("disk holds %q, want the selected candidate", got)
			}
		})
	}
}

// The delivery decision reads which basis earned a grant, not the eligibility
// answer: the mint can still refuse what eligibility allowed. A decision that
// said "delivers" over a refused mint would write nothing at all.
func TestTheDecisionReadsTheGrantNotTheEligibility(t *testing.T) {
	w := newAutomaticWorld(t, workContract, routeWinner, nil, true)
	// The request's grants are retired, so minting refuses from here on.
	retireAuthorizationGrants(w.ctx, grantTerminal)
	recs := captureShadow(t, func() {
		res, err := w.write(t)
		if err != nil {
			t.Fatalf("write failed: %v", err)
		}
		if res == nil || !res.Success {
			t.Fatalf("the caller's write did not land: %+v", res)
		}
	})
	if got := w.disk(t); got != routeBaseline {
		t.Errorf("disk holds %q, want the caller's bytes", got)
	}
	decisions := recordsOfKind(recs, "candidate_policy_decision")
	if len(decisions) != 1 {
		t.Fatalf("%d delivery decisions, want 1", len(decisions))
	}
	if decisions[0]["delivers"] == true {
		t.Errorf("the decision %v says it delivers over a grant the mint refused",
			decisions[0]["decision"])
	}
}

// --- selection identity, not score ------------------------------------------

// The proxy delivers only the bytes the selection NAMED. A selection identity
// that is missing, blank, incomplete or names other bytes fails closed.
func TestAWrongOrAmbiguousSelectionIdentityFailsClosed(t *testing.T) {
	base := automaticEligibilityInput{
		SelectedCandidateID: contentSHA256(routeWinner),
		CandidateHash:       contentSHA256(routeWinner),
		Identity: V3EvidenceProvenance{RequestID: "r", InvocationID: "i",
			CandidateInstanceID: "c", WorkspaceStateHash: "w",
			CandidateHash: contentSHA256(routeWinner)},
		Scope:          automaticTestScope(t),
		TargetGrounded: true,
	}
	if ok, _ := automaticDeliveryAllowed(base); !ok {
		t.Fatal("the complete case is not eligible")
	}
	cases := []struct {
		name string
		mut  func(*automaticEligibilityInput)
		want string
	}{
		{"no selection", func(i *automaticEligibilityInput) { i.SelectedCandidateID = "" },
			automaticNoSelection},
		{"blank selection", func(i *automaticEligibilityInput) { i.SelectedCandidateID = "   " },
			automaticNoSelection},
		// The bytes in hand won nothing when the service named other bytes.
		{"selection names other bytes", func(i *automaticEligibilityInput) {
			i.SelectedCandidateID = contentSHA256("def solve(v):\n    return 0\n")
		}, automaticNotTheWinner},
		{"no candidate hash", func(i *automaticEligibilityInput) { i.CandidateHash = "" },
			automaticIdentityIncomplete},
		{"no request id", func(i *automaticEligibilityInput) { i.Identity.RequestID = "" },
			automaticIdentityIncomplete},
		{"no invocation id", func(i *automaticEligibilityInput) { i.Identity.InvocationID = "" },
			automaticIdentityIncomplete},
		{"no candidate instance id", func(i *automaticEligibilityInput) { i.Identity.CandidateInstanceID = "" },
			automaticIdentityIncomplete},
		{"no workspace state", func(i *automaticEligibilityInput) { i.Identity.WorkspaceStateHash = "" },
			automaticIdentityIncomplete},
		{"identity names other bytes", func(i *automaticEligibilityInput) { i.Identity.CandidateHash = "other" },
			automaticIdentityIncomplete},
		{"no scope", func(i *automaticEligibilityInput) { i.Scope = mutationScope{} },
			automaticNoScope},
		{"target not grounded", func(i *automaticEligibilityInput) { i.TargetGrounded = false },
			automaticTargetNotGrounded},
		{"a veto fired", func(i *automaticEligibilityInput) { i.Vetoes = []string{VetoSyntaxOrStructural} },
			automaticHardVeto},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mut(&in)
			ok, why := automaticDeliveryAllowed(in)
			if ok {
				t.Fatalf("%s was eligible", tc.name)
			}
			if why != tc.want {
				t.Errorf("refusal %q, want %q", why, tc.want)
			}
		})
	}
}

// --- absence of an oracle is not failure -------------------------------------

// An adapter that cannot measure this class is unavailable evidence, not
// failed evidence. With no declared requirement, the candidate may still land,
// and its label says it landed under the automatic rule: a user reviewing the
// diff is told where the bytes came from, and nothing about how good they are.
func TestAnUnsupportedAdapterDoesNotBlockAutomaticDelivery(t *testing.T) {
	w := newAutomaticWorld(t, workContract, routeWinner, nil, false)
	res, err := w.write(t)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if got := w.disk(t); got != routeWinner {
		t.Fatalf("an unsupported adapter blocked an otherwise safe candidate: %q", got)
	}
	if res.DeliveryProvenance != DeliveryFromAutomaticV3 {
		t.Errorf("provenance %q, want %q", res.DeliveryProvenance, DeliveryFromAutomaticV3)
	}
}

// --- declared requirements stay binding --------------------------------------

// A declared command binds whether the client named the output or the
// model's own call did. Both targets are staged and checked.
func TestADeclaredCheckStaysBinding(t *testing.T) {
	contracts := map[string]string{
		"declared output": `{"task_mode":"work","output_knowledge":"declared",` +
			`"expected_outputs":["solve.py"],` +
			`"verification_knowledge":"declared","verification":["pytest -q"]}`,
		"target the call named": `{"task_mode":"work",` +
			`"verification_knowledge":"declared","verification":["pytest -q"]}`,
	}
	cases := []struct {
		name   string
		effect stubEffect
		want   string
	}{
		{"it passed", stubEffect{}, routeWinner},
		{"it failed", stubEffect{ExitCode: 1}, routeBaseline},
		{"it timed out", stubEffect{TimedOut: true}, routeBaseline},
		{"it was memory-killed", stubEffect{ExitCode: -9, Outcome: ExecutionMemoryExhausted}, routeBaseline},
		{"it flooded its output", stubEffect{ExitCode: -13, Outcome: ExecutionOutputLimit}, routeBaseline},
	}
	for target, contract := range contracts {
		for _, tc := range cases {
			t.Run(target+"/"+tc.name, func(t *testing.T) {
				w := newAutomaticWorld(t, contract, routeWinner,
					map[string]stubEffect{"pytest -q": tc.effect}, true)
				res, err := w.write(t)
				if err != nil {
					t.Fatalf("write failed: %v", err)
				}
				if res == nil || !res.Success {
					t.Fatalf("the write did not land: %+v", res)
				}
				if got := w.disk(t); got != tc.want {
					t.Errorf("disk holds %q, want %q", got, tc.want)
				}
			})
		}
	}
}

// --- every hard veto still refuses -------------------------------------------

func TestEveryHardVetoStillRefusesUnderAutomatic(t *testing.T) {
	for _, veto := range []string{
		VetoSyntaxOrStructural, VetoLanguageOrTargetMismatch,
		VetoUnauthorizedPathExpansion, VetoCancelledOrTimedOut,
		VetoDestructiveWithoutPermission, VetoMutatedProtectedAssets,
		VetoOutsideMutationScope, VetoDeclaredVerificationFailed,
		VetoExecutionUnavailable, VetoIncompleteEvidence,
		VetoWeakerThanBaseline, VetoStaleIdentity,
	} {
		out := decideCandidatePolicy(policyContext(t),
			advisoryInput{
				Observed:          checkOutcome{Status: ValidationPassed},
				TargetDeclared:    true,
				TargetAuthorized:  true,
				ScopeAdmits:       true,
				AutomaticEligible: true,
				Vetoes:            []string{veto},
			}, false)
		if out.Decision != PolicyCandidateRejectedHardVeto {
			t.Errorf("%s decided %q under automatic", veto, out.Decision)
		}
		if out.Delivers {
			t.Errorf("%s delivered under automatic", veto)
		}
	}
}

// --- capture-only ------------------------------------------------------------

func TestCaptureOnlySuppressesAutomaticDelivery(t *testing.T) {
	t.Setenv(CandidateCaptureOnlyEnv, "1")
	// Unsupported adapter, so the decision under test is the automatic one
	// rather than a strict authorization that would have happened anyway.
	w := newAutomaticWorld(t, workContract, routeWinner, nil, false)
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatalf("write failed: %v", err)
		}
	})
	if got := w.disk(t); got != routeBaseline {
		t.Fatalf("capture-only let a candidate land: %q", got)
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Error("a grant survived a suppressed delivery")
	}
	dispositions := recordsOfKind(recs, "candidate_capture_only_disposition")
	if len(dispositions) != 1 {
		t.Fatalf("%d capture-only dispositions, want 1", len(dispositions))
	}
	if dispositions[0]["would_have"] != CaptureWouldDeliverAutomaticV3 {
		t.Errorf("would_have %v, want %s", dispositions[0]["would_have"],
			CaptureWouldDeliverAutomaticV3)
	}
	if dispositions[0]["delivered"] == true {
		t.Error("a suppressed disposition claims a delivery")
	}
	for _, rec := range recordsOfKind(recs, "authorization_grant_event") {
		t.Errorf("a grant event survived capture-only: %v", rec["event"])
	}
}

// --- structural ownership -----------------------------------------------------

// Every automatic candidate goes through the shared authorization owner and
// the shared exact-byte delivery owner. There is no second path.
func TestAutomaticSharesTheOneAuthorizationAndDeliveryOwner(t *testing.T) {
	body := map[string]string{}
	for _, f := range append([]string{"automatic_delivery.go", "candidate_delivery.go",
		"authorization_grant.go", "advisory_policy.go", "edit_route_delivery.go"}, toolFiles...) {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		body[f] = codeWithoutComments(string(src))
	}
	// The eligibility owner decides nothing about bytes: it neither writes,
	// nor mints, nor reaches a producer.
	for _, banned := range []string{"os.WriteFile", "writeFileRecorded", "mintAuthorizationGrant",
		"http.", "exec.", "callV3"} {
		if strings.Contains(body["automatic_delivery.go"], banned) {
			t.Errorf("the automatic owner reaches %q", banned)
		}
	}
	// One minting site, still.
	if n := strings.Count(body["candidate_delivery.go"], "mintAuthorizationGrant("); n != 1 {
		t.Errorf("%d minting sites in the delivery owner, want 1", n)
	}
	total := 0
	for f, src := range body {
		if f == "authorization_grant.go" {
			continue
		}
		total += strings.Count(src, "mintAuthorizationGrant(")
	}
	if total != 1 {
		t.Errorf("%d production minting call sites, want exactly one", total)
	}
	// The automatic basis cannot be minted without the eligibility answer.
	grant := body["authorization_grant.go"]
	if !strings.Contains(grant, "basis == grantBasisStrict") {
		t.Error("the evidence preconditions are no longer scoped to the strict basis")
	}
	if !strings.Contains(grant, "basis == grantBasisAutomaticV3") {
		t.Error("the automatic basis does not carry its declared-command requirement")
	}
}

// The policy and the vetoes are decided once, in the authorization owner, and
// the two routes hand it their facts.
func TestTheRoutesHandTheirFactsToTheOneAuthorizationOwner(t *testing.T) {
	body := map[string]string{}
	for _, f := range append([]string{"candidate_delivery.go", "edit_route_delivery.go"}, toolFiles...) {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		body[f] = codeWithoutComments(string(src))
	}
	// The delivery file does not look up the policy for itself.
	for _, banned := range []string{"candidatePolicyOf", "decideCandidatePolicy"} {
		if strings.Contains(body["candidate_delivery.go"], banned) {
			t.Errorf("the delivery owner re-decides the policy via %s", banned)
		}
	}
	// The authorization owner computes the vetoes once, after its decision,
	// and both routes hand it their facts rather than computing a list of
	// their own that the decision cannot reach.
	if n := strings.Count(body["candidate_delivery.go"], "advisoryVetoes("); n != 1 {
		t.Errorf("the authorization owner computes the vetoes %d times, want once", n)
	}
	for _, f := range append([]string{"edit_route_delivery.go"}, toolFiles...) {
		if n := strings.Count(body[f], "advisoryVetoes("); n != 0 {
			t.Errorf("%s computes the vetoes itself (%d times)", f, n)
		}
	}
	for _, route := range []string{"tool_write_v3.go", "edit_route_delivery.go"} {
		if !strings.Contains(body[route], "automaticIntent{") {
			t.Errorf("%s does not hand the authorization owner its intent", route)
		}
	}
}

// No approval prompt exists for a candidate, in any form.
func TestNoCandidateApprovalSurfaceExists(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		src, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		body := codeWithoutComments(string(src))
		for _, banned := range []string{
			"CandidatePolicyConfirm", "PolicyHumanConfirmationRequired",
			"DeliveryFromHumanApproval", "CaptureWouldRequireHumanConfirmation",
			"candidate_approval", "approveCandidate", "awaitCandidateApproval",
		} {
			if strings.Contains(body, banned) {
				t.Errorf("%s carries a candidate-approval surface: %s", n, banned)
			}
		}
	}
}

// A declared output whose syntax check could not run is not a check that
// passed. The hard vetoes are computed after the authorization decision and
// after the structural classification that records "not run", once, for the
// automatic eligibility and the policy alike; computed before them, the veto
// could never fire.
func TestAnAutomaticCandidateDoesNotLandWhenItsDeclaredCheckNeverRan(t *testing.T) {
	w := newAutomaticWorld(t, workContract, routeWinner, nil, true)
	origin, err := url.Parse(w.ctx.SandboxURL)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(origin)
	checkerDown := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			http.Error(rw, "unavailable", http.StatusServiceUnavailable)
			return
		}
		forward.ServeHTTP(rw, r)
	}))
	t.Cleanup(checkerDown.Close)
	w.ctx.SandboxURL = checkerDown.URL

	w.write(t)
	if w.disk(t) == routeWinner {
		t.Fatal("the candidate landed although its declared syntax check never ran")
	}
}

// A declared command is staged even for a file type the syntax gate does not
// check. Only checked extensions carry a syntax obligation, so if staging
// waited for one, a declared main.rs would never run its declared command and
// its candidate could never be authorized.
func TestADeclaredCommandIsStagedForAnUncheckedFileType(t *testing.T) {
	const command = "cargo run -q"
	const rustWinner = "fn main() {\n    println!(\"7\");\n}\n"
	const rustBaseline = "fn main() {\n    println!(\"6\");\n}\n"
	w := newAutomaticWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["main.rs"],`+
			`"verification_knowledge":"declared","verification":["`+command+`"]}`,
		rustWinner, map[string]stubEffect{command: {ExitCode: 0}}, true)
	w.path = filepath.Join(w.dir, "main.rs")

	if _, err := writeFileWithV3(w.path, rustBaseline, w.ctx); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if n := w.shell.runsOf(command); n != 1 {
		t.Fatalf("the declared command ran %d times, want once", n)
	}
	if got := w.disk(t); got != rustWinner {
		t.Errorf("the candidate did not land on its passing declared command; disk holds %q", got)
	}
}
