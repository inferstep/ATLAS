package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A grant is the difference between "these bytes were authorized" and "these
// bytes may land now".
//
// Each test here holds something that looks like permission and is told it is
// not: the same grant twice, a grant for another candidate, a grant whose
// workspace moved underneath it, a grant the request already cancelled.
// Nothing is delivered in this file. Minting and spending are the whole
// subject; delivery is tested in candidate_delivery_test.go.

// grantWorld is an authorized candidate and everything the decision was
// reached over, kept so a test can change exactly one thing.
type grantWorld struct {
	*authWorld
	in       authorizationInput
	decision AuthorizationDecision
}

func newGrantWorld(t *testing.T, commands ...string) *grantWorld {
	t.Helper()
	return newGrantWorldWithCode(t, authPy, commands...)
}

// newGrantWorldWithCode builds the world around exact bytes, so a test that
// cares what lands does not have to swap the candidate out from under evidence
// that already described something else.
func newGrantWorldWithCode(t *testing.T, code string, commands ...string) *grantWorld {
	t.Helper()
	var w *authWorld
	if len(commands) == 0 {
		w = newAuthWorld(t,
			`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["solve.py"]}`,
			"solve.py", code, true)
	} else {
		w = stagedWorldWithCode(t, code, commands...)
	}
	ev, evID := w.mustObserve(t)
	evidence := append([]proxyEvidence{ev}, w.stage(evID)...)

	generation, stateHash := workspaceIdentity(w.ctx)
	_, witness := baselineWitness(w.ctx, w.path)
	in := authorizationInput{
		Obligations:   requestObligations(w.ctx),
		TargetPath:    w.path,
		CandidateHash: contentSHA256(w.code),
		Identity: V3EvidenceProvenance{
			RequestID:           requestIDOf(w.ctx),
			InvocationID:        evID.InvocationID,
			CandidateInstanceID: evID.CandidateInstanceID,
			CandidateHash:       contentSHA256(w.code),
			WorkspaceGeneration: generation,
			WorkspaceStateHash:  stateHash,
			BaselineIdentity:    baselineIdentityFor(w.ctx, w.path),
		},
		Evidence:                evidence,
		BaselineWitnessCommand:  witness,
		OutputKnowledgeDeclared: outputKnowledgeDeclared(w.ctx),
		// A grant is refused without the structured intent of the call it
		// would be bound to, so the fixture carries the scope its route
		// would have derived.
		Scope:          testMutationScope(w.ctx, mintRouteEntry(w.ctx), w.path, w.code),
		CandidateBytes: w.code,
	}
	d := decideAuthorization(w.ctx, in)
	return &grantWorld{authWorld: w, in: in, decision: d}
}

// claim builds what a consumer presents, from the live state, the same way
// the delivery owner has to.
func (w *grantWorld) claim() grantClaim {
	generation, stateHash := workspaceIdentity(w.ctx)
	return grantClaim{
		RequestID:           requestIDOf(w.ctx),
		InvocationID:        w.in.Identity.InvocationID,
		CandidateInstanceID: w.in.Identity.CandidateInstanceID,
		CandidateHash:       w.in.CandidateHash,
		TargetPath:          w.path,
		WorkspaceGeneration: generation,
		WorkspaceStateHash:  stateHash,
		BaselineIdentity:    baselineIdentityFor(w.ctx, w.path),
		BaselineHash:        fileSHA256(w.ctx, w.path),
		BaselineGeneration:  targetGeneration(w.ctx, w.path),
		BaselineTombstoned:  targetTombstoned(w.ctx, w.path),
		ObligationSetID:     obligationSetIdentity(w.in.Obligations),
		EvidenceSetID:       evidenceSetIdentity(w.in.Evidence),
	}
}

func (w *grantWorld) mint(t *testing.T) *authorizationGrant {
	t.Helper()
	g, ok, why := mintAuthorizationGrant(w.ctx, w.in, w.decision, "selected-1", grantBasisStrict)
	if !ok {
		t.Fatalf("an authorized decision minted no grant: %s", why)
	}
	return g
}

