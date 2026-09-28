package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Literal edit arguments are applied exactly as sent.
//
// edit_file used to strip read_file's "N<tab>" display prefix from old_str and
// new_str whenever the stripped old_str matched, and report nothing. In
// stabilization cycle 2 (smallrung_toml rep 2) that rewrote an 18-line
// replacement silently. The contract now:
//   - old_str and new_str are applied as sent; text that really starts with
//     "N<tab>" (tab-separated data) edits normally;
//   - when only the prefix-free old_str matches, nothing changes, and the
//     refusal names the matched lines and a replace_lines call that makes the
//     change without reproducing the text;
//   - a match that needed quote-style or whitespace tolerance still applies,
//     and the result says so.

func exactEditWorld(t *testing.T, name, content string) (*AgentContext, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.PermissionMode = PermissionYolo
	ctx.RecordFileRead(path, content)
	ctx.RecordBodySeen(path)
	return ctx, path
}

func exactEditCall(t *testing.T, ctx *AgentContext, tool string, args map[string]interface{}) *ToolResult {
	t.Helper()
	raw, _ := json.Marshal(args)
	return executeToolCall(tool, raw, ctx)
}

func diskBytes(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTextThatReallyStartsWithANumberAndTabEditsExactly(t *testing.T) {
	file := "id\tname\n1\talpha\n2\tbeta\n3\tgamma\n"
	ctx, path := exactEditWorld(t, "table.tsv", file)
	res := exactEditCall(t, ctx, "edit_file", map[string]interface{}{"path": "table.tsv",
		"old_str": "2\tbeta", "new_str": "2\tBETA\t✓"})
	if !res.Success {
		t.Fatalf("an exact match on numbered content was refused: %s", res.Error)
	}
	if got, want := diskBytes(t, path), "id\tname\n1\talpha\n2\tBETA\t✓\n3\tgamma\n"; got != want {
		t.Errorf("bytes:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(string(res.Data), "content_note") {
		t.Errorf("an exact match must not carry a note: %s", res.Data)
	}
}

func TestCopiedDisplayPrefixesAreRefusedAndTheSuggestedRetryApplies(t *testing.T) {
	file := "import re\n\ndef clean(s):\n\tpat = re.compile(r\"\\s+\")  # naïve\n\treturn pat.sub(\" \", s)\n\nprint(clean(\"a  b\"))\n"
	ctx, path := exactEditWorld(t, "clean.py", file)
	res := exactEditCall(t, ctx, "edit_file", map[string]interface{}{"path": "clean.py",
		"old_str": "4\t\tpat = re.compile(r\"\\s+\")  # naïve\n5\t\treturn pat.sub(\" \", s)",
		"new_str": "4\t\tpat = re.compile(r\"[ \\t]+\")  # naïve\n5\t\treturn pat.sub(\" \", s).strip()"})
	if res.Success {
		t.Fatal("a prefixed old_str was applied")
	}
	if diskBytes(t, path) != file {
		t.Fatal("the file changed on a refused edit")
	}
	for _, want := range []string{"NOT applied", "lines 4-5 of clean.py", "new_str also has the prefix on 2 line(s)",
		`start_line 4, end_line 5`, `expected_first_line "pat = re.compile(r\"\\s+\")  # naïve"`,
		`expected_last_line "return pat.sub(\" \", s)"`} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("refusal lacks %q:\n%s", want, res.Error)
		}
	}
	// The suggested call, as the model would make it.
	retry := exactEditCall(t, ctx, "replace_lines", map[string]interface{}{"path": "clean.py",
		"start_line": 4, "end_line": 5,
		"expected_first_line": "pat = re.compile(r\"\\s+\")  # naïve",
		"expected_last_line":  "return pat.sub(\" \", s)",
		"content":             "\tpat = re.compile(r\"[ \\t]+\")  # naïve\n\treturn pat.sub(\" \", s).strip()\n"})
	if !retry.Success {
		t.Fatalf("the suggested replace_lines call failed: %s", retry.Error)
	}
	want := "import re\n\ndef clean(s):\n\tpat = re.compile(r\"[ \\t]+\")  # naïve\n\treturn pat.sub(\" \", s).strip()\n\nprint(clean(\"a  b\"))\n"
	if got := diskBytes(t, path); got != want {
		t.Errorf("bytes after retry:\n got %q\nwant %q", got, want)
	}
}

