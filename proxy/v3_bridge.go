package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// v3CallTimeout is the interactive wall-clock cap for a single V3 pipeline
// call. It bounds the long-tail repair stall that left a user waiting 11 min.
// Set ATLAS_V3_TIMEOUT to a second count to override, or 0 to disable the cap
// (restores the May-10 uncapped behavior for offline bench runs).
//
// 300s, not the 180s this shipped with. 180 could not contain the pipeline it
// was capping: PlanSearch spends two LLM calls per candidate, so k=3 costs
// ~162s at the measured ~22s per call, before the probe and the self-test
// that precede it. Measured across 43 runs, sessions spent a median 207s of a
// 180s budget on generation alone and phase-3 repair was reached with 7-9s
// left and skipped 19 times. The corrected budget cap responds by cutting k
// to 1, which removes the candidates candidate-agreement needs to compare.
func v3CallTimeout() time.Duration {
	if v := os.Getenv("ATLAS_V3_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 300 * time.Second
}

// ---------------------------------------------------------------------------
// V3 Bridge — Go ↔ Python V3 service communication
// ---------------------------------------------------------------------------

// V3ProgressFn is called for each V3 pipeline progress event.
// `data` carries the structured payload from the V3 service (since the
// 2026-05 observability pass) — counts, timings, indices, strategy
// labels. Empty for legacy stages that haven't been enriched yet; the
// proxy bridge falls back to the human-readable `detail` in that case.
type V3ProgressFn func(stage, detail string, data map[string]interface{})

// callV3GenerateStreaming sends a file generation request to the V3 Python
// service and streams progress events back via the callback. Returns the
// final result when the pipeline completes.
func callV3GenerateStreaming(reqCtx context.Context, v3URL string, req V3GenerateRequest, onProgress V3ProgressFn) (*V3GenerateResponse, error) {
	endpoint := v3URL + "/v3/generate"
	// Bind the agent's request context so a user Ctrl-C (which cancels
	// ctx.Ctx via /cancel) actually aborts an in-flight V3 run. The
	// May-10 comment claimed cancellation "still works", but http.NewRequest
	// carried no context, so Ctrl-C could not stop a multi-minute
	// PlanSearch — exactly the "ctrl-c does not stop it" report. nil falls
	// back to Background for any non-agent caller.
	if reqCtx == nil {
		reqCtx = context.Background()
	}

	// Interactive wall-clock cap. The May-10 design ran V3 uncapped so a
	// >15-min Phase-3 repair could finish; that's right for an offline bench
	// run but unshippable interactively — observed an 11-min stall on a
	// 103-line write_file while a user waited. Cap the agent path so a runaway
	// pipeline falls back to the model's own content (all three callers treat
	// a V3 error as "write the baseline") instead of hanging the session. The
	// model's content is already syntax-gated, so the fallback is safe.
	// ATLAS_V3_TIMEOUT (seconds) overrides; 0 restores the uncapped behavior
	// for bench/offline use.
	// The cap actually applied to this call, which is what a timeout must
	// report: quoting the configured ceiling when the session's remaining
	// time shortened it tells the operator to raise a setting that was not
	// the limit they hit.
	var effectiveCap time.Duration
	if d := v3CallTimeout(); d > 0 {
		// The cap is a per-call ceiling, not a claim on the whole run. A
		// pipeline that spends its full 300s on the FIRST of five files
		// leaves the session unable to write the rest: measured 2026-09-14,
		// V3 burned 295s of a 570s work budget repairing file one and the
		// run ended with one file and no verification, while the same prompt
		// with a cheap first file delivered five files in 248s.
		//
		// Take at most half of what is left, so whatever V3 spends on this
		// file, the run keeps as much again for the work still to do. When
		// the pipeline is cut short the caller falls back to the model's own
		// content, which is already syntax-gated, so a smaller window
		// degrades instead of failing.
		if deadline, ok := reqCtx.Deadline(); ok {
			if half := time.Until(deadline) / 2; half > 0 && half < d {
				log.Printf("[v3] capping this call at %s — half of the %s left in the session (configured %s)",
					half.Round(time.Second), time.Until(deadline).Round(time.Second), d)
				d = half
			}
		}
		effectiveCap = d
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(reqCtx, d)
		defer cancel()
	}
	// The service plans against the cap this call actually has, not the
	// configured ceiling: told nothing, it planned a 300s run inside a call
	// the line above may have cut to a fraction of that.
	req.BudgetMs = 0
	if effectiveCap > 0 {
		req.BudgetMs = effectiveCap.Milliseconds()
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal V3 request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create V3 request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Trace identity, forwarded so a candidate pool can be joined back to the
	// request that caused it. It is a JOIN KEY only: it may be absent, it may
	// repeat outside a controlled harness, and nothing downstream may treat it
	// as unique or as cancellation authority. It never reaches the model.
	if id := requestIDFromContext(reqCtx); id != "" {
		httpReq.Header.Set(requestIDHeader, id)
	}

	// Abort is user-driven via the bound request context, plus the
	// interactive deadline applied above.
	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("V3 service call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("V3 service returned %d", resp.StatusCode)
	}

	// Read SSE stream: progress events + final result
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20) // 1MB buffer

	var result *V3GenerateResponse
	var resultErr error

	for scanner.Scan() {
		line := scanner.Text()

		// Final result event
		if strings.HasPrefix(line, "event: result") {
			// Next line has the data
			if scanner.Scan() {
				dataLine := scanner.Text()
				if strings.HasPrefix(dataLine, "data: ") {
					data := strings.TrimPrefix(dataLine, "data: ")
					var r V3GenerateResponse
					if err := json.Unmarshal([]byte(data), &r); err != nil {
						resultErr = fmt.Errorf("V3 sent a result this proxy could not decode "+
							"(%d bytes): %w", len(data), err)
					} else {
						result = &r
						resultErr = nil
					}
				}
			}
			continue
		}

		// Done marker
		if line == "data: [DONE]" {
			break
		}

		// Progress event
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var event struct {
				Stage  string                 `json:"stage"`
				Detail string                 `json:"detail"`
				Data   map[string]interface{} `json:"data"`
			}
			if json.Unmarshal([]byte(data), &event) == nil && onProgress != nil {
				onProgress(event.Stage, event.Detail, event.Data)
			}
		}
	}

	if result == nil {
		// "completed without result" was the message for every one of
		// these, including the case where this side hung up: the context
		// deadline cancels the request, Scan returns false, and V3 is
		// blamed for finishing empty while it was still working. Report
		// which of them actually happened.
		switch {
		case resultErr != nil:
			return nil, resultErr
		case reqCtx.Err() != nil:
			limit := effectiveCap
			source := "ATLAS_V3_TIMEOUT"
			if configured := v3CallTimeout(); limit > 0 && limit < configured {
				source = "half the session's remaining time; ATLAS_V3_TIMEOUT is " +
					configured.String()
			} else if limit == 0 {
				limit = configured
			}
			return nil, fmt.Errorf("V3 was still working when this proxy hung up at the "+
				"%s cap (%s); its work is discarded and the write falls back "+
				"to the model's own output: %w", limit, source, reqCtx.Err())
		case scanner.Err() != nil:
			return nil, fmt.Errorf("V3 stream ended early: %w", scanner.Err())
		default:
			return nil, fmt.Errorf("V3 stream closed without sending a result event")
		}
	}
	// The lens could not score inside this call. That is not a V3 failure
	// the route may answer with a fallback write: the run stops on it
	// (lens_required.go).
	if result.LensUnavailable != "" {
		return nil, &lensUnavailableError{Reason: result.LensUnavailable}
	}

	return result, nil
}

