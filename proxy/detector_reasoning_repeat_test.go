package main

import (
	"strings"
	"testing"
)

// The reasoning-repetition detector compares the normalized opening of each
// turn's reasoning. Two consecutive repeats (three turns with the same
// opening) trigger an intervention. A single repeat, or a turn with no
// reasoning, does not.

func TestRecordReasoningTriggersOnConsecutiveRepeat(t *testing.T) {
	ctx := &AgentContext{}
	// Turn 1: first reasoning. No intervention.
	if msg, _, fired := recordReasoning(ctx, "Now I need to read the file to understand the structure."); fired || msg != "" {
		t.Fatalf("turn 1: expected no fire, got fired=%v msg=%q", fired, msg)
	}
	// Turn 2: same opening prefix. count=1 (not yet at threshold of 2).
	if msg, _, fired := recordReasoning(ctx, "Now I need to read the file to understand the structure."); fired || msg != "" {
		t.Fatalf("turn 2: expected no fire (count=1, threshold=2), got fired=%v msg=%q", fired, msg)
	}
	// Turn 3: same opening prefix again. count=2. FIRES.
	msg, obs, fired := recordReasoning(ctx, "Now I need to read the file to understand the structure.")
	if !fired {
		t.Fatalf("turn 3: expected intervention, got no fire")
	}
	if !strings.Contains(msg, "Reasoning repetition") {
		t.Errorf("intervention message missing canonical prefix: %s", msg)
	}
	if !strings.Contains(msg, "3 consecutive turns") {
		t.Errorf("intervention should report 3 consecutive turns, got: %s", msg)
	}
	// The observation is what the caller renders its log line and its
	// agent_reasoning_intervention payload from, so it must carry the
	// same count the message reports and the snippet that repeated.
	if obs.Count != 3 {
		t.Errorf("observation count = %d, want 3 consecutive turns", obs.Count)
	}
	if obs.Snippet != normalizeReasoningSnippet("Now I need to read the file to understand the structure.") {
		t.Errorf("observation snippet = %q, want the normalized repeated opening", obs.Snippet)
	}
	// The detector owns the reset. Reading the streak back off ctx now
	// sees the cleared values — which is exactly why the observation is
	// returned instead.
	if ctx.ConsecutiveReasoningRepeats != 0 || ctx.LastReasoningSnippet != "" {
		t.Errorf("firing must clear the streak; got repeats=%d snippet=%q",
			ctx.ConsecutiveReasoningRepeats, ctx.LastReasoningSnippet)
	}
	// A fourth identical turn starts over rather than re-firing the
	// same loop immediately.
	if _, _, again := recordReasoning(ctx, "Now I need to read the file to understand the structure."); again {
		t.Error("the turn right after a fire must start a fresh streak")
	}
}

func TestRecordReasoningResetOnDivergence(t *testing.T) {
	ctx := &AgentContext{}
	recordReasoning(ctx, "Now I need to read the file.")
	recordReasoning(ctx, "Now I need to read the file.")
	// Turn 3: model commits to a different thought — counter resets.
	if _, _, fired := recordReasoning(ctx, "I have the file content. Now let me write the new version."); fired {
		t.Error("divergent reasoning should reset the counter, no intervention expected")
	}
	if ctx.ConsecutiveReasoningRepeats != 0 {
		t.Errorf("counter should reset to 0 after divergence, got %d", ctx.ConsecutiveReasoningRepeats)
	}
	// Turn 4: similar to turn 3 (the new pattern). count=1.
	if _, _, fired := recordReasoning(ctx, "I have the file content. Now let me write the new version."); fired {
		t.Error("turn 4 should be count=1 (one repeat), no fire yet")
	}
	// Turn 5: third identical → FIRES.
	if _, _, fired := recordReasoning(ctx, "I have the file content. Now let me write the new version."); !fired {
		t.Error("turn 5 should fire (count=2 of new pattern)")
	}
}

func TestRecordReasoningIgnoresEmptyTurns(t *testing.T) {
	ctx := &AgentContext{}
	recordReasoning(ctx, "Now I need to read the file.")
	// Turn 2: empty reasoning (model committed straight to action). Counter resets.
	if _, _, fired := recordReasoning(ctx, ""); fired {
		t.Error("empty reasoning should not fire")
	}
	if ctx.ConsecutiveReasoningRepeats != 0 || ctx.LastReasoningSnippet != "" {
		t.Errorf("empty reasoning should reset state; got repeats=%d snippet=%q",
			ctx.ConsecutiveReasoningRepeats, ctx.LastReasoningSnippet)
	}
	// Turn 3: same prose as turn 1, but the empty turn 2 broke the streak.
	// Should be treated as a fresh start, not count=2.
	recordReasoning(ctx, "Now I need to read the file.")
	if ctx.ConsecutiveReasoningRepeats != 0 {
		t.Errorf("post-empty: counter should be 0 (fresh start), got %d", ctx.ConsecutiveReasoningRepeats)
	}
}

// Openings that differ only in case and whitespace, or only after the first
// 80 characters, count as the same opening.
func TestRecordReasoningCountsTheSameOpeningAsARepeat(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second string
	}{
		{"case and whitespace differ", "  Now I  need\nto    read the file.\n", "now i need to read the file."},
		{
			"the text differs after the first 80 characters",
			"Looking at the existing dashboard.html, I see the basic Flask template that needs to be transformed into a metrics view.",
			"Looking at the existing dashboard.html, I see the basic Flask template that needs to be expanded with three KPI cards.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &AgentContext{}
			recordReasoning(ctx, tc.first)
			recordReasoning(ctx, tc.second)
			if ctx.ConsecutiveReasoningRepeats != 1 {
				t.Errorf("got count=%d, want 1; snippet=%q",
					ctx.ConsecutiveReasoningRepeats, ctx.LastReasoningSnippet)
			}
		})
	}
}

func TestNormalizeReasoningSnippet(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   \n  ", ""},
		{"Hello World", "hello world"},
		{"  HELLO\n\tWORLD  ", "hello world"},
		{strings.Repeat("a", 200), strings.Repeat("a", 80)},
	}
	for _, tc := range cases {
		if got := normalizeReasoningSnippet(tc.in); got != tc.want {
			t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
