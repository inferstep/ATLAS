package app

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/itigges22/atlas-proxy/internal/taskcontract"
	"github.com/itigges22/atlas-proxy/internal/wire"
)

// What a request obliges, derived from its decisions and the ledger, and
// whether a delivery has settled it. The obligation vocabulary is in
// internal/taskcontract/obligation_kinds.go.

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
		addOutputObligations(ctx, outputs.Items, add)
	}
	if verification.KnowledgeSpecified {
		addCommandObligations(verification, add)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// addOutputObligations adds what each declared output owes.
func addOutputObligations(ctx *AgentContext, outputs []string, add func(kind, subject, baseline string)) {
	for _, p := range outputs {
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

// addCommandObligations adds what each declared verification command owes.
func addCommandObligations(verification taskcontract.ObligationDecision, add func(kind, subject, baseline string)) {
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
