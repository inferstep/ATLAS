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

	_, got := classifyParseFailure(raw)
	if !strings.Contains(got, "TRUNCATED") {
		t.Errorf("expected TRUNCATED callout, got %q", got)
	}
	if !strings.Contains(got, "shrink") && !strings.Contains(got, "smaller") {
		t.Errorf("expected actionable shrink advice, got %q", got)
	}
}

func TestClassifyParseFailureEmptyResponse(t *testing.T) {
	if _, got := classifyParseFailure(""); !strings.Contains(got, "empty") {
		t.Errorf("empty input should mention empty, got %q", got)
	}
	if _, got := classifyParseFailure("   \n\t "); !strings.Contains(got, "empty") {
		t.Errorf("whitespace-only should be treated as empty, got %q", got)
	}
}

func TestClassifyParseFailureMalformedToolCall(t *testing.T) {
	// Looks like a tool_call but ends cleanly — different feedback
	// than truncation.
	raw := `{"type":"tool_call","name":"read_file","args":{"path":"app.py",}}`
	_, got := classifyParseFailure(raw)
	if strings.Contains(got, "TRUNCATED") {
		t.Errorf("clean-ending malformed shouldn't say TRUNCATED, got %q", got)
	}
}

func TestClassifyParseFailureProse(t *testing.T) {
	raw := "Here's what I'll do: I'll read the file first..."
	_, got := classifyParseFailure(raw)
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

func TestApplyRepetitionSamplingDefaultsEnableDry(t *testing.T) {
	body := map[string]interface{}{}
	applyRepetitionSampling(body)

	if body["dry_multiplier"] != 0.8 {
		t.Fatalf("dry_multiplier = %v, want 0.8 (DRY must be on by default — "+
			"llama-server ships every repetition control disabled)", body["dry_multiplier"])
	}
	// Above llama.cpp's default of 2: 3-token runs are ordinary in source.
	if body["dry_allowed_length"] != 6 {
		t.Fatalf("dry_allowed_length = %v, want 6", body["dry_allowed_length"])
	}
	if body["dry_penalty_last_n"] != 2048 {
		t.Fatalf("dry_penalty_last_n = %v, want 2048 (bounded lookback, not -1)",
			body["dry_penalty_last_n"])
	}
	// repeat_penalty scores individual tokens and mangles code indentation;
	// it must stay off unless explicitly opted into.
	if _, ok := body["repeat_penalty"]; ok {
		t.Fatalf("repeat_penalty must not be set by default, got %v", body["repeat_penalty"])
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

func TestSandboxSubdirRejectsPath(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"message": "hi", "sandbox_subdir": "a/b"})
	req := httptest.NewRequest("POST", "/v1/agent", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleAgent(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != string(ErrInvalidInput) {
		t.Errorf("error = %q, want invalid_input", env.Error)
	}
	if !strings.Contains(env.Detail, "sandbox_subdir") || !strings.Contains(env.Detail, "bare directory name") {
		t.Errorf("detail = %q, want sandbox_subdir bare directory name requirement", env.Detail)
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
			got, _ := classifyParseFailure(tc.raw)
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

func TestShouldGeneratePlanV3BypassDisablesPlanner(t *testing.T) {
	ctx := &AgentContext{Tier: Tier3Hard, BypassV3: true}
	if shouldGeneratePlan(ctx, "Build and verify a multi-file service") {
		t.Fatal("V3-bypassed baseline request must not run the pre-flight planner")
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
// stuck-pattern detectors are the real safety net. T0 keeps a small
// cap as a SHAPE constraint (conversational input shouldn't loop).
// The agent loop treats MaxTurns == 0 as "no limit"; cancellation
// via ctx.Ctx is the upper bound.
func TestTierMaxTurnsUncappedDefaults(t *testing.T) {
	t.Setenv("ATLAS_MAX_TURNS", "")
	if got := TierMaxTurns(Tier0Conversational); got != 5 {
		t.Errorf("T0 = %d, want 5 (shape constraint)", got)
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
	if got := TierMaxTurns(Tier0Conversational); got != 5 {
		t.Errorf("env=0, T0 = %d, want 5 (shape cap preserved)", got)
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

// May 9 2026: under BiasBusters mitigations the model now reaches for
// structural_edit + edit_file too. Real flask test logs show 10K-12K char
// structural_edit responses parse-erroring with no recovery path. Lock the
// generalized recovery so future regressions can't slip back in.

func TestRecoverTruncatedStructuralEditFullPayload(t *testing.T) {
	// Well-formed but unparseable-as-JSON payload (e.g. trailing brace
	// dropped by the model). Recovery still extracts the fields.
	partial := `{"type":"tool_call","name":"structural_edit","args":{"path":"templates/index.html","selector":"<html>","content":"<!DOCTYPE html>\n<html lang=\"en\"><head></head><body>hi</body></html>"`
	resp, ok := recoverTruncatedToolCall(partial)
	if !ok {
		t.Fatal("recovery returned false")
	}
	if resp.Type != "tool_call" || resp.Name != "structural_edit" {
		t.Fatalf("got Type=%q Name=%q, want tool_call/structural_edit", resp.Type, resp.Name)
	}
	var args StructuralEditInput
	if err := json.Unmarshal(resp.Args, &args); err != nil {
		t.Fatalf("unmarshal recovered args: %v", err)
	}
	if args.Path != "templates/index.html" {
		t.Errorf("Path = %q, want templates/index.html", args.Path)
	}
	if args.Selector != "<html>" {
		t.Errorf("Selector = %q, want <html>", args.Selector)
	}
	if !strings.HasPrefix(args.Content, "<!DOCTYPE html>") {
		preview := args.Content
		if len(preview) > 20 {
			preview = preview[:20]
		}
		t.Errorf("Content prefix = %q, want <!DOCTYPE html>", preview)
	}
	if !strings.Contains(args.Content, `lang="en"`) {
		t.Errorf("Content missing unescaped lang=\"en\": %q", args.Content)
	}
}

func TestRecoverTruncatedStructuralEditMidContent(t *testing.T) {
	// Realistic case from May 9 logs: response cut off mid-content with
	// no closing quote/braces. Recovery returns whatever content made
	// it through so the agent can write SOMETHING useful and continue.
	partial := `{"type":"tool_call","name":"structural_edit","args":{"path":"app.py","selector":"function:dashboard","content":"@app.route('/dashboard')\ndef dashboard():\n    users = get_users()\n    return render_template(`
	resp, ok := recoverTruncatedToolCall(partial)
	if !ok {
		t.Fatal("recovery returned false on mid-content truncation")
	}
	var args StructuralEditInput
	if err := json.Unmarshal(resp.Args, &args); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if args.Path != "app.py" || args.Selector != "function:dashboard" {
		t.Errorf("path/selector wrong: %+v", args)
	}
	if !strings.Contains(args.Content, "def dashboard()") {
		t.Errorf("content missing def dashboard: %q", args.Content)
	}
}

func TestRecoverTruncatedEditFileBothFields(t *testing.T) {
	partial := `{"type":"tool_call","name":"edit_file","args":{"path":"app.py","old_str":"return None","new_str":"return {}","replace_all":false}`
	resp, ok := recoverTruncatedToolCall(partial)
	if !ok {
		t.Fatal("recovery returned false")
	}
	if resp.Name != "edit_file" {
		t.Errorf("Name = %q", resp.Name)
	}
	var args EditFileInput
	if err := json.Unmarshal(resp.Args, &args); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if args.Path != "app.py" || args.OldStr != "return None" || args.NewStr != "return {}" {
		t.Errorf("field recovery wrong: %+v", args)
	}
}

func TestRecoverTruncatedEditFileMidNewStr(t *testing.T) {
	// Truncated mid new_str — should still recover with what we have.
	partial := `{"type":"tool_call","name":"edit_file","args":{"path":"app.py","old_str":"return None","new_str":"return {\\"users\\":`
	resp, ok := recoverTruncatedToolCall(partial)
	if !ok {
		t.Fatal("recovery returned false")
	}
	var args EditFileInput
	_ = json.Unmarshal(resp.Args, &args)
	if args.OldStr != "return None" {
		t.Errorf("OldStr = %q", args.OldStr)
	}
	if !strings.HasPrefix(args.NewStr, "return ") {
		t.Errorf("NewStr should start with 'return ', got %q", args.NewStr)
	}
}

func TestRecoverTruncatedToolCallUnknownToolReturnsFalse(t *testing.T) {
	// Tool we don't have a recovery for → return false so caller falls
	// through to the diagnostic error (not silent failure).
	partial := `{"type":"tool_call","name":"read_file","args":{"path":"app.py"`
	if _, ok := recoverTruncatedToolCall(partial); ok {
		t.Error("expected no recovery for read_file")
	}
}

func TestRecoverTruncatedStructuralEditMissingSelectorFails(t *testing.T) {
	// Malformed — selector missing entirely. Recovery should fail
	// rather than emit a tool call with empty selector that structural_edit
	// would reject downstream anyway.
	partial := `{"type":"tool_call","name":"structural_edit","args":{"path":"app.py","content":"def foo(): pass"}`
	if _, ok := recoverTruncatedToolCall(partial); ok {
		t.Error("expected no recovery when selector is missing")
	}
}

// Locks the new diagnostic behavior: when the brace-balanced parse
// fails, extractModelResponse must surface the actual unmarshal error
// so logs tell us WHY ("invalid character '\\n'" vs "unexpected end")
// instead of a generic "could not parse JSON".
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

func TestRecoveryRejectsRepeatedNewlineContent(t *testing.T) {
	junk := strings.Repeat("\n", 400)
	partial := `{"type":"tool_call","name":"write_file","args":{"path":"app.py","content":"` +
		strings.ReplaceAll(junk, "\n", `\n`)

	if _, ok := recoverTruncatedToolCall(partial); ok {
		t.Fatal("recovered a write_file from 400 repeated newlines — degenerate output must not become a real write")
	}
}

func TestRecoveryRejectsRepeatingTailEditFile(t *testing.T) {
	tail := strings.Repeat("return None; return None; return None; return None;", 8)
	partial := `{"type":"tool_call","name":"edit_file","args":{"path":"app.py","old_str":"x = 1","new_str":"` + tail

	if _, ok := recoverTruncatedToolCall(partial); ok {
		t.Fatal("recovered an edit_file whose new_str is a repeating tail")
	}
}

func TestRecoveryRejectsDegenerateStructuralEditContent(t *testing.T) {
	junk := strings.Repeat(" ", 500)
	partial := `{"type":"tool_call","name":"structural_edit","args":{"path":"app.py","selector":"function:main","content":"` + junk

	if _, ok := recoverTruncatedToolCall(partial); ok {
		t.Fatal("recovered a structural_edit from 500 spaces")
	}
}

// The guard must not reject legitimate truncated code, which is the entire
// reason recovery exists.
func TestRecoveryStillAcceptsRealTruncatedCode(t *testing.T) {
	body := "def handler(request):\n" +
		"    user = get_user(request)\n" +
		"    if user is None:\n" +
		"        return abort(404)\n" +
		"    rows = query_orders(user.id)\n" +
		"    total = sum(r.amount for r in rows)\n" +
		"    return render_template('orders.html', rows=rows, total=total"
	partial := `{"type":"tool_call","name":"write_file","args":{"path":"app.py","content":"` +
		strings.ReplaceAll(strings.ReplaceAll(body, `"`, `\"`), "\n", `\n`)

	got, ok := recoverTruncatedToolCall(partial)
	if !ok {
		t.Fatal("rejected a legitimately truncated write_file — recovery must still work for real code")
	}
	if got.Name != "write_file" {
		t.Fatalf("recovered %q, want write_file", got.Name)
	}
}

// Indented code is whitespace-heavy but nowhere near the degenerate ratio.
func TestLooksDegenerateAllowsIndentedCode(t *testing.T) {
	code := "            result.append(transform(item, config, index))\n" +
		"            totals[key] = totals.get(key, 0) + item.amount\n" +
		"            if item.status == 'pending' and not item.archived:\n" +
		"                queue.push(item.id, priority=item.rank)\n" +
		"            seen.add(item.id)\n"
	if looksDegenerate(code) {
		t.Fatal("flagged ordinary deeply-indented code as degenerate")
	}

	// A long file that legitimately repeats a boilerplate line must survive:
	// repetition only counts as degeneracy when it dominates the value.
	boiler := strings.Repeat("x = compute(a, b, c, d, e, f, g, h, i, j, k)\n", 3)
	body := boiler + strings.Repeat("def f(q):\n    return q.value * 2 + offset(q)\n", 40)
	if looksDegenerate(body) {
		t.Fatal("flagged a long file with some repeated boilerplate as degenerate")
	}
}

func TestLooksDegenerateIgnoresShortValues(t *testing.T) {
	if looksDegenerate("\n\n\n\n") {
		t.Fatal("short values must be exempt — a small new_str cannot look degenerate")
	}
}

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
			payload, _ := json.Marshal(map[string]interface{}{
				"code": winnerCode, "passed": true,
				"phase_solved": "phase1", "candidates_tested": 3,
				"winning_score": 0.9,
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
	ctx.Ctx = context.Background()
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
// used to strip it after the fact. The strip now happens at the boundary, and
// it MUST run before the regression check: a fenced candidate does not parse as
// Python, so checking first would drop good candidates as "broken" and silently
// disable V3 for every fenced response.
func TestImproveContentV3SanitizesBeforeJudgingCandidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.py")
	modelEdit := "def index():\n    return 'ok'\n"
	code := "def index():\n    return 'better'\n"
	fenced := "Looking at the task, I need to update the handler.\n\n```python\n" + code + "```\n"

	v3 := v3AndStructuralServer(t, fenced, "render_template")
	defer v3.Close()
	sb := fakeSyntaxSandbox(t, "```") // a surviving fence would fail the gate
	defer sb.Close()
	ctx := writeGateCtx(t, v3.URL, sb.URL, dir)

	out, meta, err := improveContentWithV3(path, modelEdit, ctx)
	if err != nil {
		t.Fatalf("improveContentWithV3 error: %v", err)
	}
	if strings.Contains(out, "```") || strings.Contains(out, "Looking at the task") {
		t.Errorf("wrapper must be stripped at the boundary, got %q", out)
	}
	if strings.TrimSpace(out) != strings.TrimSpace(code) {
		t.Errorf("expected the unwrapped code, got %q", out)
	}
	if !meta.Used {
		t.Error("a fenced but otherwise clean candidate must still be adopted, not dropped as unparseable")
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
	if res == nil || res.Success {
		t.Fatalf("unparseable content must be rejected, got %+v", res)
	}
	if v3Called {
		t.Error("V3 must not be called for content that does not parse")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("nothing should have landed on disk")
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
	st := &runState{turn: 3, response: "{}"}
	st.bounceToolCall(ctx, "write_file", "write_file is for creating files")

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
func TestDirectWriteGatesAnUnparseableNewFile(t *testing.T) {
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
	if res != nil && res.Success {
		t.Error("an unparseable new file must not reach disk")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "t.py")); statErr == nil {
		t.Error("nothing should have been written")
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
