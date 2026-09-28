package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Running a client's declared commands against a candidate, in a workspace
// that is not the user's.
//
// This builds no second executor. The sandbox already snapshots the workspace,
// overlays files, runs a command in the copy and deletes it in a `finally` --
// that is the mechanism V3 build verification has used all along, and a second
// one would be a second set of safety rules to keep in step. What this adds is
// the part that was missing: materialising the exact candidate at the exact
// target, asking the executor what the workspace looked like either side of
// each command, and refusing every result that cannot be shown to be about the
// bytes it claims.
//
// The production workspace is never written. Candidate bytes go into the
// overlay and nowhere else, which is what makes running a client's test safe
// before anything is delivered.

// stagingBackgroundRe recognises a command that would leave work running after
// it returns. A staged command must exit; one that forks into the background
// has not finished when we look, so whatever we observed is about a workspace
// something is still writing to.
var stagingBackgroundRe = regexp.MustCompile(`(^|[^&|>])&\s*$|\bnohup\b|\bdisown\b|\bsetsid\b|&\s*(;|$)`)

// stageCandidate runs the complete declared set against one candidate and
// reports what happened.
//
// The sequence is fixed and every step is checked:
//
//  1. the request is validated, including that the bytes are the candidate
//     the identity names;
//  2. the candidate is materialised at the canonical target inside the
//     isolated overlay, and nowhere else;
//  3. the staged target's hash is confirmed to equal the candidate hash
//     before anything runs;
//  4. each command goes through the existing safety check and the existing
//     sandbox path, with the existing cancellation and timeout controls;
//  5. the target and the workspace are re-hashed after every command;
//  6. anything that changed the candidate, changed an input, escaped, ran
//     long, backgrounded or could not be observed produces a refusal rather
//     than a pass;
//  7. the overlay is destroyed by the executor whatever happened.
//
// A set that cannot be run whole inside the budget is reported incomplete. It
// is never run in part and called finished.
func stageCandidate(ctx *AgentContext, req stagingRequest) (stagingResult, bool) {
	res := stagingResult{WireVersion: stagingWireVersion, Identity: req.Identity}
	if ctx == nil || strings.TrimSpace(ctx.SandboxURL) == "" {
		res.Commands = stagingAllUnavailable(req, stagingUnavailable)
		return res, true
	}
	if ok, _ := req.validate(); !ok {
		return stagingResult{}, false
	}
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		res.Commands = stagingAllUnavailable(req, stagingCancelled)
		return res, true
	}

	// The overlay path is workspace-relative: the executor refuses an absolute
	// path or one containing "..", and a candidate outside the workspace is
	// not a candidate for this request.
	overlayPath, ok := stagingOverlayPath(ctx, req.Identity.TargetPath)
	if !ok {
		res.Commands = stagingAllUnavailable(req, stagingRefused)
		return res, true
	}
	overlay := map[string]string{overlayPath: req.CandidateBytes}

	deadline := time.Now().Add(time.Duration(req.Budget.TotalTimeoutSec) * time.Second)
	stagedHash := ""
	for _, cmd := range req.Commands {
		out := stagingCommandResult{
			CommandIdentity: cmd.Identity, ObligationID: cmd.ObligationID,
			Index: cmd.Index, Count: cmd.Count,
		}

		switch {
		case ctx.Ctx != nil && ctx.Ctx.Err() != nil:
			out.Outcome = stagingCancelled
		case time.Now().After(deadline):
			out.Outcome = stagingBudgetExceeded
		case stagingBackgroundRe.MatchString(cmd.Text):
			// Refused before it runs: a command that has not exited when we
			// look cannot be observed, and killing it afterwards would not
			// make the observation true.
			out.Outcome = stagingBackgroundedRefusal()
		case validateShellCommand(cmd.Text) != "":
			// The existing safety check, unchanged. Client authority to ask
			// for verification is not authority to do something the safety
			// gate refuses the model.
			out.Outcome = stagingRefused
		default:
			obs, ran := stagingRun(ctx, cmd.Text, overlay, overlayPath,
				req.Budget.PerCommandTimeoutSec, deadline)
			if !ran {
				out.Outcome = stagingUnavailable
			} else {
				stagingApplyObservation(&out, obs, req.Identity.CandidateHash, &stagedHash)
			}
		}

		res.Commands = append(res.Commands, out)
		if out.Outcome == stagingCancelled || out.Outcome == stagingBudgetExceeded {
			// Everything after this is unrun, and an unrun command is not a
			// failed one. Report the set as incomplete and stop.
			break
		}
	}

	res.Complete = len(res.Commands) == len(req.Commands)
	for _, c := range res.Commands {
		if c.Outcome == stagingBudgetExceeded || c.Outcome == stagingCancelled ||
			c.Outcome == stagingUnavailable {
			res.Complete = false
		}
	}
	// The executor deletes its overlay in a finally, on every path including a
	// timeout and an exception. Recorded as a fact about the mechanism used,
	// not as a claim this function verified from outside the container.
	res.WorkspaceDestroyed = true
	return res, true
}

