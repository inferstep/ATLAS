package main

import (
	"strings"
)

// What the model actually asked to change, read off its own tool call.
//
// The interactive case is the hard one. A person types "make this faster" and
// nobody declares an output path or a command; a contract-shaped authority has
// nothing to bind to, and the honest answer -- retain the baseline -- makes the
// candidate pipeline useless for the traffic it exists to serve. Inferring a
// contract from the prose is the other failure, and it is worse: a
// seventy-character window before a filename is how an INPUT file once became a
// deliverable.
//
// There is a third thing on the table, and it is structured rather than
// inferred. The model made a tool call. That call names a canonical target and
// bounds a mutation, and it does so in fields rather than in sentences. What a
// candidate may touch is knowable from it exactly.
//
// What this is NOT is evidence. A structured intent says WHERE a candidate is
// allowed to act. It says nothing about whether the candidate is any good, it
// cannot expand a path, change a target, authorize a deletion or weaken a
// permission, and it mints nothing. Its only power is to narrow: a candidate
// outside the scope its own tool call defined is refused, and a candidate
// inside it is exactly as unproven as it was before.

// The tools whose calls bound a mutation. Every one names a target and either
// creates it or edits it in place; a tool absent from this set defines no scope
// and therefore admits no candidate.
var mutationScopeTools = map[string]bool{
	"write_file":      true,
	"edit_file":       true,
	"insert_after":    true,
	"replace_lines":   true,
	"structural_edit": true,
}

// How a call changes its target. The kind comes from the TOOL, not from
// whether the target happens to exist.
//
// That distinction was wrong once and it cost real candidates. A second
// write_file to a target the same session already created was classified as an
// in-place edit because the file was now there, and the edit-boundary rule --
// "a line the edit kept and the candidate dropped is out of scope" -- was
// applied to a call whose whole purpose is to replace the file. Measured on the
// eligibility pilot: six routes across four families refused for leaving a
// boundary their tool never had, while a seventh passed only because its
// candidate happened to append rather than rewrite.
const (
	// mutationScopeNewFile: write_file creating an artifact that was not there.
	mutationScopeNewFile = "new_file"
	// mutationScopeWholeFile: write_file replacing an artifact that was. The
	// authority is the whole target either way, so there is no line boundary
	// to leave -- only the target, the containment and the identity checks,
	// which all still apply.
	mutationScopeWholeFile = "whole_file"
	// mutationScopeInPlaceEdit: one of the four edit tools changing part of an
	// artifact, where the part it changes IS the boundary.
	mutationScopeInPlaceEdit = "in_place_edit"
)

// wholeFileTools replace their target outright. Everything else in
// mutationScopeTools edits in place.
var wholeFileTools = map[string]bool{"write_file": true}

// mutationScope is one tool call's structured intent.
//
// Bytes live here only long enough to answer the boundary question. They never
// reach a log, a record or the wire; everything that travels is a hash.
type mutationScope struct {
	Tool string
	Kind string
	// Target is canonical: resolved, cleaned, absolute, inside the workspace.
	Target string

	RequestID    string
	RouteEntryID string

	// Original is the artifact as it stood before this call, and Proposal is
	// the caller's own result for it. Their difference IS the boundary.
	Original string
	Proposal string

	OriginalHash string
	ProposalHash string

	WorkspaceGeneration int
	WorkspaceStateHash  string
	// TargetGeneration is the ledger's count for this exact path, which is
	// what notices a move, a recreation or a tombstone at the target while a
	// workspace digest is still comparing everything at once.
	TargetGeneration int
}

// valid reports whether this scope was fully derived. A partial scope is not a
// weaker scope: it is no scope, and it admits nothing.
func (s mutationScope) valid() bool {
	if !mutationScopeTools[s.Tool] {
		return false
	}
	switch s.Kind {
	case mutationScopeNewFile, mutationScopeWholeFile, mutationScopeInPlaceEdit:
	default:
		return false
	}
	for _, v := range []string{s.Target, s.RequestID, s.RouteEntryID, s.ProposalHash,
		s.WorkspaceStateHash} {
		if strings.TrimSpace(v) == "" {
			return false
		}
	}
	return s.WorkspaceGeneration >= 0 && s.TargetGeneration >= 0
}

// identity is the scope's content-free name, carried on records and bound into
// a grant. Two calls to the same target in one request differ by route entry,
// and a call whose boundary moved differs by proposal hash.
func (s mutationScope) identity() string {
	if !s.valid() {
		return ""
	}
	return contentSHA256(strings.Join([]string{
		s.Tool, s.Kind, s.Target, s.RequestID, s.RouteEntryID,
		s.OriginalHash, s.ProposalHash, s.WorkspaceStateHash,
	}, "\x00"))
}

