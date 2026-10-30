// Plan gates: how the agent loop follows the pre-flight plan.
//
//	Plan adherence — matches each tool call against the pre-flight plan and
//	  counts the off-plan streak, regenerating the plan once the streak runs
//	  long. Advisory: it never blocks a call.
//	Plan-progress reminder — renders the compact step-progress block
//	  injected ahead of each LLM call so a long multi-file task doesn't
//	  lose track of what's left.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// ---------------------------------------------------------------------------
// Plan adherence — track tool calls against the pre-flight plan
// ---------------------------------------------------------------------------
//
// Adherence is advisory by default: we record which planned steps a tool
// call satisfies and emit metric events, but we don't block the model.
// Hard-blocking off-plan calls would be brittle when the plan was
// suboptimal — the model often discovers correct work the planner
// missed.
//
// What we DO actively do: count the off-plan streak, and once it
// crosses planAutoReviseThreshold we regenerate the plan with whatever
// context the agent has discovered so far. That's the "plan_revise
// escape" — the agent doesn't have to know about a plan_revise tool;
// the loop notices the divergence and re-plans for it.
//
// Adherence rules:
//   - A tool call satisfies the FIRST unsatisfied plan step whose
//     action verb matches the tool name (read_file ↔ "read_file" or
//     "read", run_command ↔ "run_command" or "run").
//   - If the planned step has a target, we additionally require the
//     tool's path/command to mention that target. Loose substring
//     match — paths normalize to basename so /workspace/app.py and
//     ./app.py both match a step targeting "app.py".
//   - Steps are matched in order (first unsatisfied wins) so the
//     model can revisit a planned action without re-satisfying earlier
//     steps. Out-of-order is fine; off-plan is what we count.

const (
	// planAutoReviseThreshold is the number of consecutive off-plan
	// tool calls before we auto-revise the plan. Bumped 3→5 alongside
	// the recon-tool neutrality fix below: even with recon excluded,
	// 3 was firing on routine exploration patterns (the May 6 session
	// hit it twice on read_file/list_directory chains for templates
	// the model was hunting). 5 unmatched non-recon calls is a real
	// off-plan signal; 3 was thrashing on normal agent behavior.
	planAutoReviseThreshold = 5

	// planMaxRevisions caps how many times we'll regenerate per loop.
	// After this we give up and run plan-free for the remainder.
	planMaxRevisions = 2
)

// isReconTool returns true for tools that gather information without
// taking action. These calls are neutral for plan adherence — they
// neither satisfy plan steps (a plan rarely lists "read_file app.py"
// as a step) nor count as off-plan (recon between planned actions is
// expected and shouldn't burn the off-streak counter).
//
// Without this, the agent's natural "look around before changing
// anything" pattern triggered plan revisions purely from exploratory
// reads — visible in the May 6 session as 2 revisions fired purely
// from read_file/list_directory chains.
func isReconTool(name string) bool {
	switch name {
	case "read_file", "list_directory", "find_file", "search_files":
		return true
	}
	return false
}

// matchPlanStep returns the index of the first unsatisfied plan step
// that the tool call (toolName, args) satisfies, or -1 if no match.
// satisfied must be the same length as plan.Steps.
func matchPlanStep(plan *Plan, satisfied []bool, toolName string, args json.RawMessage) int {
	if plan == nil || len(plan.Steps) == 0 {
		return -1
	}
	if len(satisfied) != len(plan.Steps) {
		return -1
	}
	target := extractToolTarget(toolName, args)
	for i, step := range plan.Steps {
		if satisfied[i] {
			continue
		}
		if !actionMatchesTool(step.Action, toolName) {
			continue
		}
		// Target match is advisory — if the step has no target field
		// or the tool args don't carry an obvious target, the action
		// match alone is enough.
		if step.Target != "" && target != "" {
			if !targetsOverlap(step.Target, target) {
				continue
			}
		}
		return i
	}
	return -1
}

