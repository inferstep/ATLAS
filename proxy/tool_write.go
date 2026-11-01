// The write_file tool: its gates, its direct route to disk, and the atomic write itself.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// write_file — T0/T1 direct, T2/T3 routes through V3 pipeline
// ---------------------------------------------------------------------------

func writeFileTool() *ToolDef {
	return &ToolDef{
		Name:   "write_file",
		Effect: ToolEffectDirectMutation,
		Description: "Create a NEW file from scratch. Creates parent directories if needed. " +
			"For any file longer than a few lines, set content to \"@fenced\" and provide " +
			"the file as a plain fenced code block when asked — code embedded in a JSON " +
			"string loses parens and newlines to escaping. " +
			"DO NOT use to overwrite existing files — for existing files use structural_edit (whole function/class/element rewrite) or edit_file (≤10-line surgical change). " +
			"If a write_file call is rejected because the path already exists, switch to structural_edit (whole-block rewrite) or edit_file (surgical change). DO NOT retry with edit_file simply because the file is large.",
		InputSchema: WriteFileInput{},
		ReadOnly:    false,
		Destructive: true,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (res *ToolResult, execErr error) {
			var input WriteFileInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
			}

			// Reject empty path — same reasoning as read_file.
			if strings.TrimSpace(input.Path) == "" {
				return noMutation("write_file: path cannot be empty. Provide a relative path like \"snake_game.py\" or \"src/main.py\"."), nil
			}

			path := resolveAgentPath(ctx, input.Path)

			// Sanitise model output before anything else touches it.
			// Otherwise a markdown-fenced response with a prose preamble
			// ("Looking at the task..." / ```html / actual code / ```)
			// lands on disk verbatim and the file becomes unparseable.
			cleaned, sanitized := sanitizeFileContent(input.Path, input.Content)
			if sanitized {
				log.Printf("[write_file] removed a whole-file fence wrapper from %s (was %d chars, now %d)",
					input.Path, len(input.Content), len(cleaned))
				input.Content = cleaned
				defer func() { noteContentChange(res, wholeFileWrapperNote) }()
			}

			if refusal := writeFileEarlyRefusal(ctx, input, path); refusal != nil {
				return refusal, nil
			}

			// Stub detection. Reject one-heading or to-do-marker
			// placeholder writes that pass syntactic gates but ship the
			// minimum content humanly possible. The model's lazy-completion
			// failure mode is to write 8-line stubs and call it done; this
			// gate forces it to either commit real content or acknowledge
			// the stub explicitly. New files only — edits to existing
			// files might legitimately shrink to a stub via refactor.
			if isNewWrite(path) {
				if reason := looksLikeStub(input.Path, input.Content); reason != "" {
					return refusedNoCheck(reason), nil
				}
			}

			fileTier := writeFileTier(ctx, input)

			// V3 pipeline fires on T2+ files when the V3 service is available.
			// V3 takes the model's content as the baseline candidate, generates
			// diverse alternatives via PlanSearch/DivSampling, build-verifies
			// each, and selects the best.
			//
			// Except during an active edit-test-fix loop: once the model has
			// written this file and just saw it fail a run, the next write is a
			// targeted fix, and V3's full pipeline (minutes per call, and on a
			// mid-debug file it often completes without a result and falls back
			// anyway) throttles iteration to a handful of cycles. Fast-path it
			// (direct, still syntax-gated below) so the model can iterate at
			// run speed. V3 still owns the first write of each file, the
			// baseline generation where it adds value.
			iterating := isActiveDebugIteration(ctx, input.Path)

			return writeFileByRoute(ctx, input, path, fileTier, iterating)
		},
	}
}

// writeFileEarlyRefusal is the refusal a write meets before it is classified:
// a new file that should start from its siblings, or bytes that echo the file
// already there. nil when neither applies.
func writeFileEarlyRefusal(ctx *AgentContext, input WriteFileInput, path string) *ToolResult {
	// Pattern-matching reflex. When the model creates a
	// NEW file in a non-empty directory of similar files (HTML
	// alongside HTML, route handler alongside route handlers),
	// nudge it to read a sibling first instead of generating
	// content from scratch. Only fires for genuinely-new files
	// to avoid breaking edits-via-write_file. Soft hint via
	// tool result, not a hard reject — the model can ignore it
	// if the content is clearly intentional.
	if hint := patternMatchHint(path, ctx.SnapshotFilesRead()); hint != "" {
		return refusedNoCheck(hint)
	}

	// Echoed write: the content is the file that is already there.
	// Reproducing data the session already has is never the task,
	// and it is where the worst measured failure starts — the model
	// retyping a 2000-line fixture from memory, degenerating into
	// repetition, and getting its stream cut mid-JSON.
	if existing, err := readRegularFile(path); err == nil {
		if echoesExistingFile(string(existing), input.Content) ||
			(ctx.SessionWrites[input.Path] && resendsOwnWrite(string(existing), input.Content)) {
			log.Printf("[write_file] refusing an echoed write of %s (%d bytes on disk, %d incoming)",
				logPath(input.Path), len(existing), len(input.Content))
			return refusedNoCheck(echoedWriteRejection(input.Path, ctx.SessionWrites[input.Path]))
		}
	}
	return nil
}

