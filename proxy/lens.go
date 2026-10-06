// The proxy's lens surfaces: everything that reads the geometric-lens
// service or reports whether it is calibrated for the model currently being
// served.
//
// In file order:
//
//	Per-write scoring — every write_file / edit_file payload goes to
//	  /internal/lens/score-per-step for its C(x) and G(x) numbers. A run of
//	  low gx_score_min, or one write below the severe cutoff, is the "stub
//	  loop" signal the agent loop breaks with a corrective. Thresholds come
//	  from the model's own calibration or the check is skipped — one model's
//	  cutoffs are meaningless against another's residual stream.
//	VerificationRecord — the evidence record the agent loop keeps for every
//	  passing verification command.
//	Calibration probes — /v1/calibration/status, built from the lens
//	  service's /health plus a local read of the ASA control vector, is the
//	  seven-dimension table the TUI badge and `atlas doctor` both render.
//
// The calibration probe sits next to the scoring code whose behavior it
// reports on.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Agent-loop lens integration. Scores write_file / edit_file content
// per-tool-call via geometric-lens /internal/lens/score-per-step. Tracks
// recent gx_score_min values per session so a "stub loop" pattern (the
// kind that hit the May 6 templates/resources.html session in production)
// can be detected and broken with a corrective system message before the
// next LLM call.

// Number of consecutive low-score write/edit calls that count as a
// regression. 2 is the minimum that's clearly a pattern (not a one-off
// dud); higher values (3+) miss the May 6 stub-loop case where the
// model only got 2 attempts in before the error-loop break fired.
const lensRegressionRunLength = 2

// Severe-threshold short-circuit: a single write whose gx_score_min
// drops below this is bad enough to trigger intervention immediately
// without waiting for a second confirmation. Calibrated from the May 7
// dashboard.html session where gx_min=0.040 on turn 2 (off_rails at
// token 14 of 840) was unambiguously a stub but the run-of-2 rule
// waited until turn 4 to act — by which point V3's sandbox-verifier
// had already approved the write. Anything below 0.05 is so far into
// the "likely_incorrect" band that one sample is enough signal.
//
// Language-agnostic: gx values are normalized 0-1 outputs of the
// XGBoost head on the residual stream. They don't depend on the
// surface language of the file being scored — Python stub, HTML stub,
// Rust stub, Java stub all produce the same kind of low gx_min when
// the model's internal state collapses to a placeholder pattern.
type lensAggregate struct {
	FirstOffRailsIdx int     `json:"first_off_rails_idx"`
	GxScoreMin       float64 `json:"gx_score_min"`
	GxScoreMean      float64 `json:"gx_score_mean"`
	CxNormMax        float64 `json:"cx_norm_max"`
}

// lensThresholds are the per-model operating points the lens service judged a
// score against. They ship with the lens artifact (gx_thresholds.json) and are
// returned in every score response so the proxy's regression checks use the
// loaded model's calibration instead of the hardcoded fallback constants.
type lensThresholds struct {
	OffRails float64 `json:"off_rails"`
	Low      float64 `json:"low"`
	Severe   float64 `json:"severe"`
}

