// The V3 route of a write or edit: the request, the candidate it returns, and how the route ends.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"
)

// v3CandidatesTested unwraps a possibly-nil V3 response so the
// stage_end envelope can carry a count even on error paths.
func v3CandidatesTested(r *V3GenerateResponse) int {
	if r == nil {
		return 0
	}
	return r.CandidatesTested
}

// latestUserMessage is the HUMAN's request, for handing the V3 pipeline the
// requirement it is generating against. ctx.HumanTask is authoritative: the
// loop stores the request verbatim before appending anything, because the
// conversation itself cannot answer this question — correctives, manifests
// and re-injected file content all ride user-role messages for chat-template
// compatibility, and "last user turn" was observed resolving to "[system
// note]: run the program standalone" mid-session, sending V3 off to generate
// against harness feedback instead of the task (third-party audit finding).
// The scan below is a fallback for contexts built without the loop (tests,
// direct bridge calls) and skips synthetic notes.
func latestUserMessage(ctx *AgentContext) string {
	if ctx == nil {
		return ""
	}
	if ctx.HumanTask != "" {
		return ctx.HumanTask
	}
	for i := len(ctx.Messages) - 1; i >= 0; i-- {
		if ctx.Messages[i].Role != "user" {
			continue
		}
		if strings.HasPrefix(ctx.Messages[i].Content, "[system note]:") {
			continue
		}
		return ctx.Messages[i].Content
	}
	return ""
}

// writeFileWithV3 routes through the V3 pipeline for T2/T3 tasks.
// The model's content is the incumbent: V3 checks and ranks it with the
// alternatives it generates, and sends it back unchanged when none ranks
// higher (#259), which proposedV3Candidate reads as no proposal.
func writeFileWithV3(path, baselineContent string, ctx *AgentContext) (*ToolResult, error) {
	req := v3WriteRequest(path, baselineContent, ctx)

	// The production call path for invocation feasibility: before a single
	// candidate is generated, record whether this task could close at all. The
	// answer is recorded only; generation proceeds whatever it concludes.
	//
	// One entry of the candidate-generation route, named before anything is
	// decided about it. Every record this attempt produces carries it, so a
	// retry's work can never be mistaken for this one's.
	entry := mintRouteEntry(ctx)
	// One owner for how this entry ends, finalised exactly once. The deferred
	// default means a branch that forgets to speak still records the
	// fail-closed member instead of leaving an entry that never ended.
	lifecycle := newRouteLifecycle(entry)
	// The attribution is deferred first so it runs last, over the recorded
	// ending.
	defer lifecycle.recordAttribution(ctx)
	defer lifecycle.finalizeDefault(ctx)
	// Recorded, never enforced: generation proceeds whatever it says.
	observeInvocationFeasibility(ctx, entry)

	v3Result, err := generateV3Write(ctx, entry, path, req)
	if err != nil {
		return writeAfterV3Failure(ctx, lifecycle, path, baselineContent, err)
	}
	// V3 answered for this write: an earlier unchecked write of the file is
	// no longer the one to name.
	ctx.clearV3Unchecked(path)

	write := proposeWriteCandidate(ctx, entry, lifecycle, path, baselineContent, v3Result)
	evidence := collectWriteCandidateEvidence(ctx, entry, write)
	decideWriteCandidateDelivery(ctx, entry, write, evidence)
	if refusal := applyWriteGates(write); refusal != nil {
		return refusal, nil
	}

	// A gate fallback wrote the model's own baseline, which was NOT
	// V3-sandbox-verified (only syntax- and structural-checked). Report it
	// as a plain direct write with no V3 metadata — the same honest
	// reporting as the V3-unavailable fallback — so the vetoed winner's
	// score/phase/evidence don't attach to unverified content and the
	// "V3 verified this edit" completion nudge (agent.go) doesn't fire.
	// The observation that travels to the write must describe the bytes being
	// written. Every transition above re-observes what it changed, so this
	// normally returns what it was handed; it re-evaluates only if a future
	// branch alters the bytes without saying what it found about them, which is
	// the one way this invariant could rot.
	logMandatoryChecks(ctx, "write_file", path, "candidate route")
	final := write.deliveredCheck
	if write.checkedFor != write.code {
		log.Printf("[write_file] final bytes for %s are not the observed ones — re-checking",
			logPath(path))
		final = fallbackSyntaxOutcomeFor(ctx, path, write.code).aggregate()
	}

	if write.fellBack || !write.authorizedV3 {
		return writeRetainedContent(write, final)
	}

	// Stream V3 completion summary — after the gate, so a rejected write
	// doesn't present as a successfully completed V3 stage.
	if ctx.StreamFn != nil {
		ctx.StreamFn("v3_progress", map[string]string{
			"message": fmt.Sprintf("  └──── V3 complete: %s, %d candidates", v3Result.PhaseSolved, v3Result.CandidatesTested),
		})
	}

	// THE consumer of the authorization grant, and the only mutation on this
	// route that a typed request can reach. Everything it needs is re-read
	// from disk inside it: the grant froze a moment and this is a later one.
	if write.delivery.Typed {
		lifecycle.finish(ctx, routingCandidateAuthorized, contentSHA256(write.code), "")
		result, outcome, err := deliverAuthorizedCandidate(ctx, path, write.code,
			write.delivery.Grant, final, write.delivery.MetCommands, write.delivery.BaselinePreserved)
		lifecycle.noteDelivery(outcome)
		return settleTypedDelivery(write, final, result, outcome, err)
	}

	// Unreachable by an authorized candidate: authorization now requires a
	// grant, and a grant requires declared output knowledge. Kept as the
	// fail-closed floor rather than deleted, because a future policy that
	// authorizes without minting would otherwise write bytes through a path
	// nobody reviewed.
	log.Printf("[write_file] authorized delivery reached the ungranted path for %s — writing the caller's content",
		logPath(path))
	return write.writeCallersContent()
}

// v3WriteRequest is the V3 request for a write: an edit's request, and the
// files this session wrote as context too.
func v3WriteRequest(path, baselineContent string, ctx *AgentContext) V3GenerateRequest {
	req := v3Request(path, baselineContent, ctx)

	// Files this session wrote are project context too; otherwise
	// siblings written but never read are invisible to candidate
	// generation. Disk content wins over any stale read snapshot: the
	// on-disk version is what verification produced.
	for rel := range ctx.SessionWrites {
		if rel == "" {
			continue
		}
		abs := resolveAgentPath(ctx, rel)
		if abs == path {
			continue // the file being generated is the baseline, not context
		}
		data, rerr := readRegularFile(abs)
		if rerr != nil {
			continue
		}
		if req.ProjectContext == nil {
			req.ProjectContext = make(map[string]string)
		}
		req.ProjectContext[rel] = contextEntry(string(data), 4000)
	}
	return req
}

