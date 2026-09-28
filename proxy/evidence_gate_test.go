package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An answer that names a file the run never opened is a guess, and it reads
// exactly like knowledge. Measured over 12 sessions of a diagnostic question
// across three modules: every one ran list_directory, outlined a single file,
// and answered. The outcome tracked the filename it guessed rather than
// anything it read — scoring.py wrong 11 times out of 11, planning.py right
// once out of once.

func evidenceCtx(t *testing.T, files map[string]string) *AgentContext {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &AgentContext{
		WorkingDir:    root,
		BodySeen:      map[string]bool{},
		FilesRead:     map[string]string{},
		FileReadTimes: map[string]time.Time{},
	}
}

func TestCitingAFileTheRunNeverOpened(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{
		"scoring.py":  "def score_candidate():\n    pass\n",
		"planning.py": "def select_best():\n    pass\n",
	})
	// The exact shape observed: a file, a function, and a line range, none of
	// which the model was ever shown.
	answer := "The issue is in `scoring.py` within the `score_candidate` " +
		"function (lines 134-142)."
	got := unreadFileCitations(ctx, answer)
	if len(got) != 1 || got[0] != "scoring.py" {
		t.Fatalf("want [scoring.py], got %v", got)
	}
}

func TestReadingTheFileClearsTheGate(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"scoring.py": "x = 1\n"})
	ctx.RecordBodySeen(resolveAgentPath(ctx, "scoring.py"))
	if got := unreadFileCitations(ctx, "The bug is in scoring.py"); len(got) != 0 {
		t.Fatalf("a file that was read must not be cited: %v", got)
	}
}

// The distinction the gate rests on: outline_file caches the whole source for
// staleness tracking, so FilesRead says "read" for a file the model has only
// ever seen the signatures of.
func TestOutlineDoesNotCountAsSeeingTheBody(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"scoring.py": "def f():\n    pass\n"})
	path := resolveAgentPath(ctx, "scoring.py")
	// What outline_file does: cache the source, show the model signatures.
	ctx.RecordFileRead(path, "def f():\n    pass\n")
	if !ctx.WasFileRead(path) {
		t.Fatal("outline still populates the read cache, by design")
	}
	if ctx.WasBodySeen(path) {
		t.Fatal("an outline shows no code and must not count as seeing the body")
	}
	if got := unreadFileCitations(ctx, "the bug is in scoring.py"); len(got) != 1 {
		t.Fatalf("outlined-only file should still be cited, got %v", got)
	}
}

func TestAFileTheModelWroteIsEvidenceEnough(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"out.py": "print(1)\n"})
	ctx.RecordBodySeen(resolveAgentPath(ctx, "out.py"))
	if got := unreadFileCitations(ctx, "I created out.py with the helper."); len(got) != 0 {
		t.Fatalf("the model authored the contents: %v", got)
	}
}

func TestNamesThatAreNotWorkspaceFilesAreIgnored(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"real.py": "x = 1\n"})
	for _, answer := range []string{
		"upgrade to V3.2 for this",
		"the score dropped to 0.34",
		"import os.path at the top",
		"see requirements.txt for the pins", // not present in this workspace
	} {
		if got := unreadFileCitations(ctx, answer); len(got) != 0 {
			t.Errorf("%q should cite nothing, got %v", answer, got)
		}
	}
}

func TestAnEmptyReplyCitesNothing(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"real.py": "x = 1\n"})
	if got := unreadFileCitations(ctx, "   "); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestTheRejectionNamesTheFileAndTheFix(t *testing.T) {
	msg := unreadCitationMessage([]string{"scoring.py"})
	for _, want := range []string{"scoring.py", "read_file", "outline_file", "search_files"} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection should mention %q: %s", want, msg)
		}
	}
}

func TestTheRejectionIsCappedAndReadable(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{
		"a.py": "1", "b.py": "1", "c.py": "1", "d.py": "1",
	})
	got := unreadFileCitations(ctx, "look at a.py, b.py, c.py and d.py")
	if len(got) != maxCitedPaths {
		t.Fatalf("want %d cited paths, got %v", maxCitedPaths, got)
	}
	msg := unreadCitationMessage(got)
	for _, p := range got {
		if !strings.Contains(msg, p) {
			t.Errorf("cited %s but the rejection does not name it: %s", p, msg)
		}
	}
	// Plural subject, plural object: "read_file on it" is wrong for three files.
	if !strings.Contains(msg, "those files") || !strings.Contains(msg, "each of them") {
		t.Errorf("plural rejection reads as singular: %s", msg)
	}
	if strings.Contains(msg, ",,") || strings.Contains(msg, " and ,") {
		t.Errorf("list punctuation is malformed: %s", msg)
	}
}

// The failure the evidence gate backstops was caused upstream, by guidance.
// The tool description sold outline_file on costing "almost no context" and
// told the model to reach for it FIRST "instead of reading the whole thing",
// while the system prompt separately said never to answer from code it had
// not opened. The model followed the procedure and dropped the principle.
//
// Measured over run 16's 28 sessions: peak prompt tokens median 6,294, worst
// case 15,406, against a 131,072-token window. Context frugality is buying
// nothing at 12% utilisation, and read_file already caps itself on a file too
// large to load, so the advice duplicated a guard that works and cost
// correctness to do it.
func TestOutlineIsNotSoldAsACheaperRead(t *testing.T) {
	def := outlineFileTool()
	for _, banned := range []string{"almost no context", "instead of reading the whole"} {
		if strings.Contains(def.Description, banned) {
			t.Errorf("outline_file must not be pitched as a cheaper read: %q", banned)
		}
	}
	// It has to say what it withholds, or an outline reads as having read.
	if !strings.Contains(def.Description, "NO code") {
		t.Errorf("outline_file must state that it returns no code: %s", def.Description)
	}
}

// Every write path has to record the body, not just the one I checked.
//
// The gate shipped recording only on the T1 direct write, so files written
// through the V3 pipeline stayed unmarked and the gate fired on code the
// model had authored moments earlier. Measured on run 18: 11 of 12 firings
// were solve.py or test_stats.py, every one a V3-path write. This asserts
// the funnel rather than the call sites, since the bug was a call site
// nobody updated.
func TestEveryWritePathRunsThroughTheRecorder(t *testing.T) {
	src, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, "writeFileDirect(") {
			continue
		}
		// Its definition and the one call inside the recorder are the only
		// legitimate mentions.
		if strings.Contains(line, "func writeFileDirect") ||
			strings.Contains(line, "res, err := writeFileDirect(path, content)") {
			continue
		}
		t.Errorf("tools.go:%d calls writeFileDirect directly, so a write there "+
			"never records the body and the evidence gate will fire on a file "+
			"the model just wrote: %s", i+1, strings.TrimSpace(line))
	}
}
