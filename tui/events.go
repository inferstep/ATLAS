// Chat-event rendering — the translation layer from /v1/agent SSE
// events (and /events envelopes) into chat-history rows and their
// human-readable one-liners. Split from model.go: everything here is
// formatting/classification over event payloads; the update loop and
// view live in model.go.

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// appendChatEvent translates a /v1/agent SSE event into one or more
// chat history rows.
func (m *tuiModel) appendChatEvent(ev chatEvent) {
	switch ev.Type {
	case "turn_start":
		// Visual separator + turn counter. Compact one-liner so a long
		// task's chat doesn't drown in headers — but enough that the
		// user can see "where am I, what turn just started".
		var p struct {
			Turn     int  `json:"turn"`
			Messages int  `json:"messages"`
			Trimmed  bool `json:"trimmed"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		body := fmt.Sprintf("turn %d  ·  ctx=%d msgs", p.Turn+1, p.Messages)
		if p.Trimmed {
			body += "  (trimmed)"
		}
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "turn", Body: body,
		})

	case "llm_call_start":
		// Marker: prompt is being encoded by llama-server. No tokens yet —
		// time-to-first-token reflects prompt eval duration. The body is
		// rewritten on llm_prompt_progress (live %), llm_first_token
		// (decoding starts), and llm_call_end (totals).
		var p struct {
			PromptTokens int `json:"prompt_tokens"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "llm",
			Body: "encoding prompt…",
		})
		m.streamingLLM = true
		m.streamingLLMText = ""
		m.streamingReasoningText = ""
		m.promptProcessed = 0
		m.promptTotal = p.PromptTokens
		m.promptPct = 0
		m.promptEvalStart = time.Now()
		// Pre-fill the context gauge with the prompt-token estimate so
		// the user sees ctx fill up the moment the call starts, not
		// only on llm_call_end. Each llm_token below increments this
		// further; llm_call_end replaces with the authoritative count.
		if p.PromptTokens > 0 {
			m.lastTurnTokens = p.PromptTokens
		}

	case "llm_prompt_progress":
		// Live prompt-eval progress from the proxy's poller. ElapsedMS
		// is always set; processed/total/pct are present only when
		// llama-server's /slots endpoint exposes them. We render a bar
		// when we have %, a spinner+timer otherwise.
		//
		// Guard: the poller runs on a fixed cadence and can emit one more
		// progress event AFTER llm_first_token has flipped the row to
		// "decoding…" and tokens are streaming. Without this check that
		// stale event overwrites the live token row, the next token
		// overwrites it back, and the row flickers between "encoding" and
		// the stream every frame. Once promptEvalStart is zeroed (decoding
		// has begun) we're past prompt eval — drop late progress events.
		if m.promptEvalStart.IsZero() {
			break
		}
		var p struct {
			Processed int     `json:"processed"`
			Total     int     `json:"total"`
			Pct       float64 `json:"pct"`
			ElapsedMS int64   `json:"elapsed_ms"`
		}
		if json.Unmarshal(ev.Data, &p) == nil {
			m.promptProcessed = p.Processed
			m.promptTotal = p.Total
			m.promptPct = p.Pct
			// Live ctx gauge during prompt eval: if /slots gives us
			// processed-tokens, push that into lastTurnTokens so the
			// header context indicator fills as the prompt is encoded
			// (instead of jumping at llm_call_end). On builds where
			// /slots is silent this is a no-op — we still show the
			// chars/4 estimate from llm_call_start.
			if p.Processed > m.lastTurnTokens {
				m.lastTurnTokens = p.Processed
			}
			m.replaceLLMRow(formatPromptProgress(p.Processed, p.Total, p.Pct, p.ElapsedMS))
		}

	case "llm_first_token":
		// Prompt eval finished — decoding has started. Show the prompt
		// duration so the user can see "where the dead air went". The
		// body is rebuilt below as tokens stream in.
		var p struct {
			PromptMS int64 `json:"prompt_ms"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		m.promptEvalStart = time.Time{} // stop tick-rewrite of the row
		secs := float64(p.PromptMS) / 1000.0
		header := fmt.Sprintf("decoding…  (prompt eval: %.1fs)", secs)
		m.streamingLLMHeader = header
		m.replaceLLMRow(header)

	case "llm_token":
		// One delta from the LLM stream. Append to the streaming buffer
		// and re-render the trailing llm row with header + tail of the
		// stream so the user sees the JSON come together token-by-token.
		// The rendered row is dim grey ("machine internals" style) —
		// the polished tool_call/text events below are the bright
		// "outputs from the machine".
		var p struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &p) == nil && p.Text != "" {
			m.streamingLLMText += p.Text
			body := m.streamingLLMHeader + "\n"
			if m.streamingReasoningText != "" {
				body += "  ‹thinking› " + formatStreamingLLM(m.streamingReasoningText) + "\n"
			}
			body += formatStreamingLLM(m.streamingLLMText)
			m.replaceLLMRow(body)
			// Live context-utilization update: each llm_token delta is
			// roughly 1 model token, so increment the gauge per event.
			// Authoritative count replaces this on llm_call_end.
			m.lastTurnTokens++
		}

	case "reasoning_token":
		// reasoning_content may stream alongside
		// content. Accumulate into a parallel buffer and re-render the
		// streaming row with a "‹thinking›" prefix so the user can see
		// the model's thought process. Distinct from llm_token because
		// reasoning is internal model state, not the JSON tool call.
		var p struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &p) == nil && p.Text != "" {
			m.streamingReasoningText += p.Text
			body := m.streamingLLMHeader + "\n" +
				"  ‹thinking› " + formatStreamingLLM(m.streamingReasoningText)
			if m.streamingLLMText != "" {
				body += "\n" + formatStreamingLLM(m.streamingLLMText)
			}
			m.replaceLLMRow(body)
		}

	case "llm_call_end":
		// Replace the streaming row with totals so the scrollback shows
		// a compact "model replied · 8421 tok · 12.3s" instead of the
		// raw token tail. The actual tool_call / text output rows that
		// follow are the bright "outputs from the machine"; this row is
		// the dim "internals" summary.
		var p struct {
			Turn        int    `json:"turn"`
			Tokens      int    `json:"tokens"`
			TotalTokens int    `json:"total_tokens"`
			MS          int64  `json:"ms"`
			Chars       int    `json:"chars"`
			Error       string `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		secs := float64(p.MS) / 1000.0
		var body string
		switch {
		case p.Error != "" && (m.userCancelled || looksCancelled(p.Error)):
			body = fmt.Sprintf("model call cancelled after %.1fs", secs)
		case p.Error != "":
			body = fmt.Sprintf("model failed in %.1fs — %s", secs, p.Error)
		default:
			body = fmt.Sprintf("model replied · %d tok · %d chars · %.1fs",
				p.Tokens, p.Chars, secs)
		}
		m.replaceLLMRow(body)
		m.streamingLLM = false
		m.streamingLLMText = ""
		m.streamingReasoningText = ""
		m.streamingLLMHeader = ""
		// Track tokens for the stats line. llama-server's usage.total_tokens
		// is "prompt + completion of *this* call", which is the right
		// value for "context window utilization". The session-wide sum
		// comes from the proxy's running ctx.TotalTokens (==accumulated
		// per-call totals).
		m.lastTurnTokens = p.Tokens
		m.totalTokensSession = p.TotalTokens

	case "text":
		var p struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(ev.Data, &p) == nil && p.Content != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleAssistant, Body: p.Content,
			})
			// Persist after an assistant text row so a resumed transcript
			// reconstructs the full exchange even if the turn's done marker
			// never arrives (process killed mid-turn).
			m.saveSession()
		}

	case "tool_call":
		var p struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
			Turn int             `json:"turn"`
		}
		if json.Unmarshal(ev.Data, &p) == nil {
			m.chat = append(m.chat, chatMessage{
				Role: roleTool, Meta: "→ " + p.Name,
				Body: summarizeToolArgs(p.Name, p.Args),
			})
			// Highlight files touched by write/edit/delete in the
			// sidebar. The path is normalized to the same form
			// scanFiles produces (relative to workingDir) so the map
			// lookup hits in renderFilesPane. The actual rescan
			// happens on the next tick — fast enough that the new
			// file appears within a few hundred ms, but doesn't block
			// the event handler.
			switch p.Name {
			case "write_file", "edit_file", "delete_file":
				if path := extractWritePath(p.Args); path != "" {
					if m.modifiedFiles == nil {
						m.modifiedFiles = map[string]bool{}
					}
					m.modifiedFiles[path] = true
					// Force-expire the debounce so the next tick scans.
					m.lastFileScan = time.Time{}
					// Track content writes for /review (a deleted file has
					// nothing to review or regenerate).
					if p.Name != "delete_file" {
						if m.passWrites == nil {
							m.passWrites = map[string]bool{}
						}
						m.passWrites[path] = true
					}
				}
			}
		}

	case "tool_result":
		var p struct {
			Tool    string          `json:"tool"`
			Success bool            `json:"success"`
			Data    json.RawMessage `json:"data"`
			Error   string          `json:"error"`
			Elapsed string          `json:"elapsed"`
		}
		if json.Unmarshal(ev.Data, &p) == nil {
			body := p.Error
			if p.Success {
				body = summarizeToolResult(p.Tool, p.Data)
			}
			if p.Elapsed != "" {
				if body == "" {
					body = p.Elapsed
				} else {
					body = fmt.Sprintf("%s  ·  %s", body, p.Elapsed)
				}
			}
			m.chat = append(m.chat, chatMessage{
				Role: roleTool, Meta: "← " + p.Tool,
				Success: p.Success, Body: body,
			})
		}

	case "gate":
		// A completion gate holding the run back — "you were asked to change
		// something and have not". The rejection reached the model and
		// nothing else, so the user watched an unexplained pause while the
		// run silently retried. Shown as a system line: it is the harness
		// speaking, not a tool result, and no tool was executed.
		var p struct {
			Gate   string `json:"gate"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal(ev.Data, &p) == nil {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: p.Gate,
				Body: p.Reason, Echo: true,
			})
		}

	case "permission_request":
		var p struct {
			ToolName    string          `json:"tool_name"`
			Message     string          `json:"message"`
			ToolCallID  string          `json:"tool_call_id"`
			Args        json.RawMessage `json:"args"`
			OneTimeOnly bool            `json:"one_time_only"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		// A deletion is approved one file at a time: the proxy marks the
		// request one_time_only, and delete_file is treated so for a proxy
		// that does not. One "allow for session" used to answer every later
		// deletion here, without the user seeing which file.
		oneTime := p.OneTimeOnly || p.ToolName == "delete_file"
		// A tool already approved "for session" auto-answers allow without
		// showing the modal, so the user isn't re-prompted for it. The POST
		// is fire-and-forget (appendChatEvent has no Cmd return path); the
		// proxy fail-safe still bounds the turn if it never lands.
		if m.sessionAllowedTools[p.ToolName] && !oneTime {
			proxyURL := m.proxyURL
			sid := m.turnSessionID
			cid := p.ToolCallID
			go func() { _ = postPermissionDecision(proxyURL, sid, cid, "allow", "once") }()
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "permission",
				Body: "auto-allowed " + p.ToolName + " (session)", Echo: true,
			})
			return
		}
		// Otherwise raise the modal and gate input until the user answers.
		// Capture the current turn's session id so the decision correlates
		// to THIS turn on POST /v1/permission.
		m.pendingPerm = &permPrompt{
			toolName:    p.ToolName,
			message:     p.Message,
			toolCallID:  p.ToolCallID,
			sessionID:   m.turnSessionID,
			oneTimeOnly: oneTime,
		}

	case "permission_denied":
		var p struct {
			Tool string `json:"tool"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		// A deny can originate proxy-side (timeout, cancel) while the
		// modal is still up — clear it so the input isn't gated by a
		// prompt that no longer has a pending request behind it.
		if m.pendingPerm != nil && m.pendingPerm.toolName == p.Tool {
			m.pendingPerm = nil
		}
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "denied",
			Body: fmt.Sprintf("permission denied for %s", p.Tool),
		})

	case "error":
		var p struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		// Suppress error rows that are really just cancellation echoes
		// (proxy still emits them when ctx.Ctx is cancelled). The user
		// already saw the "turn cancelled" row when they hit Ctrl+C.
		if m.userCancelled || looksCancelled(p.Error) {
			return
		}
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "error", Body: p.Error,
		})

	case "done":
		var p struct {
			Summary string `json:"summary"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		if p.Summary != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "done", Body: p.Summary,
			})
		}

	case "v3_llm_start":
		// V3 is starting an LLM call. Insert a dim "v3-llm" row that
		// the v3_token handler will fill in. Mirrors the agent's
		// llm_call_start row, but with a "V3" tag so the user can
		// tell V3-internal calls from agent-loop calls at a glance.
		var p struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		body := "calling model…"
		if p.Detail != "" {
			body = p.Detail + " · calling model…"
		}
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "v3-llm", Body: body,
		})
		m.streamingV3 = true
		m.streamingV3Text = ""

	case "v3_token":
		// Per-token delta from V3's streaming LLM call. Append to the
		// active v3-llm row (updated in place so we don't spawn
		// thousands of chat rows during a long candidate generation).
		var p struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &p) == nil && p.Text != "" {
			m.streamingV3Text += p.Text
			body := "decoding…\n" + formatStreamingLLM(m.streamingV3Text)
			m.replaceV3LLMRow(body)
		}

	case "v3_reasoning_token":
		// Reasoning deltas from V3's streaming LLM call (candidate
		// generation / repair phases think before emitting code).
		// Rendered into the same in-place v3-llm row as v3_token, with
		// the ‹thinking› prefix the chat-path reasoning_token uses, so
		// long PlanSearch phases show live progress instead of a
		// frozen "decoding…" row.
		var p struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &p) == nil && p.Text != "" {
			m.streamingV3ReasoningText += p.Text
			body := "decoding…\n" +
				"  ‹thinking› " + formatStreamingLLM(m.streamingV3ReasoningText)
			if m.streamingV3Text != "" {
				body += "\n" + formatStreamingLLM(m.streamingV3Text)
			}
			m.replaceV3LLMRow(body)
		}

	case "v3_llm_end":
		// V3's LLM call finished. Replace the streaming row with the
		// summary detail ("1234 tok · 12345ms") so scrollback shows a
		// compact line, not the raw token tail.
		var p struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		body := "model replied"
		if p.Detail != "" {
			body = "model replied · " + p.Detail
		}
		m.replaceV3LLMRow(body)
		m.streamingV3 = false
		m.streamingV3Text = ""
		m.streamingV3ReasoningText = ""

	case "v3_progress":
		// V3 pipeline narration emitted by proxy/tools.go via
		// ctx.StreamFn("v3_progress", {message: "..."}). One row per
		// stage (e.g. "[probe] Generating probe candidate..."). These
		// were silently dropped in the first cut — without this case
		// the user sees a frozen chat pane during a 1-2 minute V3 run.
		var p struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(ev.Data, &p) == nil && p.Message != "" {
			// Trim the leading box-drawing prefix the proxy adds for
			// legacy pretty-print; the TUI styles its own rows.
			msg := strings.TrimLeft(p.Message, " │└├")
			msg = strings.TrimSpace(msg)
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "V3", Body: msg,
			})
		}

	// V3 typed observability events. Each carries a structured `data`
	// payload from the V3 service (counts, indices, timings, strategy
	// names) on top of the human-readable `detail` string. We render
	// each as a dedicated row in the chat with the stage tag bolded.
	// The pipeline pane reads the same events to drive its progress
	// rows. Added 2026-05.
	case "v3_phase", "v3_plansearch", "v3_divsampling", "v3_sandbox",
		"v3_select", "v3_repair", "v3_probe", "v3_self_test":
		body := formatV3StageEvent(ev.Type, ev.Data)
		if body != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "V3", Body: body,
			})
		}

	// PC-207 wiring: per-token lens scoring of a V3 candidate. Each
	// event carries first_off_rails_idx (-1 if clean), gx_score_min,
	// and the candidate index. We surface a compact one-liner per
	// candidate so the user can see WHERE quality cratered without
	// reading raw scores.
	case "v3_lens_per_step":
		body := formatLensPerStep(ev.Data)
		if body != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "lens", Body: body,
			})
		}

	// PC-207 alignment: V3 vetoed a sandbox-passing candidate because the
	// lens flagged it as a stub. Different signal from v3_lens_per_step
	// (which is informational telemetry) — this one means a candidate
	// was actively rejected, so it gets its own row with a clear "veto"
	// meta tag so it stands out in the pane.
	case "v3_lens_veto":
		body := formatLensVeto(ev.Data)
		if body != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "veto!", Body: body,
			})
		}

	// GH #39 point 1: V3 vetoed a sandbox-passing candidate because
	// tree-sitter found unresolved direct-identifier calls. Different
	// failure mode from lens veto (which catches stub-shaped content);
	// this catches "your code calls bar() but bar isn't defined,
	// imported, builtin, or anywhere in the project."
	case "v3_structural_veto":
		body := formatStructuralVeto(ev.Data)
		if body != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "veto!", Body: body,
			})
		}

	// GH #39 point 3: phase-3 repair built call-chain context for the
	// failing function. Informational row — shows that PR-CoT /
	// refinement got structural context layered on top of the bare
	// stderr the LLM otherwise sees.
	case "v3_call_chain_context":
		body := formatCallChainContext(ev.Data)
		if body != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "phase3", Body: body,
			})
		}

	// PC-207 agent-loop integration: lens scored a write_file/edit_file
	// tool call's content. One row per write/edit. Fires whether or not
	// it triggers an intervention; the intervention itself is a
	// separate `agent_lens_intervention` event.
	case "agent_lens_score":
		body := formatAgentLensScore(ev.Data)
		if body != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "lens", Body: body,
			})
		}

	// PC-207 agent-loop integration: the lens detected a regression
	// pattern (N consecutive low-quality writes) and the proxy is
	// queueing a corrective system message for the next LLM call.
	// We surface this prominently so the user knows the loop saw the
	// stuck pattern and broke it.
	case "agent_lens_intervention":
		body := formatAgentLensIntervention(ev.Data)
		if body != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "lens!", Body: body,
			})
		}

	// Tool-call repetition detector: the proxy saw the model emit the
	// same (tool, args) signature N times in close succession and
	// queued a corrective for the next LLM call. Different signal
	// from the lens intervention (semantic vs structural) but same
	// "the loop noticed and broke the model out" surface.
	case "agent_repeat_intervention":
		body := formatAgentRepeatIntervention(ev.Data)
		if body != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "repeat!", Body: body,
			})
		}

	// Asset-graph lint: cross-file coherence findings after a write
	// (orphaned templates/static files, dangling references). Advisory
	// only — render as a system row so the user sees what the model saw.
	case "asset_lint":
		var d struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Detail != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "assets", Body: d.Detail,
			})
		}

	// Reasoning repetition detector: the proxy saw the model open its
	// reasoning stream with the same prefix on consecutive turns and
	// queued a corrective for the next LLM call. Third member of the
	// "model is stuck" family alongside the lens and tool-repeat
	// interventions.
	case "agent_reasoning_intervention":
		body := formatAgentReasoningIntervention(ev.Data)
		if body != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "repeat!", Body: body,
			})
		}

	// Stream cut: the proxy detected the model's content repeating
	// itself mid-stream and stopped the call rather than letting it
	// spin to the token limit.
	case "content_loop_cut":
		var p struct {
			Chars int `json:"chars"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "cut",
			Body: fmt.Sprintf("content loop detected — stream cut after %d chars", p.Chars),
		})

	// The other half of content_loop_cut: the cut was ANSWERED rather than
	// ending the run — the model was told why and given another attempt.
	// Without a case here the user watched a stream stop and a new turn begin
	// with nothing said in between, and the run reported an event the TUI
	// could not render (measured on 2 of 28 benchmark sessions).
	case "agent_loop_recovery":
		var p struct {
			Turn    int    `json:"turn"`
			Attempt int    `json:"attempt"`
			Reason  string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		body := p.Reason
		if body == "" {
			body = "the model began repeating itself; the stream was cut and it was told why"
		}
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "recovered",
			Body: fmt.Sprintf("%s — retrying (attempt %d)", body, p.Attempt),
		})

	// Stream cut: the model burned its reasoning budget without ever
	// emitting content, so the proxy stopped the call and re-prompts.
	case "reasoning_budget_cut":
		var p struct {
			ReasoningChars int `json:"reasoning_chars"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		m.chat = append(m.chat, chatMessage{
			Role: roleSystem, Meta: "cut",
			Body: fmt.Sprintf("reasoning budget exceeded (%d chars, no content) — stream cut, re-prompting", p.ReasoningChars),
		})

	// Symbol-index context: the proxy matched project symbols against
	// the user's request and injected their snippets as a system note.
	case "symbol_index_injected":
		body := formatSymbolIndexInjected(ev.Data)
		if body != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "symbols", Body: body,
			})
		}

	// Plan pipeline progress (planner candidate generation, scoring,
	// selection). Lots of these fire during a 3-candidate sweep but
	// we already drop per-token noise in the proxy callback — what
	// arrives here is structural ("candidate 1/3 scored 0.80") and
	// fits one row per event.
	case "v3_plan":
		body := formatV3StageEvent(ev.Type, ev.Data)
		if body != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "plan", Body: body,
			})
		}

	// Plan loaded — proxy emits one of these after a plan is selected
	// (initial generation OR revision). Carries the full step list,
	// which we stash on m.plan and render as a multi-line chat row.
	case "plan_loaded":
		if msg, ok := applyPlanLoaded(m, ev.Data); ok {
			m.chat = append(m.chat, msg)
		}

	// Plan adherence — fires per tool call. Matched=true ticks off a
	// step in m.plan and renders a one-liner; matched=false (off-plan)
	// is silent here to avoid clogging chat. The off-streak that
	// triggers a revision flows through plan_revise below.
	case "plan_adherence":
		if body := applyPlanAdherence(m, ev.Data); body != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "plan", Body: body,
			})
		}

	// Plan revising — agent went off-plan past the threshold. The
	// next plan_loaded supersedes m.plan; this row tells the user
	// re-planning is in flight.
	case "plan_revise":
		if body := applyPlanRevise(m, ev.Data); body != "" {
			m.chat = append(m.chat, chatMessage{
				Role: roleSystem, Meta: "plan", Body: body,
			})
		}
	}
}