// generateV3Write calls V3 for a write and reports its stages to the client
// and the pipeline pane.
func generateV3Write(ctx *AgentContext, entry routeEntry, path string,
	req V3GenerateRequest) (*V3GenerateResponse, error) {
	// Tell the user V3 is taking over so they don't think the file
	// vanished. write_file with V3 holds the disk write until V3 picks
	// a winner — without this message the chat goes silent for the 1–3
	// minute V3 cycle and looks broken.
	if ctx.StreamFn != nil {
		ctx.StreamFn("v3_progress", map[string]string{
			"message": fmt.Sprintf("V3 pipeline starting for %s — generating diverse candidates and build-verifying each.", filepath.Base(path)),
		})
	}
	Emit(NewEnvelope(EvtStageStart, "v3", map[string]interface{}{
		"detail": fmt.Sprintf("file=%s", filepath.Base(path)),
		// Additive: an existing consumer sees the field it always saw. This
		// one names which entry of the route generated, so an observer can
		// pair a generation with its own feasibility decision instead of
		// counting events.
		"route_entry": entry.ID,
	}))
	v3Start := time.Now()

	// Call V3 service with streaming progress. Each stage callback also
	// fires a typed envelope so the pipeline pane shows V3 progress.
	// Three categories of progress events:
	//   token       — per-LLM-token delta from V3's streaming generator
	//   llm_start   — V3 is starting an LLM call (candidate gen, scoring…)
	//   llm_end     — V3's LLM call finished (with token/timing summary)
	//   <other>     — pipeline stage marker (probe, plansearch, sandbox…)
	var stages v3StageMarks
	v3Result, err := callV3GenerateStreaming(ctx.Ctx, ctx.V3URL, req, func(stage, detail string, data map[string]interface{}) {
		forwardV3Stage(ctx, stage, detail, data)
		switch stage {
		case "token", "reasoning_token", "llm_start", "llm_end":
			// No envelope: it would bloat /events with thousands of metric
			// events for a single call.
		default:
			stages.note(stage, detail, data)
		}
	})
	stages.closeCurrent(err == nil)
	emitV3WriteStageEnd(v3Start, v3Result, err)
	return v3Result, err
}

// v3StageMarks turns V3's stage markers into start and end envelopes for the
// pipeline pane.
type v3StageMarks struct {
	current string
}

// note closes the previous stage when a new name comes, and reports progress
// inside the same stage.
func (m *v3StageMarks) note(stage, detail string, data map[string]interface{}) {
	if stage == m.current {
		Emit(NewEnvelope(EvtMetric, "v3:"+stage,
			map[string]interface{}{"name": "progress", "value": detail}))
		return
	}
	m.closeCurrent(true)
	payload := map[string]interface{}{"detail": detail}
	for k, v := range data {
		payload[k] = v
	}
	Emit(NewEnvelope(EvtStageStart, "v3:"+stage, payload))
	m.current = stage
}

// closeCurrent emits the end of the stage that is open, if one is.
func (m *v3StageMarks) closeCurrent(success bool) {
	if m.current == "" {
		return
	}
	Emit(Envelope{
		EventID:   NewEventID(),
		Timestamp: float64(time.Now().UnixNano()) / 1e9,
		Type:      EvtStageEnd,
		Stage:     "v3:" + m.current,
		Payload:   map[string]interface{}{"success": success},
	})
}

// emitV3WriteStageEnd closes the V3 stage of a write.
func emitV3WriteStageEnd(v3Start time.Time, v3Result *V3GenerateResponse, err error) {
	// The structured evidence rides the telemetry envelope, whole. It is
	// deliberately NOT put on the ToolResult: that is projected to the model
	// and to the guarded tool-result SSE, where an unconsumed research field
	// would become an accidental interface. Telemetry is where a durable
	// record belongs while nothing decides from it yet.
	v3StagePayload := map[string]interface{}{
		"success":           err == nil,
		"candidates_tested": v3CandidatesTested(v3Result),
	}
	if v3Result != nil {
		v3StagePayload["evidence"] = evidenceTelemetry(
			v3Result.Evidence, v3Result.EvidenceUnavailableReason)
		// The authorization decision and its reason, recorded where a durable
		// record belongs. It is deliberately not put on the ToolResult, which
		// is projected to the model and to the guarded tool-result SSE.
		authorized, why := v3DeliveryAuthorized(v3Result, v3Result.Code)
		auth := map[string]interface{}{"authorized": authorized}
		if why != "" {
			auth["reason"] = why
		}
		v3StagePayload["authorization"] = auth
	}
	Emit(Envelope{
		EventID:    NewEventID(),
		Timestamp:  float64(time.Now().UnixNano()) / 1e9,
		Type:       EvtStageEnd,
		Stage:      "v3",
		DurationMS: time.Since(v3Start).Milliseconds(),
		Payload:    v3StagePayload,
	})
}

// writeAfterV3Failure ends a write whose V3 call failed.
func writeAfterV3Failure(ctx *AgentContext, lifecycle *routeLifecycle, path, baselineContent string,
	err error) (*ToolResult, error) {
	// User cancellation is not a fallback case — the turn was aborted,
	// so nothing should land on disk.
	if errors.Is(err, context.Canceled) || (ctx.Ctx != nil && ctx.Ctx.Err() != nil) {
		log.Printf("[write_file] V3 aborted by cancellation — not writing %s", path)
		lifecycle.finish(ctx, routingCancelled, "", "")
		return &ToolResult{
			Success: false,
			Error:   "write_file cancelled — no content was written",
		}, nil
	}
	// The lens stopped scoring inside V3. The lens is required, so this
	// is not a fallback case either: nothing is written, and the agent
	// loop ends the run with the reason (lens_required.go).
	if why, down := lensUnavailable(err); down {
		log.Printf("[write_file] the lens cannot score (%s) — not writing %s", why, logPath(path))
		ctx.noteLensDown(why)
		lifecycle.finish(ctx, routingProducerUnavailable, "", "")
		return noMutation("write_file not applied: the geometric lens stopped answering (" + why + ")"), nil
	}
	// Fallback to direct write if V3 service unavailable — but never
	// land content the sandbox confirms is broken: a truncated tool
	// call writing a SyntaxError to disk with success=true is how the
	// mini-bench got its two broken files (t06/t09).
	log.Printf("[write_file] V3 failed: %s — falling back to direct write", err)
	// The final summary names this file while these bytes are on disk.
	ctx.noteV3Unchecked(path, v3FailureReason(err), baselineContent)
	if errors.Is(err, context.DeadlineExceeded) {
		lifecycle.finish(ctx, routingProducerTimedOut, "", "")
	} else {
		lifecycle.finish(ctx, routingProducerUnavailable, "", "")
	}
	msg := "  └─ V3 unavailable, writing directly"
	if errors.Is(err, context.DeadlineExceeded) {
		// Not the configured ceiling: the session's remaining time can
		// shorten this call's cap, and naming the wrong number tells the
		// operator to raise a setting that was not the limit they hit.
		// The exact cap and its source are in the error and the log.
		msg = "  └─ V3 ran out of its time budget, writing your version"
	}
	return writeWithoutCandidate(ctx, path, baselineContent, msg)
}

// v3Write is one write once V3 has answered. `code` is what would land, and
// the fields below it describe those bytes: a step that changes `code` changes
// them with it, through revokeV3 and restoreBaseline.
type v3Write struct {
	ctx             *AgentContext
	lifecycle       *routeLifecycle
	path            string
	baselineContent string
	v3Result        *V3GenerateResponse
	scope           mutationScope
	scopeOK         bool

	code           string
	authorizedV3   bool
	fellBack       bool
	deliveredCheck checkOutcome
	checkedFor     string // the bytes deliveredCheck describes

	candidateProposed           bool
	proposalIdentity            string
	languageOrBoundaryViolation bool
	delivery                    deliveryAuthorization
	policy                      candidatePolicyOutcome
}

