package app

import "testing"

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
