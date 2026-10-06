package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The steering status looks for the control vector in the workspace too.
// That place is read through the workspace folder.
func TestSteeringStatusReadsItsWorkspacePlaceInsideTheWorkspace(t *testing.T) {
	const vector = "placeholder-vector.gguf"
	place := filepath.Join("models", vector)
	status := func(t *testing.T, workspace string) ASAStatus {
		t.Helper()
		t.Setenv("ATLAS_CONTROL_VECTOR", "/models/"+vector)
		t.Setenv("ATLAS_WORKSPACE_DIR", workspace)
		t.Setenv("ATLAS_MODELS_DIR", "")
		t.Setenv("ATLAS_MODEL_NAME", "placeholder-model")
		return probeASAStatus()
	}

	t.Run("a vector and its marker in the workspace", func(t *testing.T) {
		workspace := t.TempDir()
		writeInWorkspace(t, workspace, place, "vector")
		writeInWorkspace(t, workspace, place+".model", "placeholder-model\n")
		if got := status(t, workspace); got.Verdict != "active" || !got.VectorPresent {
			t.Errorf("status = %+v, want an active vector", got)
		}
	})
	t.Run("a vector name that resolves outside the workspace", func(t *testing.T) {
		workspace := t.TempDir()
		linkOutOfWorkspace(t, workspace, place, "vector")
		if got := status(t, workspace); got.Verdict != "missing" || got.VectorPresent {
			t.Errorf("status = %+v, want no vector", got)
		}
	})
	t.Run("a marker name that resolves outside the workspace", func(t *testing.T) {
		workspace := t.TempDir()
		writeInWorkspace(t, workspace, place, "vector")
		linkOutOfWorkspace(t, workspace, place+".model", "placeholder-other-model\n")
		got := status(t, workspace)
		if got.Verdict != "unverified" {
			t.Errorf("verdict = %q, want unverified: the marker is not in the workspace", got.Verdict)
		}
		if strings.Contains(got.Hint, "placeholder-other-model") {
			t.Errorf("the hint carries text from outside the workspace: %q", got.Hint)
		}
	})
}