// proposeWriteCandidate takes V3's answer down to the bytes this write would
// land, and observes them.
func proposeWriteCandidate(ctx *AgentContext, entry routeEntry, lifecycle *routeLifecycle,
	path, baselineContent string, v3Result *V3GenerateResponse) *v3Write {
	// Write the winning candidate, or the baseline if V3 did not improve. The
	// presence of Code is not authorization: an unverified alternative must
	// never displace the baseline, and delivery is decided below by the proxy's
	// policy over evidence it produced about these exact bytes.
	//
	// The structured intent of this call: write_file, this canonical target,
	// the caller's own bytes as the proposal. Derived before anything is
	// decided, from the call rather than from the prose, and consulted only to
	// narrow what a candidate may touch.
	scope, scopeOK := deriveMutationScope(ctx, entry, "write_file", path,
		originalForScope(path, ctx), baselineContent)

	code, proposed := proposedV3Candidate(v3Result, baselineContent)
	// Declared here, with the bytes it describes. It used to be declared
	// below the language-swap gate, which is precisely why that gate could
	// restore the baseline without withdrawing provenance.
	fellBack := !proposed

	// Language-swap gate. V3 generates candidates for the TASK, and on a
	// multi-file job the task is not the file: "build me a snake game"
	// produced JavaScript for index.html, replacing a correct 18-line
	// document with 149 lines of JS and no tags. The in-pipeline smoke
	// check could not see it because for .html it runs an HTML parser, and
	// an HTML parser accepts any text at all.
	languageOrBoundaryViolation := false
	if swapped := v3SwappedTheLanguage(path, baselineContent, code); swapped != "" {
		languageOrBoundaryViolation = true
		code, proposed, fellBack = revokeV3(baselineContent, swapped, path)
	}

	// Sanitise V3 output. The pipeline's underlying LLM response
	// occasionally arrives with markdown fences and prose preamble
	// intact; if we don't strip them, every V3-rewritten file ships
	// with a "Looking at the task..." header on disk.
	if cleaned, sanitized := sanitizeFileContent(path, code); sanitized {
		log.Printf("[write_file] sanitised V3 output for %s", path)
		code = cleaned
	}

	// Sanitisation can turn a proposal back into the caller's own content. That
	// is not a candidate, and carrying it forward as one would mint a licence
	// for a delivery that is not going to happen.
	if proposed && code == baselineContent {
		code, proposed, fellBack = revokeV3(baselineContent,
			"sanitisation left the caller's own content", path)
	}

	// FINAL-BYTE OBSERVATION. The bytes are now chosen, and every artifact this
	// machine delivers must carry a structured observation of exactly the bytes
	// that land. It sits here, after sanitisation, because sanitisation rewrites
	// the candidate AFTER V3 earned its verdict, and because V3 verifies by
	// RUNNING code -- a different question from whether these exact bytes parse,
	// answered about text that no longer exists in that form.
	//
	// deliveredCheck is only ever assigned beside the bytes it describes, and
	// checkedFor records which bytes those were, so the pairing is checkable
	// rather than a discipline each branch below has to remember.
	deliveredCheck := fallbackSyntaxOutcomeFor(ctx, path, code).aggregate()
	write := &v3Write{
		ctx: ctx, lifecycle: lifecycle, path: path, baselineContent: baselineContent,
		v3Result: v3Result, scope: scope, scopeOK: scopeOK,
		code: code, fellBack: fellBack, deliveredCheck: deliveredCheck, checkedFor: code,
		candidateProposed:           proposed,
		languageOrBoundaryViolation: languageOrBoundaryViolation,
	}
	// The proposal's own identity, taken here rather than at the ending.
	// Every gate below that withdraws a candidate restores `code` to the
	// caller's bytes, so a hash read at the end names the BASELINE -- which is
	// how a retained baseline came to be recorded under `candidate_hash`, with
	// the model's own content attributed to the pipeline. The pilot has one:
	// route entry 1 of pilot_go_duration_label carries the sha256 of the
	// main.go the model wrote itself.
	if proposed {
		write.proposalIdentity = contentSHA256(code)
	}
	return write
}

// writeCandidateEvidence is what this machine observed about a candidate's
// exact bytes.
type writeCandidateEvidence struct {
	observed             []proxyEvidence
	evID                 candidateEvidenceIdentity
	unmet                map[string]AuthorizationReason
	stagingMutatedAssets bool
	selected             string
	scopeAdmits          bool
	scopeRefusal         string
}

// collectWriteCandidateEvidence gathers the evidence the authorization owner
// decides over.
//
// The production call path for the proxy-owned syntax producer. It sits
// here because this is the one moment the bytes are fixed and the gate has
// just reported on exactly those bytes: the verdict is handed over, never
// recomputed, so there is no second sandbox call and no second opinion. Its
// evidence goes to private telemetry and into the authorization owner
// below.
//
// The live authorization owner. For a request that declared structured
// obligations the typed answer is binding: no candidate lands without a
// one-time grant, and a refusal keeps the caller's own content rather than
// falling through to a candidate the envelope happened to like. A request
// that declared nothing has no typed answer to give.
//
// A retained baseline is not a candidate.
//
// When nothing materially different came back, `code` is the caller's own
// content. Staging it, taking an authorization decision over it and minting
// a one-time licence for it describes a delivery that is not going to
// happen: the fallback below writes those same bytes directly and the
// licence retires unused.
//
// What a candidate does not have to be is service-certified. The service
// ranks; the evidence this machine produces about these exact bytes is what
// authorizes, and it cannot be produced without staging the candidate.
func collectWriteCandidateEvidence(ctx *AgentContext, entry routeEntry, write *v3Write) writeCandidateEvidence {
	path, code := write.path, write.code
	var evidence writeCandidateEvidence
	if ev, id, seen := observeDeliveredCandidateSyntax(ctx, entry, path, code, write.deliveredCheck); seen && write.candidateProposed {
		evidence.observed, evidence.evID = []proxyEvidence{ev}, id
	}
	if write.candidateProposed && evidence.evID.InvocationID == "" {
		// No producer spoke -- a request that declared no outputs owes no
		// syntax obligation, and a class the syntax gate does not check owes
		// none either -- but the candidate is still this route entry's and
		// these exact bytes, and the authorization owner binds to that.
		evidence.evID = proposedCandidateIdentity(ctx, entry, code)
	}
	if write.candidateProposed {
		// THE production call path for the client-declared verification
		// producer. It stages these exact bytes in a workspace that is not the
		// caller's and runs the commands the client declared there, so the
		// behavioral question has an answer rather than a blocker. It runs
		// commands only for a request that declared them, and the staging
		// budget bounds what it may spend on one. It no longer waits for a
		// syntax obligation: a declared main.rs has none, so its declared
		// command was owed and never run.
		behavioral, why, mutatedAssets := observeCandidateVerification(ctx, path, code, evidence.evID, write.scope)
		evidence.observed, evidence.unmet = append(evidence.observed, behavioral...), why
		evidence.stagingMutatedAssets = mutatedAssets
	}
	if write.v3Result != nil && write.v3Result.Evidence != nil {
		evidence.selected = write.v3Result.Evidence.Identity.CandidateContentHash
	}
	// Unconditionally, and deliberately so. The producers decline for a target
	// the client never declared and for a class the structural gate does not
	// govern, and gating the AUTHORIZATION on a producer having spoken would
	// mean exactly those two cases fell through to the legacy decision -- a
	// candidate landing on a structured request because nothing could speak
	// for it. The owner handles an empty evidence set: it refuses.
	evidence.scopeAdmits, evidence.scopeRefusal = false, scopeRefusedNoScope
	if write.scopeOK {
		evidence.scopeAdmits, evidence.scopeRefusal = scopeAdmitsCandidate(ctx, write.scope, code)
	}
	if write.candidateProposed {
		recordMutationScope(ctx, write.scope, evidence.scopeAdmits, evidence.scopeRefusal)
	}
	return evidence
}

