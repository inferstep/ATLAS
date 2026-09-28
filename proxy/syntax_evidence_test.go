package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The first trusted evidence producer, and every way it must decline.
//
// What it attests is what ATLAS's own structural gate already evaluated. What
// it may close is one kind of obligation at one strength. Nothing here
// delivers a candidate: authority is computed and never consulted.

// syntaxSandbox is a sandbox whose syntax check answers `valid` and records
// what it was asked. It answers nothing else, so a producer that reached for
// another endpoint fails rather than silently degrading.
func syntaxSandbox(t *testing.T, valid bool, seen *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/syntax-check") {
			http.NotFound(w, r)
			return
		}
		var in struct{ Code, Language string }
		json.NewDecoder(r.Body).Decode(&in)
		if seen != nil {
			*seen = append(*seen, in.Code)
		}
		out := map[string]interface{}{"valid": valid}
		if !valid {
			out["errors"] = []string{"SyntaxError: invalid syntax"}
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func syntaxEvidenceCtx(t *testing.T, srv *httptest.Server) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.SandboxURL = srv.URL
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-fixture")
	return ctx
}

func syntaxObligation(t *testing.T, subject string) taskObligation {
	t.Helper()
	o, ok := newTaskObligation(ObligationSyntacticValidity, subject, "", true)
	if !ok {
		t.Fatalf("syntax obligation for %q refused", subject)
	}
	return o
}

// syntaxRequest builds the request the way production does: the caller runs
// the live gate and hands the producer the verdict it reached. The producer
// runs nothing of its own, so a test that skipped this would be testing a
// different function.
func syntaxRequest(t *testing.T, ctx *AgentContext, subject, code string) syntaxEvidenceRequest {
	t.Helper()
	return syntaxEvidenceRequest{
		Obligation:          syntaxObligation(t, subject),
		Path:                subject,
		CandidateBytes:      code,
		CandidateHash:       contentSHA256(code),
		Outcome:             fallbackSyntaxOutcomeFor(ctx, subject, code).aggregate(),
		InvocationID:        "inv-1",
		CandidateInstanceID: "cand-1",
	}
}

// --- the positive case -------------------------------------------------------

func TestAnExactCandidateSyntaxPassProducesOneBoundRecord(t *testing.T) {
	var seen []string
	ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, &seen))
	const code = "A = 1\n"
	req := syntaxRequest(t, ctx, "solve.py", code)

	ev, ok := produceSyntaxEvidence(ctx, req)
	if !ok {
		t.Fatal("a passing structural check produced no evidence")
	}
	if ev.Outcome != ValidationPassed {
		t.Errorf("outcome %q, want passed", ev.Outcome)
	}
	p := ev.Provenance
	if p.Source != ProvenanceProxyOwnedValidation {
		t.Errorf("source %q, want proxy_owned_validation", p.Source)
	}
	if p.CandidateHash != contentSHA256(code) {
		t.Error("evidence does not name the exact bytes it was about")
	}
	if p.ObligationID != req.Obligation.ID {
		t.Error("evidence does not name its obligation")
	}
	if p.RequestID != "req-fixture" || p.InvocationID != "inv-1" ||
		p.CandidateInstanceID != "cand-1" {
		t.Errorf("identity incomplete: %+v", p)
	}
	if p.ObservedStrength != "syntax" || p.RequiredStrength != "syntax" {
		t.Errorf("strengths %q/%q, want syntax/syntax",
			p.RequiredStrength, p.ObservedStrength)
	}
	if p.WorkspaceStateHash == "" {
		t.Error("no workspace state identity")
	}
	if authorized, why := ev.Authorizes(); !authorized {
		t.Errorf("a proxy syntax pass could not close a syntax obligation: %s", why)
	}
	// The bytes reached the existing validator, and only it.
	if len(seen) != 1 || seen[0] != code {
		t.Errorf("the gate saw %d payloads, want the exact candidate once", len(seen))
	}
}

