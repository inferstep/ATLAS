package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Repair in progress: a file this session left unparseable stays open
// until it parses, every tool result names it, a done is sent back while it is
// open, and a session that ends with it open hands it to the user with what
// was tried.

const (
	repairBroken  = "def broken(:\n    return 1\n"
	repairBroken2 = "def broken(:\n    return 2\n"
	repairFixed   = "def fixed():\n    return 1\n"
	repairSynErr  = "SyntaxError: invalid syntax (line 1)"
)

type repairEvent struct {
	Type string
	Data map[string]interface{}
}

// repairCtx is a workspace whose syntax check fails any code holding
// "def broken(:", with every streamed event recorded.
func repairCtx(t *testing.T) (*AgentContext, string, *[]repairEvent) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/syntax-check") {
			http.NotFound(w, r)
			return
		}
		var in struct{ Code string }
		json.NewDecoder(r.Body).Decode(&in)
		out := map[string]interface{}{"valid": !strings.Contains(in.Code, "def broken(:")}
		if !out["valid"].(bool) {
			out["errors"] = []string{repairSynErr}
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = srv.URL
	ctx.PermissionMode = PermissionYolo
	events := &[]repairEvent{}
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		var m map[string]interface{}
		json.Unmarshal(b, &m)
		*events = append(*events, repairEvent{et, m})
	}
	return ctx, dir, events
}

func repairArgs(path string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"path": path})
	return b
}

// landedResult is a call that changed the file: with a parse error when
// synErr is set, parsing otherwise.
func landedResult(synErr string) *ToolResult {
	r := &ToolResult{Success: true, MutationStatus: MutationApplied,
		ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed}
	if synErr != "" {
		r.ValidationStatus, r.ValidationDetail = ValidationFailed, synErr
	}
	return r
}

func repairEventsNamed(events []repairEvent) []string {
	var out []string
	for _, e := range events {
		if e.Type == "repair" {
			out = append(out, e.Data["event"].(string))
		}
	}
	return out
}

func writeRepairFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "mod.py"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestARepairOpensOnAnUnparseableWriteAndClosesWhenItParses(t *testing.T) {
	ctx, dir, events := repairCtx(t)
	st := &runState{}

	writeRepairFile(t, dir, repairBroken)
	st.observeRepair(ctx, 1, "write_file", repairArgs("mod.py"), landedResult(repairSynErr))
	note := st.repairNote()
	for _, want := range []string{"mod.py does not parse: " + repairSynErr + ".", "> 1\tdef broken(:", "You can still write other files"} {
		if !strings.Contains(note, want) {
			t.Errorf("the open-repair note lacks %q:\n%s", want, note)
		}
	}

	st.observeRepair(ctx, 2, "edit_file", repairArgs("mod.py"),
		&ToolResult{Error: "old_str not found in mod.py", MutationStatus: MutationRefused})
	st.observeRepair(ctx, 3, "read_file", repairArgs("mod.py"), &ToolResult{Success: true})

	writeRepairFile(t, dir, repairFixed)
	st.observeRepair(ctx, 4, "edit_file", repairArgs("mod.py"), landedResult(""))
	if note := st.repairNote(); note != "" {
		t.Errorf("a repair that parses is still named: %s", note)
	}
	if got := strings.Join(repairEventsNamed(*events), ","); got != "opened,attempt,closed" {
		t.Errorf("repair events = %s, want opened,attempt,closed", got)
	}
}

func TestAnEditThatLeavesAFileBrokenRecordsThatItAlreadyWas(t *testing.T) {
	ctx, dir, events := repairCtx(t)
	st := &runState{}
	writeRepairFile(t, dir, repairBroken)
	st.observeRepair(ctx, 4, "structural_edit", repairArgs("mod.py"), landedResult(repairSynErr))
	if (*events)[0].Data["broken_before"] != true {
		t.Errorf("an edit that left the file unparseable did not record that it already was: %v", (*events)[0].Data)
	}
	if h := repairHandoff(st, "work_deadline"); !strings.Contains(h, "It already did not parse before the edit at turn 5.") {
		t.Errorf("the hand-off does not say the file already failed:\n%s", h)
	}
}

