// The fenced channel: the sub-call that fetches a file body, what it may spend, and the note when it is used up.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fencedWatchdog arms the fenced sub-call's progress watchdog on cancel, with
// the first-content deadline. progress re-arms it with the idle deadline, and
// armStalled with the shorter one for a stream that has opened.
func fencedWatchdog(cancel context.CancelFunc) (*time.Timer, func(), func()) {
	idle := fencedIdleTimeout()
	stalled := fencedStalledTimeout()
	var mu sync.Mutex
	timer := time.AfterFunc(fencedFirstContentTimeout(), cancel)
	progress := func() {
		mu.Lock()
		defer mu.Unlock()
		timer.Reset(idle)
	}
	// Once the server has sent ANY frame, generation has started, and
	// on a healthy stream the first content frame follows in the same
	// millisecond -- measured at 0.00s across every direct reproduction
	// at small and large context. So a stream that has opened and then
	// says nothing is dead, and waiting the full first-content budget
	// for it only burns the session. Shorten the deadline instead of
	// spending 60s twice per file on a stream that will never speak.
	armStalled := func() {
		mu.Lock()
		defer mu.Unlock()
		timer.Reset(stalled)
	}
	return timer, progress, armStalled
}

// extractModelResponse extracts a ModelResponse from the LLM output,
// handling cases where the model adds text before/after the JSON or
// where the JSON is truncated.
// rawResponseForFence renders the tool call as the assistant turn the
// fenced-content sub-call replays. Marshalling the parsed struct rather than
// reusing raw model text keeps the sub-call deterministic.
func rawResponseForFence(parsed ModelResponse) string {
	b, err := json.Marshal(parsed)
	if err != nil {
		return "{\"type\":\"tool_call\",\"name\":\"write_file\"}"
	}
	return string(b)
}

// rawEmissionSentinel, passed as the grammar argument, sends the request with
// neither GBNF nor response_format: the model replies in free text.
const rawEmissionSentinel = "__raw_text__"

// rawFenceGraceChars is how much prose the raw-emission sub-call may emit
// before a missing fence opener is treated as "not coming". Generous enough
// for a short preamble the model sometimes writes ahead of the block, far
// short of the multi-minute run to max_tokens it replaces.
const rawFenceGraceChars = 800

// fenceTagForPath picks the fence language tag the sub-call prompt asks
// for, from the target file's extension. The prompt hardcoded ```python
// whatever the file was — a .html or .go target got asked for a python
// block (audit finding). Empty means "use a bare fence".
func fenceTagForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py":
		return "python"
	case ".js", ".mjs":
		return "javascript"
	case ".ts":
		return "typescript"
	case ".html", ".htm":
		return "html"
	case ".css":
		return "css"
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".c", ".h":
		return "c"
	case ".cpp", ".cc", ".hpp":
		return "c++"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	case ".sh":
		return "bash"
	case ".json":
		return "json"
	case ".md":
		return "markdown"
	case ".sql":
		return "sql"
	case ".yaml", ".yml":
		return "yaml"
	default:
		return ""
	}
}

// Fenced-fetch progress bounds. The sub-call asks for ONE fenced block and
// nothing else, so the only useful output is CONTENT: a model that streams
// reasoning to max_tokens and never opens a fence is the zero-byte failure,
// and reasoning must not look like progress.
//
// Measured on the seed-20260901 run: 9 of 17 zero-byte failures ran 175-311s
// each, and two attempts can consume ~10 minutes. These bounds are NOT
// derived from whole-fetch duration -- a successful fetch legitimately ran
// 217s while producing content the whole way, and cutting on total elapsed
// would have killed it.
//
// They are conservative defaults over the quantities that DO discriminate:
// observed time-to-first-token is p50 378ms, p90 2.9s, p99 4.4s, max 11.8s
// across 540 calls, so 60s to first CONTENT is ~5x the worst observed start
// with headroom for a reasoning preamble; and at the ~25 tok/s decode rate
// this deployment sustains, 30s of complete silence mid-file is ~750 tokens
// of nothing, which is not generation in progress. Both are env-overridable
// on the existing envOr pattern and are pending real-model canary validation
// before Phase 2 is declared complete.
const (
	defaultFencedFirstContentSec = 60
	defaultFencedIdleSec         = 30
	// Generous next to the 0.00s gap measured between the opening frame and
	// the first content frame on every healthy stream, and far cheaper than
	// spending the whole first-content budget on a stream that has gone
	// silent after opening.
	defaultFencedStalledSec = 25
)

