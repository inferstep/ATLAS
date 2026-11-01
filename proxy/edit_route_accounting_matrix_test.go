package main

// The edit-route accounting matrix: what the ledger-backed coverage must and
// must not let a run complete, pinned through the real loop and at the owner.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The work completes only when a passing run covers the file's current bytes.
// A run that came before the last write or edit speaks for bytes that are
// gone. A file that changed is not evidence that it is right.
//
// The fresh.py rows use a file the session creates: write_file over a user
// file the session did not author is refused by the overwrite guard (see
// TestAnEditDoesNotMakeAUserFileTheModelsOwnDraft).
func TestOnlyARunOverTheCurrentBytesCompletesTheWork(t *testing.T) {
	helperReturning := func(n string) string {
		return strings.Replace(accountingSeed, "    return 1\n", "    return "+n+"\n", 1)
	}
	userFile := map[string]string{"mod.py": accountingSeed}
	noFiles := map[string]string{}
	for _, tc := range []struct {
		name         string
		seed         map[string]string
		prompt       string
		steps        []map[string]interface{}
		file         string
		wantDisk     string
		wantTerminal [2]string // status, reason
	}{
		{"edit, no run", userFile, "Change helper in mod.py.",
			[]map[string]interface{}{
				stepRead("mod.py"),
				stepEdit("mod.py", "    return 1\n", "    return 2\n"),
				stepDone("edited mod.py")},
			"mod.py", helperReturning("2"), [2]string{"incomplete", "verification_demanded_unmet"}},
		{"edit, run, edit", userFile, "Change helper in mod.py and check it.",
			[]map[string]interface{}{
				stepRead("mod.py"),
				stepEdit("mod.py", "    return 1\n", "    return 2\n"),
				stepRun("python3 mod.py"),
				stepEdit("mod.py", "    return 2\n", "    return 3\n"),
				stepDone("edited mod.py twice")},
			"mod.py", helperReturning("3"), [2]string{"incomplete", "verification_demanded_unmet"}},
		{"edit, edit, run", userFile, "Change helper in mod.py and check it.",
			[]map[string]interface{}{
				stepRead("mod.py"),
				stepEdit("mod.py", "    return 1\n", "    return 2\n"),
				stepEdit("mod.py", "    return 2\n", "    return 3\n"),
				stepRun("python3 mod.py"),
				stepDone("edited mod.py twice and ran it")},
			"mod.py", helperReturning("3"), [2]string{"completed", "deliverables_demonstrated"}},
		{"write, edit, run, write", noFiles, "Create fresh.py and check it.",
			[]map[string]interface{}{
				stepWrite("fresh.py", accountingSeed),
				stepRead("fresh.py"),
				stepEdit("fresh.py", "    return 1\n", "    return 2\n"),
				stepRun("python3 fresh.py"),
				stepWrite("fresh.py", helperReturning("7")),
				stepDone("rewrote fresh.py")},
			"fresh.py", helperReturning("7"), [2]string{"incomplete", "verification_demanded_unmet"}},
		{"write, run, edit", noFiles, "Create fresh.py and check it.",
			[]map[string]interface{}{
				stepWrite("fresh.py", accountingSeed),
				stepRun("python3 fresh.py"),
				stepRead("fresh.py"),
				stepEdit("fresh.py", "    return 1\n", "    return 8\n"),
				stepDone("wrote and edited fresh.py")},
			"fresh.py", helperReturning("8"), [2]string{"incomplete", "verification_demanded_unmet"}},
		{"write, edit, run", noFiles, "Create fresh.py and check it.",
			[]map[string]interface{}{
				stepWrite("fresh.py", accountingSeed),
				stepRead("fresh.py"),
				stepEdit("fresh.py", "    return 1\n", "    return 8\n"),
				stepRun("python3 fresh.py"),
				stepDone("wrote, edited and ran fresh.py")},
			"fresh.py", helperReturning("8"), [2]string{"completed", "deliverables_demonstrated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := editLoopFixture(t, tc.seed, tuiStrictWork, tc.prompt, script(tc.steps...), editLoopOptions{})

			disk := r.disk(t, tc.file)
			if disk != tc.wantDisk {
				t.Fatalf("the last write or edit did not land: %s", r.describe())
			}
			if !r.tracked(tc.file) {
				t.Fatalf("the landed change is not in the ledger: %s", r.describe())
			}
			if h := r.ledgerHash(tc.file); h != hashBytes([]byte(disk)) {
				t.Fatalf("ledger hash %q is not the current bytes", h)
			}
			if got := [2]string{r.terminal["status"], r.terminal["reason"]}; got != tc.wantTerminal {
				t.Errorf("terminal %q, want %q: %s", got, tc.wantTerminal, r.describe())
			}
		})
	}
}

