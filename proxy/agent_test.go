package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTrimMessagesPinsRecentUser(t *testing.T) {
	// system + user instruction + 10 tool exchanges. keepLast=8 means the
	// raw tail starts at index 3 — the user instruction (index 1) is gone.
	// Pin must restore it.
	msgs := []AgentMessage{{Role: "system", Content: "sys"}}
	msgs = append(msgs, AgentMessage{Role: "user", Content: "fix the bug"})
	for i := 0; i < 10; i++ {
		msgs = append(msgs,
			AgentMessage{Role: "assistant", Content: "tool call"},
			AgentMessage{Role: "tool", Content: "result"})
	}

	got := trimMessages(msgs, 8)

	if got[0].Role != "system" {
		t.Fatalf("got[0].Role = %q, want system", got[0].Role)
	}
	if got[1].Role != "user" || got[1].Content != "fix the bug" {
		t.Fatalf("got[1] = %+v, want pinned user 'fix the bug'", got[1])
	}
	if len(got) != 1+1+8 {
		t.Errorf("len(got) = %d, want 10 (system + pin + 8 tail)", len(got))
	}
}

func TestTrimMessagesNoDuplicateWhenPinInWindow(t *testing.T) {
	// Short conversation: user instruction is already in the tail window.
	// Don't duplicate it.
	msgs := []AgentMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2 — current"},
		{Role: "assistant", Content: "a2"},
		{Role: "tool", Content: "t1"},
		{Role: "assistant", Content: "a3"},
		{Role: "tool", Content: "t2"},
		{Role: "assistant", Content: "a4"},
		{Role: "tool", Content: "t3"},
		{Role: "assistant", Content: "a5"},
		{Role: "tool", Content: "t4"},
		{Role: "assistant", Content: "a6"},
	}
	// 13 messages, keepLast=8 → tailStart=5. Most-recent user is at idx 3,
	// outside window → gets pinned.
	got := trimMessages(msgs, 8)
	userCount := 0
	for _, m := range got {
		if m.Role == "user" {
			userCount++
		}
	}
	if userCount != 1 {
		t.Errorf("user count = %d, want 1 (no duplicate pin)", userCount)
	}
	if got[1].Content != "u2 — current" {
		t.Errorf("pinned msg = %q, want most-recent user 'u2 — current'", got[1].Content)
	}
}

func TestTrimMessagesPinAlreadyInTailNoDuplicate(t *testing.T) {
	// User msg is inside tail window — function shouldn't pin (would dup).
	msgs := []AgentMessage{
		{Role: "system", Content: "sys"},
		{Role: "assistant", Content: "old1"},
		{Role: "tool", Content: "old2"},
		{Role: "assistant", Content: "old3"},
		{Role: "user", Content: "current ask"},
		{Role: "assistant", Content: "tail1"},
		{Role: "tool", Content: "tail2"},
		{Role: "assistant", Content: "tail3"},
		{Role: "tool", Content: "tail4"},
		{Role: "assistant", Content: "tail5"},
		{Role: "tool", Content: "tail6"},
		{Role: "assistant", Content: "tail7"},
	}
	// 12 messages, keepLast=8 → tailStart=4. User at idx 4 → in window.
	got := trimMessages(msgs, 8)
	userCount := 0
	for _, m := range got {
		if m.Role == "user" {
			userCount++
		}
	}
	if userCount != 1 {
		t.Errorf("user count = %d, want 1 (already in tail, no dup)", userCount)
	}
	if len(got) != 1+8 {
		t.Errorf("len(got) = %d, want 9 (system + 8 tail, no pin)", len(got))
	}
}

func TestTrimMessagesShortConversationUnchanged(t *testing.T) {
	msgs := []AgentMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	got := trimMessages(msgs, 8)
	if len(got) != 3 {
		t.Errorf("len(got) = %d, want 3 (under threshold, no trim)", len(got))
	}
}

func TestTrimMessagesPriorHistoryDoesNotConfusePin(t *testing.T) {
	// Reproduces the bug: PriorHistory put a prior-turn user msg at idx 1.
	// Hardcoded ctx.Messages[1] would have pinned the WRONG user message.
	// trimMessages scans backwards, so it picks the current-turn user.
	msgs := []AgentMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "PRIOR turn ask"},        // from PriorHistory
		{Role: "assistant", Content: "PRIOR turn reply"}, // from PriorHistory
		{Role: "user", Content: "CURRENT turn ask"},
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs,
			AgentMessage{Role: "assistant", Content: "tool call"},
			AgentMessage{Role: "tool", Content: "result"})
	}

	got := trimMessages(msgs, 8)
	if got[1].Content != "CURRENT turn ask" {
		t.Errorf("pinned = %q, want 'CURRENT turn ask' (most-recent, not idx-1)", got[1].Content)
	}
}

func TestClassifyAgentTierTrivialChatStaysT0(t *testing.T) {
	for _, msg := range []string{
		"hi", "Hello", "hey", "thanks", "thank you", "ok", "yes", "no",
		"perfect", "got it", "cool", "bye",
	} {
		if got := classifyAgentTier(msg); got != Tier0Conversational {
			t.Errorf("classifyAgentTier(%q) = %v, want T0", msg, got)
		}
	}
}

func TestClassifyAgentTierEmptyOrSubFiveCharsStaysT0(t *testing.T) {
	for _, msg := range []string{"", " ", "  \n", "abc", "a"} {
		if got := classifyAgentTier(msg); got != Tier0Conversational {
			t.Errorf("classifyAgentTier(%q) = %v, want T0", msg, got)
		}
	}
}

func TestClassifyAgentTierRealTaskDefaultsToT2(t *testing.T) {
	// These were T1 under the old cascade — too quick a fall-through.
	// New rule: anything that isn't trivial chat is T2 minimum, so V3
	// has a chance to fire on every real task.
	t2Prompts := []string{
		"fix the issues in the flask web app",
		"why isn't this rendering",
		"add a button to the page",
		"the form submission is broken",
		"can you check what's wrong",
		"make a quick utility",
		"refactor this function",
		"remove the unused import",
	}
	for _, msg := range t2Prompts {
		if got := classifyAgentTier(msg); got < Tier2Medium {
			t.Errorf("classifyAgentTier(%q) = %v, want >= T2", msg, got)
		}
	}
}

func TestClassifyAgentTierMultiComponentIsTreatedAsWork(t *testing.T) {
	// This asserted Tier3Hard when classifyAgentTier had a multi-component
	// branch. That branch was removed because T3 was indistinguishable from
	// T2 at every consumer: TierMaxTurns leaves T1/T2/T3 uncapped alike,
	// shouldGeneratePlan tests only Tier0Conversational, and v3-service
	// reads the tier into a log line without branching on it. The contract
	// that actually holds is that these are work, not chat.
	workPrompts := []string{
		"build a full application with frontend and backend authentication",
		"set up middleware, database, and authentication for the api",
	}
	for _, msg := range workPrompts {
		if got := classifyAgentTier(msg); got == Tier0Conversational {
			t.Errorf("classifyAgentTier(%q) = T0 — a multi-component build must not be capped at 5 turns", msg)
		}
	}
}

// The 2026-07-21 dogfooding regression. This message names no file, matches
// no task-verb list, and is not a question — it describes desired behaviour.
// Classified T0 it was capped at 5 turns and produced a zero-tool-call
// non-answer instead of an edit.
func TestClassifyAgentTierBehaviourDescriptionIsWork(t *testing.T) {
	msgs := []string{
		"the snake is still moving way too fast, please slow it down significantly",
		"the sidebar overlaps the content on narrow screens",
		"it crashes when the list is empty",
	}
	for _, msg := range msgs {
		if got := classifyAgentTier(msg); got == Tier0Conversational {
			t.Errorf("classifyAgentTier(%q) = T0 — describing broken behaviour is a task; "+
				"absence of a recognized task word is not evidence of chat", msg)
		}
	}
}

func TestClassifyAgentTierGreetingsAndQuestionsAreConversational(t *testing.T) {
	for _, msg := range []string{"hi", "thanks", "ok", "yep", "  hello  "} {
		if got := classifyAgentTier(msg); got != Tier0Conversational {
			t.Errorf("classifyAgentTier(%q) = %v, want T0", msg, got)
		}
	}
	for _, msg := range []string{
		"why does the game store direction as a string",
		"what does the lens actually score here",
		"is the sandbox mounted read-only?",
	} {
		if got := classifyAgentTier(msg); got != Tier0Conversational {
			t.Errorf("classifyAgentTier(%q) = %v, want T0 (question shape)", msg, got)
		}
	}
}

// A question that is also a request must be treated as the request.
func TestClassifyAgentTierTaskWinsOverQuestionShape(t *testing.T) {
	for _, msg := range []string{
		"can you fix the login bug?",
		"could you add a logout button to the navbar?",
		"why is it broken - can you repair the parser?",
	} {
		if got := classifyAgentTier(msg); got != Tier2Medium {
			t.Errorf("classifyAgentTier(%q) = %v, want T2 — task intent outranks question shape", msg, got)
		}
	}
}

func TestClassifyParseFailureTruncatedEditFile(t *testing.T) {
	// Real shape from the May 2026 user logs: tool_call with edit_file
	// + huge old_str cut mid-string. Feedback should call out
	// truncation explicitly so the model shrinks the next attempt.
	raw := `{"type":"tool_call","name":"edit_file","args":{"path":"snake/app.py","old_str":"@app.route('/')\ndef index():\n    return render_template('index.html')\n\n@app.route('/product')\ndef product():\n    return render_template('product.html')\n\n@app.route('/solutions')\ndef solutions():\n    return render_template('solutions.html')\n\n@app.route('/pricing"`

	_, got := classifyParseFailure(raw, "")
	if !strings.Contains(got, "TRUNCATED") {
		t.Errorf("expected TRUNCATED callout, got %q", got)
	}
	if !strings.Contains(got, "shrink") && !strings.Contains(got, "smaller") {
		t.Errorf("expected actionable shrink advice, got %q", got)
	}
}

func TestClassifyParseFailureEmptyResponse(t *testing.T) {
	if _, got := classifyParseFailure("", ""); !strings.Contains(got, "empty") {
		t.Errorf("empty input should mention empty, got %q", got)
	}
	if _, got := classifyParseFailure("   \n\t ", ""); !strings.Contains(got, "empty") {
		t.Errorf("whitespace-only should be treated as empty, got %q", got)
	}
}

func TestClassifyParseFailureMalformedToolCall(t *testing.T) {
	// Looks like a tool_call but ends cleanly — different feedback
	// than truncation.
	raw := `{"type":"tool_call","name":"read_file","args":{"path":"app.py",}}`
	_, got := classifyParseFailure(raw, "")
	if strings.Contains(got, "TRUNCATED") {
		t.Errorf("clean-ending malformed shouldn't say TRUNCATED, got %q", got)
	}
}

func TestClassifyParseFailureProse(t *testing.T) {
	raw := "Here's what I'll do: I'll read the file first..."
	_, got := classifyParseFailure(raw, "")
	if !strings.Contains(got, "JSON") {
		t.Errorf("prose response should get JSON-only nudge, got %q", got)
	}
}

// budgetedKeepLast must COUNT the pinned messages (recent user + recent
// file read) — trimMessages re-injects them even when they're outside
// the tail, so ignoring them under-budgets the real prompt (observed
// live as a llama-server exceed_context_size 400: 32844 > 32768).
func TestBudgetedKeepLastCountsPinnedFile(t *testing.T) {
	t.Setenv("ATLAS_CTX_SIZE", "131072")
	t.Setenv("ATLAS_PARALLEL_SLOTS", "4")
	// Budget ≈ 32768 - 8192 - 2048 - 4096 = 18432 tokens.
	bigFile := strings.Repeat("x", 40000) // ~10k tokens, pinned read_file
	msgs := []AgentMessage{{Role: "system", Content: "sys"}}
	msgs = append(msgs, AgentMessage{Role: "user", Content: "task"})
	msgs = append(msgs, AgentMessage{Role: "tool", ToolName: "read_file", Content: bigFile})
	// 40 tool exchanges of ~1k tokens each — far beyond budget together
	// with the pinned file.
	chunk := strings.Repeat("y", 4000)
	for i := 0; i < 40; i++ {
		msgs = append(msgs,
			AgentMessage{Role: "assistant", Content: "call"},
			AgentMessage{Role: "tool", Content: chunk})
	}

	keep := budgetedKeepLast(msgs)
	kept := trimMessages(msgs, keep)

	// Sum the estimate over what trimMessages actually keeps; it must
	// fit the budget (the old bug: pinned file was re-injected on top
	// of an already-full tail).
	total := 0
	for _, m := range kept {
		total += estTokens(m.Content)
	}
	if budget := conversationTokenBudget(); total > budget {
		t.Errorf("kept set estimates %d tokens > budget %d — pins not counted", total, budget)
	}
	if keep < 8 {
		t.Errorf("keep = %d, floor is 8", keep)
	}
}

// Overflow detection keys on llama.cpp's error type string and message.
func TestIsContextOverflow(t *testing.T) {
	err := fmt.Errorf(`LLM returned 400: {"error":{"code":400,"message":"request (32844 tokens) exceeds the available context size (32768 tokens), try increasing it","type":"exceed_context_size_error"}}`)
	if !isContextOverflow(err) {
		t.Error("expected overflow detection on exceed_context_size_error")
	}
	if isContextOverflow(fmt.Errorf("LLM returned 500: upstream crashed")) {
		t.Error("false positive on unrelated LLM error")
	}
	if isContextOverflow(nil) {
		t.Error("false positive on nil")
	}
}

// --- repetition sampling ---------------------------------------------------