// formatV3StageEvent renders a structured V3 stage event as a single
// chat-row body. We extract the most useful 1–3 fields and append them
// to the human-readable detail. Keeps the line short — the pipeline
// pane is the place to show timelines and counters in detail.
func formatV3StageEvent(eventType string, data json.RawMessage) string {
	var p struct {
		Stage      string  `json:"stage"`
		Detail     string  `json:"detail"`
		Index      int     `json:"index"`
		ElapsedMS  int     `json:"elapsed_ms"`
		Energy     float64 `json:"energy"`
		Passed     int     `json:"passed"`
		Total      int     `json:"total"`
		K          int     `json:"k"`
		Plans      int     `json:"plans"`
		Slots      int     `json:"slots"`
		Tier       string  `json:"tier"`
		Strategy   string  `json:"strategy"`
		Iterations int     `json:"iterations"`
		Tokens     int     `json:"tokens"`
		Failing    int     `json:"failing"`
	}
	_ = json.Unmarshal(data, &p)
	if p.Detail == "" && p.Stage == "" {
		return ""
	}
	tag := strings.TrimPrefix(eventType, "v3_")
	body := tag
	if p.Stage != "" && p.Stage != tag {
		body += "·" + p.Stage
	}
	body += " — " + p.Detail
	// Append the most informative structured field for this event.
	switch p.Stage {
	case "sandbox_pass", "sandbox_fail":
		if p.ElapsedMS > 0 {
			body += fmt.Sprintf(" · %dms", p.ElapsedMS)
		}
	case "sandbox_done":
		if p.Total > 0 {
			body += fmt.Sprintf(" · %d/%d", p.Passed, p.Total)
		}
	case "phase2_allocated":
		if p.K > 0 {
			body += fmt.Sprintf(" · k=%d tier=%s", p.K, p.Tier)
		}
	case "plansearch_done":
		if p.Tokens > 0 {
			body += fmt.Sprintf(" · %d tok", p.Tokens)
		}
	case "refinement_pass":
		if p.Iterations > 0 {
			body += fmt.Sprintf(" · %d iter · %d tok", p.Iterations, p.Tokens)
		}
	case "selected":
		if p.Energy > 0 {
			body += fmt.Sprintf(" · E=%.2f", p.Energy)
		}
	}
	return body
}

