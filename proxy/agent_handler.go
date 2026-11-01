// The /v1/agent and /cancel endpoints: what a request carries and how its run is opened.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sync"
)

// activeSessions tracks in-flight /v1/agent turns by session_id so
// /cancel can abort them. Map value is a *sessionCancel wrapping the
// context.CancelFunc from the per-request context.WithCancel wrapper.
//
// The pointer doubles as a per-turn identity token: cleanup uses
// CompareAndDelete so a finishing turn only removes its own entry. If a
// second /v1/agent request reuses the same session_id, the first turn's
// deferred cleanup no longer deletes the second turn's cancel func.
//
// Defense-in-depth: cancellation also flows naturally through TCP
// disconnect (handleAgent already binds ctx to r.Context()), but a
// reverse proxy may buffer the disconnect. /cancel gives the TUI a
// reliable, explicit kill switch.
var activeSessions sync.Map

// sessionCancel is the activeSessions map value — a comparable wrapper
// around the per-turn cancel func.
type sessionCancel struct {
	cancel context.CancelFunc
}

// ---------------------------------------------------------------------------
// HTTP handler for /v1/agent endpoint
// ---------------------------------------------------------------------------

// handleAgent is the HTTP handler for the new agent endpoint.
func handleAgent(w http.ResponseWriter, r *http.Request) {
	req, ok := readAgentRequest(w, r)
	if !ok {
		return
	}

	workingDir, hostDir, ok := agentWorkingDirs(w, req)
	if !ok {
		return
	}

	// Classify tier from message
	tier := classifyAgentTier(req.Message)

	// Create agent context
	ctx := NewAgentContext(workingDir, tier)
	if msg := removedSwitchRefusal(req.RemovedBypassV3, req.RemovedV3Mode, req.RemovedFeasibilityMode); msg != "" {
		writeError(w, http.StatusBadRequest, ErrInvalidInput, msg)
		return
	}
	configureAgentContext(ctx, req, workingDir, hostDir)
	seedPriorHistory(ctx, req)

	// Carry the upstream cancellation through so disconnects abort the loop
	// and llama-server's in-flight generation.
	//
	// Also wrap in a cancellable context so POST /cancel can
	// abort even when the TCP disconnect is buffered upstream.
	reqCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Two lifetimes, deliberately separate.
	//
	// reqCtx is the RESPONSE lifetime: it lives until the client goes away or
	// the handler returns, and finalisation -- reaping, rehashing, restoring,
	// and the terminal event itself -- runs on it. workCtx is the WORK
	// lifetime: everything that costs time (LLM calls, tools, gates, V3, the
	// sandbox) hangs off it, and it ends one reserve before the session
	// budget does. Without the split, the deadline that stops the work also
	// kills the channel that would explain why it stopped.
	total, reserve := sessionBudget()
	workCtx, cancelWork := context.WithTimeout(reqCtx, total-reserve)
	defer cancelWork()
	ctx.RequestCtx = reqCtx
	ctx.Ctx = workCtx
	ctx.cancelWork = cancelWork
	ctx.PassID = req.SessionID
	if req.SessionID != "" {
		entry := &sessionCancel{cancel: cancel}
		activeSessions.Store(req.SessionID, entry)
		defer activeSessions.CompareAndDelete(req.SessionID, entry)
	}

	applyPermissionMode(ctx, req)

	// The client's declaration, checked against the workspace this request
	// resolved to. A bad contract is a bad request: it is refused outright
	// rather than dropped, because a client that declared obligations and had
	// them silently discarded would be told its run finished having proved
	// none of them.
	if !applyTaskContract(w, ctx, req, workingDir) {
		return
	}
	// The lens is required (lens_required.go): a request is not started
	// while it cannot score. After every 400, and before any work or any
	// streamed byte, so the answer is an ordinary HTTP error the client
	// shows as such.
	if ok, why := lensReady(lensURL); !ok {
		writeError(w, http.StatusServiceUnavailable, ErrDependencyDown,
			"ATLAS needs the geometric lens for every request, and "+why+
				". Run `atlas doctor`.")
		return
	}

	// Detect project (implemented in context.go)
	ctx.Project = detectProjectInfo(workingDir)

	flusher, ok := openEventStream(w, ctx)
	if !ok {
		return
	}
	streamAgentRun(w, flusher, ctx, req)
}

