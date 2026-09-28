// PC-062: chat clients for the ATLAS agent stream and the demo's one-shot
// raw OpenAI-compatible stream.
//
// Two SSE protocols flow into this TUI:
//
//   /events  — typed Envelope stream (streamEvents, below) — pipeline visibility
//   /v1/agent — {type, data} chat stream (this file) — assistant reply
//   /v1/chat/completions — standard OpenAI SSE used only by the raw demo pane
//
// The chat stream is request-scoped: each user message opens a fresh
// SSE connection that closes when the agent loop returns [DONE]. This
// is intentional — chat turns are RPC-like, not a persistent feed.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// chatEvent mirrors the SSEEvent shape in proxy/types.go.
// Different from Envelope — this is the older, simpler chat protocol.
type chatEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// historyMessage is one prior-turn user/assistant text row sent to the
// proxy on each /v1/agent call. Field shape MUST match the historyMsg
// struct in proxy/agent.go's handleAgent.
type historyMessage struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

// agentRequest is the POST body for /v1/agent. Field tags MUST match
// the anonymous struct in proxy/agent.go's handleAgent.
type agentRequest struct {
	Message    string           `json:"message"`
	WorkingDir string           `json:"working_dir"`
	Mode       string           `json:"mode"`       // "default" | "accept-edits" | "yolo"
	SessionID  string           `json:"session_id"` // PC-062: required so /cancel can target this turn
	History    []historyMessage `json:"history,omitempty"`
	// SessionAllowedTools carries the tools the user approved for the whole
	// session so the proxy skips the interactive permission prompt for them
	// on later turns. Populated from the model's session allowlist.
	SessionAllowedTools []string `json:"session_allowed_tools,omitempty"`
	DisableFreshSlot    bool     `json:"disable_fresh_slot,omitempty"` // /demo flag — skip PC-045 so pre-warm survives
	SandboxSubdir       string   `json:"sandbox_subdir,omitempty"`     // /demo flag — write files into this subdir of the workspace
	// TaskContract is what this client declares about the request. Omitted
	// entirely when the caller declared nothing.
	TaskContract *taskContract `json:"task_contract,omitempty"`
}

type rawChatRequest struct {
	Model              string              `json:"model"`
	Messages           []map[string]string `json:"messages"`
	Temperature        float64             `json:"temperature"`
	MaxTokens          int                 `json:"max_tokens"`
	Stream             bool                `json:"stream"`
	StreamOptions      map[string]bool     `json:"stream_options"`
	ChatTemplateKwargs map[string]bool     `json:"chat_template_kwargs"`
}

