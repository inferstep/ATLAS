package app

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A tool call whose arguments are not under "args" gets them lifted from
// where the model put them; one it cannot place keeps none.
func TestToolCallArgumentsAreLiftedFromWhereTheModelPutThem(t *testing.T) {
	for _, tc := range []struct {
		name, raw, wantName, wantArgs string
	}{
		{"the tool's name as the type", `{"type":"read_file","path":"a.py"}`, "read_file", `{"path":"a.py"}`},
		{"null args", `{"type":"tool_call","name":"read_file","args":null,"path":"a.py"}`, "read_file", `{"path":"a.py"}`},
		{"under input", `{"type":"tool_call","name":"read_file","input":{"path":"a.py"}}`, "read_file", `{"path":"a.py"}`},
		{"a null alt key is not the args", `{"type":"tool_call","name":"read_file","arguments":null,"path":"a.py"}`,
			"read_file", `{"arguments":null,"path":"a.py"}`},
		{"the envelope keys stay out", `{"type":"tool_call","name":"read_file","summary":"s","content":"c","path":"a.py"}`,
			"read_file", `{"path":"a.py"}`},
		{"nothing to lift", `{"type":"tool_call","name":"read_file"}`, "read_file", ""},
		{"no name, nothing lifted", `{"type":"tool_call","path":"a.py"}`, "", ""},
	} {
		resp, err := extractModelResponse(tc.raw)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if resp.Name != tc.wantName {
			t.Errorf("%s: name %q, want %q", tc.name, resp.Name, tc.wantName)
		}
		if string(resp.Args) != tc.wantArgs {
			t.Errorf("%s: args %s, want %s", tc.name, resp.Args, tc.wantArgs)
		}
	}
}

// Each kind of reply that does not parse gets its own category.
func TestAReplyThatDoesNotParseIsNamedByItsShape(t *testing.T) {
	for _, tc := range []struct {
		name, raw, category string
	}{
		{"an entity-encoded tool call", `{"type":"tool_call","name":"write_file","args":{"content":"a &gt; b"` + "\n", "html_entities"},
		{"an entity-encoded call with a spaced type", `note {"type": "tool_call","args":{"content":"&amp;"}`, "html_entities"},
		{"a cut call with a spaced start", `{ "type": "tool_call", "name": "write_file", "args": {"content": "x`, "truncated_tool"},
		{"a cut call with a spaced colon", `{"type": "tool_call", "name": "write_file", "args": {"content": "x`, "truncated_tool"},
		{"a call ending in a bracket", `{"type":"tool_call","name":"write_file","args":{"items":[1,`, "truncated_tool"},
		{"a malformed call ending in a list", `{"type":"tool_call","name":"x","args":{"items":[1,]]`, "malformed_tool"},
		{"entities in prose without a tool call", `use a &lt;div&gt; here`, "prose"},
	} {
		if category, _ := classifyParseFailure(tc.raw, ""); category != tc.category {
			t.Errorf("%s: %q, want %q", tc.name, category, tc.category)
		}
	}
}

func TestCancelledModelPhaseDoesNotCallInference(t *testing.T) {
	calls := 0
	server := stubInference(t, []string{`{"type":"done","summary":"done"}`}, 1, &calls)
	defer server.Close()
	ctx := NewAgentContext(t.TempDir(), Tier0Conversational)
	ctx.InferenceURL = server.URL
	ctx.Ctx, ctx.cancelWork = context.WithCancel(context.Background())
	ctx.cancelWork()
	loop := &agentLoop{ctx: ctx, st: &runState{}}
	check := modelCheckInput{loop: loop}
	result := runCheckPhase(proxyChecks.modelStart, &check)
	if result != (checkResult{checkInfrastructureError, runEnds}) || check.err != nil || ctx.TerminalStatus != "incomplete" || ctx.TerminalReason != "cancelled" {
		t.Fatalf("cancelled phase = %+v, %v", result, check.err)
	}
	response, parsed, outcome, err := loop.takeModelTurn(0)
	if response != "" || parsed.Type != "" || outcome != runEnds || err != nil {
		t.Fatalf("cancelled turn = %q, %+v, %v, %v", response, parsed, outcome, err)
	}
	if calls != 0 {
		t.Errorf("cancelled turn made %d inference calls", calls)
	}
}

func TestFencedFetchCategoriesPreserveTheErrorAndFailureCharge(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier0Conversational)
	ctx.FencedFailures = map[string]int{fencedKey(ctx, "note.txt"): maxFencedFailuresPerPath}
	_, err := fetchFencedContent(ctx, "call", "note.txt")
	if err == nil || fencedErrorCategory(err) != checkGuardRefusal {
		t.Fatalf("spent allowance category = %v", err)
	}
	if ctx.FencedFailures[fencedKey(ctx, "note.txt")] != maxFencedFailuresPerPath {
		t.Fatal("admission charged another attempt")
	}

	ctx.Ctx, ctx.cancelWork = context.WithCancel(context.Background())
	ctx.cancelWork()
	fetch := fencedFetch{ctx: ctx, path: "cancelled.txt"}
	_, done, err := fetch.settle(fencedAnswer{err: context.Canceled}, 0)
	if !done || fencedErrorCategory(err) != checkInfrastructureError || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled fetch = %v, %v", done, err)
	}
	if ctx.FencedFailures[fencedKey(ctx, "cancelled.txt")] != 1 {
		t.Fatal("cancelled attempt was not charged once")
	}

	calls := 0
	server := stubInference(t, []string{"no block", "still no block"}, 1, &calls)
	defer server.Close()
	ctx = NewAgentContext(t.TempDir(), Tier0Conversational)
	ctx.InferenceURL = server.URL
	_, err = fetchFencedContent(ctx, "call", "note.txt")
	if err == nil || fencedErrorCategory(err) != checkModelError || !strings.HasPrefix(err.Error(), "no fenced block after 2 attempt(s)") {
		t.Fatalf("unusable model reply = %v", err)
	}
	if calls != 2 || ctx.FencedFailures[fencedKey(ctx, "note.txt")] != 2 {
		t.Fatalf("attempts=%d, failures=%v", calls, ctx.FencedFailures)
	}
}