// TestTheProducerReusesTheLiveGate pins that there is no second checker: the
// verdict comes from the same function every write goes through.
func TestTheProducerReusesTheLiveGate(t *testing.T) {
	ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, false, nil))
	const code = "def(\n"
	live := fallbackSyntaxOutcomeFor(ctx, "solve.py", code).aggregate()
	ev, ok := produceSyntaxEvidence(ctx, syntaxRequest(t, ctx, "solve.py", code))
	if !ok {
		t.Fatal("an attempted check produced no evidence")
	}
	if ev.Outcome != live.Status {
		t.Errorf("producer said %q, the live gate said %q", ev.Outcome, live.Status)
	}
}

// --- a negative observation is bound, and authorizes nothing -----------------

func TestASyntaxFailureIsBoundAndCarriesNoAuthority(t *testing.T) {
	ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, false, nil))
	ev, ok := produceSyntaxEvidence(ctx, syntaxRequest(t, ctx, "solve.py", "def(\n"))
	if !ok {
		t.Fatal("a demonstrated failure produced no observation at all")
	}
	if ev.Outcome != ValidationFailed {
		t.Errorf("outcome %q, want failed", ev.Outcome)
	}
	if ev.Provenance.CandidateHash == "" || ev.Provenance.ObligationID == "" {
		t.Error("a negative observation must still say what it is about")
	}
	if authorized, _ := ev.Authorizes(); authorized {
		t.Error("a structural failure authorized its obligation")
	}
}

// --- every way the producer must decline -------------------------------------

func TestTheProducerDeclinesWhenTheGateDidNotEvaluateTheseBytes(t *testing.T) {
	t.Run("not applicable", func(t *testing.T) {
		ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
		req := syntaxRequest(t, ctx, "notes.txt", "hello\n")
		if _, ok := produceSyntaxEvidence(ctx, req); ok {
			t.Error("an artifact class the gate does not govern produced evidence")
		}
	})
	t.Run("not run", func(t *testing.T) {
		ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
		ctx.SandboxURL = ""
		if _, ok := produceSyntaxEvidence(ctx, syntaxRequest(t, ctx, "solve.py", "A = 1\n")); ok {
			t.Error("an unreachable validator produced evidence")
		}
	})
	t.Run("validator refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no", http.StatusForbidden)
		}))
		defer srv.Close()
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		ctx.SandboxURL = srv.URL
		ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-fixture")
		if _, ok := produceSyntaxEvidence(ctx, syntaxRequest(t, ctx, "solve.py", "A = 1\n")); ok {
			t.Error("a refused check produced evidence")
		}
	})
	t.Run("undecodable answer", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("not json"))
		}))
		defer srv.Close()
		ctx := NewAgentContext(t.TempDir(), Tier2Medium)
		ctx.SandboxURL = srv.URL
		ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-fixture")
		if _, ok := produceSyntaxEvidence(ctx, syntaxRequest(t, ctx, "solve.py", "A = 1\n")); ok {
			t.Error("an unparseable verdict produced evidence")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
		cancelled, cancel := context.WithCancel(
			context.WithValue(context.Background(), requestIDKey, "req-fixture"))
		cancel()
		ctx.Ctx = cancelled
		if _, ok := produceSyntaxEvidence(ctx, syntaxRequest(t, ctx, "solve.py", "A = 1\n")); ok {
			t.Error("a cancelled run produced evidence")
		}
	})
	t.Run("no request identity", func(t *testing.T) {
		ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
		ctx.Ctx = context.Background()
		if _, ok := produceSyntaxEvidence(ctx, syntaxRequest(t, ctx, "solve.py", "A = 1\n")); ok {
			t.Error("evidence was produced with no request to bind to")
		}
	})
	t.Run("no invocation or candidate identity", func(t *testing.T) {
		ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
		for _, mut := range []func(*syntaxEvidenceRequest){
			func(r *syntaxEvidenceRequest) { r.InvocationID = "" },
			func(r *syntaxEvidenceRequest) { r.CandidateInstanceID = "" },
		} {
			req := syntaxRequest(t, ctx, "solve.py", "A = 1\n")
			mut(&req)
			if _, ok := produceSyntaxEvidence(ctx, req); ok {
				t.Error("evidence was produced with an incomplete identity")
			}
		}
	})
}