func TestMultilineUnicodeAndEscapesEditExactlyWithSurroundingBytesUnchanged(t *testing.T) {
	file := "# café\r\nSEP = \"\\t\"\r\nMSG = '''line one\r\nline \"two\"\r\n'''\r\nEND = 1\r\n"
	ctx, path := exactEditWorld(t, "m.py", file)
	res := exactEditCall(t, ctx, "edit_file", map[string]interface{}{"path": "m.py",
		"old_str": "MSG = '''line one\r\nline \"two\"\r\n'''",
		"new_str": "MSG = '''línea uno\r\nline \"two\" \\\\ done\r\n'''"})
	if !res.Success {
		t.Fatalf("exact multi-line edit refused: %s", res.Error)
	}
	want := "# café\r\nSEP = \"\\t\"\r\nMSG = '''línea uno\r\nline \"two\" \\\\ done\r\n'''\r\nEND = 1\r\n"
	if got := diskBytes(t, path); got != want {
		t.Errorf("bytes:\n got %q\nwant %q", got, want)
	}
}

// The inherited failure, with the arguments captured in stabilization cycle 2.
// At 8ec94b6 this edit applied with both arguments rewritten.
func TestTheCapturedPrefixedEditIsRefusedNotRewritten(t *testing.T) {
	raw, err := os.ReadFile("testdata/captured_prefixed_edit.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct{ OldStr, NewStr, Excerpt string }
	if err := json.Unmarshal(raw, &struct {
		OldStr  *string `json:"old_str"`
		NewStr  *string `json:"new_str"`
		Excerpt *string `json:"excerpt"`
	}{&c.OldStr, &c.NewStr, &c.Excerpt}); err != nil {
		t.Fatal(err)
	}
	ctx, path := exactEditWorld(t, "executor_server.py", c.Excerpt)
	res := exactEditCall(t, ctx, "edit_file", map[string]interface{}{"path": "executor_server.py",
		"old_str": c.OldStr, "new_str": c.NewStr})
	if res.Success {
		t.Fatal("the captured prefixed edit was applied")
	}
	if diskBytes(t, path) != c.Excerpt {
		t.Fatal("the file changed")
	}
	for _, want := range []string{"line 6 of executor_server.py", "new_str also has the prefix on 18 line(s)",
		"start_line 6, end_line 6", `expected_first_line "elif lang == \"php\":"`} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("refusal lacks %q:\n%s", want, res.Error)
		}
	}
}

func TestAPrefixFreeMatchInSeveralPlacesSaysItIsNotUnique(t *testing.T) {
	file := "x = 1\ny = 2\nx = 1\n"
	ctx, path := exactEditWorld(t, "d.py", file)
	res := exactEditCall(t, ctx, "edit_file", map[string]interface{}{"path": "d.py", "old_str": "1\tx = 1", "new_str": "x = 3"})
	if res.Success || diskBytes(t, path) != file {
		t.Fatal("an ambiguous prefixed edit was applied")
	}
	if !strings.Contains(res.Error, "matches 2 places") || strings.Contains(res.Error, "start_line") {
		t.Errorf("ambiguity not stated, or a range was offered anyway:\n%s", res.Error)
	}
}

func TestRelaxedMatchesApplyAndAreReported(t *testing.T) {
	t.Run("quote style", func(t *testing.T) {
		file := "msg = \"hello\"\nother = 1\n"
		ctx, path := exactEditWorld(t, "q.py", file)
		res := exactEditCall(t, ctx, "edit_file", map[string]interface{}{"path": "q.py",
			"old_str": "msg = “hello”", "new_str": "msg = \"bye\""})
		if !res.Success {
			t.Fatalf("refused: %s", res.Error)
		}
		if got := diskBytes(t, path); got != "msg = \"bye\"\nother = 1\n" {
			t.Errorf("bytes: %q", got)
		}
		if !strings.Contains(string(res.Data), "curly and straight quotes") {
			t.Errorf("the quote-tolerant match was not reported: %s", res.Data)
		}
	})
	t.Run("whitespace", func(t *testing.T) {
		file := "def f():\n    if x:\n        return 1\n    return 2\n"
		ctx, path := exactEditWorld(t, "w.py", file)
		res := exactEditCall(t, ctx, "edit_file", map[string]interface{}{"path": "w.py",
			"old_str": "if x:\n  return 1", "new_str": "    if x:\n        return 3"})
		if !res.Success {
			t.Fatalf("refused: %s", res.Error)
		}
		if got := diskBytes(t, path); got != "def f():\n    if x:\n        return 3\n    return 2\n" {
			t.Errorf("bytes: %q", got)
		}
		if !strings.Contains(string(res.Data), "ignoring leading and trailing whitespace") ||
			!strings.Contains(string(res.Data), "lines 2-3") {
			t.Errorf("the whitespace-tolerant match was not reported with its lines: %s", res.Data)
		}
	})
}