// decideWriteCandidateDelivery asks the authorization owner and the policy
// owner about the candidate, and records whether it may land.
func decideWriteCandidateDelivery(ctx *AgentContext, entry routeEntry, write *v3Write,
	evidence writeCandidateEvidence) {
	path, code, v3Result := write.path, write.code, write.v3Result
	// ONE veto list, two readers. The authorization owner needs the
	// disqualifying facts before it can mint an automatic grant, and the
	// policy owner needs them to reach its decision. The authorization owner
	// computes it, after its decision, and both read that list.
	vetoInput := advisoryInput{
		Observed:                    write.deliveredCheck,
		TargetDeclared:              outputKnowledgeDeclared(ctx),
		TargetAuthorized:            targetIsAuthorized(requestObligations(ctx), resolveAgentPath(ctx, path)),
		LanguageOrBoundaryViolation: write.languageOrBoundaryViolation,
		Unmet:                       evidence.unmet,
		Evidence:                    evidence.observed,
		Cancelled:                   ctx.Ctx != nil && ctx.Ctx.Err() != nil,
		MutatedProtectedAssets:      evidence.stagingMutatedAssets,
		ScopeAdmits:                 evidence.scopeAdmits,
		ScopeRefusal:                evidence.scopeRefusal,
	}
	vetoInput.Envelope = envelopeOf(v3Result)
	// Nil without a candidate: the policy owner then computes the list itself,
	// from the same input.
	var vetoes []string
	var delivery deliveryAuthorization
	if write.candidateProposed {
		delivery = authorizeCandidateDelivery(ctx, entry, path, code, evidence.evID,
			v3Result.Evidence, evidence.observed, evidence.selected, evidence.unmet,
			write.deliveredCheck, write.scope, automaticIntent{VetoInput: vetoInput})
		vetoes = delivery.Vetoes
		vetoInput.Unmet = delivery.Unmet
		write.lifecycle.noteAuthorization(delivery, evidence.evID, contentSHA256(code), vetoes)
	}
	// THE policy owner. It reads the typed answer, the trusted observations and
	// the disqualifying facts, and says which of the honest decisions this
	// candidate earned. Only the strict authorization delivers in this build.
	policyInput := vetoInput
	policyInput.Decision = delivery.Decision
	policyInput.CaptureOnlySuppressed = delivery.CaptureOnly
	policyInput.AutomaticEligible = delivery.holds(grantBasisAutomaticV3)
	// The vetoes the authorization owner computed, verbatim. Recomputing them
	// here is exactly the duplication that lets two answers drift apart.
	policyInput.Vetoes = vetoes
	// The would-have, not the outcome: an acquisition control takes the licence
	// and leaves the answer, so the policy is asked the question it would have
	// been asked without one.
	// Strict means strict. An automatic licence also holds a grant, and
	// reporting it as a strict authorization would name the wrong rule in the
	// record a calibration is computed from.
	policy := decideCandidatePolicy(ctx, policyInput,
		write.candidateProposed && delivery.holds(grantBasisStrict))
	if write.candidateProposed {
		recordCandidatePolicyDecision(ctx, entry, contentSHA256(code), policy)
		if delivery.CaptureOnly {
			recordCaptureOnlyDisposition(ctx, entry, contentSHA256(code), policy, true)
		}
	}
	write.delivery, write.policy = delivery, policy

	// Whether the proposal becomes the delivered artifact. One owner, for every
	// request: the proxy's policy, over evidence the proxy produced about these
	// exact bytes.
	//
	// A request that declared nothing has no trusted verification to satisfy,
	// so under strict it retains the baseline -- the model's own bytes land,
	// through the model's own path, exactly as if the pipeline had not run.
	// The service's closure verdict does not decide it. A machine that let the
	// producer of a candidate certify that candidate has no authorization
	// story at all, and "it was like that before" is not one either.
	write.authorizedV3 = write.candidateProposed && policy.mayDeliverUnderPolicy()
}

// checkBaseline observes the caller's own content before it is restored. A
// non-nil result refuses the write: the content does not parse.
func (w *v3Write) checkBaseline() (checkOutcome, *ToolResult) {
	baseCheck := fallbackSyntaxOutcomeFor(w.ctx, w.path, w.baselineContent).aggregate()
	if baseCheck.Status == ValidationFailed {
		return baseCheck, syntaxRefusal(w.path, w.baselineContent, baseCheck.Detail)
	}
	return baseCheck, nil
}

// restoreBaseline withdraws the candidate. The verdict on the caller's content
// travels with it to the write.
func (w *v3Write) restoreBaseline(baseCheck checkOutcome, reason string) {
	w.code, w.authorizedV3, w.fellBack = revokeV3(w.baselineContent, reason, w.path)
	w.deliveredCheck, w.checkedFor = baseCheck, w.code
}

// syntaxRefusal refuses content that does not parse.
func syntaxRefusal(path, content, detail string) *ToolResult {
	return &ToolResult{Success: false,
		Error:            fallbackSyntaxRejection(path, content, detail),
		MutationStatus:   MutationRefused,
		ValidationKind:   ValidationKindSyntax,
		ValidationStatus: ValidationFailed,
		ValidationDetail: detail}
}

// applyWriteGates runs the gates between the decision and the disk, in order.
// A non-nil result ends the write with it.
func applyWriteGates(write *v3Write) *ToolResult {
	for _, gate := range []func(*v3Write) *ToolResult{
		withdrawRefusedCandidate, refuseContentThatDoesNotParse, refuseUnresolvedCalls,
		refuseBrokenEmbeddedScript, refuseDuplicateEntrypoint, refuseCancelledWrite,
	} {
		if refusal := gate(write); refusal != nil {
			return refusal
		}
	}
	return nil
}

// withdrawRefusedCandidate applies the policy's refusal. A refusal withdraws the
// candidate. The caller's own content is the alternative, and it is checked
// before being restored -- the same rule every other gate on this path follows.
func withdrawRefusedCandidate(write *v3Write) *ToolResult {
	if !write.candidateProposed || write.authorizedV3 {
		return nil
	}
	write.lifecycle.finish(write.ctx, routingAuthorizationRefused, contentSHA256(write.code),
		AuthorizationReason(write.delivery.Refusal))
	baseCheck, refusal := write.checkBaseline()
	if refusal != nil {
		return refusal
	}
	write.restoreBaseline(baseCheck, "the policy refused it ("+string(write.policy.Decision)+")")
	return nil
}

