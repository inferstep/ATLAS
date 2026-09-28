package main

import (
	"sort"
	"strings"
)

// The typed transport for running a client's declared verification commands
// against a candidate, and the closed vocabulary of what came back.
//
// The trust boundary is the point of this file. The proxy is the only thing
// that knows whether a command came from a validated TaskContract, so the
// proxy alone decides what may be staged and what the result means. The
// executor runs what it is handed and reports what it observed; it draws no
// conclusion, declares nothing trusted, and has no way to know what the client
// asked for. A caller that reached the executor directly could run a command
// and would still not be able to produce client-declared authority, because
// authority is granted here, against obligations derived from a request this
// file can see and that caller cannot forge.
//
// Nothing model-authored reaches this. Commands come from the validated
// contract's own list; the model cannot add one, alter one, remove one, or
// relabel its own `run_command` as a declared verification, because the only
// input is the obligation set and the only match is byte equality with a
// subject the request boundary already canonicalised.
//
// This file is transport and validation. It executes nothing.

// stagingWireVersion is the contract this build speaks. An unknown version is
// refused rather than best-effort decoded: a result whose shape is not the one
// these checks were written against is a result nobody has checked.
const stagingWireVersion = "1"

// stagingCommandOutcome is how one staged command ended, in a closed set.
//
// Every way a command can fail to be usable evidence gets its own name. A
// single "not ok" would make a refusal indistinguishable from a timeout and a
// mutation indistinguishable from a nonzero exit, and each of those needs a
// different answer from the thing reading it.
type stagingCommandOutcome string

const (
	// stagingExitedZero: ran to completion in the foreground and exited 0.
	stagingExitedZero stagingCommandOutcome = "exited_zero"
	// stagingExitedNonZero: ran to completion and failed.
	stagingExitedNonZero stagingCommandOutcome = "exited_nonzero"
	// stagingTimedOut: killed at the budget's per-command ceiling.
	stagingTimedOut stagingCommandOutcome = "timed_out"
	// stagingResourceExhausted: stopped at a memory, process or output
	// ceiling. NOT a failure of the candidate: the command never reached its
	// own conclusion, so it demonstrates nothing about the bytes it was
	// pointed at. It exits non-zero exactly like a failing test, which is why
	// it needs its own name rather than sharing exited_nonzero.
	stagingResourceExhausted stagingCommandOutcome = "resource_exhausted"
	// stagingCancelled: the request was cancelled while it ran.
	stagingCancelled stagingCommandOutcome = "cancelled"
	// stagingRefused: a safety check or the executor declined to run it.
	stagingRefused stagingCommandOutcome = "refused"
	// stagingMutatedTarget: the command changed the candidate it was meant to
	// be testing. Whatever it proves, it does not prove it about the bytes
	// that would be delivered.
	stagingMutatedTarget stagingCommandOutcome = "mutated_target"
	// stagingMutatedWorkspace: it changed inputs outside what a test may
	// touch, so the next command in the set would run against a workspace the
	// binding no longer describes.
	stagingMutatedWorkspace stagingCommandOutcome = "mutated_workspace"
	// stagingUnobservable: the executor could not describe the state either
	// side of the run. Not a pass and not a failure: no observation was made.
	stagingUnobservable stagingCommandOutcome = "unobservable"
	// stagingBudgetExceeded: the declared set could not be run inside the
	// staging budget. The set is incomplete, never partially complete.
	stagingBudgetExceeded stagingCommandOutcome = "budget_exceeded"
	// stagingUnavailable: no staging path could be reached at all.
	stagingUnavailable stagingCommandOutcome = "unavailable"
)

var stagingCommandOutcomes = map[stagingCommandOutcome]bool{
	stagingExitedZero: true, stagingExitedNonZero: true, stagingTimedOut: true,
	stagingCancelled: true, stagingRefused: true, stagingMutatedTarget: true,
	stagingMutatedWorkspace: true, stagingUnobservable: true,
	stagingBudgetExceeded: true, stagingUnavailable: true,
	stagingResourceExhausted: true,
}

// stagingIdentity is everything a staging run and its result are about.
//
// Carried on the request and echoed on the result, and compared field by field
// before anything is believed. One candidate may not borrow another's run, one
// invocation may not borrow another's, and a result about a workspace two
// mutations ago is about a workspace that no longer exists.
type stagingIdentity struct {
	RequestID string `json:"request_id"`
	// RouteEntryID is the entry of the candidate-generation route this
	// staging run belongs to. Carried so evidence produced from it stays
	// attributable to the attempt rather than only to the candidate.
	RouteEntryID        string `json:"-"`
	InvocationID        string `json:"invocation_id"`
	CandidateInstanceID string `json:"candidate_instance_id"`
	CandidateHash       string `json:"candidate_hash"`
	// TargetPath is the canonical artifact the candidate would replace. It is
	// a path and never contents.
	TargetPath string `json:"target_path"`
	// BaselineIdentity names the validated baseline being replaced, or "".
	BaselineIdentity string `json:"baseline_identity,omitempty"`
	// The evaluation workspace this run is bound to.
	WorkspaceGeneration int    `json:"workspace_generation"`
	WorkspaceStateHash  string `json:"workspace_state_hash"`
}