// Through the agent loop: a prefixed edit is refused, the model makes the
// replace_lines call the refusal names, and the change lands exactly.
func TestAPrefixedEditRecoversThroughTheLoop(t *testing.T) {
	file := "def area(r):\n    return 3.14 * r * r\n"
	run := integrityLoop(t, "Use math.pi in area().", map[string]string{"geo.py": file}, []string{
		toolCall("read_file", map[string]interface{}{"path": "geo.py"}),
		toolCall("edit_file", map[string]interface{}{"path": "geo.py",
			"old_str": "2\t    return 3.14 * r * r", "new_str": "2\t    return math.pi * r * r"}),
		toolCall("replace_lines", map[string]interface{}{"path": "geo.py", "start_line": 2, "end_line": 2,
			"expected_first_line": "return 3.14 * r * r", "expected_last_line": "return 3.14 * r * r",
			"content": "    return math.pi * r * r\n"}),
	}, nil)
	if !run.told("start_line 2, end_line 2") {
		t.Error("the refusal the model saw did not name the replace_lines range")
	}
	if got := func() string { s, _ := run.disk(t, "geo.py"); return s }(); got != "def area(r):\n    return math.pi * r * r\n" {
		t.Errorf("bytes after recovery: %q", got)
	}
}

// A no-op edit is refused with the span's location as it stands now.
//
// Measured (cycle 7 refusal audit, family O): edit_file refused at turns 11,
// 14 and 16 for identical old_str/new_str, against a file last read at turn 5
// and edited successfully several times since, alternating with a
// structural_edit that changed nothing, until the run stopped on repeated
// refusals. The refusal sent the run to `replace_lines` "with the line numbers
// read_file printed" — numbers that were by then stale. Where those bytes sit
// is something the call already knows.
func TestANoOpEditIsRefusedWithTheSpansCurrentLines(t *testing.T) {
	file := "def area(r):\n    return 3.14 * r * r\n\n\ndef perimeter(r):\n    return 2 * 3.14 * r\n"
	ctx, path := exactEditWorld(t, "geo.py", file)
	same := "    return 2 * 3.14 * r"
	res := exactEditCall(t, ctx, "edit_file", map[string]interface{}{"path": "geo.py",
		"old_str": same, "new_str": same})
	if res.Success {
		t.Fatal("a no-op edit was applied")
	}
	for _, want := range []string{"identical", "line 6", "start_line 6, end_line 6",
		`expected_first_line "return 2 * 3.14 * r"`} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("refusal lacks %q:\n%s", want, res.Error)
		}
	}
	// The call it names lands.
	retry := exactEditCall(t, ctx, "replace_lines", map[string]interface{}{"path": "geo.py",
		"start_line": 6, "end_line": 6,
		"expected_first_line": "return 2 * 3.14 * r", "expected_last_line": "return 2 * 3.14 * r",
		"content": "    return 2 * math.pi * r\n"})
	if !retry.Success {
		t.Fatalf("the named replace_lines call failed: %s", retry.Error)
	}
	if got := diskBytes(t, path); !strings.Contains(got, "math.pi") {
		t.Errorf("bytes after the retry: %q", got)
	}
}

// Text that is not in the file keeps the existing not-found refusal, which
// already quotes the closest real line — and no line numbers are invented for
// a span that is not there.
func TestANoOpEditOnTextThatIsGoneSaysSo(t *testing.T) {
	ctx, _ := exactEditWorld(t, "geo.py", "def area(r):\n    return 3.14 * r * r\n")
	gone := "    return 2 * 3.14 * r"
	res := exactEditCall(t, ctx, "edit_file", map[string]interface{}{"path": "geo.py",
		"old_str": gone, "new_str": gone})
	if res.Success {
		t.Fatal("an edit against absent text was applied")
	}
	if !strings.Contains(res.Error, "string to replace not found") || strings.Contains(res.Error, "start_line") {
		t.Errorf("refusal should be the not-found one, with no invented range:\n%s", res.Error)
	}
	// The note itself, asked directly about text the file does not hold, says
	// the numbers are stale rather than guessing.
	note := currentSpanNote("geo.py", "def area(r):\n", "    return 2 * 3.14 * r")
	if !strings.Contains(note, "not in geo.py as it stands now") || strings.Contains(note, "start_line") {
		t.Errorf("absent-span note: %s", note)
	}
}
