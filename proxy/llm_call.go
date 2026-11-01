// One call to llama-server: the request, its stream, and the slot and progress work around it.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// LLM call with grammar constraint
// ---------------------------------------------------------------------------

// isContextOverflow reports whether an LLM-call error is llama-server's
// exceed_context_size_error 400 (prompt tokens > per-slot n_ctx). Matched
// on the error body text — model-agnostic, keyed to llama.cpp's stable
// error type string with the human message as fallback.
func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "exceed_context_size") ||
		strings.Contains(s, "exceeds the available context size")
}

// callLLMConstrained calls the LLM with json_schema or grammar constraint and
// returns the raw response text and token count.
//
// When the model emits zero tokens (raw_len=0), usually after a tool result
// under a constrained JSON grammar, it retries inline once with a bumped
// temperature and a transient "continue" nudge; this avoids burning a full
// agent-loop turn (~30s + tokens) on the parse-error retry path.
//
// Per-step tool restriction: after a write_file rejection on an existing
// .py/.html file over 5 lines, the model tends to retry with edit_file
// (lexically closer to write_file) although structural_edit is the right tool.
// So buildStepRequest (a) drops edit_file and write_file from the GBNF
// tool-name production for this single decision and (b) adds an ephemeral
// [system note] pointing to structural_edit. ctx.Messages is not mutated; the
// nudge and the grammar restriction are scoped to this call.
func callLLMConstrained(ctx *AgentContext) (string, int, error) {
	messages, grammar := buildStepRequest(ctx)

	content, tokens, err := callLLMOnceWithGrammar(ctx, messages, 0.3, grammar)
	if isContextOverflow(err) {
		// The real prompt exceeded the per-slot context despite the
		// budget estimate (dense content under-counts at chars/4).
		// Recover instead of hard-killing the session: force-trim the
		// conversation to the minimum window (system + pins + 8-tail)
		// and retry once. The trim persists on ctx.Messages — the
		// conversation genuinely no longer fits, so shrinking it is
		// the correct durable state, not just a retry hack.
		log.Printf("[agent] context overflow from llama-server — force-trimming to minimum window and retrying")
		ctx.Messages = trimMessages(ctx.Messages, 8)
		messages, grammar = buildStepRequest(ctx)
		content, tokens, err = callLLMOnceWithGrammar(ctx, messages, 0.3, grammar)
	}
	if err != nil {
		return "", tokens, err
	}
	if strings.TrimSpace(content) != "" {
		return content, tokens, nil
	}

	// Empty response — retry once with a transient continuation nudge
	// and a higher temperature. The nudge gives the model an explicit
	// next-action prompt; the temperature bump escapes the EOS-local
	// minimum that the json_object grammar can wedge the model into.
	log.Printf("[agent] empty LLM response, retrying with temp=0.7 + continuation nudge")
	nudged := append(append([]AgentMessage(nil), messages...), AgentMessage{
		Role:    "user",
		Content: `Continue. Respond with one JSON object: {"type":"tool_call","name":"<tool>","args":{...}} for the next action, or {"type":"done","summary":"..."} if the task is complete. Do not emit empty content.`,
	})
	content2, tokens2, err := callLLMOnceWithGrammar(ctx, nudged, 0.7, grammar)
	if err != nil {
		// Return whatever we have from the original call; caller
		// handles empty via parse-error retry.
		return content, tokens, nil
	}
	return content2, tokens + tokens2, nil
}

// buildStepRequest assembles the messages and grammar for the next LLM
// call. With no plan and no restriction it returns ctx.Messages and an
// empty grammar. A plan adds the plan-progress reminder at the tail.
// After a crash with a traceback it bans the run tools
// (tracebackExclusion); after a write_file rejection on an existing
// .py/.html file it adds an ephemeral [system note] and a GBNF grammar
// without edit_file and write_file. See callLLMConstrained for why.
func buildStepRequest(ctx *AgentContext) ([]AgentMessage, string) {
	// Plan-progress reminder. Rendered whenever ctx.Plan has steps; not
	// persisted to ctx.Messages, so it does not accumulate. Lands AT
	// THE TAIL of the messages so the model sees it as the most recent
	// user-role input right before its next decision. Injecting it per
	// turn keeps plan progress visible after trimming without bloating
	// history.
	planReminder := buildPlanReminder(ctx)

	// Traceback step-restriction (issue #39 / option 3): after a crash, ban the
	// run tools so the model can't loop on re-running and is forced to edit the
	// fix site the traceback names. Takes precedence over the write_file case.
	if tbExcluded, tbNote := tracebackExclusion(ctx); len(tbExcluded) > 0 {
		messages := append([]AgentMessage(nil), ctx.Messages...)
		if planReminder != "" {
			messages = append(messages, AgentMessage{Role: "user", Content: planReminder})
		}
		messages = append(messages, AgentMessage{Role: "user", Content: tbNote})
		log.Printf("[agent] traceback step-restriction: banning run tools, forcing an edit")
		return messages, buildGBNFGrammarForTools(tbExcluded)
	}

	excluded, ext := stepExclusions(ctx)
	if len(excluded) == 0 {
		if planReminder == "" {
			return ctx.Messages, ""
		}
		messages := append([]AgentMessage(nil), ctx.Messages...)
		messages = append(messages, AgentMessage{Role: "user", Content: planReminder})
		return messages, ""
	}

	_, target := stepExclusionTarget(ctx)
	var source string
	if b, _, rerr := readWorkspaceFile(ctx, target); rerr == nil {
		source = string(b)
	}
	note := fmt.Sprintf(
		"[system note]: For this single decision, %s is unavailable. The previous write_file was rejected because the target is an existing %s file >5 lines. Use structural_edit to rewrite the named node you are changing (%s). structural_edit doesn't need old_str so it doesn't truncate on long content. Emit exactly one structural_edit tool_call with path %s, that selector, and the node's complete replacement as content.",
		strings.Join(excluded, " and "),
		strings.TrimPrefix(ext, "."),
		selectorGuidanceOrOutline(target, source),
		target,
	)
	messages := append([]AgentMessage(nil), ctx.Messages...)
	if planReminder != "" {
		messages = append(messages, AgentMessage{Role: "user", Content: planReminder})
	}
	messages = append(messages, AgentMessage{Role: "user", Content: note})

	grammar := buildGBNFGrammarForTools(excluded)
	log.Printf("[agent] step-restriction active: banning %v from tool-name enum (ext=%q) — BiasBusters #2/#3", excluded, ext)
	return messages, grammar
}