// --- minting ---------------------------------------------------------------------

func TestAnAuthorizedDecisionMintsABoundGrant(t *testing.T) {
	w := newGrantWorld(t)
	if !w.decision.Authorized {
		t.Fatalf("the fixture is not authorized: %s", w.decision.Reason)
	}
	g := w.mint(t)

	for _, c := range []struct{ name, got, want string }{
		{"request_id", g.RequestID, requestIDOf(w.ctx)},
		{"invocation_id", g.InvocationID, w.in.Identity.InvocationID},
		{"candidate_instance_id", g.CandidateInstanceID, w.in.Identity.CandidateInstanceID},
		{"candidate_hash", g.CandidateHash, w.in.CandidateHash},
		{"target", g.TargetPath, w.path},
		{"workspace_state_hash", g.WorkspaceStateHash, w.in.Identity.WorkspaceStateHash},
		{"baseline_identity", g.BaselineIdentity, w.in.Identity.BaselineIdentity},
		{"obligation_set", g.ObligationSetID, obligationSetIdentity(w.in.Obligations)},
		{"evidence_set", g.EvidenceSetID, evidenceSetIdentity(w.in.Evidence)},
		{"selected_candidate", g.SelectedCandidateID, "selected-1"},
	} {
		if c.got != c.want {
			t.Errorf("%s is %q, want %q", c.name, c.got, c.want)
		}
	}
	if g.WorkspaceGeneration != w.in.Identity.WorkspaceGeneration {
		t.Error("the grant does not bind the workspace generation")
	}
	if g.DecisionGeneration == 0 {
		t.Error("the grant has no decision generation")
	}
	if liveGrantCount(w.ctx) != 1 {
		t.Errorf("%d live grants, want the one just minted", liveGrantCount(w.ctx))
	}
}

func TestAnUnauthorizedDecisionMintsNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		commands []string
		change   func(*grantWorld)
	}{
		{"refused outright", nil, func(w *grantWorld) {
			w.decision.Authorized, w.decision.Reason = false, ReasonEvidenceMissing
		}},
		{"authorized with a reason that is not", nil, func(w *grantWorld) {
			w.decision.Reason = ReasonWorkspaceStale
		}},
		{"something still owed", nil, func(w *grantWorld) {
			w.decision.Missing = []string{"declared_command:owed"}
		}},
		{"nothing demonstrated", nil, func(w *grantWorld) { w.decision.Satisfied = nil }},
		{"no contract", nil, func(w *grantWorld) { w.ctx.TaskContract = nil }},
		{"no obligations", nil, func(w *grantWorld) { w.in.Obligations = nil }},
		{"undeclared target", nil, func(w *grantWorld) {
			w.in.TargetPath = filepath.Join(w.ctx.WorkingDir, "not-declared.py")
		}},
		{"identity and candidate disagree", nil, func(w *grantWorld) {
			w.in.Identity.CandidateHash = contentSHA256("other")
		}},
		{"another request's identity", nil, func(w *grantWorld) {
			w.in.Identity.RequestID = "req-somebody-else"
		}},
		{"no invocation", nil, func(w *grantWorld) { w.in.Identity.InvocationID = "" }},
		{"no candidate instance", nil, func(w *grantWorld) { w.in.Identity.CandidateInstanceID = "" }},
		{"no workspace state", nil, func(w *grantWorld) { w.in.Identity.WorkspaceStateHash = "" }},
		// The decision still says yes. Minting checks coverage again rather
		// than taking the decision's word.
		{"declared command without evidence", []string{"pytest -q"}, func(w *grantWorld) {
			var kept []proxyEvidence
			for _, e := range w.in.Evidence {
				if e.Provenance.Source != ProvenanceClientDeclaredVerification {
					kept = append(kept, e)
				}
			}
			w.in.Evidence = kept
		}},
		// With a V3 service record, the strict basis also needs the record to
		// be readable and its adapter to support the artifact.
		{"a service record that cannot be read", nil, func(w *grantWorld) {
			w.in.Envelope = &V3EvidenceEnvelope{Evaluation: V3EvidenceEvaluation{Supported: true}}
		}},
		{"an adapter that does not support the artifact", nil, func(w *grantWorld) {
			w.in.Envelope = &V3EvidenceEnvelope{
				WireVersion: evidenceWireMajor + ".0",
				Identity: V3EvidenceIdentity{ContractID: "contract", ContractVersion: "1", ArtifactScope: "file",
					EvaluationContextHash: "context", CalibrationID: "calibration"},
				Evaluation: V3EvidenceEvaluation{ExecutionStatus: "ok", EvidenceStrength: "syntax"},
				Selection:  V3EvidenceSelection{Status: "no_verified_winner"},
			}
		}},
		{"output knowledge not declared", nil, func(w *grantWorld) { w.in.OutputKnowledgeDeclared = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newGrantWorld(t, tc.commands...)
			if !w.decision.Authorized {
				t.Fatalf("the fixture is not authorized: %s", w.decision.Reason)
			}
			tc.change(w)
			if _, ok, _ := mintAuthorizationGrant(w.ctx, w.in, w.decision, "s", grantBasisStrict); ok {
				t.Error("a grant was minted")
			}
		})
	}
}

