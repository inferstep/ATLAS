package main

import "testing"

// Some chat templates drop role:"tool" messages, so the model never sees a
// tool result and loops re-issuing the call. Tool results go on the wire as a
// user turn with a [tool result] marker; other roles pass through unchanged.
func TestToWireMessagesRendersToolAsUser(t *testing.T) {
	in := []AgentMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "list files"},
		{Role: "assistant", Content: `{"type":"tool_call","name":"list_directory"}`},
		{Role: "tool", ToolName: "list_directory", Content: `{"entries":["app.py","static"]}`},
	}
	out := toWireMessages(in)
	if len(out) != len(in) {
		t.Fatalf("len mismatch: %d != %d", len(out), len(in))
	}
	// system / user / assistant pass through untouched.
	if out[0]["role"] != "system" || out[1]["role"] != "user" || out[2]["role"] != "assistant" {
		t.Errorf("non-tool roles altered: %+v", out[:3])
	}
	if out[2]["content"] != in[2].Content {
		t.Errorf("assistant content altered: %q", out[2]["content"])
	}
	// tool → user with marker, content preserved after the marker.
	if out[3]["role"] != "user" {
		t.Errorf("tool role not converted to user: %q", out[3]["role"])
	}
	if out[3]["content"] != "[tool result] "+in[3].Content {
		t.Errorf("tool content not marked/preserved: %q", out[3]["content"])
	}
}
