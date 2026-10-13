package main

import (
	"strings"
	"testing"
)

// add_function rep 2 (smoke run 2026-09-28): the model re-sent the test file
// it had just written, three times, and was told each time that it did not
// need to reproduce input data. The test never ran, and passing work ended
// "stopped". For the session's own file the refusal says what happened and
// what comes next.

// ownTestFile is long enough (over 200 bytes) to count as an echo.
const ownTestFile = "from stats import mean, median\n\n\ndef test_mean():\n" +
	"    assert mean([1, 2, 3]) == 2.0\n    assert mean([1, 2, 3, 4]) == 2.5\n\n\n" +
	"def test_median():\n    assert median([1, 3, 2]) == 2\n    assert median([1, 2, 3, 4]) == 2.5\n" +
	"    assert median([1]) == 1\n\n\nif __name__ == '__main__':\n    test_mean()\n    test_median()\n"

func TestAnEchoOfTheSessionsOwnFileSaysRunIt(t *testing.T) {
	ctx, _ := exactEditWorld(t, "test_stats.py", ownTestFile)
	ctx.SessionWrites["test_stats.py"] = true
	res := exactEditCall(t, ctx, "write_file", map[string]interface{}{
		"path": "test_stats.py", "content": ownTestFile})
	if res.Success {
		t.Fatal("an echoed write was applied")
	}
	for _, want := range []string{"already holds exactly this content",
		"You wrote it earlier in this session", "run it"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("refusal lacks %q:\n%s", want, res.Error)
		}
	}
	if strings.Contains(res.Error, "input or fixture data") {
		t.Errorf("the session's own file was treated as fixture data:\n%s", res.Error)
	}
}

func TestAnEchoOfAFileTheSessionDidNotWriteKeepsTheFixtureWording(t *testing.T) {
	ctx, _ := exactEditWorld(t, "test_stats.py", ownTestFile)
	res := exactEditCall(t, ctx, "write_file", map[string]interface{}{
		"path": "test_stats.py", "content": ownTestFile})
	if res.Success || !strings.Contains(res.Error, "input or fixture data") {
		t.Errorf("want the fixture refusal, got success=%v:\n%s", res.Success, res.Error)
	}
}

// offbyone (ccc71fc smoke runs, 2026-09-29): the test file the session wrote,
// recorded byte for byte from its fenced fetch. It is 76 bytes, under the
// 200-byte echo floor, so each of five identical re-sends was written again,
// the file never ran, and passing work ended stopped/repeat_detector.
const smallOwnTestFile = "from chunk import chunks\n\nresult = chunks([1, 2, 3, 4, 5], 2)\nprint(result)\n"

func TestASmallReSendOfTheSessionsOwnFileSaysRunIt(t *testing.T) {
	ctx, path := exactEditWorld(t, "test_chunk.py", smallOwnTestFile)
	ctx.SessionWrites["test_chunk.py"] = true
	res := exactEditCall(t, ctx, "write_file", map[string]interface{}{
		"path": "test_chunk.py", "content": smallOwnTestFile})
	if res.Success {
		t.Fatal("a re-send of the session's own 76-byte file was written again")
	}
	for _, want := range []string{"already holds exactly this content",
		"You wrote it earlier in this session", "run it"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("refusal lacks %q:\n%s", want, res.Error)
		}
	}
	if got := diskBytes(t, path); got != smallOwnTestFile {
		t.Errorf("the refusal changed the file:\n%q", got)
	}
}

func TestAChangedSmallWriteOfTheSessionsOwnFileLands(t *testing.T) {
	ctx, path := exactEditWorld(t, "test_chunk.py", smallOwnTestFile)
	ctx.SessionWrites["test_chunk.py"] = true
	changed := strings.Replace(smallOwnTestFile, "print(result)", "assert result[-1] == [5]\nprint(result)", 1)
	res := exactEditCall(t, ctx, "write_file", map[string]interface{}{
		"path": "test_chunk.py", "content": changed})
	if !res.Success {
		t.Fatalf("a changed write of the session's own file was refused: %s", res.Error)
	}
	if got := diskBytes(t, path); got != changed {
		t.Errorf("disk holds %q, want the changed content", got)
	}
}

func TestASmallFileTheSessionDidNotWriteIsNotRefusedAsAnEcho(t *testing.T) {
	// The floor stays for files the session did not write: a short file sent
	// back unchanged proves nothing about retyping a fixture.
	ctx, _ := exactEditWorld(t, "test_chunk.py", smallOwnTestFile)
	res := exactEditCall(t, ctx, "write_file", map[string]interface{}{
		"path": "test_chunk.py", "content": smallOwnTestFile})
	if !res.Success && strings.Contains(res.Error, "already holds exactly this content") {
		t.Errorf("a small file the session did not write was refused as an echo:\n%s", res.Error)
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
