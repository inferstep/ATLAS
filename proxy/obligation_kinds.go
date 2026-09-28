package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"
)

// What a task obliges, as a closed typed vocabulary.
//
// obligations.go decided WHOSE list a run owes -- the caller's or the
// heuristic's. It says nothing about what sort of thing each item is or how
// strongly it must be shown, so a path and a command arrived as the same
// untyped string and any evidence about either looked equally good.
//
// This file names the sorts and their floors. Three things stay separate:
//
//	KIND      what sort of thing is owed (a closed set)
//	STRENGTH  how strong the evidence closing it must be
//	SUBJECT   the exact thing it is owed about, carried as a hash
//
// The subject is a hash and never the text. A declared verification command
// is a subject, and a command string in an operator log is a content leak; a
// uniform rule that never carries text cannot leak one by exception.
//
// This is the only copy of the vocabulary. The V3 service never receives
// task obligations -- V3GenerateRequest has no field for them -- so nothing
// there names a kind.

const (
	// ObligationArtifactExists: the artifact the client named must exist at
	// its canonical path.
	ObligationArtifactExists = "artifact_exists"
	// ObligationSyntacticValidity: its bytes must satisfy the structural
	// check the proxy already owns for that artifact class -- the same gate
	// live completion policy requires, not a second extension table.
	ObligationSyntacticValidity = "syntactic_validity"
	// ObligationDeclaredCommand: one exact command the client required, run
	// as the client wrote it.
	ObligationDeclaredCommand = "declared_command"
	// ObligationDeclaredExample: one example or oracle case the client
	// stated, with its own expected answer.
	ObligationDeclaredExample = "declared_example"
	// ObligationBaselinePreserved: an artifact that already carries a current
	// passing verdict must not be replaced by something demonstrated less
	// well.
	ObligationBaselinePreserved = "baseline_preserved"
	// ObligationUnsupported: something is owed and nothing here can name
	// what. Never satisfiable, never vacuously complete.
	ObligationUnsupported = "unsupported"
)

// obligationKinds is the closed set, in declaration order.
var obligationKinds = []string{
	ObligationArtifactExists,
	ObligationSyntacticValidity,
	ObligationDeclaredCommand,
	ObligationDeclaredExample,
	ObligationBaselinePreserved,
	ObligationUnsupported,
}

// obligationKindRequiredStrength is how strong the evidence closing each kind
// must be.
//
// ObligationDeclaredCommand is absent because its floor is not a constant
// either: it is the kind the client typed for that exact command, and runtime
// for a command declared without a type. No declared command reaches oracle --
// exit zero says the command ran and succeeded against these bytes, never that
// an answer was compared with a reference.
//
// ObligationBaselinePreserved is absent because its floor is not a constant:
// it is whatever the evidence currently describing that baseline already
// reached. ObligationUnsupported is absent because no strength closes it.
var obligationKindRequiredStrength = map[string]string{
	ObligationArtifactExists:    "syntax",
	ObligationSyntacticValidity: "syntax",
	ObligationDeclaredExample:   "oracle",
}

// --- when an obligation can be answered ------------------------------------
//
// The first structured task could never close, and the reason was circular.
// artifact_exists was a required obligation with a syntax floor; nothing can
// evidence a file's existence before the candidate lands; delivery needs
// authorization; authorization needed the obligation met. The task was
// unsatisfiable by construction, and the loop was invisible because every
// piece of it looked reasonable on its own.
//
// The fix is a typed distinction rather than a special case at the one site
// that noticed. Three roles, and every kind has exactly one:
//
//	target_identity              names WHICH artifact a delivery may replace.
//	                             It is the client saying "this path is mine to
//	                             hand you". It is never evidence about bytes.
//	authorization_prerequisite   must be satisfied, by evidence bound to the
//	                             exact candidate, BEFORE those bytes may land.
//	post_delivery_settlement     can only be answered after the bytes are on
//	                             disk and the ledger agrees they are there.
//
// artifact_exists carries the first and the third and neither of the second:
// a declared path authorizes a target and settles afterwards, and at no point
// does "the client asked for this path" say the bytes are any good.
const (
	ObligationRoleTargetIdentity            = "target_identity"
	ObligationRoleAuthorizationPrerequisite = "authorization_prerequisite"
	ObligationRolePostDeliverySettlement    = "post_delivery_settlement"
)