// fencedStreamStats is what the wire showed during one fenced sub-call.
type fencedStreamStats struct {
	FirstFrame     time.Duration // since the request; 0 when no frame came
	WireLines      int
	FirstLine      string
	ReasoningChars int
	ContentChars   int
	// Cut names the watchdog that ended the stream: first_content (no
	// frame at all), stalled (frames, but no content), or idle (content,
	// then none). Empty when no watchdog cut it.
	Cut string
}

func fencedFirstContentTimeout() time.Duration {
	return envDurationSec("ATLAS_FENCED_FIRST_CONTENT_SEC", defaultFencedFirstContentSec)
}

func fencedIdleTimeout() time.Duration {
	return envDurationSec("ATLAS_FENCED_IDLE_SEC", defaultFencedIdleSec)
}

// fencedStalledTimeout bounds the wait for the FIRST content frame once the
// server has already opened the stream. Separate from the first-content
// budget, which also has to cover prompt evaluation: past the opening frame
// the model is demonstrably generating, and a healthy stream delivers content
// in the same millisecond.
func fencedStalledTimeout() time.Duration {
	return envDurationSec("ATLAS_FENCED_STALL_SEC", defaultFencedStalledSec)
}

// maxFencedOverrideSec caps the override. A bound of hours is indistinguishable
// from no bound, and the failure this exists to stop is a ~300s stall, so an
// absurd value must fall back rather than quietly disable the safety net.
const maxFencedOverrideSec = 600

func envDurationSec(key string, def int) time.Duration {
	if v := strings.TrimSpace(envOr(key, "")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= maxFencedOverrideSec {
			return time.Duration(n) * time.Second
		}
		log.Printf("[agent] ignoring %s=%q (want 1..%d seconds) — using %ds",
			key, v, maxFencedOverrideSec, def)
	}
	return time.Duration(def) * time.Second
}

// maxFencedFailuresPerPath is the whole-session allowance: one attempt plus one
// retry. Past that the model must change mutation strategy — inlining the body
// in write_file, or editing instead of rewriting — because a third resolution
// has never been observed to succeed where two failed and each one costs a full
// generation.
const maxFencedFailuresPerPath = 2

// fencedBudgetExhausted reports whether this path has already spent its
// session allowance. Checked BEFORE any generation starts.
func fencedBudgetExhausted(ctx *AgentContext, path string) bool {
	return ctx != nil && ctx.FencedFailures[fencedKey(ctx, path)] >= maxFencedFailuresPerPath
}

// fencedSessionStallLimit is how many zero-content fenced sub-calls a session
// tolerates before the channel is turned off for the rest of the run. One: the
// stall reflects the session's llama-server state, so the first stall predicts
// the rest, and every later @fenced would pay another ~25s watchdog cut for
// nothing. Inline writes are the fallback, defended by the swallowed-content
// detector (swallowedContentFeedback).
const fencedSessionStallLimit = 1

// fencedChannelDisabledForSession reports whether the fenced channel has
// stalled enough this session to be turned off for every remaining write.
func fencedChannelDisabledForSession(ctx *AgentContext) bool {
	return ctx != nil && ctx.FencedStalls >= fencedSessionStallLimit
}

// fencedKey canonicalises the target so equivalent spellings share one
// allowance. Keying on raw model input let "solve.py" and "./solve.py" hold
// separate budgets, which is the same restart-the-counter hole one level
// down.
func fencedKey(ctx *AgentContext, path string) string {
	if ctx == nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(resolveAgentPath(ctx, path))
}

// charge records one consumed attempt against the path's session allowance.
func charge(ctx *AgentContext, path string) {
	if ctx == nil {
		return
	}
	if ctx.FencedFailures == nil {
		ctx.FencedFailures = map[string]int{}
	}
	ctx.FencedFailures[fencedKey(ctx, path)]++
}