func TestACommandThatChangesTheFileIsSeen(t *testing.T) {
	ctx, dir, events := repairCtx(t)
	st := &runState{}
	cmd, _ := json.Marshal(map[string]string{"command": "sed -i s/x/y/ mod.py"})

	writeRepairFile(t, dir, repairBroken)
	st.observeRepair(ctx, 1, "write_file", repairArgs("mod.py"), landedResult(repairSynErr))
	// Still broken, different bytes: an attempt made by the command.
	writeRepairFile(t, dir, repairBroken2)
	st.observeRepair(ctx, 2, "run_command", cmd, &ToolResult{Success: true})
	// Fixed by a command.
	writeRepairFile(t, dir, repairFixed)
	st.observeRepair(ctx, 3, "run_command", cmd, &ToolResult{Success: true})

	if got := strings.Join(repairEventsNamed(*events), ","); got != "opened,attempt,closed" {
		t.Fatalf("repair events = %s, want opened,attempt,closed", got)
	}
	if tool := (*events)[1].Data["tool"]; tool != "run_command" {
		t.Errorf("the command's attempt is recorded as %v", tool)
	}
	if len(st.openRepairs()) != 0 {
		t.Error("a file a command fixed is still open")
	}
}

func TestADeletedFileClosesItsRepair(t *testing.T) {
	ctx, dir, events := repairCtx(t)
	st := &runState{}
	writeRepairFile(t, dir, repairBroken)
	st.observeRepair(ctx, 1, "write_file", repairArgs("mod.py"), landedResult(repairSynErr))
	os.Remove(filepath.Join(dir, "mod.py"))
	st.observeRepair(ctx, 2, "delete_file", repairArgs("mod.py"),
		&ToolResult{Success: true, MutationStatus: MutationApplied})
	if len(st.openRepairs()) != 0 {
		t.Fatal("a deleted file is still an open repair")
	}
	last := (*events)[len(*events)-1].Data
	if last["event"] != "closed" || last["how"] != "removed" {
		t.Errorf("last repair event = %v, want closed/removed", last)
	}
}

// openRepairState is a session that wrote mod.py unparseable, failed one
// edit, and left it that way.
func openRepairState(t *testing.T) (*AgentContext, *runState, *[]repairEvent) {
	ctx, dir, events := repairCtx(t)
	st := &runState{gateBounces: map[string]int{"repair_gate": 3}}
	writeRepairFile(t, dir, repairBroken)
	st.observeRepair(ctx, 0, "write_file", repairArgs("mod.py"), landedResult(repairSynErr))
	st.observeRepair(ctx, 1, "edit_file", repairArgs("mod.py"),
		&ToolResult{Error: "old_str not found in mod.py", MutationStatus: MutationRefused})
	return ctx, st, events
}

func TestTheHandOffListsWhatWasTriedAndAsksTheUserToLook(t *testing.T) {
	_, st, _ := openRepairState(t)
	h := repairHandoff(st, "repair_unfinished")
	for _, want := range []string{
		"Stopped: mod.py still does not parse, so the work is not finished.",
		"What was tried on mod.py:",
		"- turn 1, write_file: " + repairSynErr,
		"- turn 2, edit_file, not applied: old_str not found in mod.py",
		"The error that remains: " + repairSynErr + ". That line is:",
		"> 1\tdef broken(:",
		"The session ended because the agent was sent back to fix it 3 times and it still did not parse.",
		"Please take a look at mod.py.",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("the hand-off lacks %q:\n%s", want, h)
		}
	}
	if claim := completionClaimIn(h); claim != "" {
		t.Errorf("the hand-off claims completion (%q)", claim)
	}
	if !hasHonestMarker(h) {
		t.Error("the hand-off carries no honest marker")
	}
}

func TestEveryEndingWithAnOpenRepairHandsItOff(t *testing.T) {
	ctx, st, _ := openRepairState(t)
	got := honestTerminalSummary(ctx, st, TerminalTimedOut, "work_deadline",
		"Stopped: the session ran out of time before the work finished.")
	for _, want := range []string{
		"Stopped: the session ran out of time before the work finished.",
		"What was tried on mod.py:",
		"The session ended because it ran out of time. Please take a look at mod.py.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the deadline summary lacks %q:\n%s", want, got)
		}
	}
}

