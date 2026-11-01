// The summaries a run ends with when it did not finish cleanly: out of
// turns, a refused call sent again unchanged, the repeat breaker, a failed
// model call, nothing written, nothing changed, and a completion claim that
// nothing in the run verified. deliverablesDemonstrablyValid, which the
// repeat-breaker summary decides from, is here too.

package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// unverifiedSummary replaces a completion claim the run's own evidence cannot
// support.
//
// The verification gate bounces a `done` three times and then, out of
// bounces, lets it through — so the model's summary reaches the user
// unchanged, whatever the run showed.
//
// Rewriting the summary is mechanical and needs nothing from the model, which
// is why it is done here rather than by asking the model to be more careful.
// Making `done` ungrammatical would be stronger, but that needs strict
// schema-GBNF and Gemma-family models require the loose grammar (a strict
// schema makes them emit `done` instead of calling tools at all).
//
// The model's own words are kept, labelled, because they usually do describe
// the intended change accurately — it is the verification claim inside them
// that is unsupported.
func unverifiedSummary(wrote bool, claim string) string {
	var sb strings.Builder
	if wrote {
		sb.WriteString("Changes were written to disk, but NOTHING in this run verified them — " +
			"no build, test, or probe command completed successfully. Run it yourself before " +
			"relying on it.")
	} else {
		sb.WriteString("Nothing was written to disk in this run, and no verification command " +
			"completed successfully.")
	}
	if c := strings.TrimSpace(claim); c != "" {
		sb.WriteString("\n\nThe agent's own account, which is UNVERIFIED and may describe work " +
			"that did not land:\n")
		sb.WriteString(truncateStr(c, 1200))
	}
	return sb.String()
}

// outOfTurnsSummary is what the user reads when the loop hits its turn cap.
//
// The cap used to end the turn with an `error` event and nothing else, so a
// question whose recon ran long came back blank. A blank reply is the worst
// outcome the harness can produce: the user cannot tell whether ATLAS is
// broken, still thinking, or ignoring them. Say what happened, say what was
// learned, and name the next move.
func outOfTurnsSummary(ctx *AgentContext, wrote bool) string {
	var sb strings.Builder
	sb.WriteString("I ran out of turns for this request before finishing.")
	if wrote {
		sb.WriteString(" Changes were written to disk — check them before relying on them.")
	} else {
		sb.WriteString(" Nothing was written to disk.")
	}
	if files := ctx.SnapshotFilesRead(); len(files) > 0 {
		names := make([]string, 0, len(files))
		for p := range files {
			if rel, err := filepath.Rel(ctx.WorkingDir, p); err == nil && rel != "" {
				names = append(names, rel)
			} else {
				names = append(names, filepath.Base(p))
			}
		}
		sort.Strings(names)
		fmt.Fprintf(&sb, "\n\nI did get to look at: %s.", strings.Join(names, ", "))
	}
	sb.WriteString("\n\nAsk again and point me at the specific file or function you care " +
		"about — a narrower request finishes inside the budget.")
	return sb.String()
}

// repeatedRefusalSummary ends a run that kept re-sending one rejected call.
//
// The user needs to know the tool call was refused rather than attempted, and
// that re-running the same prompt will do the same thing — otherwise the
// obvious response is to try again verbatim.
func repeatedRefusalSummary(tool, path string, wrote bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Stopped: the same `%s` call was re-sent after being refused, and kept being "+
		"re-sent without changing.", tool)
	if path != "" {
		fmt.Fprintf(&sb, " Target: %s.", path)
	}
	if wrote {
		sb.WriteString(" Earlier changes in this run did land on disk — check them.")
	} else {
		sb.WriteString(" Nothing was written to disk.")
	}
	sb.WriteString("\n\nThe refusal reason is in the per-turn errors above. Re-running this " +
		"prompt unchanged will hit the same wall; say specifically what to change and where, " +
		"or point at a different file.")
	return sb.String()
}

