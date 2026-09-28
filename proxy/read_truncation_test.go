package main

import (
	"strings"
	"testing"
)

// The truncation notice must not steer a data file toward shell pipelines.
//
// Its old wording told the model to "process it with run_command
// (grep/awk/sed/head, or a python script) instead of loading it all into
// context". For a large source file that is right. For a data file the
// program is meant to open at runtime it is not: it points at pipelines and
// stdin, and the caller then runs `python solve.py` with no stdin and gets 0.
//
// Measured on the AoC tasks, whose answer is computed from input.txt:
//
//	shoal   1 line,    600 B  -> never truncated -> 92%
//	slope   400 lines, 12800 B -> truncated      -> 50%
//	course  1200 lines, 9707 B -> truncated      -> 27%
//	sonar   2000 lines, 8707 B -> truncated      -> 27%
//
// The same model prompted directly never reads the file and scored 83-100%.
func TestTruncationNoticeDoesNotSteerTowardPipelines(t *testing.T) {
	// A DATA file: start 0, 1131 lines shown of 2001, 8707 bytes. The data
	// advice is the whole point of this test and must survive the source/data
	// split added for smallrung_toml.
	notice := readFileTruncationNotice(0, 1131, 2001, 8707, false)
	for _, banned := range []string{"grep", "awk", "sed", "run_command"} {
		if strings.Contains(notice, banned) {
			t.Errorf("truncation notice must not suggest %q: %s", banned, notice)
		}
	}
	if !strings.Contains(notice, "open it") {
		t.Errorf("it should tell the model its program can open the file: %s", notice)
	}
	if !strings.Contains(notice, "offset/limit") {
		t.Errorf("inspecting another range is still the right advice: %s", notice)
	}
}

func TestTruncationNoticeStatesWhatWasShown(t *testing.T) {
	notice := readFileTruncationNotice(0, 255, 401, 12800, false)
	for _, want := range []string{"255", "401", "12800"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice should report %s: %s", want, notice)
		}
	}
	// And it must state the WINDOW, not "the first N": an offset read that
	// called itself "the first" made pagination look broken to the model.
	off := readFileTruncationNotice(214, 225, 2026, 15000, false)
	if !strings.Contains(off, "215-439") {
		t.Errorf("offset read must report lines 215-439: %s", off)
	}
	if strings.Contains(off, "the first") {
		t.Errorf("offset read must not call itself 'the first': %s", off)
	}
}