// verifyStepSatisfiedBy returns the index of the plan's unsatisfied verify
// step when command verifies (it probes a service or runs the program, as
// classifyCommandEvidence judges it) and runs the same program as the step's
// planned command; -1 otherwise.
func verifyStepSatisfiedBy(plan *Plan, satisfied []bool, command string) int {
	if plan == nil || plan.VerifyStep == "" || len(satisfied) != len(plan.Steps) {
		return -1
	}
	if !classifyCommandEvidence(command).Kind.verifies() {
		return -1
	}
	for i, step := range plan.Steps {
		if step.ID != plan.VerifyStep || satisfied[i] || !actionMatchesTool(step.Action, "run_command") {
			continue
		}
		if p := commandProgram(step.Target); p != "" && p == commandProgram(command) {
			return i
		}
	}
	return -1
}

// actionMatchesTool reports whether step.Action describes the same
// operation as a tool call named toolName. We check both directions
// (action→tool and tool→action) and normalize underscores so plans
// written as "read file" or "read_file" both match read_file.
func actionMatchesTool(action, toolName string) bool {
	if action == "" || toolName == "" {
		return false
	}
	a := strings.ToLower(strings.ReplaceAll(action, "_", " "))
	t := strings.ToLower(strings.ReplaceAll(toolName, "_", " "))
	if a == t || strings.Contains(a, t) {
		return true
	}
	// Also allow the verb stem ("read" matches "read_file" tool).
	verb := strings.SplitN(t, " ", 2)[0]
	if verb != "" && strings.HasPrefix(a, verb) {
		return true
	}
	// The edit tools are one family. A plan step that says "edit_file
	// stats.py" is satisfied by editing stats.py, whichever edit tool did it
	// — the step describes the change, not the spelling of the call.
	//
	// Measured: add_function planned `s1: edit_file`, added the function with
	// insert_after, and was told at the exit that s1 had not landed. The
	// statement was false: the file had been edited, the test passed, and the
	// session spent its three plan-gate bounces arguing about it.
	//
	// This does not loosen the gate. Family membership only decides the ACTION
	// half; the target half still has to agree, and it now can, because these
	// tools finally report their path above. An insert_after on some other
	// file satisfies nothing.
	if editToolFamily[toolName] && editToolFamily[strings.ReplaceAll(a, " ", "_")] {
		return true
	}
	return false
}

// editToolFamily is the set of tools that change part of an existing file.
//
// write_file is deliberately NOT a member. Replacing a whole file is a
// different act from editing one, and TestActionMatchesTool has pinned
// "write_file" against the edit_file tool since before this: a plan that says
// to rewrite a file is not satisfied by a one-line edit, or the other way
// round. That contract is untouched here.
var editToolFamily = map[string]bool{
	"edit_file": true, "structural_edit": true,
	"insert_after": true, "replace_lines": true,
}

// targetsOverlap reports whether two paths/targets refer to the same
// thing. For paths: equality or path-suffix match (so
// "templates/index.html" matches "/workspace/templates/index.html").
// For commands (anything with a space or non-path char): loose
// substring match so "curl http://localhost:5000/" matches a plan
// target of "curl http://localhost:5000/hello".
//
// Path-shaped strings require a path-component boundary: without it,
// "app.py" would match "tests/test_app.py" and reads of the test file
// would tick off the source-file plan step.
func targetsOverlap(planTarget, toolTarget string) bool {
	a := strings.ToLower(strings.TrimSpace(planTarget))
	b := strings.ToLower(strings.TrimSpace(toolTarget))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	a = strings.TrimPrefix(a, "./")
	b = strings.TrimPrefix(b, "./")
	if a == b {
		return true
	}
	// Path-suffix match: basename or last-N-components alignment.
	if strings.HasSuffix(b, "/"+a) || strings.HasSuffix(a, "/"+b) {
		return true
	}
	// Heuristic: anything with a space looks like a command rather
	// than a filename. Allow substring there so partial command
	// matches still count.
	if strings.ContainsAny(a, " \t") || strings.ContainsAny(b, " \t") {
		return strings.Contains(a, b) || strings.Contains(b, a)
	}
	return false
}