// repeatTerminalSummary is the terminal for the repeat detector, and the one
// place that decides whether a run may say its change landed.
//
// `wrote` (madeProductiveChange) is a PROGRESS HINT. It records that some
// mutation succeeded earlier in the run; it says nothing about the bytes on
// disk now, and it may never authorize a completion claim. Measured on the
// seed-20260901 confirmation, task debounce5: an accepted write set the hint,
// the model then repeated a failing verification, and the terminal read "Made
// your change ... the change is on disk" over a file containing a
// SyntaxError. One false success in 50 sessions, and the only terminal in
// that run which misreported its own outcome.
//
// Completion is therefore derived from a FRESH observation of the declared
// deliverable's CURRENT bytes, through the same syntax contract the write
// path uses. Existence is not validity, an earlier success is not this
// content, and anything short of an explicit pass -- not_run, not_applicable,
// unknown, unreadable, or nothing declared -- is undemonstrated and stops.
// recovered carries the per-path outcomes of Phase 3B restoration, which the
// caller performs before composing this summary. It is disclosed as its own
// clause: recovery changes what is on disk, never whether the run finished.
func repeatTerminalSummary(ctx *AgentContext, expected []string, wrote bool,
	recovered []restoreDecision) string {
	var sb strings.Builder
	sb.WriteString("Stopped: the same tool call kept repeating without making progress")
	// Validation status alters DISCLOSURE only. A repeat-breaker is an
	// operational failure whatever the bytes look like: syntax is not task
	// completion, and the run stopped without finishing its verification.
	// Neither branch may read as a completion claim.
	switch {
	case !wrote:
		sb.WriteString(", and nothing was written to disk")
	case deliverablesDemonstrablyValid(ctx, expected):
		sb.WriteString(". Your work is on disk and parses, but the " +
			"verification did not complete, so this run cannot say the task is done")
	default:
		sb.WriteString(". Earlier changes did land on disk, but the current " +
			"contents were not shown to be valid — treat them as unverified")
	}
	sb.WriteString(". Try a more specific instruction (e.g. name the file and " +
		"the exact change).")
	// Appended last, and never in place of the stop: a restored file is a
	// safer starting point, not a completed task.
	sb.WriteString(restorationDisclosure(recovered))
	return sb.String()
}

// deliverablesDemonstrablyValid answers the completion question and nothing
// else: does every declared deliverable, as it exists RIGHT NOW, pass the
// syntax contract? With nothing declared there is nothing demonstrated, so
// the answer is no.
func deliverablesDemonstrablyValid(ctx *AgentContext, expected []string) bool {
	if len(expected) == 0 {
		return false
	}
	for _, rel := range expected {
		// A deliverable that resolves outside the workspace demonstrates nothing.
		content, resolved, err := readWorkspaceFile(ctx, rel)
		if err != nil {
			return false
		}
		checked := fallbackSyntaxOutcomeFor(ctx, resolved, string(content))
		// A script the file embeds that the harness found broken is a
		// demonstrated failure, whatever the whole-file parse says: a Flask
		// app whose HTML_TEMPLATE has a stray `)` in its <script> compiles,
		// serves, and breaks in the browser. Only a demonstrated one: an
		// embedded check that could not run is no finding (the documented
		// fail-soft).
		if checked.Embedded.Status == ValidationFailed {
			return false
		}
		status := checked.WholeFile.Status
		if status == ValidationPassed {
			continue
		}
		// A document has no syntax to pass. Requiring one meant a valid
		// notes.txt could never demonstrate anything, however current its
		// bytes were. What stands in for the pass is existence plus currency:
		// the checker reports that nothing applies, the path is prose rather
		// than unsupported code, and the ledger's record still describes the
		// bytes that are there. This is not a syntax pass and is never
		// relabelled as one.
		if status == ValidationNotApplicable && documentDeliverableCurrent(ctx, resolved, content) {
			continue
		}
		return false
	}
	return true
}