func TestApplyRepetitionSamplingDefaultsOffBecauseItPenalisesCopying(t *testing.T) {
	// Flipped from on-by-default. DRY penalises "extending a sequence that
	// already occurred in the input", which is the definition of copying an
	// anchor out of a file the model just read.
	//
	// The old default was 0.8 with dry_penalty_last_n=2048, believed to bound
	// the scan to the current generation. It does not: llama-server's
	// init_sampler() accepts every PROMPT token into the same ring buffer, so
	// the read_file result is inside it. Penalty is
	// multiplier * base^(matched-allowed_length), so a 12-token match costs
	// -23.0 logits and the correct continuation loses to the runner-up.
	// Observed as scoreElement -> scorerElement.
	body := map[string]interface{}{}
	applyRepetitionSampling(body)

	for _, k := range []string{"dry_multiplier", "dry_base", "dry_allowed_length", "dry_penalty_last_n"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s must not be set by default, got %v", k, body[k])
		}
	}
	// repeat_penalty scores individual tokens and mangles code indentation;
	// it must stay off unless explicitly opted into.
	if _, ok := body["repeat_penalty"]; ok {
		t.Errorf("repeat_penalty must not be set by default, got %v", body["repeat_penalty"])
	}
}

func TestDryStillConfigurableBackOn(t *testing.T) {
	// The escape hatch has to work, or the change is irreversible in the field.
	t.Setenv("ATLAS_DRY_MULTIPLIER", "0.8")
	body := map[string]interface{}{}
	applyRepetitionSampling(body)
	if body["dry_multiplier"] != 0.8 {
		t.Fatalf("dry_multiplier = %v, want 0.8 when explicitly set", body["dry_multiplier"])
	}
	if body["dry_allowed_length"] != 6 {
		t.Fatalf("dry_allowed_length = %v, want 6", body["dry_allowed_length"])
	}
}

func TestApplyRepetitionSamplingDryDisabledByEnv(t *testing.T) {
	t.Setenv("ATLAS_DRY_MULTIPLIER", "0")
	body := map[string]interface{}{}
	applyRepetitionSampling(body)

	for _, k := range []string{"dry_multiplier", "dry_base", "dry_allowed_length", "dry_penalty_last_n"} {
		if _, ok := body[k]; ok {
			t.Fatalf("%s set despite ATLAS_DRY_MULTIPLIER=0", k)
		}
	}
}

func TestApplyRepetitionSamplingRepeatPenaltyOptIn(t *testing.T) {
	t.Setenv("ATLAS_REPEAT_PENALTY", "1.1")
	body := map[string]interface{}{}
	applyRepetitionSampling(body)

	if body["repeat_penalty"] != 1.1 {
		t.Fatalf("repeat_penalty = %v, want 1.1", body["repeat_penalty"])
	}
	if body["repeat_last_n"] != 64 {
		t.Fatalf("repeat_last_n = %v, want 64", body["repeat_last_n"])
	}
}

func TestEnvFloatOrAndEnvIntOrFallBackOnGarbage(t *testing.T) {
	t.Setenv("ATLAS_TEST_FLOAT", "not-a-float")
	t.Setenv("ATLAS_TEST_INT", "not-an-int")
	if got := envFloatOr("ATLAS_TEST_FLOAT", 1.75); got != 1.75 {
		t.Fatalf("envFloatOr on garbage = %v, want fallback 1.75", got)
	}
	if got := envIntOr("ATLAS_TEST_INT", 6); got != 6 {
		t.Fatalf("envIntOr on garbage = %v, want fallback 6", got)
	}
}

// Tests for the /cancel endpoint.

func TestCancelEndpointAbortsSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	activeSessions.Store("sess-abc", &sessionCancel{cancel: cancel})

	body, _ := json.Marshal(map[string]string{"session_id": "sess-abc"})
	req := httptest.NewRequest("POST", "/cancel", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleCancel(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp["cancelled"] {
		t.Errorf("cancelled = false, want true")
	}
	// Session should be removed (idempotent — second cancel returns 404).
	if _, ok := activeSessions.Load("sess-abc"); ok {
		t.Errorf("session not removed from map after cancel")
	}
	// And the cancel func must have actually fired.
	select {
	case <-ctx.Done():
		// good
	default:
		t.Errorf("context not cancelled")
	}
}

func TestSessionCleanupOnlyRemovesOwnEntry(t *testing.T) {
	// Two turns racing on the same session_id: the second Store overwrites
	// the first entry, so the first turn's CompareAndDelete must be a no-op
	// and leave the second turn's cancel func registered.
	first := &sessionCancel{cancel: func() {}}
	second := &sessionCancel{cancel: func() {}}
	activeSessions.Store("sess-dup", first)
	activeSessions.Store("sess-dup", second)
	defer activeSessions.Delete("sess-dup")

	activeSessions.CompareAndDelete("sess-dup", first)

	v, ok := activeSessions.Load("sess-dup")
	if !ok {
		t.Fatalf("second turn's entry was removed by the first turn's cleanup")
	}
	if v.(*sessionCancel) != second {
		t.Errorf("registry holds wrong entry after first turn's cleanup")
	}

	// The owning turn's cleanup still removes its own entry.
	activeSessions.CompareAndDelete("sess-dup", second)
	if _, ok := activeSessions.Load("sess-dup"); ok {
		t.Errorf("second turn's cleanup did not remove its own entry")
	}
}

func TestCancelEndpointUnknownSessionReturns404(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"session_id": "does-not-exist"})
	req := httptest.NewRequest("POST", "/cancel", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleCancel(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestCancelEndpointRejectsGet(t *testing.T) {
	req := httptest.NewRequest("GET", "/cancel", nil)
	rec := httptest.NewRecorder()
	handleCancel(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestCancelEndpointRequiresSessionID(t *testing.T) {
	body, _ := json.Marshal(map[string]string{})
	req := httptest.NewRequest("POST", "/cancel", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleCancel(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// Reproduces the May 8 categorizer miss: a response starting with
// `{"type":"tool_call"` and containing &lt;/&gt; entities was being
// categorized as `malformed_tool` even though the html_entities
// branch should fire first. Locks the expected behavior so the bug
// can't regress silently.
func TestCategorizeParseFailureHtmlEntitiesShape(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			"tool_call envelope with HTML-entity-encoded args",
			`{"type":"tool_call","name":"edit_file","args":{"path":"x.html","old_str":"&lt;!DOCTYPE html&gt;\n&lt;html&gt;\n","new_str":"&lt;!DOCTYPE html&gt;\n&lt;html&gt;\n"}}`,
			"html_entities",
		},
		{
			"prose preamble + tool_call + entities",
			"Now I can see the existing dashboard.html content. I'll use edit_file to replace the entire file content with the new dashboard template.\n\n" +
				`{"type":"tool_call","name":"edit_file","args":{"path":"x.html","old_str":"&lt;!DOCTYPE html&gt;","new_str":"&lt;!DOCTYPE html&gt;"}}`,
			"html_entities",
		},
		{
			"tool_call envelope truncated, no entities",
			`{"type":"tool_call","name":"edit_file","args":{"path":"x.py","old_str":"def foo():\n    return 1","new_str":"def foo():\n    return`,
			"truncated_tool",
		},
		{
			"tool_call envelope, malformed JSON, no entities",
			`{"type":"tool_call","name":"edit_file","args":{"path":"x.py","old_str":"def foo():","new_str":"def bar(): }}`,
			"malformed_tool",
		},
		{
			"prose narration only",
			`Now let me read the index.html template.`,
			"prose",
		},
		{
			"empty",
			"",
			"empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := classifyParseFailure(tc.raw, "")
			if got != tc.want {
				t.Errorf("classifyParseFailure() category = %q, want %q\nraw: %q", got, tc.want, tc.raw)
			}
		})
	}
}

func TestIsLoopingTail(t *testing.T) {
	loop := "The first line is <!DOCTYPE html>. " +
		strings.Repeat("Wait, I'll check if I can see the output. I can't. I'll just say it. ", 6)
	if !isLoopingTail(loop) {
		t.Errorf("expected a repeating self-doubt stream to be detected as a loop")
	}
	normal := "The first line of index.html is `<!DOCTYPE html>`. It declares the document type for the HTML5 page, followed by the html and head elements with meta tags and a title."
	if isLoopingTail(normal) {
		t.Errorf("a normal varied response must not be flagged as a loop")
	}
}

// Tests for callLLMOnce's failure handling against a fake llama-server:
// HTTP errors, unreachable server, truncated streams, mid-stream
// cancellation, and the reasoning_content fallback. The agent loop's
// resilience depends on these paths returning promptly with a
// classifiable error instead of hanging or panicking.

func llmTestCtx(url string) *AgentContext {
	return &AgentContext{
		InferenceURL: url,
		Ctx:          context.Background(),
		Messages:     []AgentMessage{{Role: "user", Content: "hi"}},
	}
}

func sseWrite(w http.ResponseWriter, lines ...string) {
	fl, _ := w.(http.Flusher)
	for _, l := range lines {
		io.WriteString(w, l+"\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
}

func TestCallLLMOnce_HTTPErrorSurfacesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":{"message":"model loading"}}`,
				http.StatusInternalServerError)
		}))
	defer srv.Close()

	ctx := llmTestCtx(srv.URL)
	_, _, err := callLLMOnce(ctx, ctx.Messages, 0.3)
	if err == nil {
		t.Fatal("500 from llama-server produced no error")
	}
	if !strings.Contains(err.Error(), "LLM returned 500") {
		t.Errorf("error %q does not name the status code", err)
	}
	// The response body is part of the error so the agent loop (and the
	// user-facing failure event) can say WHY llama-server refused.
	if !strings.Contains(err.Error(), "model loading") {
		t.Errorf("error %q drops the server's explanation", err)
	}
}

func TestCallLLMOnce_UnreachableServerFailsFast(t *testing.T) {
	// A server that existed and is gone — connection refused territory.
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	ctx := llmTestCtx(url)
	start := time.Now()
	_, _, err := callLLMOnce(ctx, ctx.Messages, 0.3)
	if err == nil {
		t.Fatal("dead llama-server produced no error")
	}
	if !strings.Contains(err.Error(), "LLM request failed") {
		t.Errorf("error %q is not the request-failure classification", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("failure took %v — should fail fast, not wait on a timeout", elapsed)
	}
}

func TestCallLLMOnce_TruncatedStreamReturnsPartialContent(t *testing.T) {
	// Server streams one delta then ends the response without [DONE] —
	// the shape of a llama-server crash mid-generation. Current contract:
	// clean EOF ends the scan without error and the partial content is
	// returned, so the caller's parse step decides what to do with it.
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			sseWrite(w, `data: {"choices":[{"delta":{"content":"{\"type\":\"do"}}]}`)
			// no [DONE], no usage — connection just ends
		}))
	defer srv.Close()

	ctx := llmTestCtx(srv.URL)
	content, _, err := callLLMOnce(ctx, ctx.Messages, 0.3)
	if err != nil {
		t.Fatalf("clean-EOF truncation returned error: %v", err)
	}
	if content != `{"type":"do` {
		t.Errorf("partial content = %q, want the streamed prefix", content)
	}
}

func TestCallLLMOnce_ContextCancelAbortsStalledStream(t *testing.T) {
	// Server sends one token then stalls forever. Cancelling the agent
	// context (what /cancel does) must abort the read promptly with the
	// stream-read classification, returning whatever was accumulated.
	// The `release` channel unblocks the handler after the call returns —
	// srv.Close() waits for active handlers, and server-side disconnect
	// detection isn't reliable enough to end the stall on its own.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			sseWrite(w, `data: {"choices":[{"delta":{"content":"partial"}}]}`)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
	defer srv.Close()
	// LIFO: this runs BEFORE srv.Close(), releasing the stalled handler
	// so Close doesn't wait forever on it.
	defer close(release)

	cancelCtx, cancel := context.WithCancel(context.Background())
	ctx := llmTestCtx(srv.URL)
	ctx.Ctx = cancelCtx

	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	content, _, err := callLLMOnce(ctx, ctx.Messages, 0.3)
	if err == nil {
		t.Fatal("cancelled stream returned no error")
	}
	if !strings.Contains(err.Error(), "read LLM stream") {
		t.Errorf("error %q is not the stream-read classification", err)
	}
	if content != "partial" {
		t.Errorf("accumulated content = %q, want %q", content, "partial")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("cancel took %v to unblock the call", elapsed)
	}
}

func TestCallLLMOnce_ReasoningOnlyStreamRecoversToolCall(t *testing.T) {
	// Model ignores enable_thinking=false and streams everything as
	// reasoning_content, including the JSON tool call. The empty-content
	// fallback must recover the structured envelope.
	payload := `{\"type\":\"tool_call\",\"name\":\"read_file\",\"args\":{\"path\":\"main.go\"}}`
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			sseWrite(w,
				`data: {"choices":[{"delta":{"reasoning_content":"I should read the file. "}}]}`,
				`data: {"choices":[{"delta":{"reasoning_content":"`+payload+`"}}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"total_tokens":9}}`,
				`data: [DONE]`)
		}))
	defer srv.Close()

	ctx := llmTestCtx(srv.URL)
	content, tokens, err := callLLMOnce(ctx, ctx.Messages, 0.3)
	if err != nil {
		t.Fatalf("reasoning-only stream errored: %v", err)
	}
	if !strings.Contains(content, `"read_file"`) {
		t.Errorf("recovered content %q lost the tool call", content)
	}
	if tokens != 9 {
		t.Errorf("total tokens = %d, want 9 from the usage block", tokens)
	}
}

func TestCallLLMOnce_ReasoningOnlyProseReturnsEmpty(t *testing.T) {
	// Pure narration with no tool call must come back EMPTY so the
	// caller's re-prompt fires — returning the prose would just
	// parse-error and waste the turn.
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			sseWrite(w,
				`data: {"choices":[{"delta":{"reasoning_content":"Now I need to look at the file and think about it more."}}]}`,
				`data: [DONE]`)
		}))
	defer srv.Close()

	ctx := llmTestCtx(srv.URL)
	content, _, err := callLLMOnce(ctx, ctx.Messages, 0.3)
	if err != nil {
		t.Fatalf("prose-only stream errored: %v", err)
	}
	if content != "" {
		t.Errorf("prose-only reasoning returned %q, want empty so the caller re-prompts", content)
	}
	if ctx.LastTurnReasoning == "" {
		t.Error("reasoning was not stashed on ctx for the repetition detector")
	}
}

// May 10 2026 path-aware error-loop breaker + plan-progress reminder.
// Locks both against regression.

