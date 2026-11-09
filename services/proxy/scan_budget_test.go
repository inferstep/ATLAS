package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itigges22/atlas-proxy/internal/assetlint"
	"github.com/itigges22/atlas-proxy/internal/projectcontext"
	"github.com/itigges22/atlas-proxy/internal/wire"
)

// Issue #365, second fault: file work outside a tool call had no time limit, so
// a start scan over a deep tree could hold the session and the drain that
// follows a shutdown. The budget is stated in scan_budget.go; these cases hold
// the two halves of it — a spent deadline stops every scan, and a scan that is
// merely slow says so instead of staying silent.

// scanWorkspace is a workspace wide enough that a scan has something to do: a
// README the priority pass takes by name, and .py files in a folder.
func scanWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("the project"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		name := filepath.Join(pkg, "m"+string(rune('a'+i))+".py")
		if err := os.WriteFile(name, []byte("x = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// expiredDeadline is a deadline that is already spent, so every scan meets it
// on its first entry.
func expiredDeadline() time.Time { return time.Now().Add(-time.Millisecond) }

func TestAnExpiredScanDeadlineStopsEveryStartScan(t *testing.T) {
	dir := scanWorkspace(t)
	deadline := expiredDeadline()

	if got := walkPythonFilesWithin(dir, deadline); len(got) != 0 {
		t.Errorf("walkPythonFiles read %d files past its deadline: %v", len(got), keysOf(got))
	}
	if snap := snapshotWorkspaceWithin(dir, deadline); len(snap.files) != 0 || !snap.truncated {
		t.Errorf("snapshotWorkspace past its deadline = %d files, truncated=%v; want none and truncated",
			len(snap.files), snap.truncated)
	}
	if got := projectcontext.SampleWithin(dir, 10, 4096, deadline); len(got) != 0 {
		t.Errorf("Sample read %d files past its deadline: %v", len(got), keysOf(got))
	}
	if got := assetlint.FindingsWithin(dir, deadline); got != nil {
		t.Errorf("assetlint judged a project it did not finish reading: %v", got)
	}
}

// The other half: a budget that is not spent reads everything it did before.
func TestTheSameScansStillReadInsideTheirBudget(t *testing.T) {
	dir := scanWorkspace(t)

	if got := walkPythonFilesWithin(dir, scanDeadline()); got["pkg/ma.py"] != "x = 1\n" {
		t.Errorf("walkPythonFiles inside its budget = %v, want pkg/ma.py", keysOf(got))
	}
	snap := snapshotWorkspaceWithin(dir, scanDeadline())
	if snap.truncated || snap.files["README.md"].size == 0 {
		t.Errorf("snapshotWorkspace inside its budget = %d files, truncated=%v", len(snap.files), snap.truncated)
	}
	if got := projectcontext.SampleWithin(dir, 10, 4096, scanDeadline()); got["README.md"] != "the project" {
		t.Errorf("Sample inside its budget = %v, want the README's text", keysOf(got))
	}
	if got := assetlint.FindingsWithin(dir, scanDeadline()); len(got) != 0 {
		t.Errorf("assetlint on a project with nothing to report = %v", got)
	}
}

// captureEvents swaps in a broker that keeps what it is sent, and returns the
// channel those envelopes land on.
func captureEvents(t *testing.T) chan wire.Envelope {
	t.Helper()
	prev := defaultBroker
	defaultBroker = &broker{subscribers: map[chan wire.Envelope]struct{}{}}
	t.Cleanup(func() { defaultBroker = prev })
	events := defaultBroker.subscribe()
	if events == nil {
		t.Fatal("the broker refused a subscription")
	}
	return events
}

// nextEvent waits for one envelope of the given type, skipping the others.
func nextEvent(t *testing.T, events chan wire.Envelope, kind string) wire.Envelope {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s envelope arrived", kind)
		}
	}
}

func TestASlowStartScanReportsItself(t *testing.T) {
	events := captureEvents(t)

	watch := beginScanWatch(workspaceStage, 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond) // past the threshold, still scanning
	watch.End(7, false)

	start := nextEvent(t, events, wire.EvtStageStart)
	if start.Stage != workspaceStage {
		t.Errorf("slow scan started stage %q, want %q", start.Stage, workspaceStage)
	}
	end := nextEvent(t, events, wire.EvtStageEnd)
	if end.Stage != workspaceStage {
		t.Errorf("slow scan ended stage %q, want %q", end.Stage, workspaceStage)
	}
	if end.Payload["files"] != 7 {
		t.Errorf("stage_end reported files=%v, want 7", end.Payload["files"])
	}
}

// A scan inside the threshold is the ordinary case: it must add no rows to the
// pipeline pane, or every session start pays for a report nobody needs.
func TestAFastStartScanSaysNothing(t *testing.T) {
	events := captureEvents(t)

	watch := beginScanWatch(workspaceStage, time.Hour)
	watch.End(2, false)

	select {
	case ev := <-events:
		t.Fatalf("a scan inside its threshold emitted %s on stage %q", ev.Type, ev.Stage)
	case <-time.After(50 * time.Millisecond):
	}
}

// The stated limit, held as a number: a budget that is longer than the session
// total would not bound a shutdown, and a threshold at or past the budget could
// never fire.
func TestTheScanBudgetIsStatedAndShort(t *testing.T) {
	total, _ := sessionBudget()
	if workspaceScanBudget <= 0 || workspaceScanBudget >= total {
		t.Errorf("workspaceScanBudget = %v, want more than zero and less than the session total %v",
			workspaceScanBudget, total)
	}
	if workspaceScanSlow <= 0 || workspaceScanSlow >= workspaceScanBudget {
		t.Errorf("workspaceScanSlow = %v, want more than zero and less than the budget %v",
			workspaceScanSlow, workspaceScanBudget)
	}
}
