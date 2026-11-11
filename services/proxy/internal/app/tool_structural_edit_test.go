package app

import (
	"github.com/itigges22/atlas-proxy/internal/wire"
	"strings"
	"testing"
)

// structural_edit refuses a splice that leaves a file that parsed unable to
// parse, as edit_file, insert_after and replace_lines do. Otherwise the edits
// after a broken splice land on a broken file.

func TestStructuralEditRefusesToBreakAFileThatParses(t *testing.T) {
	r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiStrictWork,
		"Change helper in mod.py.",
		script(stepRead("mod.py"), stepStructural("mod.py", "function:helper", "def broken(:\n    return 2\n"),
			stepDone("changed helper")),
		editLoopOptions{})
	var refusal string
	for _, res := range r.results {
		if res["tool"] == "structural_edit" {
			refusal, _ = res["error"].(string)
		}
	}
	for _, want := range []string{"does not parse", "The file was NOT modified", "function:helper"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the structural_edit refusal lacks %q: %q (%s)", want, refusal, r.describe())
		}
	}
	if r.disk(t, "mod.py") != accountingSeed {
		t.Error("the refused splice reached disk")
	}
}

// It fails open like the other edit tools: with no parse verdict available,
// the splice lands.
func TestStructuralEditLandsWhenTheCheckCannotRun(t *testing.T) {
	r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiStrictWork,
		"Change helper in mod.py.",
		script(stepRead("mod.py"), stepStructural("mod.py", "function:helper", "def broken(:\n    return 2\n"),
			stepDone("changed helper")),
		editLoopOptions{syntaxDown: true})
	if !strings.Contains(r.disk(t, "mod.py"), "def broken(:") {
		t.Fatalf("the splice did not land with the check down: %s", r.describe())
	}
}

func TestStructuralSpliceChecksKeepTheFirstFailure(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier0Conversational)
	for _, tc := range []struct {
		name     string
		splice   structuralSplice
		category checkCategory
		message  string
	}{
		{"service before shrinkage", structuralSplice{Error: "selector missing", OldSize: 1000, NewSize: 1}, checkToolError, "selector missing"},
		{"shrinkage before unchanged", structuralSplice{Success: true, OldSize: 1000, NewSize: 1}, checkGuardRefusal, "suspiciously small"},
		{"unchanged", structuralSplice{Success: true, OldSize: 20, NewSize: 20}, checkGuardRefusal, "IDENTICAL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := structuralSpliceCheckInput{ctx: ctx, input: wire.StructuralEditInput{Path: "note.txt", Selector: "node"}, astResp: tc.splice}
			result := runCheckPhase(proxyChecks.structuralSplice, &check)
			if result != (checkResult{tc.category, turnEnds}) || check.result == nil {
				t.Fatalf("splice result = %+v, tool result = %+v", result, check.result)
			}
			if !strings.Contains(check.result.Error, tc.message) {
				t.Errorf("first failure = %q, want %q", check.result.Error, tc.message)
			}
			if check.result.MutationStatus != wire.MutationNone && check.result.MutationStatus != wire.MutationRefused {
				t.Errorf("a refused splice reported mutation: %+v", check.result)
			}
		})
	}
}