func TestExtractFailurePath(t *testing.T) {
	cases := []struct {
		tool string
		args string
		want string
	}{
		{"read_file", `{"path":"app.py","offset":0,"limit":100}`, "app.py"},
		{"write_file", `{"path":"new.py","content":"hello"}`, "new.py"},
		{"edit_file", `{"path":"a.py","old_str":"x","new_str":"y"}`, "a.py"},
		{"structural_edit", `{"path":"t.html","selector":"<body>","content":"..."}`, "t.html"},
		{"delete_file", `{"path":"old.py"}`, "old.py"},
		{"find_file", `{"pattern":".*test.*\\.py$"}`, ""},      // no path field
		{"find_file", `{"pattern":"x","path":"src/"}`, "src/"}, // optional path
		{"list_directory", `{"path":"templates"}`, "templates"},
		{"search_files", `{"pattern":"TODO","path":"src/"}`, "src/"},
		{"run_command", `{"command":"python app.py"}`, ""}, // no path applicable
		{"run_background", `{"command":"flask run"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			got := extractFailurePath(tc.tool, json.RawMessage(tc.args))
			if got != tc.want {
				t.Errorf("extractFailurePath(%q, %q) = %q, want %q", tc.tool, tc.args, got, tc.want)
			}
		})
	}
}

func TestBuildPlanReminderNoPlan(t *testing.T) {
	ctx := &AgentContext{}
	if got := buildPlanReminder(ctx); got != "" {
		t.Errorf("no plan should return empty, got %q", got)
	}
}

func TestBuildPlanReminderRendersProgress(t *testing.T) {
	ctx := &AgentContext{
		Plan: &Plan{
			Steps: []PlanStep{
				{ID: "s1", Action: "read_file", Target: "app.py"},
				{ID: "s2", Action: "structural_edit", Target: "templates/dashboard.html"},
				{ID: "s3", Action: "run_command", Target: "curl localhost:5000/dashboard"},
			},
			VerifyStep: "s3",
		},
		PlanStepsSatisfied: []bool{true, false, false},
	}
	got := buildPlanReminder(ctx)
	if got == "" {
		t.Fatal("expected reminder, got empty")
	}
	for _, s := range []string{"[system note]: plan progress", "1/3", "s2", "structural_edit templates/dashboard.html", "Done: s1", "Remaining: s2, s3"} {
		if !strings.Contains(got, s) {
			t.Errorf("reminder missing %q: %s", s, got)
		}
	}
}

func TestBuildPlanReminderAllSatisfied(t *testing.T) {
	ctx := &AgentContext{
		Plan: &Plan{
			Steps:      []PlanStep{{ID: "s1"}, {ID: "s2"}},
			VerifyStep: "s2",
		},
		PlanStepsSatisfied: []bool{true, true},
	}
	got := buildPlanReminder(ctx)
	for _, s := range []string{"plan complete", "2/2", "s2"} {
		if !strings.Contains(got, s) {
			t.Errorf("complete-plan reminder missing %q: %s", s, got)
		}
	}
}

func TestBuildPlanReminderLazyInitsSatisfied(t *testing.T) {
	// PlanStepsSatisfied can be nil at first turn — the reminder
	// should lazily initialize it so it doesn't panic.
	ctx := &AgentContext{
		Plan: &Plan{
			Steps: []PlanStep{{ID: "s1", Action: "read_file", Target: "app.py"}},
		},
	}
	got := buildPlanReminder(ctx)
	if got == "" {
		t.Fatal("expected reminder, got empty")
	}
	if ctx.PlanStepsSatisfied == nil || len(ctx.PlanStepsSatisfied) != 1 {
		t.Errorf("reminder should have lazily initialized PlanStepsSatisfied; got %v", ctx.PlanStepsSatisfied)
	}
}

func TestSamplePlanContextPicksUpPriorityFiles(t *testing.T) {
	dir := t.TempDir()
	// Lay down a typical flask app shape.
	if err := os.WriteFile(filepath.Join(dir, "app.py"),
		[]byte("from flask import Flask\napp = Flask(__name__)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "index.html"),
		[]byte("<html><body>hi</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A noisy unrelated file that shouldn't appear.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"),
		[]byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := samplePlanContext(dir, 6, 2000)
	if _, ok := got["app.py"]; !ok {
		t.Errorf("expected app.py in context, got keys %v", keys(got))
	}
	if _, ok := got["templates/index.html"]; !ok {
		t.Errorf("expected templates/index.html in context, got keys %v", keys(got))
	}
	if _, ok := got["notes.txt"]; ok {
		t.Errorf("notes.txt leaked into priority context")
	}
}

func TestSamplePlanContextTruncatesLargeFiles(t *testing.T) {
	// File between maxBytes (1000) and the hard-skip ceiling (4×maxBytes
	// = 4000) should pass the size gate and get truncated. Files above
	// 4000 are skipped wholesale to avoid yanking a 50KB README into
	// the planner.
	dir := t.TempDir()
	big := make([]byte, 3000)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	got := samplePlanContext(dir, 6, 1000)
	content, ok := got["main.py"]
	if !ok {
		t.Fatal("main.py missing from sampled context")
	}
	// 1000 bytes of body + "\n... (truncated)" marker.
	if len(content) > 1100 {
		t.Errorf("content %d bytes — sampler should truncate to ~1000", len(content))
	}
	if len(content) < 1000 {
		t.Errorf("content %d bytes — sampler shouldn't truncate below maxBytes", len(content))
	}
}

func TestSamplePlanContextSkipsHugeFiles(t *testing.T) {
	// Files >4×maxBytes are skipped wholesale to keep the planner
	// budget small. Verifies the hard-skip ceiling.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"),
		[]byte("from flask import Flask\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	huge := make([]byte, 10_000)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), huge, 0o644); err != nil {
		t.Fatal(err)
	}

	got := samplePlanContext(dir, 6, 1000)
	if _, ok := got["README.md"]; ok {
		t.Errorf("10KB README should be skipped at maxBytes=1000")
	}
	if _, ok := got["app.py"]; !ok {
		t.Errorf("small app.py should still be picked up")
	}
}

func TestSamplePlanContextFallsBackToShallowWalk(t *testing.T) {
	// No priority files — sampler should still pick up source files
	// from a shallow read of the working dir.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "weird_entry.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.dat"),
		[]byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := samplePlanContext(dir, 6, 2000)
	if _, ok := got["weird_entry.go"]; !ok {
		t.Errorf("expected weird_entry.go in fallback walk, got %v", keys(got))
	}
	if _, ok := got["ignored.dat"]; ok {
		t.Errorf(".dat file leaked through extension filter")
	}
}

func TestSamplePlanContextEmptyOnMissingDir(t *testing.T) {
	got := samplePlanContext("", 5, 1000)
	if got != nil {
		t.Errorf("expected nil for empty workingDir, got %v", got)
	}
}

func TestSamplePlanContextWalksSubdirsForPriorityFiles(t *testing.T) {
	// May 2026 user case: workspace root has no app.py, but a
	// snake/ subdir does. Sampler should pick up snake/app.py with
	// the path keyed as "snake/app.py" so the planner emits tool
	// calls using that exact path.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "snake", "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snake", "app.py"),
		[]byte("from flask import Flask\napp=Flask(__name__)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snake", "templates", "index.html"),
		[]byte("<html>hi</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := samplePlanContext(dir, 6, 2000)
	if _, ok := got["snake/app.py"]; !ok {
		t.Errorf("expected snake/app.py via subdir walk, got keys %v", keys(got))
	}
	if _, ok := got["snake/templates/index.html"]; !ok {
		t.Errorf("expected snake/templates/index.html via subdir walk, got keys %v", keys(got))
	}
}

func TestSamplePlanContextSkipsNoiseDirs(t *testing.T) {
	// venv/ and node_modules/ shouldn't be walked even if they
	// contain a priority filename — these are cache/vendor dirs.
	dir := t.TempDir()
	for _, junk := range []string{"venv", "node_modules", ".git", "__pycache__"} {
		if err := os.MkdirAll(filepath.Join(dir, junk), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, junk, "app.py"),
			[]byte("# noise"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := samplePlanContext(dir, 6, 2000)
	for _, junk := range []string{"venv/app.py", "node_modules/app.py", ".git/app.py", "__pycache__/app.py"} {
		if _, ok := got[junk]; ok {
			t.Errorf("noise path %q leaked into context", junk)
		}
	}
}

func TestShouldGeneratePlanGates(t *testing.T) {
	cases := []struct {
		name string
		tier Tier
		msg  string
		want bool
	}{
		{"T0 trivial chat", Tier0Conversational, "thanks man", false},
		{"short ack", Tier2Medium, "yes", false},
		{"borderline short", Tier2Medium, "fix it", false}, // 6 chars
		{"real fix request", Tier2Medium, "fix the index.html template", true},
		{"feature add", Tier2Medium, "add a /hello route to app.py", true},
		{"T3 architectural", Tier3Hard, "build a flask app with auth and a database", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &AgentContext{Tier: tc.tier}
			if got := shouldGeneratePlan(ctx, tc.msg); got != tc.want {
				t.Errorf("shouldGeneratePlan(%q, tier=%v) = %v, want %v",
					tc.msg, tc.tier, got, tc.want)
			}
		})
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestDetectProjectVenvPythonFindsCommonShapes(t *testing.T) {
	cases := []struct {
		name    string
		layout  []string // relative paths to create as files
		wantRel string   // expected venv-python relative path
	}{
		{"venv/bin/python", []string{"venv/bin/python"}, "venv/bin/python"},
		{".venv/bin/python", []string{".venv/bin/python"}, ".venv/bin/python"},
		{"env/bin/python3", []string{"env/bin/python3"}, "env/bin/python3"},
		{"prefers-venv-over-.venv", []string{"venv/bin/python", ".venv/bin/python"}, "venv/bin/python"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, rel := range tc.layout {
				abs := filepath.Join(dir, rel)
				if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(abs, []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			got := detectProjectVenvPython(dir)
			want := filepath.Join(dir, tc.wantRel)
			if got != want {
				t.Errorf("detectProjectVenvPython() = %q, want %q", got, want)
			}
		})
	}
}

func TestDetectProjectVenvPythonReturnsEmptyWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	// No venv layout — just a stray app.py.
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectProjectVenvPython(dir); got != "" {
		t.Errorf("detectProjectVenvPython() = %q, want empty", got)
	}
}

func TestDetectProjectVenvPythonRejectsDirectoryNamedPython(t *testing.T) {
	// Edge case: a directory called `venv/bin/python/` (not a file)
	// must NOT be treated as the python binary. Prevents false
	// positives when a venv has been corrupted or scaffolded weirdly.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "venv", "bin", "python"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := detectProjectVenvPython(dir); got != "" {
		t.Errorf("detectProjectVenvPython() = %q, want empty (directory not file)", got)
	}
}

func TestDetectProjectToolchainsPython(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tcs := detectProjectToolchains(dir)
	if len(tcs) != 1 || tcs[0].Name != "python" {
		t.Fatalf("got %+v, want one python toolchain", tcs)
	}
	if tcs[0].InstallCommand != "pip install -r requirements.txt" {
		t.Errorf("install = %q, want pip install -r", tcs[0].InstallCommand)
	}
}

func TestDetectProjectToolchainsPolyglot(t *testing.T) {
	// React frontend + Django backend + Rust core in one repo.
	dir := t.TempDir()
	for _, f := range []string{"package.json", "tsconfig.json", "pyproject.toml", "Cargo.toml", "Cargo.lock"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tcs := detectProjectToolchains(dir)
	names := map[string]bool{}
	for _, tc := range tcs {
		names[tc.Name] = true
	}
	for _, want := range []string{"python", "node", "rust"} {
		if !names[want] {
			t.Errorf("missing toolchain %q in %v", want, names)
		}
	}
	// Node with tsconfig.json should pick the tsx runner.
	for _, tc := range tcs {
		if tc.Name == "node" && tc.Runner != "tsx" {
			t.Errorf("node runner = %q, want tsx (tsconfig present)", tc.Runner)
		}
	}
}

func TestDetectProjectToolchainsNodePkgManager(t *testing.T) {
	cases := []struct {
		lockfile string
		wantPM   string
	}{
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "yarn"},
		{"bun.lockb", "bun"},
		{"package-lock.json", "npm"},
	}
	for _, tc := range cases {
		t.Run(tc.lockfile, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644)
			os.WriteFile(filepath.Join(dir, tc.lockfile), []byte(""), 0o644)
			tcs := detectProjectToolchains(dir)
			if len(tcs) != 1 || tcs[0].PackageManager != tc.wantPM {
				t.Fatalf("got pkgManager %q, want %q", tcs[0].PackageManager, tc.wantPM)
			}
		})
	}
}

func TestProbeToolchainReadyPythonVenv(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask"), 0o644)
	tcs := detectProjectToolchains(dir)
	if got := probeToolchainReady(dir, tcs[0]); got != "needs install" {
		t.Errorf("no venv yet, got %q want %q", got, "needs install")
	}
	// Now scaffold a populated venv.
	sp := filepath.Join(dir, "venv", "lib", "python3.11", "site-packages", "flask")
	os.MkdirAll(sp, 0o755)
	if got := probeToolchainReady(dir, tcs[0]); got != "ready" {
		t.Errorf("populated venv not detected: %q want %q", got, "ready")
	}
}

func TestProbeToolchainReadyNodeModules(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644)
	tcs := detectProjectToolchains(dir)
	if got := probeToolchainReady(dir, tcs[0]); got != "needs install" {
		t.Errorf("no node_modules, got %q want %q", got, "needs install")
	}
	os.MkdirAll(filepath.Join(dir, "node_modules", "express"), 0o755)
	if got := probeToolchainReady(dir, tcs[0]); got != "ready" {
		t.Errorf("populated node_modules not detected: %q want %q", got, "ready")
	}
}

func TestDisplayRelativeRunner(t *testing.T) {
	cases := []struct {
		runner, cwd, want string
	}{
		{"/workspace/venv/bin/python", "/workspace", "venv/bin/python"},
		{"node", "/workspace", "node"},
		{"/usr/bin/python", "/workspace", "/usr/bin/python"}, // outside cwd → unchanged
		{"./gradlew run", "/workspace", "./gradlew run"},
	}
	for _, tc := range cases {
		if got := displayRelativeRunner(tc.runner, tc.cwd); got != tc.want {
			t.Errorf("displayRelativeRunner(%q, %q) = %q, want %q", tc.runner, tc.cwd, got, tc.want)
		}
	}
}

func TestHasUserPackagesIgnoresPipOnly(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"pip", "setuptools", "wheel", "pkg_resources"} {
		os.MkdirAll(filepath.Join(dir, p), 0o755)
	}
	if hasUserPackages(dir) {
		t.Error("pip-only site-packages should not count as 'user packages'")
	}
	os.MkdirAll(filepath.Join(dir, "flask"), 0o755)
	if !hasUserPackages(dir) {
		t.Error("flask present should count as user package")
	}
}

// May 10 2026: T1/T2/T3 default to uncapped (returns 0); the 8
// stuck-pattern detectors are the real safety net. T0 keeps a cap as a SHAPE
// constraint (conversational input shouldn't loop) — 12 since 2026-08-02,
// because a question ABOUT the code is conversational and still has to read
// it. At 5, "how does the contact form work?" spent turn 0 on a bounced text
// exit and turns 1-3 on searches with the wrong glob, reached the right file
// on turn 4, and hit the cap with no answer for the user.
// The agent loop treats MaxTurns == 0 as "no limit"; cancellation
// via ctx.Ctx is the upper bound.
func TestTierMaxTurnsUncappedDefaults(t *testing.T) {
	t.Setenv("ATLAS_MAX_TURNS", "")
	if got := TierMaxTurns(Tier0Conversational); got != 12 {
		t.Errorf("T0 = %d, want 12 (shape constraint with room to read)", got)
	}
	if got := TierMaxTurns(Tier1Simple); got != 0 {
		t.Errorf("T1 = %d, want 0 (uncapped)", got)
	}
	if got := TierMaxTurns(Tier2Medium); got != 0 {
		t.Errorf("T2 = %d, want 0 (uncapped)", got)
	}
	if got := TierMaxTurns(Tier3Hard); got != 0 {
		t.Errorf("T3 = %d, want 0 (uncapped)", got)
	}
}

func TestTierMaxTurnsEnvOverride(t *testing.T) {
	t.Setenv("ATLAS_MAX_TURNS", "150")
	if got := TierMaxTurns(Tier2Medium); got != 150 {
		t.Errorf("env override = %d, want 150 (operator's call, no upper clamp)", got)
	}
}

func TestTierMaxTurnsEnvOverrideHighIsHonored(t *testing.T) {
	// May 10 2026: removed the absoluteMaxTurns ceiling. If the
	// operator says 10000, that's their call.
	t.Setenv("ATLAS_MAX_TURNS", "10000")
	if got := TierMaxTurns(Tier2Medium); got != 10000 {
		t.Errorf("env=10000 = %d, want 10000 (no upper clamp)", got)
	}
}

func TestTierMaxTurnsZeroEnvFallsThrough(t *testing.T) {
	// env=0 means "fall through to tier default" — for T2 that's 0
	// (uncapped). For T0 that's 5 (shape cap).
	t.Setenv("ATLAS_MAX_TURNS", "0")
	if got := TierMaxTurns(Tier2Medium); got != 0 {
		t.Errorf("env=0, T2 = %d, want 0 (uncapped default)", got)
	}
	if got := TierMaxTurns(Tier0Conversational); got != 12 {
		t.Errorf("env=0, T0 = %d, want 12 (shape cap preserved)", got)
	}
}

func TestTierMaxTurnsInvalidEnvFallsThrough(t *testing.T) {
	t.Setenv("ATLAS_MAX_TURNS", "garbage")
	if got := TierMaxTurns(Tier2Medium); got != 0 {
		t.Errorf("invalid env, T2 = %d, want 0 (uncapped default)", got)
	}
}

func TestRecoverStructuredReasoningAcceptsWhitespaceFormattedText(t *testing.T) {
	raw := "{\n  \"type\": \"text\",\n  \"content\": \"agent-ok\"\n}"
	recovered, ok := recoverStructuredReasoning(raw)
	if !ok {
		t.Fatal("valid text envelope in reasoning_content was discarded")
	}
	parsed, err := extractModelResponse(recovered)
	if err != nil || parsed.Type != "text" || parsed.Content != "agent-ok" {
		t.Fatalf("recovered response = %#v, err=%v", parsed, err)
	}
}

func TestRecoverStructuredReasoningRejectsNarration(t *testing.T) {
	if recovered, ok := recoverStructuredReasoning("I should inspect the repository first."); ok {
		t.Fatalf("pure narration recovered as agent response: %q", recovered)
	}
}

func TestRecoverStructuredReasoningAcceptsDoneAndToolCall(t *testing.T) {
	for _, raw := range []string{
		`{"type": "done", "summary": "finished"}`,
		`{"args": {"path": "."}, "name": "list_directory", "type": "tool_call"}`,
	} {
		if _, ok := recoverStructuredReasoning(raw); !ok {
			t.Fatalf("valid structured response was discarded: %s", raw)
		}
	}
}

// A cut-off or malformed content-bearing tool call is refused, never rebuilt.
// Recovery used to reconstruct write_file / edit_file / structural_edit args
// from whatever prefix arrived and execute them: a write cut mid-structure
// landed as the fragment, reported as a clean success. The intended bytes
// cannot be known from a prefix, so extractModelResponse must return an error
// and let the parse-failure path tell the model nothing was executed.
func TestTruncatedOrMalformedContentCallsAreNotReconstructed(t *testing.T) {
	for _, partial := range []string{
		`{"type":"tool_call","name":"write_file","args":{"path":"cut.py","content":"def f():\n    return [{\"user\": 1`,
		`{"type":"tool_call","name":"write_file","args":{"path":"bad.py","content":"P = re.compile(\"\d+\")\n"}}`,
		`{"type":"tool_call","name":"edit_file","args":{"path":"e.py","old_str":"return 1","new_str":"return [1, 2`,
		`{"type":"tool_call","name":"structural_edit","args":{"path":"templates/index.html","selector":"<html>","content":"<!DOCTYPE html>\n<html lang=\"en\"><head></head><body>hi</body></html>"`,
	} {
		resp, err := extractModelResponse(partial)
		if err == nil {
			t.Errorf("a truncated/malformed call was reconstructed into %s %s:\n  %s", resp.Type, resp.Name, partial)
		}
	}
}

func TestExtractModelResponseSurfacesUnmarshalError(t *testing.T) {
	// Brace-balanced JSON with a literal LF inside a string — invalid
	// per RFC 8259 and the kind of failure we used to swallow.
	raw := "{\"type\":\"tool_call\",\"name\":\"read_file\",\"args\":{\"path\":\"a\nb\"}}"
	_, err := extractModelResponse(raw)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "could not parse JSON from response") {
		t.Errorf("error missing canonical prefix: %v", err)
	}
	// Wrapped error must carry the underlying json error (anything
	// containing 'invalid character' or 'unexpected' is acceptable —
	// we just need SOME signal beyond the canonical prefix).
	inner := err.Error()
	if !strings.Contains(inner, "invalid character") && !strings.Contains(inner, "unexpected") {
		t.Errorf("error missing inner json detail: %v", err)
	}
}

// --- degenerate-output rejection -------------------------------------------
//
// Truncation recovery is structural: it reconstructs args from whatever the
// field extractor can read. Degenerate generations parse just as cleanly as
// real content, so without a sense-check they are "recovered" into a real
// write against the user's file.

// May 2026 BiasBusters #2/#3 — locks the trigger that activates the
// per-step grammar restriction. The restriction must fire exactly when
// the model is about to pick a write_file/edit_file retry on an existing
// .py/.html file (the bias case from the May 7 flask session). It must
// NOT fire on stale rejections, non-Python/HTML files, or non-write_file
// failures.
func TestStepExclusions(t *testing.T) {
	cases := []struct {
		name      string
		messages  []AgentMessage
		wantTools []string
		wantExt   string
	}{
		{
			name: "fresh write_file rejection on .html — fires",
			messages: []AgentMessage{
				{Role: "system", Content: "sys"},
				{Role: "user", Content: "expand dashboard.html"},
				{Role: "assistant", Content: `{"type":"tool_call","name":"write_file","args":{"path":"templates/dashboard.html","content":"..."}}`},
				{Role: "tool", ToolName: "write_file", Content: `{"success":false,"error":"File templates/dashboard.html already exists (87 lines). write_file is for creating new files..."}`},
			},
			wantTools: []string{"edit_file", "write_file"},
			wantExt:   ".html",
		},
		{
			name: "fresh write_file rejection on .py — fires",
			messages: []AgentMessage{
				{Role: "tool", ToolName: "write_file", Content: `{"success":false,"error":"File app.py already exists (42 lines). write_file is for creating new files..."}`},
			},
			wantTools: []string{"edit_file", "write_file"},
			wantExt:   ".py",
		},
		{
			name: "fresh write_file rejection on .htm — fires",
			messages: []AgentMessage{
				{Role: "tool", ToolName: "write_file", Content: `{"success":false,"error":"File legacy.htm already exists (200 lines). write_file is for creating new files..."}`},
			},
			wantTools: []string{"edit_file", "write_file"},
			wantExt:   ".htm",
		},
		{
			name: "rejection on .css — does not fire (not a tree-sitter target)",
			messages: []AgentMessage{
				{Role: "tool", ToolName: "write_file", Content: `{"success":false,"error":"File styles.css already exists (50 lines). write_file is for creating new files..."}`},
			},
			wantTools: nil,
			wantExt:   "",
		},
		{
			name: "stale rejection — assistant has already corrected — does not fire",
			messages: []AgentMessage{
				{Role: "tool", ToolName: "write_file", Content: `{"success":false,"error":"File app.py already exists (42 lines)..."}`},
				{Role: "assistant", Content: `{"type":"tool_call","name":"structural_edit","args":{...}}`},
				{Role: "tool", ToolName: "structural_edit", Content: `{"success":true}`},
			},
			wantTools: nil,
			wantExt:   "",
		},
		{
			name: "edit_file rejection (wrong tool, not the trigger) — does not fire",
			messages: []AgentMessage{
				{Role: "tool", ToolName: "edit_file", Content: `{"success":false,"error":"old_str not found in app.py"}`},
			},
			wantTools: nil,
			wantExt:   "",
		},
		{
			name: "ephemeral system note already injected last turn — still fires",
			messages: []AgentMessage{
				{Role: "tool", ToolName: "write_file", Content: `{"success":false,"error":"File templates/index.html already exists (150 lines)..."}`},
				{Role: "user", Content: "[system note]: For this single decision, edit_file and write_file are unavailable..."},
			},
			wantTools: []string{"edit_file", "write_file"},
			wantExt:   ".html",
		},
		{
			name:      "empty conversation — does not fire",
			messages:  []AgentMessage{},
			wantTools: nil,
			wantExt:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &AgentContext{Messages: tc.messages}
			gotTools, gotExt := stepExclusions(ctx)
			if !equalStringSlices(gotTools, tc.wantTools) {
				t.Errorf("stepExclusions() tools = %v, want %v", gotTools, tc.wantTools)
			}
			if gotExt != tc.wantExt {
				t.Errorf("stepExclusions() ext = %q, want %q", gotExt, tc.wantExt)
			}
		})
	}
}

// Locks the GBNF restriction shape: the banned tool names must be absent
// from the tool-name production. Without this guard, a future refactor
// of buildGBNFGrammarForTools could silently drop the exclusion logic
// and leave the model free to pick edit_file again.
//
// GBNF quotes a tool name as `"\"edit_file\""` (a literal JSON string
// token), so we match against the raw escaped sequence rather than
// `"edit_file"`.
func TestBuildGBNFGrammarForToolsExcludes(t *testing.T) {
	const editFileTok = `"\"edit_file\""`
	const writeFileTok = `"\"write_file\""`
	const structuralEditTok = `"\"structural_edit\""`
	const readFileTok = `"\"read_file\""`

	all := buildGBNFGrammarForTools(nil)
	if !strings.Contains(all, editFileTok) {
		t.Fatalf("baseline grammar should contain %s before exclusion; grammar=\n%s", editFileTok, all)
	}
	restricted := buildGBNFGrammarForTools([]string{"edit_file", "write_file"})
	if strings.Contains(restricted, editFileTok) {
		t.Errorf("restricted grammar still contains %s", editFileTok)
	}
	if strings.Contains(restricted, writeFileTok) {
		t.Errorf("restricted grammar still contains %s", writeFileTok)
	}
	if !strings.Contains(restricted, structuralEditTok) {
		t.Errorf("restricted grammar must keep %s available", structuralEditTok)
	}
	if !strings.Contains(restricted, readFileTok) {
		t.Errorf("restricted grammar must keep %s available", readFileTok)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// v3AndStructuralServer stands in for the v3-service: it serves both
// /v3/generate (SSE, returns `winnerCode` as the pipeline result) and
// /internal/structural_check (flags `flagName` in any source that calls it
// without importing it). One server because both live behind ctx.V3URL.
func v3AndStructuralServer(t *testing.T, winnerCode, flagName string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/generate":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			// A service that verified this winner says so in the envelope.
			// Without one nothing is authorized -- `passed` alone no longer
			// replaces the caller's content.
			payload, _ := json.Marshal(map[string]interface{}{
				"code": winnerCode, "passed": true,
				"phase_solved": "phase1", "candidates_tested": 3,
				"winning_score": 0.9,
				"evidence":      envelopeFor(t, winnerCode, nil),
			})
			for _, line := range []string{"event: result", "data: " + string(payload), "", "data: [DONE]", ""} {
				fmt.Fprint(w, line+"\n")
				if fl != nil {
					fl.Flush()
				}
			}
		case "/internal/structural_check":
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				Source string `json:"source"`
			}
			_ = json.Unmarshal(raw, &body)
			out := map[string]interface{}{"ok": true, "unresolved": []string{}}
			if strings.Contains(body.Source, flagName+"(") &&
				!strings.Contains(body.Source, "import "+flagName) {
				out["unresolved"] = []string{flagName}
			}
			b, _ := json.Marshal(out)
			_, _ = w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
}

// fakeSyntaxSandbox serves /syntax-check the way checkFallbackSyntax expects:
// valid unless the source contains `brokenMarker`.
func fakeSyntaxSandbox(t *testing.T, brokenMarker string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/syntax-check" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &body)
		valid := brokenMarker == "" || !strings.Contains(body.Code, brokenMarker)
		out := map[string]interface{}{"valid": valid}
		if !valid {
			out["errors"] = []string{"SyntaxError: invalid syntax"}
		}
		b, _ := json.Marshal(out)
		_, _ = w.Write(b)
	}))
}