// On the automatic basis, with no declared outputs, only the model's own
// structured call can ground the target, and a call of another request
// grounds nothing.
func TestTheAutomaticBasisMintsNothingForATargetNoCallOfThisRequestGrounds(t *testing.T) {
	w := newGrantWorld(t)
	w.in.OutputKnowledgeDeclared = false
	w.in.Scope.RequestID = "req-somebody-else"
	if _, ok, why := mintAuthorizationGrant(w.ctx, w.in, w.decision, "s", grantBasisAutomaticV3); ok || why != structuredTargetNotThisRequest {
		t.Errorf("minted %v with %q, want no grant and %q", ok, why, structuredTargetNotThisRequest)
	}
}

func TestAliasSpellingsShareOneGrant(t *testing.T) {
	w := newGrantWorld(t)
	g := w.mint(t)
	rel, err := filepath.Rel(w.ctx.WorkingDir, w.path)
	if err != nil {
		t.Fatal(err)
	}
	c := w.claim()
	for _, alias := range []string{rel, "./" + rel, w.path, filepath.Join(w.ctx.WorkingDir, ".", rel)} {
		key := grantKey(c.RequestID, c.InvocationID, c.CandidateInstanceID,
			resolveAgentPath(w.ctx, alias))
		if key != g.ID {
			t.Errorf("%q names a different grant", alias)
		}
	}
	// Minting again under an alias replaces the grant rather than adding one.
	aliased := w.in
	aliased.TargetPath = rel
	if _, ok, why := mintAuthorizationGrant(w.ctx, aliased, w.decision, "s", grantBasisStrict); !ok {
		t.Fatalf("an alias could not mint: %s", why)
	}
	if liveGrantCount(w.ctx) != 1 {
		t.Errorf("%d live grants for one delivery", liveGrantCount(w.ctx))
	}
}

func TestALaterDecisionSupersedesTheEarlierGrant(t *testing.T) {
	w := newGrantWorld(t)
	first := w.mint(t)
	// A second candidate for the same target.
	later := w.in
	later.Identity.CandidateInstanceID = w.in.Identity.CandidateInstanceID + ":again"
	second, ok, why := mintAuthorizationGrant(w.ctx, later, w.decision, "selected-2", grantBasisStrict)
	if !ok {
		t.Fatalf("the later decision minted nothing: %s", why)
	}
	if second.DecisionGeneration <= first.DecisionGeneration {
		t.Error("the later grant does not order after the earlier one")
	}
	if liveGrantCount(w.ctx) != 1 {
		t.Errorf("%d live grants for one target", liveGrantCount(w.ctx))
	}
	if _, why := consumeAuthorizationGrant(w.ctx, w.claim()); why == "" {
		t.Error("a superseded grant was spent")
	}
}