// write_file over a user file the session only edited stays refused by the
// overwrite guard: an edit does not make the user's file the model's draft.
func TestAnEditDoesNotMakeAUserFileTheModelsOwnDraft(t *testing.T) {
	r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiStrictWork,
		"Change helper in mod.py.",
		script(stepRead("mod.py"),
			stepEdit("mod.py", "    return 1\n", "    return 2\n"),
			stepWrite("mod.py", "print(7)\n"),
			stepDone("rewrote mod.py")),
		editLoopOptions{})
	disk := r.disk(t, "mod.py")
	if disk == "print(7)\n" {
		t.Fatalf("the user's edited file was rewritten wholesale: %s", r.describe())
	}
	if !strings.Contains(disk, "return 2") {
		t.Fatalf("the edit itself did not land: %s", r.describe())
	}
	if r.ctx.SessionWrites["mod.py"] {
		t.Error("an edit registered the user's file as the session's own write")
	}
}

// An edit that lands nothing registers nothing, and the work cannot complete
// on it. A refused edit (its anchor is not in the file) and a no-op edit are
// both refused before they touch the disk.
func TestAnEditThatLandsNothingRegistersNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		old, new string
	}{
		{"anchor not found", "    return 42\n", "    return 2\n"},
		{"no-op", "    return 1\n", "    return 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiStrictWork,
				"Change helper in mod.py.",
				script(stepRead("mod.py"),
					stepEdit("mod.py", tc.old, tc.new),
					stepDone("edited mod.py")),
				editLoopOptions{})

			if r.tracked("mod.py") || len(changedPathsForCoverage(r.ctx)) != 0 {
				t.Fatalf("an edit that landed nothing became a coverable change: %s", r.describe())
			}
			if r.disk(t, "mod.py") != accountingSeed {
				t.Fatal("an edit that landed nothing changed the file")
			}
			if r.terminal["status"] == "completed" {
				t.Fatalf("a run whose only edit landed nothing completed: %s", r.describe())
			}
		})
	}
}

// Alias spellings of one file are one identity: the edit registers the
// spelling the model used, the ledger keys the canonical path, and a run that
// names the file covers it.
func TestAliasSpellingsAreOneChangedPath(t *testing.T) {
	r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiStrictWork,
		"Change helper in mod.py and check it.",
		script(stepRead("./mod.py"),
			stepEdit("./mod.py", "    return 1\n", "    return 2\n"),
			stepRun("python3 mod.py"),
			stepDone("edited and ran mod.py")),
		editLoopOptions{})
	if got := changedPathsForCoverage(r.ctx); len(got) != 1 || filepath.Base(got[0]) != "mod.py" {
		t.Fatalf("coverable paths %v, want one canonical identity", got)
	}
	keys := 0
	r.ctx.LedgerMu.Lock()
	for k := range r.ctx.Ledger {
		if filepath.Base(k) == "mod.py" {
			keys++
		}
	}
	r.ctx.LedgerMu.Unlock()
	if keys != 1 {
		t.Fatalf("%d ledger identities for one file", keys)
	}
	if r.terminal["status"] != "completed" {
		t.Fatalf("terminal %q/%q, want completed: %s", r.terminal["status"], r.terminal["reason"], r.describe())
	}
}