// obligationKindRole is total over obligationKinds. A kind with no role is a
// kind nothing knows when to ask about, so the lookup fails closed.
var obligationKindRole = map[string]string{
	ObligationArtifactExists:    ObligationRolePostDeliverySettlement,
	ObligationSyntacticValidity: ObligationRoleAuthorizationPrerequisite,
	ObligationDeclaredCommand:   ObligationRoleAuthorizationPrerequisite,
	ObligationDeclaredExample:   ObligationRoleAuthorizationPrerequisite,
	ObligationBaselinePreserved: ObligationRoleAuthorizationPrerequisite,
	// Something is owed that nothing here can name. It is a prerequisite so
	// it blocks authorization; it is unsatisfiable so it blocks it forever.
	ObligationUnsupported: ObligationRoleAuthorizationPrerequisite,
}

// obligationKindNamesTarget is the separate question: does this kind identify
// an artifact a delivery may replace? Only the declared output does, and it
// does so WITHOUT thereby saying anything about candidate quality.
var obligationKindNamesTarget = map[string]bool{
	ObligationArtifactExists: true,
}

func obligationRole(kind string) (string, bool) {
	role, ok := obligationKindRole[kind]
	return role, ok
}

// obligationDynamicStrengthKinds take their floor from the obligation rather
// than from the kind.
//
// A declared command is here because its floor is what the CLIENT typed about
// it. The kind says "a command the client required ran"; how strongly that
// counts is a statement only the client can make, and a build that read it off
// the kind would give every caller the strongest floor for saying nothing.
var obligationDynamicStrengthKinds = map[string]bool{
	ObligationBaselinePreserved: true,
	ObligationDeclaredCommand:   true,
}

// obligationUnsatisfiableKinds can be closed by nothing.
var obligationUnsatisfiableKinds = map[string]bool{
	ObligationUnsupported: true,
}

func knownObligationKind(kind string) bool {
	for _, k := range obligationKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// obligationID is the canonical name of one obligation.
//
// Deterministic and content-free: the subject is hashed, so the id can be
// logged, compared and carried on the wire without ever holding a path's
// contents or a command's text.
func obligationID(kind, subject string) (string, bool) {
	if !knownObligationKind(kind) || strings.TrimSpace(subject) == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(subject))
	return kind + ":" + hex.EncodeToString(sum[:])[:32], true
}

// obligationRequiredStrength is the floor evidence must reach to close this
// kind. A baseline obligation is at least as strong as the evidence already
// describing that baseline: replacing a file whose behaviour was demonstrated
// on the strength of a compile is a regression dressed as a delivery.
func obligationRequiredStrength(kind, baselineStrength string) (string, bool) {
	if !knownObligationKind(kind) || obligationUnsatisfiableKinds[kind] {
		return "", false
	}
	if obligationDynamicStrengthKinds[kind] {
		if strengthRank(baselineStrength) < 0 {
			return "", false
		}
		// A declared command carries what the client typed about it, and the
		// vocabulary a client may type stops below oracle: exit zero says the
		// command ran and succeeded, never that an answer was compared with a
		// reference. A caller asking for oracle here is asking for an
		// authority no command can produce.
		if kind == ObligationDeclaredCommand && !verificationKinds[baselineStrength] {
			return "", false
		}
		return baselineStrength, true
	}
	if baselineStrength != "" {
		// A fixed-strength kind cannot be raised by a baseline; silently
		// ignoring the argument is how a caller's mistake becomes a claim.
		return "", false
	}
	s, ok := obligationKindRequiredStrength[kind]
	return s, ok
}

// taskObligation is one thing the task owes, named by hash and floored by
// kind. Subject text never travels: Subject holds the canonical value only
// long enough for the producer that derived it, and is not serialised.
type taskObligation struct {
	ID   string
	Kind string
	// Subject is the canonical path or the exact command this obligation is
	// about. It is never logged and never leaves the process.
	Subject string
	// RequiredStrength is empty only for an unsatisfiable kind.
	RequiredStrength string
	Required         bool
}

// newTaskObligation fails closed: an unknown kind, an empty subject, or a
// baseline strength that does not name a real strength all produce nothing
// rather than an obligation nobody can measure.
func newTaskObligation(kind, subject, baselineStrength string, required bool) (taskObligation, bool) {
	id, ok := obligationID(kind, subject)
	if !ok {
		return taskObligation{}, false
	}
	if obligationUnsatisfiableKinds[kind] {
		return taskObligation{ID: id, Kind: kind, Subject: subject, Required: required}, true
	}
	strength, ok := obligationRequiredStrength(kind, baselineStrength)
	if !ok {
		return taskObligation{}, false
	}
	return taskObligation{
		ID: id, Kind: kind, Subject: subject,
		RequiredStrength: strength, Required: required,
	}, true
}

// --- the three roles, read off a derived obligation set ----------------------