// refuseContentThatDoesNotParse is the syntax gate on what would land.
func refuseContentThatDoesNotParse(write *v3Write) *ToolResult {
	path, deliveredCheck := write.path, write.deliveredCheck
	if deliveredCheck.Status != ValidationFailed {
		return nil
	}
	if !write.authorizedV3 {
		// The caller's own content, and it does not parse. Nothing here
		// authored it and there is no alternative to fall back to.
		log.Printf("[write_file] fallback content for %s does not parse: %s",
			logPath(path), safeDiagnosticSummary(deliveredCheck.Detail))
		return syntaxRefusal(path, write.code, deliveredCheck.Detail)
	}
	// A candidate that does not parse is not a candidate. The model's own
	// content is the alternative, and it is checked before being restored:
	// falling back to bytes nobody looked at is how the harness would
	// replace one broken artifact with another.
	baseCheck, refusal := write.checkBaseline()
	if refusal != nil {
		log.Printf("[write_file] V3 winner and baseline both fail the syntax gate for %s",
			logPath(path))
		return refusal
	}
	log.Printf("[write_file] V3 winner for %s does not parse — writing your version instead (%s)",
		logPath(path), safeDiagnosticSummary(deliveredCheck.Detail))
	write.restoreBaseline(baseCheck, "the winner does not parse")
	return nil
}

// refuseUnresolvedCalls is the #147 gate: authoritative structural gate on
// whatever is about to land — the in-pipeline veto only prunes phase-1
// sandbox-passing candidates, so probe/repair returns, the energy fallback, and
// the baseline resurrection above can all deliver content that calls a name the
// file never binds. Same rule as the edit paths: original is the on-disk content
// (empty for a first write), only NEWLY unresolved calls block, and an unreadable
// existing original skips the gate. When the WINNER fails but the model's own
// baseline passes, write the baseline instead of rejecting: the offending call
// is V3-authored, and a rejection would blame the model for content it never
// wrote and cost a full pipeline retry per resend.
func refuseUnresolvedCalls(write *v3Write) *ToolResult {
	original, origOK := readOriginalForGate(write.path)
	if !origOK {
		return nil
	}
	introduced := editIntroducesUnresolved(write.ctx, write.path, original, write.code)
	if len(introduced) == 0 {
		return nil
	}
	if write.code != write.baselineContent {
		// The baseline is about to become the delivered artifact, so it
		// is observed before it is restored -- and the verdict is kept,
		// not spent on the allow/refuse decision and discarded.
		baseCheck, refusal := write.checkBaseline()
		if refusal != nil {
			return refusal
		}
		introduced = restoreBaselineWithoutUnresolvedCalls(write, original, introduced, baseCheck)
	}
	if write.fellBack {
		return nil
	}
	log.Printf("[write_file] V3 result introduces unresolved call(s) %v in %s — rejecting", logPaths(introduced), logPath(write.path))
	// Syntax already passed on these exact bytes at the final-byte
	// check; the structural failure is what refused them, so it is
	// the decisive outcome. The earlier pass is not carried here --
	// it would describe a check that did not make this decision.
	return structuralRefusal(structuralWriteRejection(write.path, introduced))
}

// restoreBaselineWithoutUnresolvedCalls restores the baseline when it passes the
// gate its winner failed. It returns the names to report if the write is
// refused after all.
func restoreBaselineWithoutUnresolvedCalls(write *v3Write, original string, introduced []string,
	baseCheck checkOutcome) []string {
	intrBase := editIntroducesUnresolved(write.ctx, write.path, original, write.baselineContent)
	if len(intrBase) > 0 {
		return intrBase // name what the MODEL can act on
	}
	log.Printf("[write_file] V3 winner introduces unresolved call(s) %v in %s — writing gate-passing baseline instead", logPaths(introduced), logPath(write.path))
	if write.ctx.StreamFn != nil {
		write.ctx.StreamFn("v3_progress", map[string]string{
			"message": "  └─ V3 winner failed the structural gate — writing your version",
		})
	}
	write.restoreBaseline(baseCheck, "winner failed the structural gate")
	return introduced
}

// refuseBrokenEmbeddedScript is the embedded-script gate: V3 verifies a candidate
// by RUNNING it — the server starts and the page returns 200 — which is exactly
// the evidence a broken <script> block inside the rendered HTML survives. Nothing
// else on this path parses that JavaScript. Same healthy->broken rule; and when
// the WINNER is the one carrying the break, write the model's gate-passing
// baseline rather than blaming it for V3-authored content.
func refuseBrokenEmbeddedScript(write *v3Write) *ToolResult {
	ctx, path, baselineContent := write.ctx, write.path, write.baselineContent
	original, origOK := readOriginalForGate(path)
	if !origOK {
		return nil
	}
	msg := embeddedScriptGate(ctx, path, original, write.code)
	if msg == "" {
		return nil
	}
	if write.code == baselineContent || embeddedScriptGate(ctx, path, original, baselineContent) != "" {
		log.Printf("[write_file] embedded-script gate rejected content for %s", logPath(path))
		// Structural, not syntax. A standalone embedded parse failure
		// is already owned by the final-byte check, which asks without
		// a `previous` and would have refused before this gate ran.
		// What survives to here is what only a before/after comparison
		// can see -- a render loop the change stopped driving -- which
		// is a structural regression in code that parses.
		return structuralRefusal(msg)
	}
	// Same rule as the structural revocation: the baseline is
	// observed before it is restored, and the verdict travels with
	// it to the write.
	baseCheck, refusal := write.checkBaseline()
	if refusal != nil {
		return refusal
	}
	log.Printf("[write_file] V3 winner breaks an embedded script in %s — writing gate-passing baseline instead", logPath(path))
	write.restoreBaseline(baseCheck, "winner breaks an embedded script")
	return nil
}

// refuseDuplicateEntrypoint is the same entrypoint check every edit path runs.
// write_file on an existing file is a whole-file replacement, which is exactly
// how a second `if __name__` block gets appended.
func refuseDuplicateEntrypoint(write *v3Write) *ToolResult {
	original, origOK := readOriginalForGate(write.path)
	if !origOK {
		return nil
	}
	msg := duplicateMainGuard(write.path, original, write.code)
	if msg == "" {
		return nil
	}
	log.Printf("[write_file] write duplicates the module entrypoint in %s — rejecting", logPath(write.path))
	// The file parses; what is wrong is its module-level structure, so
	// this is a structural refusal like the two above.
	return structuralRefusal(msg)
}

// refuseCancelledWrite stops a write the user cancelled. The structural gate made
// HTTP calls; a user cancel during that window must not land content on disk,
// mirroring the main call's abort path.
func refuseCancelledWrite(write *v3Write) *ToolResult {
	ctx := write.ctx
	if ctx.Ctx == nil || ctx.Ctx.Err() == nil {
		return nil
	}
	log.Printf("[write_file] cancelled during structural gate — not writing %s", write.path)
	// MutationNone, not Refused: no gate declined anything and no write was
	// attempted -- the turn ended. The observation earned on the current
	// bytes is still true and is carried, but nothing new is evaluated
	// here: cancellation is exactly the point at which no further work may
	// be done. It is overlaid only while it still describes those bytes.
	cancelled := &ToolResult{
		Success:        false,
		Error:          "write_file cancelled — no content was written",
		MutationStatus: MutationNone,
	}
	if write.checkedFor == write.code {
		overlayValidation(cancelled, write.deliveredCheck)
	}
	return cancelled
}