// The ledger holds at most grantCapacity live grants. A spent grant stays as a
// tombstone, so a replay can be told apart from a grant that never existed,
// but it holds no room: otherwise a long run would stop being able to deliver.
func TestGrantCapacityCountsOnlyLiveGrants(t *testing.T) {
	for _, tc := range []struct {
		name       string
		filler     grantRetirement
		wantLive   int
		wantMinted bool
	}{
		{"full of live grants", grantLive, grantCapacity, false},
		{"full of spent grants", grantConsumed, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newGrantWorld(t)
			// Filled directly. A contract may declare at most
			// maxTaskContractEntries outputs, which equals the capacity, so
			// no client can reach the limit through declared targets.
			w.ctx.grantMu.Lock()
			if w.ctx.grants == nil {
				w.ctx.grants = map[string]*authorizationGrant{}
			}
			for i := 0; i < grantCapacity; i++ {
				id := contentSHA256(fmt.Sprintf("filler-%d", i))
				w.ctx.grants[id] = &authorizationGrant{
					ID: id, TargetPath: "/w/filler-" + id, retired: tc.filler}
			}
			w.ctx.grantMu.Unlock()
			if n := liveGrantCount(w.ctx); n != tc.wantLive {
				t.Fatalf("%d live grants before minting, want %d", n, tc.wantLive)
			}

			_, minted, why := mintAuthorizationGrant(w.ctx, w.in, w.decision, "s", grantBasisStrict)
			if minted != tc.wantMinted {
				t.Fatalf("minted=%v, want %v (%s)", minted, tc.wantMinted, why)
			}
			if !minted && !strings.Contains(why, "too many") {
				t.Errorf("refusal %q does not name capacity", why)
			}
			if !minted && liveGrantCount(w.ctx) != tc.wantLive {
				t.Error("the refused mint changed the live set")
			}
		})
	}
}

// --- spending --------------------------------------------------------------------

func TestAGrantIsSpentExactlyOnce(t *testing.T) {
	w := newGrantWorld(t)
	g := w.mint(t)
	spent, why := consumeAuthorizationGrant(w.ctx, w.claim())
	if why != "" {
		t.Fatalf("a live grant would not spend: %s", why)
	}
	if spent.ID != g.ID || spent.CandidateHash != g.CandidateHash {
		t.Error("consumption returned a different grant")
	}
	again, why := consumeAuthorizationGrant(w.ctx, w.claim())
	if why == "" || again != nil {
		t.Fatal("a grant was spent twice")
	}
	if why != string(grantConsumed) {
		t.Errorf("replay refusal %q, want %q", why, grantConsumed)
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Error("a spent grant is still live")
	}
}

func TestConcurrentConsumersGetExactlyOneSuccess(t *testing.T) {
	w := newGrantWorld(t)
	w.mint(t)
	const racers = 16
	var wg sync.WaitGroup
	results := make([]string, racers)
	start := make(chan struct{})
	// Built once, before the racers start. Building it inside each goroutine
	// takes the ledger lock and serialises them, which would leave the
	// consumer's own atomicity untested.
	claim := w.claim()
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, why := consumeAuthorizationGrant(w.ctx, claim)
			results[i] = why
		}(i)
	}
	close(start)
	wg.Wait()

	wins := 0
	for _, why := range results {
		if why == "" {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("%d of %d consumers succeeded, want exactly one", wins, racers)
	}
}