// authorizationPrerequisites are the obligations that must be met by evidence
// bound to the exact candidate before those bytes may land.
//
// artifact_exists is deliberately absent: it cannot be evidenced before the
// candidate is on disk, and treating it as a prerequisite is the circle this
// split removes.
func authorizationPrerequisites(obs []taskObligation) []taskObligation {
	var out []taskObligation
	for _, o := range obs {
		if role, ok := obligationRole(o.Kind); ok &&
			role == ObligationRoleAuthorizationPrerequisite {
			out = append(out, o)
		}
	}
	return out
}

// postDeliverySettlement are the obligations answerable only once the bytes
// are on disk and the ledger confirms them.
func postDeliverySettlement(obs []taskObligation) []taskObligation {
	var out []taskObligation
	for _, o := range obs {
		if role, ok := obligationRole(o.Kind); ok &&
			role == ObligationRolePostDeliverySettlement {
			out = append(out, o)
		}
	}
	return out
}

// authorizedTargets are the canonical paths a delivery may replace, in
// canonical order.
//
// This is identity, not quality. A path appearing here says the client owns
// the request to produce it; it says nothing whatever about whether any given
// bytes belong in it, and no caller may read it as if it did.
func authorizedTargets(obs []taskObligation) []string {
	seen := map[string]bool{}
	var out []string
	for _, o := range obs {
		if !obligationKindNamesTarget[o.Kind] || o.Subject == "" || seen[o.Subject] {
			continue
		}
		seen[o.Subject] = true
		out = append(out, o.Subject)
	}
	sort.Strings(out)
	return out
}

// targetIsAuthorized reports whether a delivery to this canonical path was
// asked for. A path the client never declared cannot borrow another output's
// authority, so this is an exact-membership test and never a prefix or
// directory rule.
func targetIsAuthorized(obs []taskObligation, resolved string) bool {
	if strings.TrimSpace(resolved) == "" {
		return false
	}
	for _, t := range authorizedTargets(obs) {
		if t == resolved {
			return true
		}
	}
	return false
}

// authorizationFloor is the strongest floor the PREREQUISITES demand, and ""
// when the task states no prerequisite at all.
//
// "" is a real answer and not a permissive one. A declared document with no
// declared verification owes nothing this build can measure -- syntax is not
// applicable to it and inventing one would be fabricating a requirement -- so
// there is no floor. That does not authorize it: authorization additionally
// requires at least one satisfied prerequisite bound to the exact candidate,
// and a task with none has nothing to satisfy. The task is not impossible
// either; a client that declares a command gives it a path.
func authorizationFloor(obs []taskObligation) string {
	floor := ""
	for _, o := range authorizationPrerequisites(obs) {
		if !o.Required {
			continue
		}
		if obligationUnsatisfiableKinds[o.Kind] {
			return evidenceStrengthOrder[len(evidenceStrengthOrder)-1]
		}
		if floor == "" || strengthRank(o.RequiredStrength) > strengthRank(floor) {
			floor = o.RequiredStrength
		}
	}
	return floor
}

// settlementIsComplete reports whether every post-delivery obligation is now
// answerable in the affirmative: the exact bytes are on disk AND the ledger
// records them at that hash.
//
// Both halves are required. Disk without a ledger entry is a file nothing in
// this session owns; a ledger entry without matching bytes is a record about
// something that is no longer there.
func settlementIsComplete(ctx *AgentContext, obs []taskObligation,
	deliveredHash string) (bool, string) {
	for _, o := range postDeliverySettlement(obs) {
		if !o.Required {
			continue
		}
		disk := fileSHA256(ctx, o.Subject)
		if disk == "" {
			return false, "artifact is not on disk"
		}
		if deliveredHash != "" && disk != deliveredHash {
			return false, "bytes on disk are not the delivered bytes"
		}
		if !ledgerConfirms(ctx, o.Subject, disk) {
			return false, "the ledger does not confirm the bytes on disk"
		}
	}
	return true, ""
}

// ledgerConfirms reports whether the session's own record agrees that these
// exact bytes are what is at this path now.
func ledgerConfirms(ctx *AgentContext, resolved, hash string) bool {
	if ctx == nil || hash == "" {
		return false
	}
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	for key, d := range ctx.Ledger {
		if d == nil || d.Tombstoned {
			continue
		}
		if resolveAgentPath(ctx, key) != resolveAgentPath(ctx, resolved) {
			continue
		}
		return d.CurrentHash == hash && d.Generation > 0
	}
	return false
}

// --- derivation from the validated request ----------------------------------
//
// Obligations are derived from what the CALLER declared, never from prose,
// a filename convention, a benchmark, or anything the model emitted. A caller
// that stated no knowledge produces no structured obligation at all: its
// prose heuristic may still govern proxy completion, and converting that into
// structured authority is exactly the promotion this file must not make.