// documentDeliverableCurrent is the existence-and-currency evidence a non-code
// deliverable can offer in place of a syntax pass.
//
// Every clause is required. Prose only, so an unsupported language or an
// unknown extension holding logic is excluded; the ledger must already own the
// path, so nothing the session never wrote qualifies; its recorded verdict
// must be exactly "checked nothing because nothing applies"; and that record
// has to describe the bytes on disk right now.
func documentDeliverableCurrent(ctx *AgentContext, resolved string, content []byte) bool {
	if !isDocumentAsset(resolved) {
		return false
	}
	key := ledgerKey(ctx, resolved)
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[key]
	var kind ValidationKind
	var status ValidationStatus
	var tombstoned bool
	var current string
	if d != nil {
		kind, status = d.CurrentValidation()
		tombstoned, current = d.Tombstoned, d.CurrentHash
	}
	ctx.LedgerMu.Unlock()
	if d == nil || tombstoned {
		return false
	}
	if kind != ValidationKindNone || status != ValidationNotApplicable {
		return false
	}
	return current == hashBytes(content)
}

// inferenceFailureSummary is what the user reads when the model call itself
// fails — the stream cannot continue, so this is the last thing they get.
//
// The context-size 400 is called out by name because it is actionable and
// because it was the deterministic killer: `aoc_sonar` hit it in both reps at
// turn 3, and the run ended with an `error` event and no outcome at all.
func inferenceFailureSummary(err error, wrote bool) string {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	var sb strings.Builder
	if strings.Contains(msg, "exceed_context_size") || strings.Contains(msg, "exceeds the available context size") {
		sb.WriteString("Stopped: the conversation outgrew the model's context window, so the " +
			"request was refused before it ran. This usually means a large file was read " +
			"into the session. Start a fresh request naming just the file and change you " +
			"want, or raise the server's context size.")
	} else {
		sb.WriteString("Stopped: the model call failed, so the run could not continue.")
		if msg != "" {
			fmt.Fprintf(&sb, "\n\n%s", truncateStr(msg, 300))
		}
	}
	if wrote {
		sb.WriteString("\n\nChanges made earlier in this run are on disk — check them before re-running.")
	} else {
		sb.WriteString("\n\nNothing was written to disk.")
	}
	return sb.String()
}

// nothingWrittenSummary states that a run which was asked to change
// something finished without changing anything.
//
// The action gate bounces this while it can, then stops — its bounces are
// capped so an exhausted gate cannot loop. Past the cap the exit is
// unremarked, and the worst version is a summary that looks like success:
// observed on smallrung_toml, a refused structural_edit, the model giving
// up on tools and emitting the replacement as chat text, and that code
// arriving as the run's summary. A user reading it has no way to tell the
// change was never applied.
//
// Prefixed rather than replacing: whatever the model said may still be
// useful, it just cannot stand as a completion claim.
func nothingWrittenSummary(original string) string {
	const lead = "Nothing was written — no file was created or changed in this run. " +
		"Any code below is a proposal, not something on disk."
	if strings.TrimSpace(original) == "" {
		return lead
	}
	return lead + "\n\n" + original
}

// noChangeNote is what a completed run that looked into the project and
// changed nothing says about it. Nothing demanded a change, or the stronger
// nothingWrittenSummary would apply; but a request misread as a question
// ended completed with the model's "Updated calc.py" as its whole summary,
// and nothing told the user no file had changed.
const noChangeNote = "No file was created or changed in this run."

// withNoChangeNote appends noChangeNote to a completed run's summary when the
// run inspected the workspace and is known to have changed nothing. A reply
// that never looked at the project (a greeting, a general question) is left
// as it is.
func withNoChangeNote(ctx *AgentContext, st *runState, status TerminalStatus, summary string) string {
	if !status.Completed() || st == nil || !st.inspectedWorkspace || !runChangedNothing(ctx, st) {
		return summary
	}
	if strings.TrimSpace(summary) == "" {
		return noChangeNote
	}
	return summary + "\n\n" + noChangeNote
}

// runChangedNothing reports whether this run is known to have changed no
// file: no tool write, nothing the shell wrote or removed, and no part of the
// workspace the shell touched left unobserved. A shell-written file sets no
// tool flag, so the ledger is what says whether anything changed.
func runChangedNothing(ctx *AgentContext, st *runState) bool {
	if ctx == nil || st == nil || st.madeProductiveChange || ctx.ShellEffectsUnobserved {
		return false
	}
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	for _, d := range ctx.Ledger {
		if d.Tombstoned || d.Generation > 0 {
			return false
		}
	}
	return true
}