// stagingBackgroundedRefusal names the background case. Split out so the
// outcome vocabulary stays greppable from the one place that assigns it.
func stagingBackgroundedRefusal() stagingCommandOutcome { return stagingRefused }

// stagingAllUnavailable marks every command in the set with one outcome, for
// the cases where nothing could run at all.
func stagingAllUnavailable(req stagingRequest, outcome stagingCommandOutcome) []stagingCommandResult {
	out := make([]stagingCommandResult, 0, len(req.Commands))
	for _, c := range req.Commands {
		out = append(out, stagingCommandResult{
			CommandIdentity: c.Identity, ObligationID: c.ObligationID,
			Index: c.Index, Count: c.Count, Outcome: outcome,
		})
	}
	return out
}

// stagingOverlayPath is the target as the executor's overlay names it:
// workspace-relative, forward-slashed, never absolute and never escaping.
func stagingOverlayPath(ctx *AgentContext, target string) (string, bool) {
	resolved := resolveAgentPath(ctx, target)
	rel, err := filepath.Rel(ctx.WorkingDir, resolved)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// stagingObservation is the executor's answer about one run.
type stagingObservation struct {
	TargetBefore    map[string]string `json:"target_before"`
	TargetAfter     map[string]string `json:"target_after"`
	WorkspaceBefore string            `json:"workspace_before"`
	WorkspaceAfter  string            `json:"workspace_after"`
	DigestTruncated bool              `json:"digest_truncated"`
	ExitCode        int               `json:"exit_code"`
	Success         bool              `json:"success"`
	TimedOut        bool              `json:"timed_out"`
	// Outcome is how the executor says the command ended. Canonicalised at
	// the decode, so an executor that predates the vocabulary arrives as
	// unclassified and cannot be scored as a completion.
	Outcome ExecutionOutcome `json:"-"`
	// Cancelled is set by this side when the request context ended the call.
	Cancelled bool `json:"-"`
	Path      string
}

// stagingRun executes one command through the existing sandbox path.
//
// Nothing about the response beyond hashes, an exit code and the two flags is
// read. stdout and stderr are decoded because the endpoint returns them and
// are then dropped on the floor: they are never assigned, logged, recorded or
// returned, so no candidate byte and no test output can reach a caller.
func stagingRun(ctx *AgentContext, command string, overlay map[string]string,
	observePath string, perCommandSec int, deadline time.Time) (stagingObservation, bool) {
	// Rounded up, not truncated: a set with a whole second still to spend must
	// not be reported unrunnable because the fraction fell off.
	remaining := int(math.Ceil(time.Until(deadline).Seconds()))
	if remaining <= 0 {
		return stagingObservation{}, false
	}
	if perCommandSec < remaining {
		remaining = perCommandSec
	}
	body, err := json.Marshal(map[string]interface{}{
		"command":       command,
		"timeout":       remaining,
		"files":         overlay,
		"observe_paths": []string{observePath},
	})
	if err != nil {
		return stagingObservation{}, false
	}
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	httpReq, err := http.NewRequestWithContext(reqCtx, "POST",
		ctx.SandboxURL+"/shell", bytes.NewReader(body))
	if err != nil {
		return stagingObservation{}, false
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: time.Duration(remaining+30) * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		if reqCtx.Err() != nil {
			return stagingObservation{Cancelled: true, Path: observePath}, true
		}
		return stagingObservation{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// A refusal from the executor -- an unsafe overlay path, a bad cwd.
		// The detail is deliberately not read: it can quote the path it
		// refused, and that is content.
		return stagingObservation{Path: observePath, ExitCode: -1}, true
	}
	var sr struct {
		Success     bool   `json:"success"`
		ExitCode    int    `json:"exit_code"`
		TimedOut    bool   `json:"timed_out"`
		Outcome     string `json:"outcome"`
		Observation *struct {
			TargetBefore    map[string]string `json:"target_before"`
			TargetAfter     map[string]string `json:"target_after"`
			WorkspaceBefore string            `json:"workspace_before"`
			WorkspaceAfter  string            `json:"workspace_after"`
			DigestTruncated bool              `json:"digest_truncated"`
		} `json:"observation"`
	}
	if json.NewDecoder(resp.Body).Decode(&sr) != nil {
		return stagingObservation{}, false
	}
	obs := stagingObservation{
		Success: sr.Success, ExitCode: sr.ExitCode, TimedOut: sr.TimedOut,
		Outcome: canonicalExecutionOutcome(sr.Outcome),
		Path:    observePath,
	}
	if sr.Observation != nil {
		obs.TargetBefore = sr.Observation.TargetBefore
		obs.TargetAfter = sr.Observation.TargetAfter
		obs.WorkspaceBefore = sr.Observation.WorkspaceBefore
		obs.WorkspaceAfter = sr.Observation.WorkspaceAfter
		obs.DigestTruncated = sr.Observation.DigestTruncated
	}
	return obs, true
}

// stagingApplyObservation turns one executor answer into one command result.
//
// The order is the order in which an answer stops being usable: could we see
// anything, were we looking at the right bytes, did the command change them,
// did it change anything else, and only then did it succeed.
func stagingApplyObservation(out *stagingCommandResult, obs stagingObservation,
	candidateHash string, stagedHash *string) {
	if obs.Cancelled {
		out.Outcome = stagingCancelled
		return
	}
	before := obs.TargetBefore[obs.Path]
	after := obs.TargetAfter[obs.Path]
	out.TargetHashBefore = before
	out.TargetHashAfter = after
	out.WorkspaceHashBefore = obs.WorkspaceBefore
	out.WorkspaceHashAfter = obs.WorkspaceAfter
	out.ExitStatus = obs.ExitCode

	// An observation that could not describe the workspace exactly describes
	// nothing this can rely on. Truncation is not "unchanged".
	if obs.DigestTruncated || obs.WorkspaceBefore == "" || obs.WorkspaceAfter == "" {
		out.Outcome = stagingUnobservable
		return
	}
	// The staged bytes must be the candidate. If the overlay did not take, or
	// the target already held something else, nothing that follows is about
	// the candidate at all.
	if before != candidateHash {
		out.Outcome = stagingUnobservable
		return
	}
	// Every command in the set must see the same starting bytes.
	if *stagedHash == "" {
		*stagedHash = before
	} else if *stagedHash != before {
		out.Outcome = stagingUnobservable
		return
	}

	out.MutatedTarget = before != after
	out.MutatedWorkspace = obs.WorkspaceBefore != obs.WorkspaceAfter
	switch {
	case out.MutatedTarget:
		out.Outcome = stagingMutatedTarget
	case out.MutatedWorkspace:
		out.Outcome = stagingMutatedWorkspace
	case obs.TimedOut:
		out.Outcome = stagingTimedOut
	case executionStoppedByResource(obs.Outcome):
		// Ahead of the exit-code branches on purpose. A memory-killed pytest
		// exits non-zero, and reading that as exited_nonzero would record a
		// behavioural failure of the candidate over a run that never finished.
		out.Outcome = stagingResourceExhausted
	case !executionCompleted(obs.Outcome):
		// Cancelled, spawn-failed, or an executor this build cannot read. None
		// of them ran to an end, and none may be scored as one.
		out.Outcome = stagingUnobservable
	case obs.ExitCode == 0 && obs.Success:
		out.Outcome = stagingExitedZero
	default:
		out.Outcome = stagingExitedNonZero
	}
}