// stepExclusions inspects the tail of ctx.Messages and returns the list
// of tool names that must be banned for the next decision, plus the
// triggering file extension. Returns nil/"" in the common case.
//
// Trigger: most recent tool-result message is from write_file with a
// success=false body whose error mentions "already exists", and the
// path being targeted has extension .py / .html / .htm. The window
// scanned is the last 6 messages (assistant call + tool result + a few
// recent siblings).
func stepExclusions(ctx *AgentContext) ([]string, string) {
	tools, path := stepExclusionTarget(ctx)
	if tools == nil {
		return nil, ""
	}
	return tools, strings.ToLower(filepath.Ext(path))
}

// stepExclusionTarget is stepExclusions with the rejected write's path, which
// the steering note needs in order to name selectors that exist in it.
func stepExclusionTarget(ctx *AgentContext) ([]string, string) {
	n := len(ctx.Messages)
	if n == 0 {
		return nil, ""
	}
	// Walk backwards over the recent tail. We only fire when the LAST
	// tool message is a write_file rejection on .py/.html. If a fresh
	// assistant turn has already happened (the model corrected itself),
	// the tail will end in something other than that tool result and we
	// return nil — the restriction expires after a single decision.
	startIdx := min(n-1, 6)
	for i := n - 1; i >= n-1-startIdx && i >= 0; i-- {
		msg := ctx.Messages[i]
		if msg.Role != "tool" {
			// First non-tool message we encounter while walking back —
			// stop. We don't want a stale rejection from 4 turns ago to
			// keep firing.
			if msg.Role == "user" && strings.HasPrefix(strings.TrimSpace(msg.Content), "[system note]:") {
				continue
			}
			break
		}
		path, ok := rejectedWritePath(msg)
		if !ok {
			continue
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".py" && ext != ".html" && ext != ".htm" {
			return nil, ""
		}
		// Ban write_file (just got rejected) and edit_file (the wrong
		// shortcut the model is biased toward). Leave structural_edit and the
		// read/run/etc tools available.
		return []string{"edit_file", "write_file"}, path
	}
	return nil, ""
}

// rejectedWritePath reads the path from write_file's refusal of an existing
// file. The format (see surgical-edit gate) is:
//
//	"File <path> already exists (<n> lines). ..."
func rejectedWritePath(msg AgentMessage) (string, bool) {
	if msg.ToolName != "write_file" || !strings.Contains(msg.Content, "already exists") {
		return "", false
	}
	const pfx = "File "
	idx := strings.Index(msg.Content, pfx)
	if idx < 0 {
		return "", false
	}
	path, _, found := strings.Cut(msg.Content[idx+len(pfx):], " ")
	return path, found
}

// eraseLlamaSlot clears llama.cpp's KV slots to give the next chat
// completion a fresh prefix. Errors are logged and
// swallowed — slot erase is a best-effort isolation step, not a
// correctness requirement.
//
// All slots are erased, not just slot 0. With --parallel > 1 and prompt
// caching on, llama-server picks a slot per request by prefix match /
// LRU, so a new session can land on slot 1..N-1. If only slot 0 were
// cleared, those other slots would still hold a prior session's KV and
// reuse it — the exact cross-session bleed this prevents.
func eraseLlamaSlot(ctx *AgentContext) {
	llamaURL := envOr("ATLAS_LLAMA_URL", ctx.InferenceURL)

	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	client := &http.Client{Timeout: 5 * time.Second}

	erased := 0
	slots := parallelSlots()
	var stale []int
	for id := 0; id < slots; id++ {
		// llama-server handles the erase on its main loop, so a slot that
		// is mid-decode answers only once it frees up. A single 5s attempt
		// can miss that slot, and it is precisely the one still holding
		// the previous session's KV, so eraseOneSlot retries.
		if eraseOneSlot(reqCtx, client, llamaURL, id) {
			erased++
		} else {
			stale = append(stale, id)
		}
	}
	if len(stale) > 0 {
		// Claiming a fresh cache here would describe the intent rather
		// than the result: an un-erased slot can be picked by prefix match
		// and reuse a prior session's KV, which is the bleed this exists
		// to prevent.
		log.Printf("[agent] erased %d/%d llama slots — slot(s) %v still hold prior KV "+
			"and may be reused by prefix match", erased, slots, stale)
		return
	}
	log.Printf("[agent] erased %d/%d llama slots — fresh KV cache for this session", erased, slots)
}

// eraseRetryable classifies a slot-erase response by what the status means,
// not by its hundreds digit. Retrying costs real time -- 0.5s then 1.0s per
// slot, at the start of every agent loop -- so a retry is spent only where a
// second answer can differ from the first.
//
// The matrix, read against the pinned llama-server (tools/server at
// LLAMA_CPP_REV): its error types map to 400 invalid request ("Invalid slot
// ID", "Invalid action"), 401 authentication, 403 permission, 404 not found,
// 500 server, 501 not supported ("does not support slots endpoint", and with
// no --slot-save-path "does not support slots action"), 503 unavailable
// (model still loading, or no slot free). A slot that is mid-decode is not an
// error at all: the erase task is deferred and answered when the slot frees,
// so on the client side that is a slow answer or a transport timeout.
//
//	permanent, no retry   400 401 403 404 405 410 501, and any other 4xx: the
//	                      server understood the request and refused it, or
//	                      the operation does not exist here. No blind retry
//	                      on an authentication or permission refusal.
//	transient, retried    408 409 425 429: timed out, in conflict, too early,
//	                      rate-limited -- each names a state that passes.
//	                      5xx other than 501: the server is the one that
//	                      failed, and 503 is the busy/loading answer.
//	transport failure     no answer at all: retried, in eraseOneSlot.
//
// llama-server itself emits none of 408/409/425/429 on this endpoint; a
// reverse proxy or gateway in front of it can, and those are exactly the
// statuses a hundreds-digit rule would have wrongly made final.
func eraseRetryable(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusConflict,
		http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	case http.StatusNotImplemented:
		return false
	}
	return status >= 500
}

