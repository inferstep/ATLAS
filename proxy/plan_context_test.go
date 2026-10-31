package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// More files were read than fit into the context of a revised plan. The
// planner sees the first of them in sorted path order, every time, not
// whatever the map's order gives.
func TestTheFilesReadThatFillAPlansContextAreTheSameEachTime(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	for n := 11; n >= 0; n-- {
		ctx.RecordFileRead(filepath.Join(dir, fmt.Sprintf("f%02d.py", n)), "x = 1\n")
	}
	want := []string{"f00.py", "f01.py", "f02.py", "f03.py", "f04.py", "f05.py", "f06.py", "f07.py"}
	for try := 1; try <= 100; try++ {
		pctx := map[string]string{}
		addFilesReadToPlanContext(ctx, pctx, 8)
		if got := slices.Sorted(maps.Keys(pctx)); !reflect.DeepEqual(got, want) {
			t.Fatalf("try %d: %v, want %v", try, got, want)
		}
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
	var sent V3GenerateRequest
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
	addFilesReadToPlanContext(ctx, planContext, 8)

	mark := "\n... (truncated)"
	for _, c := range []struct{ name, got, want string }{
		{"a write, a file read", sent.ProjectContext["read.py"], big[:4000] + mark},
		{"a write, a file written", sent.ProjectContext["written.py"], big[:4000] + mark},
		{"an edit", v3EditRequest(target, "", ctx).ProjectContext["read.py"], big[:4000] + mark},
		{"the structural check", structuralProjectContext(ctx, target)["read.py"], big[:4000] + mark},
		{"a revised plan", planContext["read.py"], big[:2000] + mark},
		{"a file at the limit stays whole", contextEntry(big[:2000], 2000), big[:2000]},
	} {
		if c.got != c.want {
			t.Errorf("%s: %d bytes, want %d", c.name, len(c.got), len(c.want))
		}
	}
}
