package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The interactive request. A person typed into the TUI; the client declared
// work and, by explicit selection, automatic_v3 -- and nothing else, because
// it knows nothing structured about which files the task requires. The model
// then made a write_file call naming one target. That call is the structured
// mutation target, and it is what may ground the automatic delivery.
const tuiAutomaticContract = `{"task_mode":"work","candidate_policy":"automatic_v3"}`

func TestTUIAutomaticRequestDeliversTheSelectedCandidateToTheStructuredTarget(t *testing.T) {
	w := newAutomaticWorld(t, tuiAutomaticContract, routeWinner, nil, true)
	res, err := w.write(t)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("delivery failed: %+v", res)
	}
	if got := w.disk(t); got != routeWinner {
		t.Fatalf("disk holds the baseline; the selected candidate was not delivered to the structured target")
	}
	if res.AuthorizedDeliveryHash != contentSHA256(routeWinner) {
		t.Error("the result does not name the authorization it spent")
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Error("the grant was not spent")
	}
}

// --- The behaviour matrix, through the real owners -----------------------------

const (
	tuiStrictContract   = `{"task_mode":"work","candidate_policy":"strict"}`
	tuiAdvisoryContract = `{"task_mode":"work","candidate_policy":"advisory"}`
	tuiDefaultContract  = `{"task_mode":"work"}`
	tuiQuestionAuto     = `{"task_mode":"question","candidate_policy":"automatic_v3"}`
)

func expectBaselineKept(t *testing.T, w *automaticWorld, why string) {
	t.Helper()
	res, err := w.write(t)
	if err != nil {
		t.Fatalf("%s: write failed: %v", why, err)
	}
	if res == nil || !res.Success {
		t.Fatalf("%s: the write itself failed: %+v", why, res)
	}
	if got := w.disk(t); got != routeBaseline {
		t.Fatalf("%s: disk holds %q, want the model's own bytes", why, got)
	}
	if res.AuthorizedDeliveryHash != "" {
		t.Errorf("%s: an authorization was spent", why)
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Errorf("%s: a grant is still live", why)
	}
}

// The structured target grounds the same under every policy spelling an older
// client may send, and under none: nothing a request says selects a mode.
func TestStructuredTargetGroundsTheSameUnderEveryOlderSpelling(t *testing.T) {
	for name, contract := range map[string]string{
		"omitted policy": tuiDefaultContract, "explicit strict": tuiStrictContract,
		"advisory": tuiAdvisoryContract, "automatic_v3": tuiAutomaticContract,
	} {
		t.Run(name, func(t *testing.T) {
			w := newAutomaticWorld(t, contract, routeWinner, nil, true)
			res, err := w.write(t)
			if err != nil || res == nil || !res.Success {
				t.Fatalf("the write did not land: %+v, %v", res, err)
			}
			if got := w.disk(t); got != routeWinner {
				t.Errorf("disk holds %q, want the selected candidate", got)
			}
			if res.AuthorizedDeliveryHash != contentSHA256(routeWinner) {
				t.Errorf("no authorization was spent on the candidate")
			}
		})
	}
}

// A question can create no mutation authority, whatever it selected.
func TestQuestionModeGetsNoAutomaticAuthority(t *testing.T) {
	w := newAutomaticWorld(t, tuiQuestionAuto, routeWinner, nil, true)
	expectBaselineKept(t, w, "question with automatic_v3")
}