// writeFileTier is the tier of the file a write lands in, which the real
// cyclomatic complexity may raise but never lower.
func writeFileTier(ctx *AgentContext, input WriteFileInput) Tier {
	// Per-file tier classification — determines V3 pipeline activation
	fileTier := classifyFileTier(input.Path, input.Content)
	// GH #39 point 2: real cyclomatic complexity from tree-sitter
	// can escalate the regex classifier's verdict. Never downgrades.
	if cc, ok := cyclomaticComplexity(ctx, input.Path, input.Content); ok {
		if refined := refineTierWithCC(fileTier, cc); refined != fileTier {
			log.Printf("[write_file] %s tier %s→%s via cc=%d", input.Path, fileTier, refined, cc)
			fileTier = refined
		} else {
			log.Printf("[write_file] %s cc=%d (tier %s unchanged)", input.Path, cc, fileTier)
		}
	}
	log.Printf("[write_file] %s → %s (%d lines)", input.Path, fileTier, strings.Count(input.Content, "\n")+1)
	return fileTier
}

// writeFileByRoute applies the regression gate every write meets, then sends
// the write to the V3 pipeline or down the direct route, as the activation
// owner decides.
func writeFileByRoute(ctx *AgentContext, input WriteFileInput, path string, fileTier Tier, iterating bool) (*ToolResult, error) {
	proposalCheck, baselineCheck, refusal := writeRegressionGate(ctx, input, path)
	if refusal != nil {
		return refusal, nil
	}
	// Content that does not parse skips V3. V3 improves a working candidate; it
	// does not exist to guess what a malformed one meant, and the post-V3
	// fallback rejects an unparseable baseline anyway, so spending the budget
	// first only delays the same answer. The active-debug fast path cannot
	// cover this: it keys off a successful write of the file, which a model
	// failing the syntax gate never records.
	//
	// One owner decides whether the producer is consulted and records which
	// threshold turned it away.
	//
	// The typed predicate, named at the site that reaches the producer. The
	// owner below decides which reason applies and in what order; this is the
	// dependency being visible where the dispatch happens, so a reader looking
	// at the call that reaches generation can see that disabling generation
	// reaches it.
	writeBypass := writeGenerationBypass(ctx, fileTier, iterating)
	recordCandidateGenerationBypass(ctx, "write_file", writeBypass,
		fileTier, strings.Count(input.Content, "\n")+1)
	logBudgetBypass("write_file", input.Path, writeBypass)
	if writeBypass == bypassNone {
		return writeFileThroughV3(ctx, input, path, fileTier, proposalCheck, baselineCheck)
	}
	return writeFileDirectRoute(ctx, input, path, iterating, proposalCheck, baselineCheck)
}

// writeRegressionGate refuses to replace bytes on disk that parse with bytes
// that do not. It returns what it observed of the proposal and of the bytes on
// disk, Unknown for a side it did not evaluate, and the refusal if it refused.
func writeRegressionGate(ctx *AgentContext, input WriteFileInput, path string) (checkOutcome, checkOutcome, *ToolResult) {
	// Regression protection for existing code, independent of tier
	// and of whether V3 is configured. The healthy->broken rule
	// exists to protect WORKING code, but it used to live only
	// inside branches gated on `fileTier >= Tier2Medium &&
	// ctx.V3URL != ""` or on an active debug iteration. A small
	// file, or any file in a session without V3, reached neither and
	// had valid bytes replaced by invalid ones with no check at all.
	// Measured black-box through the agent loop: a T1 overwrite
	// destroyed good bytes whether or not V3 was configured, while
	// the T2+V3 cell refused the same rewrite.
	//
	// Scope is deliberately narrow, so the three intended policies
	// survive: a NEW file still lands with a warning (nothing on
	// disk to protect), an already-broken file still accepts a
	// repair attempt (the baseline demonstrably fails), and a
	// non-gated language is unaffected (the checker reports
	// not_applicable off syntaxGateLanguages).
	//
	// The observations are KEPT rather than collapsed to a boolean.
	// The active-debug branch below asks the same question of the same
	// bytes, and re-asking the checker there costs the fast path a
	// second sandbox round trip and loses the distinction between
	// passed, not_run and not_applicable that a route needs in order to
	// classify its own result. Each side is evaluated at most once per
	// dispatch; ValidationUnknown means "not evaluated here".
	proposalCheck := checkOutcome{Status: ValidationUnknown}
	baselineCheck := checkOutcome{Status: ValidationUnknown}
	if _, statErr := os.Stat(path); statErr == nil {
		proposalCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, input.Content).aggregate()
		if proposalCheck.Status == ValidationFailed {
			synErr := proposalCheck.Detail
			if prior, priorOK := readOriginalForGate(path); priorOK {
				baselineCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, prior).aggregate()
				if !baselineAllowsRepair(baselineCheck) {
					log.Printf("[write_file] %s: refusing to regress valid content to invalid (%s)",
						logPath(input.Path), safeDiagnosticSummary(synErr))
					// The check examined input.Content, which is exactly
					// the content that would have been written, and the
					// refusal happens before any byte reaches disk.
					return proposalCheck, baselineCheck, &ToolResult{
						Success:          false,
						Error:            fallbackSyntaxRejection(input.Path, input.Content, synErr),
						MutationStatus:   MutationRefused,
						ValidationKind:   ValidationKindSyntax,
						ValidationStatus: ValidationFailed,
						ValidationDetail: synErr,
					}
				}
			}
		}
	}
	return proposalCheck, baselineCheck, nil
}