// lensFailure is why the lens did not score an input. `embed_capacity` is
// llama-server refusing the embedding because the input exceeds its physical
// batch (ATLAS_UBATCH); the two counts are the server's own. It is a transport
// limit on this deployment, reported as such, and never a score.
type lensFailure struct {
	Kind           string `json:"kind"`
	InputTokens    int    `json:"input_tokens,omitempty"`
	CapacityTokens int    `json:"capacity_tokens,omitempty"`
	Status         int    `json:"status,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

type lensPerStepResult struct {
	Enabled     bool            `json:"enabled"`
	Scored      *bool           `json:"scored,omitempty"`
	GxAvailable bool            `json:"gx_available"`
	NTokens     int             `json:"n_tokens"`
	HiddenDim   int             `json:"hidden_dim"`
	Layer       string          `json:"layer"`
	Aggregate   lensAggregate   `json:"aggregate"`
	LatencyMS   float64         `json:"latency_ms"`
	Thresholds  *lensThresholds `json:"thresholds,omitempty"`
	Failure     *lensFailure    `json:"failure,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// calibratedThresholds returns operating points only when the selected
// model's Lens artifact supplied a valid calibration. Uncalibrated scores are
// useful telemetry, but must not trigger corrective behavior using another
// model's cutoffs.
func (r lensPerStepResult) calibratedThresholds() (low, severe float64, ok bool) {
	if r.Thresholds == nil || r.Thresholds.Low <= 0 || r.Thresholds.Severe <= 0 ||
		r.Thresholds.Severe > r.Thresholds.Low {
		return 0, 0, false
	}
	return r.Thresholds.Low, r.Thresholds.Severe, true
}

// scoreContentForAgent calls /internal/lens/score-per-step on the given
// text and returns the parsed result, whether it is a score, and -- when the
// lens itself cannot score -- why. The lens is required: a non-empty third
// value ends the run (lens_required.go). An answer that declined this input
// (a typed failure such as embed_capacity, or no tokens scored) is returned
// unscored with an empty reason, its failure attached, so nothing downstream
// can read it as a verdict. Carries the agent's ctx so client cancellation
// kills the lens call too.
func scoreContentForAgent(ctx context.Context, lensURL, content string) (lensPerStepResult, bool, string) {
	var zero lensPerStepResult
	if lensURL == "" || content == "" {
		return zero, false, ""
	}
	body, err := json.Marshal(map[string]interface{}{"text": content})
	if err != nil {
		return zero, false, ""
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := newLensRequest(reqCtx, "POST", lensURL+"/internal/lens/score-per-step", body)
	if err != nil {
		return zero, false, ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// The request was cancelled: that is the cancellation's to
			// report, not the lens's.
			return zero, false, ""
		}
		log.Printf("[agent-lens] score request failed: %v", err)
		return zero, false, "it did not answer: " + truncateStr(err.Error(), 160)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, false, "its answer could not be read"
	}
	if resp.StatusCode != http.StatusOK {
		return zero, false, fmt.Sprintf("it answered HTTP %d", resp.StatusCode)
	}
	var r lensPerStepResult
	if err := json.Unmarshal(raw, &r); err != nil {
		log.Printf("[agent-lens] score parse failed: %v", err)
		return zero, false, "its answer could not be read"
	}
	if !r.Enabled {
		return zero, false, "it has no model loaded"
	}
	if r.Failure != nil && lensDownKinds[r.Failure.Kind] {
		log.Printf("[agent-lens] the lens cannot score (%s)", r.Failure.Kind)
		return r, false, r.Failure.Kind + ": " + truncateStr(r.Failure.Detail, 160)
	}
	if r.Failure != nil || (r.Scored != nil && !*r.Scored) || r.NTokens == 0 {
		if r.Failure != nil {
			// Counts are stated in prose: the log filter masks `<name>token<...>=`
			// pairs as credentials, and these are the numbers an operator needs.
			log.Printf("[agent-lens] unscored (%s): input %d tokens, embed capacity %d tokens, status %d",
				r.Failure.Kind, r.Failure.InputTokens, r.Failure.CapacityTokens, r.Failure.Status)
		} else {
			log.Printf("[agent-lens] unscored: no tokens scored (%s)", truncateStr(r.Error, 120))
		}
		return r, false, ""
	}
	return r, true, ""
}

// extractScorableContent pulls lens-scoreable text from a tool call.
// Only write_file (`content`) and edit_file (`new_str`) qualify — other
// tools either don't carry generated text (read_file, list_directory)
// or the scoring-on-shell-commands signal isn't useful. Returns the
// text and a bool indicating whether the tool was scoreable.
func extractScorableContent(toolName string, args json.RawMessage) (string, bool) {
	switch toolName {
	case "write_file":
		var p struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(args, &p); err == nil && p.Content != "" {
			return p.Content, true
		}
	case "edit_file":
		var p struct {
			NewStr string `json:"new_str"`
		}
		if err := json.Unmarshal(args, &p); err == nil && p.NewStr != "" {
			return p.NewStr, true
		}
	}
	return "", false
}

// extractFailurePath returns the path argument of a tool call when
// the tool operates on a file (read/write/edit/structural_edit/delete/
// search/list/find/run_background's cwd). Used by the path-aware
// error-loop breaker to distinguish "stuck on one file" from
// "grinding through different files." Returns "" when no path is
// applicable to the tool (e.g. run_command's arbitrary
// shell) — empty paths compare unequal, which prevents the breaker
// from firing on tool-mix sequences.
func extractFailurePath(toolName string, args json.RawMessage) string {
	switch toolName {
	// insert_after and replace_lines were missing here, so a failing loop on
	// either returned "" and the path-aware 3-strike breaker read three
	// identical failures as three different paths and never fired.
	case "read_file", "write_file", "edit_file", "structural_edit", "delete_file", "find_file",
		"insert_after", "replace_lines":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &p); err == nil {
			return p.Path
		}
	case "list_directory", "search_files":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &p); err == nil {
			return p.Path
		}
	}
	return ""
}

