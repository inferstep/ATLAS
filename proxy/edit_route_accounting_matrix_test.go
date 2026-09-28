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

// A successful edit that nobody verified stays incomplete: the fact that the
// path changed is not evidence that it is right.
func TestAnUnverifiedEditStaysIncomplete(t *testing.T) {
	r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiStrictWork,
		"Change helper in mod.py.",
		script(stepRead("mod.py"),
			stepEdit("mod.py", "    return 1\n", "    return 2\n"),
			stepDone("edited mod.py")),
		editLoopOptions{})
	if !r.tracked("mod.py") {
		t.Fatalf("the landed edit is not in the ledger: %s", r.describe())
	}
	if r.terminal["status"] == "completed" {
		t.Fatalf("an unverified edit completed: %s", r.describe())
	}
	if r.terminal["reason"] != "verification_demanded_unmet" {
		t.Errorf("reason %q, want the verification demand to own the terminal", r.terminal["reason"])
	}
}

// A verification of bytes that were edited again afterwards speaks for stale
// bytes; the current generation is unverified and the run stays incomplete.
func TestVerificationOfStaleBytesDoesNotComplete(t *testing.T) {
	r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiStrictWork,
		"Change helper in mod.py and check it.",
		script(stepRead("mod.py"),
			stepEdit("mod.py", "    return 1\n", "    return 2\n"),
			stepRun("python3 mod.py"),
			stepEdit("mod.py", "    return 2\n", "    return 3\n"),
			stepDone("edited mod.py twice")),
		editLoopOptions{})
	if !strings.Contains(r.disk(t, "mod.py"), "return 3") {
		t.Fatalf("the second edit did not land: %s", r.describe())
	}
	if r.terminal["status"] == "completed" {
		t.Fatalf("a run verified before the last edit completed: %s", r.describe())
	}
}

// Several edits of one path, then one run over the final bytes: the coverage
// is of the current generation and the run completes.
func TestSeveralEditsOfOnePathAreCoveredByOneCurrentRun(t *testing.T) {
	r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiStrictWork,
		"Change helper in mod.py and check it.",
		script(stepRead("mod.py"),
			stepEdit("mod.py", "    return 1\n", "    return 2\n"),
			stepEdit("mod.py", "    return 2\n", "    return 3\n"),
			stepRun("python3 mod.py"),
			stepDone("edited mod.py twice and ran it")),
		editLoopOptions{})
	disk := r.disk(t, "mod.py")
	if !strings.Contains(disk, "return 3") {
		t.Fatalf("the second edit did not land: %s", r.describe())
	}
	if h := r.ledgerHash("mod.py"); h != hashBytes([]byte(disk)) {
		t.Fatalf("ledger hash %q is not the current bytes", h)
	}
	if r.terminal["status"] != "completed" {
		t.Fatalf("terminal %q/%q, want completed: %s", r.terminal["status"], r.terminal["reason"], r.describe())
	}
}