// The structured target is the model's own canonical path, however the call
// spelled it. The write tool canonicalizes with resolveAgentPath before the
// route runs, the scope owner canonicalizes with resolveWorkspacePath and
// refuses if the two disagree, and both name one target for every spelling.
func TestStructuredTargetIsCanonicalAcrossAliasSpellings(t *testing.T) {
	w := newAutomaticWorld(t, tuiAutomaticContract, routeWinner, nil, true)
	for _, spelling := range []string{"solve.py", "./solve.py", "./sub/../solve.py"} {
		agent := resolveAgentPath(w.ctx, spelling)
		workspace, err := resolveWorkspacePath(w.ctx, spelling)
		if err != nil || agent != w.path || workspace != w.path {
			t.Fatalf("%q resolved to %q / %q (err %v), want %q", spelling, agent, workspace, err, w.path)
		}
		scope, ok := deriveMutationScope(w.ctx, mintRouteEntry(w.ctx), "write_file", spelling, "", routeBaseline)
		if !ok || scope.Target != w.path {
			t.Fatalf("%q derived scope ok=%v target=%q, want %q", spelling, ok, scope.Target, w.path)
		}
	}
	// And the delivery itself, on the path the tool hands the route after
	// canonicalizing an alias.
	res, err := writeFileWithV3(resolveAgentPath(w.ctx, "./solve.py"), routeBaseline, w.ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("write failed: %v %+v", err, res)
	}
	if got := w.disk(t); got != routeWinner {
		t.Fatalf("the alias-spelled call did not deliver to the structured target: %q", got)
	}
}

// The service's selected hash must be the bytes that arrived. A service that
// selected something else, or nothing, delivers nothing.
func TestStructuredRouteRefusesASelectionThatIsNotTheBytes(t *testing.T) {
	w := newAutomaticWorld(t, tuiAutomaticContract, routeWinner, nil, true)
	*w.selected = contentSHA256(routeWinner + "// other\n")
	expectBaselineKept(t, w, "selected hash names other bytes")
}

// Every hard veto refuses under the structured route exactly as under a
// declared target. The policy owner is asked in the shape the structured route
// hands it -- no declared target, automatic eligible -- for every veto.
func TestStructuredRouteStillHonoursHardVetoes(t *testing.T) {
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
				TargetDeclared:    false,
				TargetAuthorized:  false,
				ScopeAdmits:       true,
				AutomaticEligible: true,
				Vetoes:            []string{veto},
			}, false)
		if out.Decision != PolicyCandidateRejectedHardVeto || out.Delivers {
			t.Errorf("%s: decided %q delivers=%v under the structured route", veto, out.Decision, out.Delivers)
		}
	}
	// And one veto reached through the real route: a request that ended
	// before the write derives no scope, fires the cancellation veto, and keeps
	// the model's own bytes.
	w := newAutomaticWorld(t, tuiAutomaticContract, routeWinner, nil, true)
	cancelled, cancel := context.WithCancel(w.ctx.Ctx)
	cancel()
	w.ctx.Ctx = cancelled
	res, _ := w.write(t)
	if b, err := os.ReadFile(w.path); err == nil && string(b) == routeWinner {
		t.Fatal("a cancelled request delivered a candidate")
	}
	if res != nil && res.AuthorizedDeliveryHash != "" {
		t.Error("a cancelled request spent an authorization")
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Error("a cancelled request left a live grant")
	}
}

// V3 gone: the model's own bytes land, no grant, no error.
func TestStructuredRouteKeepsTheBaselineWhenV3IsUnavailable(t *testing.T) {
	w := newAutomaticWorld(t, tuiAutomaticContract, routeWinner, nil, true)
	w.ctx.V3URL = "http://127.0.0.1:9"
	expectBaselineKept(t, w, "V3 unreachable")
}

// Exact bytes, terminator included, from selection to disk.
func TestStructuredRouteDeliversExactBytesIncludingTheFinalNewline(t *testing.T) {
	for name, winner := range map[string]string{
		"one final newline": routeWinner, "two trailing newlines": routeWinner + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			w := newAutomaticWorld(t, tuiAutomaticContract, winner, nil, true)
			res, err := w.write(t)
			if err != nil || res == nil || !res.Success {
				t.Fatalf("write failed: %v %+v", err, res)
			}
			if got := w.disk(t); got != winner {
				t.Fatalf("disk %q, want the exact candidate %q", got, winner)
			}
			if res.AuthorizedDeliveryHash != contentSHA256(winner) {
				t.Error("the spent authorization does not name the bytes on disk")
			}
		})
	}
}

