package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// A run that did not finish cleanly says what happened and what is on disk,
// and the model's own claim never stands as the outcome.
func TestTheSummariesSayWhatHappened(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier0Conversational)
	ctx.RecordFileRead(filepath.Join(dir, "index.html"), "<html></html>")
	ctx.RecordFileRead(filepath.Join(dir, "script.js"), "const f = 1;")
	claim := "I updated the snake game logic and verified that the page loads."
	contextFull := errors.New(`LLM returned 400: {"code":400,"message":"request (33012 tokens) ` +
		`exceeds the available context size (32768 tokens)","type":"exceed_context_size_error"}`)
	code := "elif lang == \"toml\":\n    fpath.write_text(code)"
	salvaged := "def solve():\n    grid = [line.strip() for line in f]\n    # Wait, if"
	cutShort := salvaged + "\n\n(The reply was cut short — it had begun repeating itself. Ask again if something is missing.)"
	for _, c := range []struct {
		name, summary string
		prefix        string // the summary must start with it, so it reads as no success
		wants         []string
	}{
		{"an unverified claim over written changes", unverifiedSummary(true, claim), "",
			[]string{"NOTHING in this run verified", "Run it yourself", "UNVERIFIED", claim}},
		{"an unverified claim with nothing written", unverifiedSummary(false, claim), "", []string{"Nothing was written to disk"}},
		{"out of turns, with nothing written", outOfTurnsSummary(ctx, false), "",
			[]string{"ran out of turns", "Nothing was written", "index.html", "script.js", "narrower request"}},
		{"out of turns, after a write", outOfTurnsSummary(ctx, true), "", []string{"written to disk"}},
		{"the context window overflowed", inferenceFailureSummary(contextFull, false), "",
			[]string{"outgrew the model's context window", "large file was read", "Nothing was written"}},
		{"another model-call failure, after a write", inferenceFailureSummary(errors.New("connection refused"), true), "",
			[]string{"model call failed", "connection refused", "are on disk"}},
		{"a model-call failure with no error", inferenceFailureSummary(nil, false), "", []string{"model call failed", "Nothing was written"}},
		{"nothing written, and the model said nothing", nothingWrittenSummary(""), "Nothing was written", nil},
		{"nothing written, and the model sent only whitespace", nothingWrittenSummary("   \n  "), "Nothing was written", nil},
		{"nothing written, and the model sent code", nothingWrittenSummary(code), "Nothing was written",
			[]string{code, "proposal, not something on disk"}},
		{"nothing written, and the reply was cut short", nothingWrittenSummary(cutShort), "Nothing was written",
			[]string{salvaged, "cut short"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !strings.HasPrefix(c.summary, c.prefix) {
				t.Errorf("summary does not start with %q:\n%s", c.prefix, c.summary)
			}
			for _, want := range c.wants {
				if !strings.Contains(c.summary, want) {
					t.Errorf("summary lacks %q:\n%s", want, c.summary)
				}
			}
		})
	}
}

// A completed run that looked into the project and changed nothing says so
// under its summary. A change by a tool or the shell, a shell effect the run
// could not observe, a run that never looked, or one that did not complete,
// leaves the summary as it is.
func TestACompletedRunThatChangedNothingSaysSo(t *testing.T) {
	for _, c := range []struct {
		name       string
		st         *runState
		ledger     map[string]*DeliverableState // nil: nothing recorded
		unobserved bool
		status     TerminalStatus
		summary    string
		want       string
	}{
		{name: "it changed nothing", st: &runState{inspectedWorkspace: true}, status: TerminalCompleted,
			summary: "Updated calc.py.", want: "Updated calc.py.\n\n" + noChangeNote},
		{name: "it changed nothing and said nothing", st: &runState{inspectedWorkspace: true}, status: TerminalCompleted,
			summary: "", want: noChangeNote},
		{name: "it never looked at the project", st: &runState{}, status: TerminalCompleted,
			summary: "Done.", want: "Done."},
		{name: "a tool wrote a file", st: &runState{inspectedWorkspace: true, madeProductiveChange: true}, status: TerminalCompleted,
			summary: "Done.", want: "Done."},
		{name: "the shell wrote a file", st: &runState{inspectedWorkspace: true}, status: TerminalCompleted,
			ledger: map[string]*DeliverableState{"out.txt": {Generation: 1}}, summary: "Done.", want: "Done."},
		{name: "the shell removed a file", st: &runState{inspectedWorkspace: true}, status: TerminalCompleted,
			ledger: map[string]*DeliverableState{"old.txt": {Tombstoned: true}}, summary: "Done.", want: "Done."},
		{name: "shell effects went unobserved", st: &runState{inspectedWorkspace: true}, status: TerminalCompleted,
			unobserved: true, summary: "Done.", want: "Done."},
		{name: "the run did not complete", st: &runState{inspectedWorkspace: true}, status: TerminalIncomplete,
			summary: "Done.", want: "Done."},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := NewAgentContext(t.TempDir(), Tier0Conversational)
			ctx.Ledger = c.ledger
			ctx.ShellEffectsUnobserved = c.unobserved
			if got := withNoChangeNote(ctx, c.st, c.status, c.summary); got != c.want {
				t.Errorf("summary became %q, want %q", got, c.want)
			}
		})
	}
}

// appendLastReadRestatementFor is in agent.go. It adds the last file read to the
// end of the wire after the token budget was spent, which is how a large
// input overflowed the context window (the 400 inferenceFailureSummary
// names). So it yields when the slot is nearly full, and it never adds a
// second copy of content already in the window.
func TestTheLastReadIsRestatedOnlyWhenItFitsAndIsNew(t *testing.T) {
	// 2000 short lines, about 8.7 KB: under restatementMaxBytes, so the room
	// left in the slot is what decides.
	input := strings.Repeat("159\n", 2000)
	for _, c := range []struct {
		name  string
		read  string
		wire  []map[string]string
		added int // messages the restatement adds: 1, or 0 when it yields
	}{
		{"a wire close to the slot limit", input,
			[]map[string]string{{"role": "user", "content": strings.Repeat("x", perSlotContext()*4)}}, 0},
		{"a short wire", input, []map[string]string{{"role": "user", "content": "fix the bug"}}, 1},
		{"the content already earlier in the wire", "alpha\nbravo\n", []map[string]string{
			{"role": "user", "content": "here is the file: alpha\nbravo\n"},
			{"role": "assistant", "content": "ok"},
			{"role": "user", "content": "now fix it"},
		}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.RecordFileRead(filepath.Join(dir, "input.txt"), c.read)
			got := appendLastReadRestatementFor(ctx, c.wire, "")
			if len(got) != len(c.wire)+c.added {
				t.Errorf("%d messages from %d, want %d", len(got), len(c.wire), len(c.wire)+c.added)
			}
		})
	}
}