// writeFileThroughV3 is the route of a write the pipeline sees. Bytes that do
// not parse never reach the pipeline: they land with a warning as a new file
// or as a repair of a file already broken, and are refused otherwise.
func writeFileThroughV3(ctx *AgentContext, input WriteFileInput, path string, fileTier Tier,
	proposalCheck, baselineCheck checkOutcome) (*ToolResult, error) {
	// The regression gate above evaluated these exact bytes
	// whenever the destination exists, so the evaluation here is
	// for the case it skipped: a file that does not exist yet,
	// which is this gate's main case.
	if proposalCheck.Status == ValidationUnknown {
		proposalCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, input.Content).aggregate()
	}
	if proposalCheck.Status == ValidationFailed {
		synErr := proposalCheck.Detail
		// Feeding V3 broken content wastes its whole budget, so
		// V3 is skipped either way. What happens to the bytes
		// depends on what is at the path: clobbering an existing
		// file with garbage is still refused, but a NEW file
		// lands with a warning so the model can run it and read
		// the real traceback. See writeNewFileWithWarning.
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			log.Printf("[write_file] new file %s does not parse — writing with a warning, skipping V3 (%s)",
				logPath(input.Path), safeDiagnosticSummary(synErr))
			recordCandidateGenerationBypass(ctx, "write_file",
				bypassProposalFailedSyntaxGuard, fileTier,
				strings.Count(input.Content, "\n")+1)
			return preflightWarnedWrite(path, input.Path, input.Content, proposalCheck, ctx)
		}
		// The strictness on existing files protects WORKING code.
		// When what is on disk is itself unparseable, there is
		// nothing to protect, and rejecting an imperfect fix
		// guarantees the broken version survives. Same
		// healthy->broken rule the fast-path below already uses.
		// Measured twice on the novel benchmark: a broken first
		// draft landed, the corrective write carried a new syntax
		// slip, the rejection fed the model a line it could not
		// act on, and it re-sent byte-identical content until the
		// repetition breaker ended the session with the ORIGINAL
		// broken file still on disk.
		if prior, priorOK := readOriginalForGate(path); priorOK {
			if baselineCheck.Status == ValidationUnknown {
				baselineCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, prior).aggregate()
			}
			if baselineAllowsRepair(baselineCheck) {
				log.Printf("[write_file] %s already broken on disk — landing the repair attempt with a warning (%s)",
					logPath(input.Path), safeDiagnosticSummary(synErr))
				recordCandidateGenerationBypass(ctx, "write_file",
					bypassProposalFailedSyntaxGuard, fileTier,
					strings.Count(input.Content, "\n")+1)
				return preflightWarnedWrite(path, input.Path, input.Content, proposalCheck, ctx)
			}
		}
		log.Printf("[write_file] %s does not parse — rejecting before V3 (%s)",
			logPath(input.Path), safeDiagnosticSummary(synErr))
		recordCandidateGenerationBypass(ctx, "write_file",
			bypassProposalFailedSyntaxGuard, fileTier,
			strings.Count(input.Content, "\n")+1)
		// Refused before any byte reached disk, on exactly the
		// content that would have been written. An unreadable
		// baseline arrives here too: without a baseline there is
		// no healthy->broken comparison to make, and the
		// conservative answer is the refusal.
		return &ToolResult{
			Success:          false,
			Error:            fallbackSyntaxRejection(input.Path, input.Content, synErr),
			MutationStatus:   MutationRefused,
			ValidationKind:   ValidationKindSyntax,
			ValidationStatus: ValidationFailed,
			ValidationDetail: synErr,
		}, nil
	}

	// proposalCheck now describes input.Content: the BASELINE
	// handed to the pipeline, not the bytes it may come back with.
	// A winning candidate is different content and needs its own
	// observation, and the fallback write inside writeFileWithV3
	// re-checks the baseline itself. Threading this observation in
	// there is a separate change: it would start a partial
	// migration of that function's own state machine, so the
	// handoff is left explicitly pending rather than stored on the
	// context or in a package-level value.
	log.Printf("[write_file] V3 pipeline activating for %s", input.Path)
	res, err := writeFileWithV3(path, input.Content, ctx)
	if err == nil && res != nil && res.Success {
		ctx.SessionWrites[input.Path] = true
	}
	return res, err
}