func TestAMovedHashFailsClosed(t *testing.T) {
	ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
	req := syntaxRequest(t, ctx, "solve.py", "A = 1\n")
	// The caller has moved on to different bytes and is asking about those.
	req.CandidateHash = contentSHA256("A = 2\n")
	if _, ok := produceSyntaxEvidence(ctx, req); ok {
		t.Error("evidence was produced about bytes the validator never saw")
	}
	req.CandidateHash = ""
	if _, ok := produceSyntaxEvidence(ctx, req); ok {
		t.Error("evidence was produced with no hash to bind to")
	}
}

// --- one kind, one strength ---------------------------------------------------

func TestASyntaxPassMayDescribeOnlyASyntaxObligation(t *testing.T) {
	ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
	for _, kind := range []string{
		ObligationDeclaredCommand, ObligationDeclaredExample,
		ObligationArtifactExists, ObligationUnsupported,
	} {
		o, ok := newTaskObligation(kind, "solve.py", "", true)
		if !ok {
			continue
		}
		req := syntaxRequest(t, ctx, "solve.py", "A = 1\n")
		req.Obligation = o
		if _, ok := produceSyntaxEvidence(ctx, req); ok {
			t.Errorf("a syntax pass described a %s obligation", kind)
		}
	}
}

func TestAPassingSyntaxRecordCannotSatisfyABehavioralObligation(t *testing.T) {
	ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
	ev, ok := produceSyntaxEvidence(ctx, syntaxRequest(t, ctx, "solve.py", "A = 1\n"))
	if !ok {
		t.Fatal("no evidence to test")
	}
	// Forge the demand upward. The ceiling is on the SOURCE, so nothing a
	// record says about itself can raise it.
	forged := ev
	forged.Provenance.RequiredStrength = "behavioral"
	if authorized, _ := forged.Authorizes(); authorized {
		t.Error("proxy syntax evidence authorized a behavioral obligation")
	}
	forged.Provenance.ObservedStrength = "oracle"
	if authorized, _ := forged.Authorizes(); authorized {
		t.Error("a forged observed strength raised the source ceiling")
	}
}

func TestASyntaxPassCannotPreserveAStrongerBaseline(t *testing.T) {
	ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
	base, ok := newTaskObligation(ObligationBaselinePreserved, "solve.py", "behavioral", true)
	if !ok {
		t.Fatal("baseline obligation refused")
	}
	req := syntaxRequest(t, ctx, "solve.py", "A = 1\n")
	req.Obligation = base
	if _, ok := produceSyntaxEvidence(ctx, req); ok {
		t.Error("a compile claimed a behavioural baseline survived")
	}
}

// --- binding failures ---------------------------------------------------------

func TestEveryIdentityMismatchFailsClosed(t *testing.T) {
	ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
	req := syntaxRequest(t, ctx, "solve.py", "A = 1\n")
	req.BaselineIdentity = "syntax:baseline-a"
	ev, ok := produceSyntaxEvidence(ctx, req)
	if !ok {
		t.Fatal("no evidence to test")
	}
	held := ev.Provenance
	if bound, why := held.BindsTo(held); !bound {
		t.Fatalf("evidence does not bind to itself: %s", why)
	}
	for _, c := range []struct {
		name string
		mut  func(*V3EvidenceProvenance)
	}{
		{"request", func(p *V3EvidenceProvenance) { p.RequestID = "req-other" }},
		{"invocation", func(p *V3EvidenceProvenance) { p.InvocationID = "inv-2" }},
		{"candidate instance", func(p *V3EvidenceProvenance) { p.CandidateInstanceID = "cand-2" }},
		{"candidate hash", func(p *V3EvidenceProvenance) { p.CandidateHash = contentSHA256("other") }},
		{"workspace generation", func(p *V3EvidenceProvenance) { p.WorkspaceGeneration++ }},
		{"workspace state", func(p *V3EvidenceProvenance) { p.WorkspaceStateHash = "moved" }},
		{"baseline", func(p *V3EvidenceProvenance) { p.BaselineIdentity = "syntax:baseline-b" }},
		{"obligation", func(p *V3EvidenceProvenance) { p.ObligationID = "syntactic_validity:other" }},
	} {
		asked := held
		c.mut(&asked)
		if bound, _ := held.BindsTo(asked); bound {
			t.Errorf("%s mismatch still bound", c.name)
		}
	}
}

