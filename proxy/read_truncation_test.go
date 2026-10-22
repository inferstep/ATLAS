package main

import (
	"strings"
	"testing"
)

// The truncation notice must not steer a data file toward shell pipelines.
// For a data file the program is meant to open at runtime, advice to process
// it with run_command (grep, awk, sed) points at pipelines and stdin, and the
// caller then runs `python solve.py` with no stdin and gets nothing.
func TestTruncationNoticeDoesNotSteerTowardPipelines(t *testing.T) {
	// A DATA file (the last argument false): start 0, 1131 lines shown of
	// 2001, 8707 bytes.
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