// agentLensRegression returns the corrective message to inject (and true)
// when the recent agent-loop scoring history shows a quality crash
// pattern. Returns ("", false) when no intervention is warranted.
//
// Pattern: the most recent N (= lensRegressionRunLength) gx_score_min
// values are all below the calibrated low threshold. This is the "model is
// stuck on a stub or near-duplicate response" signature — the May 6
// resources.html loop is the canonical example.
// low and severe are the per-model thresholds (resolved from the lens score's
// bundled thresholds. Callers skip intervention when calibration is absent.
func agentLensRegression(history []float64, low, severe float64) (string, bool) {
	if len(history) == 0 {
		return "", false
	}
	// Severe single-write short-circuit: gx_min below the severe threshold
	// is so far into the "likely_incorrect" band that one sample is enough —
	// don't wait for a second confirmation while V3's sandbox-verifier
	// rubber-stamps the stub in the same iteration.
	last := history[len(history)-1]
	if last < severe {
		return fmt.Sprintf(
			"⚠ Lens severe-quality alert: the geometric lens scored your last write at "+
				"gx_min=%.3f, which is in the unambiguously-bad band (<%.2f). This usually "+
				"means the file is a stub, a placeholder, or has collapsed into a repetitive "+
				"pattern. STOP and try a different approach: (a) read a sibling file in the "+
				"same directory to model the right structure, (b) ask the user for "+
				"clarification on what concrete content is needed, or (c) skip this file and "+
				"move on if it's not blocking the verify step. DO NOT re-issue the same "+
				"write — the lens will catch it again.",
			last, severe), true
	}
	// Run-of-N moderate-low check: lensRegressionRunLength consecutive
	// scores below the calibrated low threshold. Catches gradual stub
	// loops where each write is moderately bad but no single one is
	// catastrophic.
	if len(history) < lensRegressionRunLength {
		return "", false
	}
	recent := history[len(history)-lensRegressionRunLength:]
	for _, score := range recent {
		if score >= low {
			return "", false
		}
	}
	return fmt.Sprintf(
		"⚠ Lens regression detected: the geometric lens flagged your last %d write attempts as "+
			"severely low-quality (gx_score_min values: %s). This is the signature of a stuck "+
			"or repetitive pattern — likely a stub/placeholder being submitted over and over, or "+
			"near-duplicate responses that aren't making progress. STOP and try a different "+
			"approach: (a) read a sibling file in the same directory to model the right "+
			"structure, (b) ask the user for clarification on what concrete content is needed, "+
			"or (c) skip this file and move on if it's not blocking the verify step.",
		lensRegressionRunLength, formatScoreSlice(recent)), true
}

