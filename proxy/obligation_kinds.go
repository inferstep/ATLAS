package main

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/itigges22/atlas-proxy/internal/taskcontract"
	"github.com/itigges22/atlas-proxy/internal/wire"
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

// --- the three roles, read off a derived obligation set ----------------------

// settlementIsComplete reports whether every post-delivery obligation is now
// answerable in the affirmative: the exact bytes are on disk AND the ledger
// records them at that hash.
//
// Both halves are required. Disk without a ledger entry is a file nothing in
// this session owns; a ledger entry without matching bytes is a record about
// something that is no longer there.
func settlementIsComplete(ctx *AgentContext, obs []taskcontract.TaskObligation,
	deliveredHash string) (bool, string) {
	for _, o := range taskcontract.PostDeliverySettlement(obs) {
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
func requestObligations(ctx *AgentContext) []taskcontract.TaskObligation {
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
func deriveTaskObligations(ctx *AgentContext, outputs, verification taskcontract.ObligationDecision) []taskcontract.TaskObligation {
	var out []taskcontract.TaskObligation
	seen := map[string]bool{}
	add := func(kind, subject, baseline string) {
		o, ok := taskcontract.NewTaskObligation(kind, subject, baseline, true)
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
			add(taskcontract.ObligationArtifactExists, resolved, "")
			// Structural validity is owed only for artifact classes the
			// proxy's own gate already governs. There is no second table:
			// this is the same map live completion policy consults.
			if _, gated := syntaxGateLanguages[strings.ToLower(filepath.Ext(resolved))]; gated {
				add(taskcontract.ObligationSyntacticValidity, resolved, "")
			}
			// Replacing an artifact that already carries current evidence
			// owes at least what that evidence already reached.
			if s := baselineEvidenceStrength(ctx, resolved); s != "" {
				add(taskcontract.ObligationBaselinePreserved, resolved, s)
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
				req = taskcontract.LegacyVerificationRequirement(cmd)
			}
			add(taskcontract.ObligationDeclaredCommand, cmd, req.DeclaredStrength())
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
		if d.ValidationStatus == wire.ValidationPassed && d.ValidatedHash == disk {
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
