// Tests for the pipeline state machine (PC-062 step 2).
//
// State is a pure function of the event sequence — these tests pin
// that contract by replaying realistic event sequences and asserting
// the derived state matches what the pipeline pane would render.

package pipelinestate

import (
	"testing"
	"time"

	"github.com/itigges22/atlas-tui/internal/proxyclient"
)

// envOf is a tiny helper for building test envelopes inline.
func envOf(typ, stage string, payload map[string]interface{}, ts float64) proxyclient.Envelope {
	if payload == nil {
		payload = map[string]interface{}{}
	}
	return proxyclient.Envelope{
		EventID:   "evt_test",
		Timestamp: ts,
		Type:      typ,
		Stage:     stage,
		Payload:   payload,
	}
}

func TestStageStartCreatesRunningStage(t *testing.T) {
	p := New()
	p.Apply(envOf(proxyclient.EvtStageStart, "phase2", nil, 1.0))

	stages := p.Stages()
	if len(stages) != 1 {
		t.Fatalf("got %d stages, want 1", len(stages))
	}
	if !stages[0].Running() {
		t.Errorf("stage should be running after stage_start")
	}
	if stages[0].Name != "phase2" {
		t.Errorf("name = %q, want phase2", stages[0].Name)
	}
}

func TestStageEndMarksStageComplete(t *testing.T) {
	p := New()
	p.Apply(envOf(proxyclient.EvtStageStart, "phase2", nil, 1.0))
	p.Apply(envOf(proxyclient.EvtStageEnd, "phase2",
		map[string]interface{}{"success": true}, 1.5))

	stages := p.Stages()
	if stages[0].Running() {
		t.Errorf("stage should NOT be running after stage_end")
	}
	if !stages[0].Success {
		t.Errorf("success not set")
	}
	want := 500 * time.Millisecond
	got := stages[0].Duration()
	if got != want {
		t.Errorf("duration = %v, want %v", got, want)
	}
}

// A stage_end with success false leaves the stage failed.
func TestStageEndSuccessFalseMarksTheStageFailed(t *testing.T) {
	p := New()
	p.Apply(envOf(proxyclient.EvtStageStart, "pr_cot", nil, 1.0))
	p.Apply(envOf(proxyclient.EvtStageEnd, "pr_cot",
		map[string]interface{}{"success": false}, 2.0))

	stages := p.Stages()
	if stages[0].Success {
		t.Errorf("success should be false")
	}
}

func TestStageRetryResetsTimingNotIdentity(t *testing.T) {
	// probe → probe_retry → probe_pass should pair the pass with the retry's
	// timing, not the original probe's. (probe_retry strips to "probe" via
	// suffix mapping; v3-service emits stage_start("probe_retry"), which
	// at the proxy level looks like stage_start with stage="probe" after
	// suffix stripping. Here in the TUI, we receive whatever stage name
	// the server sent — the server already did the suffix stripping.)
	p := New()
	p.Apply(envOf(proxyclient.EvtStageStart, "probe", nil, 1.0))
	p.Apply(envOf(proxyclient.EvtStageStart, "probe", nil, 5.0)) // retry — same stage name
	p.Apply(envOf(proxyclient.EvtStageEnd, "probe",
		map[string]interface{}{"success": true}, 6.0))

	stages := p.Stages()
	if len(stages) != 1 {
		t.Fatalf("retry should not duplicate stage rows: %d stages", len(stages))
	}
	want := time.Second
	got := stages[0].Duration()
	if got != want {
		t.Errorf("retry duration = %v, want %v (timing should reset on retry)", got, want)
	}
}

func TestActiveStagePicksMostRecentRunning(t *testing.T) {
	p := New()
	p.Apply(envOf(proxyclient.EvtStageStart, "outer", nil, 1.0))
	p.Apply(envOf(proxyclient.EvtStageStart, "inner", nil, 2.0))

	active := p.ActiveStage()
	if active == nil {
		t.Fatal("ActiveStage = nil, want inner")
	}
	if active.Name != "inner" {
		t.Errorf("active = %q, want inner", active.Name)
	}
}

func TestActiveStageNilWhenNoneRunning(t *testing.T) {
	p := New()
	p.Apply(envOf(proxyclient.EvtStageStart, "phase2", nil, 1.0))
	p.Apply(envOf(proxyclient.EvtStageEnd, "phase2",
		map[string]interface{}{"success": true}, 2.0))

	if p.ActiveStage() != nil {
		t.Errorf("ActiveStage should be nil when all stages ended")
	}
}

func TestToolCallCountersIncrement(t *testing.T) {
	p := New()
	p.Apply(envOf(proxyclient.EvtToolCall, "agent",
		map[string]interface{}{"name": "edit_file", "turn": float64(1)}, 1.0))
	p.Apply(envOf(proxyclient.EvtToolResult, "agent",
		map[string]interface{}{"name": "edit_file", "success": true}, 1.5))
	p.Apply(envOf(proxyclient.EvtToolResult, "agent",
		map[string]interface{}{"name": "run_command", "success": false}, 2.0))

	if p.toolCalls != 1 {
		t.Errorf("toolCalls = %d, want 1", p.toolCalls)
	}
	if p.ToolSuccesses != 1 {
		t.Errorf("ToolSuccesses = %d, want 1", p.ToolSuccesses)
	}
	if p.ToolFailures != 1 {
		t.Errorf("ToolFailures = %d, want 1", p.ToolFailures)
	}
	if p.CurrentTurn != 1 {
		t.Errorf("CurrentTurn = %d, want 1", p.CurrentTurn)
	}
}

func TestErrorEventIncrementsErrorCounter(t *testing.T) {
	p := New()
	p.Apply(envOf(proxyclient.EvtError, "pr_cot",
		map[string]interface{}{"message": "model output empty",
			"recoverable": true}, 1.0))
	if p.Errors != 1 {
		t.Errorf("Errors = %d, want 1", p.Errors)
	}
}

func TestDoneEventCapturesFinalState(t *testing.T) {
	p := New()
	p.Apply(envOf(proxyclient.EvtDone, "pipeline",
		map[string]interface{}{"success": true,
			"total_duration_ms": float64(12453),
			"summary":           "done"}, 1.0))

	if !p.Done {
		t.Error("done flag not set")
	}
	if !p.DoneSuccess {
		t.Error("DoneSuccess not set")
	}
	if p.TotalMS != 12453 {
		t.Errorf("TotalMS = %d, want 12453", p.TotalMS)
	}
}

func TestStagesPreserveInsertionOrder(t *testing.T) {
	p := New()
	for _, stage := range []string{"probe", "phase1", "phase2", "pr_cot", "selected"} {
		p.Apply(envOf(proxyclient.EvtStageStart, stage, nil, 1.0))
	}
	got := []string{}
	for _, s := range p.Stages() {
		got = append(got, s.Name)
	}
	want := []string{"probe", "phase1", "phase2", "pr_cot", "selected"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestStageEndWithoutMatchingStartSynthesizesEntry(t *testing.T) {
	// Defensive: server can drop a stage_start (network blip during
	// reconnect). The TUI shouldn't crash — synthesize an entry so the
	// stage_end is at least visible.
	p := New()
	p.Apply(envOf(proxyclient.EvtStageEnd, "phase2",
		map[string]interface{}{"success": true}, 1.0))

	stages := p.Stages()
	if len(stages) != 1 {
		t.Fatalf("got %d stages, want 1", len(stages))
	}
	if stages[0].Running() {
		t.Errorf("synthesized stage should be marked complete (has stage_end)")
	}
}