func formatScoreSlice(s []float64) string {
	parts := make([]string, len(s))
	for i, v := range s {
		parts[i] = fmt.Sprintf("%.3f", v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// VerificationRecord binds one green verification command to what it
// exercised. Covered maps each session-written path the command named to
// the sha256 of its bytes at the moment the command passed — the bytes the
// run vouched for, nothing else.
type VerificationRecord struct {
	Command  string            // the run_command line
	Redirect string            // stdin redirect source ("" = ran standalone)
	Covered  map[string]string // session-written path -> sha256 when it ran
	Turn     int

	// Kind is what the run demonstrates (commandEvidenceKind.String):
	// "execution" or "probe" show the program working; "static" (a parse,
	// lint or build) and "none" are recorded only because the client declared
	// the command, and bind no coverage. Empty is a record made before kinds
	// existed, which only a verification command could produce.
	Kind string
	// Failed marks a run that exited non-zero. It is kept so a later failure
	// on the same bytes takes back an earlier pass (coverageRecord).
	Failed bool

	// The workspace this run was about, stamped from workspaceIdentity after
	// the command's own effects were reconciled into the ledger.
	//
	// Covered answers "which artifact bytes did this command exercise", and a
	// command that names no file answers it with nothing -- which is honest,
	// and is why a pathless command could never be current before. These two
	// answer a different question: WHEN did it run, in terms the session can
	// re-check. A pathless command is current exactly while both still equal
	// the workspace's current identity, and any material mutation to a tracked
	// artifact moves them.
	//
	// They are not a second coverage: they say nothing about which bytes the
	// command touched, and nothing here may satisfy a path obligation.
	WorkspaceGeneration int
	WorkspaceStateHash  string
}

// showsWorking reports a passing run of a kind that shows the program
// working.
func (r VerificationRecord) showsWorking() bool {
	if r.Failed {
		return false
	}
	switch r.Kind {
	case "", "execution", "probe":
		return true
	}
	return false
}

// Calibration status endpoint — surfaces lens + ASA compat for the TUI.
//
// GH #101: the geometric-lens /health endpoint already exposes the
// data we need (cost_field_dim, embed_dim, cost_field_loaded). This file
// forwards that into a verdict-shaped response under /v1/calibration/status
// that the TUI renders as a header badge.
//
// GH #113 extends the `asa` block from a file-presence check to a
// proper dim-vs-model probe; the JSON shape stays the same so TUI
// rendering doesn't churn.

// CalibrationStatus is the JSON returned by /v1/calibration/status.
// Shape is stable: TUI and atlas doctor both key off it.
type CalibrationStatus struct {
	Lens       LensStatus        `json:"lens"`
	ASA        ASAStatus         `json:"asa"`
	Dimensions []StatusDimension `json:"dimensions"`
}

// StatusDimension is one row of the canonical seven-dimension status
// (SUPPORT_MATRIX § "Reference-model status dimensions"). Separating
// these prevents the ambiguity where "the lens works" conflated model
// runtime, raw scoring, calibration, and intervention behavior. Every
// surface that shows lens/ASA status (this endpoint, the TUI badge,
// atlas doctor, atlas lens check) renders the SAME rows so they cannot
// disagree — they all read this list.
type StatusDimension struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// buildDimensions maps the raw lens/ASA probe onto the seven named
// dimensions. Intervention is reported "neutral" whenever calibration is
// absent, matching the enforced runtime behavior (agent.go only applies
// thresholds when calibratedThresholds() succeeds) — a disabled/
// uncalibrated lens never steers using another model's cutoffs.
func buildDimensions(lens LensStatus, asa ASAStatus) []StatusDimension {
	reachable := lens.Verdict != "unreachable"

	// The model server as the lens sees it. Reaching the lens is not
	// reaching llama-server: this row said "model served and reachable"
	// while the lens reported llama-server down.
	modelRuntime := "supported"
	modelDetail := "model served and reachable"
	switch {
	case !reachable:
		modelRuntime, modelDetail = "unknown", "the lens is unreachable, so nothing reports on the model server"
	case lens.ModelServerReachable != nil && !*lens.ModelServerReachable:
		modelRuntime, modelDetail = "unreachable", "llama-server is not reachable from the lens"
	}

	// The agent runs only while the lens can score (lens_required.go).
	canScore := lensVerdictCanScore(lens.Verdict)
	directAgent, directDetail := "supported", "tools, permissions and sandbox verify; the lens can score"
	if !canScore {
		directAgent = "blocked"
		directDetail = "requests are refused while the lens cannot score: " + lens.Verdict
		if lens.Hint != "" {
			directDetail += " — " + lens.Hint
		}
	}

	// Identity/dimension contract.
	identity := "supported"
	identityDetail := "cost field matches the served model's dimension"
	switch {
	case !reachable:
		identity, identityDetail = "unknown", "service unreachable"
	case !lens.CostFieldLoaded:
		identity, identityDetail = "no-artifacts",
			"no cost field loaded for this model"
	case lens.EmbedDim > 0 && lens.CostFieldDim != lens.EmbedDim:
		identity, identityDetail = "dim-mismatch",
			fmt.Sprintf("cost field is %d-dim, model emits %d-dim",
				lens.CostFieldDim, lens.EmbedDim)
	}

	// Raw scoring availability.
	scoring := "disabled"
	scoringDetail := "cost field / G(x) not loaded"
	if !canScore && reachable && lens.CostFieldLoaded && lens.GxLoaded {
		scoringDetail = "the lens cannot score: " + lens.Verdict
	} else if reachable && lens.CostFieldLoaded && lens.GxLoaded {
		scoring, scoringDetail = "supported", "C(x) + G(x) scoring available"
	} else if reachable && lens.CostFieldLoaded && !lens.GxLoaded {
		scoring, scoringDetail = "partial", "C(x) loaded; G(x) missing"
	}
	// Capacity. One score is one embedding request, and llama-server refuses
	// any input longer than its physical batch (ATLAS_UBATCH). When the lens
	// knows that capacity and it is below the per-turn generation ceiling,
	// the longest writes this proxy can produce come back unscored (typed,
	// never a neutral number), so raw scoring is partial. An unknown capacity
	// changes nothing; calibration and intervention are not affected.
	if scoring != "disabled" && lens.EmbedCapacityTokens > 0 {
		if ceiling := agentMaxTokens(); lens.EmbedCapacityTokens < ceiling {
			source := lens.EmbedCapacitySource
			if source == "" {
				source = "reported"
			}
			scoring = "partial"
			scoringDetail = fmt.Sprintf(
				"%s for inputs up to %d tokens (%s embed capacity); "+
					"ATLAS_MAX_TOKENS=%d allows longer writes, which are reported "+
					"unscored. Raise ATLAS_UBATCH (VRAM: ~ubatch x n_embd x 280 B) "+
					"or lower ATLAS_MAX_TOKENS",
				scoringDetail, lens.EmbedCapacityTokens, source, ceiling)
		}
	}

	// Calibration.
	calibration := "disabled"
	calDetail := "artifacts not loaded"
	if !canScore && reachable && lens.CostFieldLoaded {
		calDetail = "the lens cannot score: " + lens.Verdict
	} else if reachable && lens.CostFieldLoaded {
		if lens.CxCalibrated && lens.GxCalibrated {
			calibration, calDetail = "calibrated",
				"per-model normalization + thresholds loaded"
		} else {
			calibration, calDetail = "uncalibrated",
				"loaded without this model's calibration files"
		}
	}

	// Intervention behavior — neutral/disabled unless calibrated.
	intervention := "disabled"
	intDetail := "no scoring; no intervention"
	if calibration == "calibrated" {
		intervention, intDetail = "active",
			"threshold interventions enabled"
	} else if scoring != "disabled" {
		intervention, intDetail = "neutral",
			"raw telemetry only; no automatic intervention"
	}

	return []StatusDimension{
		{"model_runtime", modelRuntime, modelDetail},
		{"direct_agent", directAgent, directDetail},
		{"lens_identity", identity, identityDetail},
		{"lens_scoring", scoring, scoringDetail},
		{"lens_calibration", calibration, calDetail},
		{"lens_intervention", intervention, intDetail},
		{"asa", asa.Verdict, asa.Hint},
	}
}

type LensStatus struct {
	// "supported" | "no-artifacts" | "incomplete-artifacts" |
	// "uncalibrated" | "dim-mismatch" | "unreachable" |
	// "drifted" | "self-test-failed" | "model-server-unreachable"
	Verdict string `json:"verdict"`
	// CanScore says whether this lens can score, the question the request
	// path asks before it starts any work (lens_required.go). An
	// uncalibrated lens can; every other verdict but "supported" cannot.
	CanScore bool `json:"can_score"`
	// ModelServerReachable is llama-server's reachability as the lens sees
	// it; nil when the lens did not say (or could not be reached).
	ModelServerReachable *bool `json:"model_server_reachable,omitempty"`
	CostFieldLoaded      bool  `json:"cost_field_loaded"`
	CostFieldDim         int   `json:"cost_field_dim"`
	EmbedDim             int   `json:"embed_dim"`
	GxLoaded             bool  `json:"gx_loaded"`
	CxCalibrated         bool  `json:"cx_calibrated"`
	GxCalibrated         bool  `json:"gx_calibrated"`
	// The longest input one score can be computed from: llama-server's
	// physical batch (`-ub`, ATLAS_UBATCH) as the lens reports it, declared
	// by the deployment or observed from a refusal. 0 when unknown.
	EmbedCapacityTokens int    `json:"embed_capacity_tokens"`
	EmbedCapacitySource string `json:"embed_capacity_source"`
	Hint                string `json:"hint"`
}

type ASAStatus struct {
	// "supported" | "missing" | "unverified"
	Verdict       string `json:"verdict"`
	VectorPath    string `json:"vector_path"`
	VectorPresent bool   `json:"vector_present"`
	Hint          string `json:"hint"`
}

// lensHealthShape mirrors the lens /health JSON we read. Defensive — the
// service can be reachable but mid-startup with partial fields. We treat
// missing fields as zero values rather than failing the whole probe.
type lensHealthShape struct {
	Status     string `json:"status"`
	Subsystems struct {
		// Pointer: an older lens that omits the field is not read as down.
		LlamaServer struct {
			Reachable *bool  `json:"reachable"`
			Error     string `json:"error"`
		} `json:"llama_server"`
		Lens struct {
			CostFieldLoaded bool   `json:"cost_field_loaded"`
			CostFieldDim    int    `json:"cost_field_dim"`
			EmbedDim        int    `json:"embed_dim"`
			GxLoaded        bool   `json:"gx_loaded"`
			CxCalibrated    bool   `json:"cx_calibrated"`
			GxCalibrated    bool   `json:"gx_calibrated"`
			SelfTestPass    bool   `json:"self_test_pass"`
			SelfTestError   string `json:"self_test_error"`
			// Null while no fingerprint file exists, so only an explicit
			// false means drift.
			FingerprintOK    *bool  `json:"fingerprint_ok"`
			FingerprintError string `json:"fingerprint_error"`
			// Pointer: the lens reports null while the capacity is unknown.
			EmbedCapacityTokens *int   `json:"embed_capacity_tokens"`
			EmbedCapacitySource string `json:"embed_capacity_source"`
		} `json:"lens"`
	} `json:"subsystems"`
}

// probeLensStatus calls the lens /health endpoint and renders a verdict.
// Timeout is short — this fires on a TUI startup ping and on the proxy's
// own startup banner; we don't want to block either if the lens is wedged.
func probeLensStatus(ctx context.Context, lensBaseURL string) LensStatus {
	out := LensStatus{Verdict: "unreachable",
		Hint: "geometric-lens unreachable at " + lensBaseURL +
			" (is the stack up?)"}

	pCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pCtx, "GET", lensBaseURL+"/health", nil)
	if err != nil {
		return out
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return out
	}
	var h lensHealthShape
	if err := json.Unmarshal(body, &h); err != nil {
		out.Hint = "lens /health returned non-JSON: " + truncateStr(string(body), 80)
		return out
	}

	out.ModelServerReachable = h.Subsystems.LlamaServer.Reachable
	out.CostFieldLoaded = h.Subsystems.Lens.CostFieldLoaded
	out.CostFieldDim = h.Subsystems.Lens.CostFieldDim
	out.EmbedDim = h.Subsystems.Lens.EmbedDim
	out.GxLoaded = h.Subsystems.Lens.GxLoaded
	out.CxCalibrated = h.Subsystems.Lens.CxCalibrated
	out.GxCalibrated = h.Subsystems.Lens.GxCalibrated
	if h.Subsystems.Lens.EmbedCapacityTokens != nil && *h.Subsystems.Lens.EmbedCapacityTokens > 0 {
		out.EmbedCapacityTokens = *h.Subsystems.Lens.EmbedCapacityTokens
		out.EmbedCapacitySource = h.Subsystems.Lens.EmbedCapacitySource
	}

	lens := h.Subsystems.Lens
	switch {
	case !out.CostFieldLoaded:
		out.Verdict = "no-artifacts"
		if h.Subsystems.Lens.SelfTestError != "" {
			out.Hint = h.Subsystems.Lens.SelfTestError
		} else {
			out.Hint = "no cost_field.pt loaded — run `atlas lens build` to train one"
		}
	case out.EmbedDim > 0 && out.CostFieldDim != out.EmbedDim:
		out.Verdict = "dim-mismatch"
		out.Hint = fmt.Sprintf("cost_field expects %d-dim, model emits %d-dim "+
			"— run `atlas lens build` to retrain at the model's native dim",
			out.CostFieldDim, out.EmbedDim)
	case !out.GxLoaded:
		out.Verdict = "incomplete-artifacts"
		out.Hint = "C(x) loaded but G(x) artifacts are missing — run `atlas lens build`"
	case lens.FingerprintOK != nil && !*lens.FingerprintOK:
		out.Verdict = "drifted"
		out.Hint = "the lens has drifted from the served model: " + truncateStr(lens.FingerprintError, 160)
	case !lens.SelfTestPass:
		out.Verdict = "self-test-failed"
		out.Hint = "the lens self-test failed: " + truncateStr(lens.SelfTestError, 160) +
			" — see `docker logs atlas-geometric-lens-1` and `atlas doctor`"
	case out.ModelServerReachable != nil && !*out.ModelServerReachable:
		out.Verdict = "model-server-unreachable"
		out.Hint = "the lens cannot reach llama-server, so it cannot score"
	case !out.CxCalibrated || !out.GxCalibrated:
		out.Verdict = "uncalibrated"
		out.Hint = "Lens weights loaded without this model's calibration files — " +
			"run `atlas lens build` to generate cx_normalization.json and gx_thresholds.json"
	default:
		out.Verdict = "supported"
		out.Hint = "ready"
	}
	out.CanScore = lensVerdictCanScore(out.Verdict)
	return out
}

// lensVerdictCanScore is whether a lens with this verdict can score: the
// question the request path asks before it starts (lens_required.go). An
// uncalibrated lens can; it scores raw energies, and only the calibrated
// uses of them wait for calibration.
func lensVerdictCanScore(verdict string) bool {
	return verdict == "supported" || verdict == "uncalibrated"
}

// probeASAStatus checks for the configured ASA control-vector file on disk.
// The configured path is container-relative (e.g.
// /models/ast_edit_steering.gguf as llama-server sees it). The proxy
// container doesn't have /models mounted, so we try several candidate
// host-visible paths before giving up:
//
//  1. The configured path verbatim (works when proxy DOES have a /models
//     mount — some K3s deployments do).
//  2. <workspace>/models/<basename> (proxy's bind-mounted project root,
//     ATLAS_WORKSPACE_DIR, plus the standard models/ subdir).
//  3. The env-supplied ATLAS_MODELS_DIR if set.
//
// llama-server is the authoritative source of "is the vector actually
// loaded" but doesn't expose that via /props (verified 2026-05-17), so
// disk presence is the best we can do without an out-of-band probe.
// For the user-facing verdict, `atlas asa check` does the deeper GGUF
// dim parse on the host — this endpoint is the "first impression" the
// TUI badge renders.
func probeASAStatus() ASAStatus {
	configured := envOr("ATLAS_CONTROL_VECTOR", "/models/ast_edit_steering.gguf")
	out := ASAStatus{VectorPath: configured, Verdict: "unverified"}

	// Candidate paths to probe, in order.
	candidates := []vectorCandidate{{path: configured}}
	if strings.HasPrefix(configured, "/models/") {
		base := strings.TrimPrefix(configured, "/models/")
		workspace := envOr("ATLAS_WORKSPACE_DIR", "/workspace")
		candidates = append(candidates, vectorCandidate{
			path: workspace + "/models/" + base, root: workspace, rel: filepath.Join("models", base)})
		if mdir := os.Getenv("ATLAS_MODELS_DIR"); mdir != "" {
			candidates = append(candidates, vectorCandidate{path: mdir + "/" + base})
		}
	}

	for _, c := range candidates {
		if bytes, markedFor, found := lookAtVector(c); found {
			out.VectorPresent = true
			out.VectorPath = c.path
			expected := os.Getenv("ATLAS_MODEL_NAME")
			size := strconv.FormatInt(bytes, 10)
			switch {
			case expected != "" && sameModelIdentity(markedFor, expected):
				// Active, not "supported": the marker says which model the
				// vector is for, not that its effect was measured. That is
				// the registry's asa_status, which the proxy does not hold.
				out.Verdict = "active"
				out.Hint = "control vector active for " + expected +
					" (" + size + " bytes)"
			case expected != "" && markedFor != "":
				out.Verdict = "incompatible"
				out.Hint = "control vector is marked for " + markedFor +
					", but the selected model is " + expected
			default:
				out.Verdict = "unverified"
				out.Hint = "control vector present (" + size +
					" bytes) without a matching model marker; run `atlas asa build`"
			}
			return out
		}
	}

	out.VectorPresent = false
	out.Verdict = "missing"
	out.Hint = "no control vector at " + configured +
		" (also tried workspace/models/ + ATLAS_MODELS_DIR) — " +
		"build one via `atlas asa build` " +
		"or see geometric-lens/asa_calibration/README.md"
	return out
}

// vectorCandidate is one place the control vector can be. root and rel are
// set for the place inside the workspace: the workspace folder, and the
// path below it.
type vectorCandidate struct {
	path string
	root string
	rel  string
}

// lookAtVector returns the size of the vector at a candidate and the text of
// the marker file beside it. The place inside the workspace is read through
// the workspace folder, so a name there that resolves somewhere else counts
// as absent, like a file that is not there.
func lookAtVector(c vectorCandidate) (size int64, marker string, found bool) {
	if c.root == "" {
		info, err := os.Stat(c.path)
		if err != nil {
			return 0, "", false
		}
		raw, _ := os.ReadFile(c.path + ".model")
		return info.Size(), strings.TrimSpace(string(raw)), true
	}
	dir := openConfinedDir(c.root)
	defer dir.Close()
	info, err := dir.Stat(c.rel)
	if err != nil {
		return 0, "", false
	}
	raw, _ := dir.ReadFile(c.rel + ".model")
	return info.Size(), strings.TrimSpace(string(raw)), true
}

func sameModelIdentity(a, b string) bool {
	canonical := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.TrimSuffix(value, ".gguf")
		if slash := strings.LastIndex(value, "/"); slash >= 0 {
			value = value[slash+1:]
		}
		return value
	}
	return canonical(a) != "" && canonical(a) == canonical(b)
}

func handleCalibrationStatus(w http.ResponseWriter, r *http.Request) {
	lens := probeLensStatus(r.Context(), lensURL)
	asa := probeASAStatus()
	status := CalibrationStatus{
		Lens:       lens,
		ASA:        asa,
		Dimensions: buildDimensions(lens, asa),
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(status)
}

// logCalibrationStatusAtStartup is called once from main() so operators
// see the same compat verdict the TUI will render, in the proxy banner.
// Fail-soft: if the lens service isn't reachable yet, we log it and move
// on — startup blocks long enough as-is without a synchronous probe.
func logCalibrationStatusAtStartup() {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	lens := probeLensStatus(ctx, lensURL)
	asa := probeASAStatus()
	log.Printf("  Lens: %s — %s", lens.Verdict, lens.Hint)
	log.Printf("  ASA:  %s — %s", asa.Verdict, asa.Hint)
}
