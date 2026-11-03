package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/itigges22/atlas-proxy/internal/projectcontext"
	"github.com/itigges22/atlas-proxy/internal/wire"
)

// A revised plan is shown the files the run has read, under the paths the agent
// uses for them: relative to the working folder.
func TestARevisedPlanIsShownTheFilesReadUnderTheirRelativePaths(t *testing.T) {
	var sent wire.V3PlanRequest
	v3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Error(err)
		}
		http.Error(w, "the request is all this test needs", http.StatusInternalServerError)
	}))
	defer v3.Close()
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.V3URL = v3.URL
	ctx.Plan = &wire.Plan{Steps: []wire.PlanStep{{ID: "s1", Action: "read_file", Target: "app.py"}}}
	ctx.RecordFileRead(filepath.Join(dir, "app.py"), "x = 1\n")

	revisePlan(ctx, "add a health endpoint", "off-plan thrash")

	if got := sent.ProjectContext["app.py"]; got != "x = 1\n" {
		t.Errorf("the planner was sent %q under app.py (keys %v), want the file read", got, slices.Sorted(maps.Keys(sent.ProjectContext)))
	}
}

// Each request carries a context file cut to its own limit, so a session that
// read a large file does not send all of it with every request.
func TestEachRequestCutsAContextFileToItsLimit(t *testing.T) {
	big := strings.Repeat("x", 5000)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "written.py"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	var sent wire.V3GenerateRequest
	v3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/generate" {
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Error(err)
			}
		}
		http.Error(w, "the request is all this test needs", http.StatusInternalServerError)
	}))
	defer v3.Close()
	sandbox := fakeSyntaxSandbox(t, "")
	defer sandbox.Close()
	ctx := writeGateCtx(t, v3.URL, sandbox.URL, dir)
	ctx.RecordFileRead(filepath.Join(dir, "read.py"), big)
	ctx.SessionWrites["written.py"] = true
	target := filepath.Join(dir, "target.py")

	if _, err := writeFileWithV3(target, "x = 1\n", ctx); err != nil {
		t.Fatal(err)
	}
	planContext := map[string]string{}
	projectcontext.AddFilesRead(planContext, ctx.SnapshotFilesRead(), dir, 8)

	mark := "\n... (truncated)"
	for _, c := range []struct{ name, got, want string }{
		{"a write, a file read", sent.ProjectContext["read.py"], big[:4000] + mark},
		{"a write, a file written", sent.ProjectContext["written.py"], big[:4000] + mark},
		{"an edit", v3Request(target, "", ctx).ProjectContext["read.py"], big[:4000] + mark},
		{"the structural check", structuralProjectContext(ctx, target)["read.py"], big[:4000] + mark},
		{"a revised plan", planContext["read.py"], big[:2000] + mark},
		{"a file at the limit stays whole", projectcontext.Entry(big[:2000], 2000), big[:2000]},
	} {
		if c.got != c.want {
			t.Errorf("%s: %d bytes, want %d", c.name, len(c.got), len(c.want))
		}
	}
}