// fencedReserve is the time held back for validating the write and sending an
// honest terminal once a fetch returns.
const fencedReserve = 20 * time.Second

// fencedFitsRemainingBudget reports whether the work deadline leaves room
// for one first-content wait plus fencedReserve to validate the write and
// send an honest terminal. A context with no deadline has no budget to
// reserve from, so it returns true and the fetch is bounded by the progress
// watchdog alone.
func fencedFitsRemainingBudget(ctx *AgentContext) bool {
	if ctx == nil || ctx.Ctx == nil {
		return true
	}
	deadline, ok := ctx.Ctx.Deadline()
	if !ok {
		return true // no session budget exists to reserve from
	}
	need := fencedFirstContentTimeout() + fencedReserve
	return time.Until(deadline) >= need
}

// fencedFetchRefusal is the reason the session makes no fenced fetch for path
// at all, or nil: the channel is off for the session, the path has spent its
// allowance, or too little of the session is left to fetch a file and check it.
func fencedFetchRefusal(ctx *AgentContext, path string) error {
	if fencedChannelDisabledForSession(ctx) {
		// The channel already stalled this session; a sub-call would only
		// stall again. Fail without making it, so no watchdog time is spent.
		return fmt.Errorf("the fenced channel stalled earlier this session "+
			"and is off for the rest of the run; send %s inline or edit it instead", path)
	}
	if fencedBudgetExhausted(ctx, path) {
		return fmt.Errorf("fenced resolution for %s has already failed %d times "+
			"this session; send the file inline or edit it instead", path,
			ctx.FencedFailures[fencedKey(ctx, path)])
	}
	if !fencedFitsRemainingBudget(ctx) {
		return fmt.Errorf("not enough session budget left to resolve %s "+
			"and still validate the result", path)
	}
	return nil
}

// fencedFetch is one fenced fetch across its attempts: the conversation the
// sub-call is sent, which grows by each unusable reply and the note that asks
// again, and the error of the last attempt that was cut.
type fencedFetch struct {
	ctx     *AgentContext
	path    string
	tag     string
	msgs    []AgentMessage
	lastErr error
}

// fencedAnswer is what one fenced attempt brought back, read the way the
// inline path reads a body.
type fencedAnswer struct {
	reply          string
	framing        fenceFraming
	content        string
	err            error
	got            bool
	cutWithContent bool
}