func writeGateCtx(t *testing.T, v3URL, sandboxURL, workDir string) *AgentContext {
	t.Helper()
	// NewAgentContext, not a bare literal: the struct has several maps
	// (FilesRead, ManifestAnnounced, ...) that only the constructor
	// initialises, and a test touching one of them panicked on a nil map.
	ctx := NewAgentContext(workDir, Tier2Medium)
	ctx.V3URL = v3URL
	ctx.SandboxURL = sandboxURL
	// A request identity, because the route mints one entry per attempt and a
	// call that cannot be named bounds no mutation. Production always has one.
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-write-gate")
	return ctx
}

// When the V3 winner introduces an unresolved call but the model's own
// baseline is clean, writeFileWithV3 must write the BASELINE and report it
// as a plain (non-V3) write — no V3Used/PhaseSolved/score attached to
// content that never went through V3 sandbox verification. Guards against
// the done-nudge telling the model an unverified baseline was "V3 verified".
func TestWriteFileV3WinnerVetoFallsBackToBaseline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.py")
	baseline := "def index():\n    return 'ok'\n"
	winner := "def index():\n    return render_template('index.html')\n"

	v3 := v3AndStructuralServer(t, winner, "render_template")
	defer v3.Close()
	sb := fakeSyntaxSandbox(t, "") // baseline always parses
	defer sb.Close()
	ctx := writeGateCtx(t, v3.URL, sb.URL, dir)

	res, err := writeFileWithV3(path, baseline, ctx)
	if err != nil {
		t.Fatalf("writeFileWithV3 error: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("expected success, got %+v", res)
	}
	// Baseline landed, not the vetoed winner.
	onDisk, _ := os.ReadFile(path)
	if string(onDisk) != baseline {
		t.Errorf("expected baseline on disk, got %q", string(onDisk))
	}
	// Telemetry must NOT claim V3 verification of the baseline.
	if res.V3Used || res.PhaseSolved != "" || res.WinningScore != 0 || res.VerificationEvidence != nil {
		t.Errorf("fallback write must carry no V3 metadata, got V3Used=%v phase=%q score=%v evidence=%v",
			res.V3Used, res.PhaseSolved, res.WinningScore, res.VerificationEvidence)
	}
}

