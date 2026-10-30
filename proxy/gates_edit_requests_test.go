package main

import (
	"fmt"
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

// The java branch of sandbox/executor_server.py's _syntax_check_impl, verbatim
// (lines 1341-1366 at 9756223, including its continuation lines, comment and
// trailing whitespace), inside a minimal dispatch so it stands alone.
const javaDispatch = `def _syntax_check_impl(lang, code, workspace, filename=None):
    errors = []
    if lang == "python":
        pass

    elif lang == "java":
        class_name = _extract_java_classname(code)
        package = _extract_java_package(code)

        if package:
            # com.exampe => com/example (the package dir is created by
            # _contained_path, which makes the parent for every check file)
            fpath = _contained_path(workspace, *package.split('.'),
                                    f"{class_name}.java")
        else:
            fpath = _contained_path(workspace, f"{class_name}.java")

        fpath.write_text(code)
        result = _run_cmd(
            ["javac", "-d", str(workspace), str(fpath)],
            timeout=10, cwd=workspace
        )
        if result["returncode"] != 0:
            stderr = result.get("stderr", "")
            for line in stderr.splitlines():
                if "error:" in line:
                    errors.append(line.strip())
            if not errors and stderr.strip():
                errors.append(stderr.strip().split("\n")[-1])

    elif lang == "kotlin":
        pass
    return errors
`

// A toml branch, as a model sent it for insertion into the java branch.
const tomlBranch = `
    elif lang == "toml":
        try:
            import tomllib
            tomllib.loads(code)
        except Exception as e:
            errors.append(f"TomlSyntaxError: {e}")`

func blockLineOf(t *testing.T, src, needle string) int {
	t.Helper()
	for i, l := range strings.Split(src, "\n") {
		if strings.Contains(l, needle) {
			return i + 1
		}
	}
	t.Fatalf("anchor %q not found", needle)
	return 0
}

func TestInsertingIntoTheMiddleOfABlockIsRefusedThroughTheTool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "executor_server.py")
	if err := os.WriteFile(path, []byte(javaDispatch), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.PermissionMode = PermissionYolo
	ctx.RecordFileRead(path, javaDispatch)

	// After `stderr = result.get("stderr", "")`, in the middle of the java branch.
	at := blockLineOf(t, javaDispatch, `stderr = result.get("stderr", "")`)
	res := callTool(t, ctx, "insert_after", map[string]interface{}{
		"path": "executor_server.py", "line": at, "content": tomlBranch})
	if res.Success {
		t.Fatalf("an insertion that splits the java branch was accepted")
	}
	msg := toolText(res)
	for _, want := range []string{"for line in stderr.splitlines():", "if result[\"returncode\"] != 0:", "except Exception as e:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal should name %q so the model can see what would move; got: %s", want, msg)
		}
	}
	// The model edits by line number, so the refusal names the first line that would move by its number.
	moving := blockLineOf(t, javaDispatch, "for line in stderr.splitlines():")
	if want := fmt.Sprintf("line %d `for line in stderr.splitlines():`", moving); !strings.Contains(msg, want) {
		t.Errorf("refusal should name %q; got: %s", want, msg)
	}
	if got, _ := os.ReadFile(path); string(got) != javaDispatch {
		t.Errorf("the file changed although the insertion was refused")
	}
}

func TestInsertingAFullBranchBetweenBranchesIsAccepted(t *testing.T) {
	lines := strings.Split(javaDispatch, "\n")
	// After the java branch's last statement, before the blank line and
	// `elif lang == "kotlin":` -- the correct place.
	at := blockLineOf(t, javaDispatch, `errors.append(stderr.strip().split("\n")[-1])`)
	if msg, moved := insertionReparentsPython("executor_server.py", lines, at, strings.Split(tomlBranch, "\n")); moved {
		t.Fatalf("a branch inserted after the end of the previous branch was refused: %s", msg)
	}
	// And a plain statement added at the same indentation inside a block.
	at = blockLineOf(t, javaDispatch, `fpath.write_text(code)`)
	if msg, moved := insertionReparentsPython("executor_server.py", lines, at,
		[]string{`        log_attempt(fpath)`}); moved {
		t.Fatalf("a same-level statement was refused: %s", msg)
	}
}

func TestBlockScannerHandlesStringsBracketsAndContinuations(t *testing.T) {
	src := strings.Join([]string{
		`def f(x):`,                 // 1
		`    doc = """`,             // 2 triple string opens
		`if this looked like code:`, // 3 inside the string
		`    it is not"""`,          // 4 closes
		`    total = (x +`,          // 5 bracket opens
		`  1)`,                      // 6 continuation, odd indent
		`    y = x \`,               // 7 backslash continuation
		`        + 2`,               // 8
		`    if total:`,             // 9
		`        return y  # (unbalanced in a comment`, // 10
		`    return total`, // 11
	}, "\n")
	parents, ok := pythonBlockParents(strings.Split(src, "\n"))
	if !ok {
		t.Fatalf("scanner lost track of a well-formed file")
	}
	want := map[int]int{0: -1, 1: 0, 4: 0, 6: 0, 8: 0, 9: 8, 10: 0}
	for i, p := range want {
		if parents[i] != p {
			t.Errorf("line %d: parent %d, want %d", i+1, parents[i], p)
		}
	}
	for _, i := range []int{2, 3, 5, 7} {
		if parents[i] != notLogicalStart {
			t.Errorf("line %d is inside a string or continuation and must not be a statement start", i+1)
		}
	}
	// An insertion that is only a comment or blank line cannot move anything.
	if _, moved := insertionReparentsPython("f.py", strings.Split(src, "\n"), 9, []string{"", "# note"}); moved {
		t.Errorf("a comment-only insertion was reported as moving code")
	}
	// Non-Python files are not judged by Python indentation rules.
	if _, moved := insertionReparentsPython("f.yaml", strings.Split(javaDispatch, "\n"),
		blockLineOf(t, javaDispatch, `stderr = result.get("stderr", "")`), strings.Split(tomlBranch, "\n")); moved {
		t.Errorf("a non-Python file was judged")
	}
}