// writeRetainedContent lands the caller's own bytes: no candidate came back, or
// a gate withdrew it.
func writeRetainedContent(write *v3Write, final checkOutcome) (*ToolResult, error) {
	ctx, path := write.ctx, write.path
	// The caller's own bytes are what lands. Any grant minted over them was
	// never owed a delivery, and says so through its own lifecycle.
	markBaselineRetainedGrant(ctx, write.delivery)
	// WHICH of the two things happened is a different fact, and the
	// vocabulary already has both words for it. Nothing materially
	// different came back, or a gate above withdrew what did -- and only
	// the second had a candidate to name. Recording both as a retained
	// baseline is what made this ending unreadable: an analysis cannot
	// tell a producer that agreed with the caller from one whose winner
	// was revoked, and the hash it joined on belonged to neither.
	ending := routingBaselineRetained
	if write.candidateProposed {
		ending = routingRevokedByGate
	}
	write.lifecycle.finish(ctx, ending, write.proposalIdentity,
		AuthorizationReason(write.delivery.Refusal))
	// Authorization governs METADATA as well as bytes: reporting
	// V3Used/score/phase/evidence over content V3 did not author is the
	// same false claim in a different field, and it fires the agent's
	// "V3 verified this edit" completion nudge.
	fbRes, fbErr := writeFileRecorded(path, write.code, ctx)
	out, outErr := applyRouteObservation(fbRes, fbErr, final)
	emitDeliveryProvenance(ctx, path, DeliveryFromModelProposal, write.policy)
	return withDeliveryProvenance(out, DeliveryFromModelProposal), outErr
}

// writeCallersContent lands the caller's own bytes, checked before they land,
// for a delivery that cannot go ahead after the policy said it could.
func (w *v3Write) writeCallersContent() (*ToolResult, error) {
	baseCheck, refusal := w.checkBaseline()
	if refusal != nil {
		return refusal, nil
	}
	fbRes, fbErr := writeFileRecorded(w.path, w.baselineContent, w.ctx)
	out, outErr := applyRouteObservation(fbRes, fbErr, baseCheck)
	emitDeliveryProvenance(w.ctx, w.path, DeliveryFromModelProposal, w.policy)
	return withDeliveryProvenance(out, DeliveryFromModelProposal), outErr
}

// settleTypedDelivery turns what the grant's consumer did into the tool result.
func settleTypedDelivery(write *v3Write, final checkOutcome, result *ToolResult,
	outcome deliveryOutcome, err error) (*ToolResult, error) {
	if err != nil {
		if result == nil && errors.Is(err, errDeliveryUnauthorized) {
			// Refused before any byte moved. The caller's own content is
			// still the alternative, exactly as on the edit route and on
			// every other refusal here. Refusing the whole write told the
			// model its content was kept when nothing had been written.
			log.Printf("[write_file] the candidate was not spendable (%s) — writing the caller's content",
				outcome.Reason)
			return write.writeCallersContent()
		}
		if result == nil {
			// The write itself failed. The licence was spent on bytes that
			// never reached disk: nothing moved and nothing is claimed.
			return &ToolResult{Success: false,
				Error:            "the file could not be written (" + outcome.Reason + ") — nothing was changed",
				MutationStatus:   MutationRefused,
				ValidationKind:   final.kind(),
				ValidationStatus: final.Status,
				ValidationDetail: final.Detail}, nil
		}
		return result, err
	}
	if !outcome.Delivered {
		// The write happened and the result is not what was authorized.
		// No V3 metadata is attached: reporting a verified delivery over
		// bytes that did not settle is the same false claim in a
		// different field.
		return result, nil
	}
	provenance := deliveryProvenanceFor(write.policy)
	emitDeliveryProvenance(write.ctx, write.path, provenance, write.policy)
	return withDeliveryProvenance(v3DeliveredResult(write.ctx, result, write.v3Result, write.code),
		provenance), nil
}

// originalForScope is the artifact as it stands before this call, or "" when
// there is nothing there. Read through the gate's own reader so a file the
// process cannot read is treated as absent rather than as empty.
func originalForScope(path string, ctx *AgentContext) string {
	if original, ok := readOriginalForGate(path); ok {
		return original
	}
	return ""
}

// withDeliveryProvenance labels a result with where its bytes came from.
func withDeliveryProvenance(result *ToolResult, provenance string) *ToolResult {
	if result == nil || !deliveryProvenanceValues[provenance] {
		return result
	}
	result.DeliveryProvenance = provenance
	return result
}

// v3DeliveredResult attaches the pipeline's metadata to a delivery that
// actually happened. One owner for both routes, so the typed and untyped
// deliveries are provably identical in what they report.
func v3DeliveredResult(ctx *AgentContext, result *ToolResult,
	v3Result *V3GenerateResponse, code string) *ToolResult {
	if result == nil || v3Result == nil {
		return result
	}
	out := WriteFileOutput{
		BytesWritten:         len(code),
		V3Used:               true,
		CandidatesTested:     v3Result.CandidatesTested,
		WinningScore:         v3Result.WinningScore,
		PhaseSolved:          v3Result.PhaseSolved,
		VerificationEvidence: v3Result.VerificationEvidence,
	}
	outBytes, _ := json.Marshal(out)
	result.Data = outBytes
	result.V3Used = true
	result.CandidatesTested = v3Result.CandidatesTested
	result.WinningScore = v3Result.WinningScore
	result.PhaseSolved = v3Result.PhaseSolved
	result.VerificationEvidence = v3Result.VerificationEvidence
	return result
}

// V3EditMetadata captures what V3 did to an edit_file request, so the
// edit_file result can carry the same v3_used / candidates_tested fields
// write_file does.
type V3EditMetadata struct {
	Used                 bool
	CandidatesTested     int
	WinningScore         float64
	PhaseSolved          string
	VerificationEvidence []V3VerificationEvidence
	// Envelope is the service's own evidence record for the proposal, carried
	// so the route that decides can read its ranking signals. Advisory only:
	// nothing in it authorizes a delivery on any route.
	Envelope *V3EvidenceEnvelope
}

// improveContentWithV3 sends content through the V3 pipeline and returns
// V3's chosen code (baseline candidate or a better-scoring alternative).
// On error, returns "" + zero metadata; the caller should fall back to
// writing the original content.

// proposedV3Candidate is the PROPOSAL boundary: what the service offered, and
// whether it is materially different from what the caller wrote.
//
// It answers one question and deliberately not the other. Whether a candidate
// may LAND is a decision this machine makes, from evidence it produced itself,
// against the policy the client or the operator declared -- and it cannot make
// that decision about bytes it never received. Collapsing an uncertified
// proposal to the baseline here is what put the trusted producer on the far
// side of a gate that needed its output: staging runs the client's declared
// command against the candidate, so a candidate discarded before staging can
// never be the thing that command is run against.
//
// The service's own verdict travels with the bytes as advisory metadata. It
// ranks; it does not authorize. Nothing downstream reads it as permission, and
// no field of it mints a grant.
//
// A proposal is still refused outright for the things that make bytes unusable
// rather than unproven: no response, no code, or code identical to the
// caller's own, which is not a proposal at all.
func proposedV3Candidate(result *V3GenerateResponse, baseline string) (string, bool) {
	if result == nil || strings.TrimSpace(result.Code) == "" {
		// No bytes, or nothing but whitespace. Not a proposal: unusable is a
		// different thing from unproven, and only the second is worth staging.
		return baseline, false
	}
	if result.Code == baseline {
		// The service agreed with the caller. There is nothing to decide, no
		// licence to mint, and no delivery to describe.
		return baseline, false
	}
	return result.Code, true
}