// A clean V3 winner lands with full V3 telemetry (the non-fallback path).
func TestWriteFileV3CleanWinnerKeepsTelemetry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.py")
	baseline := "def index():\n    return 'ok'\n"
	winner := "def index():\n    return 'better'\n" // no unresolved call

	v3 := v3AndStructuralServer(t, winner, "render_template")
	defer v3.Close()
	sb := fakeSyntaxSandbox(t, "")
	defer sb.Close()
	ctx := writeGateCtx(t, v3.URL, sb.URL, dir)
	// The client declares the artifact it asked for. Without a declared target
	// there is nothing to authorize against and the model's own bytes land,
	// which is a different test from this one.
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["app.py"]}`)

	res, err := writeFileWithV3(path, baseline, ctx)
	if err != nil {
		t.Fatalf("writeFileWithV3 error: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("expected success, got %+v", res)
	}
	onDisk, _ := os.ReadFile(path)
	if string(onDisk) != winner {
		t.Errorf("expected winner on disk, got %q", string(onDisk))
	}
	if !res.V3Used || res.PhaseSolved != "phase1" {
		t.Errorf("clean winner must keep V3 telemetry, got V3Used=%v phase=%q", res.V3Used, res.PhaseSolved)
	}
}

// Both winner AND baseline introduce an unresolved call → reject (the
// model's own content is genuinely broken, name what it can act on).
func TestWriteFileV3BothBrokenRejects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.py")
	baseline := "def index():\n    return render_template('x')\n" // model's own is broken too
	winner := "def index():\n    return render_template('index.html')\n"

	v3 := v3AndStructuralServer(t, winner, "render_template")
	defer v3.Close()
	sb := fakeSyntaxSandbox(t, "")
	defer sb.Close()
	ctx := writeGateCtx(t, v3.URL, sb.URL, dir)
	ctx.TaskContract = mustContract(t, dir,
		`{"task_mode":"work","output_knowledge":"declared","expected_outputs":["app.py"]}`)

	res, err := writeFileWithV3(path, baseline, ctx)
	if err != nil {
		t.Fatalf("writeFileWithV3 error: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("expected rejection, got %+v", res)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("nothing should have landed on disk")
	}
	if !strings.Contains(res.Error, "render_template") || !strings.Contains(res.Error, "write_file") {
		t.Errorf("rejection should name the call and be write-flavored: %q", res.Error)
	}
}

// edit_file and structural_edit hand their spliced content to
// improveContentWithV3 and write whatever comes back. A candidate that
// regresses that content must be dropped at the boundary: otherwise the write
// gates reject the edit and quote a line number from a candidate the model
// never saw, and it spends its remaining turns hunting a defect that is not in
// the file (the observed E2E failure). The caller's content must come back
// carrying NO V3 metadata, since nothing V3 produced was used.
func TestImproveContentV3DropsRegressingCandidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.py")
	modelEdit := "def index():\n    return 'ok'\n"
	candidate := "def index():\n    return render_template('index.html')\n"

	v3 := v3AndStructuralServer(t, candidate, "render_template")
	defer v3.Close()
	sb := fakeSyntaxSandbox(t, "")
	defer sb.Close()
	ctx := writeGateCtx(t, v3.URL, sb.URL, dir)

	out, meta, err := improveContentWithV3(path, modelEdit, ctx)
	if err != nil {
		t.Fatalf("improveContentWithV3 error: %v", err)
	}
	if out != modelEdit {
		t.Errorf("regressing candidate must be dropped for the caller's content, got %q", out)
	}
	if meta.Used || meta.PhaseSolved != "" || meta.WinningScore != 0 {
		t.Errorf("dropped candidate must carry no V3 metadata, got %+v", meta)
	}
}

// The caller's own content is already broken, so the candidate is not a
// regression: it is returned and the downstream gate rejects with a message
// about content the model genuinely wrote.
func TestImproveContentV3KeepsCandidateWhenCallerAlreadyBroken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.py")
	modelEdit := "def index():\n    return render_template('x')\n"
	candidate := "def index():\n    return render_template('index.html')\n"

	v3 := v3AndStructuralServer(t, candidate, "render_template")
	defer v3.Close()
	sb := fakeSyntaxSandbox(t, "")
	defer sb.Close()
	ctx := writeGateCtx(t, v3.URL, sb.URL, dir)

	out, meta, err := improveContentWithV3(path, modelEdit, ctx)
	if err != nil {
		t.Fatalf("improveContentWithV3 error: %v", err)
	}
	if out != candidate || !meta.Used {
		t.Errorf("candidate must survive when the caller's content is broken too, got out=%q used=%v", out, meta.Used)
	}
}

// V3 output arrives wrapped in a markdown fence often enough that both callers
// used to strip it after the fact. The strip happens at the boundary, before
// the regression check, so a fenced candidate is never mistaken for a broken
// one -- and stripping produces DIFFERENT bytes, which the service's evidence
// does not describe. Authorization is re-asked of the stripped form and
// withdrawn, so the caller's own content stands with no provenance. A fenced
// candidate becomes deliverable again when the service hashes the form it
// hands over.
func TestImproveContentV3RevokesAFencedCandidateItMustRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.py")
	modelEdit := "def index():\n    return 'ok'\n"
	code := "def index():\n    return 'better'\n"
	// Only an exact whole-content wrapper is stripped; prose around a fence is
	// left for the syntax gate to reject.
	fenced := "```python\n" + code + "```\n"

	v3 := v3AndStructuralServer(t, fenced, "render_template")
	defer v3.Close()
	sb := fakeSyntaxSandbox(t, "```") // a surviving fence would fail the gate
	defer sb.Close()
	ctx := writeGateCtx(t, v3.URL, sb.URL, dir)

	out, _, err := improveContentWithV3(path, modelEdit, ctx)
	if err != nil {
		t.Fatalf("improveContentWithV3 error: %v", err)
	}
	if strings.Contains(out, "```") {
		t.Errorf("the wrapper must never reach the caller, got %q", out)
	}
	// The stripped bytes are a proposal -- they are materially different from
	// the caller's edit -- and the route that receives them decides. Nothing
	// the service says about them is an authorization, and a request that
	// declared nothing has no trusted verification to satisfy, so what lands
	// is the caller's own edit.
	if out != code {
		t.Errorf("the sanitised proposal was not carried; got %q", out)
	}
	// The same code, returned in the form it is delivered in, IS adopted --
	// this is authorization against the final bytes, not a refusal of fences.
	v3b := v3AndStructuralServer(t, code, "render_template")
	defer v3b.Close()
	outB, metaB, err := improveContentWithV3(path, modelEdit, writeGateCtx(t, v3b.URL, sb.URL, dir))
	if err != nil {
		t.Fatalf("improveContentWithV3 error: %v", err)
	}
	if outB != code || !metaB.Used {
		t.Errorf("an unwrapped verified candidate must be adopted, got %q used=%v",
			outB, metaB.Used)
	}
}

// A clean candidate is returned with full telemetry (the non-drop path).
func TestImproveContentV3KeepsCleanCandidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.py")
	modelEdit := "def index():\n    return 'ok'\n"
	candidate := "def index():\n    return 'better'\n"

	v3 := v3AndStructuralServer(t, candidate, "render_template")
	defer v3.Close()
	sb := fakeSyntaxSandbox(t, "")
	defer sb.Close()
	ctx := writeGateCtx(t, v3.URL, sb.URL, dir)

	out, meta, err := improveContentWithV3(path, modelEdit, ctx)
	if err != nil {
		t.Fatalf("improveContentWithV3 error: %v", err)
	}
	if out != candidate {
		t.Errorf("clean candidate must be adopted, got %q", out)
	}
	if !meta.Used || meta.PhaseSolved != "phase1" {
		t.Errorf("clean candidate must keep V3 telemetry, got %+v", meta)
	}
}

// A user cancel while the winner-gate structural check is in flight must
// land nothing on disk.
func TestWriteFileV3CancelDuringGateWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.py")
	baseline := "def index():\n    return 'ok'\n"
	winner := "def index():\n    return 'better'\n"

	reqCtx, cancel := context.WithCancel(context.Background())
	// Structural server cancels the request context on the first
	// /internal/structural_check call, simulating a mid-gate Ctrl+C.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/generate":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			payload, _ := json.Marshal(map[string]interface{}{
				"code": winner, "passed": true, "phase_solved": "phase1", "candidates_tested": 1,
			})
			for _, line := range []string{"event: result", "data: " + string(payload), "", "data: [DONE]", ""} {
				fmt.Fprint(w, line+"\n")
				if fl != nil {
					fl.Flush()
				}
			}
		case "/internal/structural_check":
			cancel() // user hit Ctrl+C during the gate
			_, _ = w.Write([]byte(`{"ok":true,"unresolved":[]}`))
		}
	}))
	defer srv.Close()
	ctx := writeGateCtx(t, srv.URL, "", dir)
	ctx.Ctx = reqCtx

	res, err := writeFileWithV3(path, baseline, ctx)
	if err != nil {
		t.Fatalf("writeFileWithV3 error: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("cancelled write must not succeed, got %+v", res)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("cancelled write must not land content on disk")
	}
}

// A background server the model started to verify its own work keeps the port,
// so its next run of the same app fails against "another program" it has no way
// to identify — an observed session spent its remaining turns on that conflict.
// The failure must name the model's own job.
func TestOwnBackgroundJobHintNamesTheCulprit(t *testing.T) {
	ctx := &AgentContext{BackgroundJobs: map[string]string{
		"7216f34ccea3": "python app.py",
	}}
	hint := ownBackgroundJobHint(ctx, "Address already in use\nPort 5001 is in use by another program.")
	if !strings.Contains(hint, "7216f34ccea3") || !strings.Contains(hint, "python app.py") {
		t.Errorf("hint must name the job id and its command, got %q", hint)
	}
	if !strings.Contains(hint, "stop_background") {
		t.Errorf("hint must name the remedy, got %q", hint)
	}
}

func TestOwnBackgroundJobHintSilentWhenUnrelated(t *testing.T) {
	ctx := &AgentContext{BackgroundJobs: map[string]string{"abc": "python app.py"}}
	if h := ownBackgroundJobHint(ctx, "ModuleNotFoundError: No module named 'flask'"); h != "" {
		t.Errorf("unrelated failure must not get a job hint, got %q", h)
	}
}

func TestOwnBackgroundJobHintSilentWithNoJobs(t *testing.T) {
	if h := ownBackgroundJobHint(&AgentContext{}, "Address already in use"); h != "" {
		t.Errorf("no running jobs must produce no hint, got %q", h)
	}
}

// Background jobs outlive the agent loop on purpose — a loop is one user
// message, so killing them would break "start the server" then "now curl it".
// The defect was that they did so silently: the next turn hits a bound port
// with no explanation and the user is never told anything is still running.
func TestLiveBackgroundJobNoteNamesRunningJobs(t *testing.T) {
	ctx := &AgentContext{BackgroundJobs: map[string]string{"7216f34ccea3": "python app.py"}}
	note := liveBackgroundJobNote(ctx)
	if !strings.Contains(note, "7216f34ccea3") || !strings.Contains(note, "python app.py") {
		t.Errorf("note must name the job and its command, got %q", note)
	}
	if !strings.Contains(note, "stop_background") {
		t.Errorf("note must name the remedy, got %q", note)
	}
}