// ask makes one fenced attempt: the sub-call, its accounting, the reading of
// its reply, and the two events that record the attempt.
func (f *fencedFetch) ask(grammar string, attempt int, attemptStart time.Time) fencedAnswer {
	ctx, path, tag, msgs := f.ctx, f.path, f.tag, f.msgs
	reply, tokens, err := callLLMOnceRestating(ctx, msgs, 0.2, grammar, path)
	elapsed := time.Since(attemptStart)
	// Every attempt is a real generation and is accounted whether or
	// not it yielded a usable block — an unaccounted sub-call made the
	// run totals lie by one generation per written file.
	ctx.TotalTokens += tokens
	ctx.FencedCalls++
	ctx.FencedTokens += tokens
	// Under the fence grammar a code file may end on three backticks
	// (fenceBlockGrammar); that line is the closer the grammar accepted.
	if isFenceBlockGrammar(grammar) && fenceShortCloserAllowed(tag) {
		reply = closeShortFence(reply)
	}
	// The same framing decision the inline path makes, so a reply the
	// parent would refuse inline cannot be accepted here instead.
	framing, content := classifyFencedPayload(reply)
	// The model sometimes wraps the SENTINEL in the fence instead of the
	// file — measured: "```python\n@fenced\n```". That extracts as
	// non-empty and would land a file whose entire contents are the word
	// @fenced, so it counts as no block and the attempt is retried.
	if strings.TrimSpace(content) == rawEmissionSentinel ||
		strings.TrimSpace(content) == "@fenced" {
		log.Printf("[agent] fenced reply for %s contained only the sentinel — treating as no block", path)
		content = ""
	}
	got := content != ""
	contentBytes := len(content)
	Emit(Envelope{
		EventID:    NewEventID(),
		Timestamp:  float64(time.Now().UnixNano()) / 1e9,
		Type:       EvtStageEnd,
		Stage:      "fenced_fetch",
		DurationMS: elapsed.Milliseconds(),
		Payload: map[string]interface{}{
			"success":          err == nil && got,
			"path":             path,
			"attempt":          attempt + 1,
			"generated_tokens": tokens,
			"content_bytes":    contentBytes,
			"total_tokens":     ctx.TotalTokens,
		},
	})
	// A cut that came after the model had written content is not a stall:
	// the channel works, and the block was not closed (under the fence
	// grammar the model's three-backtick closer is a body line, so the
	// attempt runs until the watchdog cuts it). It is answered like any
	// reply without a usable block, below, and the retry without the
	// grammar follows. Counting it as a stall turned the channel off for
	// the session and skipped that retry.
	cutWithContent := err != nil && strings.TrimSpace(reply) != "" &&
		(ctx.Ctx == nil || ctx.Ctx.Err() == nil)
	outcome := "unusable"
	switch {
	case err != nil && !cutWithContent && ctx.Ctx != nil && ctx.Ctx.Err() != nil:
		outcome = "cancelled"
	case err != nil && !cutWithContent:
		outcome = "stalled"
	case got:
		outcome = "used"
	}
	emitFencedFetch(ctx, path, attempt, grammar, elapsed, tokens, outcome)
	return fencedAnswer{reply: reply, framing: framing, content: content, err: err, got: got,
		cutWithContent: cutWithContent}
}

// settle answers one attempt. A stall is charged, and ends the fetch when the
// session itself stopped; a usable block ends it and clears the path's
// failures; any other reply is charged, logged and asked for again. It
// returns the block, whether the fetch ends here, and the fetch's error.
func (f *fencedFetch) settle(a fencedAnswer, attempt int) (string, bool, error) {
	ctx, path, tag := f.ctx, f.path, f.tag
	reply, framing, content, err := a.reply, a.framing, a.content, a.err
	got, cutWithContent := a.got, a.cutWithContent
	if err != nil && !cutWithContent {
		// Every way this attempt can end WITHOUT a fenced block charges
		// the session: watchdog cancellation, transport error, HTTP
		// failure. Not charging here is how the black-box loop issued one
		// unbounded fetch per turn — the allowance was only ever spent by
		// a clean empty reply.
		charge(ctx, path)
		// The SESSION being cancelled ends everything; the watchdog
		// cancelling its own child is a recoverable zero-byte failure and
		// the one permitted retry may still follow, bounded by the
		// allowance the loop already counted.
		if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
			return "", true, err
		}
		// A watchdog-cut zero-content fetch. Counted session-wide (not just
		// per-path) because the next file's fetch will stall the same way;
		// once this reaches the limit the channel is off for the run.
		ctx.FencedStalls++
		f.lastErr = err
		return "", false, nil
	}
	if cutWithContent {
		f.lastErr = err // a closed block is still used below; an open one is retried
	}
	if got {
		// A successful resolution clears the consecutive-failure state:
		// the path is healthy again and a later stall gets its own budget.
		if ctx.FencedFailures != nil {
			delete(ctx.FencedFailures, fencedKey(ctx, path))
		}
		return content, true, nil
	}
	// Charge the session, not the local loop, so the allowance survives
	// into the next write_file call for this path.
	charge(ctx, path)
	// What the model sent instead is the whole diagnosis, and without it
	// "no fenced block after 2 attempts" says only that something went
	// wrong. Measured a 56% failure rate on this fetch with no way to see
	// why: the same request reproduced in a short context returns a clean
	// block every time, so the cause lives in the session context and
	// cannot be found without the reply.
	log.Printf("[agent] fenced attempt %d for %s produced no usable block (%s, %d chars, cut=%q, session failures %d/%d): %q",
		attempt+1, path, framing, len(reply), ctx.LastStreamCut,
		ctx.FencedFailures[fencedKey(ctx, path)], maxFencedFailuresPerPath,
		safeTextSummary(reply))
	f.msgs = append(f.msgs, AgentMessage{Role: "assistant", Content: reply},
		AgentMessage{Role: "user", Content: fencedRetryNote(framing, tag)})
	return "", false, nil
}