// sendRawChat makes one direct OpenAI-compatible model request. It never uses
// /v1/agent, so the raw demo pane has no planner, tool loop, candidate search,
// filesystem access, or other orchestration layer.
func sendRawChat(ctx context.Context, proxyURL, modelID, message string,
	out chan<- chatEvent) error {
	if strings.TrimSpace(modelID) == "" {
		modelID = "local-model"
	}
	promptTokens := len(message) / 4
	out <- makeChatEvent("llm_call_start", map[string]int{"prompt_tokens": promptTokens})

	body, err := json.Marshal(rawChatRequest{
		Model:              modelID,
		Messages:           []map[string]string{{"role": "user", "content": message}},
		Temperature:        0.3,
		MaxTokens:          8192,
		Stream:             true,
		StreamOptions:      map[string]bool{"include_usage": true},
		ChatTemplateKwargs: map[string]bool{"enable_thinking": false},
	})
	if err != nil {
		return fmt.Errorf("encode raw request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(proxyURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build raw request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	setProxyAuth(req)

	started := time.Now()
	resp, err := (&http.Client{Transport: &http.Transport{
		ResponseHeaderTimeout: 60 * time.Second,
	}}).Do(req)
	if err != nil {
		return fmt.Errorf("raw model request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("raw model returned %d: %s", resp.StatusCode,
			strings.TrimSpace(string(responseBody)))
	}

	var content strings.Builder
	var reasoning strings.Builder
	firstToken := true
	totalTokens := 0
	completionTokens := 0
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage struct {
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Usage.TotalTokens > 0 {
			totalTokens = chunk.Usage.TotalTokens
			completionTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if firstToken && (delta.Content != "" || delta.ReasoningContent != "") {
			firstToken = false
			out <- makeChatEvent("llm_first_token", map[string]int64{
				"prompt_ms": time.Since(started).Milliseconds(),
			})
		}
		if delta.ReasoningContent != "" {
			reasoning.WriteString(delta.ReasoningContent)
			out <- makeChatEvent("reasoning_token", map[string]string{"text": delta.ReasoningContent})
		}
		if delta.Content != "" {
			content.WriteString(delta.Content)
			out <- makeChatEvent("llm_token", map[string]string{"text": delta.Content})
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read raw model stream: %w", err)
	}
	if content.Len() == 0 && reasoning.Len() > 0 {
		content.WriteString(reasoning.String())
	}
	if completionTokens == 0 {
		completionTokens = len(content.String()) / 4
	}
	if totalTokens == 0 {
		totalTokens = promptTokens + completionTokens
	}
	out <- makeChatEvent("llm_call_end", map[string]interface{}{
		"tokens":       totalTokens,
		"total_tokens": totalTokens,
		"chars":        content.Len(),
		"ms":           time.Since(started).Milliseconds(),
	})
	if content.Len() > 0 {
		out <- makeChatEvent("text", map[string]string{"content": content.String()})
	}
	return nil
}

func makeChatEvent(eventType string, payload interface{}) chatEvent {
	data, _ := json.Marshal(payload)
	return chatEvent{Type: eventType, Data: data}
}

// postPermissionDecision answers a mid-turn "permission_request" by POSTing to
// /v1/permission. decision is "allow" or "deny"; scope is "once" or "session".
// sessionID is the turn's session id (the value sent on THIS turn) and
// toolCallID echoes the id carried on the request. A 404 means the pending
// request already resolved (turn cancelled or timed out) — nothing left to
// answer, so it is treated as success. Best-effort with a short timeout.
func postPermissionDecision(proxyURL, sessionID, toolCallID, decision, scope string) error {
	if sessionID == "" || toolCallID == "" {
		return fmt.Errorf("empty session id or tool call id")
	}
	body, _ := json.Marshal(map[string]string{
		"session_id":   sessionID,
		"tool_call_id": toolCallID,
		"decision":     decision,
		"scope":        scope,
	})
	req, err := http.NewRequest("POST",
		strings.TrimRight(proxyURL, "/")+"/v1/permission", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	setProxyAuth(req)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	// 404 (already resolved) is not an error for the caller — the decision
	// simply has nowhere to land. Any other status is ignored too: the POST
	// is advisory, the turn proceeds on the proxy's own fail-safe.
	resp.Body.Close()
	return nil
}

// cancelTurn POSTs /cancel for a session_id. Best-effort: returns
// immediately on connection failure. The TCP-disconnect path in the
// chat client is the primary cancel mechanism; this is defense-in-depth
// for cases where a reverse proxy buffers the disconnect.
func cancelTurn(proxyURL, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("empty session id")
	}
	body, _ := json.Marshal(map[string]string{"session_id": sessionID})
	req, err := http.NewRequest("POST",
		strings.TrimRight(proxyURL, "/")+"/cancel", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	setProxyAuth(req)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// demoOpts bundles the per-request flags the /demo split-pane needs.
// Held in a struct rather than added as positional args to keep
// sendChatOpts readable as the demo grows.
// taskMode is what the CLIENT declares about a request: whether the person
// asked for the workspace to change, or asked a question about it. It mirrors
// the proxy's task_mode and exists so the decision is a control the user
// operates rather than something inferred from the words they typed.
//
// Nothing here reads the message. The same sentence goes either way depending
// only on which control the user used.
type taskMode string

const (
	taskModeWork     taskMode = "work"
	taskModeQuestion taskMode = "question"
)

// taskContract is the optional structured declaration sent with a request.
// Field tags MUST match proxy/types.go's TaskContract.
//
// ExpectedOutputs and Verification stay empty from the TUI: a person typing
// into a chat box has told the client nothing structured about which files
// must exist or which command must run, and guessing from their prose is the
// thing this replaces. A harness that genuinely knows fills them in.
type taskContract struct {
	TaskMode        taskMode `json:"task_mode"`
	ExpectedOutputs []string `json:"expected_outputs,omitempty"`
	Verification    []string `json:"verification,omitempty"`
}

type demoOpts struct {
	disableFreshSlot bool
	sandboxSubdir    string
	// allowedTools is the session allowlist sent as session_allowed_tools so
	// the proxy skips the permission prompt for pre-approved tools.
	allowedTools []string
	// taskMode is the client's declaration for this one request. Empty means
	// the ordinary case, which is work -- an owned sender always declares
	// something, because a request with no contract is indistinguishable from
	// a stranger's.
	taskMode taskMode
	// omitTaskContract sends no contract at all. Exactly one fixture sets it,
	// to prove an external or legacy caller is still accepted; ordinary owned
	// traffic must never reach that path by accident.
	omitTaskContract bool
}

// sendChatOpts opens an SSE POST to /v1/agent and forwards each parsed
// event to out. Returns nil on clean [DONE], err otherwise. Caller is
// responsible for closing the channel after this returns.
//
// history is the prior-turn user/assistant transcript so the agent can
// answer follow-ups. Pass nil for the first turn of a session.
//
// opts carries the /demo split-pane flags (pass demoOpts{} otherwise):
// the V3 side keeps PC-045 from wiping the pre-warmed prefix cache
// (disableFreshSlot), and writes files into per-side sandbox subdirs so
// the two panes never clobber each other (sandboxSubdir).
func sendChatOpts(ctx context.Context, proxyURL, message, workingDir, mode,
	sessionID string, history []historyMessage,
	opts demoOpts, out chan<- chatEvent) error {

	var contract *taskContract
	if !opts.omitTaskContract {
		// Declared, never derived: the only input is the caller's selection,
		// and an unselected owned caller is work.
		mode := opts.taskMode
		if mode == "" {
			mode = taskModeWork
		}
		contract = &taskContract{TaskMode: mode}
	}
	body, err := json.Marshal(agentRequest{
		Message:             message,
		WorkingDir:          workingDir,
		Mode:                mode,
		SessionID:           sessionID,
		History:             history,
		SessionAllowedTools: opts.allowedTools,
		DisableFreshSlot:    opts.disableFreshSlot,
		SandboxSubdir:       opts.sandboxSubdir,
		TaskContract:        contract,
	})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		strings.TrimRight(proxyURL, "/")+"/v1/agent", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	setProxyAuth(req)

	// No overall timeout — agent turns can run minutes for long
	// generations. Connection-level timeout only.
	client := &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		// The proxy answers a refused request with an error envelope; its
		// detail is the sentence meant for the user (for example: the lens
		// cannot score, run atlas doctor).
		var env struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(b, &env) == nil && env.Detail != "" {
			return fmt.Errorf("%s", env.Detail)
		}
		return fmt.Errorf("status %d: %s", resp.StatusCode,
			strings.TrimSpace(string(b)))
	}
	return parseChatSSE(ctx, resp.Body, out)
}

// parseChatSSE reads the chat-protocol SSE stream. Returns nil when it
// sees `data: [DONE]` (clean end-of-turn) or io.EOF.
func parseChatSSE(ctx context.Context, r io.Reader, out chan<- chatEvent) error {
	scanner := bufio.NewScanner(r)
	// tool_result frames carry diff blobs; bump the buffer for the
	// same reason streamEvents does below.
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			return nil
		}
		var ev chatEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue // skip malformed frame, don't kill the turn
		}
		if ev.Type == "" {
			continue
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return scanner.Err()
}

// loadBearerToken returns the token from the api-keys file, if one is
// configured. setProxyAuth sends it only when no service token is: a proxy
// with a service token accepts that token and nothing else.
//
// Search order: $ATLAS_API_KEYS_PATH, then ./secrets/api-keys.json
// relative to cwd. Returns "" if no token is found — caller must
// handle the empty case (don't set the header).
func loadBearerToken() string {
	path := os.Getenv("ATLAS_API_KEYS_PATH")
	if path == "" {
		wd, err := os.Getwd()
		if err == nil {
			cand := filepath.Join(wd, "secrets", "api-keys.json")
			if _, err := os.Stat(cand); err == nil {
				path = cand
			}
		}
	}
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// File shapes: {"api_key": "..."}, {"keys": {"tui": "..."}}, or the
	// `atlas init` shape {"sk-…": {"user": "local", …}} where each top-
	// level key IS a token mapping to a metadata object. Try in order.
	var single struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(data, &single); err == nil && single.APIKey != "" {
		return single.APIKey
	}
	var grouped struct {
		Keys map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(data, &grouped); err == nil {
		if v, ok := grouped.Keys["tui"]; ok && v != "" {
			return v
		}
	}
	// Token-keyed shape ({"sk-…": {"user": "local", …}}): accept only
	// entries whose key looks like a token or whose value carries key
	// metadata, so an unrelated object-of-objects (e.g. {"keys": {...}})
	// is not mistaken for a token map. Use the lexicographically first
	// qualifying token so repeated loads pick the same one.
	var keyed map[string]map[string]interface{}
	if err := json.Unmarshal(data, &keyed); err == nil && len(keyed) > 0 {
		tokens := make([]string, 0, len(keyed))
		for k, meta := range keyed {
			_, hasUser := meta["user"]
			_, hasCreatedBy := meta["created_by"]
			if strings.HasPrefix(k, "sk-") || hasUser || hasCreatedBy {
				tokens = append(tokens, k)
			}
		}
		if len(tokens) > 0 {
			sort.Strings(tokens)
			return tokens[0]
		}
	}
	return ""
}

// Internal service auth, client side. The `atlas` launcher
// (atlas/commands/tui.py) resolves the checkout's
// secrets/service-token and passes its path via
// ATLAS_SERVICE_TOKEN_FILE; a cwd-relative secrets/service-token is
// the fallback for direct binary runs from the checkout. No token =>
// no header (works against an auth-disabled proxy).

var serviceToken = loadServiceToken()

func loadServiceToken() string {
	path := os.Getenv("ATLAS_SERVICE_TOKEN_FILE")
	if path == "" {
		path = "secrets/service-token"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// setProxyAuth puts the credential the proxy checks on a request to it. The
// service token wins: when the installation has one, the proxy accepts only
// it, and sending the api-keys token instead got a 401 on every call.
func setProxyAuth(req *http.Request) {
	tok := serviceToken
	if tok == "" {
		tok = loadBearerToken()
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

type tokenTransport struct {
	base http.RoundTripper
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if serviceToken != "" && req.Header.Get("Authorization") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+serviceToken)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// installTokenTransport covers the clients that use the process default
// transport and set no header themselves. Requests that build their own
// transport (the chat stream, the raw demo lane, the events stream) set
// the header with setProxyAuth; the wrapper never ran for them.
func installTokenTransport() {
	if serviceToken == "" {
		return
	}
	http.DefaultTransport = &tokenTransport{base: http.DefaultTransport}
}

// PC-062: SSE event consumer for the TUI.
//
// Mirrors the Envelope struct in proxy/events.go and the Python
// dataclass in atlas/events.py. The schema contract is documented
// in docs/PROTOCOL.md — any change here MUST be made in lockstep
// across all three implementations.
//
// The TUI receives Envelope events on a channel and feeds them into
// the Bubbletea model via tea.Cmd / tea.Msg. The connection is
// resumable: if /events drops, we reconnect with exponential backoff.

// Envelope is the wire-format event. Field tags MUST match
// atlas/events.py and proxy/events.go exactly.
type Envelope struct {
	EventID    string                 `json:"event_id"`
	Timestamp  float64                `json:"timestamp"`
	Type       string                 `json:"type"`
	Stage      string                 `json:"stage"`
	Payload    map[string]interface{} `json:"payload"`
	DurationMS int64                  `json:"duration_ms,omitempty"`
}

// Legal types — mirror atlas.cli.events.EVENT_TYPES.
const (
	EvtStageStart = "stage_start"
	EvtStageEnd   = "stage_end"
	EvtToolCall   = "tool_call"
	EvtToolResult = "tool_result"
	EvtMetric     = "metric"
	EvtError      = "error"
	EvtDone       = "done"
)

// streamEvents connects to the /events SSE endpoint and pushes parsed
// envelopes onto out. Returns when ctx is cancelled or the connection
// dies unrecoverably. The TUI's reconnect loop wraps this.
func streamEvents(ctx context.Context, eventsURL string, out chan<- Envelope) error {
	req, err := http.NewRequestWithContext(ctx, "GET", eventsURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	setProxyAuth(req)

	client := &http.Client{
		// No timeout on the response body — SSE streams indefinitely.
		// Connect timeout is the only meaningful deadline.
		Transport: &http.Transport{
			ResponseHeaderTimeout: 5 * time.Second,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	return parseSSE(ctx, resp.Body, out)
}

// parseSSE reads SSE frames from r and pushes parsed envelopes onto out.
// Skips comment lines (`: connected`, `: heartbeat`) and anything that
// is not a `data:` line — see docs/PROTOCOL.md "SSE control frames".
func parseSSE(ctx context.Context, r io.Reader, out chan<- Envelope) error {
	scanner := bufio.NewScanner(r)
	// SSE frames can contain large JSON blobs. The default 64KB buffer
	// is too small for tool_result payloads with embedded file diffs.
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue // blank line = frame boundary; `:` = SSE comment
		}
		if !strings.HasPrefix(line, "data:") {
			continue // includes named `event:` lines — /events never emits them
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}

		var ev Envelope
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			// Malformed envelope — skip rather than killing the stream.
			// Could be the legacy `{stage, detail}` shape from v3-service
			// emitting in dual-mode without our opt-in header. We just
			// don't render those.
			continue
		}
		if ev.Type == "" || ev.EventID == "" {
			continue // legacy or unrecognized
		}

		select {
		case out <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return scanner.Err()
}

// streamEventsWithReconnect wraps streamEvents with exponential backoff
// reconnection. Useful when the proxy restarts mid-session — TUI keeps
// running rather than dying on the first dropped connection.
func streamEventsWithReconnect(ctx context.Context, eventsURL string, out chan<- Envelope) {
	const initialBackoff = 500 * time.Millisecond
	backoff := initialBackoff
	maxBackoff := 30 * time.Second
	for {
		connectedAt := time.Now()
		err := streamEvents(ctx, eventsURL, out)
		if ctx.Err() != nil {
			return // user quit / TUI shutdown
		}
		// A connection that survived a while was healthy — start the
		// backoff ladder over instead of carrying an escalated delay
		// from failures that happened long ago (e.g. proxy restarts
		// hours apart would otherwise each cost the max 30s).
		if time.Since(connectedAt) > time.Minute {
			backoff = initialBackoff
		}
		// Connection died; back off and retry.
		dlog("conn", "events_disconnected", map[string]interface{}{
			"err": fmt.Sprintf("%v", err), "backoff_ms": backoff.Milliseconds(),
		})
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}
