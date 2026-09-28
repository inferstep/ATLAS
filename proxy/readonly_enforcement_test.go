package main

import ("encoding/json"; "os"; "path/filepath"; "testing")

// The guarantee that matters: a mutating call under a read-only request must
// leave the file on disk UNCHANGED, proven through executeToolCall (the real
// dispatch), not through the helper.
func TestReadOnlyRequestLeavesTheFileUntouchedThroughRealDispatch(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "orders.py")
	const original = "def apply_discount(total, pct):\n    return total * (1 - pct/100)\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil { t.Fatal(err) }

	ctx := NewAgentContext(dir, Tier0Conversational)
	ctx.HumanTask = "In orders.py, what does apply_discount do? Just explain — do not change any code."

	args, _ := json.Marshal(WriteFileInput{Path: "orders.py", Content: "CLOBBERED\n"})
	res := executeToolCall("write_file", args, ctx)
	if res.Success {
		t.Error("write_file SUCCEEDED under an explicit read-only request")
	}
	if res.MutationStatus != MutationNone {
		t.Errorf("MutationStatus = %v, want MutationNone", res.MutationStatus)
	}
	got, _ := os.ReadFile(target)
	if string(got) != original {
		t.Fatalf("the file was modified under a read-only request:\n%q", string(got))
	}
	// Indirect path: a shell command that would rewrite the file in place.
	sh, _ := json.Marshal(RunCommandInput{Command: "sed -i 's/return/#return/' orders.py"})
	res2 := executeToolCall("run_command", sh, ctx)
	if res2.Success {
		t.Error("run_command SUCCEEDED under an explicit read-only request")
	}
	got2, _ := os.ReadFile(target)
	if string(got2) != original {
		t.Fatalf("the file was modified by shell under a read-only request:\n%q", string(got2))
	}
	// Reading must still work, or the model cannot answer.
	rd, _ := json.Marshal(ReadFileInput{Path: "orders.py"})
	if r := executeToolCall("read_file", rd, ctx); !r.Success {
		t.Errorf("read_file was refused under a read-only request: %s", r.Error)
	}
}
