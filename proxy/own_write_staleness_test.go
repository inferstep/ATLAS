package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An edit to a file this session wrote must not be refused as "modified since
// last read" when nothing but the session touched it -- and must still be
// refused when something else did.
//
// These drive the real tools through executeToolCall. The earlier fix for this
// scenario (editViewIsCurrent) was tested only as a helper; the helper passed
// while edit_file kept refusing at the mtime check immediately after it.
//
// Measured: 9 refusals in 5 saved sessions, all on a path whose last toucher
// was the session's own write_file -- including the multifile_cli-rep2
// deadlock and aoc_sonar-rep1, where a one-line repair of a traceback the
// model had just read was refused.

func ownWriteCtx(t *testing.T) (*AgentContext, string) {
	t.Helper()
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.PermissionMode = PermissionYolo
	return ctx, dir
}

func callTool(t *testing.T, ctx *AgentContext, name string, args map[string]interface{}) *ToolResult {
	t.Helper()
	b, _ := json.Marshal(args)
	res := executeToolCall(name, b, ctx)
	if res == nil {
		t.Fatalf("%s returned nil", name)
	}
	return res
}

func toolText(res *ToolResult) string {
	return res.Error + " " + string(res.Data)
}

func TestAnEditRightAfterTheSessionsOwnWriteIsNotRefusedAsStale(t *testing.T) {
	ctx, dir := ownWriteCtx(t)

	w := callTool(t, ctx, "write_file", map[string]interface{}{
		"path": "store.py", "content": "def load():\n    return []\n"})
	if !w.Success {
		t.Fatalf("setup write failed: %s", toolText(w))
	}

	// No read_file in between. The model authored every byte on disk.
	e := callTool(t, ctx, "edit_file", map[string]interface{}{
		"path": "store.py", "old_str": "return []", "new_str": "return ['x']"})
	if strings.Contains(toolText(e), "modified since last read") {
		t.Fatalf("the session's own write was reported as an outside modification: %s", toolText(e))
	}
	if !e.Success {
		t.Fatalf("edit after own write failed for another reason: %s", toolText(e))
	}
	got, _ := os.ReadFile(filepath.Join(dir, "store.py"))
	if !strings.Contains(string(got), "return ['x']") {
		t.Errorf("edit reported success but disk has %q", got)
	}
}

func TestAStructuralEditRightAfterTheSessionsOwnWriteIsNotRefusedAsStale(t *testing.T) {
	ctx, dir := ownWriteCtx(t)

	w := callTool(t, ctx, "write_file", map[string]interface{}{
		"path": "stats.py", "content": "def mean(xs):\n    return 0\n"})
	if !w.Success {
		t.Fatalf("setup write failed: %s", toolText(w))
	}
	s := callTool(t, ctx, "structural_edit", map[string]interface{}{
		"path": "stats.py", "selector": "function:mean",
		"content": "def mean(xs):\n    return sum(xs) / len(xs) if xs else 0\n"})
	if strings.Contains(toolText(s), "modified since last read") {
		t.Fatalf("structural_edit refused the session's own write as stale: %s", toolText(s))
	}
	// Unit tests have no V3 service. structural_edit checks for it only AFTER
	// the staleness check, so reaching that refusal proves staleness passed.
	// If that ordering ever changes this branch stops proving anything -- keep
	// the V3 check downstream of staleness, or give this test a V3 stub.
	if !s.Success {
		if strings.Contains(toolText(s), "V3 service URL not configured") {
			return
		}
		t.Fatalf("structural_edit after own write failed for another reason: %s", toolText(s))
	}
	got, _ := os.ReadFile(filepath.Join(dir, "stats.py"))
	if !strings.Contains(string(got), "sum(xs)") {
		t.Errorf("structural_edit reported success but disk has %q", got)
	}
}

// SAFETY IS PRESERVED: bytes changed by anything other than the session's own
// write must still be refused, for a written file and for a read file alike.
func TestAnOutsideChangeAfterTheSessionsOwnWriteIsStillRefused(t *testing.T) {
	ctx, dir := ownWriteCtx(t)
	target := filepath.Join(dir, "store.py")

	w := callTool(t, ctx, "write_file", map[string]interface{}{
		"path": "store.py", "content": "def load():\n    return []\n"})
	if !w.Success {
		t.Fatalf("setup write failed: %s", toolText(w))
	}
	// As a shell command this run issued could: different bytes, later mtime.
	if err := os.WriteFile(target, []byte("def load():\n    return None  # changed elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(target, future, future)

	e := callTool(t, ctx, "edit_file", map[string]interface{}{
		"path": "store.py", "old_str": "return None", "new_str": "return []"})
	if e.Success {
		t.Fatalf("an edit over bytes the session never wrote or read was accepted: %s", toolText(e))
	}
	if !strings.Contains(toolText(e), "modified since last read") &&
		!strings.Contains(toolText(e), "not read yet") {
		t.Errorf("refused, but not as a staleness refusal: %s", toolText(e))
	}
}

func TestAnOutsideChangeAfterAReadIsStillRefused(t *testing.T) {
	ctx, dir := ownWriteCtx(t)
	target := filepath.Join(dir, "config.py")
	if err := os.WriteFile(target, []byte("DEBUG = False\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := callTool(t, ctx, "read_file", map[string]interface{}{"path": "config.py"})
	if !r.Success {
		t.Fatalf("setup read failed: %s", toolText(r))
	}
	if err := os.WriteFile(target, []byte("DEBUG = True\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(target, future, future)

	e := callTool(t, ctx, "edit_file", map[string]interface{}{
		"path": "config.py", "old_str": "DEBUG = True", "new_str": "DEBUG = False"})
	if e.Success {
		t.Fatalf("an edit after an outside change to a read file was accepted: %s", toolText(e))
	}
	if !strings.Contains(toolText(e), "modified since last read") {
		t.Errorf("want the staleness refusal, got: %s", toolText(e))
	}
}
