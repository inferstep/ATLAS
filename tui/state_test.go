// Tests for how the TUI draws the pipeline state: a failed stage, and the
// pane before any stage has started.

package main

import (
	"testing"
	"time"

	"github.com/itigges22/atlas-tui/internal/pipelinestate"
)

// A stage that ended without success shows an icon and the label FAIL.
func TestAFailedStageRendersFailState(t *testing.T) {
	failed := &pipelinestate.Stage{Name: "pr_cot", StartedAt: time.Unix(1, 0), EndedAt: time.Unix(2, 0), Success: false}
	icon, _ := stageIcon(failed)
	if icon == "" {
		t.Fatal("stageIcon empty")
	}
	if stageStatusLabel(failed) != "FAIL" {
		t.Errorf("label = %q, want FAIL", stageStatusLabel(failed))
	}
}

func TestRenderPipelinePaneEmptyShowsWaitingMessage(t *testing.T) {
	p := pipelinestate.New()
	got, _, _ := renderPipelinePane(&p, 5, 80, 0)
	if got == "" {
		t.Fatal("expected non-empty content for empty state")
	}
	// Should contain something user-facing about waiting.
	if !contains(got, "waiting") {
		t.Errorf("got %q, expected mention of waiting", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
