// Package v3client is the proxy's client for the V3 Python service: the
// /v3/generate and /v3/plan event streams. It reads no setting: the caller
// passes the request id and the cap of a call.
package v3client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/itigges22/atlas-proxy/internal/wire"
)

// sseDataPrefix starts the data line of a server-sent event.
const sseDataPrefix = "data: "

// ProgressFn is called for each V3 pipeline progress event.
// `data` carries the structured payload from the V3 service: counts, timings,
// indices, strategy labels. Empty for stages without one; the proxy bridge
// falls back to the human-readable `detail` in that case.
type ProgressFn func(stage, detail string, data map[string]interface{})

// GenerateStreaming sends a file generation request to the V3 Python
// service and streams progress events back via the callback. Returns the
// final result when the pipeline completes.
//
// requestID travels as wire.RequestIDHeader when it is not empty. callCap is
// the configured cap of one call (ATLAS_V3_TIMEOUT, read by the caller); 0
// means no cap.
func GenerateStreaming(reqCtx context.Context, v3URL, requestID string, callCap time.Duration,
	req wire.V3GenerateRequest, onProgress ProgressFn) (*wire.V3GenerateResponse, error) {
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

	// Interactive wall-clock cap. A >15-min Phase-3 repair can finish in an
	// offline bench run, but not while a user waits. Cap the agent path so a runaway
	// pipeline falls back to the model's own content (all three callers treat
	// a V3 error as "write the baseline") instead of hanging the session. The
	// model's content is already syntax-gated, so the fallback is safe.
	// ATLAS_V3_TIMEOUT (seconds) overrides; 0 removes the cap for bench/offline
	// use.
	// The cap actually applied to this call, which is what a timeout must
	// report: quoting the configured ceiling when the session's remaining
	// time shortened it tells the operator to raise a setting that was not
	// the limit they hit.
	var effectiveCap time.Duration
	if callCap > 0 {
		effectiveCap = capAtHalfTheSessionLeft(reqCtx, callCap)
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(reqCtx, effectiveCap)
		defer cancel()
	}
	httpReq, err := newV3GenerateRequest(reqCtx, endpoint, requestID, req, effectiveCap)
	if err != nil {
		return nil, err
	}

	// Abort is user-driven via the bound request context, plus the
	// interactive deadline applied above.
	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("V3 service call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("V3 service returned %d", resp.StatusCode)
	}

	// Read SSE stream: progress events + final result
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20) // 1MB buffer
	result, resultErr := readV3GenerateStream(scanner, onProgress)

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
			if limit > 0 && limit < callCap {
				source = "half the session's remaining time; ATLAS_V3_TIMEOUT is " +
					callCap.String()
			} else if limit == 0 {
				limit = callCap
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
		return nil, &LensUnavailableError{Reason: result.LensUnavailable}
	}

	return result, nil
}

// capAtHalfTheSessionLeft returns the call cap d, or half the time the
// session has left when that is shorter.
func capAtHalfTheSessionLeft(reqCtx context.Context, d time.Duration) time.Duration {
	// The cap is a per-call ceiling, not a claim on the whole run. A
	// pipeline that spends its full 300s on the FIRST of five files
	// leaves the session unable to write the rest.
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
	return d
}

// newV3GenerateRequest builds the POST of req to the V3 service, with the
// budget set to the cap this call has.
func newV3GenerateRequest(reqCtx context.Context, endpoint, requestID string, req wire.V3GenerateRequest,
	effectiveCap time.Duration) (*http.Request, error) {
	// The service plans against the cap this call actually has, not the
	// configured ceiling: told nothing, it planned a 300s run inside a call
	// capAtHalfTheSessionLeft may have cut to a fraction of that.
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
	if requestID != "" {
		httpReq.Header.Set(wire.RequestIDHeader, requestID)
	}
	return httpReq, nil
}