// formatAgentLensScore renders an agent-loop lens-score event as one
// chat row. PC-207 fires one of these per write_file/edit_file tool call
// — it's the per-tool quality verdict the agent loop uses to detect
// stuck/repetitive patterns. A clean write looks like
// "write_file @ turn 4 · 320 tok · clean (gx_min=0.78)".
// A bad one looks like "write_file @ turn 15 · 12 tok · off-rails @ tok 0 (gx_min=0.04)".
func formatAgentLensScore(data json.RawMessage) string {
	var p struct {
		Tool             string  `json:"tool"`
		Turn             int     `json:"turn"`
		NTokens          int     `json:"n_tokens"`
		FirstOffRailsIdx int     `json:"first_off_rails_idx"`
		GxScoreMin       float64 `json:"gx_score_min"`
		GxScoreMean      float64 `json:"gx_score_mean"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	var verdict string
	if p.FirstOffRailsIdx >= 0 {
		verdict = fmt.Sprintf("off-rails @ tok %d", p.FirstOffRailsIdx)
	} else {
		verdict = "clean"
	}
	tool := p.Tool
	if tool == "" {
		tool = "write"
	}
	return fmt.Sprintf("%s @ turn %d · %d tok · %s (gx_min=%.2f, gx_mean=%.2f)",
		tool, p.Turn, p.NTokens, verdict, p.GxScoreMin, p.GxScoreMean)
}

// formatAgentLensIntervention renders the agent_lens_intervention event,
// which fires when N consecutive low-quality writes triggered the
// corrective-message inject. The reason field is the multi-sentence
// system message the proxy queued for the next LLM call — we surface a
// shortened version so the user can see WHY the lens intervened.
func formatAgentLensIntervention(data json.RawMessage) string {
	var p struct {
		Turn   int    `json:"turn"`
		Tool   string `json:"tool"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	// The reason is verbose; show just the first sentence + score range
	// so the row stays readable. Full text reaches the model via the
	// injected system message.
	reasonPreview := p.Reason
	if len(reasonPreview) > 200 {
		// Trim to the first sentence ending in period.
		if cut := strings.Index(reasonPreview, ". "); cut > 0 && cut < 200 {
			reasonPreview = reasonPreview[:cut+1]
		} else {
			reasonPreview = reasonPreview[:197] + "..."
		}
	}
	return fmt.Sprintf("INTERVENTION at turn %d on %s — %s", p.Turn, p.Tool, reasonPreview)
}

// formatAgentRepeatIntervention renders the agent_repeat_intervention
// event, which fires when the proxy detected the model issuing the same
// (tool, args) signature N times in close succession (toolRepeatThreshold
// in proxy/detectors.go). Sibling event to agent_lens_intervention but
// catches structural loops the lens (which only sees write content) misses.
// Reason is the verbose corrective queued for the next LLM call; we trim
// it for display.
func formatAgentRepeatIntervention(data json.RawMessage) string {
	var p struct {
		Turn   int    `json:"turn"`
		Tool   string `json:"tool"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	reasonPreview := p.Reason
	if len(reasonPreview) > 200 {
		if cut := strings.Index(reasonPreview, ". "); cut > 0 && cut < 200 {
			reasonPreview = reasonPreview[:cut+1]
		} else {
			reasonPreview = reasonPreview[:197] + "..."
		}
	}
	return fmt.Sprintf("REPEAT at turn %d on %s — %s", p.Turn, p.Tool, reasonPreview)
}

// formatAgentReasoningIntervention renders the agent_reasoning_intervention
// event, which fires when the proxy saw the model's reasoning stream open
// with the same normalized prefix on consecutive turns and queued a
// corrective for the next LLM call. Reason is the verbose corrective;
// trimmed for display like its sibling interventions.
func formatAgentReasoningIntervention(data json.RawMessage) string {
	var p struct {
		Turn        int    `json:"turn"`
		Consecutive int    `json:"consecutive"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	reasonPreview := p.Reason
	if len(reasonPreview) > 200 {
		if cut := strings.Index(reasonPreview, ". "); cut > 0 && cut < 200 {
			reasonPreview = reasonPreview[:cut+1]
		} else {
			reasonPreview = reasonPreview[:197] + "..."
		}
	}
	return fmt.Sprintf("REASONING REPEAT at turn %d (×%d) — %s",
		p.Turn, p.Consecutive, reasonPreview)
}

// formatSymbolIndexInjected renders the symbol_index_injected event —
// the proxy matched project symbols against the request and prepended
// their snippets as a system note before the first LLM call.
func formatSymbolIndexInjected(data json.RawMessage) string {
	var p struct {
		Matched []string `json:"matched"`
		NFiles  int      `json:"n_files"`
		Skipped int      `json:"skipped"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	names := strings.Join(p.Matched, ", ")
	if len(names) > 100 {
		names = names[:97] + "..."
	}
	body := fmt.Sprintf("injected %d symbol snippet(s) from %d project file(s)",
		len(p.Matched), p.NFiles)
	if names != "" {
		body += " — " + names
	}
	if p.Skipped > 0 {
		body += fmt.Sprintf(" (%d skipped)", p.Skipped)
	}
	return body
}

// formatLensVeto renders a v3_lens_veto event as a single chat row.
// Fires when V3 rejected a sandbox-passing candidate because gx_min sat
// in the unambiguously-bad band — i.e. sandbox said "this code runs"
// but the lens said "the model was emitting a stub when it generated
// this." Distinct visual signal from v3_lens_per_step (telemetry) so
// it's obvious in the pane that a real action was taken.
func formatLensVeto(data json.RawMessage) string {
	var p struct {
		Index            int     `json:"index"`
		GxScoreMin       float64 `json:"gx_score_min"`
		FirstOffRailsIdx int     `json:"first_off_rails_idx"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	off := "clean"
	if p.FirstOffRailsIdx >= 0 {
		off = fmt.Sprintf("off-rails @ tok %d", p.FirstOffRailsIdx)
	}
	return fmt.Sprintf("VETO cand %d: sandbox-passed but lens-rejected (gx_min=%.3f, %s) — likely a stub",
		p.Index, p.GxScoreMin, off)
}

// formatCallChainContext renders a v3_call_chain_context event. Fires
// once when V3's phase-3 repair builds a callers/callees map for the
// failing function. The rendered body is short on purpose — the
// detailed context goes into the LLM's repair prompt, not the TUI.
func formatCallChainContext(data json.RawMessage) string {
	var p struct {
		Function string `json:"function"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	if p.Function == "" {
		return ""
	}
	return fmt.Sprintf("phase 3: built call-chain context for failing `%s`", p.Function)
}

// formatStructuralVeto renders a v3_structural_veto event. Fires when
// tree-sitter walks a sandbox-passing candidate and finds direct-identifier
// calls that don't resolve to any local def, import, builtin, or project
// symbol. Sibling to v3_lens_veto in shape — caller only sees the row when
// V3 actually rejected the candidate, not informational telemetry.
func formatStructuralVeto(data json.RawMessage) string {
	var p struct {
		Index           int      `json:"index"`
		NUnresolved     int      `json:"n_unresolved"`
		UnresolvedCalls []string `json:"unresolved_calls"`
		NCallsTotal     int      `json:"n_calls_total"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	preview := strings.Join(p.UnresolvedCalls, ", ")
	if len(preview) > 100 {
		preview = preview[:97] + "..."
	}
	return fmt.Sprintf("STRUCTURAL VETO cand %d: %d/%d unresolved calls — %s",
		p.Index, p.NUnresolved, p.NCallsTotal, preview)
}

// formatLensPerStep renders a v3_lens_per_step event as a single chat row.
// PC-207 wiring fires one of these per V3 candidate after generation.
// The interesting signals: first_off_rails_idx tells the user WHICH token
// the candidate first dipped below the gx threshold (-1 = clean run);
// gx_score_min is the worst per-token quality verdict in the candidate.
// A clean candidate looks like "lens · cand 1: 320 tok · clean (gx_min=0.74)".
// A bad one looks like "lens · cand 0: 320 tok · off-rails @ tok 80 (gx_min=0.08)".
func formatLensPerStep(data json.RawMessage) string {
	var p struct {
		Index            int     `json:"index"`
		Source           string  `json:"source"`
		FirstOffRailsIdx int     `json:"first_off_rails_idx"`
		GxScoreMin       float64 `json:"gx_score_min"`
		GxScoreMean      float64 `json:"gx_score_mean"`
		CxNormMax        float64 `json:"cx_norm_max"`
		NTokens          int     `json:"n_tokens"`
		Detail           string  `json:"detail"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	src := p.Source
	if src == "" {
		src = "candidate"
	}
	tokSummary := fmt.Sprintf("%d tok", p.NTokens)
	var verdict string
	if p.FirstOffRailsIdx >= 0 {
		verdict = fmt.Sprintf("off-rails @ tok %d", p.FirstOffRailsIdx)
	} else {
		verdict = "clean"
	}
	return fmt.Sprintf("%s · cand %d: %s · %s (gx_min=%.2f, gx_mean=%.2f)",
		src, p.Index, tokSummary, verdict, p.GxScoreMin, p.GxScoreMean)
}

// envelopeLooksCancelled returns true if a /events envelope is just
// the cancellation echo we should hide from the events / pipeline pane
// while m.userCancelled is set. Error envelopes always qualify; stage
// _end with success=false qualifies because the only reason a stage
// would mark itself failed during a user-cancelled turn is the
// context-cancelled propagation.
func envelopeLooksCancelled(ev Envelope) bool {
	if ev.Type == EvtError {
		return true
	}
	if ev.Type == EvtStageEnd {
		if ok, _ := ev.Payload["success"].(bool); !ok {
			return true
		}
	}
	return false
}

// looksCancelled returns true if an error string looks like the
// user-initiated context cancellation rather than a real failure.
// The proxy/Go runtime surface this as "context canceled" /
// "context deadline exceeded" / "client disconnected"; the chat-stream
// scanner adds its own "context canceled" wrapping. None of these are
// useful for the user to see — they already pressed Ctrl+C.
func looksCancelled(err string) bool {
	if err == "" {
		return false
	}
	low := strings.ToLower(err)
	for _, sig := range []string{"context canceled", "context cancelled",
		"client disconnected", "request canceled", "operation was canceled",
		"use of closed network connection"} {
		if strings.Contains(low, sig) {
			return true
		}
	}
	return false
}

// formatPromptProgress renders the encoding-prompt progress row. When
// llama.cpp's /slots exposes prompt-eval token counts (some builds do,
// others don't) we render a 24-cell bar plus the running counters.
// When only elapsed time is known, we render a spinner + timer + the
// chars/4 estimate so the user sees motion and rough magnitude. The
// proxy emits one of these every 100ms while llama-server is grinding
// through prompt eval (30–90s on long histories).
func formatPromptProgress(processed, total int, pct float64, elapsedMS int64) string {
	secs := float64(elapsedMS) / 1000.0
	if pct > 0 && total > 0 {
		const barWidth = 24
		if pct > 1 {
			pct = 1
		}
		filled := int(pct*float64(barWidth) + 0.5)
		if filled > barWidth {
			filled = barWidth
		}
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
		return fmt.Sprintf("encoding prompt  [%s] %d/%d (%.0f%%)  · %.1fs",
			bar, processed, total, pct*100, secs)
	}
	// No token counters — show a 10-frame braille spinner indexed by
	// 100ms elapsed so the spinner advances every tick, and surface the
	// chars/4 prompt estimate (`total`) so the user knows how big the
	// prompt is even when llama.cpp doesn't report live progress.
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	frame := frames[(elapsedMS/100)%int64(len(frames))]
	// %.1fs is one-decimal seconds (e.g. "5.4s") — that's what the user
	// asked for. The row redraws every 100ms via the spinner ticker so
	// the timer increments every tick, not every 250ms poll.
	if total > 0 {
		return fmt.Sprintf("encoding prompt  %s  ~%d tok · %.1fs",
			frame, total, secs)
	}
	return fmt.Sprintf("encoding prompt  %s  %.1fs", frame, secs)
}

// formatStreamingLLM renders the partial JSON the model is mid-emitting.
// For write_file calls, the bulk of tokens land inside `"content":"..."`
// as JSON-escaped source code (\n, \", \t…). Showing those raw makes
// the streaming view unreadable. We split at the content boundary and
// unescape the suffix in-place so the user sees code as code.
//
// The escape order matters: replace `\\` last via a placeholder so it
// doesn't double-substitute through \n / \". Truncated trailing escapes
// (e.g. a stray `\` at the buffer tail) are left alone — they'll resolve
// on the next token.
func formatStreamingLLM(s string) string {
	s = strings.TrimLeft(s, " \n\r\t")
	var cut int
	for _, marker := range []string{`"content":"`, `"content": "`} {
		if i := strings.Index(s, marker); i >= 0 {
			cut = i + len(marker)
			break
		}
	}
	if cut == 0 {
		return s
	}
	prefix := s[:cut]
	suffix := s[cut:]

	// Order matters: protect literal backslashes via a placeholder so
	// they don't double-substitute through the \n / \" rules.
	const placeholder = "\x00BS\x00"
	suffix = strings.ReplaceAll(suffix, `\\`, placeholder)
	suffix = strings.ReplaceAll(suffix, `\"`, `"`)
	suffix = strings.ReplaceAll(suffix, `\n`, "\n")
	suffix = strings.ReplaceAll(suffix, `\r`, "")
	suffix = strings.ReplaceAll(suffix, `\t`, "    ")
	suffix = strings.ReplaceAll(suffix, placeholder, `\`)

	// Cap to last N lines. The streaming buffer grows unbounded as the
	// model decodes (a 30k-token write_file is many KB) and we re-wrap
	// it on EVERY tick + token + resize event. Without a cap, drag-
	// resizing the terminal fires dozens of WindowSizeMsg in quick
	// succession; each one runs wrapPlain across the entire buffer,
	// which on a big content payload looks like a freeze. The cap
	// shows a tail view during streaming; the full buffer isn't lost
	// — it's still there in m.streamingLLMText, just truncated for
	// display until llm_call_end replaces the row with stats.
	const streamTailLines = 80
	lines := strings.Split(suffix, "\n")
	if len(lines) > streamTailLines {
		omitted := len(lines) - streamTailLines
		head := fmt.Sprintf("… (%d earlier lines)", omitted)
		suffix = head + "\n" + strings.Join(lines[len(lines)-streamTailLines:], "\n")
	}

	return prefix + "\n" + suffix
}

func summarizeToolArgs(name string, args json.RawMessage) string {
	var generic map[string]interface{}
	if err := json.Unmarshal(args, &generic); err != nil {
		return truncate(string(args), 80)
	}
	switch name {
	case "read_file", "write_file":
		return fmt.Sprintf("path=%v", generic["path"])
	case "edit_file":
		return fmt.Sprintf("path=%v  old=%q",
			generic["path"], truncateAny(generic["old_str"], 40))
	case "run_command":
		return truncateAny(generic["command"], 80)
	}
	parts := []string{}
	for k, v := range generic {
		parts = append(parts, fmt.Sprintf("%s=%s", k, truncateAny(v, 40)))
	}
	return truncate(strings.Join(parts, "  "), 100)
}

func summarizeToolResult(name string, data json.RawMessage) string {
	var generic map[string]interface{}
	if err := json.Unmarshal(data, &generic); err != nil || generic == nil {
		return truncate(string(data), 80)
	}
	for _, k := range []string{"summary", "stdout", "content", "message"} {
		if v, ok := generic[k]; ok {
			return truncateAny(v, 100)
		}
	}
	return ""
}
