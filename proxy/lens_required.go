package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The lens is required (docs/adr/0011-the-lens-is-required.md).
//
// A request is not started while the geometric lens cannot score, and a run
// whose lens stops scoring ends there, saying why. Before this, a lens that
// was switched off, had no model loaded, or could not reach llama-server
// degraded to "no signal": writes went unscored, V3 ranked its candidates
// on neutral scores, and nothing told the user.
//
// "Cannot score" is a fault of the lens or of its model server. An input the
// lens declines -- longer than the embedding batch, empty, or scoring to a
// non-finite number -- is reported as unscored and does not stop anything:
// the lens is up, and that input is the problem (ADR 0010).

// lensUnavailableError is how a lens that cannot score reaches a route. V3
// reports it in its result, and the bridge returns this instead of an error
// the route would answer with a fallback write.
type lensUnavailableError struct{ Reason string }

func (e *lensUnavailableError) Error() string {
	return "the geometric lens cannot score: " + e.Reason
}

// lensUnavailable returns the reason when err says the lens cannot score.
func lensUnavailable(err error) (string, bool) {
	var e *lensUnavailableError
	if errors.As(err, &e) {
		return e.Reason, true
	}
	return "", false
}

// noteLensDown records that the lens stopped scoring during this run. The
// agent loop ends the run at its next check, with the first reason noted.
func (c *AgentContext) noteLensDown(reason string) {
	if c != nil && c.lensDown == "" {
		c.lensDown = reason
	}
}

// lensDownReason is the reason noted by noteLensDown, or "".
func (c *AgentContext) lensDownReason() string {
	if c == nil {
		return ""
	}
	return c.lensDown
}

// lensUnavailableSummary is what the user reads when a run stops because the
// lens stopped scoring.
func lensUnavailableSummary(reason string, wrote bool) string {
	var sb strings.Builder
	sb.WriteString("Stopped: ATLAS needs the geometric lens for every request, and it stopped " +
		"answering (" + reason + "). Run `atlas doctor` to see what is wrong.")
	if wrote {
		sb.WriteString(" Changes made earlier in this run are on disk.")
	} else {
		sb.WriteString(" Nothing was written to disk.")
	}
	return sb.String()
}

// lensDownKinds are the typed scoring failures that describe the lens or its
// model server rather than the input. Anything else a score answer carries
// (embed_capacity, empty_input, nonfinite_score) is an unscored input.
var lensDownKinds = map[string]bool{
	"model_server_error":       true,
	"model_server_unreachable": true,
	"embedding_contract":       true,
	"internal":                 true,
}

// lensReadiness is the probe the request path asks before any work. A
// package variable so a test can substitute a stub; production has one.
var lensReadiness = probeLensReadiness

// lensReadyTTL bounds how stale a readiness answer may be: a lens that goes
// down is noticed on the next request after it, and a burst of requests does
// not probe it once each.
const lensReadyTTL = 5 * time.Second

var lensReadyCache struct {
	sync.Mutex
	url string
	at  time.Time
	ok  bool
	why string
}

// lensReady reports whether the lens at url can score, and why not when it
// cannot, from an answer at most lensReadyTTL old.
func lensReady(url string) (bool, string) {
	lensReadyCache.Lock()
	defer lensReadyCache.Unlock()
	if lensReadyCache.url == url && time.Since(lensReadyCache.at) < lensReadyTTL {
		return lensReadyCache.ok, lensReadyCache.why
	}
	ok, why := lensReadiness(url)
	lensReadyCache.url, lensReadyCache.at = url, time.Now()
	lensReadyCache.ok, lensReadyCache.why = ok, why
	return ok, why
}

// probeLensReadiness asks the lens itself. /ready first: it retries a
// self-test that failed while llama-server was still starting, and answers
// 503 with a reason when the lens cannot score. Then /health, because
// /ready answers 200 for a lens with no G(x) model, which cannot score.
func probeLensReadiness(url string) (bool, string) {
	resp, err := healthClient.Get(url + "/ready")
	if err != nil {
		return false, "it is unreachable at " + url
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusServiceUnavailable:
		return false, lensNotReadyReason(body)
	default:
		return false, fmt.Sprintf("its readiness check answered HTTP %d", resp.StatusCode)
	}

	resp, err = healthClient.Get(url + "/health")
	if err != nil {
		return false, "it is unreachable at " + url
	}
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	var h lensHealthShape
	if err := json.Unmarshal(body, &h); err != nil {
		return false, "its health report could not be read"
	}
	if ok, why := lensHealthVerdict(h); !ok {
		return false, why
	}
	return true, ""
}

// lensHealthVerdict classifies a lens health report: whether this lens can
// score, and why not. An uncalibrated lens can: it scores raw energies, and
// only the calibrated uses of them (normalized routing, veto and correction
// thresholds) wait for calibration, as its status says.
func lensHealthVerdict(h lensHealthShape) (bool, string) {
	lens := h.Subsystems.Lens
	switch {
	case !lens.CostFieldLoaded:
		why := "it has no C(x) model loaded"
		if lens.SelfTestError != "" {
			why += ": " + truncateStr(lens.SelfTestError, 200)
		}
		return false, why
	case !lens.GxLoaded:
		return false, "it has no G(x) model loaded; run `atlas lens build` or `atlas model install-artifacts`"
	case lens.FingerprintOK != nil && !*lens.FingerprintOK:
		return false, "it has drifted from the served model: " + truncateStr(lens.FingerprintError, 200)
	case !lens.SelfTestPass:
		return false, "its self-test failed: " + truncateStr(lens.SelfTestError, 200)
	case h.Subsystems.LlamaServer.Reachable != nil && !*h.Subsystems.LlamaServer.Reachable:
		return false, "it cannot reach llama-server"
	}
	return true, ""
}

// lensNotReadyReason reads the lens's 503 body, which nests its payload under
// "detail", into a reason a user can act on.
func lensNotReadyReason(body []byte) string {
	var p struct {
		Detail struct {
			Reason        *string `json:"reason"`
			LlamaServer   *bool   `json:"llama_server"`
			FingerprintOK *bool   `json:"fingerprint_ok"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(body, &p); err == nil {
		d := p.Detail
		switch {
		case d.LlamaServer != nil && !*d.LlamaServer:
			return "it cannot reach llama-server"
		case d.FingerprintOK != nil && !*d.FingerprintOK:
			return "it has drifted from the served model"
		case d.Reason != nil && *d.Reason != "":
			return "it is not ready: " + truncateStr(*d.Reason, 200)
		}
	}
	return "it is not ready"
}

// stopLensDown ends the run because the lens cannot score. The tool call the
// model made is answered first, so the stream never carries a call without a
// result.
func (s *runState) stopLensDown(ctx *AgentContext, tool, reason string) error {
	s.pendingToolCall = ""
	ctx.Stream("tool_result", map[string]interface{}{
		"tool":    tool,
		"success": false,
		"error":   "not run: the geometric lens stopped answering",
	})
	emitTerminal(ctx, s, TerminalFailed, "lens_unavailable",
		lensUnavailableSummary(reason, s.madeProductiveChange)+liveBackgroundJobNote(ctx))
	return nil
}