// writeFileDirectRoute is the route of a write the pipeline does not see: the
// gates of the active-debug fast path, or the structural gate of the direct
// write, and then the landing.
func writeFileDirectRoute(ctx *AgentContext, input WriteFileInput, path string, iterating bool,
	proposalCheck, baselineCheck checkOutcome) (*ToolResult, error) {
	if iterating {
		var refusal *ToolResult
		proposalCheck, refusal = writeFastPathGates(ctx, input, path, proposalCheck, baselineCheck)
		if refusal != nil {
			return refusal, nil
		}
	}

	// #147: the T0/T1 direct path skipped the structural gate — a
	// sub-10-line .py calling an unimported name landed as verified,
	// the same NameError class the edit path blocks. Structural gate
	// ONLY (.py-scoped, healthy->broken, fail-open): a syntax gate
	// here would hard-block legitimate non-parsing T1 content that
	// this branch exists to handle (JSONC, multi-doc and templated
	// YAML, scaffold .py templates). An unreadable existing original
	// skips the gate (fail open) — treating it as empty would count
	// every pre-existing call as introduced.
	if !iterating {
		if original, ok := readOriginalForGate(path); ok {
			if introduced := editIntroducesUnresolved(ctx, path, original, input.Content); len(introduced) > 0 {
				log.Printf("[write_file] direct write introduces unresolved call(s) %v in %s — rejecting", logPaths(introduced), logPath(input.Path))
				// Structural validation failed before any mutation.
				// Syntax does NOT run on this route (see the comment
				// above), so no syntax verdict is implied in either
				// direction -- the decisive demonstrated fact is the
				// structural failure.
				//
				// editIntroducesUnresolved -> checkStructuralUnresolved
				// posts to /internal/structural_check (NOT
				// /internal/symbol_index, a different path in
				// context.go) and returns
				// {"ok": bool, "unresolved": []string}. It FAILS OPEN
				// when ok is absent or false, on transport error,
				// non-200, parse error or missing tree-sitter, and
				// needs BOTH the edited- and original-side calls to
				// succeed before it can refuse. Observing an HTTP
				// request therefore does not prove the gate ran; only
				// the refusal does.
				rejection := structuralWriteRejection(input.Path, introduced)
				return &ToolResult{
					Success:          false,
					Error:            rejection,
					MutationStatus:   MutationRefused,
					ValidationKind:   ValidationKindStructural,
					ValidationStatus: ValidationFailed,
					ValidationDetail: rejection,
				}, nil
			}
		}
	}

	return landDirectWrite(ctx, input, path, proposalCheck)
}

