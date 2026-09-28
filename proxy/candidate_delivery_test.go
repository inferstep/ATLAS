package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Spending an authorization on actual bytes.
//
// The grant tests prove a licence can be issued and spent once. These prove
// what happens at the filesystem: the exact authorized bytes land or nothing
// does, a write that fails claims nothing, and a delivery that does not settle
// is never reported as one.

// deliveryWorld is an authorized candidate with a live grant, ready to spend.
type deliveryWorld struct {
	*grantWorld
	grant *authorizationGrant
	check checkOutcome
}

func newDeliveryWorld(t *testing.T, commands ...string) *deliveryWorld {
	t.Helper()
	w := newGrantWorld(t, commands...)
	if !w.decision.Authorized {
		t.Fatalf("the fixture is not authorized: %s", w.decision.Reason)
	}
	return &deliveryWorld{
		grantWorld: w,
		grant:      w.mint(t),
		check:      fallbackSyntaxOutcomeFor(w.ctx, w.path, w.code).aggregate(),
	}
}

func (w *deliveryWorld) onDisk(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const deliveredCode = "print(42)\n"

// --- the authorized case ---------------------------------------------------------

func TestAnAuthorizedCandidateLandsByteForByte(t *testing.T) {
	w := newDeliveryWorld(t)
	// The grant is for w.code, and w.code is what gets written.
	res, out, err := deliverAuthorizedCandidate(w.ctx, w.path, w.code, w.grant, w.check, nil, true)
	if err != nil {
		t.Fatalf("an authorized delivery failed: %v", err)
	}
	if !out.Delivered {
		t.Fatalf("delivery not reported: %s", out.Reason)
	}
	if got := w.onDisk(t); got != w.code {
		t.Errorf("disk holds %q, want the authorized bytes exactly", got)
	}
	if out.Hash != contentSHA256(w.code) {
		t.Error("the outcome does not name the bytes that landed")
	}
	if res.AuthorizedDeliveryHash != w.grant.CandidateHash {
		t.Error("the result does not name the authorization it spent")
	}
	if !res.Success {
		t.Error("a settled delivery is not reported successful")
	}
	// And the licence is spent: a second delivery of the same bytes refuses.
	if _, out2, err2 := deliverAuthorizedCandidate(w.ctx, w.path, w.code, w.grant, w.check, nil, true); err2 == nil {
		t.Errorf("a spent grant delivered again (%+v)", out2)
	}
}

func TestDeliveryWritesTheBytesUnaltered(t *testing.T) {
	// No trailing newline, odd whitespace, and a shape a normaliser would be
	// tempted to fix. What was authorized is what must land.
	for _, body := range []string{
		"x = 1", "x = 1\n\n\n", "\tif True:\n\t\tpass\n", "print('a')\r\nprint('b')\r\n",
	} {
		w := newGrantWorldWithCode(t, body)
		g, ok, why := mintAuthorizationGrant(w.ctx, w.in, w.decision, "s", grantBasisStrict)
		if !ok {
			t.Fatalf("%q: no grant (%s)", body, why)
		}
		check := fallbackSyntaxOutcomeFor(w.ctx, w.path, body).aggregate()
		_, out, err := deliverAuthorizedCandidate(w.ctx, w.path, body, g, check, nil, true)
		if err != nil || !out.Delivered {
			t.Fatalf("%q: not delivered (%s, %v)", body, out.Reason, err)
		}
		got, err := os.ReadFile(w.path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Errorf("delivery altered the bytes: wrote %q, disk holds %q", body, string(got))
		}
	}
}

// --- refused before anything moves -----------------------------------------------

func TestABeforeWriteMismatchMutatesNothing(t *testing.T) {
	for name, mutate := range map[string]func(*deliveryWorld) (string, string){
		"other bytes": func(w *deliveryWorld) (string, string) {
			return w.path, deliveredCode
		},
		"another path": func(w *deliveryWorld) (string, string) {
			return w.ctx.WorkingDir + "/elsewhere.py", w.code
		},
	} {
		w := newDeliveryWorld(t)
		before := w.onDisk(t)
		path, code := mutate(w)
		res, out, err := deliverAuthorizedCandidate(w.ctx, path, code, w.grant, w.check, nil, true)
		if err == nil {
			t.Errorf("%s: delivery was allowed", name)
		}
		if res != nil {
			t.Errorf("%s: a refused delivery produced a result", name)
		}
		if out.Delivered {
			t.Errorf("%s: a refusal claimed delivery", name)
		}
		if got := w.onDisk(t); got != before {
			t.Errorf("%s: the target was mutated", name)
		}
		// The grant is untouched, so the honest delivery can still happen.
		if liveGrantCount(w.ctx) != 1 {
			t.Errorf("%s: a refused delivery spent the grant", name)
		}
	}
}

func TestAnInterveningMutationRefusesTheDelivery(t *testing.T) {
	w := newDeliveryWorld(t)
	// Something rewrote the target between the authorization and the write.
	if err := os.WriteFile(w.path, []byte("SOMEONE ELSE = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, out, err := deliverAuthorizedCandidate(w.ctx, w.path, w.code, w.grant, w.check, nil, true)
	if err == nil || res != nil || out.Delivered {
		t.Fatal("a delivery ran against a target that had changed underneath it")
	}
	if got := w.onDisk(t); got != "SOMEONE ELSE = 1\n" {
		t.Error("the refused delivery mutated the target anyway")
	}
}

func TestATombstonedTargetRefusesTheDelivery(t *testing.T) {
	w := newDeliveryWorld(t)
	tombstoneDeliverable(w.ctx, w.path, "deleted")
	if _, out, err := deliverAuthorizedCandidate(w.ctx, w.path, w.code, w.grant, w.check, nil, true); err == nil {
		t.Errorf("a delivery resurrected a deliberately removed file (%+v)", out)
	}
}

func TestACancelledRequestDeliversNothing(t *testing.T) {
	w := newDeliveryWorld(t)
	before := w.onDisk(t)
	cancelled, cancel := context.WithCancel(
		context.WithValue(context.Background(), requestIDKey, requestIDOf(w.ctx)))
	cancel()
	w.ctx.Ctx = cancelled
	if _, out, err := deliverAuthorizedCandidate(w.ctx, w.path, w.code, w.grant, w.check, nil, true); err == nil {
		t.Errorf("a cancelled request delivered (%+v)", out)
	}
	if got := w.onDisk(t); got != before {
		t.Error("a cancelled request mutated the target")
	}
}

func TestDeliveryWithoutAGrantRefuses(t *testing.T) {
	w := newDeliveryWorld(t)
	before := w.onDisk(t)
	if _, _, err := deliverAuthorizedCandidate(w.ctx, w.path, w.code, nil, w.check, nil, true); err == nil {
		t.Error("a delivery with no authorization at all was allowed")
	}
	if got := w.onDisk(t); got != before {
		t.Error("an unauthorized delivery mutated the target")
	}
}

// --- after the write --------------------------------------------------------------

func TestAPostWriteValidationFailureNeverClaimsDelivery(t *testing.T) {
	w := newDeliveryWorld(t)
	// The bytes are what was authorized; the observation about them failed.
	failed := checkOutcome{Status: ValidationFailed, Detail: "SyntaxError: invalid syntax"}
	res, out, err := deliverAuthorizedCandidate(w.ctx, w.path, w.code, w.grant, failed, nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Delivered {
		t.Error("a delivery whose validation failed was reported delivered")
	}
	if out.Reason != "post_write_validation_failed" {
		t.Errorf("reason %q, want post_write_validation_failed", out.Reason)
	}
	if res.Success {
		t.Error("a delivery that did not settle is reported successful")
	}
	if res.AuthorizedDeliveryHash != "" {
		t.Error("a delivery that did not settle named an authorization")
	}
	if res.ValidationStatus != ValidationFailed {
		t.Errorf("validation reported as %q", res.ValidationStatus)
	}
}

func TestRestorationRehashesRatherThanTrustingTheLedger(t *testing.T) {
	w := newAuthWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
		"solve.py", authPy, true)
	// A demonstrably-valid checkpoint to return to, established BEFORE
	// anything is authorized: the grant binds the target's ledger generation,
	// so putting a checkpoint in afterwards would be exactly the intervening
	// change the delivery is meant to refuse.
	observeDeliverable(w.ctx, w.path, []byte(authPy),
		ValidationKindSyntax, ValidationPassed, "")

	ev, evID := w.mustObserve(t)
	a := w.authorize(evID, nil, ev)
	if a.Grant == nil {
		t.Fatalf("no grant: %s (%s)", a.Refusal, a.Decision.Reason)
	}
	failed := checkOutcome{Status: ValidationFailed, Detail: "SyntaxError"}
	_, out, err := deliverAuthorizedCandidate(w.ctx, w.path, w.code, a.Grant, failed, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if out.Delivered {
		t.Fatal("a failed delivery was reported delivered")
	}
	// Whether restoration was possible is the restorer's call; what must hold
	// either way is that the outcome says so truthfully and nothing claims a
	// delivery. When it did restore, disk holds the checkpoint -- rehashed by
	// the restorer, not asserted from the ledger.
	if !out.Restored && out.Reason == "" {
		t.Error("no restoration and no reason")
	}
	if out.Restored {
		got, readErr := os.ReadFile(w.path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(got) != authPy {
			t.Errorf("restoration reported but disk holds %q", string(got))
		}
	}
}

func TestBytesThatDoNotLandAreNotDelivered(t *testing.T) {
	w := newDeliveryWorld(t)
	// A grant whose candidate hash names bytes other than the ones written.
	// This is the shape a normaliser or a repair pass would produce, and the
	// delivery has to notice rather than report success.
	w.ctx.grantMu.Lock()
	w.ctx.grants[w.grant.ID].CandidateHash = contentSHA256("something else\n")
	forged := *w.ctx.grants[w.grant.ID]
	w.ctx.grantMu.Unlock()

	_, out, err := deliverAuthorizedCandidate(w.ctx, w.path, w.code, &forged, w.check, nil, true)
	if err == nil {
		t.Fatal("bytes that are not the authorized ones were delivered")
	}
	if out.Delivered {
		t.Error("a mismatch claimed delivery")
	}
	if out.Reason != "candidate_hash_mismatch" {
		t.Errorf("reason %q, want candidate_hash_mismatch", out.Reason)
	}
}

// --- what the delivery reports ----------------------------------------------------

func TestTheDeliveryResultIsSeparateFromThePoolLabel(t *testing.T) {
	// `delivered` in a V3 pool record is the service describing what it
	// selected, written before anything reached this filesystem. The live
	// result is a statement about disk, made afterwards, by the side that
	// wrote it. A test reads both names to keep them from converging.
	src, err := os.ReadFile("candidate_delivery.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "AuthorizedDeliveryHash") {
		t.Error("the live delivery result is gone")
	}
	for _, borrowed := range []string{
		"Evidence.Delivery", "DescribesDeliveredCandidate", "DeliveredContentHash",
		"v3Result.Passed", "PhaseSolved", "WinningScore",
	} {
		if strings.Contains(body, borrowed) {
			t.Errorf("the delivery owner reads %s: the pool's own label is history, "+
				"not a statement about disk", borrowed)
		}
	}
}

func TestTheDeliveryOwnerLeaksNoContent(t *testing.T) {
	const secret = "TOKEN = 'hunter2'\nprint(7)\n"
	w := newGrantWorldWithCode(t, secret)
	g, ok, why := mintAuthorizationGrant(w.ctx, w.in, w.decision, "s", grantBasisStrict)
	if !ok {
		t.Fatalf("no grant: %s", why)
	}
	failed := checkOutcome{Status: ValidationFailed, Detail: "SyntaxError: line 1"}
	_, out, _ := deliverAuthorizedCandidate(w.ctx, w.path, secret, g, failed, nil, true)
	for _, needle := range []string{secret, "hunter2", "TOKEN", "print(7)", "SyntaxError"} {
		if strings.Contains(out.Reason, needle) {
			t.Errorf("the outcome reason carries %q", needle)
		}
	}
}

// --- the live route ---------------------------------------------------------------

// routeWorld drives the real write path with a V3 service that returns an
// authorized winner, so the routing rules are exercised end to end rather than
// asserted about a function called in isolation.
type routeWorld struct {
	ctx    *AgentContext
	dir    string
	path   string
	winner string
	shell  *stubSandbox
	gen    *int32
	// shellGone makes the staging executor unreachable while the structural
	// gate keeps answering, which is the shape of a partial outage.
	shellGone *bool
}

// The two fields a service record carries about its own verdict. They travel
// together in production -- a record that is not closure-eligible is not a
// verified winner -- so the fixture keeps them consistent.
func evidenceStrengthFor(closureEligible bool) string {
	if closureEligible {
		return "behavioral"
	}
	return "syntax"
}

func selectionStatusFor(closureEligible bool) string {
	if closureEligible {
		return "verified_winner"
	}
	return "best_not_closure_eligible"
}

const routeBaseline = "def solve(values):\n    total = 0\n    for v in values:\n        total += v\n    return total\n"
const routeWinner = "def solve(values):\n    return sum(values)\n"

func newRouteWorld(t *testing.T, contract string, commands map[string]stubEffect) *routeWorld {
	t.Helper()
	return newRouteWorldWithClosure(t, contract, commands, true)
}

// newRouteWorldWithClosure is the same world with the service's own closure
// verdict as a knob. An uncertified proposal is the ordinary case for real
// code -- the python adapter reaches syntax and the default floor is
// behavioral -- so the routing rules have to be exercised for it too.
func newRouteWorldWithClosure(t *testing.T, contract string,
	commands map[string]stubEffect, closureEligible bool) *routeWorld {
	t.Helper()
	dir := t.TempDir()
	winnerHash := contentSHA256(routeWinner)
	stub := newStubSandbox(t)
	for cmd, effect := range commands {
		stub.script(cmd, effect)
	}
	var generateCalls int32
	shellGone := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v3/generate":
			atomic.AddInt32(&generateCalls, 1)
			body, _ := json.Marshal(map[string]interface{}{
				"code": routeWinner, "passed": true, "phase_solved": "phase_one",
				"candidates_tested": 3, "winning_score": 0.9,
				"evidence": map[string]interface{}{
					"wire_version": "1.0.0", "record_schema_version": "1.1.0",
					"identity": map[string]interface{}{
						"contract_id": "c.v1", "contract_version": "1",
						"adapter_id": "python_compile", "adapter_version": "0.1.0-prototype",
						"artifact_scope": "solve.py", "evaluation_context_hash": "ctx",
						"candidate_content_hash": winnerHash,
					},
					"evaluation": map[string]interface{}{
						"execution_status": "ok", "supported": true,
						"evidence_strength":     evidenceStrengthFor(closureEligible),
						"requirements_complete": true,
						"closure_eligible":      closureEligible,
						"quality": map[string]interface{}{
							"required_coverage": 1.0, "optional_quality": 1.0, "overall": 1.0},
					},
					"coverage":  map[string]interface{}{"required": []string{}, "demonstrated": []string{}},
					"selection": map[string]interface{}{"status": selectionStatusFor(closureEligible), "reason": "highest"},
					"delivery": map[string]interface{}{
						"delivered_content_hash": winnerHash, "describes_delivered_candidate": true},
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
			if shellGone {
				// The connection dies mid-request: a transport failure, not a
				// refusal, which is what an executor that is not there looks
				// like from here.
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						conn.Close()
						return
					}
				}
				return
			}
			stub.srv.Config.Handler.ServeHTTP(w, r)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-route")
	ctx.V3URL, ctx.SandboxURL = srv.URL, srv.URL
	ctx.HumanTask = "Make solve fast."
	if contract != "" {
		ctx.TaskContract = mustContract(t, dir, contract)
	}
	return &routeWorld{ctx: ctx, dir: dir, path: filepath.Join(dir, "solve.py"),
		winner: routeWinner, shell: stub, gen: &generateCalls, shellGone: &shellGone}
}

// generateCalls is how many times the pipeline was actually asked for
// candidates. Feasibility is observe-only, so this must not move.
func (w *routeWorld) generateCalls() int { return int(atomic.LoadInt32(w.gen)) }

func (w *routeWorld) write(t *testing.T) (*ToolResult, error) {
	t.Helper()
	return writeFileWithV3(w.path, routeBaseline, w.ctx)
}

func (w *routeWorld) disk(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTheTypedRouteDeliversAnAuthorizedWinner(t *testing.T) {
	w := newRouteWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`, nil)
	res, err := w.write(t)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("delivery failed: %+v", res)
	}
	if got := w.disk(t); got != routeWinner {
		t.Errorf("disk holds %q, want the authorized winner", got)
	}
	if res.AuthorizedDeliveryHash != contentSHA256(routeWinner) {
		t.Error("the result does not name the authorization it spent")
	}
	if !res.V3Used {
		t.Error("a delivered candidate does not report the pipeline that produced it")
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Error("the grant was not spent")
	}
}

func TestATypedRefusalKeepsTheCallersContent(t *testing.T) {
	// A declared command with no staging result: the obligation is owed, so
	// the typed answer refuses and the caller's own content is kept.
	w := newRouteWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"],`+
			`"verification_knowledge":"declared","verification":["pytest -q"]}`,
		map[string]stubEffect{"pytest -q": {ExitCode: 1}})
	res, err := w.write(t)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("the caller's own content was not written: %+v", res)
	}
	if got := w.disk(t); got != routeBaseline {
		t.Errorf("disk holds %q, want the caller's own content", got)
	}
	if res.V3Used {
		t.Error("a refused candidate reported V3 provenance")
	}
	if res.AuthorizedDeliveryHash != "" {
		t.Error("a refused candidate named an authorization")
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Error("a refusal left a live grant behind")
	}
}

// A request with no contract, or a question, keeps the MODEL's bytes.
//
// It used to keep the service's, on the service's own closure verdict, which
// is the producer of a candidate certifying that candidate. There is no target
// the client named and no work the model's own call could ground, so nothing
// here can be authorized against, and the honest answer is the caller's own
// content. A work request is different: its own call names the target.
func TestContractlessTrafficKeepsItsPreviousRoute(t *testing.T) {
	for _, contract := range []string{"", `{"task_mode":"question"}`} {
		w := newRouteWorld(t, contract, nil)
		res, err := w.write(t)
		if err != nil {
			t.Fatalf("%q: write failed: %v", contract, err)
		}
		if res == nil || !res.Success {
			t.Fatalf("%q: delivery failed: %+v", contract, res)
		}
		// The model's own bytes land, through the model's own path, exactly as
		// if the pipeline had not run. No licence is minted and none is spent.
		if got := w.disk(t); got != routeBaseline {
			t.Errorf("%q: disk holds %q, want the caller's own content", contract, got)
		}
		if res.AuthorizedDeliveryHash != "" {
			t.Errorf("%q: contractless traffic spent an authorization", contract)
		}
		if res.V3Used {
			t.Errorf("%q: the service's own verdict delivered a candidate", contract)
		}
		if res.DeliveryProvenance != DeliveryFromModelProposal {
			t.Errorf("%q: provenance %q, want the model's own proposal",
				contract, res.DeliveryProvenance)
		}
		if liveGrantCount(w.ctx) != 0 {
			t.Errorf("%q: a licence outlived a contractless request", contract)
		}
	}
}

func TestOneCandidateGetsOneAuthorizationAnswer(t *testing.T) {
	w := newRouteWorld(t,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`, nil)
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})
	if got := recordsOfKind(recs, "candidate_authorization_decision"); len(got) != 1 {
		t.Errorf("%d authorization decisions for one candidate, want exactly one", len(got))
	}
	consumed := 0
	for _, r := range recordsOfKind(recs, "authorization_grant_event") {
		if r["event"] == string(grantConsumedAuthorized) {
			consumed++
		}
	}
	if consumed != 1 {
		t.Errorf("%d grants consumed for one candidate, want exactly one", consumed)
	}
}

func TestNothingIsDeliveredAfterTerminalEmission(t *testing.T) {
	w := newGrantWorld(t)
	w.mint(t)
	// The run finished. Whatever else is true, a licence granted during it
	// cannot outlive it.
	retireAuthorizationGrants(w.ctx, grantTerminal)
	check := fallbackSyntaxOutcomeFor(w.ctx, w.path, w.code).aggregate()
	before, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatal(err)
	}
	w.ctx.grantMu.Lock()
	var g *authorizationGrant
	for _, held := range w.ctx.grants {
		g = held
	}
	w.ctx.grantMu.Unlock()
	if _, out, err := deliverAuthorizedCandidate(w.ctx, w.path, w.code, g, check, nil, true); err == nil {
		t.Errorf("a candidate landed after the terminal (%+v)", out)
	}
	after, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("a post-terminal delivery mutated the target")
	}
}

// TestRetirementSitsAtTheEmissionNotTheVerdict pins where retirement lives.
//
// finalizeCompletion is also called on the path that bounces for debt recovery
// and keeps running; retiring there would leave the recovery turn unable to
// deliver the thing it was bounced to write. The emission is the point of no
// return, and it happens once.
func TestRetirementSitsAtTheEmissionNotTheVerdict(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	sites := 0
	for _, fn := range []string{"func emitTerminal(", "func finalizeCompletion("} {
		start := strings.Index(body, fn)
		if start < 0 {
			t.Fatalf("%s is gone", fn)
		}
		end := strings.Index(body[start+1:], "\nfunc ")
		region := body[start:]
		if end >= 0 {
			region = body[start : start+1+end]
		}
		has := strings.Contains(region, "retireAuthorizationGrants(")
		if fn == "func emitTerminal(" {
			if !has {
				t.Error("the terminal emission does not retire authorizations")
			}
			sites++
			// Inside terminalOnce, so a second emission cannot re-retire and
			// a bounce cannot reach it at all.
			if !strings.Contains(region, "ctx.terminalOnce.Do(func() {\n\t\t// Nothing may be delivered") {
				t.Error("retirement is not inside the once-only emission")
			}
		} else if has {
			t.Error("the verdict retires authorizations; a debt-recovery bounce " +
				"would then be unable to deliver what it was bounced to write")
		}
	}
	if sites != 1 {
		t.Errorf("%d retirement sites, want exactly the emission", sites)
	}
}

// TestADebtRecoveryBounceCanStillDeliver is the case that moved it: the run
// did not end, so the licence machinery must still work.
func TestADebtRecoveryBounceCanStillDeliver(t *testing.T) {
	w := newGrantWorld(t)
	g := w.mint(t)
	// finalizeCompletion runs and the loop continues. Nothing was emitted.
	st := &runState{}
	finalizeCompletion(w.ctx, st, "make it fast", "")
	w.ctx.grantMu.Lock()
	still := w.ctx.grants[g.ID]
	off := w.ctx.grantsOff
	w.ctx.grantMu.Unlock()
	if still == nil || still.retired != grantLive {
		t.Error("reaching the verdict retired a live authorization")
	}
	if off != "" {
		t.Errorf("reaching the verdict stopped further minting (%q)", off)
	}
}