func TestLiveBackgroundJobNoteEmptyWhenNothingRuns(t *testing.T) {
	if n := liveBackgroundJobNote(&AgentContext{}); n != "" {
		t.Errorf("no jobs must add nothing to the summary, got %q", n)
	}
}

// Content that does not parse must be rejected before the V3 budget is spent.
// The debug fast-path cannot cover this: it keys off a SUCCESSFUL write of the
// file, and a model failing the syntax gate never records one, so a
// degenerating model paid the full V3 timeout on every attempt (observed:
// markdown bold inside code, four times, 180s each).
func TestWriteFileRejectsUnparseableContentWithoutCallingV3(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test_chunk.py")

	v3Called := false
	v3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v3/generate") {
			v3Called = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"unresolved":[]}`))
	}))
	defer v3.Close()
	sb := fakeSyntaxSandbox(t, "**") // markdown bold marks the content invalid
	defer sb.Close()
	ctx := writeGateCtx(t, v3.URL, sb.URL, dir)

	// Must reach T2 or the V3 path (and this gate) never engages — the
	// T0/T1 direct write is deliberately ungated. Mirrors the observed file:
	// "test_chunk.py -> T2:medium (17 lines)".
	body := "from chunk import chunks\n\n\ndef main():\n    data = [1, 2, **3**, 4, 5]\n" +
		"    size = 2\n    result = chunks(data, size)\n    print(result)\n" +
		"    for row in result:\n        if len(row) != size:\n            print('short')\n" +
		"        else:\n            print('full')\n    return result\n\n\n" +
		"if __name__ == '__main__':\n    main()\n"
	args, _ := json.Marshal(map[string]string{"path": "test_chunk.py", "content": body})
	res, err := writeFileTool().Execute(json.RawMessage(args), ctx)
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	// The contract here changed on measured evidence. Rejecting broken
	// content in a NEW file forbade the write-run-read-the-traceback loop —
	// the one the no-tool baseline resolves its own syntax errors with at
	// 85-100% — and sessions died as "solve.py was never created" instead.
	// The write now LANDS with a warning. What this test still pins is the
	// original point: V3 must not be fed content that does not parse.
	if res == nil || !res.Success {
		t.Fatalf("a new file lands with a warning, got %+v", res)
	}
	var out WriteFileOutput
	if json.Unmarshal(res.Data, &out) != nil || out.Warning == "" {
		t.Fatalf("the result must warn that the content does not parse: %s", string(res.Data))
	}
	if v3Called {
		t.Error("V3 must not be called for content that does not parse")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Error("the file must be on disk so the model can run it")
	}
}

// A rejection that lands after the tool_call event has gone out must still
// produce a tool_result, or the consumer sees a call that never resolves.
// Observed live: a model tried to overwrite a fixture input, the
// surgical-edit gate refused, and the session's tool_call and tool_result
// counts disagreed by one — the TUI printed the call row and nothing after.
func TestBounceToolCallEmitsAMatchingResult(t *testing.T) {
	var events []string
	ctx := &AgentContext{
		StreamFn: func(evt string, data interface{}) { events = append(events, evt) },
	}
	st := &runState{turn: 3, response: "{}", pendingToolCall: "write_file"}
	st.bounceToolCall(ctx, "write_file", "write_file is for creating files")

	// The bounce answers the call, so the outstanding-call marker has to
	// clear with it. Left set, a later exit answers the same call a second
	// time through endStream and the counts disagree the other way.
	if st.pendingToolCall != "" {
		t.Errorf("bounce answered the call but left it marked outstanding (%q)",
			st.pendingToolCall)
	}

	var results int
	for _, e := range events {
		if e == "tool_result" {
			results++
		}
	}
	if results != 1 {
		t.Errorf("expected exactly one tool_result, got %d (events=%v)", results, events)
	}
	// The model still has to receive the rejection in its conversation.
	if len(ctx.Messages) < 2 {
		t.Fatalf("bounce must append the assistant+tool messages, got %d", len(ctx.Messages))
	}
	last := ctx.Messages[len(ctx.Messages)-1]
	if last.Role != "tool" || !strings.Contains(last.Content, "creating files") {
		t.Errorf("rejection must reach the model, got %+v", last)
	}
}

// The exit gates fire before any tool_call is streamed, so they must NOT
// emit a result for a call the consumer never saw.
func TestPlainBounceEmitsNoToolResult(t *testing.T) {
	var events []string
	ctx := &AgentContext{
		StreamFn: func(evt string, data interface{}) { events = append(events, evt) },
	}
	st := &runState{turn: 1, response: "{}"}
	st.bounce(ctx, "verification_gate", "run the tests first")
	for _, e := range events {
		if e == "tool_result" {
			t.Errorf("a gate bounce must not fabricate a tool_result, got %v", events)
		}
	}
}

// A NEW file that does not parse is unambiguously wrong. The direct-write
// path carried no syntax gate because the sandbox's YAML checker rejected
// multi-document files (valid YAML, and the shape every Kubernetes manifest
// uses); with that checker fixed, the gate is safe here. Observed: a 4-line
// test_discount.py with an unterminated string reached disk this way.
func TestDirectWriteLandsAnUnparseableNewFileWithAWarning(t *testing.T) {
	dir := t.TempDir()
	sb := fakeSyntaxSandbox(t, "UNTERMINATED")
	defer sb.Close()
	ctx := writeGateCtx(t, "", sb.URL, dir)
	ctx.V3URL = "" // force the direct path

	res, err := writeFileTool().Execute(
		json.RawMessage(`{"path":"t.py","content":"x = \"UNTERMINATED\n"}`), ctx)
	if err != nil && res == nil {
		t.Fatalf("write_file: %v", err)
	}
	// Fail-forward: a NEW file lands with a warning so the model can run it
	// and read the real traceback (see TestWriteFileRejectsUnparseable...
	// for the measured reasoning). An EXISTING file is still protected.
	if res == nil || !res.Success {
		t.Errorf("an unparseable new file lands with a warning, got %+v", res)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "t.py")); statErr != nil {
		t.Error("the file must be on disk so the model can run it")
	}
}

// A clean new file still writes — the gate must not block ordinary creation.
func TestDirectWriteAllowsACleanNewFile(t *testing.T) {
	dir := t.TempDir()
	sb := fakeSyntaxSandbox(t, "UNTERMINATED")
	defer sb.Close()
	ctx := writeGateCtx(t, "", sb.URL, dir)
	ctx.V3URL = ""

	res, err := writeFileTool().Execute(
		json.RawMessage(`{"path":"ok.py","content":"x = 1\n"}`), ctx)
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a clean new file must write, got %+v", res)
	}
}

// A long multi-line anchor is a transcription burden this model does not
// survive: asked to reproduce a ten-line block it emitted "safe_load_aller"
// for "safe_load_all". The mismatch should say so, not just "not found".
func TestLongOldStrMismatchAsksForASingleLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.py")
	if err := os.WriteFile(path, []byte("x = 1\ny = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.Ctx = context.Background()
	ctx.RecordFileRead(path, "x = 1\ny = 2\n")

	long := "a\nb\nc\nd\ne\nf"
	args, _ := json.Marshal(map[string]string{
		"path": "m.py", "old_str": long, "new_str": "z"})
	_, err := editFileTool().Execute(json.RawMessage(args), ctx)
	if err == nil {
		t.Fatal("expected a mismatch error")
	}
	if !strings.Contains(err.Error(), "6 lines") {
		t.Errorf("must name the anchor length, got %q", err)
	}
	if !strings.Contains(err.Error(), "ONE short line") {
		t.Errorf("must ask for a single line, got %q", err)
	}
	// A tool added later has to be named where the model is actually stuck,
	// not only in the tool list. insert_after shipped and both steers still
	// pointed only at edit_file and structural_edit.
	if !strings.Contains(err.Error(), "insert_after") {
		t.Errorf("must offer insert_after for additions, got %q", err)
	}
}

// A short anchor that misses keeps the ordinary mismatch guidance.
func TestShortOldStrMismatchKeepsNormalAdvice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.py")
	if err := os.WriteFile(path, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.Ctx = context.Background()
	ctx.RecordFileRead(path, "x = 1\n")
	args, _ := json.Marshal(map[string]string{
		"path": "m.py", "old_str": "nope = 9", "new_str": "z"})
	_, err := editFileTool().Execute(json.RawMessage(args), ctx)
	if err == nil {
		t.Fatal("expected a mismatch error")
	}
	if strings.Contains(err.Error(), "ONE short line") {
		t.Errorf("short anchor must not get the long-anchor advice: %q", err)
	}
}

// The live failure: "Yes, please list the files" produced a plan of
// read_file app.py -> structural_edit app.py -> run_command python app.py.
// The model listed the directory as asked, then followed the plan into
// editing the file it found. A wrong plan is worse than no plan.
func TestReadOnlyRequestsGetNoPlan(t *testing.T) {
	ctx := &AgentContext{Tier: Tier1Simple}
	for _, m := range []string{
		"Yes, please list the files",
		"please list the files in this directory",
		"show me the config file",
		"what files are in the project",
	} {
		if shouldGeneratePlan(ctx, m) {
			t.Errorf("read-only request should not be planned: %q", m)
		}
	}
	// Work still gets a plan.
	for _, m := range []string{
		"add a pause feature to the snake game",
		"find and fix the collision bug",
		"refactor the game loop into smaller functions",
	} {
		if !shouldGeneratePlan(ctx, m) {
			t.Errorf("work request lost its plan: %q", m)
		}
	}
}

// The read_file result the model copies from sits thousands of tokens back,
// behind the system prompt and every tool description, while its own emitted
// copy sits at the very end. Restating the file at the generation point makes
// the source reachable at the same distance as the model's own output.
func TestLastReadIsRestatedAtTheGenerationPoint(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.RecordFileRead(filepath.Join(dir, "app.py"), "alpha\nbravo\n")

	wire := []map[string]string{{"role": "user", "content": "fix the bug"}}
	got := appendLastReadRestatement(ctx, wire)
	if len(got) != 2 {
		t.Fatalf("expected a restatement appended, got %d messages", len(got))
	}
	last := got[len(got)-1]["content"]
	for _, want := range []string{"app.py", "1\talpha", "2\tbravo", "NOT in the file"} {
		if !strings.Contains(last, want) {
			t.Errorf("restatement missing %q:\n%s", want, last)
		}
	}
}

func TestRestatementSkipsWhenItWouldDuplicate(t *testing.T) {
	// Immediately after read_file the content is already the last message.
	// Restating there costs prompt-processing on every turn and buys nothing.
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.RecordFileRead(filepath.Join(dir, "a.py"), "alpha\n")

	wire := []map[string]string{{"role": "user", "content": "here it is: alpha\n"}}
	if got := appendLastReadRestatement(ctx, wire); len(got) != 1 {
		t.Errorf("must not duplicate content already at the end, got %d messages", len(got))
	}
}

func TestRestatementSkipsBigFilesAndEmptyState(t *testing.T) {
	dir := t.TempDir()
	wire := []map[string]string{{"role": "user", "content": "x"}}

	// Nothing read yet.
	fresh := NewAgentContext(dir, Tier1Simple)
	if got := appendLastReadRestatement(fresh, wire); len(got) != 1 {
		t.Error("restated with no file read")
	}
	// Too big to be worth pasting every turn.
	big := NewAgentContext(dir, Tier1Simple)
	big.RecordFileRead(filepath.Join(dir, "big.py"), strings.Repeat("x", restatementMaxBytes+1))
	if got := appendLastReadRestatement(big, wire); len(got) != 1 {
		t.Error("restated a file over the size cap")
	}
	// Escape hatch works.
	t.Setenv("ATLAS_RESTATE_LAST_READ", "0")
	off := NewAgentContext(dir, Tier1Simple)
	off.RecordFileRead(filepath.Join(dir, "a.py"), "alpha\n")
	if got := appendLastReadRestatement(off, wire); len(got) != 1 {
		t.Error("ATLAS_RESTATE_LAST_READ=0 did not disable it")
	}
}

// The model had written 5,897 characters answering a question, began
// repeating itself, the loop detector cut the stream, and the user received
// nothing — the closing quote and brace were missing so nothing parsed. The
// tool-call path had recoverTruncatedToolCall; a text answer had no
// equivalent.
func TestACutTextAnswerIsSalvagedRatherThanDiscarded(t *testing.T) {
	body := "The issue is in `scoring.py` within the `score_candidate` function " +
		"(lines 122-130). Line 129 returns the raw value instead of the normalised " +
		"one, so every candidate scores identically and the tie-break never runs. " +
		"The fix is to divide by the max before returning, which is what the " +
		"docstring already describes and what the caller assumes."
	raw := `{"type":"text","content":"` + strings.ReplaceAll(body, `"`, `\"`)

	got, ok := recoverTruncatedText(raw)
	if !ok {
		t.Fatal("a cut text answer was discarded")
	}
	if !strings.Contains(got, "scoring.py") || !strings.Contains(got, "tie-break never runs") {
		t.Errorf("salvaged content lost the answer:\n%s", got)
	}

	// Escapes have to survive the hand-rolled walk.
	esc := `{"type":"text","content":"line one\nline two\ta \"quoted\" bit, and a \\ backslash. ` +
		strings.Repeat("padding to clear the length floor. ", 8)
	out, ok := recoverTruncatedText(esc)
	if !ok {
		t.Fatal("escaped content was discarded")
	}
	for _, want := range []string{"line one\nline two\ta", `"quoted"`, `\ backslash`} {
		if !strings.Contains(out, want) {
			t.Errorf("escape handling lost %q:\n%s", want, out)
		}
	}

	// A fragment too short to be useful is more likely to mislead than help.
	if _, ok := recoverTruncatedText(`{"type":"text","content":"the issue is`); ok {
		t.Error("salvaged a fragment too short to be worth showing")
	}
	// A tool call is not a text answer.
	if _, ok := recoverTruncatedText(`{"type":"tool_call","name":"write_file","args":{"path":"a.py"`); ok {
		t.Error("mistook a truncated tool call for a text answer")
	}
}