// callV3PlanStreaming sends a plan-generation request to the V3 Python
// service and streams plan_* progress events through the callback.
// Returns the winning Plan when the planner finishes.
//
// Plans are cheap-but-not-free: 3 candidate samples × ~5s each = ~15s
// wall time, mostly dominated by the LLM. Timeout is 5 min — well above
// expected (15s) and below the agent loop's overall request timeout.
func callV3PlanStreaming(reqCtx context.Context, v3URL string, req V3PlanRequest, onProgress V3ProgressFn) (*Plan, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal plan request: %w", err)
	}

	endpoint := v3URL + "/v3/plan"
	// Bind the agent's request context so a user Ctrl-C (via /cancel)
	// aborts an in-flight plan run, mirroring callV3GenerateStreaming.
	// nil falls back to Background for any non-agent caller.
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	httpReq, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create plan request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if id := requestIDFromContext(reqCtx); id != "" {
		httpReq.Header.Set(requestIDHeader, id)
	}

	// May 10 2026: timeout removed (was 5 min). Plan generation can run
	// long on multi-candidate scoring; bounding it via the client
	// timeout killed slow-but-progressing calls. Abort is user-driven
	// via the bound request context.
	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("plan call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plan service returned %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	// The plan result is one SSE `data:` line: a flat step list plus rationale
	// and scoring metadata. 1MB comfortably bounds it while still clearing the
	// 64KB scanner default, which a verbose plan could trip (ErrTooLong would
	// silently drop the plan).
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	var plan *Plan

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: result") {
			if scanner.Scan() {
				dataLine := scanner.Text()
				if strings.HasPrefix(dataLine, "data: ") {
					data := strings.TrimPrefix(dataLine, "data: ")
					var p Plan
					if json.Unmarshal([]byte(data), &p) == nil {
						plan = &p
					}
				}
			}
			continue
		}

		if line == "data: [DONE]" {
			break
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var event struct {
				Stage  string                 `json:"stage"`
				Detail string                 `json:"detail"`
				Data   map[string]interface{} `json:"data"`
			}
			if json.Unmarshal([]byte(data), &event) == nil && onProgress != nil {
				onProgress(event.Stage, event.Detail, event.Data)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		// Most likely bufio.ErrTooLong on an oversized result line. Surface it
		// so the cause is diagnosable instead of a misleading "no result".
		return nil, fmt.Errorf("reading plan stream: %w", err)
	}

	if plan == nil {
		return nil, fmt.Errorf("plan service completed without result")
	}

	return plan, nil
}

