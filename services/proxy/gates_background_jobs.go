// Background jobs: what the model is told about the sandbox jobs a session
// started — the jobs still running when a turn ends, the exit gate's
// instruction to stop them, and the job that holds a port a command needs.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// liveBackgroundJobNote reports background jobs still running as the turn
// ends, for appending to the done summary.
//
// Jobs deliberately outlive the agent loop: a loop is one user message, so
// killing them here would break "start the dev server" followed by "now curl
// it". What is wrong is that they outlive it SILENTLY — the sandbox has no
// session concept and only reaps after two hours, so the next turn's
// `python app.py` fails on a bound port with no indication of why, and the
// user is never told anything is still running. Naming them keeps the
// behaviour and removes the surprise.
func liveBackgroundJobNote(ctx *AgentContext) string {
	if ctx == nil || len(ctx.BackgroundJobs) == 0 {
		return ""
	}
	ids := make([]string, 0, len(ctx.BackgroundJobs))
	for id := range ctx.BackgroundJobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var sb strings.Builder
	sb.WriteString("\n\nStill running in the sandbox:")
	for _, id := range ids {
		fmt.Fprintf(&sb, "\n  %s — %s", id, truncateStr(ctx.BackgroundJobs[id], 80))
	}
	sb.WriteString("\nThese keep their ports until stopped. Use stop_background to end them.")
	return sb.String()
}

// backgroundStopMessage is the exit gate's instruction when the run's own
// background jobs are still live: the jobs by id, and the one call that
// resolves each.
func backgroundStopMessage(ctx *AgentContext, live []string) string {
	var sb strings.Builder
	for _, id := range live {
		fmt.Fprintf(&sb, "Background job %s (%s) is still running — stop it with "+
			"stop_background {\"job_id\": %q} before finishing.\n",
			id, truncateStr(ctx.BackgroundJobs[id], 80), id)
	}
	sb.WriteString("The run cannot be confirmed finished while a process of its own may still " +
		"be writing to the workspace. If you started it only to verify your work, stop it and " +
		"then finish. If the user asked for it to stay running, say so in your summary and " +
		"finish; the run will then be reported as unfinished with the job named.")
	return sb.String()
}

// ownBackgroundJobHint names the model's own background job when a command
// just failed because that job is holding the resource.
//
// The sandbox has no session concept — a job lives until stop_background or
// the two-hour reaper — so a server the model started to verify its own work
// keeps the port, and its next `python app.py` fails against "another
// program" it has no way to identify. An observed session spent its remaining
// turns on that conflict. Returns "" when nothing is running or the failure is
// unrelated, so the common case is unchanged.
func ownBackgroundJobHint(ctx *AgentContext, errMsg string) string {
	if ctx == nil {
		return ""
	}
	if !strings.Contains(strings.ToLower(errMsg), "address already in use") &&
		!strings.Contains(strings.ToLower(errMsg), "port is already allocated") {
		return ""
	}
	if len(ctx.BackgroundJobs) == 0 {
		// Nothing this session started explains it, and the registry is
		// process-wide: a server an EARLIER session left running holds the
		// port under an id this session never saw. Ask the sandbox.
		return foreignBackgroundJobHint(ctx)
	}
	ids := make([]string, 0, len(ctx.BackgroundJobs))
	for id := range ctx.BackgroundJobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var sb strings.Builder
	sb.WriteString("\n\nThat port is held by a background job YOU started in this session")
	for _, id := range ids {
		fmt.Fprintf(&sb, "\n  job %s: %s", id, truncateStr(ctx.BackgroundJobs[id], 80))
	}
	sb.WriteString("\nStop it with stop_background before re-running, or probe the " +
		"already-running service instead of starting a second copy.")
	return sb.String()
}

// foreignBackgroundJobHint names the sandbox jobs THIS session did not start,
// so the model can stop the one holding the port.
//
// "Either identify and stop that program" is the sandbox's own advice on a
// bind failure, and until now it was unfollowable: /jobs/{id} needs an id, and
// a job from a previous session was never announced to this one. GET /jobs
// lists the registry whole. Fail-soft — an unreachable sandbox or an empty
// list adds nothing, leaving the bare bind error the model already had.
func foreignBackgroundJobHint(ctx *AgentContext) string {
	if ctx.SandboxURL == "" {
		return ""
	}
	base := ctx.Ctx
	if base == nil {
		base = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(base, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", ctx.SandboxURL+"/jobs", nil)
	if err != nil {
		return ""
	}
	if serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+serviceToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var out struct {
		Jobs []struct {
			JobID   string `json:"job_id"`
			Command string `json:"command"`
			Running bool   `json:"running"`
		} `json:"jobs"`
	}
	if json.NewDecoder(serviceAnswer(resp.Body)).Decode(&out) != nil {
		return ""
	}
	var sb strings.Builder
	for _, j := range out.Jobs {
		if !j.Running {
			continue
		}
		fmt.Fprintf(&sb, "\n  job %s: %s", j.JobID, truncateStr(j.Command, 80))
	}
	if sb.Len() == 0 {
		return ""
	}
	return "\n\nThat port is held by a background job left running in this sandbox by an " +
		"earlier session:" + sb.String() +
		"\nStop it with stop_background and re-run, or probe the already-running service " +
		"instead of starting a second copy."
}

// anyBackgroundJobID returns one job id this session has running, so the
// verification gate can point at the server the model already started
// instead of telling it to start another.
func anyBackgroundJobID(ctx *AgentContext) string {
	if ctx == nil || len(ctx.BackgroundJobs) == 0 {
		return ""
	}
	ids := make([]string, 0, len(ctx.BackgroundJobs))
	for id := range ctx.BackgroundJobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids[0]
}