// A model that re-sends the file it just wrote must be told to run it. Told
// instead that it need not reproduce input data, it re-sent the file again
// and the test never ran.

// ownTestFile is over 200 bytes, so it counts as an echo for any file.
const ownTestFile = "from stats import mean, median\n\n\ndef test_mean():\n" +
	"    assert mean([1, 2, 3]) == 2.0\n    assert mean([1, 2, 3, 4]) == 2.5\n\n\n" +
	"def test_median():\n    assert median([1, 3, 2]) == 2\n    assert median([1, 2, 3, 4]) == 2.5\n" +
	"    assert median([1]) == 1\n\n\nif __name__ == '__main__':\n    test_mean()\n    test_median()\n"

// smallOwnTestFile is 76 bytes, under the 200-byte echo floor. The floor
// applies only to files the session did not write.
const smallOwnTestFile = "from chunk import chunks\n\nresult = chunks([1, 2, 3, 4, 5], 2)\nprint(result)\n"

func TestWriteFileOfContentTheFileAlreadyHolds(t *testing.T) {
	runIt := []string{"already holds exactly this content", "You wrote it earlier in this session", "run it"}
	changedSmallFile := strings.Replace(smallOwnTestFile, "print(result)", "assert result[-1] == [5]\nprint(result)", 1)

	for _, tc := range []struct {
		name         string
		path         string
		onDisk       string
		sessionWrote bool
		send         string
		wantSuccess  bool
		errorHas     []string
		errorLacks   string
	}{
		{name: "own file re-sent",
			path: "test_stats.py", onDisk: ownTestFile, sessionWrote: true, send: ownTestFile,
			errorHas: runIt, errorLacks: "input or fixture data"},
		{name: "small own file re-sent",
			path: "test_chunk.py", onDisk: smallOwnTestFile, sessionWrote: true, send: smallOwnTestFile,
			errorHas: runIt, errorLacks: "input or fixture data"},
		{name: "changed small own file lands",
			path: "test_chunk.py", onDisk: smallOwnTestFile, sessionWrote: true, send: changedSmallFile,
			wantSuccess: true},
		{name: "file the session did not write gets the fixture wording",
			path: "test_stats.py", onDisk: ownTestFile, send: ownTestFile,
			errorHas: []string{"input or fixture data"}},
		// A short file sent back unchanged proves nothing about retyping a
		// fixture.
		{name: "small file the session did not write is not an echo",
			path: "test_chunk.py", onDisk: smallOwnTestFile, send: smallOwnTestFile,
			wantSuccess: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, path := exactEditWorld(t, tc.path, tc.onDisk)
			if tc.sessionWrote {
				ctx.SessionWrites[tc.path] = true
			}

			res := exactEditCall(t, ctx, "write_file", map[string]interface{}{
				"path": tc.path, "content": tc.send})

			if res.Success != tc.wantSuccess {
				t.Fatalf("success = %v, want %v (error %q)", res.Success, tc.wantSuccess, res.Error)
			}
			for _, want := range tc.errorHas {
				if !strings.Contains(res.Error, want) {
					t.Errorf("refusal lacks %q:\n%s", want, res.Error)
				}
			}
			if tc.errorLacks != "" && strings.Contains(res.Error, tc.errorLacks) {
				t.Errorf("refusal says %q:\n%s", tc.errorLacks, res.Error)
			}
			wantDisk := tc.onDisk
			if tc.wantSuccess {
				wantDisk = tc.send
			}
			if got := diskBytes(t, path); got != wantDisk {
				t.Errorf("disk holds %q, want %q", got, wantDisk)
			}
		})
	}
}

func TestResendsOwnWriteIgnoresSurroundingWhitespaceButNotEmptiness(t *testing.T) {
	if !resendsOwnWrite(smallOwnTestFile, "\n"+strings.TrimSpace(smallOwnTestFile)+"\n\n") {
		t.Error("a re-send that differs only in surrounding whitespace was not recognised")
	}
	if resendsOwnWrite("", "") || resendsOwnWrite("\n", "  ") {
		t.Error("an empty re-send counted as an echo")
	}
	if resendsOwnWrite(smallOwnTestFile, strings.Replace(smallOwnTestFile, "2)", "3)", 1)) {
		t.Error("a changed write counted as an echo")
	}
}