// An edit, a passing run, then a write_file over the same path: the run spoke
// for bytes that are gone, and the write needs its own verification. The file
// is one the session created: write_file over a user file the session only
// edited is still steered to the edit tools by the overwrite guard, which this
// change leaves exactly as it was (see TestSuccessClearsSteeringState).
func TestAnEditThenAWriteNeedsAFreshVerification(t *testing.T) {
	rewritten := strings.Replace(accountingSeed, "    return 1\n", "    return 7\n", 1)
	r := editLoopFixture(t, map[string]string{}, tuiStrictWork,
		"Create fresh.py and check it.",
		script(stepWrite("fresh.py", accountingSeed),
			stepRead("fresh.py"),
			stepEdit("fresh.py", "    return 1\n", "    return 2\n"),
			stepRun("python3 fresh.py"),
			stepWrite("fresh.py", rewritten),
			stepDone("rewrote fresh.py")),
		editLoopOptions{})
	if r.disk(t, "fresh.py") != rewritten {
		t.Fatalf("the rewrite did not land: %s", r.describe())
	}
	if r.terminal["status"] == "completed" {
		t.Fatalf("a rewrite after the only run completed: %s", r.describe())
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

// A write of a new file, a passing run, then an edit of it: the run spoke for
// bytes that are gone, and the edit needs its own verification. (write_file
// over an existing file the session did not author is refused by the overwrite
// guard, unchanged here, so the file is one the session creates.)
func TestAWriteThenAnEditNeedsAFreshVerification(t *testing.T) {
	r := editLoopFixture(t, map[string]string{}, tuiStrictWork,
		"Create fresh.py and check it.",
		script(stepWrite("fresh.py", accountingSeed),
			stepRun("python3 fresh.py"),
			stepRead("fresh.py"),
			stepEdit("fresh.py", "    return 1\n", "    return 8\n"),
			stepDone("wrote and edited fresh.py")),
		editLoopOptions{})
	if !strings.Contains(r.disk(t, "fresh.py"), "return 8") {
		t.Fatalf("the edit after the write did not land: %s", r.describe())
	}
	if r.terminal["status"] == "completed" {
		t.Fatalf("an edit after the only run completed: %s", r.describe())
	}
}

// The same order with the run last completes: the run covers the current bytes.
func TestAWriteThenAnEditThenARunCompletes(t *testing.T) {
	r := editLoopFixture(t, map[string]string{}, tuiStrictWork,
		"Create fresh.py and check it.",
		script(stepWrite("fresh.py", accountingSeed),
			stepRead("fresh.py"),
			stepEdit("fresh.py", "    return 1\n", "    return 8\n"),
			stepRun("python3 fresh.py"),
			stepDone("wrote, edited and ran fresh.py")),
		editLoopOptions{})
	disk := r.disk(t, "fresh.py")
	if !strings.Contains(disk, "return 8") {
		t.Fatalf("the edit did not land: %s", r.describe())
	}
	if h := r.ledgerHash("fresh.py"); h != hashBytes([]byte(disk)) {
		t.Fatalf("ledger hash %q is not the current bytes", h)
	}
	if r.terminal["status"] != "completed" {
		t.Fatalf("terminal %q/%q, want completed: %s", r.terminal["status"], r.terminal["reason"], r.describe())
	}
}

// A refused edit (anchor not found) registers nothing and the run cannot
// complete on it.
func TestARefusedEditRegistersNoSessionWrite(t *testing.T) {
	r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiStrictWork,
		"Change helper in mod.py.",
		script(stepRead("mod.py"),
			stepEdit("mod.py", "    return 42\n", "    return 2\n"),
			stepDone("edited mod.py")),
		editLoopOptions{})
	if r.tracked("mod.py") || len(changedPathsForCoverage(r.ctx)) != 0 {
		t.Fatalf("a refused edit became a coverable change: %s", r.describe())
	}
	if r.disk(t, "mod.py") != accountingSeed {
		t.Fatal("a refused edit changed the file")
	}
	if r.terminal["status"] == "completed" {
		t.Fatalf("a run with a refused edit completed: %s", r.describe())
	}
}

// A no-op edit is refused before it touches disk: no productive change, no
// session write.
func TestANoOpEditRegistersNothing(t *testing.T) {
	r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiStrictWork,
		"Change helper in mod.py.",
		script(stepRead("mod.py"),
			stepEdit("mod.py", "    return 1\n", "    return 1\n"),
			stepDone("edited mod.py")),
		editLoopOptions{})
	if r.tracked("mod.py") || len(changedPathsForCoverage(r.ctx)) != 0 {
		t.Fatalf("a no-op edit became a coverable change: %s", r.describe())
	}
	if r.terminal["status"] == "completed" {
		t.Fatalf("a run whose only edit was a no-op completed: %s", r.describe())
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
	for _, contract := range []string{tuiStrictWork, tuiAdvisoryContract} {
		r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, contract,
			"Change helper in mod.py and check it.",
			script(stepRead("mod.py"),
				stepEdit("mod.py", "    return 1\n", "    return 2\n"),
				stepRun("python3 mod.py"),
				stepDone("edited and ran mod.py")),
			editLoopOptions{v3Winner: winner})
		disk := r.disk(t, "mod.py")
		if !strings.Contains(disk, "return 9") {
			t.Fatalf("%s: disk holds %q, want the selected candidate", contract, disk[len(disk)-80:])
		}
		if !r.tracked("mod.py") {
			t.Errorf("%s: the landed edit is not in the ledger", contract)
		}
		if r.terminal["status"] != "completed" {
			t.Errorf("%s: terminal %q/%q, want completed: %s", contract,
				r.terminal["status"], r.terminal["reason"], r.describe())
		}
	}
}

// A cancelled request on the pipeline path lands nothing and registers
// nothing: the route reports the cancellation and the tool writes no bytes.
// (Cancellation is observed by the route and by the agent loop, which stops
// dispatching tools; a tool asked to land its own bytes with no producer in
// play does so, so the direct path is not where a cancelled edit is refused.)
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
	// Generation runs only where a candidate could be delivered (C2); declared
	// outputs make this session one, so the fixture still reaches the V3 route.
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
	src, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if n := strings.Count(body, "ctx.SessionWrites[input.Path] = true"); n != 2 {
		t.Errorf("write_file's own registrations changed: %d, want 2", n)
	}
	if n := strings.Count(body, "ctx.SessionWrites[in.Path] = true"); n != 2 {
		t.Errorf("insert_after/replace_lines registrations changed: %d, want 2", n)
	}
	for _, tool := range []string{"editFileTool", "structuralEditTool"} {
		start := strings.Index(body, "func "+tool+"(")
		end := strings.Index(body[start+1:], "\nfunc ")
		if strings.Contains(body[start:start+1+end], "SessionWrites[") {
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
