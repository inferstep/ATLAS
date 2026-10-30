package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A write_file over an existing file nobody has looked at is refused, however
// small: a model once replaced a one-line puzzle input with the textbook example
// without reading it, and solved the wrong data. Once read, replacing it is the
// model's call again.
func TestUnreadOverwriteIsRefusedRegardlessOfSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "input.txt")
	if err := os.WriteFile(path, []byte("3,4,3,1,2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)

	if !isUnreadOverwrite(ctx, path, false, false) {
		t.Error("an existing file nobody has read was not protected")
	}
	ctx.RecordFileRead(path, "3,4,3,1,2\n")
	if isUnreadOverwrite(ctx, path, false, false) {
		t.Error("after read_file the overwrite was still refused")
	}
}

// Two files stay overwritable unread: the session's own draft, which it must be
// able to correct, and a corrupted file, which only a full rewrite repairs.
func TestUnreadOverwriteAllowsSessionOwnedAndCorrupted(t *testing.T) {
	for _, c := range []struct {
		name                   string
		corrupted, sessionOwns bool
	}{
		{"the session's own draft", false, true},
		{"a corrupted file", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "draft.py")
			if err := os.WriteFile(path, []byte("x = 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx := NewAgentContext(dir, Tier2Medium)
			if isUnreadOverwrite(ctx, path, c.corrupted, c.sessionOwns) {
				t.Error("refused the overwrite")
			}
		})
	}
}

// A CRLF file is normal. Bare or repeated CRs in an old_str are the signature
// of a model that degenerated partway through copying a block; one carried
// runs of \r and a literal `\rVert`, a LaTeX fragment, in JavaScript.
func TestStrayCarriageReturns(t *testing.T) {
	for _, c := range []struct {
		name, text string
		want       int
	}{
		{"a CRLF file", "line one\r\nline two\r\nline three\r\n", 0},
		{"a run of CRs ending in a CRLF, and a CR inside a word",
			"        let gameActive = true;\n        \r\r\r\r\r\r\n        \rVert", 6},
		{"text with no CR", "def f():\n    return 1\n", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := strayCarriageReturns(c.text); got != c.want {
				t.Errorf("strayCarriageReturns(%q) = %d, want %d", c.text, got, c.want)
			}
		})
	}
}

// The failure this came from: a model read snake_app.py, then sent
// old_str="76\t const ctx = canvas.getContext('2d');" — the correct line with
// read_file's display prefix still attached. It got a generic "not found",
// tried again with the prefix still on, and gave up after three failures.
func TestLineNumberPrefixIsDetectedInOldStr(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"the observed failure", "\n76\t const ctx = canvas.getContext('2d');", 1},
		{"bare prefix at start", "12\tfoo()", 1},
		{"several numbered lines", "10\ta\n11\tb\n12\tc", 3},
		{"indented before the number", "  7\tx = 1", 1},

		// Must NOT fire: these are legitimate file content.
		{"clean single line", "const ctx = canvas.getContext('2d');", 0},
		{"tab-indented code", "\tif (x) {", 0},
		{"a number with no tab", "76 const ctx", 0},
		{"digits mid-line", "const x = 76;\tconst y = 2;", 0},
		{"number after real text", "foo 12\tbar", 0},
		{"empty", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lineNumberPrefixedLines(tc.in); got != tc.want {
				t.Errorf("lineNumberPrefixedLines(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// A structural_edit once carried the module's `if __name__` block into a
// 3-line function, and the splice appended a second one: the file parsed and
// ran, with dead code under a blocking app.run().
func TestDuplicateEntrypointIsRefused(t *testing.T) {
	original := "import flask\n\n\nif __name__ == \"__main__\":\n    app.run()\n"
	edited := original + "\n\nif __name__ == \"__main__\":\n    app.run(port=5001)\n"

	msg := duplicateMainGuard("app.py", original, edited)
	for _, want := range []string{"app.py would end up with 2 `if __name__", "it was NOT written", "Only the first one ever runs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection missing %q:\n%s", want, msg)
		}
	}
}

// Only a change from at most one module entrypoint to more is refused, and
// only in Python.
func TestDuplicateEntrypointGuardLeavesEverythingElseAlone(t *testing.T) {
	one := "if __name__ == '__main__':\n    main()\n"
	two := one + one
	for _, c := range []struct {
		name, path, original, edited string
	}{
		{"the first entrypoint", "app.py", "x = 1\n", one},
		{"a file that already had two", "app.py", two, two + "print(1)\n"},
		{"a file that is not Python", "app.js", "x\n", two},
		{"a guard inside a function is not a module entrypoint", "app.py",
			one, one + "def go():\n    if __name__ == '__main__':\n        main()\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if msg := duplicateMainGuard(c.path, c.original, c.edited); msg != "" {
				t.Errorf("refused:\n%s", msg)
			}
		})
	}
}