// deriveMutationScope reads the scope off the call that is happening.
//
// Fails closed on everything: an unknown tool, a path that does not resolve
// inside the workspace, a spelling the two resolvers disagree about, a call
// with no identity, a deletion, and a request that is already over. Each is a
// state where "what may this candidate touch" has no answer, and no answer is
// not permission.
func deriveMutationScope(ctx *AgentContext, entry routeEntry, tool, path,
	original, proposal string) (mutationScope, bool) {
	if ctx == nil || !mutationScopeTools[tool] || !entry.valid() {
		return mutationScope{}, false
	}
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		return mutationScope{}, false
	}
	requestID := requestIDOf(ctx)
	if strings.TrimSpace(requestID) == "" {
		return mutationScope{}, false
	}
	// The one path resolver the rest of the proxy uses, so containment and
	// canonical identity are decided in one place. An alias resolves to the
	// canonical target and IS that target; anything resolving outside the
	// workspace is not a target at all.
	target, err := resolveWorkspacePath(ctx, path)
	if err != nil || strings.TrimSpace(target) == "" {
		return mutationScope{}, false
	}
	if target != resolveAgentPath(ctx, path) {
		// Two resolvers disagreeing about one spelling is exactly the state a
		// scope must not paper over.
		return mutationScope{}, false
	}
	// A call that leaves nothing behind is a deletion, and deletion has its own
	// explicit permission flow. A scope never authorizes one.
	if strings.TrimSpace(proposal) == "" {
		return mutationScope{}, false
	}
	kind := mutationScopeInPlaceEdit
	originalHash := contentSHA256(original)
	switch {
	case strings.TrimSpace(original) == "":
		// Nothing was there. A new file has no prior bytes, so its original
		// hash names nothing: recording the hash of the empty string would
		// make "absent" and "empty" the same fact.
		kind, originalHash = mutationScopeNewFile, ""
	case wholeFileTools[tool]:
		// The target exists and the call replaces it whole. Still bounded by
		// the target, the workspace and the identity checks; not by a line
		// boundary the tool does not have.
		kind = mutationScopeWholeFile
	}
	generation, stateHash := workspaceIdentity(ctx)
	s := mutationScope{
		Tool: tool, Kind: kind, Target: target,
		RequestID: requestID, RouteEntryID: entry.ID,
		Original: original, Proposal: proposal,
		OriginalHash: originalHash, ProposalHash: contentSHA256(proposal),
		WorkspaceGeneration: generation, WorkspaceStateHash: stateHash,
		TargetGeneration: targetGeneration(ctx, target),
	}
	if !s.valid() {
		return mutationScope{}, false
	}
	return s, true
}

// Why a candidate is outside the scope its own call defined. Closed, and every
// value is a fact rather than a judgement.
const (
	scopeRefusedNoScope        = "no_structured_scope"
	scopeRefusedEmptyCandidate = "candidate_is_a_deletion"
	scopeRefusedTargetMoved    = "target_moved_or_recreated"
	scopeRefusedWorkspaceMoved = "workspace_moved"
	scopeRefusedBoundary       = "candidate_left_the_mutation_boundary"
	scopeRefusedCancelled      = "request_ended"
)

// scopeAdmitsCandidate reports whether these bytes are inside the boundary this
// call defined, and names the reason when they are not.
//
// Everything is re-read from the live state rather than taken from the scope:
// the scope froze a moment and this is a later one. A scope that agreed with
// itself would notice nothing.
func scopeAdmitsCandidate(ctx *AgentContext, s mutationScope, candidate string) (bool, string) {
	if ctx == nil || !s.valid() {
		return false, scopeRefusedNoScope
	}
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		return false, scopeRefusedCancelled
	}
	if strings.TrimSpace(candidate) == "" {
		return false, scopeRefusedEmptyCandidate
	}
	if requestIDOf(ctx) != s.RequestID {
		return false, scopeRefusedNoScope
	}
	if targetGeneration(ctx, s.Target) != s.TargetGeneration || targetTombstoned(ctx, s.Target) {
		return false, scopeRefusedTargetMoved
	}
	generation, stateHash := workspaceIdentity(ctx)
	if generation != s.WorkspaceGeneration || stateHash != s.WorkspaceStateHash {
		return false, scopeRefusedWorkspaceMoved
	}
	if s.Kind == mutationScopeInPlaceEdit {
		// The same boundary rule the edit routes already apply to a winner: a
		// line the edit kept and the candidate dropped is out of scope.
		if why := v3RewroteBeyondTheEdit(s.Original, s.Proposal, candidate); why != "" {
			return false, scopeRefusedBoundary
		}
	}
	return true, ""
}

// recordMutationScope writes one structured intent to the private shadow sink.
//
// Hashes and closed values. No candidate byte, no path content, no prompt, no
// command. The target travels as a hash like every other subject.
func recordMutationScope(ctx *AgentContext, s mutationScope, admitted bool, reason string) {
	sink := activeShadowSink.Load()
	if !sink.enabled() {
		return
	}
	sink.submit(map[string]interface{}{
		"schema_version":       shadowSchemaVersionMutationScope,
		"record_kind":          "candidate_mutation_scope",
		"request_id":           s.RequestID,
		"route_entry_id":       s.RouteEntryID,
		"scope_id":             s.identity(),
		"tool":                 s.Tool,
		"scope_kind":           s.Kind,
		"target_identity":      contentSHA256(s.Target),
		"original_identity":    s.OriginalHash,
		"proposal_identity":    s.ProposalHash,
		"workspace_generation": s.WorkspaceGeneration,
		"workspace_state_hash": s.WorkspaceStateHash,
		"target_generation":    s.TargetGeneration,
		"admitted":             admitted,
		"refusal":              reason,
		// A scope narrows and never grants. It is consulted before a licence
		// may be minted and can only prevent one.
		"influences_live_decision": true,
		"authorizes":               false,
		"build_version":            APIVersion,
	})
}