// emitFencedFetch records one fenced attempt (#254): what the wire showed
// and what became of it. The outcome is the branch the fetch loop takes:
// used, unusable (retried while attempts remain), stalled (charged against
// the session's stall limit), or cancelled.
func emitFencedFetch(ctx *AgentContext, path string, attempt int, grammar string,
	elapsed time.Duration, tokens int, outcome string) {
	fs := ctx.LastFencedStream
	g := "raw"
	if isFenceBlockGrammar(grammar) {
		g = "fence"
	}
	ev := map[string]interface{}{
		"path": path, "attempt": attempt + 1, "grammar": g, "outcome": outcome,
		"elapsed_ms": elapsed.Milliseconds(), "tokens": tokens,
		"first_frame_ms": fs.FirstFrame.Milliseconds(), "wire_lines": fs.WireLines,
		"content_chars": fs.ContentChars, "reasoning_chars": fs.ReasoningChars,
	}
	if fs.Cut != "" {
		ev["cut"] = fs.Cut
	}
	if ctx.LastStreamCut != "" {
		ev["stream_cut"] = ctx.LastStreamCut
	}
	if fs.ContentChars == 0 && fs.FirstLine != "" {
		ev["first_line"] = fs.FirstLine
	}
	ctx.Stream("fenced_fetch", ev)
}

// --- The exhausted fenced channel -------------------------------------------
//
// The allowance stops the generations; it does not tell the model anything it
// can act on. In the frozen run debounce2 asked for the same resolution 147
// times and was refused 144 of them with the same sentence, because the only
// thing the refusal could say was that the channel had failed.
//
// This is the earlier branch than the C5 run-first state and stays separate
// from it: no warned artifact is required, and the trigger is the allowance
// being spent rather than a pending demand to run something.
//
// Offered once per canonical path. It reads and never writes, runs no command,
// forces no tool, and starts no generation.
func fencedChannelRecovery(ctx *AgentContext, st *runState, relPath string) string {
	if ctx == nil || st == nil {
		return ""
	}
	// Only when the channel is genuinely spent for THIS path.
	if !fencedBudgetExhausted(ctx, relPath) {
		return ""
	}
	// A run that is already ending owns its own terminal.
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		return ""
	}
	key := fencedKey(ctx, relPath)
	if st.fencedChannelClosed[key] {
		return ""
	}
	if tooLateToRecover(ctx, "the fenced-channel recovery for "+relPath) {
		return ""
	}
	if st.fencedChannelClosed == nil {
		st.fencedChannelClosed = map[string]bool{}
	}
	st.fencedChannelClosed[key] = true
	log.Printf("[agent] fenced channel spent for %s — offering the alternatives once", relPath)

	var sb strings.Builder
	fmt.Fprintf(&sb, "The fenced-content channel for %s is used up in this session: "+
		"the sub-call was asked for the file and did not return one, and it will not be "+
		"asked again for this path. Sending \"content\": \"@fenced\" for %s cannot "+
		"succeed now, however many times it is repeated.\n\n", relPath, relPath)

	if source, truncated, err := boundedCurrentSource(ctx, relPath); err == nil {
		writeCurrentSource(&sb, relPath, source, truncated)
		sb.WriteString("\n\n")
	} else {
		fmt.Fprintf(&sb, "%s is not on disk yet, so there is nothing to show you "+
			"of it.\n\n", relPath)
	}
	if detail := currentValidationDetail(ctx, relPath); detail != "" {
		fmt.Fprintf(&sb, "The last thing checked about those exact bytes: %s\n\n", detail)
	}
	fmt.Fprintf(&sb, "What still works for %s: send write_file with the complete file "+
		"INLINE in the content field; make a targeted change with edit_file or "+
		"replace_lines; read it with read_file; or run it with run_command and read the "+
		"real error. Pick one of those.", relPath)
	return sb.String()
}