// matches reports whether two identities describe the same thing.
func (s stagingIdentity) matches(other stagingIdentity) (bool, string) {
	for _, f := range []struct{ name, a, b string }{
		{"request_id", s.RequestID, other.RequestID},
		{"invocation_id", s.InvocationID, other.InvocationID},
		{"candidate_instance_id", s.CandidateInstanceID, other.CandidateInstanceID},
		{"candidate_hash", s.CandidateHash, other.CandidateHash},
		{"target_path", s.TargetPath, other.TargetPath},
		{"baseline_identity", s.BaselineIdentity, other.BaselineIdentity},
		{"workspace_state_hash", s.WorkspaceStateHash, other.WorkspaceStateHash},
	} {
		if f.a != f.b {
			return false, f.name + " differs"
		}
	}
	if s.WorkspaceGeneration != other.WorkspaceGeneration {
		return false, "workspace_generation differs"
	}
	return true, ""
}

func (s stagingIdentity) complete() (bool, string) {
	for _, f := range []struct{ name, value string }{
		{"request_id", s.RequestID}, {"invocation_id", s.InvocationID},
		{"candidate_instance_id", s.CandidateInstanceID},
		{"candidate_hash", s.CandidateHash}, {"target_path", s.TargetPath},
		{"workspace_state_hash", s.WorkspaceStateHash},
	} {
		if strings.TrimSpace(f.value) == "" {
			return false, f.name + " is required"
		}
	}
	if s.WorkspaceGeneration < 0 {
		return false, "workspace_generation is negative"
	}
	return true, ""
}

// stagingCommand is one declared command, with its position in the set.
//
// Text and identity travel together and are used for different things: the
// text is handed to the executor and never recorded, the identity is recorded
// and never executed. Index and count make the set explicit, so a result
// describing three of four commands is visibly incomplete rather than looking
// like a complete run of three.
type stagingCommand struct {
	// Text is the exact command the client declared. It never enters a log,
	// a record or telemetry.
	Text string `json:"-"`
	// Identity is the hash of Text, and is what gets recorded.
	Identity string `json:"command_identity"`
	// ObligationID is the declared-command obligation this command discharges.
	ObligationID string `json:"obligation_id"`
	Index        int    `json:"command_index"`
	Count        int    `json:"command_count"`
}

// stagingRequest is one candidate and the complete set of commands its task
// declared. Nothing about the TaskContract beyond this reaches the executor:
// not the mode, not the outputs, not the prose, not the authority itself.
type stagingRequest struct {
	WireVersion string          `json:"wire_version"`
	Identity    stagingIdentity `json:"identity"`
	// CandidateBytes are materialised into the isolated workspace and never
	// written to the production one.
	CandidateBytes string `json:"-"`
	Commands       []stagingCommand
	Budget         stagingBudget
}

// validate refuses a request that cannot produce a checkable result.
//
// Fails closed on an unknown version, an incomplete identity, an empty or
// over-budget set, a duplicate command identity, a command whose index or
// count contradicts the set, or a command with no text to run.
func (r stagingRequest) validate() (bool, string) {
	if r.WireVersion != stagingWireVersion {
		return false, "unknown staging wire version " + r.WireVersion
	}
	if ok, why := r.Identity.complete(); !ok {
		return false, why
	}
	if contentSHA256(r.CandidateBytes) != r.Identity.CandidateHash {
		return false, "candidate bytes are not the candidate the identity names"
	}
	if len(r.Commands) == 0 {
		return false, "no declared commands to stage"
	}
	if ok, why := r.Budget.validate(); !ok {
		return false, why
	}
	if len(r.Commands) > r.Budget.MaxCommands {
		return false, "declared command set exceeds the staging budget"
	}
	seen := map[string]bool{}
	for i, c := range r.Commands {
		if strings.TrimSpace(c.Text) == "" {
			return false, "a declared command has no text"
		}
		if c.Identity != contentSHA256(c.Text) {
			return false, "command identity does not name its command"
		}
		if strings.TrimSpace(c.ObligationID) == "" {
			return false, "a declared command names no obligation"
		}
		if seen[c.Identity] {
			return false, "duplicate command identity in one set"
		}
		seen[c.Identity] = true
		if c.Index != i || c.Count != len(r.Commands) {
			return false, "command index or count contradicts the set"
		}
	}
	return true, ""
}

// stagingCommandResult is what the executor observed about one command.
//
// Identities, hashes, an outcome and an exit status. No command text, no
// candidate bytes, no stdout, no stderr, no diagnostic prose -- an executor
// that returned any of those would be handing the caller content to log.
type stagingCommandResult struct {
	CommandIdentity string                `json:"command_identity"`
	ObligationID    string                `json:"obligation_id"`
	Index           int                   `json:"command_index"`
	Count           int                   `json:"command_count"`
	Outcome         stagingCommandOutcome `json:"outcome"`
	// ExitStatus is meaningful only for exited_zero and exited_nonzero.
	ExitStatus int `json:"exit_status"`

	// What the workspace looked like either side of this command.
	TargetHashBefore    string `json:"target_hash_before"`
	TargetHashAfter     string `json:"target_hash_after"`
	WorkspaceHashBefore string `json:"workspace_hash_before"`
	WorkspaceHashAfter  string `json:"workspace_hash_after"`
	// MutatedTarget and MutatedWorkspace are derived from those hashes by the
	// caller, never asserted by the executor.
	MutatedTarget    bool `json:"mutated_target"`
	MutatedWorkspace bool `json:"mutated_workspace"`
}