func TestAWorkspaceMutationMovesTheGeneration(t *testing.T) {
	ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
	before, beforeHash := workspaceIdentity(ctx)
	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*DeliverableState{}
	}
	ctx.Ledger["solve.py"] = &DeliverableState{
		Path: "solve.py", CurrentHash: "h1", Generation: 1}
	after, afterHash := workspaceIdentity(ctx)
	if after <= before {
		t.Errorf("generation %d did not advance past %d", after, before)
	}
	if afterHash == beforeHash {
		t.Error("workspace state hash did not move on a mutation")
	}
	ctx.Ledger["solve.py"].CurrentHash = "h2"
	ctx.Ledger["solve.py"].Generation = 2
	third, thirdHash := workspaceIdentity(ctx)
	if third <= after || thirdHash == afterHash {
		t.Error("a second mutation did not move the workspace identity")
	}
}

// TestABaselineIdentityNamesNoBytes pins that the identity is a hash pair, so
// naming a baseline never carries the baseline's contents.
func TestABaselineIdentityNamesNoBytes(t *testing.T) {
	ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
	if got := baselineIdentityFor(ctx, resolveAgentPath(ctx, "absent.py")); got != "" {
		t.Errorf("a file that is not there has baseline identity %q", got)
	}
}

// --- nothing this produces is on the way to a log or a delivery ---------------

func TestTheProducerHoldsNoCandidateBytes(t *testing.T) {
	ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
	const secret = "SECRET_TOKEN = 'hunter2'\n"
	ev, ok := produceSyntaxEvidence(ctx, syntaxRequest(t, ctx, "solve.py", secret))
	if !ok {
		t.Fatal("no evidence to test")
	}
	blob, err := json.Marshal(ev.Provenance)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"hunter2", "SECRET_TOKEN", secret} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("the serialised binding carries %q", needle)
		}
	}
}

func TestLegacyEvidenceRemainsNonAuthorizing(t *testing.T) {
	ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, true, nil))
	ev, ok := produceSyntaxEvidence(ctx, syntaxRequest(t, ctx, "solve.py", "A = 1\n"))
	if !ok {
		t.Fatal("no evidence to test")
	}
	for _, source := range []string{ProvenanceLegacy, ProvenanceModelGenerated, ProvenanceUnknown} {
		forged := ev
		forged.Provenance.Source = source
		if authorized, _ := forged.Authorizes(); authorized {
			t.Errorf("%s evidence authorized an obligation", source)
		}
	}
}

// TestProxyValidationVerdictsAreUnchangedByTheProducer pins that producing
// evidence is an observation, not an intervention: the gate's own answer for
// the same bytes is identical whether or not a producer ran.
func TestProxyValidationVerdictsAreUnchangedByTheProducer(t *testing.T) {
	for _, valid := range []bool{true, false} {
		ctx := syntaxEvidenceCtx(t, syntaxSandbox(t, valid, nil))
		const code = "A = 1\n"
		before := fallbackSyntaxOutcomeFor(ctx, "solve.py", code)
		produceSyntaxEvidence(ctx, syntaxRequest(t, ctx, "solve.py", code))
		after := fallbackSyntaxOutcomeFor(ctx, "solve.py", code)
		if before != after {
			t.Errorf("valid=%v: the gate's verdict moved from %+v to %+v",
				valid, before, after)
		}
	}
}