func TestAnOpenRepairIsNeverReportedCompleted(t *testing.T) {
	ctx, st, events := openRepairState(t)
	emitTerminal(ctx, st, TerminalCompleted, "deliverables_demonstrated", "All done.")
	var done map[string]interface{}
	var handoffs int
	for _, e := range *events {
		switch {
		case e.Type == "done":
			done = e.Data
		case e.Type == "repair" && e.Data["event"] == "handoff":
			handoffs++
		}
	}
	if done == nil {
		t.Fatal("no done event")
	}
	if done["status"] != "incomplete" || done["reason"] != "repair_unfinished" {
		t.Errorf("done = %v/%v, want incomplete/repair_unfinished", done["status"], done["reason"])
	}
	if done["repair_open"] != "mod.py" {
		t.Errorf("repair_open = %v, want mod.py", done["repair_open"])
	}
	if !strings.Contains(done["summary"].(string), "Please take a look at mod.py.") {
		t.Errorf("the summary is not the hand-off: %v", done["summary"])
	}
	if handoffs != 1 {
		t.Errorf("handoff events = %d, want 1", handoffs)
	}
}

// --- the real loop -------------------------------------------------------------

func TestAFileLeftUnparseableEndsTheRunWithAHandOff(t *testing.T) {
	r := editLoopFixture(t, nil, "", "Create mod.py with a helper that returns 1.",
		script(stepWrite("mod.py", repairBroken), stepDone("created mod.py")),
		editLoopOptions{})
	if r.terminal["status"] != "incomplete" || r.terminal["reason"] != "repair_unfinished" {
		t.Fatalf("terminal %q/%q, want incomplete/repair_unfinished: %s",
			r.terminal["status"], r.terminal["reason"], r.describe())
	}
	if r.terminal["repair_open"] != "mod.py" {
		t.Errorf("repair_open = %q, want mod.py", r.terminal["repair_open"])
	}
	for _, want := range []string{"What was tried on mod.py:", "write_file", "Please take a look at mod.py."} {
		if !strings.Contains(r.terminal["summary"], want) {
			t.Errorf("the final message lacks %q:\n%s", want, r.terminal["summary"])
		}
	}
	// The write's own result, the first thing the model read after it, names
	// the open repair.
	if len(r.prompts) < 2 || !strings.Contains(r.prompts[1], "open_repair") {
		t.Error("the result after the unparseable write does not name the open repair")
	}
	if r.census["gate"] < maxGateBounces {
		t.Errorf("done was sent back %d times, want at least %d", r.census["gate"], maxGateBounces)
	}
	// The repair gate owns the file: the run-first exit gate does not send
	// the same file back again.
	if last := r.prompts[len(r.prompts)-1]; strings.Contains(last, "is on disk with a parse warning and has never been run") {
		t.Error("the run-first exit gate also bounced a file the repair gate owns")
	}
}

// A refusal is a tool result too: the run-first gate's refusal of an edit to
// the unparseable, never-run file carries the open repair.
func TestARefusedCallAlsoNamesTheOpenRepair(t *testing.T) {
	r := editLoopFixture(t, nil, "", "Create mod.py with a helper that returns 1.",
		script(stepWrite("mod.py", repairBroken), stepEdit("mod.py", "def broken(:", "def fixed():"),
			stepDone("created mod.py")),
		editLoopOptions{})
	const bounced = "before editing further."
	var p string
	if len(r.prompts) > 2 {
		p = r.prompts[2]
	}
	i := strings.LastIndex(p, bounced)
	if i < 0 {
		t.Fatalf("the edit was not refused by the run-first gate: %s", r.describe())
	}
	if rest := p[i+len(bounced):]; !strings.Contains(rest[:min(len(rest), 40)], "open_repair") {
		t.Errorf("the refusal does not name the open repair: %q", rest[:min(len(rest), 120)])
	}
}

func TestAFileRepairedLetsTheRunFinish(t *testing.T) {
	r := editLoopFixture(t, nil, "", "Create mod.py with a helper that returns 1.",
		script(stepWrite("mod.py", repairBroken), stepRun("python3 mod.py"), stepRead("mod.py"),
			stepEdit("mod.py", "def broken(:", "def fixed():"), stepRun("python3 mod.py"),
			stepDone("created mod.py")),
		editLoopOptions{})
	if r.disk(t, "mod.py") != repairFixed {
		t.Fatalf("the fix did not land: %q (%s)", r.disk(t, "mod.py"), r.describe())
	}
	if r.terminal["reason"] == "repair_unfinished" || r.terminal["repair_open"] != "" {
		t.Errorf("a repaired file still ended the run as a repair: %q/%q open=%q",
			r.terminal["status"], r.terminal["reason"], r.terminal["repair_open"])
	}
	if r.census["repair"] < 2 {
		t.Errorf("repair events = %d, want at least opened and closed", r.census["repair"])
	}
}