// An older client's strict or advisory spelling selects nothing: V3's winner
// lands on the edit route as it does for any work request, and the landed
// edit is accounted for like any other.
func TestOlderPolicySpellingsGetTheSameEditAndStillAccount(t *testing.T) {
	winner := strings.Replace(accountingSeed, "    return 1\n", "    return 9\n", 1)
	for _, tc := range []struct {
		name     string
		contract string
	}{
		{"strict", tuiStrictWork},
		{"advisory", tuiAdvisoryContract},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tc.contract,
				"Change helper in mod.py and check it.",
				script(stepRead("mod.py"),
					stepEdit("mod.py", "    return 1\n", "    return 2\n"),
					stepRun("python3 mod.py"),
					stepDone("edited and ran mod.py")),
				editLoopOptions{v3Winner: winner})
			disk := r.disk(t, "mod.py")
			if !strings.Contains(disk, "return 9") {
				t.Fatalf("disk holds %q, want the selected candidate", disk[len(disk)-80:])
			}
			if !r.tracked("mod.py") {
				t.Error("the landed edit is not in the ledger")
			}
			if r.terminal["status"] != "completed" {
				t.Errorf("terminal %q/%q, want completed: %s", r.terminal["status"], r.terminal["reason"], r.describe())
			}
		})
	}
}