// eraseOneSlot clears a single KV slot, retrying while it is busy. Reports
// whether the slot ended up clear.
func eraseOneSlot(reqCtx context.Context, client *http.Client, llamaURL string, id int) bool {
	endpoint := fmt.Sprintf("%s/slots/%d?action=erase", llamaURL, id)
	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, nil)
		if err != nil {
			log.Printf("[agent] erase slot %d: build request failed: %v", id, err)
			return false
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
			if !eraseRetryable(resp.StatusCode) {
				log.Printf("[agent] erase slot %d: status %d, not retrying",
					id, resp.StatusCode)
				return false
			}
			log.Printf("[agent] erase slot %d: status %d (attempt %d/%d)",
				id, resp.StatusCode, attempt, attempts)
		} else {
			log.Printf("[agent] erase slot %d: request failed: %v (attempt %d/%d)",
				id, err, attempt, attempts)
		}
		if reqCtx.Err() != nil {
			return false
		}
		if attempt < attempts {
			select {
			case <-reqCtx.Done():
				return false
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
	}
	return false
}

// pollPromptProgress emits llm_prompt_progress events at 100ms cadence
// while llama-server is in the prompt-eval phase of a streaming chat
// completion. Without these events the TUI freezes on "encoding prompt…"
// for the 30–90s prompt-eval window on long histories.
//
// Always emits elapsed_ms so the TUI can show a live timer ("encoding
// prompt · 12.3s"). Additionally tries to extract processed/total/pct
// from llama.cpp's /slots endpoint — those fields are only present in
// some llama.cpp builds (n_prompt_tokens_processed / n_prompt_tokens).
// When absent, the TUI renders a spinner-with-timer rather than a bar.
//
// Stops when stop is closed (the caller closes it on first-token
// arrival, on function return, or on context cancel).
//
// totalEst is the chars/4 prompt-token estimate; passed through to the
// TUI as `total_est` so even without /slots data the user sees the
// rough magnitude of what's being encoded.
func pollPromptProgress(ctx *AgentContext, llamaURL string, stop <-chan struct{}, totalEst int) {
	// Defense in depth: if anything panics inside this goroutine
	// (e.g. a write to a closed flusher) don't take the whole proxy
	// down with it. The WaitGroup in callLLMOnceWithGrammar should prevent the
	// race that makes this possible, but a recover here is cheap.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] pollPromptProgress recovered: %v", r)
		}
	}()
	startedAt := time.Now()
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	// Once /slots returns 404/501 we stop probing it but keep emitting
	// elapsed-time progress events — the timer is the useful signal,
	// the bar is the bonus.
	slotsAvailable := true
	for {
		select {
		case <-stop:
			return
		case <-ctx.Ctx.Done():
			return
		case <-ticker.C:
		}
		elapsed := time.Since(startedAt).Milliseconds()
		processed, total := 0, 0
		if slotsAvailable {
			processed, total, slotsAvailable = probeSlot(ctx.Ctx, client, llamaURL)
		}
		if total == 0 {
			total = totalEst
		}
		pct := 0.0
		if processed > 0 && total > 0 {
			pct = float64(processed) / float64(total)
			if pct > 1 {
				pct = 1
			}
		}
		ctx.Stream("llm_prompt_progress", map[string]interface{}{
			"processed":  processed,
			"total":      total,
			"pct":        pct,
			"elapsed_ms": elapsed,
		})
	}
}

// probeSlot does one /slots GET and pulls out prompt-eval counters when
// llama.cpp exposes them. Returns (processed, total, stillAvailable);
// stillAvailable goes false on 404/501 so the caller can stop probing.
func probeSlot(ctx context.Context, client *http.Client, llamaURL string) (int, int, bool) {
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", llamaURL+"/slots", nil)
	if err != nil {
		return 0, 0, true
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, true // transient — try again next tick
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		return 0, 0, false // /slots disabled — give up
	}
	if resp.StatusCode != http.StatusOK {
		return 0, 0, true
	}
	var slots []map[string]interface{}
	if err := json.NewDecoder(serviceAnswer(resp.Body)).Decode(&slots); err != nil {
		return 0, 0, true
	}
	for _, s := range slots {
		if isProc, ok := s["is_processing"].(bool); ok && !isProc {
			continue
		}
		processed := firstPositiveCount(s, "n_prompt_tokens_processed", "prompt_n", "n_past")
		total := firstPositiveCount(s, "n_prompt_tokens", "n_prompt")
		return processed, total, true
	}
	return 0, 0, true
}

// firstPositiveCount returns the first named counter that holds a count.
func firstPositiveCount(slot map[string]interface{}, keys ...string) int {
	for _, k := range keys {
		if v, ok := slot[k].(float64); ok && v > 0 {
			return int(v)
		}
	}
	return 0
}

