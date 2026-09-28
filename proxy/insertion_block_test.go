package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// The insertion 84296fd smallrung_toml rep2 made, verbatim.
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

	// After `stderr = result.get("stderr", "")` -- exactly where rep2 put it.
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
