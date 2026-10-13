package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rejectedWrite is the conversation tail that makes the next step name the
// selectors of path: a write_file refused because the file exists.
func rejectedWrite(path string) []AgentMessage {
	return []AgentMessage{
		{Role: "user", Content: "fix it"},
		{Role: "tool", ToolName: "write_file", Content: `{"success":false,"error":"File ` + path +
			` already exists (41 lines). Use structural_edit"}`},
	}
}

func TestStepNoteReadsItsTargetInsideTheWorkspace(t *testing.T) {
	workspace := t.TempDir()
	writeInWorkspace(t, workspace, "real.py", "def inside_name():\n    return 1\n")
	linkOutOfWorkspace(t, workspace, "linked.py", "def outside_name():\n    return 1\n")
	ctx := NewAgentContext(workspace, Tier2Medium)

	ctx.Messages = rejectedWrite("real.py")
	msgs, _ := buildStepRequest(ctx)
	if note := msgs[len(msgs)-1].Content; !strings.Contains(note, "function:inside_name") {
		t.Errorf("the step note does not name the function of the regular file:\n%s", note)
	}

	ctx.Messages = rejectedWrite("linked.py")
	msgs, _ = buildStepRequest(ctx)
	if note := msgs[len(msgs)-1].Content; strings.Contains(note, "outside_name") {
		t.Errorf("the step note names a function of a file outside the workspace:\n%s", note)
	}
}

// probeEcho stands in for the sandbox: it answers with what is at the probe
// name in the workspace, as a sandbox that shares the folder does.
func probeEcho(t *testing.T, workspace string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		data, _ := os.ReadFile(filepath.Join(workspace, mountProbeName))
		out, _ := json.Marshal(map[string]string{"stdout": string(data)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestMountProbeIsNeverWrittenThroughALink(t *testing.T) {
	t.Run("a link that leaves the workspace", func(t *testing.T) {
		workspace := t.TempDir()
		linkOutOfWorkspace(t, workspace, mountProbeName, outsideMarker)
		outside, err := os.Readlink(filepath.Join(workspace, mountProbeName))
		if err != nil {
			t.Fatalf("read the link: %v", err)
		}
		checkMountProbe(t, workspace)
		if data, err := os.ReadFile(outside); err != nil || string(data) != outsideMarker {
			t.Errorf("the file outside the workspace now holds %q (%v), want it unchanged", data, err)
		}
	})
	t.Run("a link to a file of the workspace", func(t *testing.T) {
		workspace := t.TempDir()
		writeInWorkspace(t, workspace, "app.py", "inside")
		if err := os.Symlink("app.py", filepath.Join(workspace, mountProbeName)); err != nil {
			t.Fatalf("make the link: %v", err)
		}
		checkMountProbe(t, workspace)
		if data, err := os.ReadFile(filepath.Join(workspace, "app.py")); err != nil || string(data) != "inside" {
			t.Errorf("the workspace file now holds %q (%v), want it unchanged", data, err)
		}
	})
}

// checkMountProbe runs the alignment check on a workspace whose sandbox shares
// the folder, and expects no problem and no probe left behind.
func checkMountProbe(t *testing.T, workspace string) {
	t.Helper()
	ctx := NewAgentContext(workspace, Tier2Medium)
	ctx.SandboxURL = probeEcho(t, workspace)
	resetWorkspaceAlignmentCache()
	t.Cleanup(resetWorkspaceAlignmentCache)
	if problem := verifyWorkspaceAlignment(ctx); problem != "" {
		t.Errorf("a shared folder was reported as split: %s", problem)
	}
	if _, err := os.Lstat(filepath.Join(workspace, mountProbeName)); !os.IsNotExist(err) {
		t.Errorf("the probe name is still taken after the check (%v)", err)
	}
}

func TestConfinedDirWritesOnlyANewNameAndRemovesOnlyTheNameItself(t *testing.T) {
	workspace := t.TempDir()
	writeInWorkspace(t, workspace, "taken.txt", "inside")
	linkOutOfWorkspace(t, workspace, "link.txt", outsideMarker)
	outside, err := os.Readlink(filepath.Join(workspace, "link.txt"))
	if err != nil {
		t.Fatalf("read the link: %v", err)
	}
	dir := openConfinedDir(workspace)
	defer dir.Close()

	if err := dir.WriteNewFile("new.txt", []byte("new"), 0o600); err != nil {
		t.Errorf("WriteNewFile of a free name: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(workspace, "new.txt")); string(data) != "new" {
		t.Errorf("the new file holds %q, want the content", data)
	}
	for _, name := range []string{"taken.txt", "link.txt"} {
		if err := dir.WriteNewFile(name, []byte("changed"), 0o600); err == nil {
			t.Errorf("WriteNewFile(%s) wrote over a name that was taken, want an error", name)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(workspace, "taken.txt")); string(data) != "inside" {
		t.Errorf("the taken file now holds %q, want it unchanged", data)
	}
	if err := dir.Remove("link.txt"); err != nil {
		t.Errorf("Remove of a link: %v", err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != outsideMarker {
		t.Errorf("the file outside the workspace now holds %q (%v), want it unchanged", data, err)
	}
	if err := openConfinedDir("").WriteNewFile("a.txt", nil, 0o600); err == nil {
		t.Error("WriteNewFile in a folder that is not open succeeded, want an error")
	}
}

func TestADeliverableOutsideTheWorkspaceDemonstratesNothing(t *testing.T) {
	const outsideJSON = `{"note": "` + outsideMarker + `"}`
	workspace := t.TempDir()
	writeInWorkspace(t, workspace, "real.json", "{\"a\": 1}\n")
	linkOutOfWorkspace(t, workspace, "linked.json", outsideJSON)
	outside, err := os.Readlink(filepath.Join(workspace, "linked.json"))
	if err != nil {
		t.Fatalf("read the link: %v", err)
	}
	// The syntax checker passes whatever it is sent and keeps what it was sent.
	var checked []string
	checker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Code string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		checked = append(checked, in.Code)
		_ = json.NewEncoder(w).Encode(map[string]bool{"valid": true})
	}))
	defer checker.Close()
	ctx := NewAgentContext(workspace, Tier2Medium)
	ctx.SandboxURL = checker.URL

	if !deliverablesDemonstrablyValid(ctx, []string{"real.json"}) {
		t.Error("a valid file in the workspace did not count")
	}
	if deliverablesDemonstrablyValid(ctx, []string{"linked.json"}) {
		t.Error("a link that leaves the workspace counted as a valid deliverable")
	}
	if deliverablesDemonstrablyValid(ctx, []string{outside}) {
		t.Error("a path outside the workspace counted as a valid deliverable")
	}
	if strings.Contains(strings.Join(checked, "\n"), outsideMarker) {
		t.Error("the content of a file outside the workspace was sent to the syntax checker")
	}
}
