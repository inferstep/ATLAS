package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// An answer that names a file the run never opened is a guess, and it reads
// exactly like knowledge. The evidence gate finds such citations and asks the
// model to read the file before it answers.

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

// A reply cites a workspace file whose body the run never saw. Names that are
// not workspace files are not citations.
func TestUnreadFileCitations(t *testing.T) {
	onlyRealPy := map[string]string{"real.py": "x = 1\n"}
	for _, tc := range []struct {
		name   string
		files  map[string]string
		seen   []string
		answer string
		want   []string
	}{
		{
			// A file, a function and a line range, none of which the model
			// was ever shown.
			name: "an unread file is cited",
			files: map[string]string{
				"scoring.py":  "def score_candidate():\n    pass\n",
				"planning.py": "def select_best():\n    pass\n",
			},
			answer: "The issue is in `scoring.py` within the `score_candidate` " +
				"function (lines 134-142).",
			want: []string{"scoring.py"},
		},
		{
			// Every read, write and edit path records the body as seen.
			name:   "a file whose body was seen is not cited",
			files:  map[string]string{"scoring.py": "x = 1\n"},
			seen:   []string{"scoring.py"},
			answer: "The bug is in scoring.py",
		},
		{name: "a version number", files: onlyRealPy, answer: "upgrade to V3.2 for this"},
		{name: "a decimal number", files: onlyRealPy, answer: "the score dropped to 0.34"},
		{name: "a module path", files: onlyRealPy, answer: "import os.path at the top"},
		{name: "a file not in the workspace", files: onlyRealPy, answer: "see requirements.txt for the pins"},
		{name: "an empty reply", files: onlyRealPy, answer: "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := evidenceCtx(t, tc.files)
			for _, name := range tc.seen {
				ctx.RecordBodySeen(resolveAgentPath(ctx, name))
			}
			if got := unreadFileCitations(ctx, tc.answer); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("cited %v, want %v", got, tc.want)
			}
		})
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

// The outline_file description must not sell an outline as a cheaper read.
// A model told to reach for an outline first, instead of reading the file,
// follows that advice and then answers from signatures it never saw the code
// of. read_file already caps itself on a file too large to load, so frugality
// advice adds no guard and costs correctness.
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

// A citation that climbs out of the workspace is never looked up, so a reply
// cannot learn whether a file outside it exists.
func TestACitationOutsideTheWorkspaceIsNotLookedUp(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"real.py": "x = 1\n"})
	outside := filepath.Join(filepath.Dir(ctx.WorkingDir), "secret.txt")
	if err := os.WriteFile(outside, []byte("s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := unreadFileCitations(ctx, "the key is in x/../../secret.txt"); len(got) != 0 {
		t.Fatalf("a file outside the workspace was looked up: %v", got)
	}
}

// The script a command names is read only inside the workspace.
func TestAScriptOutsideTheWorkspaceIsNotRead(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"app.py": "inside\n"})
	outside := filepath.Join(filepath.Dir(ctx.WorkingDir), "server.py")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	read := workspaceFileReader(ctx)
	if got, ok := read("app.py"); !ok || got != "inside\n" {
		t.Fatalf("app.py = %q, %v", got, ok)
	}
	if got, ok := read("../server.py"); ok {
		t.Fatalf("read a script outside the workspace: %q", got)
	}
}
