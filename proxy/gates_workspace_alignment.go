// Workspace alignment: whether the proxy and the sandbox see the same
// directory at /workspace, found by writing a file on one side and reading
// it on the other. The answer is cached for a few minutes.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// workspaceAlignment caches the last functional check of whether the proxy
// and the sandbox see the same filesystem at /workspace.
var (
	wsAlignMu      sync.Mutex
	wsAlignChecked time.Time
	wsAlignProblem string
)

const wsAlignTTL = 5 * time.Minute

// verifyWorkspaceAlignment returns "" when the proxy and the sandbox bind the
// same host directory, or a user-facing explanation when they do not.
//
// Both containers see the split as `/workspace`, so no configuration value
// either of them holds can reveal it — the divergence is in the host bind, and
// the only way to detect it from inside is to write a file on one side and
// read it from the other.
//
// It matters because nothing else notices. Every /health passes, the proxy
// writes files that the sandbox cannot see, `run_command` reports them
// missing, and the agent concludes its own work does not exist and gives up.
// `atlas doctor` has flagged this since 2026-07-18 and it recurred on
// 2026-08-03 after a power cut recreated one container from .env — which is
// exactly the case a health check has to cover, because nobody runs doctor
// after an unplanned reboot.
func verifyWorkspaceAlignment(ctx *AgentContext) string {
	if ctx == nil || ctx.SandboxURL == "" || ctx.WorkingDir == "" {
		return ""
	}
	wsAlignMu.Lock()
	if time.Since(wsAlignChecked) < wsAlignTTL {
		problem := wsAlignProblem
		wsAlignMu.Unlock()
		return problem
	}
	wsAlignMu.Unlock()

	token := fmt.Sprintf("atlas-mount-probe-%d", time.Now().UnixNano())
	removeProbe, err := writeMountProbe(ctx.WorkingDir, token)
	if err != nil {
		return "" // can't probe; not evidence of a split
	}
	defer removeProbe()

	problem := ""
	if got, ok := sandboxReadProbe(ctx); !ok {
		// Sandbox unreachable or the check could not run — fail soft.
		problem = ""
	} else if !strings.Contains(got, token) {
		problem = "The file tools and the shell are looking at different directories: " +
			"this proxy writes to one host directory and the sandbox that runs your " +
			"commands is bound to another, so files written here are invisible to " +
			"`run_command` and it will report them missing. Run `atlas workspace align` " +
			"to point both at the same directory, then retry."
	}
	wsAlignMu.Lock()
	wsAlignChecked, wsAlignProblem = time.Now(), problem
	wsAlignMu.Unlock()
	return problem
}

// sandboxReadProbe asks the sandbox to read the probe file from ITS
// /workspace. (contents, true) when the call completed, ("", false) when the
// check itself could not run.
func sandboxReadProbe(ctx *AgentContext) (string, bool) {
	// Read the probe at the path the PROXY wrote it to. Both containers mount
	// the same host directory at /workspace, so the container path is
	// identical on both sides — but only if the subdirectory is carried
	// across. Hardcoding /workspace made every session with a sandbox_subdir
	// look split, which refused 28 of 28 benchmark sessions before they ran.
	probe := filepath.Join(ctx.WorkingDir, mountProbeName)
	body, err := json.Marshal(map[string]interface{}{
		"code":     fmt.Sprintf("print(open(%q).read())", probe),
		"language": "python",
		"timeout":  10,
	})
	if err != nil {
		return "", false
	}
	base := ctx.Ctx
	if base == nil {
		base = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(base, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST",
		ctx.SandboxURL+"/execute", bytes.NewReader(body))
	if err != nil {
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")
	if serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+serviceToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var out struct {
		Stdout string `json:"stdout"`
	}
	if json.NewDecoder(serviceAnswer(resp.Body)).Decode(&out) != nil {
		return "", false
	}
	return out.Stdout, true
}

// resetWorkspaceAlignmentCache clears the cached verdict. Tests only — the
// TTL is what keeps this to one probe per session in normal use.
func resetWorkspaceAlignmentCache() {
	wsAlignMu.Lock()
	defer wsAlignMu.Unlock()
	wsAlignChecked, wsAlignProblem = time.Time{}, ""
}
