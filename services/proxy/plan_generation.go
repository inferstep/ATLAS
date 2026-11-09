// Plan generation: whether a request earns a plan, and the call that asks V3 for one.

package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/itigges22/atlas-proxy/internal/projectcontext"
	"github.com/itigges22/atlas-proxy/internal/v3client"
	"github.com/itigges22/atlas-proxy/internal/wire"
)

// shouldGeneratePlan decides whether a turn warrants the ~5-15s plan
// pipeline cost. We skip plans for:
//   - T0 (trivial chat — "hi", "thanks") where a plan is wasted budget
//   - explicit follow-up / clarification requests that depend on the
//     prior turn's plan, which we'd just regenerate identically
//
// Everything else gets a plan — we'd rather plan and have the model
// ignore it than not plan and let the model thrash.
func shouldGeneratePlan(ctx *AgentContext, message string) bool {
	if ctx.Tier == Tier0Conversational {
		return false
	}
	// Single-line ack-style messages where the user is just steering
	// the existing direction ("yes do that", "looks good", "try again")
	// — already-running plan is still relevant; a fresh one would just
	// re-derive it.
	trimmed := strings.ToLower(strings.TrimSpace(message))
	if len(trimmed) < 12 {
		return false
	}
	// A request to LOOK needs no plan, and a wrong one actively steers: "Yes,
	// please list the files" could get a plan that edits what it was only asked
	// to enumerate.
	return !isReadOnlyRequest(message)
}

// generatePlan hits /v3/plan with a sampled project context and the
// user's message, streaming plan_* stage events out to the TUI as
// `v3_plan` events. Returns the winning Plan or nil if the planner
// errored — callers should treat nil as "no plan, proceed without
// adherence gating".
func generatePlan(ctx *AgentContext, userMessage string) *wire.Plan {
	if ctx.V3URL == "" {
		return nil
	}
	pctx := projectcontext.SampleWithin(ctx.WorkingDir, 6, 2000, scanDeadline())
	req := wire.V3PlanRequest{
		UserMessage:    userMessage,
		WorkingDir:     ctx.WorkingDir,
		ProjectContext: pctx,
		ExistingFiles:  listWorkspaceFiles(ctx.WorkingDir, 400),
		NCandidates:    3,
	}

	planStart := time.Now()
	Emit(wire.NewEnvelope(wire.EvtStageStart, planStage, map[string]interface{}{
		"detail":     fmt.Sprintf("planning: %s", truncateStr(userMessage, 60)),
		"context_n":  len(pctx),
		"candidates": req.NCandidates,
	}))

	plan, err := v3client.PlanStreaming(ctx.Ctx, ctx.V3URL, requestIDOf(ctx), req, func(stage, detail string, data map[string]interface{}) {
		// Filter out per-token events: forwarding each one to the TUI as a
		// separate v3_plan row clogs the pipeline pane. The structural plan stages
		// (plan_candidate, plan_candidate_scored, plan_selected) are
		// what the renderer actually wants — token-level visibility is
		// debug noise.
		switch stage {
		case "token", "llm_start", "llm_end":
			return
		}
		payload := map[string]interface{}{"stage": stage, "detail": detail}
		for k, v := range data {
			payload[k] = v
		}
		ctx.Stream("v3_plan", payload)
		// Mirror to the typed broker so non-TUI consumers (logs, audit)
		// see the same stream.
		Emit(wire.NewEnvelope(wire.EvtMetric, "v3:plan:"+stage, payload))
	})
	dur := time.Since(planStart).Milliseconds()

	if err != nil {
		log.Printf("[agent] plan generation failed: %v", err)
		Emit(wire.Envelope{
			EventID:    wire.NewEventID(),
			Timestamp:  float64(time.Now().UnixNano()) / 1e9,
			Type:       wire.EvtStageEnd,
			Stage:      planStage,
			DurationMS: dur,
			Payload:    map[string]interface{}{"success": false, "error": err.Error()},
		})
		return nil
	}

	Emit(wire.Envelope{
		EventID:    wire.NewEventID(),
		Timestamp:  float64(time.Now().UnixNano()) / 1e9,
		Type:       wire.EvtStageEnd,
		Stage:      planStage,
		DurationMS: dur,
		Payload: map[string]interface{}{
			"success":           true,
			"steps":             len(plan.Steps),
			"verify_step":       plan.VerifyStep,
			"winning_score":     plan.WinningScore,
			"candidates_tested": plan.CandidatesTested,
		},
	})

	// Stream the full plan structure so the TUI / IDE plugins can
	// render the step list. Per-stage events (plan_start, plan_selected,
	// etc.) only carry counts and indices — the actual step rows live
	// here. One event per plan: subsequent step satisfaction goes
	// through plan_adherence, and a revision fires another plan_loaded.
	planPayload := map[string]interface{}{
		"steps":         plan.Steps,
		"verify_step":   plan.VerifyStep,
		"rationale":     plan.Rationale,
		"winning_score": plan.WinningScore,
		"revision":      0,
	}
	ctx.Stream("plan_loaded", planPayload)
	Emit(wire.NewEnvelope(wire.EvtMetric, "v3:plan:loaded", planPayload))

	return plan
}

// planStage names the planner on the event stream.
const planStage = "v3:plan"