// llmStreamClient is a long-lived HTTP client for streaming LLM calls.
// Only the dial is bounded, so connection refused or a DNS failure
// still fails fast. There is no total Client.Timeout and no
// ResponseHeaderTimeout: a streamed generation can run for minutes, and
// llama.cpp sends the response headers only with the first decoded
// token, so header time is prompt-eval time, which can take many
// minutes on a long history or on cold KV state after a V3 run. The
// request context (ctx.Ctx) carries cancellation, so user-initiated
// cancels still propagate.
var llmStreamClient = &http.Client{
	Transport: &http.Transport{
		DialContext:     (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		IdleConnTimeout: 90 * time.Second,
	},
}

// toWireMessages converts the agent's internal messages to the role/content
// pairs sent on /v1/chat/completions.
//
// Tool results are rendered as a USER turn. Some chat templates have no `tool`
// role and silently drop role:"tool" messages, so the model never sees the
// result and re-issues the same tool call until the repetition breaker fires.
// Every chat template handles the user role, so converting here is
// model-agnostic; the `[tool result]` marker tells the model this is tool
// output, not a fresh user instruction. ctx.Messages keeps the semantic "tool"
// role so trim-pinning and the step/traceback exclusions that key off ToolName
// still work.
func toWireMessages(messages []AgentMessage) []map[string]string {
	wire := make([]map[string]string, len(messages))
	for i, msg := range messages {
		role := msg.Role
		content := msg.Content
		if role == "tool" {
			role = "user"
			content = "[tool result] " + content
		}
		wire[i] = map[string]string{"role": role, "content": content}
	}
	return wire
}

// applyRepetitionSampling sets the repetition-control sampler fields on an
// outgoing llama-server request.
//
// llama-server ships every repetition control off (/props reports
// repeat_penalty=1.0, dry_multiplier=0.0, frequency_penalty=0.0,
// presence_penalty=0.0); the stream-level loop cut (streamLooping) is a
// backstop and does not stop the model entering a loop.
//
// DRY rather than repeat_penalty: repeat_penalty scores single-token
// reoccurrence, which is wrong for code (indentation, `return`, `self.` and
// closing braces repeat legitimately); DRY scores repeated sequences, and
// llama.cpp treats a newline as a sequence breaker by default, so per-line
// repetition is not penalized. dry_allowed_length is raised above llama.cpp's
// default of 2 because 3-token runs are ordinary in source.
//
// DRY defaults OFF because it penalises the one thing an edit tool needs most:
// copying an anchor out of a file the model just read.
//
// dry_penalty_last_n does not limit the scan to the current generation:
// llama-server feeds every PROMPT token to the sampler chain
// (ServerSlot::init_sampler calls common_sampler_accept for each; is_generated
// gates only the grammar and reasoning-budget samplers), so the ring buffer
// holds the tail of the prompt, which is the read_file result being copied.
//
// The penalty is multiplier * base^(matched - allowed_length), subtracted from
// the logit; at multiplier 0.8 with the defaults below it is -2.45 at 8 matched
// tokens, -7.51 at 10, -23.0 at 12, -123 at 15. Around token 10-12 of an anchor
// the correct continuation drops below the runner-up, the model takes the
// branch that breaks the match (scoreElement becomes scorerElement), the match
// length resets and the penalty collapses. Set ATLAS_DRY_MULTIPLIER (for
// example 0.8) to turn DRY on.
func applyRepetitionSampling(reqBody map[string]interface{}) {
	dryMultiplier := envFloatOr("ATLAS_DRY_MULTIPLIER", 0)
	if dryMultiplier > 0 {
		reqBody["dry_multiplier"] = dryMultiplier
		reqBody["dry_base"] = envFloatOr("ATLAS_DRY_BASE", 1.75)
		reqBody["dry_allowed_length"] = envIntOr("ATLAS_DRY_ALLOWED_LENGTH", 6)
		// Bound the lookback; -1 (llama.cpp's default) would make every prior
		// turn's text a repetition source. The window holds prompt tokens too
		// (see above), so it does not limit DRY to the current generation.
		reqBody["dry_penalty_last_n"] = envIntOr("ATLAS_DRY_PENALTY_LAST_N", 2048)
	}
	if rp := envFloatOr("ATLAS_REPEAT_PENALTY", 1.0); rp != 1.0 {
		reqBody["repeat_penalty"] = rp
		reqBody["repeat_last_n"] = envIntOr("ATLAS_REPEAT_LAST_N", 64)
	}
}

// envFloatOr reads a float tunable from the environment, falling back to def
// when unset or unparseable.
func envFloatOr(key string, def float64) float64 {
	if v := envOr(key, ""); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

// envIntOr reads an int tunable from the environment, falling back to def
// when unset or unparseable.
func envIntOr(key string, def int) int {
	if v := envOr(key, ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// restatementMaxBytes caps the restated file. Past this the copy is not the
// bottleneck anyway, and a big paste costs prompt-processing time on every turn.
const restatementMaxBytes = 24000

// appendLastReadRestatementFor puts the most recently read file back at the END of
// the message list, immediately before the generation point.
//
// Why this helps: the read_file result the model must copy from sits behind the
// system prompt, every tool description and every prior turn, thousands of
// tokens back, while the model's own partial copy sits at the very end. The two
// candidate sources for what comes next are not equally reachable, and the near
// one wins more often as the copy lengthens; that asymmetry is the most
// plausible account of copy corruptions such as food.y becoming hood.y.
// Shortening the distance between the source span and the generation point
// helps under any account of long-range retrieval degradation, and it is free.
//
// Skipped when nothing has been read, when the file is large, and when the same
// content is already in the window (the common case right after a read_file).
// ATLAS_RESTATE_LAST_READ=0 disables.
//
// When onlyPath is set, the last read is restated only if it IS that file.
//
// The file-content sub-call asks for one file, and with the context ending in
// "Current contents of" a DIFFERENT file the model can return a near copy of
// that other file. The target's own restatement is kept: it is the file being
// rewritten.
func appendLastReadRestatementFor(ctx *AgentContext, wire []map[string]string, onlyPath string) []map[string]string {
	if ctx == nil || envOr("ATLAS_RESTATE_LAST_READ", "1") == "0" {
		return wire
	}
	path, content := ctx.LastRead()
	if onlyPath != "" && path != "" && filepath.Clean(path) != filepath.Clean(resolveAgentPath(ctx, onlyPath)) {
		log.Printf("[agent] not restating %s in the sub-call for %s — a different file",
			logPath(path), logPath(onlyPath))
		return wire
	}
	if path == "" || content == "" || len(content) > restatementMaxBytes {
		return wire
	}
	// Already in the window — don't pay for it twice. trimMessages PINS the
	// most recent file-content tool result so the active file survives
	// trimming, so for the file being edited this is the normal case, and
	// restating it appended a second full copy of something already present.
	for _, m := range wire {
		if strings.Contains(m["content"], content) {
			return wire
		}
	}
	// Fit it in what the slot actually has left. The budget is computed over
	// ctx.Messages and applied by trimMessages; this block is appended to the
	// WIRE afterwards, so nothing counted it. Restating is an optimisation;
	// overflowing the slot is a 400 that ends the stream, so it yields.
	used := 0
	for _, m := range wire {
		used += estTokens(m["content"])
	}
	headroom := perSlotContext() - agentMaxTokens() - used
	if headroom < estTokens(content)+numberedLineOverhead(content) {
		log.Printf("[agent] skipping the last-read restatement of %s — no headroom (%d tokens left)",
			logPath(path), headroom)
		return wire
	}
	return append(wire, map[string]string{"role": "user", "content": numberedRestatement(ctx, path, content)})
}

// numberedRestatement is the file as the wire carries it: named from the
// workspace, with the line numbers read_file shows.
func numberedRestatement(ctx *AgentContext, path, content string) string {
	rel := path
	if ctx.WorkingDir != "" {
		if r, err := filepath.Rel(ctx.WorkingDir, path); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}
	var sb strings.Builder
	sb.WriteString("Current contents of ")
	sb.WriteString(rel)
	sb.WriteString(" (line numbers are for reference and are NOT in the file):\n")
	for i, line := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
		fmt.Fprintf(&sb, "%d\t%s\n", i+1, line)
	}
	return sb.String()
}

// callLLMOnceWithGrammar is one round-trip to llama-server's
// /v1/chat/completions. The request streams over SSE so the proxy can forward
// per-token deltas to the TUI as llm_token events; the first delta also fires
// llm_first_token with the prompt-eval duration (request sent to first token),
// which the user otherwise cannot see. A streamed call has no total-request
// timeout, so a long write_file generation is not cut (see llmStreamClient).
//
// An optional GBNF grammar override is enforced by llama-server at token-decode
// level (for example to ban edit_file and write_file from the tool-name
// production for a single decision). With a grammar the json_object
// response_format is dropped, because GBNF is the more specific constraint.
func callLLMOnceWithGrammar(ctx *AgentContext, messages []AgentMessage, temperature float64, grammar string) (string, int, error) {
	return callLLMOnceRestating(ctx, messages, temperature, grammar, "")
}

// llmRequestBody is the chat-completions request of one call: the sampling the
// agent uses, and the grammar or the JSON response format that constrains
// the reply.
func llmRequestBody(wireMessages []map[string]string, temperature float64, grammar string) []byte {
	// Per-turn hard ceiling (agentMaxTokens, default 8192). The full 32768
	// window would let a rambling content blob run ~18 minutes at the GPU's
	// capped decode rate, and nothing else catches that runaway: the
	// reasoning budget only fires on reasoning without content. An agent turn
	// is a tool call (small) or a whole-file write_file (a few thousand
	// tokens); 8192 covers a ~600-line generation. A rare legitimate overflow
	// is answered by the parse-failure path, which tells the model its call
	// was cut. conversationTokenBudget reserves this same value.
	reqBody := map[string]interface{}{
		"model":       modelName,
		"messages":    wireMessages,
		"temperature": temperature,
		"max_tokens":  agentMaxTokens(),
		"stream":      true,
		// Without include_usage, the final SSE chunk before [DONE] has no
		// usage block, so we can't report total_tokens to the TUI.
		"stream_options": map[string]bool{"include_usage": true},
		// Some reasoning-capable chat templates default to thinking, but the
		// agent loop relies on grammar-constrained JSON output — thinking
		// blocks would just bloat tokens and llama-server rejects the
		// combination outright once a trailing assistant message looks
		// like a "response prefill" (400: "Assistant response prefill is
		// incompatible with enable_thinking"). Disable explicitly.
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
	}
	applyRepetitionSampling(reqBody)

	// Transcription profile. An agent turn emits a tool call whose arguments
	// are largely COPIED from a file the model just read, and every sampler in
	// the default chain is tuned for open-ended prose: top_k 40 / top_p 0.95 /
	// min_p 0.05 all leave a live tail for a wrong-but-plausible token to be
	// drawn from, and that is what a near-miss identifier is.
	//
	// "samplers": ["top_k"] with top_k 1 builds a chain of exactly
	// logit_bias -> top_k(1) -> dist, so the penalty samplers are never
	// instantiated at all. Not temperature 0: since llama.cpp PR #9897 temp<=0
	// is handled inside the temperature sampler, which sits LAST, so it returns
	// the argmax of an already-penalised distribution.
	//
	// ATLAS_TRANSCRIPTION_SAMPLER=0 restores the server defaults.
	if envOr("ATLAS_TRANSCRIPTION_SAMPLER", "1") != "0" {
		reqBody["samplers"] = []string{"top_k"}
		reqBody["top_k"] = 1
	}

	if grammar == rawEmissionSentinel {
		// Free-text reply: neither grammar nor response_format. Used for
		// the fenced-content sub-call, where the whole point is escaping
		// the JSON channel — measured on the served model, a debounce
		// solution parses 6/6 when emitted in a fenced block and 0/6 when
		// emitted inside a JSON string.
	} else if grammar != "" {
		// Token-level restriction wins over response_format. llama-server
		// rejects requests that pass both response_format=json_object and
		// a non-trivial grammar; pass only the grammar in restricted mode.
		reqBody["grammar"] = grammar
	} else {
		reqBody["response_format"] = buildResponseFormat()
	}
	body, _ := json.Marshal(reqBody)
	return body
}

// callLLMOnceRestating is callLLMOnceWithGrammar with restateOnly: the caller
// names the file this request is about, and no other file's contents are
// appended to it (see appendLastReadRestatementFor).
func callLLMOnceRestating(ctx *AgentContext, messages []AgentMessage, temperature float64,
	grammar, restateOnly string) (string, int, error) {
	// Stale from the previous turn otherwise, which would blame a clean
	// parse failure on a cut that happened earlier.
	ctx.LastStreamCut = ""
	ctx.LastOldStrCut = nil
	ctx.LastFencedStream = fencedStreamStats{}
	wireMessages := toWireMessages(messages)
	wireMessages = appendLastReadRestatementFor(ctx, wireMessages, restateOnly)

	llamaURL := envOr("ATLAS_LLAMA_URL", ctx.InferenceURL)

	body := llmRequestBody(wireMessages, temperature, grammar)
	endpoint := llamaURL + "/v1/chat/completions"

	// Carry the agent's request context into the HTTP request so client
	// disconnects propagate down to llama-server.
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	// Progress watchdog for the fenced sub-call. Cancelling reqCtx aborts the
	// HTTP request, which ends the scanner loop and closes the slot
	// server-side, the same path a client disconnect takes, so no request or
	// goroutine outlives it. Ordinary turns are untouched. The fenced sub-call
	// is the one path with a progress watchdog and wire diagnostics; naming
	// the condition keeps every place that asks in agreement. It covers both
	// kinds of fenced attempt, the free-text one and attempt 0, which is
	// constrained by the fence grammar: the grammar reserves four backticks
	// for the closer, and a model that closes with three would otherwise
	// generate to the token ceiling.
	fencedSubCall := grammar == rawEmissionSentinel || isFenceBlockGrammar(grammar)
	progress := func() { /* only a fenced sub-call has a watchdog to feed */ }
	armStalled := func() { /* only a fenced sub-call has a watchdog to arm */ }
	if fencedSubCall {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithCancel(reqCtx)
		defer cancel()
		var timer *time.Timer
		timer, progress, armStalled = fencedWatchdog(cancel)
		defer timer.Stop()
	}
	httpReq, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set(headerContentType, "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	// Don't reuse the TCP connection across turns. We were seeing
	// `Post ".../v1/chat/completions": EOF` failures in 0ms between
	// back-to-back turns: the previous streaming response left the
	// connection in a state llama-server (--parallel 1) closed at its
	// end, then the next turn's POST reused the dead idle connection
	// from Go's pool and got EOF on first read. Setting Close=true
	// forces a fresh dial per call. The dial overhead is negligible
	// next to a 5k-token prompt eval, and the reliability win is huge.
	httpReq.Close = true

	sentAt := time.Now()

	// pollPromptProgress runs as a sibling goroutine while the LLM call is
	// in flight; it streams elapsed_ms ticks back to the TUI. We MUST
	// guarantee it has fully exited before callLLMOnceWithGrammar returns — otherwise
	// it can call ctx.Stream (which writes to handleAgent's flusher) AFTER
	// handleAgent has returned and the response writer is invalid, causing
	// a SIGSEGV inside bufio.(*Writer).Flush. The defers run LIFO: stop
	// the channel first, then wait on the WaitGroup until the goroutine
	// exits.
	stopProgressFn, waitForPoller := startPromptProgress(ctx, llamaURL, messages)
	defer waitForPoller()
	defer stopProgressFn()

	resp, err := llmStreamClient.Do(httpReq)
	if err != nil {
		// No response at all before the first-content watchdog fired: the
		// fenced_fetch event says so (#254).
		if fencedSubCall && reqCtx.Err() != nil && (ctx.Ctx == nil || ctx.Ctx.Err() == nil) {
			ctx.LastFencedStream = fencedStreamStats{Cut: "first_content"}
		}
		return "", 0, fmt.Errorf("LLM request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(serviceAnswer(resp.Body))
		return "", 0, fmt.Errorf("LLM returned %d: %s",
			resp.StatusCode, truncateStr(string(respBody), 500))
	}

	stream := &llmStream{ctx: ctx, grammar: grammar, fencedSubCall: fencedSubCall, sentAt: sentAt,
		progress: progress, armStalled: armStalled, stopProgressFn: stopProgressFn}
	return stream.read(resp, reqCtx)
}

// startPromptProgress starts the poller that streams the prompt's progress to
// the client while the model reads it, and returns the function that stops
// the poller and the one that waits until it has exited.
func startPromptProgress(ctx *AgentContext, llamaURL string, messages []AgentMessage) (func(), func()) {
	// Estimate total prompt tokens (chars/4 — works for English + code
	// within ~10–20%) so the prompt-progress poller has a baseline even
	// when /slots doesn't expose n_prompt_tokens directly.
	promptTokenEst := 0
	for _, m := range messages {
		promptTokenEst += len(m.Content) / 4
	}
	stopProgress := make(chan struct{})
	var stopOnce sync.Once
	stopProgressFn := func() { stopOnce.Do(func() { close(stopProgress) }) }
	var pollWG sync.WaitGroup
	pollWG.Add(1)
	go func() {
		defer pollWG.Done()
		pollPromptProgress(ctx, llamaURL, stopProgress, promptTokenEst)
	}()
	return stopProgressFn, pollWG.Wait
}

// llmStream is the read of one streamed reply: what has arrived on each
// channel, the token count, the cuts that end the read early, and, on a
// fenced sub-call, what the wire showed.
type llmStream struct {
	ctx            *AgentContext
	grammar        string
	fencedSubCall  bool
	sentAt         time.Time
	progress       func()
	armStalled     func()
	stopProgressFn func()

	contentBuf strings.Builder
	// reasoningBuf captures reasoning_content separately for the
	// empty-content fallback below. Some models engage thinking mode
	// despite enable_thinking=false (most reproducibly on retries with
	// bumped temperature), and then ALL output streams into
	// delta.reasoning_content; the fallback recovers a tool call from
	// it instead of returning an empty string.
	reasoningBuf   strings.Builder
	totalTokens    int
	firstTokenSent bool
	reasoningCut   bool
	contentLoopCut bool
	noFenceCut     bool
	lastLoopCheck  int
	oldStrCut      bool
	oldStrW        *oldStrWatch

	reasoningBudgetChars int

	rawLines    int
	firstLine   string
	firstFrame  time.Duration
	watchdogCut string
}

// read reads the reply to its end or to a cut, and returns it: the content,
// or what the reasoning holds when the content is empty.
func (rd *llmStream) read(resp *http.Response, reqCtx context.Context) (string, int, error) {
	ctx := rd.ctx
	// An edit_file old_str is checked against its file while it streams
	// (old_str_watch.go). Not in a fenced sub-call, which carries a body.
	rd.oldStrW = newOldStrWatch(ctx)

	// Per-turn reasoning budget. A reasoning-heavy model can spiral for
	// many thousands of tokens inside ONE generation and end with no
	// tool call, and max_tokens alone allows minutes of silence. When
	// accumulated reasoning passes the budget with no content emitted,
	// we stop reading; closing the response body cancels the slot
	// server-side. The post-loop recovery then either extracts a
	// tool_call already present in the reasoning, or returns empty so
	// the caller's empty-response retry re-prompts. Default 6144 tokens,
	// estimated at 4 chars/token; ATLAS_REASONING_BUDGET (tokens)
	// overrides, 0 disables. Keyed off stream state, not model identity.
	rd.reasoningBudgetChars = 6144 * 4
	if v := envOr("ATLAS_REASONING_BUDGET", ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			rd.reasoningBudgetChars = n * 4
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	// Default scanner buffer is 64KB which is fine per line, but bump
	// the max in case llama-server emits a fat usage payload at the end.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	// Diagnostic for the fenced sub-call only: the live failure is that the
	// server generates a full file while this reader sees no delta at all,
	// and every direct reproduction of the same request streams in ~0.25s.
	// Recording what actually came back over the wire is what distinguishes
	// "nothing was sent" from "something was sent and not parsed".
	if rd.fencedSubCall {
		log.Printf("[agent] fenced sub-call response: status=%s content-type=%q transfer-encoding=%v content-length=%d",
			resp.Status, resp.Header.Get(headerContentType),
			resp.TransferEncoding, resp.ContentLength)
		// What the wire showed, for the caller's fenced_fetch event (#254).
		defer func() {
			ctx.LastFencedStream = fencedStreamStats{
				FirstFrame: rd.firstFrame, WireLines: rd.rawLines, FirstLine: truncateStr(rd.firstLine, 160),
				ReasoningChars: rd.reasoningBuf.Len(), ContentChars: rd.contentBuf.Len(), Cut: rd.watchdogCut,
			}
		}()
	}

	for scanner.Scan() {
		if rd.readLine(scanner.Text()) {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return rd.endOnReadError(err, reqCtx)
	}

	// Stash the reasoning content on ctx so the agent loop's per-turn
	// reasoning-repetition detector can compare it against prior turns.
	// We capture regardless of whether contentBuf was non-empty — the
	// model may emit BOTH content (the JSON tool call) AND reasoning
	// (the prose narration), and we want to detect rehashed reasoning
	// even when a tool call was successfully emitted.
	ctx.LastTurnReasoning = rd.reasoningBuf.String()

	if rd.contentBuf.Len() == 0 {
		return rd.recoverEmptyContent()
	}
	return rd.contentBuf.String(), rd.totalTokens, nil
}

// readLine reads one line of the stream: on a fenced sub-call what the wire
// showed, then a frame's reasoning, its content and its token count. It reports
// whether the read stops here, at the stream's end or at a cut.
func (rd *llmStream) readLine(line string) bool {
	if rd.fencedSubCall {
		rd.rawLines++
		if rd.firstLine == "" && strings.TrimSpace(line) != "" {
			rd.firstLine = line
			rd.firstFrame = time.Since(rd.sentAt)
			// The stream is open and generating; content is due now.
			rd.armStalled()
			log.Printf("[agent] fenced sub-call first wire line after %s: %s",
				time.Since(rd.sentAt).Round(time.Millisecond), truncateStr(line, 160))
		}
	}
	if !strings.HasPrefix(line, "data: ") {
		return false
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return true
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			TotalTokens      int `json:"total_tokens"`
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return false
	}
	for _, c := range chunk.Choices {
		if c.Delta.ReasoningContent != "" {
			rd.readReasoning(c.Delta.ReasoningContent)
		}
		if c.Delta.Content == "" {
			continue
		}
		rd.readContent(c.Delta.Content)
	}
	if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
		rd.totalTokens = chunk.Usage.TotalTokens
	}
	return rd.cut()
}

// readReasoning takes one reasoning delta: the first token's event, the
// reasoning so far, and the reasoning budget's cut.
func (rd *llmStream) readReasoning(reasoning string) {
	ctx := rd.ctx
	// First output of ANY kind means prompt eval is done — for
	// reasoning models (some stream their whole chain as
	// reasoning_content, often with no content tokens until the
	// final JSON) the first delta is reasoning, not content.
	// Stop the prompt-eval poller and fire llm_first_token here
	// too; otherwise the poller keeps emitting prompt_progress
	// for the entire generation, the TUI keeps painting
	// "encoding", and it fights the streaming reasoning for the
	// row — the encode timer never stops and the screen flickers.
	if !rd.firstTokenSent {
		rd.stopProgressFn()
		ctx.Stream("llm_first_token", map[string]interface{}{
			"prompt_ms": time.Since(rd.sentAt).Milliseconds(),
		})
		rd.firstTokenSent = true
	}
	// Accumulate for the empty-content fallback below AND
	// stream to the TUI as a separate `reasoning_token` event
	// so users can see the model's thought process. The TUI
	// subscribes to reasoning_token distinctly from llm_token
	// so it can render thinking dimmed without mixing it into
	// the content stream destined for parse.
	rd.reasoningBuf.WriteString(reasoning)
	ctx.Stream("reasoning_token", map[string]interface{}{
		"text": reasoning,
	})
	if rd.reasoningBudgetChars > 0 && rd.reasoningBuf.Len() > rd.reasoningBudgetChars && rd.contentBuf.Len() == 0 {
		rd.reasoningCut = true
	}
}

// readContent takes one content delta: progress for the watchdog, the first
// token's event, the content so far, and the checks that cut a reply that will
// not end well.
func (rd *llmStream) readContent(content string) {
	ctx := rd.ctx
	// Useful progress, and the only kind: bytes of the file itself.
	// Reasoning deltas above deliberately do not reach here.
	rd.progress()
	if !rd.firstTokenSent {
		rd.stopProgressFn() // prompt eval done — kill the poller
		ctx.Stream("llm_first_token", map[string]interface{}{
			"prompt_ms": time.Since(rd.sentAt).Milliseconds(),
		})
		rd.firstTokenSent = true
	}
	rd.contentBuf.WriteString(content)
	ctx.Stream("llm_token", map[string]interface{}{
		"text": content,
	})
	// Content-loop cut. Some models state the right answer then
	// spirals on self-doubt in the CONTENT stream ("...the first line
	// is X. Wait, I can't see the output. I'll just say X. Wait, I
	// can't..." repeating) — the reasoning budget doesn't catch it
	// (that's content, not reasoning_content), so it ran to max_tokens.
	// Detect a repeating tail and cut (streamLooping). Checked
	// periodically to keep it O(n) overall.
	// Raw-emission sub-call: we asked for exactly one fenced block
	// and nothing else, so a reply with no fence opener after a few
	// hundred characters is prose that will run to max_tokens. At
	// 8192 tokens and ~25 tok/s that is ~5 minutes, and the fetch
	// makes two attempts, so a failed @fenced resolution cost a
	// user's session 10 minutes of silence before bouncing.
	// Measured: 03:21:25 request, 03:31:46 "no fenced block after 2
	// attempts" — 621 seconds, and 8 of 20 create sessions hit it.
	if rd.grammar == rawEmissionSentinel && !rd.noFenceCut &&
		rd.contentBuf.Len() > rawFenceGraceChars &&
		!strings.Contains(rd.contentBuf.String(), "```") {
		rd.noFenceCut = true
	}
	if !rd.contentLoopCut && !rd.oldStrCut && rd.contentBuf.Len() > 600 && rd.contentBuf.Len()-rd.lastLoopCheck > 200 {
		rd.lastLoopCheck = rd.contentBuf.Len()
		rd.contentLoopCut = streamLooping(rd.contentBuf.String())
		if !rd.contentLoopCut && !rd.fencedSubCall {
			rd.oldStrCut = rd.oldStrW.runaway(rd.contentBuf.String())
		}
	}
}

// cut ends the read at the first cut the frames so far call for, and says why
// on the stream and on the context. It reports whether the read stops.
func (rd *llmStream) cut() bool {
	ctx := rd.ctx
	if rd.reasoningCut {
		log.Printf("[agent] reasoning budget exceeded (%d chars, ~%d tokens) with no content emitted — cutting the stream and re-prompting",
			rd.reasoningBuf.Len(), rd.reasoningBuf.Len()/4)
		ctx.Stream("reasoning_budget_cut", map[string]interface{}{
			"reasoning_chars": rd.reasoningBuf.Len(),
		})
		ctx.LastStreamCut = "reasoning_budget"
		return true
	}
	if rd.noFenceCut {
		log.Printf("[agent] raw sub-call produced %d chars with no fenced block — cutting rather than running to max_tokens",
			rd.contentBuf.Len())
		ctx.LastStreamCut = "no_fence"
		return true
	}
	if rd.contentLoopCut {
		log.Printf("[agent] content loop detected (%d chars) — model repeating itself; cutting the stream", rd.contentBuf.Len())
		ctx.Stream("content_loop_cut", map[string]interface{}{"chars": rd.contentBuf.Len()})
		ctx.LastStreamCut = "content_loop"
		return true
	}
	if rd.oldStrCut {
		log.Printf("[agent] edit_file old_str on %s stopped matching the file after %d line(s) and ran on (%d bytes) — cutting the stream",
			logPath(rd.oldStrW.Path), rd.oldStrW.MatchedLines, rd.oldStrW.Chars)
		ctx.Stream("old_str_cut", map[string]interface{}{
			"path": rd.oldStrW.Path, "chars": rd.contentBuf.Len(),
			"old_str_chars": rd.oldStrW.Chars, "matched_lines": rd.oldStrW.MatchedLines,
		})
		ctx.LastStreamCut = "old_str_unmatched"
		ctx.LastOldStrCut = rd.oldStrW
		return true
	}
	return false
}

// endOnReadError answers a stream that broke off: a fenced block already in the
// reasoning is still salvaged after a watchdog cut, and otherwise the error
// goes back with the content so far.
func (rd *llmStream) endOnReadError(err error, reqCtx context.Context) (string, int, error) {
	ctx := rd.ctx
	// Our fenced watchdog cancels the request when no CONTENT token
	// arrives in time. A model that streamed the file into
	// reasoning_content instead has the answer sitting in reasoningBuf,
	// and returning the error here would discard it: the reasoning
	// salvage below never runs because this leaves early. Try that same
	// salvage first, but only for a benign watchdog cut, where the
	// session context is still alive (a real user cancel or transport
	// failure is not salvaged).
	if rd.fencedSubCall && (ctx.Ctx == nil || ctx.Ctx.Err() == nil) {
		// Which watchdog fired follows from what had arrived: none arms
		// only once a frame has come, and content re-arms the idle one.
		if reqCtx.Err() != nil {
			switch {
			case rd.firstLine == "":
				rd.watchdogCut = "first_content"
			case rd.contentBuf.Len() == 0:
				rd.watchdogCut = "stalled"
			default:
				rd.watchdogCut = "idle"
			}
		}
		log.Printf("[agent] fenced sub-call stream cut (%v) after %s; wire lines=%d first=%q; reasoning_content held %d chars, content %d",
			err, time.Since(rd.sentAt).Round(time.Millisecond), rd.rawLines,
			truncateStr(rd.firstLine, 120), rd.reasoningBuf.Len(), rd.contentBuf.Len())
		if rd.reasoningBuf.Len() > 0 {
			if body := extractFencedContent(rd.reasoningBuf.String()); body != "" &&
				strings.TrimSpace(body) != rawEmissionSentinel {
				log.Printf("[agent] recovered the fenced block from reasoning_content after a stream cut (%d chars)",
					rd.reasoningBuf.Len())
				return rd.reasoningBuf.String(), rd.totalTokens, nil
			}
		}
	}
	return rd.contentBuf.String(), rd.totalTokens,
		fmt.Errorf("read LLM stream: %w", err)
}

// recoverEmptyContent answers a reply with no content: a fenced block or an
// agent response found in the reasoning is the reply, and otherwise it is empty.
func (rd *llmStream) recoverEmptyContent() (string, int, error) {
	// Raw-emission sub-call: the recovery below only salvages a JSON
	// tool_call envelope, which is the right rule for the agent loop and
	// the wrong one here — this call asked for a fenced block, so a
	// block sitting in reasoning_content is exactly the answer and was
	// being thrown away. Observed: sub-call attempts returning 0 content
	// characters after minutes of generation, with no stream cut, which
	// is tokens going somewhere that is not `content`.
	if rd.grammar == rawEmissionSentinel && rd.reasoningBuf.Len() > 0 {
		if body := extractFencedContent(rd.reasoningBuf.String()); body != "" &&
			strings.TrimSpace(body) != rawEmissionSentinel {
			log.Printf("[agent] raw sub-call emitted its fenced block into reasoning_content (%d chars) — salvaging",
				rd.reasoningBuf.Len())
			return rd.reasoningBuf.String(), rd.totalTokens, nil
		}
	}
	// No content deltas: check reasoning_content. Two cases:
	//
	//   (a) The model put its actual response into the thinking stream
	//       although template-level reasoning is disabled;
	//       reasoning_content holds a JSON tool_call, which is recovered
	//       and parsed normally.
	//
	//   (b) The model emitted ONLY thinking ("Now I need to read...")
	//       and stopped without a response; the reasoning is pure prose
	//       with no tool call to recover. Returning that prose as the
	//       response would parse-error and waste a turn, and a "respond
	//       in JSON only" corrective is useless when the model did not
	//       disobey the format but stopped mid-flow.
	//
	// So recovered reasoning is returned only when it CONTAINS a tool_call
	// envelope; pure prose returns empty and is logged, so the caller's
	// empty-response retry (callLLMConstrained) re-prompts instead of treating
	// prose as a failed response.
	if rd.reasoningBuf.Len() > 0 {
		if recovered, ok := recoverStructuredReasoning(rd.reasoningBuf.String()); ok {
			log.Printf("[agent] empty content but %d chars of reasoning_content contained a structured agent response — recovering",
				rd.reasoningBuf.Len())
			return recovered, rd.totalTokens, nil
		}
		// Pure prose narration in reasoning_content with no tool
		// call. Don't return it — let the caller retry. Logged so
		// the failure mode stays visible.
		log.Printf("[agent] %d chars of reasoning_content had no valid agent envelope — discarding so caller can re-prompt",
			rd.reasoningBuf.Len())
	}
	// Truly nothing (or only narration). Caller's empty-response
	// retry path (callLLMConstrained) will handle.
	return "", rd.totalTokens, nil
}
