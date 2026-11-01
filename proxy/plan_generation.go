// Plan generation: whether a request earns a plan, and the sample of the project the planner is shown.

package main

import (
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
	"time"
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
func generatePlan(ctx *AgentContext, userMessage string) *Plan {
	if ctx.V3URL == "" {
		return nil
	}
	pctx := samplePlanContext(ctx.WorkingDir, 6, 2000)
	req := V3PlanRequest{
		UserMessage:    userMessage,
		WorkingDir:     ctx.WorkingDir,
		ProjectContext: pctx,
		ExistingFiles:  listWorkspaceFiles(ctx.WorkingDir, 400),
		NCandidates:    3,
	}

	planStart := time.Now()
	Emit(NewEnvelope(EvtStageStart, planStage, map[string]interface{}{
		"detail":     fmt.Sprintf("planning: %s", truncateStr(userMessage, 60)),
		"context_n":  len(pctx),
		"candidates": req.NCandidates,
	}))

	plan, err := callV3PlanStreaming(ctx.Ctx, ctx.V3URL, req, func(stage, detail string, data map[string]interface{}) {
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
		Emit(NewEnvelope(EvtMetric, "v3:plan:"+stage, payload))
	})
	dur := time.Since(planStart).Milliseconds()

	if err != nil {
		log.Printf("[agent] plan generation failed: %v", err)
		Emit(Envelope{
			EventID:    NewEventID(),
			Timestamp:  float64(time.Now().UnixNano()) / 1e9,
			Type:       EvtStageEnd,
			Stage:      planStage,
			DurationMS: dur,
			Payload:    map[string]interface{}{"success": false, "error": err.Error()},
		})
		return nil
	}

	Emit(Envelope{
		EventID:    NewEventID(),
		Timestamp:  float64(time.Now().UnixNano()) / 1e9,
		Type:       EvtStageEnd,
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
	Emit(NewEnvelope(EvtMetric, "v3:plan:loaded", planPayload))

	return plan
}

// samplePlanContext walks ctx.WorkingDir and reads a handful of files
// the planner is most likely to need: source files, templates,
// manifests. Limited to maxFiles per call, each truncated to maxBytes.
//
// The planner runs *before* any tool calls have happened in the loop,
// so ctx.FilesRead is empty — without this, plans for "fix the flask
// app" would have no signal about what's in app.py and would generate
// generic 5-step recipes. We pay one fs walk + a few small reads up
// front; the budget is small (~5 files × 2KB) and the planning quality
// jump is large.
func samplePlanContext(workingDir string, maxFiles, maxBytes int) map[string]string {
	if workingDir == "" {
		return nil
	}
	out := map[string]string{}
	// Files we always inline if present — most projects have at least
	// one of these and they describe shape (deps, entry point).
	priority := []string{
		"app.py", "main.py", "manage.py", "wsgi.py",
		"index.html", "templates/index.html", "templates/base.html",
		"package.json", "tsconfig.json", "vite.config.ts", "vite.config.js",
		"go.mod", "main.go",
		"Cargo.toml", "src/main.rs", "src/lib.rs",
		"requirements.txt", "pyproject.toml", "setup.py",
		"README.md",
	}
	// Every read goes through the workspace folder, so a name in it that
	// points somewhere else is skipped like a file that cannot be read.
	dir := openConfinedDir(workingDir)
	defer dir.Close()
	sample := planSample{dir: dir, out: out, maxFiles: maxFiles, maxBytes: maxBytes}
	sample.readPriorityFiles("", priority)
	// If priority files yielded nothing at the workspace root, the
	// project may live one level down -- common when the user's
	// `atlas tui` cwd was the parent dir (e.g. /workspace) and the app
	// is in a subdirectory. Walk one level looking for the SAME
	// priority filenames inside subdirectories. Without this the
	// planner sees no project context and the agent spends turns
	// finding the files.
	if len(out) == 0 {
		entries, err := dir.ReadDir()
		if err != nil {
			return nil
		}
		// First pass: peek into subdirectories for priority files.
		for _, e := range entries {
			if e.IsDir() && !notAProjectDir(e.Name()) {
				sample.readPriorityFiles(e.Name(), priority)
			}
		}
		// Second pass: shallow walk of the workspace root for any
		// source-looking files (uncommon repo layout, no priority
		// hits anywhere).
		if len(out) == 0 {
			sample.readSourceFiles(entries)
		}
	}
	return out
}

// planStage names the planner on the event stream.
const planStage = "v3:plan"

// planSample is what samplePlanContext has collected so far, and its limits.
type planSample struct {
	dir                confinedDir
	out                map[string]string
	maxFiles, maxBytes int
}

// readPriorityFiles adds the priority files under subdir, keyed by the path the
// agent will need to use in tool calls. Oversized files are skipped: the planner
// doesn't need a 50KB README.
func (s planSample) readPriorityFiles(subdir string, priority []string) {
	for _, rel := range priority {
		if len(s.out) >= s.maxFiles {
			return
		}
		path := filepath.Join(subdir, rel)
		if body, ok := s.dir.readCapped(path, s.maxBytes); ok {
			s.out[path] = body
		}
	}
}

// readSourceFiles adds the source-looking files among entries.
func (s planSample) readSourceFiles(entries []fs.DirEntry) {
	for _, e := range entries {
		if len(s.out) >= s.maxFiles {
			return
		}
		name := e.Name()
		if e.IsDir() || !isSourceExtension(strings.ToLower(filepath.Ext(name))) {
			continue
		}
		if body, ok := s.dir.readCapped(name, s.maxBytes); ok {
			s.out[name] = body
		}
	}
}

// notAProjectDir names caches, vendors and dot-dirs.
func notAProjectDir(name string) bool {
	switch name {
	case "node_modules", "venv", "__pycache__", "dist", "build", "target", "vendor":
		return true
	}
	return strings.HasPrefix(name, ".")
}

func isSourceExtension(ext string) bool {
	switch ext {
	case ".py", ".go", ".js", ".ts", ".tsx", ".jsx", ".html", ".rs", ".rb", ".java", ".kt", ".swift":
		return true
	}
	return false
}

// addFilesReadToPlanContext adds the files this run has read to the context of
// a revised plan, until the context holds `limit` files.
//
// When more files were read than fit, they are taken in sorted order of their
// paths. The run keeps no order of reading (FilesRead is a map), and the map's
// own order would make which files the planner sees a matter of chance.
func addFilesReadToPlanContext(ctx *AgentContext, pctx map[string]string, limit int) {
	files := ctx.SnapshotFilesRead()
	for _, path := range sortedKeys(files) {
		if len(pctx) >= limit {
			break
		}
		// Use relative path if possible so the planner key matches
		// what the agent will pass to read_file/edit_file later.
		rel := path
		if strings.HasPrefix(path, ctx.WorkingDir+"/") {
			rel = strings.TrimPrefix(path, ctx.WorkingDir+"/")
		}
		pctx[rel] = contextEntry(files[path], 2000)
	}
}

// contextEntry cuts a file to the bytes a request carries for it, to save
// tokens.
func contextEntry(content string, limit int) string {
	if len(content) > limit {
		return content[:limit] + "\n... (truncated)"
	}
	return content
}