// Code inside a tool_call stream is legitimately self-similar — a grid
// walker's four elif-direction branches repeat a 48-char window 4 times in a
// perfectly healthy file. With one threshold for everything, the detector
// cut healthy write_file drafts at the same structural spot every session:
// measured across one 50-task run, 17 cuts, 10 truncating write_file code,
// and the two most self-similar families (walk, debounce) went 0/5 each.
// A real spiral runs to max_tokens — hundreds of repeats — so demanding 10
// inside tool calls keeps the guard without shooting healthy code.
func TestBranchShapedCodeIsNotALoop(t *testing.T) {
	branch := "\\n    elif direction == '%s':\\n        y = (y + 1) %% 20"
	code := `{"type":"tool_call","name":"write_file","args":{"path":"solve.py","content":"`
	for _, d := range []string{"N", "S", "E", "W"} {
		code += fmt.Sprintf(branch, d)
	}
	code += `"}}`
	if n := loopingTailCount(code); n >= toolCallLoopThreshold {
		t.Fatalf("4 branch repeats must stay under the tool-call threshold, count=%d", n)
	}
}

func TestASpiralInsideAToolCallStillCuts(t *testing.T) {
	code := `{"type":"tool_call","name":"write_file","args":{"content":"`
	for i := 0; i < 40; i++ {
		code += "# I'll just print the answer. Wait, I can't see it.\\n"
	}
	if n := loopingTailCount(code); n < toolCallLoopThreshold {
		t.Fatalf("a 40x repeat is a spiral at any threshold, count=%d", n)
	}
}

// Truncation recovery must never panic on the buffers it exists to handle.
//
// Found by fuzzing: the content offset was re-derived by probing a fixed
// 15-byte window (`partial[idx:idx+15]`), which reads past the end whenever
// the content marker lands within 15 bytes of the buffer's end. Truncation
// puts the marker near the end by definition, so the panic sat in the one
// path built for truncated output — and net/http answers a panicking handler
// by closing the connection, so the user's session died mid-stream with no
// `done` event and no error.
func TestTruncationRecoveryNeverPanicsNearTheBufferEnd(t *testing.T) {
	cases := []string{
		`{"name":"write_file"content":"`,
		`{"type":"tool_call","name":"write_file","args":{"path":"a.py","content":"`,
		`{"type":"tool_call","name":"write_file","args":{"path":"a.py","content": "`,
		`"content":"`,
		`"content": "`,
		`{"content":"x`,
	}
	for _, partial := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic on truncated buffer %q: %v", partial, r)
				}
			}()
			// The parser must not panic on a truncated buffer.
			_, _ = extractModelResponse(partial)
		}()
	}
}

// A fenced sub-call whose model streams the file into reasoning_content never
// resets the content watchdog, so the watchdog cancels the request. The file
// is sitting in reasoning_content, and the salvage must run on the cut rather
// than the error being returned with the buffer discarded. Observed
// 2026-09-14: a second file's sub-call on a large context was cut this way and
// the run died having delivered only the first file.
func TestFencedSubCallSalvagesReasoningOnAWatchdogCut(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "1")
	t.Setenv("ATLAS_FENCED_IDLE_SEC", "1")
	fileBody := "<!DOCTYPE html>\n<html>\n<body><h1>Runs</h1></body>\n</html>\n"
	reasoning := "Let me write the template.\n```html\n" + fileBody + "```\n"
	rjson, _ := json.Marshal(reasoning)
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// Only the chat endpoint streams and holds open. The
			// prompt-progress poller hits /slots on this same server; if that
			// blocked too, its wait group would never release and the call
			// could never return.
			if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			// The whole file arrives as reasoning_content — never as content,
			// so progress() (content-only) never fires and the 1s
			// first-content watchdog cancels the request.
			sseWrite(w, `data: {"choices":[{"delta":{"reasoning_content":`+string(rjson)+`}}]}`)
			// Hold the stream open until the watchdog cancels, but never
			// past a bound: httptest's Close waits on live handlers, so an
			// unbounded wait here hangs teardown rather than the test.
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		}))
	defer srv.Close()

	ctx := llmTestCtx(srv.URL)
	content, _, err := callLLMOnceWithGrammar(ctx, ctx.Messages, 0.2, rawEmissionSentinel)
	if err != nil {
		t.Fatalf("a watchdog-cut sub-call errored instead of salvaging the file: %v", err)
	}
	if !strings.Contains(content, "<!DOCTYPE html>") || !strings.Contains(content, "</html>") {
		t.Errorf("salvaged content lost the file body: %q", content)
	}

	// The control: a real session cancel (not our watchdog) is NOT salvaged —
	// the caller must see the cancellation, not a half-generated file.
	cctx := llmTestCtx(srv.URL)
	cancelCtx, cancel := context.WithCancel(context.Background())
	cctx.Ctx = cancelCtx
	cancel() // session already cancelled
	if _, _, err := callLLMOnceWithGrammar(cctx, cctx.Messages, 0.2, rawEmissionSentinel); err == nil {
		t.Error("a cancelled session salvaged a file instead of surfacing the cancel")
	}
}

// A fenced sub-call whose stream opens and then says nothing is dead: on every
// healthy stream the first content frame follows the opening frame in the same
// millisecond. Waiting the whole first-content budget for it burned ~2 minutes
// of a ~9.5 minute session on each file (observed 2026-09-14, dev7/dev8 each
// delivered one file and ran out of time). The stalled deadline must cut it
// early, well before the first-content budget.
func TestFencedSubCallCutsAStreamThatOpensAndGoesSilent(t *testing.T) {
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "60") // the full budget
	t.Setenv("ATLAS_FENCED_STALL_SEC", "2")          // the stalled deadline
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			// Exactly what the live failure sends: the opening role frame,
			// then silence while the server generates out of sight.
			sseWrite(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":null}}]}`)
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
		}))
	defer srv.Close()

	ctx := llmTestCtx(srv.URL)
	start := time.Now()
	_, _, err := callLLMOnceWithGrammar(ctx, ctx.Messages, 0.2, rawEmissionSentinel)
	elapsed := time.Since(start)
	t.Logf("stalled sub-call returned after %s (err=%v)", elapsed.Round(time.Millisecond), err)

	if err == nil {
		t.Error("a stream that never sent content returned success")
	}
	if elapsed > 20*time.Second {
		t.Errorf("stalled stream was not cut early: took %s, want the ~2s stalled deadline "+
			"rather than the 60s first-content budget", elapsed.Round(time.Millisecond))
	}
}

// A model that degenerates into repetition is the failure this system exists
// to absorb. Ending the run the first time it happens hands back a half-built
// project: measured 2026-09-14, a run with three of five files written was
// terminated on the first loop with the fourth never attempted. The run must
// answer the loop and keep working, bounded.
func TestContentLoopIsCorrectedBeforeItEndsTheRun(t *testing.T) {
	dir := t.TempDir()
	const good = "def solve():\n    return 7\n\n\nprint(solve())\n"
	var mu sync.Mutex
	turns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			// The workspace-alignment probe: the proxy writes a marker and
			// asks the sandbox to read it back. Without this the run aborts
			// as workspace_misaligned before reaching the loop under test.
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		io.ReadAll(r.Body)
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if i == 0 {
			// Turn 0: degenerate into repetition mid tool-call. The loop
			// detector needs enough repeated tail to fire.
			var b strings.Builder
			b.WriteString(`{"type":"tool_call","name":"write_file","args":{"path":"solve.py","content":"`)
			for j := 0; j < 400; j++ {
				b.WriteString("the same line over and over and over. ")
			}
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{{"delta": map[string]string{"content": b.String()}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
			return
		}
		// After the corrective: do the work properly, then finish.
		var call map[string]interface{}
		if i == 1 {
			call = writeCall("solve.py", good)
		} else {
			call = map[string]interface{}{"type": "done", "summary": "wrote solve.py"}
		}
		c, _ := json.Marshal(call)
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(c)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 0
	recoveries := 0
	terminal := map[string]string{}
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		if et == "agent_loop_recovery" {
			recoveries++
		}
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	runAgentLoop(ctx, "Write solve.py so it prints 7.")

	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	t.Logf("recoveries=%d status=%q reason=%q disk=%q",
		recoveries, terminal["status"], terminal["reason"], string(got))

	if recoveries == 0 {
		t.Fatal("the repetition cut ended the run instead of correcting the model")
	}
	if string(got) != good {
		t.Errorf("the run never produced the file after recovering: %q", got)
	}
	if !NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("a run that recovered and did the work reported %q", terminal["status"])
	}
}

// The lint named the exact defect at write time -- "calls "/add_book", but
// no Flask route matches it" -- called it advisory, and the run finished with
// a form that 404s (acceptance run, 2026-09-15). The exit gate must ask for it
// before the run can finish, and a run that fixes it finishes.
func TestDoneIsBouncedWhileAFormPostsToAMissingRoute(t *testing.T) {
	dir := t.TempDir()
	appPy := "from flask import Flask, render_template\n" +
		"app = Flask(__name__)\n" +
		"@app.route('/', methods=['GET', 'POST'])\ndef index():\n" +
		"    return render_template('index.html')\n"
	badPage := "<form method=\"POST\" action=\"/add_book\"><input name=\"title\"></form>\n"
	var mu sync.Mutex
	turns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		io.ReadAll(r.Body)
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		var call map[string]interface{}
		switch i {
		case 0:
			call = writeCall("app.py", appPy)
		case 1:
			call = writeCall("templates/index.html", badPage)
		case 2:
			call = map[string]interface{}{"type": "done", "summary": "built the book site"}
		case 3:
			// Bounced: the model reads and fixes the action.
			call = map[string]interface{}{"type": "tool_call", "name": "read_file",
				"args": map[string]string{"path": "templates/index.html"}}
		case 4:
			call = map[string]interface{}{"type": "tool_call", "name": "edit_file",
				"args": map[string]string{"path": "templates/index.html",
					"old_str": `action="/add_book"`, "new_str": `action="/"`}}
		default:
			call = map[string]interface{}{"type": "done", "summary": "fixed the form action"}
		}
		c, _ := json.Marshal(call)
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(c)}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 0
	var gates []string
	terminal := map[string]string{}
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		if et == "gate" {
			var g struct{ Gate, Reason string }
			if json.Unmarshal(b, &g) == nil && g.Gate == "route_contract_gate" {
				gates = append(gates, g.Reason)
			}
		}
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	runAgentLoop(ctx, "Create app.py and templates/index.html for a book list.")

	page, _ := os.ReadFile(filepath.Join(dir, "templates/index.html"))
	t.Logf("route gates=%d status=%q reason=%q page=%q", len(gates), terminal["status"], terminal["reason"], page)
	if len(gates) != 1 {
		t.Fatalf("expected exactly one route_contract_gate bounce, got %d", len(gates))
	}
	if !strings.Contains(gates[0], "/add_book") {
		t.Errorf("the gate must name the unmatched target: %s", gates[0])
	}
	// The full message the model receives steers to the cheap repair.
	msg := routeContractMessage(routeContractFindings(dir))
	if !strings.Contains(msg, "insert_after") || !strings.Contains(msg, "not a whole-file write_file") {
		t.Errorf("the gate must say how to repair without re-entering the pipeline: %s", msg)
	}
	if !strings.Contains(string(page), `action="/"`) {
		t.Errorf("the fix never landed: %q", page)
	}
	if !NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("a run that fixed the route reported %q (%s)", terminal["status"], terminal["reason"])
	}
}

// The gate is about this run's own contract. A project that already had a
// stale form action, asked a question about, must get its answer -- the other
// exit gates are scoped the same way (files this run warned, jobs this run
// started), and an unscoped route gate would bounce that answer three times.
func TestRouteContractGateLeavesAQuestionAboutAnOldProjectAlone(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "templates"), 0o755)
	os.WriteFile(filepath.Join(dir, "app.py"), []byte("from flask import Flask\napp = Flask(__name__)\n@app.route('/')\ndef index():\n    return 'x'\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "templates/index.html"), []byte("<form action=\"/add_book\"></form>\n"), 0o644)
	ctx := NewAgentContext(dir, Tier0Conversational)
	if len(routeContractFindings(dir)) == 0 {
		t.Fatal("fixture has no mismatch; the test proves nothing")
	}
	// Nothing written this session: the gate must stay out of the way.
	if sessionWroteWebFiles(ctx) {
		t.Fatal("an empty session claims to have written web files")
	}
	st := &runState{}
	if gate, _ := st.exitGates(ctx, "what does app.py do?", "It serves one route."); gate == "route_contract_gate" {
		t.Error("a question about a pre-existing project was bounced by the route gate")
	}
	// Once the run has written a page, the same mismatch is its own.
	ctx.SessionWrites = map[string]bool{"templates/index.html": true}
	if gate, _ := st.exitGates(ctx, "fix the form", "done"); gate != "route_contract_gate" {
		t.Errorf("after writing the page the gate must fire, got %q", gate)
	}
}