// historyMsg is one row of the transcript a client sends with a request.
type historyMsg struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

// agentRequest is the body of a /v1/agent request.
type agentRequest struct {
	Message    string       `json:"message"`
	WorkingDir string       `json:"working_dir"`
	Mode       string       `json:"mode"`       // "default", "accept-edits", "yolo"
	SessionID  string       `json:"session_id"` // optional — required for /cancel
	History    []historyMsg `json:"history,omitempty"`
	// Tools the client has approved for the whole session so the proxy
	// skips the interactive prompt for them (see /v1/permission).
	SessionAllowedTools []string `json:"session_allowed_tools,omitempty"`
	// /demo split-pane flags — tags match tui/chat.go's agentRequest.
	DisableFreshSlot bool   `json:"disable_fresh_slot,omitempty"` // keep the pre-warmed KV prefix
	SandboxSubdir    string `json:"sandbox_subdir,omitempty"`     // confine writes to this workspace subdir
	// Removed switches, read only to refuse them. V3 runs on every
	// request and feasibility is recorded, never enforced; a request that
	// asks for V3 off, planner-only or enforce would otherwise be measured
	// under a system it did not ask for.
	RemovedBypassV3        *bool  `json:"bypass_v3,omitempty"`
	RemovedV3Mode          string `json:"v3_mode,omitempty"`
	RemovedFeasibilityMode string `json:"feasibility_mode,omitempty"`
	// What the client declares about the request. Optional, and absent
	// stays distinguishable from present-and-empty.
	TaskContract *TaskContract `json:"task_contract,omitempty"`
}

// readAgentRequest decodes a /v1/agent request, or answers it with the HTTP
// error that refuses it: a method other than POST, a body that does not
// decode, or no message.
func readAgentRequest(w http.ResponseWriter, r *http.Request) (*agentRequest, bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, ErrUnsupported, "method not allowed")
		return nil, false
	}
	var req agentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidInput, "invalid request body")
		return nil, false
	}

	if req.Message == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidInput, "message is required")
		return nil, false
	}
	return &req, true
}

// agentWorkingDirs resolves where a request works: the workspace the proxy
// writes to and the client's own path for it, both inside the pane's subdir
// for a /demo pane. It answers a subdir that is not allowed with HTTP 400.
func agentWorkingDirs(w http.ResponseWriter, req *agentRequest) (string, string, bool) {
	// Path translation: the TUI sends its host cwd (e.g. /home/isaac/snake)
	// as working_dir, but the proxy runs in a container where that path
	// doesn't exist — only /workspace (the bind-mount target) does. The
	// startup wrapper (atlas/runtime.py:_align_workspace) already aligns
	// the bind mount to the user's cwd, so /workspace IS the user's cwd
	// from the proxy's perspective. Use ATLAS_WORKSPACE_DIR (set in
	// docker-compose.yml) as the canonical write target. The original
	// host path is kept on HostWorkingDir for path translation (below).
	hostDir := req.WorkingDir
	if hostDir == "" {
		hostDir = "."
	}
	workingDir := envOr("ATLAS_WORKSPACE_DIR", hostDir)

	// /demo: each pane works inside its own workspace subdir. A non-empty
	// sandbox_subdir must be one allowed bare name, else HTTP 400 and no turn.
	if sub := req.SandboxSubdir; sub != "" {
		if !validSandboxSubdir(sub) {
			writeError(w, http.StatusBadRequest, ErrInvalidInput, sandboxSubdirDetail)
			return "", "", false
		}
		workingDir = filepath.Join(workingDir, sub)
		if hostDir != "" && hostDir != "." {
			hostDir = filepath.Join(hostDir, sub)
		}
	}
	return workingDir, hostDir, true
}