// writeFastPathGates are the gates of the active-debug fast path, which skips
// the pipeline. It returns the observation of the proposal, made here when
// nothing had made it yet, and the refusal when a gate refused.
func writeFastPathGates(ctx *AgentContext, input WriteFileInput, path string,
	proposalCheck, baselineCheck checkOutcome) (checkOutcome, *ToolResult) {
	log.Printf("[write_file] active edit-test-fix loop on %s — fast-path direct write (V3 skipped)", input.Path)
	// The file already exists this session; read its on-disk
	// content once and run both gates against it with the
	// healthy->broken rule edit_file uses — block only a NEWLY
	// introduced defect, allow a repair-in-progress on
	// already-broken content. Without the healthy->broken guard
	// the syntax gate would hard-block content the strict checker
	// rejects both before AND after (a multi-doc YAML or JSONC
	// config being iterated), which is exactly why the T0/T1
	// direct path below carries no syntax gate.
	original, origOK := readOriginalForGate(path)
	// The proposal was already evaluated above whenever the
	// destination exists, which is the normal state of this route:
	// it fires on a file the session wrote and just watched fail.
	// Evaluate here only for the residual case where it was not --
	// the file has been deleted since that write.
	if proposalCheck.Status == ValidationUnknown {
		proposalCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, input.Content).aggregate()
	}
	if proposalCheck.Status == ValidationFailed {
		// Reaching here with a demonstrably failing proposal means
		// the gate above did not decide: either the baseline
		// demonstrably failed (repair-in-progress), or there was
		// no baseline to read -- the file is gone or unreadable.
		// That second case is why this gate stays.
		// readOriginalForGate yields "" for it, which the checker
		// finds healthy, so the regression is still refused rather
		// than landing unexamined.
		if baselineCheck.Status == ValidationUnknown {
			baselineCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, original).aggregate()
		}
		if !baselineAllowsRepair(baselineCheck) {
			synErr := proposalCheck.Detail
			// The check examined input.Content, the exact bytes
			// that would have been written, and nothing has
			// reached disk at this point.
			return proposalCheck, &ToolResult{
				Success:          false,
				Error:            fallbackSyntaxRejection(input.Path, input.Content, synErr),
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindSyntax,
				ValidationStatus: ValidationFailed,
				ValidationDetail: synErr,
			}
		}
		log.Printf("[write_file] %s still unparsable after fast-path write (was already broken) — allowing repair-in-progress", input.Path)
	}
	// #147 review finding #1: the fast-path skips V3 (and its
	// structural veto), so it needs the same structural gate
	// edit_file/structural_edit have — otherwise a fast-path rewrite that
	// introduces render_template lands as verified and 500s. A new
	// unresolved call vs the on-disk state blocks; an unreadable
	// original skips the gate (fail open).
	if origOK {
		if introduced := editIntroducesUnresolved(ctx, path, original, input.Content); len(introduced) > 0 {
			log.Printf("[write_file] fast-path write introduces unresolved call(s) %v in %s — rejecting", logPaths(introduced), logPath(input.Path))
			// Syntax ran first and PASSED on these exact bytes; the
			// structural check is what refused them, so structural
			// is the decisive outcome.
			return proposalCheck, &ToolResult{Success: false,
				Error:            structuralWriteRejection(input.Path, introduced),
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindStructural,
				ValidationStatus: ValidationFailed,
				ValidationDetail: structuralWriteRejection(input.Path, introduced),
			}
		}
	}
	return proposalCheck, nil
}

// landDirectWrite lands the bytes of a write that does not go through the
// pipeline: with a warning when a new file, or a file already broken, does not
// parse, and refused when they break an embedded script or repeat the entry
// point.
func landDirectWrite(ctx *AgentContext, input WriteFileInput, path string, proposalCheck checkOutcome) (*ToolResult, error) {
	// A new file is checked here: a file being created has no "before"
	// state for the healthy->broken rule, so whether it parses is
	// decidable, and a new file that does not parse lands with a
	// warning (writeNewFileWithWarning) so the model can run it and
	// read the real traceback. os.Stat, not readOriginalForGate: that
	// helper returns ("", true) for a missing file (its bool means
	// "usable as a baseline", not "exists"), so testing it here would
	// skip the check. Destination absence is determined once, and when
	// the file is new the structured checker is evaluated once. This
	// route owns a real observation, so it overlays it onto the result
	// afterwards rather than leaving writeFileDirect's conservative
	// default.
	_, statErr := os.Stat(path)
	isNew := os.IsNotExist(statErr)
	newFileCheck := checkOutcome{Status: ValidationUnknown}
	if isNew {
		newFileCheck = fallbackSyntaxOutcomeFor(ctx, input.Path, input.Content).aggregate()
		if newFileCheck.Status == ValidationFailed {
			log.Printf("[write_file] new file %s does not parse — writing with a warning (%s)",
				logPath(input.Path), truncateStr(newFileCheck.Detail, 80))
			return writeNewFileWithWarning(path, input.Path, input.Content, newFileCheck.Detail, ctx)
		}
	}

	// The destination exists and these exact bytes were already
	// observed not to parse. A healthy baseline was refused outright
	// above, so reaching here means what is on disk is itself broken
	// or unreadable — the same case the non-bypassed route lands with
	// a warning. Land it the same way, and say so.
	//
	// Only the SAYING is new. The observation was already overlaid
	// onto the result below, so the ledger and the completion gate
	// knew; the model did not. It was handed {"bytes_written":1243}
	// and nothing else, which reads as a clean write. Measured on
	// multifile_cli rep2: the model rewrote test_store.py containing
	// `store.add("Buy milk"")`, was told the write succeeded, ran
	// pytest, could not reconcile the failure with a write it had
	// been told was fine, and resent content until the repetition
	// breaker closed the path — leaving the file unparseable at the
	// end of the session. The sandbox had answered `valid:false` for
	// those bytes the whole time.
	if proposalCheck.Status == ValidationFailed {
		log.Printf("[write_file] %s: landing an unparseable rewrite with a warning (%s)",
			logPath(input.Path), safeDiagnosticSummary(proposalCheck.Detail))
		return preflightWarnedWrite(path, input.Path, input.Content, proposalCheck, ctx)
	}

	// The two comparative gates the V3 route applies to the bytes it
	// writes (writeFileWithV3). They are checks on the write, not part
	// of candidate generation, so a write that does not generate --
	// small, mid-iteration, or with nothing deliverable -- must not skip
	// them. Healthy->broken and fail-soft, as there.
	if original, ok := readOriginalForGate(path); ok {
		if msg := embeddedScriptGate(ctx, path, original, input.Content); msg != "" {
			log.Printf("[write_file] direct write breaks an embedded script in %s — rejecting", logPath(input.Path))
			return &ToolResult{Success: false, Error: msg,
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindStructural,
				ValidationStatus: ValidationFailed,
				ValidationDetail: msg}, nil
		}
		if msg := duplicateMainGuard(path, original, input.Content); msg != "" {
			log.Printf("[write_file] direct write duplicates the module entrypoint in %s — rejecting", logPath(input.Path))
			return &ToolResult{Success: false, Error: msg,
				MutationStatus:   MutationRefused,
				ValidationKind:   ValidationKindStructural,
				ValidationStatus: ValidationFailed,
				ValidationDetail: msg}, nil
		}
	}

	return writeDirectAndObserve(ctx, input, path, isNew, newFileCheck, proposalCheck)
}