// ---------------------------------------------------------------------------
// Evidence envelope validation
// ---------------------------------------------------------------------------
//
// The response boundary owns this: the same file that decodes /v3/generate
// decides whether what arrived may be read. There is no separate evidence
// service or helper layer -- one wire representation in types.go, one place
// that validates it here.

// Closed vocabularies. An unknown value is not a new case to guess at — it
// means this build and that producer disagree about what the field can say.
var (
	evidenceStrengths = map[string]bool{
		"syntax": true, "runtime": true, "behavioral": true, "oracle": true,
	}
	evidenceExecutionStatuses = map[string]bool{
		"ok": true, "timeout": true, "error": true, "crash": true, "skipped": true,
	}
	evidenceSelectionStatuses = map[string]bool{
		"verified_winner": true, "best_not_closure_eligible": true, "tied": true,
		"incomparable": true, "ineligible": true, "no_verified_winner": true,
	}
)

// Validate reports whether the envelope may be read, and why not when it may
// not. It is strict on purpose: every rejection here is a case where reading
// on would mean asserting something the producer did not establish.
func (e *V3EvidenceEnvelope) Validate() (EvidenceAvailability, string) {
	if e == nil {
		return EvidenceAbsent, "no evidence envelope"
	}
	if e.WireVersion == "" {
		return EvidenceUnavailable, "envelope carries no wire version"
	}
	if major := strings.SplitN(e.WireVersion, ".", 2)[0]; major != evidenceWireMajor {
		return EvidenceUnavailable, "unsupported wire version " + e.WireVersion
	}
	// Identity must be COMPLETE, matching the producer's own rule: a partial
	// identity is how unrelated measurements compare equal through blanks.
	id := e.Identity
	if id.ContractID == "" || id.ContractVersion == "" || id.ArtifactScope == "" ||
		id.EvaluationContextHash == "" {
		return EvidenceUnavailable, "identity incomplete"
	}
	if id.CalibrationID == "" && (id.AdapterID == "" || id.AdapterVersion == "") {
		return EvidenceUnavailable, "identity incomplete: no calibration or adapter"
	}
	if !evidenceStrengths[e.Evaluation.EvidenceStrength] {
		return EvidenceUnavailable, "unknown evidence strength " + e.Evaluation.EvidenceStrength
	}
	if !evidenceExecutionStatuses[e.Evaluation.ExecutionStatus] {
		return EvidenceUnavailable, "unknown execution status " + e.Evaluation.ExecutionStatus
	}
	if !evidenceSelectionStatuses[e.Selection.Status] {
		return EvidenceUnavailable, "unknown selection status " + e.Selection.Status
	}
	// Internal contradictions. A producer that claims closure over a run that
	// did not complete, or over unmet requirements, is a producer whose other
	// fields cannot be relied on either.
	if e.Evaluation.ClosureEligible {
		if e.Evaluation.ExecutionStatus != "ok" {
			return EvidenceUnavailable, "closure claimed over execution status " +
				e.Evaluation.ExecutionStatus
		}
		if !e.Evaluation.RequirementsComplete {
			return EvidenceUnavailable, "closure claimed with incomplete requirements"
		}
		if !e.Evaluation.Supported {
			return EvidenceUnavailable, "closure claimed for an unsupported artifact"
		}
	}
	if e.Selection.Status == "verified_winner" && !e.Evaluation.ClosureEligible {
		return EvidenceUnavailable, "verified winner without closure eligibility"
	}
	return EvidenceAvailable, ""
}