// revokeV3 restores the caller's baseline AND withdraws V3 provenance in one
// step.
//
// These moved independently before: the language-swap gate reset the bytes
// to baseline but left the proposal live and fellBack false, so the final
// check attached V3Used, phase, score and verification evidence to content
// V3 had not authored -- and fired the "V3 verified this edit" nudge over it.
// Provenance describes the FINAL bytes, not the initial response, so it is
// revocable and every restoring gate must revoke it. Returning all three
// values together makes it impossible for a future gate to update one
// without the others.
func revokeV3(baseline, reason, path string) (string, bool, bool) {
	log.Printf("[write_file] V3 provenance withdrawn for %s — %s", logPath(path), reason)
	return baseline, false, true // content, live proposal, fellBack
}

// envelopeOf is the service's evidence record, or nil. Read for advisory
// signals only; nothing downstream may treat it as permission.
func envelopeOf(result *V3GenerateResponse) *V3EvidenceEnvelope {
	if result == nil {
		return nil
	}
	return result.Evidence
}

// There is no service-certification path, and its absence is the point.
// Delivering on the service's own closure verdict would let the producer of a
// candidate certify that candidate: no target the client named, no obligation,
// no floor, and an authority that comes from the side being checked. A
// contractless request keeps the model's own bytes. The structured mutation
// scope its tool call defines is the most any advisory decision may bound a
// licence to: WHERE, never whether.

func improveContentWithV3(path, content string, ctx *AgentContext) (string, V3EditMetadata, error) {
	req := v3Request(path, content, ctx)

	// Same callback logic as the write_file V3 path: tokens forward to
	// the dedicated v3_token SSE event so the TUI updates one streaming
	// row instead of spawning a chat row per token; LLM-call boundaries
	// match the chat protocol's start/end shapes; structured stages
	// emit typed events (v3_phase, v3_sandbox, etc.); only truly
	// unknown stages fall back to the v3_progress text line. Without
	// this branching, edit_file with V3 floods the chat pane with
	// thousands of "[token] X" rows during a single candidate generation.
	v3Result, err := callV3GenerateStreaming(ctx.Ctx, ctx.V3URL, req, func(stage, detail string, data map[string]interface{}) {
		forwardV3Stage(ctx, stage, detail, data)
	})
	if err != nil {
		return "", V3EditMetadata{}, err
	}

	if ctx.StreamFn != nil {
		ctx.StreamFn("v3_progress", map[string]string{
			"message": fmt.Sprintf("  └──── V3 complete: %s, %d candidates", v3Result.PhaseSolved, v3Result.CandidatesTested),
		})
	}

	chosen, proposed := proposedV3Candidate(v3Result, content)
	if !proposed {
		// Nothing materially different came back. The caller's content stands,
		// and no V3 metadata is attached to it.
		return content, V3EditMetadata{}, nil
	}
	// V3 sometimes returns code wrapped in markdown fences (the underlying
	// llama-server response had a preamble it didn't strip). Strip it here,
	// at the boundary, so the regression check below judges the code rather
	// than the wrapper — a fenced candidate is not a broken one — and so
	// neither caller ships a "Looking at the task..." header to disk.
	if cleaned, sanitized := sanitizeFileContent(path, chosen); sanitized {
		log.Printf("[v3] sanitised candidate for %s", logPath(path))
		chosen = cleaned
		// Stripping a wrapper can leave the caller's own content, which is
		// not a proposal. Everything else about the sanitised bytes is
		// decided by the route that receives them: it stages them, produces
		// evidence about them, and applies the policy.
		if chosen == content {
			log.Printf("[v3] sanitised candidate for %s is the caller's own content", logPath(path))
			return content, V3EditMetadata{}, nil
		}
	}
	// A candidate only counts as an improvement if it does not break what it
	// was handed. V3 regenerates the whole file, so it can reintroduce a
	// defect the model had already repaired; the write gates downstream would
	// then reject the edit and quote a line number from a candidate the model
	// never saw, sending it to hunt a bug that is not in the file. Drop the
	// candidate here so the caller keeps the model's own content and the gates
	// below only ever judge content the model actually wrote.
	if chosen != content {
		if reason := v3CandidateRegression(ctx, path, content, chosen); reason != "" {
			log.Printf("[v3] discarding candidate for %s — %s; keeping the caller's content", logPath(path), reason)
			return content, V3EditMetadata{}, nil
		}
	}
	return chosen, V3EditMetadata{
		Used:                 true,
		CandidatesTested:     v3Result.CandidatesTested,
		WinningScore:         v3Result.WinningScore,
		PhaseSolved:          v3Result.PhaseSolved,
		VerificationEvidence: v3Result.VerificationEvidence,
		Envelope:             envelopeOf(v3Result),
	}, nil
}

// v3Request is the V3 request for a write or edit: the caller's content as the
// baseline, and the other files this session read as its context.
func v3Request(path, content string, ctx *AgentContext) V3GenerateRequest {
	req := V3GenerateRequest{
		FilePath:     path,
		BaselineCode: content,
		Tier:         int(ctx.Tier),
		WorkingDir:   ctx.WorkingDir,
		UserMessage:  latestUserMessage(ctx),
	}
	// Add project context from files read during this session. The target
	// file's own PRE-EDIT snapshot is excluded: its current symbols come
	// from the baseline, and stale content would let the in-pipeline veto
	// credit a def this write deletes (#147 review finding #2 — same rule
	// as checkStructuralUnresolved).
	cleanTarget := filepath.Clean(path)
	if filesRead := ctx.SnapshotFilesRead(); len(filesRead) > 0 {
		req.ProjectContext = make(map[string]string)
		for p, c := range filesRead {
			if filepath.Clean(p) == cleanTarget {
				continue
			}
			rel, _ := filepath.Rel(ctx.WorkingDir, p)
			if rel == "" {
				rel = p
			}
			req.ProjectContext[rel] = contextEntry(c, 4000)
		}
	}
	if ctx.Project != nil {
		req.Framework = ctx.Project.Framework
		req.BuildCommand = ctx.Project.BuildCommand
	}
	return req
}

// forwardV3Stage hands one V3 progress event to the client, as the event the
// client renders it with.
func forwardV3Stage(ctx *AgentContext, stage, detail string, data map[string]interface{}) {
	if ctx.StreamFn == nil {
		return
	}
	// Token deltas: a separate SSE event, so the client can render them as a
	// streaming dim row, mirroring how the agent's own LLM tokens are shown.
	if stage == "token" {
		ctx.StreamFn("v3_token", map[string]string{"text": detail})
		return
	}
	// Reasoning deltas from V3's LLM calls (<think> stream). Forwarded as
	// v3_reasoning_token, NOT plain reasoning_token, because the agent-loop's
	// reasoning_token handler in the TUI targets the agent's LLM row, and V3
	// calls run in a different lifecycle. Gives the /demo viewer something to
	// watch during long PlanSearch / repair phases.
	if stage == "reasoning_token" {
		ctx.StreamFn("v3_reasoning_token", map[string]string{"text": detail})
		return
	}
	// LLM-call boundary markers. Match the chat protocol's
	// llm_call_start/end shapes so the TUI can reuse handlers.
	if stage == "llm_start" {
		payload := map[string]interface{}{"detail": detail}
		for k, v := range data {
			payload[k] = v
		}
		ctx.StreamFn("v3_llm_start", payload)
		return
	}
	if stage == "llm_end" {
		payload := map[string]interface{}{"detail": detail}
		for k, v := range data {
			payload[k] = v
		}
		ctx.StreamFn("v3_llm_end", payload)
		return
	}
	// Dedicated structured events for the pipeline pane. The TUI renders each
	// as its own row instead of a generic v3_progress string. data is the
	// structured payload from V3's _emit; it passes through verbatim with
	// `stage` and `detail` for fallback rendering.
	eventName := v3StageToEvent(stage)
	if eventName == "v3_progress" {
		// Unknown / unmapped stage — emit the legacy text line only. Keeps
		// third-party clients that haven't migrated to typed events working.
		ctx.StreamFn("v3_progress", map[string]string{
			"message": fmt.Sprintf("  │ [%s] %s", stage, detail),
		})
		return
	}
	payload := map[string]interface{}{
		"stage":  stage,
		"detail": detail,
	}
	for k, v := range data {
		payload[k] = v
	}
	ctx.StreamFn(eventName, payload)
}