// stagingResult is the complete answer for one candidate.
type stagingResult struct {
	WireVersion string                 `json:"wire_version"`
	Identity    stagingIdentity        `json:"identity"`
	Commands    []stagingCommandResult `json:"commands"`
	// Complete is true only when every declared command in the set ran and was
	// observed. A partial set is never complete, whatever its members say.
	Complete bool `json:"complete"`
	// WorkspaceDestroyed records that the isolated workspace was torn down.
	WorkspaceDestroyed bool `json:"workspace_destroyed"`
}

// validateAgainst refuses a result that does not answer the request it claims
// to answer.
//
// Every failure here is a fail-closed: an unknown version, an identity that
// does not match, a duplicate, an unknown outcome, a count that contradicts
// the set, or a command the request never asked for.
func (res stagingResult) validateAgainst(req stagingRequest) (bool, string) {
	if res.WireVersion != stagingWireVersion {
		return false, "unknown staging wire version " + res.WireVersion
	}
	if ok, why := res.Identity.matches(req.Identity); !ok {
		return false, "result identity mismatch: " + why
	}
	asked := map[string]stagingCommand{}
	for _, c := range req.Commands {
		asked[c.Identity] = c
	}
	seen := map[string]bool{}
	for _, r := range res.Commands {
		want, ok := asked[r.CommandIdentity]
		if !ok {
			return false, "result names a command the request never asked for"
		}
		if seen[r.CommandIdentity] {
			return false, "duplicate command identity in one result"
		}
		seen[r.CommandIdentity] = true
		if !stagingCommandOutcomes[r.Outcome] {
			return false, "unknown staging outcome " + string(r.Outcome)
		}
		if r.ObligationID != want.ObligationID {
			return false, "result command names a different obligation"
		}
		if r.Index != want.Index || r.Count != want.Count {
			return false, "result command index or count contradicts the request"
		}
		// The executor reports hashes; the derived flags must agree with them.
		if r.MutatedTarget != (r.TargetHashBefore != r.TargetHashAfter) {
			return false, "mutated_target contradicts the observed hashes"
		}
		if r.MutatedWorkspace != (r.WorkspaceHashBefore != r.WorkspaceHashAfter) {
			return false, "mutated_workspace contradicts the observed hashes"
		}
	}
	if res.Complete && len(seen) != len(req.Commands) {
		return false, "a result covering part of the set claims to be complete"
	}
	return true, ""
}

// authorizingOutcomes returns the obligation ids whose command ran cleanly,
// sorted. Only exited_zero, and only when the command changed nothing.
func (res stagingResult) authorizingOutcomes() []string {
	var out []string
	for _, r := range res.Commands {
		if r.Outcome != stagingExitedZero || r.MutatedTarget || r.MutatedWorkspace {
			continue
		}
		out = append(out, r.ObligationID)
	}
	sort.Strings(out)
	return out
}

// --- budget -------------------------------------------------------------------

// stagingBudget is what staging is allowed to spend, stated separately from
// the TaskContract's maximum.
//
// maxTaskContractEntries is an input-validation ceiling -- it says what a
// request may legally declare, not what this machine will run. Executing 64
// commands per candidate because the wire permits 64 is how a validation limit
// becomes an execution policy, so the two are kept apart and this one is
// deliberately small.
type stagingBudget struct {
	// MaxCommands is how many declared commands one candidate may run.
	MaxCommands int
	// PerCommandTimeoutSec bounds a single command.
	PerCommandTimeoutSec int
	// TotalTimeoutSec bounds the whole set.
	TotalTimeoutSec int
	// MaxCandidates is how many candidates one invocation may stage.
	MaxCandidates int
}

// defaultStagingBudget is the shipped policy. Small on purpose: a task whose
// complete declared set does not fit has no closure path, and finding that out
// cheaply is better than half-running it.
func defaultStagingBudget() stagingBudget {
	return stagingBudget{
		MaxCommands:          4,
		PerCommandTimeoutSec: 60,
		TotalTimeoutSec:      180,
		MaxCandidates:        3,
	}
}

func (b stagingBudget) validate() (bool, string) {
	if b.MaxCommands <= 0 || b.MaxCommands > maxTaskContractEntries {
		return false, "staging command budget is out of range"
	}
	if b.PerCommandTimeoutSec <= 0 || b.TotalTimeoutSec <= 0 {
		return false, "staging time budget is out of range"
	}
	if b.PerCommandTimeoutSec > b.TotalTimeoutSec {
		return false, "a single command may not outlast the whole set"
	}
	if b.MaxCandidates <= 0 {
		return false, "staging candidate budget is out of range"
	}
	return true, ""
}