// extractToolTarget returns the most useful "target" string for a
// tool call: file path for file tools, command string for run_command,
// path for list_directory. Empty when the tool has no clear target
// (e.g. plan_revise itself).
func extractToolTarget(toolName string, args json.RawMessage) string {
	switch toolName {
	case "write_file":
		var x WriteFileInput
		if json.Unmarshal(args, &x) == nil {
			return x.Path
		}
	case "read_file", "delete_file", "list_directory",
		"edit_file", "structural_edit", "insert_after", "replace_lines":
		// Every edit tool takes `path` (read_file, delete_file and
		// list_directory take it too). Only edit_file was listed, so a step
		// satisfied by structural_edit/insert_after/replace_lines produced no
		// target at all, and the target check was skipped for exactly the
		// tools whose file it most needed to know.
		var x struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(args, &x) == nil {
			return x.Path
		}
	case "run_command":
		var x RunCommandInput
		if json.Unmarshal(args, &x) == nil {
			return x.Command
		}
	}
	return ""
}

// recordPlanAdherence is called from the agent loop after each
// tool-call dispatch. It updates ctx.PlanStepsSatisfied and
// ctx.PlanOffStreak, emits a "plan_adherence" metric, and returns
// true if the off-streak crossed the auto-revise threshold (caller
// should regenerate the plan).
func recordPlanAdherence(ctx *AgentContext, toolName string, args json.RawMessage, success bool) bool {
	if ctx.Plan == nil {
		return false
	}
	if ctx.PlanStepsSatisfied == nil {
		ctx.PlanStepsSatisfied = make([]bool, len(ctx.Plan.Steps))
	}

	idx := matchPlanStep(ctx.Plan, ctx.PlanStepsSatisfied, toolName, args)

	// A write that landed with an unresolved parse warning has not finished
	// its step, whatever the tool result says.
	//
	// Measured (family P, cycles 9 and 10). `write_file app.py` landed at
	// t=41 s carrying "does not parse (SyntaxError: unmatched ')')" — a
	// successful tool call, so s1 ticked over. From that turn on every request
	// carried "plan progress 4/5 … Done: s1 … Stay on the current step; don't
	// jump ahead and don't re-explore finished work", while app.py sat broken
	// and never run. The run spent the rest of its budget elsewhere and
	// delivered that file unchanged. The step is the work, not the call.
	if idx >= 0 && success && writeLikePlanAction(toolName) {
		if path := extractToolTarget(toolName, args); path != "" && currentValidationDetail(ctx, path) != "" {
			log.Printf("[agent] plan step %q not satisfied: %s is on disk with a failed check",
				ctx.Plan.Steps[idx].ID, logPath(path))
			success = false
		}
	}
	// The plan's verify step is satisfied by a passing verification of the
	// same program, whatever its arguments. The planner names that command by
	// guess: the smoke run of 2026-09-27 planned `curl http://127.0.0.1:5000`,
	// the app served on 5001, and the model's passing `curl -sf
	// http://127.0.0.1:5001/` did not count. The plan gate then demanded the
	// literal step after the server had been stopped, and a finished, verified
	// task ended "stopped".
	if idx < 0 && success && toolName == "run_command" {
		idx = verifyStepSatisfiedBy(ctx.Plan, ctx.PlanStepsSatisfied, extractToolTarget(toolName, args))
	}
	// Only successful tool calls count toward step satisfaction.
	// A failed run_command shouldn't tick off the verify_step.
	if idx >= 0 && success {
		ctx.PlanStepsSatisfied[idx] = true
		ctx.PlanOffStreak = 0
		ctx.Stream("plan_adherence", map[string]interface{}{
			"matched":     true,
			"step_index":  idx,
			"step_id":     ctx.Plan.Steps[idx].ID,
			"step_action": ctx.Plan.Steps[idx].Action,
			"satisfied":   countTrue(ctx.PlanStepsSatisfied),
			"total":       len(ctx.PlanStepsSatisfied),
		})
		return false
	}

	// Recon tools (read_file / list_directory / find_file / search_files)
	// are neutral: they don't satisfy steps but they don't extend the
	// off-streak either. The agent's natural exploration pattern
	// shouldn't trigger plan revisions.
	if isReconTool(toolName) {
		ctx.Stream("plan_adherence", map[string]interface{}{
			"matched":    false,
			"neutral":    true,
			"tool":       toolName,
			"off_streak": ctx.PlanOffStreak, // unchanged
			"satisfied":  countTrue(ctx.PlanStepsSatisfied),
			"total":      len(ctx.PlanStepsSatisfied),
		})
		return false
	}

	// No matching step (or the call failed) — extend the off-streak.
	ctx.PlanOffStreak++
	ctx.Stream("plan_adherence", map[string]interface{}{
		"matched":    false,
		"tool":       toolName,
		"off_streak": ctx.PlanOffStreak,
		"satisfied":  countTrue(ctx.PlanStepsSatisfied),
		"total":      len(ctx.PlanStepsSatisfied),
	})

	// Threshold check — caller should auto-revise when this returns
	// true. We also cap at planMaxRevisions so a chronically
	// off-plan run doesn't loop forever calling /v3/plan.
	if ctx.PlanOffStreak >= planAutoReviseThreshold && ctx.PlanRevisions < planMaxRevisions {
		return true
	}
	return false
}

