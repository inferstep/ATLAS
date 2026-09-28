package main

import (
	"os"
	"strings"
	"testing"
)

// A tool_call is streamed the moment it parses, before permission and
// execution. Every exit between that point and the tool_result has to answer
// the call it announced, or the client is left rendering a spinner for a call
// that never resolves.
//
// Measured 2026-08-03 on multiturn_stats: the repetition breaker stopped the
// session one line after announcing a call, and the stream carried 12
// tool_call events against 11 tool_result. endStream exists so the invariant
// holds at every exit rather than at each one that remembered to; this test
// keeps a new exit from quietly reintroducing the gap.
func TestEveryExitAfterAToolCallAnswersIt(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatalf("read agent.go: %v", err)
	}
	lines := strings.Split(string(src), "\n")

	open, close := -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, "pendingToolCall = parsed.Name") {
			open = i
		}
		if strings.Contains(ln, `pendingToolCall = ""`) && i > open && open != -1 {
			close = i
		}
	}
	if open == -1 || close == -1 {
		t.Fatal("could not locate the tool_call announcement and its result; " +
			"if this moved, move the invariant with it")
	}

	for i := open; i < close; i++ {
		if strings.Contains(lines[i], `ctx.Stream("done"`) {
			t.Errorf("agent.go:%d ends the stream between announcing a tool_call "+
				"and answering it, leaving an orphaned call. Use endStream(summary) "+
				"instead of ctx.Stream(\"done\", ...):\n\t%s",
				i+1, strings.TrimSpace(lines[i]))
		}
	}
}