// A file the run executed cleanly is demonstrated, whatever parse verdict the
// route that wrote it left behind. Observed 2026-09-15: a structural_edit
// landed through V3 with no verdict, the server then started fine, and the
// run was told at completion the file was never written in a checkable state.
func TestACleanExecutionSettlesContentDebt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.py"), []byte("print('served')\n"), 0o644)
	ctx := NewAgentContext(dir, Tier2Medium)
	key := ledgerKey(ctx, "app.py")
	// A prior syntax checkpoint that restoration may still need...
	observeDeliverable(ctx, key, []byte("print('old')\n"), ValidationKindSyntax, ValidationPassed, "")
	// ...and then the state a content debt actually lives in: new bytes on
	// disk that no route ever gave a verdict (a structural_edit that landed
	// through V3). The ledger knows the bytes changed and knows nothing else.
	ctx.LedgerMu.Lock()
	d0 := ctx.Ledger[key]
	d0.CurrentHash, d0.CurrentSize = hashBytes([]byte("print('served')\n")), len("print('served')\n")
	d0.ValidationKind, d0.ValidationStatus, d0.ValidatedHash = ValidationKindUnknown, ValidationUnknown, ""
	ctx.LedgerMu.Unlock()
	newState := func() *runState {
		return &runState{mutationDebt: map[string]*mutationDebtEntry{
			key: {Rel: "app.py", Kind: debtContent}}}
	}

	// Naming the file is not running it.
	st := newState()
	settleDebtByExecution(ctx, st, "cat app.py", true)
	if len(st.mutationDebt) != 1 {
		t.Error("`cat app.py` settled a content debt")
	}
	// A failed execution settles nothing.
	st = newState()
	settleDebtByExecution(ctx, st, "python3 app.py", false)
	if len(st.mutationDebt) != 1 {
		t.Error("a failed run settled a content debt")
	}
	// A clean execution settles it, against the bytes that ran, as its own kind.
	st = newState()
	settleDebtByExecution(ctx, st, "python3 app.py", true)
	if len(st.mutationDebt) != 0 {
		t.Fatalf("a clean run left the debt: %v", st.mutationDebt)
	}
	d := ctx.Ledger[key]
	if k, s := d.CurrentValidation(); k != ValidationKindExecution || s != ValidationPassed {
		t.Errorf("verdict = %s/%s, want execution/passed", k, s)
	}
	if d.CurrentHash != hashBytes([]byte("print('served')\n")) {
		t.Error("the verdict does not describe the bytes that ran")
	}
	// The syntax checkpoint was not disturbed: an execution is not a write.
	if d.CheckpointKind != ValidationKindSyntax || string(d.CheckpointBytes) != "print('old')\n" {
		t.Errorf("execution overwrote the syntax checkpoint: kind=%s bytes=%q", d.CheckpointKind, d.CheckpointBytes)
	}

	// The executor's own answer decides success.
	zero, one := 0, 1
	mk := func(v interface{}) *ToolResult { b, _ := json.Marshal(v); return &ToolResult{Success: true, Data: b} }
	if !executionSucceeded("run_command", mk(RunCommandOutput{ExitCode: 0})) {
		t.Error("exit 0 is a success")
	}
	if executionSucceeded("run_command", mk(RunCommandOutput{ExitCode: 1})) {
		t.Error("exit 1 is not a success")
	}
	if executionSucceeded("run_command", mk(RunCommandOutput{ExitCode: 0, TimedOut: true})) {
		t.Error("a killed command is not a success")
	}
	if !executionSucceeded("run_background", mk(RunBackgroundOutput{Running: true})) {
		t.Error("a server still serving after its settle window is a success")
	}
	if executionSucceeded("run_background", mk(RunBackgroundOutput{Running: false, ExitCode: &one})) {
		t.Error("a background job that died with exit 1 is not a success")
	}
	if !executionSucceeded("run_background", mk(RunBackgroundOutput{Running: false, ExitCode: &zero})) {
		t.Error("a background script that exited 0 is a success")
	}
}

// --- swallowed-content detection (scenario D, 2026-09-15) ------------------
//
// A write_file whose content string carries an unescaped `"` closes the JSON
// string early; the file's remainder is absorbed into a junk key that the
// typed struct discards, so a truncated file lands reported as a clean write.
// The envelope parses, so extractModelResponse cannot catch it — the loop's
// post-parse detector must.

func TestKnownArgKeysReadsTheToolSchema(t *testing.T) {
	k := knownArgKeys("write_file")
	if !k["path"] || !k["content"] || len(k) != 2 {
		t.Fatalf("write_file keys = %v, want {path, content}", k)
	}
	if e := knownArgKeys("edit_file"); !e["old_str"] || !e["new_str"] || !e["path"] {
		t.Errorf("edit_file keys = %v, want path/old_str/new_str", e)
	}
	if knownArgKeys("no_such_tool") != nil {
		t.Error("an unknown tool must yield nil, not a claim about its shape")
	}
}

func TestAnUnescapedQuoteTruncatesTheWriteAndIsCaught(t *testing.T) {
	// The D shape, minimized: content is `<script>x("y"` and the bare quote
	// after it ends the string; the rest of the "file" becomes a long key
	// with value "DONE".
	raw := `{"type":"tool_call","name":"write_file","args":{"path":"a.html","content":"<script>x(\"y\"",");\nthe rest of the file body running well past forty characters\n</script>":"DONE"}}`

	parsed, err := extractModelResponse(raw)
	if err != nil {
		t.Fatalf("the envelope is valid JSON and must parse: %v", err)
	}
	var in WriteFileInput
	if e := json.Unmarshal(parsed.Args, &in); e != nil {
		t.Fatalf("args decode: %v", e)
	}
	// The bug: the typed decode silently truncated the file.
	if strings.Contains(in.Content, "</script>") {
		t.Fatalf("expected the truncated shape (no </script>), got %q", in.Content)
	}
	// The fix: the loop's detector refuses it and explains why.
	fb, bad := swallowedContentFeedback(parsed.Name, parsed.Args)
	if !bad {
		t.Fatal("a content string cut by an unescaped quote must be caught")
	}
	if !strings.Contains(fb, "unescaped") || !strings.Contains(fb, "NOT performed") {
		t.Errorf("feedback must name the cause and say the write did not happen:\n%s", fb)
	}
}

func TestACleanWriteIsNotFlaggedAsSwallowed(t *testing.T) {
	args, _ := json.Marshal(WriteFileInput{
		Path:    "templates/index.html",
		Content: "<!DOCTYPE html>\n<html>\n<script>const x = \"ok\";</script>\n</html>\n",
	})
	if _, bad := swallowedContentFeedback("write_file", args); bad {
		t.Error("a well-formed write must not be flagged")
	}
	// An edit_file with real, correctly-escaped multi-line bodies is clean too.
	eargs, _ := json.Marshal(EditFileInput{
		Path: "app.py", OldStr: "def f():\n    return 1\n", NewStr: "def f():\n    return 2\n",
	})
	if _, bad := swallowedContentFeedback("edit_file", eargs); bad {
		t.Error("a well-formed edit must not be flagged")
	}
}

func TestAShortUnexpectedKeyIsNotMistakenForSwallowedContent(t *testing.T) {
	// A model that adds a small stray field (a short identifier-shaped key) is
	// not the truncation shape — that is liftMissingArgs / harmless territory,
	// and flagging it would refuse a real write.
	raw := `{"path":"a.html","content":"<html></html>","note":"fyi"}`
	if _, bad := swallowedContentFeedback("write_file", json.RawMessage(raw)); bad {
		t.Error("a short extra key must not trip the detector")
	}
}

func TestNonEditToolsAreNotSubjectToTheContentCheck(t *testing.T) {
	// read_file has no long free-text field; an odd key there is not this bug.
	raw := `{"path":"a.py","this key is long enough to look like leaked content":"x"}`
	if _, bad := swallowedContentFeedback("read_file", json.RawMessage(raw)); bad {
		t.Error("a tool with no content/old_str/new_str field must be exempt")
	}
}

// --- fenced channel session-wide disable (scenarios C, E, 2026-09-15) -------
//
// The fenced sub-call stalls (opens a stream, then silence) as a property of
// the session's llama state, so once one file's fetch stalls the next file's
// stalls too. After the first stall the channel is off session-wide and writes
// go inline (now safe via the swallowed-content detector), instead of paying a
// ~25s watchdog cut per remaining file.

func TestFencedChannelDisablesAfterOneStall(t *testing.T) {
	ctx := &AgentContext{}
	if fencedChannelDisabledForSession(ctx) {
		t.Fatal("a clean session must not have the channel disabled")
	}
	ctx.FencedStalls = fencedSessionStallLimit
	if !fencedChannelDisabledForSession(ctx) {
		t.Fatal("reaching the stall limit must disable the channel session-wide")
	}
}

func TestFencedFetchMakesNoSubCallOnceTheSessionIsDisabled(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			mu.Lock()
			calls++
			mu.Unlock()
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ctx := llmTestCtx(srv.URL)
	ctx.FencedStalls = fencedSessionStallLimit // already stalled earlier this run

	_, err := fetchFencedContent(ctx, "the original @fenced call", "second_file.py")
	if err == nil {
		t.Fatal("a disabled fenced channel must not resolve content")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("the error should explain the channel stalled: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("no sub-call may be made once the channel is disabled, got %d", calls)
	}
}

// A conversationally-phrased BUILD request must not be misread as a question
// and capped at the conversational tier. The lost-and-found acceptance prompt
// (2026-09-15) described item fields as "(what it is, where it was found)";
// the ", where" relative clause tripped the question heuristic, so a full
// web-app build was classified T0 chat, capped at 12 turns, and never
// finished. A relative clause keeps subject-verb order; only an inverted
// clause ("what DOES x do") is a question.
func TestClassifyAgentTierDescriptiveWhClauseIsNotAQuestion(t *testing.T) {
	build := "our school office gets buried in lost property. i want a simple web " +
		"page where a staff member can post an item (what it is, where it was found) " +
		"and later, when someone comes for it, mark it as claimed with the claimants " +
		"name so it drops off the open list onto a claimed list. it needs to keep " +
		"working after the office pc restarts. python please, and put how to run it in a readme"
	if isQuestionMessage(build) {
		t.Errorf("a descriptive field list (what it is, where it was found) must not read as a question")
	}
	if got := classifyAgentTier(build); got == Tier0Conversational {
		t.Errorf("classifyAgentTier = T0 — a web-app build must not be capped at the conversational tier")
	}
	// Inversion is still a question, with or without a '?'. The clause opener
	// is the wh-word immediately after ", " or ". ".
	for _, q := range []string{
		"In orders.py, what does find_duplicates do",
		"the config loads. where is it read from",
		"looks fine. how do I run the tests",
	} {
		if !isQuestionMessage(q) {
			t.Errorf("inverted wh-clause must read as a question: %q", q)
		}
	}
	// Relative clauses are descriptive, not questions.
	for _, d := range []string{
		"save the row with what it is and where it came from",
		"log when it happened and who it belonged to",
	} {
		if isQuestionMessage(d) {
			t.Errorf("relative clause must not read as a question: %q", d)
		}
	}
}

// Audit follow-up: a field list with PAST-tense wh-clauses must not read as a
// question either. Past auxiliaries were dropped from the inverted-question
// set, so "(what was lost, where were they found)" stays a build request.
func TestPastTenseFieldClauseIsNotAQuestion(t *testing.T) {
	for _, d := range []string{
		"log an incident: what was lost, where were they found, who did report it",
		"store what was returned and where it had been",
	} {
		if isQuestionMessage(d) {
			t.Errorf("a past-tense field list must not read as a question: %q", d)
		}
	}
	// Present-tense and modal questions are still detected mid-message.
	for _, q := range []string{
		"In orders.py, what does find_duplicates do",
		"the config loads. how do I run it",
	} {
		if !isQuestionMessage(q) {
			t.Errorf("present/modal inverted question must still be detected: %q", q)
		}
	}
}

// Work requests with no task verb were read as questions: a wh-word matched
// as a prefix ("Whole", "Whenever", "However"), a subordinate "When ..."
// clause, an imperative "Do ...", a mid-message "Do not ...", or a "?" in a
// URL. Classified T0, the run was capped and never planned, and for a client
// that sends no contract the action gate never armed.
func TestVerblessWorkIsNotAQuestion(t *testing.T) {
	for _, msg := range []string{
		"Whole-number inputs should be rejected by the parser in calc.py",
		"Whenever a user submits the form, store the entry in entries.json",
		"Whatever port is free, serve the site on it with a small Flask app",
		"However you structure it, the CLI in tool.py needs a --verbose flag",
		"Whichever sorting approach you pick, the rows in report.py need ordering by date",
		"When the timer hits zero the page in index.html should flash red",
		"Do the same thing for the /users route",
		"solve.py should print the total of input.txt. Do not hardcode the answer.",
		"The endpoint at /api/items?page=2 returns nothing when the list is empty",
		"the pattern `a?b` in parse.py matches too much",
	} {
		if got := classifyAgentTier(msg); got == Tier0Conversational {
			t.Errorf("classifyAgentTier(%q) = T0, want a work tier", msg)
		}
	}
}

// The questions stay questions.
func TestQuestionsStillClassifyAsQuestions(t *testing.T) {
	for _, msg := range []string{
		"why does the game store direction as a string",
		"what does the lens actually score here",
		"what's the difference between run_command and run_background",
		"is the sandbox mounted read-only?",
		"When does the cache expire",
		"where is the config file read",
		"When did we switch to SQLite",
		"how is the retry delay computed",
		"who calls parse_config",
		"which file handles authentication",
		"Do you know which module owns the cache",
		"In orders.py, what does find_duplicates do",
		"what does find_duplicates do? Just explain.",
		"The docs mention a cache. Is it shared between sessions",
	} {
		if got := classifyAgentTier(msg); got != Tier0Conversational {
			t.Errorf("classifyAgentTier(%q) = %v, want T0", msg, got)
		}
	}
}

// A client that declared work is never tiered as a question, whatever the
// message looks like. "Can the page also show today's date" is a request
// phrased as a question; only the declaration can tell.
func TestADeclaredWorkRequestIsNeverTieredAsAQuestion(t *testing.T) {
	const msg = "Can the page in index.html also show today's date"
	classified := classifyAgentTier(msg)
	if classified != Tier0Conversational {
		t.Fatalf("precondition: %q classified %v, want T0", msg, classified)
	}
	for _, c := range []struct {
		name string
		tc   *TaskContract
		in   Tier
		want Tier
	}{
		{"declared work", &TaskContract{TaskMode: TaskModeWork}, classified, Tier2Medium},
		{"declared question", &TaskContract{TaskMode: TaskModeQuestion}, classified, Tier0Conversational},
		{"no contract", nil, classified, Tier0Conversational},
		{"work never lowers a tier", &TaskContract{TaskMode: TaskModeWork}, Tier3Hard, Tier3Hard},
	} {
		if got := declaredTier(c.tc, c.in); got != c.want {
			t.Errorf("%s: declaredTier = %v, want %v", c.name, got, c.want)
		}
	}
	// handleAgent applies it once the contract is validated, and resets the
	// turn cap with it.
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	set := strings.Index(body, "ctx.TaskContract = validatedContract")
	apply := strings.Index(body, "declaredTier(validatedContract, ctx.Tier)")
	if set < 0 || apply < set {
		t.Error("handleAgent does not apply the declared tier after validating the contract")
	}
	if !strings.Contains(body[apply:apply+200], "TierMaxTurns(t)") {
		t.Error("the declared tier does not reset the turn cap")
	}
}