// samePlanSteps reports whether two plans describe the same work. Compares
// action+target per step, not prose: a planner that rewords a rationale while
// producing the same steps has still produced the same plan.
func samePlanSteps(a, b *Plan) bool {
	if a == nil || b == nil || len(a.Steps) != len(b.Steps) {
		return false
	}
	for i := range a.Steps {
		if a.Steps[i].Action != b.Steps[i].Action ||
			a.Steps[i].Target != b.Steps[i].Target {
			return false
		}
	}
	return true
}

// revisePlan regenerates the plan with whatever the agent has
// discovered since the original plan was made. The user message
// passed in is the ORIGINAL one (the goal hasn't changed); we
// suffix a short note explaining why we're re-planning so the
// planner can adjust shape.
func revisePlan(ctx *AgentContext, originalUserMessage string, reason string) {
	if ctx.Plan == nil || ctx.PlanRevisions >= planMaxRevisions {
		return
	}
	// Compose a revision-aware user message. The planner prompt is
	// goal-oriented, so we keep the user's original goal verbatim
	// and append a "what we learned" note. This lets the planner
	// re-shape the plan around the new info rather than starting
	// from zero.
	noted := originalUserMessage
	if reason != "" {
		noted = fmt.Sprintf("%s\n\n[Re-planning context: %s]", originalUserMessage, reason)
	}
	log.Printf("[agent] revising plan (revision %d/%d): %s",
		ctx.PlanRevisions+1, planMaxRevisions, reason)
	ctx.Stream("plan_revise", map[string]interface{}{
		"reason":   reason,
		"revision": ctx.PlanRevisions + 1,
	})

	// Carry forward what the agent has read so far — it's the most
	// concrete signal of "what the agent knows now" beyond the
	// original priority-files sample.
	pctx := samplePlanContext(ctx.WorkingDir, 6, 2000)
	addFilesReadToPlanContext(ctx, pctx, 8)

	req := V3PlanRequest{
		UserMessage:    noted,
		WorkingDir:     ctx.WorkingDir,
		ProjectContext: pctx,
		NCandidates:    3,
	}
	plan, err := callV3PlanStreaming(ctx.Ctx, ctx.V3URL, req, func(stage, detail string, data map[string]interface{}) {
		switch stage {
		case "token", "llm_start", "llm_end":
			return
		}
		payload := map[string]interface{}{"stage": stage, "detail": detail, "revision": ctx.PlanRevisions + 1}
		for k, v := range data {
			payload[k] = v
		}
		ctx.Stream("v3_plan", payload)
	})
	ctx.PlanRevisions++
	if err != nil || plan == nil {
		log.Printf("[agent] plan revision failed: %v — continuing with previous plan", err)
		return
	}
	// A revision that comes back identical is not a revision. Observed live:
	// the model went off-plan for five calls, the gate re-planned, and the
	// planner returned the same three steps — so the streak reset and the
	// same plan kept bouncing it. When re-planning cannot produce anything
	// different, the plan is not what is wrong: the model cannot execute it.
	// Dropping it beats gating the rest of the turn on a plan already proven
	// unfollowable.
	if samePlanSteps(ctx.Plan, plan) {
		log.Printf("[agent] plan revision returned the same steps — dropping the plan")
		ctx.Plan = nil
		ctx.PlanStepsSatisfied = nil
		ctx.PlanOffStreak = 0
		ctx.Stream("plan_revise", map[string]interface{}{
			"reason":   "revision produced the same steps; continuing without a plan",
			"revision": ctx.PlanRevisions,
			"dropped":  true,
		})
		return
	}

	ctx.Plan = plan
	ctx.PlanStepsSatisfied = make([]bool, len(plan.Steps))
	ctx.PlanOffStreak = 0

	// Re-emit the full plan structure so renderers replace the
	// previous plan view with the revised one. Same shape as the
	// initial generatePlan emission so consumers can use one code
	// path for both.
	planPayload := map[string]interface{}{
		"steps":         plan.Steps,
		"verify_step":   plan.VerifyStep,
		"rationale":     plan.Rationale,
		"winning_score": plan.WinningScore,
		"revision":      ctx.PlanRevisions,
	}
	ctx.Stream("plan_loaded", planPayload)
}