// Available is the single predicate a caller may use before reading fields.
func (e *V3EvidenceEnvelope) Available() bool {
	a, _ := e.Validate()
	return a == EvidenceAvailable
}

// DescribesBytes reports whether this evidence is about exactly `code`. The
// producer's own describes_delivered_candidate flag is not consulted: the
// consumer knows which bytes it is about to write and hashes those.
func (e *V3EvidenceEnvelope) DescribesBytes(code string) bool {
	if e == nil || e.Identity.CandidateContentHash == "" {
		return false
	}
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:]) == e.Identity.CandidateContentHash
}

// EvidenceSupportsProvenanceFor is THE gate for replacing the caller's content
// with a generated candidate and for attaching service provenance to it. Every
// condition is necessary and each one is a claim the service actually made:
//
//	available        the envelope is present, same-major and self-consistent
//	verified_winner  the SELECTION concluded a winner, not merely a best record
//	closure_eligible that winner met its own contract's floor
//	describes these  the evidence is about the exact bytes about to be written
//
// `Passed`, `PhaseSolved`, `WinningScore` and the verification-evidence strings
// are deliberately not consulted: `passed` collapses a compile smoke, a partial
// oracle score and a complete one into one boolean, which is the conflation the
// envelope exists to replace. A caller may still report them; none of them may
// authorize anything.
func EvidenceSupportsProvenanceFor(e *V3EvidenceEnvelope, code string) (bool, string) {
	if code == "" {
		return false, "no candidate code"
	}
	availability, reason := e.Validate()
	if availability != EvidenceAvailable {
		return false, reason
	}
	if e.Selection.Status != "verified_winner" {
		return false, "selection status " + e.Selection.Status
	}
	if !e.Evaluation.ClosureEligible {
		return false, "record is not closure-eligible"
	}
	if !e.DescribesBytes(code) {
		return false, "evidence describes a different candidate"
	}
	return true, ""
}

// v3DeliveryAuthorized answers the one question the write path asks of a V3
// response: may THESE bytes replace the caller's content and carry service
// provenance. It is the only place that decision is made, so a second caller
// cannot invent a looser rule.
func v3DeliveryAuthorized(result *V3GenerateResponse, code string) (bool, string) {
	if result == nil {
		return false, "no V3 response"
	}
	return EvidenceSupportsProvenanceFor(result.Evidence, code)
}

// evidenceTelemetry renders the envelope for the telemetry envelope payload.
// Returned as the decoded structure rather than a summary so nothing is lost
// on the way to a subscriber; callers must not put it on a ToolResult, which
// is projected to the model.
func evidenceTelemetry(e *V3EvidenceEnvelope, unavailableReason string) map[string]interface{} {
	availability, reason := e.Validate()
	if reason == "" {
		reason = unavailableReason
	}
	out := map[string]interface{}{"availability": string(availability)}
	if reason != "" {
		out["reason"] = reason
	}
	if e != nil {
		out["envelope"] = e
	}
	return out
}