// requestObligations is the one place a caller asks "what does THIS request
// oblige". It reads the two owners once and derives from what they said.
//
// A reader, never a decider: it makes no policy call of its own, and the
// obligation owners stay the single authority on whose list a run owes.
// Everything downstream of derivation goes through here rather than reaching
// for the owners itself, so there is one place to look for who asked.
func requestObligations(ctx *AgentContext) []taskObligation {
	if ctx == nil || ctx.TaskContract == nil {
		return nil
	}
	message := ""
	if ctx.HumanTask != "" {
		message = ctx.HumanTask
	}
	return deriveTaskObligations(ctx,
		resolveOutputObligation(ctx, message),
		resolveVerificationObligation(ctx))
}

// deriveTaskObligations is THE derivation. It reads the two decisions the
// request boundary already made and the ledger's own record of what is on
// disk; it consults no shadow state and no model output.
//
// Ordering is canonical -- kind, then id -- so two runs of the same request
// produce the same set in the same order and a diff between them is a real
// difference rather than map iteration.
func deriveTaskObligations(ctx *AgentContext, outputs, verification obligationDecision) []taskObligation {
	var out []taskObligation
	seen := map[string]bool{}
	add := func(kind, subject, baseline string) {
		o, ok := newTaskObligation(kind, subject, baseline, true)
		if !ok || seen[o.ID] {
			return
		}
		seen[o.ID] = true
		out = append(out, o)
	}

	if outputs.KnowledgeSpecified {
		for _, p := range outputs.Items {
			resolved := resolveAgentPath(ctx, p)
			if resolved == "" {
				continue
			}
			// Existence is owed for every declared output.
			add(ObligationArtifactExists, resolved, "")
			// Structural validity is owed only for artifact classes the
			// proxy's own gate already governs. There is no second table:
			// this is the same map live completion policy consults.
			if _, gated := syntaxGateLanguages[strings.ToLower(filepath.Ext(resolved))]; gated {
				add(ObligationSyntacticValidity, resolved, "")
			}
			// Replacing an artifact that already carries current evidence
			// owes at least what that evidence already reached.
			if s := baselineEvidenceStrength(ctx, resolved); s != "" {
				add(ObligationBaselinePreserved, resolved, s)
			}
		}
	}

	if verification.KnowledgeSpecified {
		for _, cmd := range verification.Items {
			if strings.TrimSpace(cmd) == "" {
				continue
			}
			// The floor is the client's typed declaration for this exact
			// command, and the legacy answer -- runtime -- for one declared
			// without a type. Nothing here reads the command's text.
			req, ok := verification.Typed[cmd]
			if !ok {
				req = legacyVerificationRequirement(cmd)
			}
			add(ObligationDeclaredCommand, cmd, req.declaredStrength())
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// baselineEvidenceStrength is how strongly the artifact ALREADY on disk is
// described, or "" when nothing current describes it.
//
// Read from evidence that already exists rather than from a new judgement:
// the ledger's verdict must be about the bytes that are there now, and a
// green verification record must still cover those exact bytes. A verdict
// about superseded bytes is history, not a baseline.
// baselineWitness names what the artifact currently on disk has already been
// shown to be, and -- when that showing was behavioural -- WHICH command
// showed it.
//
// The command matters because preservation is not a strength comparison alone.
// A baseline that exists because `pytest -q` passed is preserved by `pytest -q`
// passing again, not by some other command that also exits zero. Returning the
// witness alongside the strength keeps the two from drifting apart.
func baselineWitness(ctx *AgentContext, resolved string) (string, string) {
	if ctx == nil {
		return "", ""
	}
	disk := fileSHA256(ctx, resolved)
	if disk == "" {
		// Nothing is there. A new file replaces nothing and owes no
		// preservation.
		return "", ""
	}
	// Behavioral: some green run still covers exactly these bytes for this
	// path. The command that covered them is the witness.
	if rec, ok := coverageRecord(ctx, resolveAgentPath(ctx, resolved), disk); ok {
		return "behavioral", contentSHA256(rec.Command)
	}
	// Syntax: the ledger holds a pass about exactly these bytes. A structural
	// pass has no command behind it, so there is no witness to name.
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	for key, d := range ctx.Ledger {
		if d == nil || d.Tombstoned {
			continue
		}
		if resolveAgentPath(ctx, key) != resolveAgentPath(ctx, resolved) {
			continue
		}
		if d.ValidationStatus == ValidationPassed && d.ValidatedHash == disk {
			return "syntax", ""
		}
	}
	return "", ""
}

// baselineEvidenceStrength is what the artifact on disk has already been shown
// to be, ignoring which command showed it.
func baselineEvidenceStrength(ctx *AgentContext, resolved string) string {
	strength, _ := baselineWitness(ctx, resolved)
	return strength
}
