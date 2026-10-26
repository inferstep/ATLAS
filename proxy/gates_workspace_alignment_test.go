package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Both containers see a split as `/workspace`, so no value either holds can
// reveal it: the divergence is in the host bind. The probe writes a file on the
// proxy's side and asks the sandbox to read it, at the path the proxy wrote it
// to, which carries the session's subdirectory.
func TestWorkspaceAlignmentProbeIsFunctionalNotConfigural(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "e2e")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	// A sandbox that does not see the file is bound to another directory, and
	// the user is told how to align the two.
	otherDisk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"stdout":""}`))
	}))
	defer otherDisk.Close()
	ctx := NewAgentContext(sub, Tier2Medium)
	ctx.SandboxURL = otherDisk.URL
	resetWorkspaceAlignmentCache()
	problem := verifyWorkspaceAlignment(ctx)
	for _, want := range []string{"different directories", "atlas workspace align"} {
		if !strings.Contains(problem, want) {
			t.Errorf("a split: message missing %q: %q", want, problem)
		}
	}

	// A sandbox on the same disk: it runs the probe's
	// print(open("<path>").read()). Only the path the proxy wrote holds the
	// file; a probe that asks for any other path reads nothing.
	probePath := filepath.Join(sub, mountProbeName)
	sameDisk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Code string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		path, _ := strconv.Unquote(strings.TrimSuffix(strings.TrimPrefix(req.Code, "print(open("), ").read())"))
		stdout := ""
		if path == probePath {
			data, _ := os.ReadFile(probePath)
			stdout = string(data)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"stdout": stdout})
	}))
	defer sameDisk.Close()
	// The verdict is cached for the whole package. The unreachable row runs
	// last, so "no split" is what later tests find there.
	for _, c := range []struct {
		name, sandboxURL string
	}{
		{"a sandbox that reads what the proxy wrote", sameDisk.URL},
		{"an unreachable sandbox is no evidence of a split", "http://127.0.0.1:1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := NewAgentContext(sub, Tier2Medium)
			ctx.SandboxURL = c.sandboxURL
			resetWorkspaceAlignmentCache()
			if problem := verifyWorkspaceAlignment(ctx); problem != "" {
				t.Errorf("reported a split: %s", problem)
			}
		})
	}

	if _, err := os.Stat(filepath.Join(sub, mountProbeName)); !os.IsNotExist(err) {
		t.Error("probe file left in the user's workspace")
	}
}