// Two kinds of wrong claim, and they differ in what they cost.
//
// Request, invocation, candidate instance and target are the address: they make
// the key. A claim that changes one of them names a grant that does not exist,
// so it is refused and burns nothing. That stops a wrong or guessed address
// from exhausting another candidate's grant.
//
// Everything else is what the grant is about. A claim that addresses this grant
// and is wrong about it burns it, because a free near-miss is a probe. The
// consumer takes the grant before it validates the claim, so the grant is left
// "attempted", and only a newly minted decision can deliver.
func TestAWrongClaimCannotSpendAGrant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(w *grantWorld, c *grantClaim)
		burns  bool
	}{
		{"another request", func(w *grantWorld, c *grantClaim) { c.RequestID = "req-other" }, false},
		{"another invocation", func(w *grantWorld, c *grantClaim) { c.InvocationID = "inv-other" }, false},
		{"another candidate", func(w *grantWorld, c *grantClaim) { c.CandidateInstanceID = "cand-other" }, false},
		{"another path", func(w *grantWorld, c *grantClaim) {
			c.TargetPath = filepath.Join(w.ctx.WorkingDir, "other.py")
		}, false},
		{"other bytes", func(w *grantWorld, c *grantClaim) { c.CandidateHash = contentSHA256("other") }, true},
		{"another workspace", func(w *grantWorld, c *grantClaim) { c.WorkspaceStateHash = contentSHA256("moved") }, true},
		{"a later generation", func(w *grantWorld, c *grantClaim) { c.WorkspaceGeneration += 1 }, true},
		{"the target moved on", func(w *grantWorld, c *grantClaim) { c.BaselineGeneration += 1 }, true},
		{"the target removed", func(w *grantWorld, c *grantClaim) { c.BaselineTombstoned = true }, true},
		{"another baseline", func(w *grantWorld, c *grantClaim) { c.BaselineIdentity = "syntax:elsewhere" }, true},
		{"other baseline bytes", func(w *grantWorld, c *grantClaim) {
			c.BaselineHash = contentSHA256("was something else")
		}, true},
		{"another obligation set", func(w *grantWorld, c *grantClaim) {
			c.ObligationSetID = contentSHA256("a different task")
		}, true},
		{"another evidence set", func(w *grantWorld, c *grantClaim) {
			c.EvidenceSetID = contentSHA256("different records")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newGrantWorld(t)
			w.mint(t)
			wrong := w.claim()
			tc.change(w, &wrong)
			if _, why := consumeAuthorizationGrant(w.ctx, wrong); why == "" {
				t.Fatal("a wrong claim spent the grant")
			}

			live := liveGrantCount(w.ctx)
			_, why := consumeAuthorizationGrant(w.ctx, w.claim())
			if tc.burns {
				if live != 0 {
					t.Error("the wrong claim left the grant it addressed spendable")
				}
				if why != string(grantAttempted) {
					t.Errorf("the honest claim afterwards got %q, want %q", why, grantAttempted)
				}
			} else {
				if live != 1 {
					t.Error("the wrong claim burned a grant it never addressed")
				}
				if why != "" {
					t.Errorf("the honest claim afterwards was refused: %s", why)
				}
			}
		})
	}
}

// A caller that sends wrong claims can burn only what it can name, and every
// other candidate's grant is untouched.
func TestMismatchesCannotExhaustUnrelatedGrants(t *testing.T) {
	w := newGrantWorld(t)
	kept := w.mint(t)
	for i := 0; i < 50; i++ {
		guess := w.claim()
		guess.RequestID = fmt.Sprintf("req-guess-%d", i)
		guess.InvocationID = fmt.Sprintf("inv-guess-%d", i)
		guess.CandidateInstanceID = fmt.Sprintf("cand-guess-%d", i)
		guess.CandidateHash = contentSHA256("guess")
		if _, why := consumeAuthorizationGrant(w.ctx, guess); why == "" {
			t.Fatal("a guessed claim was accepted")
		}
	}
	if liveGrantCount(w.ctx) != 1 {
		t.Error("guessed claims exhausted a grant they never addressed")
	}
	spent, why := consumeAuthorizationGrant(w.ctx, w.claim())
	if why != "" {
		t.Fatalf("the honest claim was refused after 50 guesses: %s", why)
	}
	if spent.ID != kept.ID {
		t.Error("a different grant was spent")
	}
}