// readV3GenerateStream reads the V3 event stream to its end: it passes each
// progress event to onProgress and returns the result event, or why the
// result could not be decoded.
func readV3GenerateStream(scanner *bufio.Scanner, onProgress ProgressFn) (*wire.V3GenerateResponse, error) {
	var result *wire.V3GenerateResponse
	var resultErr error

	for scanner.Scan() {
		line := scanner.Text()

		// Final result event
		if strings.HasPrefix(line, "event: result") {
			// Next line has the data
			if scanner.Scan() {
				dataLine := scanner.Text()
				result, resultErr = decodeV3Result(dataLine, result, resultErr)
			}
			continue
		}

		// Done marker
		if line == "data: [DONE]" {
			break
		}

		// Progress event
		reportV3Progress(line, onProgress)
	}
	return result, resultErr
}

// decodeV3Result decodes the data line of a result event. A line that is no
// data line leaves the result and its error as they were.
func decodeV3Result(dataLine string, result *wire.V3GenerateResponse, resultErr error) (*wire.V3GenerateResponse, error) {
	if strings.HasPrefix(dataLine, sseDataPrefix) {
		data := strings.TrimPrefix(dataLine, sseDataPrefix)
		var r wire.V3GenerateResponse
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			resultErr = fmt.Errorf("V3 sent a result this proxy could not decode "+
				"(%d bytes): %w", len(data), err)
		} else {
			result = &r
			resultErr = nil
		}
	}
	return result, resultErr
}

// reportV3Progress passes a progress event line to onProgress. A line that
// does not decode as one is skipped.
func reportV3Progress(line string, onProgress ProgressFn) {
	if strings.HasPrefix(line, sseDataPrefix) {
		data := strings.TrimPrefix(line, sseDataPrefix)
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

// PlanStreaming sends a plan-generation request to the V3 Python
// service and streams plan_* progress events through the callback.
// Returns the winning Plan when the planner finishes.
//
// Plans are cheap-but-not-free: 3 candidate samples × ~5s each = ~15s
// wall time, mostly dominated by the LLM. Timeout is 5 min — well above
// expected (15s) and below the agent loop's overall request timeout.
func PlanStreaming(reqCtx context.Context, v3URL, requestID string, req wire.V3PlanRequest,
	onProgress ProgressFn) (*wire.Plan, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal plan request: %w", err)
	}

	endpoint := v3URL + "/v3/plan"
	// Bind the agent's request context so a user Ctrl-C (via /cancel)
	// aborts an in-flight plan run, mirroring GenerateStreaming.
	// nil falls back to Background for any non-agent caller.
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	httpReq, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create plan request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if requestID != "" {
		httpReq.Header.Set(wire.RequestIDHeader, requestID)
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
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plan service returned %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	// The plan result is one SSE `data:` line: a flat step list plus rationale
	// and scoring metadata. 1MB comfortably bounds it while still clearing the
	// 64KB scanner default, which a verbose plan could trip (ErrTooLong would
	// silently drop the plan).
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	plan := readPlanStream(scanner, onProgress)

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

// readPlanStream reads the plan event stream to its end: it passes each
// progress event to onProgress and returns the plan of the result event, or
// nil when no result decoded.
func readPlanStream(scanner *bufio.Scanner, onProgress ProgressFn) *wire.Plan {
	var plan *wire.Plan

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: result") {
			if scanner.Scan() {
				dataLine := scanner.Text()
				plan = decodePlanResult(dataLine, plan)
			}
			continue
		}

		if line == "data: [DONE]" {
			break
		}

		reportV3Progress(line, onProgress)
	}
	return plan
}

// decodePlanResult decodes the data line of a plan's result event. A line that
// is no data line, or does not decode, leaves the plan as it was.
func decodePlanResult(dataLine string, plan *wire.Plan) *wire.Plan {
	if strings.HasPrefix(dataLine, sseDataPrefix) {
		data := strings.TrimPrefix(dataLine, sseDataPrefix)
		var p wire.Plan
		if json.Unmarshal([]byte(data), &p) == nil {
			return &p
		}
	}
	return plan
}

// LensUnavailableError is how a lens that cannot score reaches a route. V3
// reports it in its result, and GenerateStreaming returns this instead of an
// error the route would answer with a fallback write.
type LensUnavailableError struct{ Reason string }

func (e *LensUnavailableError) Error() string {
	return "the geometric lens cannot score: " + e.Reason
}
