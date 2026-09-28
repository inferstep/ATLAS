package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// When V3 fails on a write, the model's own bytes land, and the final summary
// names the file (v3_fallback_note.go). It names a file only while the bytes
// V3 did not check are the ones on disk.

func TestTheV3NoteNamesOnlyTheUncheckedBytesOnDisk(t *testing.T) {
	dir := t.TempDir()
	ctx := &AgentContext{WorkingDir: dir}
	p := filepath.Join(dir, "app.py")
	if err := os.WriteFile(p, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx.noteV3Unchecked(p, v3FailureReason(context.DeadlineExceeded), "x = 1\n")
	if note := v3FallbackNote(ctx); !strings.Contains(note, "app.py (V3 ran out of time)") {
		t.Errorf("the unchecked file is not named: %q", note)
	}

	// Changed since: these are not the bytes V3 failed on.
	os.WriteFile(p, []byte("x = 2\n"), 0o644)
	if note := v3FallbackNote(ctx); note != "" {
		t.Errorf("a file changed since is still named: %q", note)
	}

	// V3 answered for a later write: forgotten, even when the bytes match.
	os.WriteFile(p, []byte("x = 1\n"), 0o644)
	ctx.clearV3Unchecked(p)
	if note := v3FallbackNote(ctx); note != "" {
		t.Errorf("a file V3 answered for is still named: %q", note)
	}

	ctx.noteV3Unchecked(p, v3FailureReason(errors.New("HTTP 503")), "x = 1\n")
	if note := v3FallbackNote(ctx); !strings.Contains(note, "app.py (V3 was unavailable)") {
		t.Errorf("the reason is not given: %q", note)
	}
}

// The write route: V3 is unavailable, the model's file lands, and the final
// summary says V3 did not check it.
func TestTheSummaryNamesAWriteV3DidNotCheck(t *testing.T) {
	r := editLoopFixture(t, nil, `{"task_mode":"work"}`, "Write stats.py.",
		script(stepWrite("stats.py", lensFile), stepRun("python3 stats.py"), stepDone("wrote stats.py")),
		editLoopOptions{})
	if !strings.Contains(r.terminal["summary"], "V3 did not check these files: stats.py (V3 was unavailable)") {
		t.Errorf("the summary does not name the file V3 did not check: %q (%s)",
			r.terminal["summary"], r.describe())
	}
}

// The edit route, the same.
func TestTheSummaryNamesAnEditV3DidNotCheck(t *testing.T) {
	r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, `{"task_mode":"work"}`,
		"Change helper in mod.py.",
		script(stepRead("mod.py"), stepEdit("mod.py", "    return 1\n", "    return 2\n"),
			stepRun("python3 mod.py"), stepDone("edited mod.py")),
		editLoopOptions{})
	if !strings.Contains(r.terminal["summary"], "mod.py (V3 was unavailable)") {
		t.Errorf("the summary does not name the file V3 did not check: %q (%s)",
			r.terminal["summary"], r.describe())
	}
}

// A file changed after the unchecked write is not named: the bytes on disk
// are no longer the ones V3 failed on.
func TestAFileChangedAfterAnUncheckedWriteIsNotNamed(t *testing.T) {
	r := editLoopFixture(t, nil, `{"task_mode":"work"}`, "Write stats.py.",
		script(stepWrite("stats.py", lensFile),
			stepRun(`python3 -c "open('stats.py', 'a').write('# checked by hand\n')"`),
			stepRun("python3 stats.py"), stepDone("wrote stats.py")),
		editLoopOptions{})
	if disk := r.disk(t, "stats.py"); !strings.HasSuffix(disk, "# checked by hand\n") {
		t.Fatalf("the command did not change stats.py, so this test shows nothing: %q", disk)
	}
	if strings.Contains(r.terminal["summary"], "V3 did not check") {
		t.Errorf("a file changed since is named: %q", r.terminal["summary"])
	}
}

// A run that does not complete names the file too: the note is on every
// ending, not only a completed one.
func TestAnIncompleteRunNamesTheFileToo(t *testing.T) {
	r := editLoopFixture(t, nil, `{"task_mode":"work"}`, "Write stats.py.",
		script(stepWrite("stats.py", lensFile), stepDone("wrote stats.py")),
		editLoopOptions{})
	if r.terminal["status"] == string(TerminalCompleted) {
		t.Fatalf("the run completed without running stats.py, so this test shows nothing: %s", r.describe())
	}
	if !strings.Contains(r.terminal["summary"], "stats.py (V3 was unavailable)") {
		t.Errorf("the %s summary does not name the file V3 did not check: %q",
			r.terminal["status"], r.terminal["summary"])
	}
}

// A delivered V3 candidate is not named.
func TestADeliveredCandidateIsNotNamed(t *testing.T) {
	r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiStrictWork,
		"Change helper in mod.py.",
		script(stepRead("mod.py"), stepEdit("mod.py", "    return 1\n", "    return 2\n"),
			stepRun("python3 mod.py"), stepDone("edited mod.py")),
		editLoopOptions{v3Winner: strings.Replace(accountingSeed, "    return 1\n", "    return 3\n", 1)})
	if disk := r.disk(t, "mod.py"); !strings.Contains(disk, "    return 3\n") {
		t.Fatalf("V3's candidate did not land, so this test shows nothing: %s", r.describe())
	}
	if strings.Contains(r.terminal["summary"], "V3 did not check") {
		t.Errorf("a file V3 answered for is named: %q (%s)", r.terminal["summary"], r.describe())
	}
}