// A cancelled request on the pipeline path lands nothing and registers
// nothing: the route reports the cancellation and the tool writes no bytes.
// The tier bypass path is not where a cancelled edit is refused: there the
// agent loop stops dispatching tools.
func TestACancelledEditRegistersNothing(t *testing.T) {
	requirePython3(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mod.py")
	if err := os.WriteFile(path, []byte(accountingSeed), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	// A producer address the route will try: the request is cancelled before
	// dispatch, so nothing is ever sent to it.
	ctx.V3URL = "http://127.0.0.1:9"
	// Generation runs only where a candidate could be delivered. Declared
	// outputs make this session one, so the edit reaches the V3 route.
	ctx.TaskContract = declaredOutputs("mod.py")
	c, cancel := context.WithCancel(context.WithValue(context.Background(), requestIDKey, "req-cancel"))
	ctx.Ctx = c
	ctx.RecordFileRead(path, accountingSeed)
	ctx.RecordBodySeen(path)
	cancel()
	args, _ := json.Marshal(map[string]string{"path": "mod.py",
		"old_str": "    return 1\n", "new_str": "    return 2\n"})
	res := executeToolCall("edit_file", args, ctx)
	if res != nil && res.Success {
		t.Fatalf("a cancelled edit reported success: %+v", res)
	}
	if len(ctx.SessionWrites) != 0 || len(changedPathsForCoverage(ctx)) != 0 {
		t.Fatalf("a cancelled edit became a change: %v %v", ctx.SessionWrites, changedPathsForCoverage(ctx))
	}
	b, _ := os.ReadFile(path)
	if string(b) != accountingSeed {
		t.Fatal("a cancelled edit changed the file")
	}
}

// --- the coverage owner --------------------------------------------------------

// changedPathsForCoverage is the union of the raw session-write paths and the
// ledger's canonical code deliverables: every landed mutation, once, and
// nothing that never landed.
func TestCoverablePathsComeFromTheLedgerAndTheSessionMap(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"edited.py", "written.py", "notes.md", "gone.py"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("A = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	observeDeliverable(ctx, "edited.py", []byte("A = 1\n"), ValidationKindSyntax, ValidationPassed, "edit_file")
	observeDeliverable(ctx, "notes.md", []byte("A = 1\n"), ValidationKindNone, ValidationNotApplicable, "edit_file")
	observeDeliverable(ctx, "gone.py", []byte("A = 1\n"), ValidationKindSyntax, ValidationPassed, "edit_file")
	tombstoneDeliverable(ctx, "gone.py", "deleted")
	ctx.SessionWrites["written.py"] = true
	ctx.SessionWrites["./edited.py"] = true
	got := changedPathsForCoverage(ctx)
	want := []string{"./edited.py", filepath.Join(dir, "edited.py"), "written.py"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("coverable %v, want %v", got, want)
	}
	if changedPathsForCoverage(nil) != nil {
		t.Error("a nil context has coverable paths")
	}
}

// A run that names an edited file covers the ledger's canonical identity, and
// the demand reads the same identity, so the two agree by construction.
func TestCoverageAndDemandReadTheSameIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mod.py"), []byte(accountingSeed), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.TaskContract = mustContract(t, dir, tuiStrictWork)
	observeDeliverable(ctx, "mod.py", []byte(accountingSeed), ValidationKindSyntax, ValidationPassed, "edit_file")
	var covered []string
	for _, p := range changedPathsForCoverage(ctx) {
		if commandNamesPath("python3 mod.py", p) {
			covered = append(covered, p)
		}
	}
	demanded := codeDeliverablesFor(ctx, nil)
	if len(covered) != 1 || len(demanded) != 1 || covered[0] != demanded[0] {
		t.Fatalf("covered %v, demanded %v", covered, demanded)
	}
	if !commandNamesPath("python3 ./mod.py", covered[0]) || commandNamesPath("python3 other.py", covered[0]) {
		t.Error("the command binding does not key on the file's own name")
	}
}

// The session-write map keeps its meaning for its other readers: no edit tool
// branch gained or lost an assignment, and write_file's own stay as they were.
func TestTheSessionWriteMapIsUnchangedForItsOtherReaders(t *testing.T) {
	body := toolSources(t)
	if n := strings.Count(body, "ctx.SessionWrites[input.Path] = true"); n != 2 {
		t.Errorf("write_file's own registrations changed: %d, want 2", n)
	}
	if n := strings.Count(body, "ctx.SessionWrites[in.Path] = true"); n != 2 {
		t.Errorf("insert_after/replace_lines registrations changed: %d, want 2", n)
	}
	edit := readSourceForTest(t, "tool_edit.go")
	start := strings.Index(edit, "func editFileTool(")
	if start < 0 {
		t.Fatal("tool_edit.go has no editFileTool, so the span would be empty")
	}
	end := strings.Index(edit[start:], "\n// closestLineHint")
	if end < 0 {
		t.Fatal("tool_edit.go has no closestLineHint after editFileTool, so the span would be empty")
	}
	for tool, source := range map[string]string{
		"editFileTool":       edit[start : start+end],
		"structuralEditTool": readSourceForTest(t, "tool_structural_edit.go"),
	} {
		if strings.Contains(source, "SessionWrites[") {
			t.Errorf("%s now writes the session map; coverage comes from the ledger instead", tool)
		}
	}
	agent, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	// The loop itself moved into coverageForGreenCommand so a green run can
	// also cover the files its entry point imports. What must not move is
	// where the candidate set comes from: the ledger-backed owner, in exactly
	// one place. agent.go delegates and builds no path list of its own.
	if n := strings.Count(string(agent), "coverageForGreenCommand(ctx, strings.Join(segs, \" && \"))"); n != 1 {
		t.Errorf("agent.go calls the coverage builder %d times, want 1", n)
	}
	if strings.Contains(string(agent), "changedPathsForCoverage(") {
		t.Error("agent.go builds its own coverage path list again")
	}
	guard, err := os.ReadFile("guardrails.go")
	if err != nil {
		t.Fatal(err)
	}
	builder := string(guard)[strings.Index(string(guard), "func coverageForGreenCommand("):]
	builder = builder[:strings.Index(builder[1:], "\nfunc ")]
	if !strings.Contains(builder, "changedPathsForCoverage(ctx)") {
		t.Error("the coverage builder no longer reads the ledger-backed owner")
	}
	if !strings.Contains(builder, "commandNamesPath(command, p)") {
		t.Error("the coverage builder no longer requires a named entry point")
	}
}