// configureAgentContext gives a request's context what the run needs from
// the request and the proxy: the client's host path, the services' URLs,
// and where run_command executes.
func configureAgentContext(ctx *AgentContext, req *agentRequest, workingDir, hostDir string) {
	ctx.DisableFreshSlot = req.DisableFreshSlot
	// Stash the host path so resolveAgentPath can translate absolute
	// host paths the model receives in user prompts (e.g. "fix
	// /home/isaac/snake/app.py") into the container path. Without this
	// the model copies the user's host path verbatim into read_file
	// and the open() fails because that path doesn't exist inside the
	// proxy container — only /workspace does.
	if hostDir != "" && hostDir != "." {
		ctx.HostWorkingDir = filepath.Clean(hostDir)
	}
	ctx.InferenceURL = inferenceURL
	ctx.SandboxURL = sandboxURL
	ctx.LensURL = lensURL
	ctx.V3URL = envOr("ATLAS_V3_URL", "http://localhost:8070")

	// Host execution for run_command only when the operator chooses it;
	// the workspace's settings file may only choose the sandbox. See
	// resolveVerifyTarget. Default stays sandbox.
	ctx.VerifyOnHost = resolveVerifyTarget(workingDir) == "host"
	ctx.TrustMode = resolveTrustMode()
}

// seedPriorHistory gives the run the transcript the client sent: its last 40
// rows, each a user or an assistant turn with content.
func seedPriorHistory(ctx *AgentContext, req *agentRequest) {
	// Seed prior-turn transcript from the request body. The TUI ships
	// user/assistant text rows from its local chat history so the agent
	// can answer follow-ups; without it, every /v1/agent call starts
	// fresh. Cap defensively at 40 messages here too — the proxy's own
	// trim logic in runAgentLoop handles further overflow.
	if n := len(req.History); n > 0 {
		if n > 40 {
			req.History = req.History[n-40:]
		}
		ctx.PriorHistory = make([]AgentMessage, 0, len(req.History))
		for _, h := range req.History {
			// Only accept the two roles that make sense as conversation
			// history; anything else is skipped silently rather than
			// passed through to the LLM as an unknown role.
			if h.Role != "user" && h.Role != "assistant" {
				continue
			}
			if h.Content == "" {
				continue
			}
			ctx.PriorHistory = append(ctx.PriorHistory, AgentMessage{
				Role:    h.Role,
				Content: h.Content,
			})
		}
	}
}

// applyPermissionMode sets what the run may do without asking: the client's
// permission mode, and the tools the client approved for the whole session.
func applyPermissionMode(ctx *AgentContext, req *agentRequest) {
	// Set permission mode
	switch req.Mode {
	case "accept-edits":
		ctx.PermissionMode = PermissionAcceptEdits
	case "yolo":
		ctx.PermissionMode = PermissionYolo
		ctx.YoloMode = true
	default:
		ctx.PermissionMode = PermissionDefault
	}

	// Seed session-approved tools so pre-approved destructive tools skip the
	// interactive prompt (the client re-sends this list each turn).
	if len(req.SessionAllowedTools) > 0 {
		ctx.AllowedTools = make(map[string]bool, len(req.SessionAllowedTools))
		for _, t := range req.SessionAllowedTools {
			ctx.AllowedTools[t] = true
		}
	}
}

// applyTaskContract validates the client's task contract against the request's
// workspace and keeps it, with the tier it declares. It answers a contract that
// does not validate with HTTP 400 and reports false.
func applyTaskContract(w http.ResponseWriter, ctx *AgentContext, req *agentRequest, workingDir string) bool {
	validatedContract, contractErr := validateTaskContract(req.TaskContract, workingDir)
	if contractErr != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidInput, contractErr.Error())
		return false
	}
	ctx.TaskContract = validatedContract
	if t := declaredTier(validatedContract, ctx.Tier); t != ctx.Tier {
		ctx.Tier, ctx.MaxTurns = t, TierMaxTurns(t)
	}
	return true
}