// A write that sends back the file on disk is refused. The measured chain: a
// model retyped a 2000-line input fixture from memory, degenerated into
// repeating one line, the stream was cut mid-JSON and three identical retries
// ended the run. The cut truncates, so a long prefix is an echo too.
func TestEchoesExistingFile(t *testing.T) {
	fixture := strings.Repeat("199\n202\n201\n203\n", 200) // about 3 KB
	for _, c := range []struct {
		name, existing, incoming string
		want                     bool
	}{
		{"an exact rewrite of the file on disk", fixture, fixture, true},
		{"a truncated retype", fixture, fixture[:1200], true},
		{"an append", fixture, fixture + "\n# a real change\n", false},
		{"unrelated content", fixture, strings.Repeat("def solve():\n    pass\n", 40), false},
		{"a file too short to be evidence", "a = 1\n", "a = 1\n", false},
		{"a prefix too short to be evidence", fixture, fixture[:80], false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := echoesExistingFile(c.existing, c.incoming); got != c.want {
				t.Errorf("echoesExistingFile = %v, want %v", got, c.want)
			}
		})
	}
}

func TestEchoedWriteRejectionPointsAtTheProgramNotTheData(t *testing.T) {
	msg := echoedWriteRejection("input.txt", false)
	for _, want := range []string{"input.txt", "already has", "read it at runtime", "replace_lines"} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection missing %q:\n%s", want, msg)
		}
	}
}

// An edit needs a current view of the file: a read, or the session's own write
// while those bytes are still on disk. Ownership alone is not enough. A session
// once had its one-line repair of a file it had just written refused as "not
// read yet", re-sent the whole file, and the run died without the repair.
func TestAnEditNeedsAReadUnlessTheSessionsOwnWriteIsOnDisk(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)

	preexisting := filepath.Join(dir, "preexisting.py")
	if err := os.WriteFile(preexisting, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if editViewIsCurrent(ctx, preexisting) {
		t.Error("a file the session never read or wrote is current")
	}
	if editViewIsCurrent(ctx, filepath.Join(dir, "absent.py")) {
		t.Error("an absent file is current")
	}

	target := filepath.Join(dir, "solve.py")
	const body = "def solve():\n    return 1\n"
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// The record writeFileRecorded leaves of the session's own write.
	observeDeliverable(ctx, ledgerKey(ctx, target), []byte(body), ValidationKindSyntax, ValidationPassed, "")
	if !editViewIsCurrent(ctx, target) {
		t.Fatal("the session's own write, unchanged on disk, is not current")
	}

	// Something else, a shell command this run issued for one, changes the bytes.
	const changed = "def solve():\n    return 2  # changed elsewhere\n"
	if err := os.WriteFile(target, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if editViewIsCurrent(ctx, target) {
		t.Error("the view stayed current after the bytes on disk changed")
	}
	ctx.RecordFileRead(target, changed)
	if !editViewIsCurrent(ctx, target) {
		t.Error("a fresh read did not make the view current")
	}
}