// writeDirectAndObserve writes the bytes of a direct write and attaches the
// observation its route already holds of exactly those bytes.
func writeDirectAndObserve(ctx *AgentContext, input WriteFileInput, path string, isNew bool,
	newFileCheck, proposalCheck checkOutcome) (*ToolResult, error) {
	logMandatoryChecks(ctx, "write_file", input.Path, "direct")
	// T1: Direct write — config, data, boilerplate
	res, err := writeFileRecorded(path, input.Content, ctx)
	if err == nil && res != nil && res.Success {
		ctx.SessionWrites[input.Path] = true
	}
	// Mutation facts stay as the writer reported them; only the
	// validation observation is overlaid, and only by a route that
	// actually made one. Unknown is preserved.
	switch {
	case isNew:
		res, err = applyRouteObservation(res, err, newFileCheck)
	case proposalCheck.Status != ValidationUnknown:
		// Every remaining route through this write -- the active-debug
		// fast path and the ordinary direct write over an existing
		// file -- already holds an observation of these exact bytes.
		// The shared regression gate makes it whenever the destination
		// exists, and the active-debug branch fills it in for the one
		// case where the destination is gone, so non-Unknown IS the
		// ownership test. Restating it with a second checker call
		// would spend a round trip to risk a different answer about
		// bytes that have not changed since.
		res, err = applyRouteObservation(res, err, proposalCheck)
	}
	return res, err
}

// isActiveDebugIteration reports whether the model is in a tight edit-
// test-fix loop on `path`: it has already written this file this session
// AND the most recent tool action was a run_command/run_background that
// FAILED with output referencing this file. In that state the next write
// is a targeted fix for an error the model just saw, so the full V3
// pipeline only adds latency (and on a mid-debug file often "completes
// without result"). Fast-path those writes so iteration runs at test
// speed. The FIRST write of a file (SessionWrites false) still gets V3 —
// that is where V3's generation adds value.
func isActiveDebugIteration(ctx *AgentContext, path string) bool {
	if ctx == nil || !ctx.SessionWrites[path] {
		return false
	}
	base := filepath.Base(path)
	for i := len(ctx.Messages) - 1; i >= 0; i-- {
		m := ctx.Messages[i]
		if m.Role != "tool" {
			continue
		}
		// Only the MOST RECENT tool action counts: if the model's last
		// step was a read or an edit rather than a failing run, it isn't
		// mid-test-fix on this file.
		if m.ToolName != "run_command" && m.ToolName != "run_background" {
			return false
		}
		return strings.Contains(m.Content, `"success":false`) &&
			mentionsFilename(m.Content, base)
	}
	return false
}