// The claim is built from the live state after something changed, as the
// delivery owner builds it. The claim addresses this grant and is wrong about
// it, so the grant is burned.
func TestAGrantCannotBeSpentAfterItsWorldMoved(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(t *testing.T, w *grantWorld)
	}{
		{"the workspace moved", func(t *testing.T, w *grantWorld) {
			bumpWorkspace(w.ctx, w.path, contentSHA256("something else happened\n"))
		}},
		{"the target was rewritten", func(t *testing.T, w *grantWorld) {
			if err := os.WriteFile(w.path, []byte("REPLACED = 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newGrantWorld(t)
			w.mint(t)
			tc.move(t, w)
			if _, why := consumeAuthorizationGrant(w.ctx, w.claim()); why == "" {
				t.Error("the grant was spent against a world it never saw")
			}
			if liveGrantCount(w.ctx) != 0 {
				t.Error("the refused claim left the grant spendable")
			}
		})
	}
}

// After a burn the way forward is a new decision, reached over whatever the
// state is now. It is not a retry of the same grant.
func TestARetryNeedsANewlyMintedDecision(t *testing.T) {
	w := newGrantWorld(t)
	first := w.mint(t)
	wrong := w.claim()
	wrong.EvidenceSetID = contentSHA256("stale set")
	if _, why := consumeAuthorizationGrant(w.ctx, wrong); why == "" {
		t.Fatal("a wrong claim was accepted")
	}
	second, ok, why := mintAuthorizationGrant(w.ctx, w.in, w.decision, "s", grantBasisStrict)
	if !ok {
		t.Fatalf("a fresh decision could not mint after a burn: %s", why)
	}
	if second.DecisionGeneration <= first.DecisionGeneration {
		t.Error("the replacement grant reuses the burned decision generation")
	}
	if spent, why := consumeAuthorizationGrant(w.ctx, w.claim()); why != "" {
		t.Errorf("the newly minted grant would not spend: %s", why)
	} else if spent.DecisionGeneration != second.DecisionGeneration {
		t.Error("the old decision generation was spent")
	}
}

// --- retirement ------------------------------------------------------------------

func TestCancellationRetiresEveryGrant(t *testing.T) {
	w := newGrantWorld(t)
	w.mint(t)
	cancelled, cancel := context.WithCancel(
		context.WithValue(context.Background(), requestIDKey, requestIDOf(w.ctx)))
	w.ctx.Ctx = cancelled
	cancel()
	if _, why := consumeAuthorizationGrant(w.ctx, w.claim()); why != string(grantCancelled) {
		t.Errorf("refusal %q, want %q", why, grantCancelled)
	}
	if liveGrantCount(w.ctx) != 0 {
		t.Error("a cancelled request still holds a live grant")
	}
}

func TestRetirementIsFinalIdempotentAndBlocksMinting(t *testing.T) {
	for _, reason := range []grantRetirement{grantTerminal, grantSessionEnd, grantCancelled} {
		t.Run(string(reason), func(t *testing.T) {
			w := newGrantWorld(t)
			w.mint(t)
			if n := retireAuthorizationGrants(w.ctx, reason); n != 1 {
				t.Errorf("retired %d grants, want 1", n)
			}
			for i := 0; i < 3; i++ {
				if n := retireAuthorizationGrants(w.ctx, reason); n != 0 {
					t.Errorf("retired %d on repeat, want 0", n)
				}
			}
			if _, why := consumeAuthorizationGrant(w.ctx, w.claim()); why != string(reason) {
				t.Errorf("refusal %q, want %q", why, reason)
			}
			if _, ok, _ := mintAuthorizationGrant(w.ctx, w.in, w.decision, "s", grantBasisStrict); ok {
				t.Error("a grant was minted after retirement")
			}
		})
	}
}

func TestUnrelatedSuccessNeitherRetiresNorRefreshesAGrant(t *testing.T) {
	w := newGrantWorld(t)
	g := w.mint(t)
	// Another file is written successfully. Nothing about this delivery.
	other := filepath.Join(w.ctx.WorkingDir, "notes.md")
	if err := os.WriteFile(other, []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	observeDeliverable(w.ctx, other, []byte("# notes\n"),
		ValidationKindNone, ValidationNotApplicable, "")

	w.ctx.grantMu.Lock()
	still := w.ctx.grants[g.ID]
	w.ctx.grantMu.Unlock()
	if still == nil {
		t.Fatal("an unrelated success deleted the grant")
	}
	// An unrelated success is not a cancellation. A consumer must not be told
	// the request ended because something else went well.
	if still.retired != grantLive {
		t.Errorf("an unrelated success retired the grant as %q", still.retired)
	}
	// Nothing about the grant was derived again, so it still describes the
	// moment it was minted.
	if still.DecisionGeneration != g.DecisionGeneration ||
		still.WorkspaceStateHash != g.WorkspaceStateHash ||
		still.CandidateHash != g.CandidateHash {
		t.Error("an unrelated success refreshed the grant")
	}
	// The workspace it was about has moved on, so it cannot be spent.
	if _, why := consumeAuthorizationGrant(w.ctx, w.claim()); why == "" {
		t.Error("a grant was spent against a workspace it never saw")
	}
}

// An unrelated success cannot buy a second spending of a grant that was
// already spent.
func TestAGrantIsOneTimeAcrossAnUnrelatedSuccess(t *testing.T) {
	w := newGrantWorld(t)
	w.mint(t)
	if _, why := consumeAuthorizationGrant(w.ctx, w.claim()); why != "" {
		t.Fatalf("the grant would not spend: %s", why)
	}
	other := filepath.Join(w.ctx.WorkingDir, "notes.md")
	if err := os.WriteFile(other, []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	observeDeliverable(w.ctx, other, []byte("# notes\n"),
		ValidationKindNone, ValidationNotApplicable, "")
	if _, why := consumeAuthorizationGrant(w.ctx, w.claim()); why != string(grantConsumed) {
		t.Errorf("the second spending got %q, want %q", why, grantConsumed)
	}
}

// --- what a grant may not carry --------------------------------------------------

// The grant must hold the canonical target, because it compares against it.
// It may not hold content: bytes, command text, source, prose. The telemetry
// record leaves the process, so it may not hold the path either: a workspace
// path is content.
func TestAGrantAndItsTelemetryCarryNoContent(t *testing.T) {
	const secret = "TOKEN = 'hunter2'\nprint(7)\n"
	w := newGrantWorldWithCode(t, secret, "pytest --token=hunter2")
	content := []string{secret, "hunter2", "TOKEN", "print(7)", "pytest", "--token"}

	// captureShadow installs its own sink, so every record the grant writes
	// is captured here.
	captured := captureShadow(t, func() {
		g := w.mint(t)
		blob, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range content {
			if strings.Contains(string(blob), needle) {
				t.Errorf("the grant carries %q", needle)
			}
		}
		if g.TargetPath != w.path {
			t.Error("the grant does not name the target it is for")
		}
		if _, why := consumeAuthorizationGrant(w.ctx, w.claim()); why != "" {
			t.Fatalf("the grant would not spend: %s", why)
		}
	})

	if len(recordsOfKind(captured, "authorization_grant_event")) == 0 {
		t.Fatal("minting and spending a grant wrote no telemetry")
	}
	contentOrPath := append(content, "solve.py", w.ctx.WorkingDir)
	for _, rec := range captured {
		blob, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range contentOrPath {
			if strings.Contains(string(blob), needle) {
				t.Errorf("a %v record carries %q", rec["record_kind"], needle)
			}
		}
	}
}

// A decision computed again under new state is a different decision wearing
// the old one's identity. The point of a grant is that it froze one.
func TestConsumptionNeverRecomputesTheDecision(t *testing.T) {
	src, err := os.ReadFile("authorization_grant.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func consumeAuthorizationGrant(")
	if start < 0 {
		t.Fatal("the consumer is gone")
	}
	fn := body[start:]
	if next := strings.Index(fn[1:], "\nfunc "); next >= 0 {
		fn = fn[:next+1]
	}
	for _, banned := range []string{
		"decideAuthorization", "requestObligations", "deriveTaskObligations",
		"produceSyntaxEvidence", "produceDeclaredVerificationEvidence",
		"stageCandidate", "observeCandidateAuthorization",
	} {
		if strings.Contains(fn, banned) {
			t.Errorf("consumption calls %s: it must validate, never re-decide", banned)
		}
	}
}

// The grant records what the pipeline picked but never acts on it: selection
// has no bearing on whether the client's obligations were met.
func TestSelectionIsNotAuthorization(t *testing.T) {
	w := newGrantWorld(t)
	g := w.mint(t)
	if g.SelectedCandidateID == "" {
		t.Fatal("the grant does not record what was selected")
	}
	claim := w.claim()
	w.ctx.grantMu.Lock()
	w.ctx.grants[g.ID].SelectedCandidateID = "a completely different winner"
	w.ctx.grantMu.Unlock()
	if _, why := consumeAuthorizationGrant(w.ctx, claim); why != "" {
		t.Errorf("selection changed the consumption outcome: %s", why)
	}
}

func TestNothingRestoresOrRefreshesASpentGrant(t *testing.T) {
	src, err := os.ReadFile("authorization_grant.go")
	if err != nil {
		t.Fatal(err)
	}
	// Every assignment to `retired` moves it away from live. A line that set
	// it back would undo a spending.
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, ".retired = ") && strings.HasSuffix(trimmed, "= grantLive") {
			t.Errorf("a grant is put back into circulation: %q", trimmed)
		}
	}
}

func TestGrantAttemptTelemetryIsAClosedVocabulary(t *testing.T) {
	w := newGrantWorld(t)
	recs := captureShadow(t, func() {
		w.mint(t)
		// consumed_authorized
		if _, why := consumeAuthorizationGrant(w.ctx, w.claim()); why != "" {
			t.Fatalf("honest consumption failed: %s", why)
		}
		// already_spent
		consumeAuthorizationGrant(w.ctx, w.claim())
		// not_found
		missing := w.claim()
		missing.InvocationID = "inv-nowhere"
		consumeAuthorizationGrant(w.ctx, missing)
	})
	// consumed_refused, on its own grant.
	other := newGrantWorld(t)
	refusedRecs := captureShadow(t, func() {
		other.mint(t)
		wrong := other.claim()
		wrong.CandidateHash = contentSHA256("other")
		consumeAuthorizationGrant(other.ctx, wrong)
	})
	recs = append(recs, refusedRecs...)

	seen := map[string]bool{}
	closed := map[string]bool{
		string(grantConsumedAuthorized): true, string(grantConsumedRefused): true,
		string(grantAlreadySpent): true, string(grantNotFound): true,
		"minted": true, "retired": true,
	}
	for _, rec := range recordsOfKind(recs, "authorization_grant_event") {
		event, _ := rec["event"].(string)
		seen[event] = true
		if !closed[event] {
			t.Errorf("event %q is outside the closed vocabulary", event)
		}
		blob, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{"solve.py", w.ctx.WorkingDir, authPy, "print(", "/tmp/"} {
			if strings.Contains(string(blob), needle) {
				t.Errorf("a %s record carries %q", event, needle)
			}
		}
	}
	for _, want := range []string{
		string(grantConsumedAuthorized), string(grantConsumedRefused),
		string(grantAlreadySpent),
	} {
		if !seen[want] {
			t.Errorf("no %s record was written", want)
		}
	}
	// not_found writes nothing: there is no grant to attribute it to, and
	// inventing an identity for one would be worse than silence.
	if seen[string(grantNotFound)] {
		t.Error("a not_found attempt named a grant that does not exist")
	}
}
