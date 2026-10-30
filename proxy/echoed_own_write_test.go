package main

import (
	"strings"
	"testing"
)

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