// mentionsFilename reports whether `base` appears in text as a whole
// filename token, not as a substring of a longer name (#147 review finding
// #12: a.py must not match data.py, main.py must not match domain.py). The
// character on each side of a real occurrence must not be a filename
// character (letter, digit, _, ., -), so `webapp.py` and `app.python` don't
// count while `./app.py`, `"app.py"`, and ` app.py:12` do.
func mentionsFilename(text, base string) bool {
	if base == "" {
		return false
	}
	isNameChar := func(b byte) bool {
		return b == '_' || b == '.' || b == '-' ||
			(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}
	for i := 0; ; {
		j := strings.Index(text[i:], base)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(base)
		leftOK := start == 0 || !isNameChar(text[start-1])
		rightOK := end == len(text) || !isNameChar(text[end])
		if leftOK && rightOK {
			return true
		}
		i = start + 1
	}
}

// fileIsSourceCode reports whether a path holds code a request might ask to
// change, as opposed to data or markup a program consumes. Reuses the
// registry's own Executable flag so this cannot drift from it.
func fileIsSourceCode(path string) bool {
	meta, known := syntaxGateLanguages[strings.ToLower(filepath.Ext(path))]
	return known && meta.Executable
}

// writeNewFileWithWarning lands syntactically broken content in a file that
// does not exist yet, and says so, instead of rejecting the write.
//
// The rejection was the harness substituting its judgement for execution's.
// On an EXISTING file it earns its keep: it stops working code being
// clobbered with garbage. On a new file there is nothing to protect, and the
// rejection forbids the one loop that measurably works — write it, run it,
// read the real traceback, fix it. The no-tool baseline arm, whose only
// feedback IS the traceback, resolves its own syntax errors at 85-100%.
// Under the rejection the model retried blind against our error message,
// resent byte-identical content, and the repetition breaker ended the
// session: three AoC sessions and a novel-arm session all died as
// "solve.py was never created" — code on hand, nothing on disk.
func writeNewFileWithWarning(path, inputPath, content, synErr string, ctx *AgentContext) (*ToolResult, error) {
	logMandatoryChecks(ctx, "write_file", inputPath, "syntax failed, landing with a warning")
	res, err := writeFileRecorded(path, content, ctx)
	if err != nil || res == nil || !res.Success {
		return res, err
	}
	// Keyed on the INPUT path: every SessionWrites reader and writer uses the
	// path as the model sent it (input.Path / wfInput.Path). This helper
	// briefly keyed on the resolved path, so the session-owned carveout
	// missed and the model's own CORRECTION of a warned write was rejected
	// as an overwrite of an unread existing file — fail-forward landed the
	// broken file and then forbade fixing it. Measured: every early novel-arm
	// session hit "rejecting write_file over unread existing solve.py"
	// immediately after a warned write.
	ctx.SessionWrites[inputPath] = true
	out := WriteFileOutput{
		BytesWritten: len(content),
		Warning: fmt.Sprintf(
			"written, but it does not parse (%s).%s Run it now and read the "+
				"real traceback, then fix that line and write it again.",
			truncateStr(synErr, 160), offendingLineNote(content, synErr)),
	}
	outBytes, _ := json.Marshal(out)
	res.Data = outBytes
	// The syntax check ran on exactly `content`, and writeFileDirect wrote
	// exactly `content`, so the failure describes the bytes on disk. Applied
	// and failed are orthogonal here on purpose: the file landed AND it does
	// not parse, which is the documented debugging policy.
	res.MutationStatus = MutationApplied
	res.ValidationKind = ValidationKindSyntax
	res.ValidationStatus = ValidationFailed
	res.ValidationDetail = synErr
	return res, nil
}

// preflightWarnedWrite lands a warned write for the V3 preflight and keeps the
// observation that justified the warning attached to whichever outcome the
// filesystem produced.
//
// writeNewFileWithWarning already states applied + syntax/failed when the bytes
// land. When they do not, the writer reports not_run, which is honest for a
// layer that ran no check of its own -- but this route did run one, on exactly
// these bytes, and a failed mutation does not unmake that observation.
func preflightWarnedWrite(path, inputPath, content string, o checkOutcome, ctx *AgentContext) (*ToolResult, error) {
	res, err := writeNewFileWithWarning(path, inputPath, content, o.Detail, ctx)
	return applyRouteObservation(res, err, o)
}

// writeFileRecorded is writeFileDirect plus the body-seen record.
//
// Content the model authored is content it has seen, and that has to hold
// whichever write path ran; otherwise the evidence gate fires on files the
// model has just created. Every write path reaches writeFileDirect through
// this one function, so a new path cannot skip the record.
func writeFileRecorded(path, content string, ctx *AgentContext) (*ToolResult, error) {
	res, err := writeFileDirect(path, content)
	if err == nil && res != nil && res.Success && ctx != nil {
		ctx.RecordBodySeen(path)
	}
	return res, err
}

// classifiedError carries mutation/validation facts alongside a real error,
// so the site that OWNS the evidence can state them without changing the
// (nil, err) contract callers branch on. The V3 route and every other caller
// still receive a non-nil error and take their existing error paths; only the
// outward ToolResult synthesised at the boundary is enriched.
type classifiedError struct {
	err              error
	mutationStatus   MutationStatus
	validationKind   ValidationKind
	validationStatus ValidationStatus
}

func (c *classifiedError) Error() string { return c.err.Error() }

func (c *classifiedError) Unwrap() error { return c.err }

// failedMutation tags a filesystem failure: a mutation was attempted and did
// not establish the intended state. Validation reports honestly that no check
// passed -- not_run for recognised code, not_applicable otherwise -- because
// writeFileDirect runs no check of its own.
func failedMutation(path string, err error) error {
	kind, status := ValidationKindNone, ValidationNotApplicable
	if _, gated := syntaxGateLanguages[strings.ToLower(filepath.Ext(path))]; gated {
		kind, status = ValidationKindSyntax, ValidationNotRun
	}
	return &classifiedError{err: err, mutationStatus: MutationFailed,
		validationKind: kind, validationStatus: status}
}

// overlayValidation replaces ONLY the validation fields of a result with an
// observation the caller obtained upstream. writeFileDirect's conservative
// default (syntax/not_run for recognised code) is right when nobody checked;
// a route that DID check owns the stronger answer and overlays it here.
//
// Mutation facts and Success are never touched, and nothing is inferred from
// Success, from the extension, or from a wrapper's ok boolean. Unknown is
// preserved rather than normalised, so the result stays visibly unclassified
// and a sentinel can catch the producer defect.
func overlayValidation(res *ToolResult, o checkOutcome) {
	if res == nil {
		return
	}
	res.ValidationStatus = o.Status
	res.ValidationDetail = o.Detail
	switch o.Status {
	case ValidationNotApplicable:
		res.ValidationKind = ValidationKindNone
	case ValidationUnknown:
		res.ValidationKind = ValidationKindUnknown
	default:
		res.ValidationKind = ValidationKindSyntax
	}
}

// overlayValidationOnError does the same for a classified filesystem error:
// the mutation still failed and the error is still returned non-nil, but the
// validation observation made on those exact proposed bytes is preserved.
// Mutation failure and validation success are orthogonal -- checked bytes can
// still fail to land. An untyped error is returned unchanged, so the overlay
// never manufactures a classification no producer made.
func overlayValidationOnError(err error, o checkOutcome) error {
	var ce *classifiedError
	if !errors.As(err, &ce) {
		return err
	}
	ce.validationStatus = o.Status
	switch o.Status {
	case ValidationNotApplicable:
		ce.validationKind = ValidationKindNone
	case ValidationUnknown:
		ce.validationKind = ValidationKindUnknown
	default:
		ce.validationKind = ValidationKindSyntax
	}
	return err
}

// applyRouteObservation is how a route that owns an observation attaches it,
// on both outcomes of the mutation attempt. Success and failure take the same
// observation because they are orthogonal facts: checked bytes can still fail
// to land, and bytes that landed can still be the ones that do not parse.
// Keeping it in one place is what makes two routes provably identical here.
func applyRouteObservation(res *ToolResult, err error, o checkOutcome) (*ToolResult, error) {
	if err != nil {
		return res, overlayValidationOnError(err, o)
	}
	overlayValidation(res, o)
	return res, nil
}

// atomicTempSuffix names the temp file every atomic write renames over its target.
const atomicTempSuffix = ".atlas.tmp"

// atomicReplaceFile is the write-then-rename of the mutating tools as one
// primitive (edit_file and structural_edit still inline it). A reader of the
// target either sees the old bytes or the new ones, never a partial file, and
// a failed rename leaves the target untouched with the temp file cleaned up.
func atomicReplaceFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("cannot create parent dir for %s: %w", path, err)
	}
	tmpPath := path + atomicTempSuffix
	if err := os.WriteFile(tmpPath, content, 0644); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("cannot rename temp file: %w", err)
	}
	return nil
}

// writeFileDirect writes content to disk atomically (temp file plus rename, see
// atomicReplaceFile). This is where any write_file tool call ultimately lands.
func writeFileDirect(path, content string) (*ToolResult, error) {
	if err := atomicReplaceFile(path, []byte(content)); err != nil {
		return nil, failedMutation(path, err)
	}
	out := WriteFileOutput{BytesWritten: len(content)}
	outBytes, _ := json.Marshal(out)
	// Slice 1 classification. The bytes are on disk after the rename, so the
	// mutation is demonstrated rather than intended.
	//
	// Validation is deliberately NOT claimed here. This layer performs no
	// syntax check, so for recognized code the honest answer is not_run: a
	// caller that did validate these exact bytes upgrades it, and one that
	// did not leaves the truth visible. Claiming passed because a write
	// succeeded is the conflation this whole contract exists to remove.
	kind, status := ValidationKindNone, ValidationNotApplicable
	if _, gated := syntaxGateLanguages[strings.ToLower(filepath.Ext(path))]; gated {
		kind, status = ValidationKindSyntax, ValidationNotRun
	}
	return &ToolResult{
		Success: true, Data: outBytes,
		MutationStatus: MutationApplied,
		ValidationKind: kind, ValidationStatus: status,
	}, nil
}