// The structured target grounds a delivery and nothing else: the request still
// owes no obligation, declares no outputs, and the grant says which grounding
// it used.
func TestStructuredTargetIsNotAnObligation(t *testing.T) {
	w := newAutomaticWorld(t, tuiAutomaticContract, routeWinner, nil, true)
	before := len(requestObligations(w.ctx))
	recs := captureShadow(t, func() {
		if _, err := w.write(t); err != nil {
			t.Fatal(err)
		}
	})
	if got := w.disk(t); got != routeWinner {
		t.Fatalf("not delivered: %q", got)
	}
	if outputKnowledgeDeclared(w.ctx) {
		t.Error("the delivery made the request look like it declared outputs")
	}
	if after := len(requestObligations(w.ctx)); after != before || after != 0 {
		t.Errorf("obligations %d -> %d; the structured target became an obligation", before, after)
	}
	minted := false
	for _, r := range recordsOfKind(recs, "authorization_grant_event") {
		if r["event"] == "minted" {
			minted = true
		}
	}
	if !minted {
		t.Error("no grant was minted for the delivered candidate")
	}
	for _, r := range recordsOfKind(recs, "candidate_policy_decision") {
		if r["decision"] != string(PolicyCandidateAutomaticV3) {
			t.Errorf("policy decision %v, want %s", r["decision"], PolicyCandidateAutomaticV3)
		}
	}
}

// A grant is spent once; a second attempt with the same bytes gets nothing.
func TestStructuredRouteGrantCannotBeReplayed(t *testing.T) {
	w := newAutomaticWorld(t, tuiAutomaticContract, routeWinner, nil, true)
	if _, err := w.write(t); err != nil {
		t.Fatal(err)
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Fatal("the grant survived its delivery")
	}
	// The same bytes again: the baseline is now the winner, nothing is
	// proposed, and no licence exists to spend.
	res, err := writeFileWithV3(w.path, routeWinner, w.ctx)
	if err != nil || res == nil {
		t.Fatalf("second write: %v", err)
	}
	if res.AuthorizedDeliveryHash != "" {
		t.Error("a second write spent an authorization")
	}
}

// --- Structural guards -------------------------------------------------------

func TestStructuredMutationTargetOwnership(t *testing.T) {
	read := func(f string) string {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	own := read("structured_mutation_target.go")
	// No prose or intent helper participates, and no contract field is read.
	for _, banned := range []string{"UserMessage", "HumanTask", "Messages", "actionIntentWords",
		"isActionIntentMessage", "expectedOutputPaths", "strings.Contains(", "regexp", ".TaskContract."} {
		if strings.Contains(own, banned) {
			t.Errorf("the structured target reads %q", banned)
		}
	}
	// It is consumed only by the automatic-delivery owners, and by the staged
	// verification producer, which asks it only whether declared commands
	// should run against a target a grant could later be minted for.
	entries, _ := os.ReadDir(".")
	readers := map[string]bool{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		if strings.Contains(read(n), "structuredMutationTargetGrounds(") {
			readers[n] = true
		}
	}
	for n := range readers {
		switch n {
		case "structured_mutation_target.go", "candidate_delivery.go", "authorization_grant.go",
			"evidence_wiring.go":
		default:
			t.Errorf("%s consults the structured mutation target; only the authorization owners may", n)
		}
	}
	for _, must := range []string{"candidate_delivery.go", "authorization_grant.go"} {
		if !readers[must] {
			t.Errorf("%s no longer consults the structured mutation target", must)
		}
	}
	// It never feeds obligations, completion, permissions, or the delete tool.
	for _, f := range []string{"obligations.go", "obligation_kinds.go", "permissions.go", "delivery_settlement.go"} {
		body := read(f)
		for _, banned := range []string{"structuredMutationTargetGrounds", "targetGroundingStructuredMutationTarget", "TargetGrounding"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s reads %s", f, banned)
			}
		}
	}
	// The delete tool region of tools.go consults no scope and no grounding.
	tools := read("tools.go")
	del := tools[strings.Index(tools, "func deleteFileTool()"):]
	del = del[:strings.Index(del, "\nfunc ")]
	for _, banned := range []string{"mutationScope", "structuredMutationTarget", "TargetGrounding", "deriveMutationScope"} {
		if strings.Contains(del, banned) {
			t.Errorf("delete_file consults %s", banned)
		}
	}
	// The mint still binds to the scope and still has the one-time, exact-byte
	// path as its only delivery: guarded elsewhere, re-asserted here.
	mint := read("authorization_grant.go")
	for _, must := range []string{"scopeAdmitsCandidate", "targetIsAuthorized(in.Obligations, target)", "structuredMutationTargetGrounds"} {
		if !strings.Contains(mint, must) {
			t.Errorf("the mint no longer contains %s", must)
		}
	}
	// Declared outputs are never widened: the declared branch refuses before
	// the structured one is consulted.
	i, j := strings.Index(mint, "case in.OutputKnowledgeDeclared:"), strings.Index(mint, "case basis == grantBasisAutomaticV3:")
	if i < 0 || j < 0 || i > j {
		t.Error("the declared-outputs refusal does not precede the structured grounding")
	}
	// The identity of a proposed candidate has one owner.
	if strings.Count(read("evidence_wiring.go"), "func proposedCandidateIdentity(") != 1 {
		t.Error("proposedCandidateIdentity is not defined exactly once in the identity owner")
	}
	for _, f := range []string{"tools.go", "edit_route_delivery.go"} {
		if strings.Count(read(f), "proposedCandidateIdentity(") != 1 {
			t.Errorf("%s does not mint the proposed candidate's identity exactly once", f)
		}
	}
}

