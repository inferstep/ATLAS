package main

import (
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