// openEventStream starts the event stream the run reports on, or answers
// HTTP 500 when the connection cannot stream.
func openEventStream(w http.ResponseWriter, ctx *AgentContext) (http.Flusher, bool) {
	// Set up SSE streaming
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, ErrInternal, "streaming not supported")
		return nil, false
	}

	w.Header().Set(headerContentType, "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Flush headers immediately so the client sees the response as
	// "established" before the first LLM call returns. Without this
	// sentinel, net/http waits to flush headers until the first body
	// write, which is the first ctx.Stream() call — and that doesn't
	// happen until the agent loop emits its first event, which can
	// take 10-60s for the first LLM round-trip. Clients with a
	// reasonable ResponseHeaderTimeout (e.g. 30s) would time out
	// before getting any data.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// http.ResponseWriter is NOT goroutine-safe. StreamFn fires from at
	// least two concurrent goroutines during a single agent turn:
	//   - main agent loop (tool dispatch, LLM SSE forwarding)
	//   - pollPromptProgress (100ms ticker emitting llm_prompt_progress)
	// Concurrent Write+Flush calls interleave bytes and corrupt the
	// chunked-encoding framing; the client then errors with "chunked line ends
	// with bare LF" and drops, which the proxy sees as `context canceled`
	// mid-prompt-eval. Serialize the writes with a per-handler mutex so chunk
	// framing stays well-formed regardless of how fast or how concurrently
	// events fire.
	var streamMu sync.Mutex
	ctx.StreamFn = func(eventType string, data interface{}) {
		event := SSEEvent{Type: eventType, Data: data}
		eventJSON, _ := json.Marshal(event)
		streamMu.Lock()
		defer streamMu.Unlock()
		fmt.Fprintf(w, "data: %s\n\n", eventJSON)
		flusher.Flush()
	}
	return flusher, true
}

// streamAgentRun runs the request on its open event stream, approving every
// call in yolo mode, and ends the stream with [DONE].
func streamAgentRun(w http.ResponseWriter, flusher http.Flusher, ctx *AgentContext, req *agentRequest) {
	// For yolo mode, auto-approve all permissions
	if ctx.YoloMode {
		ctx.PermissionFn = func(string, json.RawMessage) bool { return true }
	}

	// Run agent loop
	if err := runAgentLoop(ctx, req.Message); err != nil {
		// %q quotes the error string so user-influenced fragments
		// embedded in err.Error() can't fake additional log entries.
		log.Printf("[agent] error: %q", err.Error())
	}

	// Send final done event
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ---------------------------------------------------------------------------
// /cancel — abort an in-flight /v1/agent turn by session_id
// ---------------------------------------------------------------------------

// handleCancel POSTs cancel an in-flight agent turn. Body:
//
//	{"session_id": "..."}
//
// Returns 200 with `{"cancelled": true}` if the session was found and
// cancelled, 404 with `{"cancelled": false}` if no such session is
// active. Idempotent: a second cancel for the same session returns 404.
//
// On success, the agent loop exits via context.Canceled, the SSE
// stream emits its trailing `[DONE]`, and the client connection
// closes cleanly. The TUI surfaces a "turn cancelled" system message.
func handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, ErrUnsupported, "method not allowed")
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidInput, "invalid request body")
		return
	}
	if req.SessionID == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidInput, "session_id required")
		return
	}
	v, ok := activeSessions.LoadAndDelete(req.SessionID)
	w.Header().Set(headerContentType, "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]bool{"cancelled": false})
		return
	}
	entry, ok := v.(*sessionCancel)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad session entry"})
		return
	}
	entry.cancel()
	log.Printf("[agent] cancelled session %q via /cancel", req.SessionID)
	_ = json.NewEncoder(w).Encode(map[string]bool{"cancelled": true})
}