func countTrue(bs []bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// Plan-progress reminder injection. May 10 2026.
//
// Long multi-file tasks (e.g. "redo all 10 templates to match a SaaS
// design") lose sight of the plan once conversation trimming kicks in.
// The plan is generated up front via /v3/plan and stashed on ctx.Plan,
// and PlanStepsSatisfied tracks which steps have been hit — but neither
// surfaces back to the model after the original plan-rendering message
// drops out of the trim window.
//
// Fix: at the START of each LLM call we render a compact plan-progress
// "[system note]: ..." line and prepend it to the messages slice
// passed to callLLMOnce. The note is EPHEMERAL — it's not appended to
// ctx.Messages, so it doesn't accumulate or get re-trimmed. Every
// turn, the model sees: "step 3 of 7 — currently working on edit
// templates/dashboard.html; done: index.html, contact.html; remaining:
// pricing.html, services.html, ...".
//
// Cost: ~150 chars per turn. Cheap compared to letting the model
// re-read all the templates to remember what's done.

// buildPlanReminder returns a one-line "[system note]" string with
// plan progress, or "" if no plan is active. The caller prepends this
// to the messages slice passed to a single LLM call — it's not added
// to ctx.Messages, so it doesn't bloat history.
func buildPlanReminder(ctx *AgentContext) string {
	if ctx.Plan == nil || len(ctx.Plan.Steps) == 0 {
		return ""
	}
	if ctx.PlanStepsSatisfied == nil {
		ctx.PlanStepsSatisfied = make([]bool, len(ctx.Plan.Steps))
	}

	total := len(ctx.Plan.Steps)
	doneCount := 0
	doneIDs := make([]string, 0, total)
	remainingIDs := make([]string, 0, total)
	var current *PlanStep

	for i := range ctx.Plan.Steps {
		step := &ctx.Plan.Steps[i]
		if i < len(ctx.PlanStepsSatisfied) && ctx.PlanStepsSatisfied[i] {
			doneCount++
			doneIDs = append(doneIDs, step.ID)
		} else {
			if current == nil {
				current = step
			}
			remainingIDs = append(remainingIDs, step.ID)
		}
	}

	if current == nil {
		// All steps satisfied — the model should be on the verify step
		// or about to emit done. Surface that explicitly.
		return fmt.Sprintf(
			"[system note]: plan complete (%d/%d steps satisfied). Verify your work via `%s` if you haven't already, then emit `done` with a summary of what landed.",
			doneCount, total, planVerifyHint(ctx.Plan))
	}

	doneFrag := "none yet"
	if len(doneIDs) > 0 {
		doneFrag = strings.Join(doneIDs, ", ")
	}
	return fmt.Sprintf(
		"[system note]: plan progress %d/%d — currently on step %q (%s %s). Done: %s. Remaining: %s. Stay on the current step until it's complete; don't jump ahead and don't re-explore finished work.",
		doneCount, total, current.ID, current.Action, current.Target,
		doneFrag, strings.Join(remainingIDs, ", "),
	)
}

func planVerifyHint(p *Plan) string {
	if p == nil || p.VerifyStep == "" {
		return "the appropriate test/curl/run command"
	}
	return p.VerifyStep
}

// writeLikePlanAction reports the plan actions that put bytes on disk, the
// only ones whose step can be left unfinished by a failing check.
func writeLikePlanAction(tool string) bool {
	switch tool {
	case "write_file", "edit_file", "structural_edit", "insert_after", "replace_lines":
		return true
	}
	return false
}
