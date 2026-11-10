// Background commands: run_background, tail_background and stop_background, with their sandbox /jobs client.

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/itigges22/atlas-proxy/internal/wire"
)

// ---------------------------------------------------------------------------
// Background commands
// ---------------------------------------------------------------------------
//
// Three tools wrap the sandbox /jobs/* endpoints. The pattern the model
// learns is: run_background(server) → tail_background or curl via
// run_command → stop_background. Without these, foreground servers
// (flask, npm start, cargo run) can't be verified — they don't exit
// and the model invents `timeout 5 ... || true` workarounds that tear
// the server down before any probe can hit it.
//
// All three tools require ATLAS_VERIFY_IN=sandbox (the default). Host
// mode bypasses the sandbox entirely; running long-lived processes on
// the host without any reaping is a foot-gun we don't want to ship,
// so we surface a clear error instead.

func runBackgroundTool() *ToolDef {
	return &ToolDef{
		Name:        "run_background",
		Effect:      wire.ToolEffectCommandUnobserved,
		Description: "Start a long-running command (server, watcher, etc.) in the background and return a job_id. Use for `python app.py`, `npm start`, `cargo run`, `flask run` — anything that doesn't exit. Returns initial stdout/stderr captured during a brief settle window so you can confirm startup. Pair with run_command/curl to probe the running service, then stop_background to clean up.",
		InputSchema: wire.RunBackgroundInput{},
		ReadOnly:    false,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*wire.ToolResult, error) {
			var input wire.RunBackgroundInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}

			if refusal := backgroundRefusal(ctx, input); refusal != nil {
				return refusal, nil
			}
			cwd := ctx.WorkingDir
			if input.Cwd != "" {
				cwd = resolveAgentPath(ctx, input.Cwd)
			}
			jobID, pid, err := sandboxStartBackground(ctx, input.Command, cwd)
			if err != nil {
				return &wire.ToolResult{Success: false, Error: fmt.Sprintf("sandbox start failed: %v", err)}, nil
			}
			// Settle window — give the process time to bind a port, fail
			// to import, etc., before we hand back to the model.
			time.Sleep(settleWindow(input.SettleMs))
			tail, _ := sandboxTailBackground(ctx, jobID, 50)
			out := wire.RunBackgroundOutput{
				JobID:   jobID,
				PID:     pid,
				Stdout:  tail.Stdout,
				Stderr:  tail.Stderr,
				Running: tail.Running,
			}
			if !tail.Running {
				out.ExitCode = tail.ExitCode
			}
			trackBackgroundJob(ctx, jobID, input.Command, tail.Running)
			outBytes, _ := json.Marshal(out)
			// A job that has already exited non-zero did not start. Reporting
			// that as a success would lose the failure, and the run would go
			// on to probe a port nothing is listening on. run_command reports
			// a failing command as a failure; so does this.
			if !tail.Running && tail.ExitCode != nil && *tail.ExitCode != 0 {
				return &wire.ToolResult{Success: false, Data: outBytes,
					Error: immediateExitMessage(ctx, input.Command, out)}, nil
			}
			return &wire.ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

// backgroundRefusal is the answer to a run_background call that must not start,
// or nil.
func backgroundRefusal(ctx *AgentContext, input wire.RunBackgroundInput) *wire.ToolResult {
	check := backgroundCheckInput{ctx: ctx, input: input}
	runCheckPhase(proxyChecks.background, &check)
	return check.result
}

// settleWindow is the wait after a start: 1.5 s unless the call asks for
// another, at most 10 s.
func settleWindow(settleMs *int) time.Duration {
	ms := 1500
	if settleMs != nil {
		ms = min(max(*settleMs, 0), 10000)
	}
	return time.Duration(ms) * time.Millisecond
}

// trackBackgroundJob remembers a job that is still running.
func trackBackgroundJob(ctx *AgentContext, jobID, command string, running bool) {
	if ctx.BackgroundJobs == nil {
		ctx.BackgroundJobs = make(map[string]string)
	}
	if running {
		ctx.BackgroundJobs[jobID] = command
		if ctx.BackgroundJobStarted == nil {
			ctx.BackgroundJobStarted = map[string]time.Time{}
		}
		ctx.BackgroundJobStarted[jobID] = time.Now()
	}
}

func tailBackgroundTool() *ToolDef {
	return &ToolDef{
		Name:        "tail_background",
		Effect:      wire.ToolEffectReadOnly,
		Description: "Read the recent stdout/stderr of a background job started via run_background. Returns the last N lines of each stream (default 50), the run state (running/exited), and the exit code if applicable. Use to check whether a server is still up, watch test runner output, or read the failure traceback after a crash.",
		InputSchema: wire.TailBackgroundInput{},
		ReadOnly:    true,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*wire.ToolResult, error) {
			var input wire.TailBackgroundInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			check := tailInputCheckInput{input: input}
			if runCheckPhase(proxyChecks.tailInput, &check).outcome != turnContinues {
				return check.result, nil
			}

			lines := 50
			if input.Lines != nil {
				lines = *input.Lines
				if lines < 1 {
					lines = 1
				} else if lines > 500 {
					lines = 500
				}
			}
			out, err := sandboxTailBackground(ctx, input.JobID, lines)
			if err != nil {
				return &wire.ToolResult{Success: false, Error: err.Error()}, nil
			}
			outBytes, _ := json.Marshal(out)
			return &wire.ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

func stopBackgroundTool() *ToolDef {
	return &ToolDef{
		Name:        "stop_background",
		Effect:      wire.ToolEffectCommandUnobserved,
		Description: "Stop a background job started via run_background. Sends SIGTERM, waits briefly, then SIGKILL if needed. Returns the final stdout/stderr buffer. Always call this when you're done with a background job — leaving them running blocks future job slots.",
		InputSchema: wire.StopBackgroundInput{},
		ReadOnly:    false,
		Destructive: true,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*wire.ToolResult, error) {
			var input wire.StopBackgroundInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			check := stopInputCheckInput{input: input}
			if runCheckPhase(proxyChecks.stopInput, &check).outcome != turnContinues {
				return check.result, nil
			}

			out, err := sandboxStopBackground(ctx, input.JobID)
			if err != nil {
				return &wire.ToolResult{Success: false, Error: err.Error()}, nil
			}
			delete(ctx.BackgroundJobs, input.JobID)
			outBytes, _ := json.Marshal(out)
			return &wire.ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

// sandboxStartBackground POSTs to /jobs/start. Returns (job_id, pid, err).
func sandboxStartBackground(ctx *AgentContext, command, cwd string) (string, int, error) {
	if ctx.SandboxURL == "" {
		return "", 0, fmt.Errorf("ATLAS_SANDBOX_URL not configured")
	}
	body, _ := json.Marshal(map[string]interface{}{"command": command, "cwd": cwd})
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	req, err := http.NewRequestWithContext(reqCtx, "POST", ctx.SandboxURL+"/jobs/start", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set(headerContentType, contentTypeJSON)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var d struct {
			Detail string `json:"detail"`
		}
		_ = json.NewDecoder(serviceAnswer(resp.Body)).Decode(&d)
		if d.Detail != "" {
			return "", 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, d.Detail)
		}
		return "", 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		JobID string `json:"job_id"`
		PID   int    `json:"pid"`
	}
	if err := json.NewDecoder(serviceAnswer(resp.Body)).Decode(&out); err != nil {
		return "", 0, err
	}
	return out.JobID, out.PID, nil
}

func sandboxTailBackground(ctx *AgentContext, jobID string, lines int) (wire.TailBackgroundOutput, error) {
	if ctx.SandboxURL == "" {
		return wire.TailBackgroundOutput{}, fmt.Errorf("ATLAS_SANDBOX_URL not configured")
	}
	url := fmt.Sprintf("%s/jobs/%s/output?lines=%d", ctx.SandboxURL, jobID, lines)
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	req, err := http.NewRequestWithContext(reqCtx, "GET", url, nil)
	if err != nil {
		return wire.TailBackgroundOutput{}, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return wire.TailBackgroundOutput{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return wire.TailBackgroundOutput{}, fmt.Errorf("unknown job_id %q (already cleaned up?)", jobID)
	}
	if resp.StatusCode != 200 {
		return wire.TailBackgroundOutput{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out wire.TailBackgroundOutput
	if err := json.NewDecoder(serviceAnswer(resp.Body)).Decode(&out); err != nil {
		return wire.TailBackgroundOutput{}, err
	}
	return out, nil
}

func sandboxStopBackground(ctx *AgentContext, jobID string) (wire.StopBackgroundOutput, error) {
	if ctx.SandboxURL == "" {
		return wire.StopBackgroundOutput{}, fmt.Errorf("ATLAS_SANDBOX_URL not configured")
	}
	url := fmt.Sprintf("%s/jobs/%s/stop", ctx.SandboxURL, jobID)
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	req, err := http.NewRequestWithContext(reqCtx, "POST", url, nil)
	if err != nil {
		return wire.StopBackgroundOutput{}, err
	}
	req.Header.Set(headerContentType, contentTypeJSON)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return wire.StopBackgroundOutput{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return wire.StopBackgroundOutput{}, fmt.Errorf("unknown job_id %q", jobID)
	}
	if resp.StatusCode != 200 {
		return wire.StopBackgroundOutput{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out wire.StopBackgroundOutput
	if err := json.NewDecoder(serviceAnswer(resp.Body)).Decode(&out); err != nil {
		return wire.StopBackgroundOutput{}, err
	}
	return out, nil
}

// sandboxJobState is one entry of the sandbox's /jobs listing. Only the
// fields this side reads are declared.
type sandboxJobState struct {
	JobID   string `json:"job_id"`
	Command string `json:"command"`
	Running bool   `json:"running"`
}

// sandboxListBackground reports every job the sandbox is holding.
//
// /jobs carries `running` per job, so completion is observable without
// reading a job's output — which matters because reading it is what
// tail_background does, and consuming output the model has not asked for
// would change what it sees next.
func sandboxListBackground(ctx *AgentContext) ([]sandboxJobState, error) {
	if ctx.SandboxURL == "" {
		return nil, fmt.Errorf("ATLAS_SANDBOX_URL not configured")
	}
	reqCtx := ctx.Ctx
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	req, err := http.NewRequestWithContext(reqCtx, "GET", ctx.SandboxURL+"/jobs", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Jobs []sandboxJobState `json:"jobs"`
	}
	if err := json.NewDecoder(serviceAnswer(resp.Body)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Jobs, nil
}

// finishedBackgroundNote reports jobs this run started that have since
// exited, once each, and stops tracking them.
//
// A background job's outcome is invisible unless the model calls
// tail_background, so a server that died on startup looks identical to one
// serving happily: the run continues, the next probe fails for a reason
// nothing explains, and a session can finish claiming work it never
// verified. The foreground-server redirect pushes more work down this path,
// which makes the silence cost more.
//
// Only jobs this run started are reported — the sandbox registry is
// process-wide and outlives sessions, so another session's leftovers are
// not this run's news. Errors return "": a job listing that cannot be
// fetched must not interrupt the loop.
func finishedBackgroundNote(ctx *AgentContext) string {
	if ctx == nil || len(ctx.BackgroundJobs) == 0 {
		return ""
	}
	jobs, err := sandboxListBackground(ctx)
	if err != nil {
		return ""
	}
	state := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		state[j.JobID] = j.Running
	}
	var done []string
	for id := range ctx.BackgroundJobs {
		running, known := state[id]
		if known && !running {
			done = append(done, id)
		}
	}
	if len(done) == 0 {
		return ""
	}
	sort.Strings(done)
	var sb strings.Builder
	for _, id := range done {
		cmd := ctx.BackgroundJobs[id]
		// Reported once: drop it from tracking so the next turn does not
		// repeat the same news.
		delete(ctx.BackgroundJobs, id)
		fmt.Fprintf(&sb, "\nBackground job %s has exited — `%s` is no longer running. "+
			"Read its output with tail_background(%q) before relying on it; a server "+
			"that exited on startup and one that served correctly look the same from here.",
			id, truncateStr(cmd, 80), id)
	}
	return strings.TrimLeft(sb.String(), "\n")
}

type backgroundCheckInput struct {
	toolCheckOutput
	ctx   *AgentContext
	input wire.RunBackgroundInput
}

func (check *backgroundCheckInput) trust() checkResult {
	ctx := check.ctx
	// Trust gate: same contract as run_command — untrusted mode
	// refuses all command execution, foreground or background.
	if !ctx.TrustMode.commandsAllowed() {
		return check.accept(checkGuardRefusal, &wire.ToolResult{Success: false, Error: untrustedRefusal})
	}
	return checkResult{checkPass, turnContinues}
}

func (check *backgroundCheckInput) command() checkResult {
	input := check.input
	if strings.TrimSpace(input.Command) == "" {
		return check.accept(checkModelError, &wire.ToolResult{Success: false, Error: "run_background: command cannot be empty"})
	}
	return checkResult{checkPass, turnContinues}
}

func (check *backgroundCheckInput) shell() checkResult {
	input := check.input
	if reason := validateShellCommand(input.Command); reason != "" {
		return check.accept(checkGuardRefusal, &wire.ToolResult{Success: false, Error: reason})
	}
	return checkResult{checkPass, turnContinues}
}

func (check *backgroundCheckInput) host() checkResult {
	ctx := check.ctx
	if ctx.VerifyOnHost {
		return check.accept(checkGuardRefusal, &wire.ToolResult{
			Success: false,
			Error:   "run_background is only available in sandbox mode (ATLAS_VERIFY_IN=sandbox). On the host, use `run_command` with `nohup ... &` and track the PID yourself.",
		})
	}
	return checkResult{checkPass, turnContinues}
}

type tailInputCheckInput struct {
	toolCheckOutput
	input wire.TailBackgroundInput
}

func (check *tailInputCheckInput) required() checkResult {
	input := check.input

	if strings.TrimSpace(input.JobID) == "" {
		return check.accept(checkModelError, &wire.ToolResult{Success: false, Error: "tail_background: job_id required"})
	}
	return checkResult{checkPass, turnContinues}
}

type stopInputCheckInput struct {
	toolCheckOutput
	input wire.StopBackgroundInput
}

func (check *stopInputCheckInput) required() checkResult {
	input := check.input

	if strings.TrimSpace(input.JobID) == "" {
		return check.accept(checkModelError, &wire.ToolResult{Success: false, Error: "stop_background: job_id required"})
	}
	return checkResult{checkPass, turnContinues}
}