// runEditPipeline is the V3 entry every content edit goes through: classify
// the file, and when the tier warrants it hand the composed result to the
// candidate pipeline, keeping the caller's version if the winner drifts
// outside the edit.
//
// It exists because it was inlined in edit_file and simply absent from
// insert_after and replace_lines, so the two line-addressed tools produced a
// single greedy sample with no candidate generation and no lens scoring — and
// those are the tools the guidance now steers toward. Adding an edit tool
// must not mean re-deciding whether the pipeline applies to it.
//
// Returns the content to write, the V3 metadata to report, and a non-nil
// ToolResult only when the turn was cancelled mid-pipeline (nothing may land
// on disk in that case).
func runEditPipeline(ctx *AgentContext, tool, path, relPath, original,
	edited string) editRouteOutcome {
	keep := editRouteOutcome{Content: edited}
	// Classify on max(old, new): a destructive edit that shrinks a T2+ file
	// into a T1 stub is exactly the edit that most needs checking, and
	// classifying on the result alone let it bypass the pipeline.
	fileTier := classifyFileTier(relPath, original)
	if newTier := classifyFileTier(relPath, edited); newTier > fileTier {
		fileTier = newTier
	}
	cc, ccOK := cyclomaticComplexity(ctx, relPath, edited)
	if ccOK {
		if refined := refineTierWithCC(fileTier, cc); refined != fileTier {
			log.Printf("[%s] %s tier %s→%s via cc=%d", tool, relPath, fileTier, refined, cc)
			fileTier = refined
		}
	}
	// One owner for whether the producer is consulted, and for why it was
	// not. Identical to the conditions it replaces -- including the debug
	// fast-track, which keeps the session's clock on executions rather than
	// candidates -- and now the skip says which threshold turned it away.
	bypass := editGenerationBypass(ctx, fileTier, editWarrantsV3(edited, cc, ccOK),
		isActiveDebugIteration(ctx, relPath))
	recordCandidateGenerationBypass(ctx, tool, bypass, fileTier,
		strings.Count(edited, "\n")+1)
	logBudgetBypass(tool, relPath, bypass)
	if bypass == bypassActiveDebugIteration {
		log.Printf("[%s] %s mid-debug iteration — skipping V3, execution is the feedback", tool, relPath)
	}
	if bypass != bypassNone {
		return keep
	}

	log.Printf("[%s] V3 pipeline activating for %s (file_tier=%d, req_tier=%d)", tool, relPath, fileTier, ctx.Tier)
	// THE protected edit route. A service proposal reaches disk only through
	// the same evidence, authorization, one-time grant, exact-byte delivery,
	// ledger, validation and settlement chain the new-file route uses; when it
	// does not earn that, the caller's own edit is what stays.
	return deliverEditCandidate(ctx, tool, path, relPath, original, edited)
}

// attachV3 copies pipeline metadata onto a successful tool result, so a
// candidate-verified edit reports as one wherever it came from.
func attachV3(result *ToolResult, meta V3EditMetadata) *ToolResult {
	if meta.Used {
		result.V3Used = true
		result.CandidatesTested = meta.CandidatesTested
		result.WinningScore = meta.WinningScore
		result.PhaseSolved = meta.PhaseSolved
		result.VerificationEvidence = meta.VerificationEvidence
	}
	return result
}

// writeWithoutCandidate is the direct path: the caller's own bytes, checked
// and written, with no candidate anywhere in the story.
//
// Two callers reach it and they mean different things -- a V3 outage, and an
// invocation enforcement found no closure path for -- so each supplies its own
// message. What they share is everything after: the same fresh syntax check on
// exactly the bytes about to land, the same structural gate, the same write,
// and no V3 provenance, because nothing here was generated, verified or scored.
func writeWithoutCandidate(ctx *AgentContext, path, content, message string) (*ToolResult, error) {
	// A FRESH check, deliberately: the decision above can take time, and this
	// revalidates the exact content about to be written immediately before
	// writing it. The observation therefore describes the bytes that land,
	// with no window between the two.
	check := fallbackSyntaxOutcomeFor(ctx, path, content).aggregate()
	if check.Status == ValidationFailed {
		synErr := check.Detail
		log.Printf("[write_file] fallback content for %s failed syntax gate: %s",
			logPath(path), safeDiagnosticSummary(synErr))
		// Refused before any byte reached disk, on exactly the content that
		// would have been written.
		return &ToolResult{Success: false,
			Error:            fallbackSyntaxRejection(path, content, synErr),
			MutationStatus:   MutationRefused,
			ValidationKind:   ValidationKindSyntax,
			ValidationStatus: ValidationFailed,
			ValidationDetail: synErr}, nil
	}
	// #147: structural gate on the fallback too. It matters on the
	// DeadlineExceeded case -- /generate timed out but the service is up, so
	// /internal/structural_check (own 5s timeout) still answers; when the
	// service is genuinely down the gate fails open.
	if original, ok := readOriginalForGate(path); ok {
		if introduced := editIntroducesUnresolved(ctx, path, original, content); len(introduced) > 0 {
			log.Printf("[write_file] fallback content introduces unresolved call(s) %v in %s — rejecting",
				logPaths(introduced), logPath(path))
			// Syntax ran first and did not fail on these exact bytes, so the
			// structural failure is the decisive one. Recording it as
			// syntax/failed would assert the opposite of what happened.
			rejection := structuralWriteRejection(path, introduced)
			return &ToolResult{Success: false, Error: rejection,
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindStructural,
				ValidationStatus: ValidationFailed,
				ValidationDetail: rejection}, nil
		}
	}
	// The comparative gates every other write route applies. The producer
	// failing or timing out is no reason to skip them.
	if original, ok := readOriginalForGate(path); ok {
		if msg := embeddedScriptGate(ctx, path, original, content); msg != "" {
			log.Printf("[write_file] fallback content breaks an embedded script in %s — rejecting", logPath(path))
			return &ToolResult{Success: false, Error: msg,
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindStructural,
				ValidationStatus: ValidationFailed,
				ValidationDetail: msg}, nil
		}
		if msg := duplicateMainGuard(path, original, content); msg != "" {
			log.Printf("[write_file] fallback content duplicates the module entrypoint in %s — rejecting", logPath(path))
			return &ToolResult{Success: false, Error: msg,
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindStructural,
				ValidationStatus: ValidationFailed,
				ValidationDetail: msg}, nil
		}
	}
	logMandatoryChecks(ctx, "write_file", path, "producer fallback")
	if message != "" {
		ctx.Stream("text", map[string]string{"content": message})
	}
	res, err := writeFileRecorded(path, content, ctx)
	return applyRouteObservation(res, err, check)
}