// Nothing but a parsed call grounds anything: a filename in the prose and a
// request with no tool call both leave the structured target empty.
func TestProseAndAbsentCallsGroundNothing(t *testing.T) {
	w := newAutomaticWorld(t, tuiAutomaticContract, routeWinner, nil, true)
	w.ctx.HumanTask = "please rewrite solve.py and also create helper.py"
	if len(requestObligations(w.ctx)) != 0 || outputKnowledgeDeclared(w.ctx) {
		t.Fatal("a filename in the prose became an obligation")
	}
	ok, why := structuredMutationTargetGrounds(w.ctx, mutationScope{}, w.path)
	if ok || why != structuredTargetNoScope {
		t.Errorf("no tool call grounded a target: ok=%v why=%q", ok, why)
	}
	// A scope for another path grounds nothing for this one.
	other := filepath.Join(w.dir, "helper.py")
	scope, scopeOK := deriveMutationScope(w.ctx, mintRouteEntry(w.ctx), "write_file", other, "", "x = 1\n")
	if !scopeOK {
		t.Fatal("scope for helper.py did not derive")
	}
	if ok, why := structuredMutationTargetGrounds(w.ctx, scope, w.path); ok || why != structuredTargetMismatch {
		t.Errorf("a call naming helper.py grounded solve.py: ok=%v why=%q", ok, why)
	}
	// And a deletion tool derives no scope at all.
	if _, ok := deriveMutationScope(w.ctx, mintRouteEntry(w.ctx), "delete_file", w.path, routeBaseline, ""); ok {
		t.Error("delete_file derived a mutation scope")
	}
}

// The dangerous-tool permission flow is what it was: a deletion under the
// automatic contract still asks, and the permission owner reads no scope.
func TestDeletionStillRequiresPermissionUnderAutomatic(t *testing.T) {
	w := newAutomaticWorld(t, tuiAutomaticContract, routeWinner, nil, true)
	args, _ := json.Marshal(map[string]string{"path": "solve.py"})
	for _, mode := range []PermissionMode{PermissionDefault, PermissionAcceptEdits, PermissionYolo} {
		w.ctx.PermissionMode = mode
		if !needsPermission(w.ctx, "delete_file", args) {
			t.Errorf("mode %v: delete_file no longer requires permission under automatic_v3", mode)
		}
	}
	perms, err := os.ReadFile("permissions.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"mutationScope", "structuredMutationTarget", "TargetGrounding", "candidatePolicyOf"} {
		if strings.Contains(string(perms), banned) {
			t.Errorf("permissions.go consults %s", banned)
		}
	}
}
