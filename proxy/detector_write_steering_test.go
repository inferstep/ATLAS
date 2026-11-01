package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --- Ignored write_file steering ---------------------------------------------
//
// Two branches refuse a write_file over an existing file and steer the model
// elsewhere: "read it first" when the session has never read it, and "use
// edit_file" once it has. A model that ignores the steer must still reach a
// terminal of the proxy's own; without accounting it could repeat the write
// forever in a healthy workspace.
//
// They are steering, not ordinary failures: a model that follows the steer
// must be completely unaffected.

// steerFixture drives the real loop with MaxTurns=0 over a seeded existing
// file. plan sees the prompt so a scripted model can react to what it was told.
func steerFixture(t *testing.T, seed map[string]string,
	plan func(i int, prompt string) map[string]interface{}) (
	*AgentContext, string, *int, map[string]int, map[string]string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range seed {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	turns := 0
	census := map[string]int{}
	terminal := map[string]string{}
	var bounces []string
	var mu sync.Mutex
	fenced := 0

	srv := httptest.NewServer(steerServer(dir, plan, &mu, &turns, &fenced))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 0
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		census[et]++
		census["_fenced"] = fenced
		if et == "gate" || et == "tool_result" {
			bounces = append(bounces, et+"|"+string(b))
		}
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	runAgentLoop(ctx, "Rewrite app.py so it prints 7.")
	return ctx, dir, &turns, census, terminal, &bounces
}

// steerServer stands in for the model, the sandbox and the V3 service behind
// steerFixture. V3 and the internal endpoints are down. Each model turn is
// counted in turns and answered with plan's call, up to wsCeiling; a request
// for a single fenced block is counted in fenced and gets none. mu guards both
// counts.
func steerServer(dir string, plan func(i int, prompt string) map[string]interface{},
	mu *sync.Mutex, turns, fenced *int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"valid": !strings.Contains(in.Code, "]]")})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(raw), "single fenced block") {
			mu.Lock()
			*fenced++
			mu.Unlock()
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": "no block here"}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
			return
		}
		mu.Lock()
		i := *turns
		*turns++
		mu.Unlock()
		if i >= wsCeiling {
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i, string(raw)))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}
}

// Over the five-line carve-out, so the existing-file steer applies: below it
// there is no edit-vs-rewrite distinction and a full write is allowed.
const steerSeed = "def solve():\n    total = 0\n    for i in range(3):\n        total += i\n    return 1\n\n\nprint(solve())\n"

// The "read it first" steer, ignored: the model never reads app.py and only
// ever repeats the refused write.
func TestIgnoredWriteSteeringIsBoundedWhenNeverRead(t *testing.T) {
	ctx, dir, turns, census, terminal, bounces := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
		})
	t.Logf("never read: turns=%d status=%q reason=%q", *turns,
		terminal["status"], terminal["reason"])

	steered := false
	for _, b := range *bounces {
		if strings.Contains(b, "has not read it") {
			steered = true
		}
	}
	if !steered {
		t.Fatal("the fixture never reached the never-read steer")
	}
	if *turns >= wsCeiling {
		t.Fatalf("%d turns without a terminal", *turns)
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("a run that only ever repeated a refused write reported %q",
			terminal["status"])
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result balance: %d vs %d",
			census["tool_call"], census["tool_result"])
	}
	// The file is untouched and unowned.
	const wfPath = "app.py"
	got, _ := os.ReadFile(filepath.Join(dir, wfPath))
	if string(got) != steerSeed {
		t.Errorf("the refused write reached disk: %q", got)
	}
	// The bounded recovery does show the file once, on purpose --
	// see TestUnreadSteerRecoveryShowsTheFile. What it must never do
	// is hand the model ownership of a file it did not write.
	if ctx.SessionWrites[wfPath] {
		t.Error("a refused write manufactured session ownership")
	}
}

// The "use edit_file" steer, ignored: the model reads app.py first, then only
// ever repeats the refused write.
func TestIgnoredWriteSteeringIsBoundedWhenAlreadyRead(t *testing.T) {
	ctx, dir, turns, census, terminal, bounces := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			if i == 0 {
				return map[string]interface{}{"type": "tool_call", "name": "read_file",
					"args": map[string]string{"path": "app.py"}}
			}
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
		})
	t.Logf("already read: turns=%d status=%q reason=%q", *turns,
		terminal["status"], terminal["reason"])

	steered := false
	for _, b := range *bounces {
		if strings.Contains(b, "write_file is for creating new files") {
			steered = true
		}
	}
	if !steered {
		t.Fatal("the fixture never reached the already-read steer")
	}
	if *turns >= wsCeiling {
		t.Fatalf("%d turns without a terminal", *turns)
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("a run that only ever repeated a refused write reported %q",
			terminal["status"])
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result balance: %d vs %d",
			census["tool_call"], census["tool_result"])
	}
	// The file is untouched and unowned: a refused write must never hand the
	// model ownership of a file it did not write.
	const wfPath = "app.py"
	got, _ := os.ReadFile(filepath.Join(dir, wfPath))
	if string(got) != steerSeed {
		t.Errorf("the refused write reached disk: %q", got)
	}
	if ctx.SessionWrites[wfPath] {
		t.Error("a refused write manufactured session ownership")
	}
}

// The control that matters most: a model that FOLLOWS the steer is unaffected.
func TestFollowingTheFirstSteerStillCompletes(t *testing.T) {
	ctx, dir, turns, census, terminal, _ := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, prompt string) map[string]interface{} {
			switch {
			case i == 0:
				// Refused: existing and unread.
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
			case i == 1:
				return map[string]interface{}{"type": "tool_call", "name": "read_file",
					"args": map[string]string{"path": "app.py"}}
			case i == 2:
				return map[string]interface{}{"type": "tool_call", "name": "edit_file",
					"args": map[string]string{"path": "app.py",
						"old_str": "return 1", "new_str": "return 7"}}
			case i == 3:
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "python3 app.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "app.py now returns 7"}
			}
		})
	got, _ := os.ReadFile(filepath.Join(dir, "app.py"))
	t.Logf("followed: turns=%d status=%q reason=%q disk=%q",
		*turns, terminal["status"], terminal["reason"], string(got))

	if terminal["status"] != string(TerminalCompleted) {
		t.Fatalf("following the steer did not complete: status=%q reason=%q",
			terminal["status"], terminal["reason"])
	}
	if !strings.Contains(string(got), "return 7") {
		t.Errorf("the edit did not land: %q", got)
	}
	d := ctx.Ledger[ledgerKey(ctx, "app.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Error("the ledger does not describe the final bytes")
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

// Path identity under ignored steering: aliases converge on one target, and
// distinct paths stay independent of each other. Either way the run is bounded
// and honest; the difference is WHEN it ends.

// Alternating aliases is one path repeating, so the consecutive-same-path rule
// ends it at three.
func TestIgnoredSteeringAliasesAreOneTarget(t *testing.T) {
	_, dir, turns, _, terminal, _ := steerFixture(t,
		map[string]string{"app.py": steerSeed, "util.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			path := "app.py"
			if i%2 == 1 {
				path = "./app.py"
			}
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": path, "content": "print(7)\n"}}
		})
	t.Logf("aliases are one target: turns=%d status=%q reason=%q",
		*turns, terminal["status"], terminal["reason"])

	if *turns >= wsCeiling {
		t.Fatalf("%d turns without a terminal", *turns)
	}
	if terminal["reason"] != "repeated_refusal" {
		t.Fatalf("reason=%q, want repeated_refusal", terminal["reason"])
	}
	if *turns > 3 {
		t.Errorf("aliases did not converge on one target: %d turns", *turns)
	}
	for _, f := range []string{"app.py", "util.py"} {
		if got, _ := os.ReadFile(filepath.Join(dir, f)); string(got) != steerSeed {
			t.Errorf("%s was overwritten by a refused write: %q", f, got)
		}
	}
}

// Two real paths is not a stuck loop, so it survives past three and is bounded
// by the total-failure ceiling instead.
func TestIgnoredSteeringDistinctPathsAreIndependent(t *testing.T) {
	_, dir, turns, _, terminal, _ := steerFixture(t,
		map[string]string{"app.py": steerSeed, "util.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			path := "app.py"
			if i%2 == 1 {
				path = "util.py"
			}
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": path, "content": "print(7)\n"}}
		})
	t.Logf("distinct paths are independent: turns=%d status=%q reason=%q",
		*turns, terminal["status"], terminal["reason"])

	if *turns >= wsCeiling {
		t.Fatalf("%d turns without a terminal", *turns)
	}
	if terminal["reason"] != "repeated_refusal" {
		t.Fatalf("reason=%q, want repeated_refusal", terminal["reason"])
	}
	if *turns <= 3 {
		t.Errorf("distinct paths were treated as one stuck path: %d turns", *turns)
	}
	for _, f := range []string{"app.py", "util.py"} {
		if got, _ := os.ReadFile(filepath.Join(dir, f)); string(got) != steerSeed {
			t.Errorf("%s was overwritten by a refused write: %q", f, got)
		}
	}
}

// A success on the path clears the remembered rejection, under the same
// semantics every other refusal branch already relies on: a later identical
// write is refused fresh by the gate rather than banned as a repeat.
func TestSuccessClearsSteeringState(t *testing.T) {
	ctx, _, turns, _, terminal, bounces := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			switch i {
			case 1:
				return map[string]interface{}{"type": "tool_call", "name": "read_file",
					"args": map[string]string{"path": "app.py"}}
			case 2:
				return map[string]interface{}{"type": "tool_call", "name": "edit_file",
					"args": map[string]string{"path": "app.py",
						"old_str": "return 1", "new_str": "return 7"}}
			case 0, 3: // the same refused write, before and after the edit
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "app.py returns 7"}
			}
		})
	t.Logf("clear-on-success: turns=%d failed_calls=%d status=%q reason=%q",
		*turns, len(ctx.FailedToolCalls), terminal["status"], terminal["reason"])

	steers := 0
	for _, b := range *bounces {
		if strings.HasPrefix(b, "gate|") && strings.Contains(b, "already exists") {
			steers++
		}
	}
	if steers != 2 {
		t.Errorf("%d steering refusals, want one before and one after the edit", steers)
	}
	// The post-edit refusal is the gate speaking again, not a stale ban: the
	// edit wiped the remembered rejections, so only that one is held.
	if len(ctx.FailedToolCalls) != 1 {
		t.Errorf("%d remembered rejections after the successful edit, want 1",
			len(ctx.FailedToolCalls))
	}
}

// --- One bounded recovery for ignored write_file steering --------------------

// The unread steer is ignored once, so the second refusal shows the file the
// model kept trying to destroy — through the same reader read_file uses, and
// recording exactly what was shown. It is a read, not ownership.
func TestUnreadSteerRecoveryShowsTheFile(t *testing.T) {
	ctx, dir, turns, _, terminal, bounces := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
		})
	t.Logf("unread recovery: turns=%d status=%q reason=%q",
		*turns, terminal["status"], terminal["reason"])

	// Gate events carry a truncated reason for display; the tool result the
	// model actually receives carries the whole thing.
	var offers, shown int
	for _, b := range *bounces {
		if strings.HasPrefix(b, "gate|") && strings.Contains(b, "here it is") {
			offers++
		}
		if strings.HasPrefix(b, "tool_result|") && strings.Contains(b, "here it is") &&
			strings.Contains(b, "range(3)") && strings.Contains(b, "print(solve())") {
			shown++
		}
	}
	if offers != 1 {
		t.Errorf("%d recovery offers, want exactly one", offers)
	}
	if shown != 1 {
		t.Errorf("the recovery claimed to show the file but the model received %d bodies", shown)
	}
	// The model has genuinely seen the body now, and owns nothing more.
	if !ctx.WasFileRead(filepath.Join(dir, "app.py")) {
		t.Error("the shown file was not recorded as read")
	}
	if ctx.SessionWrites["app.py"] {
		t.Error("the recovery manufactured session ownership of the file")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "app.py")); string(got) != steerSeed {
		t.Errorf("the refused write reached disk: %q", got)
	}
	// The recovery buys a better turn, not an extra one.
	if *turns > 3 {
		t.Errorf("the recovery loosened the bound: %d turns", *turns)
	}
	if terminal["reason"] != "repeated_refusal" {
		t.Errorf("reason=%q, want repeated_refusal", terminal["reason"])
	}
}

// A file too long to show whole is shown by its head, and the model owns only
// that: the lines past it stay unread.
func TestASteerRecoveryOfALongFileRecordsOnlyItsHead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "long.py")
	if err := os.WriteFile(path, []byte(strings.Repeat("x = 1\n", fencedRecoveryMaxLines+50)), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	st := &runState{}

	var note string
	for i := 0; i < steerRecoveryRepeat; i++ {
		note = steerRecovery(ctx, st, "long.py", path, true)
	}

	if want := fmt.Sprintf("(first %d lines)", fencedRecoveryMaxLines); !strings.Contains(note, want) {
		t.Errorf("the recovery does not say %q: %.120s", want, note)
	}
	if !ctx.LineWasShown(path, fencedRecoveryMaxLines) {
		t.Error("the last line of the head was not recorded as shown")
	}
	if ctx.LineWasShown(path, fencedRecoveryMaxLines+1) {
		t.Error("a line past the head was recorded as shown")
	}
}

// The already-read steer fails for a different reason, so it gets a different
// recovery: one reminder, no reread, both edit tools still on the table.
func TestAlreadyReadSteerRecoveryRemindsWithoutRereading(t *testing.T) {
	_, _, turns, _, terminal, bounces := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			if i == 0 {
				return map[string]interface{}{"type": "tool_call", "name": "read_file",
					"args": map[string]string{"path": "app.py"}}
			}
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
		})
	t.Logf("already-read recovery: turns=%d status=%q reason=%q",
		*turns, terminal["status"], terminal["reason"])

	var offers int
	for _, b := range *bounces {
		switch {
		case !strings.Contains(b, "You do not need to read it again"):
			continue
		case strings.HasPrefix(b, "gate|"):
			offers++
		case strings.HasPrefix(b, "tool_result|"):
			if strings.Contains(b, "range(3)") {
				t.Error("the reminder re-showed a file the model had already read")
			}
			// Both edit tools stay on the table; neither is imposed.
			if !strings.Contains(b, "edit_file") {
				t.Error("the reminder does not leave edit_file available")
			}
			if !strings.Contains(b, "structural_edit") {
				t.Error("the reminder does not leave structural_edit available")
			}
		}
	}
	if offers != 1 {
		t.Errorf("%d reminders, want exactly one", offers)
	}
}

// The recovery is per path and released only by a materially different action
// on that same path.
func TestSteerRecoveryIsPerPathAndReleasedByProgress(t *testing.T) {
	write := func(path string) map[string]interface{} {
		return map[string]interface{}{"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": path, "content": "print(7)\n"}}
	}
	ctx, _, _, _, _, bounces := steerFixture(t,
		map[string]string{"app.py": steerSeed, "util.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			switch i {
			case 0, 1: // app.py: steer, then the ignored repeat -> recovery
				return write("app.py")
			case 2: // a materially different action on app.py, which succeeds
				return map[string]interface{}{"type": "tool_call", "name": "read_file",
					"args": map[string]string{"path": "util.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "done"}
			}
		})
	appOffers := 0
	utilOffers := 0
	for _, b := range *bounces {
		if !strings.HasPrefix(b, "gate|") {
			continue
		}
		if strings.Contains(b, "here it is") || strings.Contains(b, "You do not need to read it again") {
			if strings.Contains(b, "util.py") {
				utilOffers++
			} else {
				appOffers++
			}
		}
	}
	t.Logf("per-path recovery: app=%d util=%d", appOffers, utilOffers)
	if appOffers != 1 {
		t.Errorf("%d recoveries for app.py, want one", appOffers)
	}
	if utilOffers != 0 {
		t.Errorf("app.py's recovery spent util.py's: %d", utilOffers)
	}
	// Reading util.py is not progress on app.py, so app.py's state stands.
	if !ctx.WasFileRead(ledgerKey(ctx, "app.py")) {
		t.Error("the recovery did not leave app.py read")
	}
}

// Causal: the recovery is what makes the difference. The model only leaves the
// loop after it has actually been shown the file — before that it repeats the
// same refused write.
func TestSteerRecoveryCausesConvergence(t *testing.T) {
	edited := false
	ctx, dir, turns, census, terminal, _ := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, prompt string) map[string]interface{} {
			// Nothing but the recovery moves it.
			if !strings.Contains(prompt, "so here it is") {
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
			}
			switch {
			case !edited:
				edited = true
				// It edits the text the recovery put in front of it.
				return map[string]interface{}{"type": "tool_call", "name": "edit_file",
					"args": map[string]string{"path": "app.py",
						"old_str": "return 1", "new_str": "return 7"}}
			case i < 5:
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "python3 app.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "app.py returns 7"}
			}
		})
	got, _ := os.ReadFile(filepath.Join(dir, "app.py"))
	t.Logf("causal: turns=%d status=%q reason=%q disk=%q",
		*turns, terminal["status"], terminal["reason"], string(got))

	if terminal["status"] != string(TerminalCompleted) {
		t.Fatalf("the recovery did not convert the loop: status=%q reason=%q",
			terminal["status"], terminal["reason"])
	}
	if !strings.Contains(string(got), "return 7") {
		t.Errorf("the edit did not land: %q", got)
	}
	// It edited the real file rather than replacing it with the model's guess.
	if !strings.Contains(string(got), "range(3)") {
		t.Errorf("the original content was destroyed: %q", got)
	}
	d := ctx.Ledger[ledgerKey(ctx, "app.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Error("the ledger does not describe the final bytes")
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

// The recovery is released by a materially different action on the SAME path,
// and by nothing else, so a path that gets genuinely unstuck and later sticks
// again is not left without one.
func TestSteerRecoveryReleasedBySamePathProgress(t *testing.T) {
	write := map[string]interface{}{"type": "tool_call", "name": "write_file",
		"args": map[string]string{"path": "app.py", "content": "print(7)\n"}}
	_, _, turns, _, _, bounces := steerFixture(t,
		map[string]string{"app.py": steerSeed},
		func(i int, _ string) map[string]interface{} {
			if i == 2 {
				// Materially different, same path, and it succeeds.
				return map[string]interface{}{"type": "tool_call", "name": "edit_file",
					"args": map[string]string{"path": "app.py",
						"old_str": "return 1", "new_str": "return 7"}}
			}
			return write
		})
	offers := 0
	for _, b := range *bounces {
		if strings.HasPrefix(b, "gate|") &&
			(strings.Contains(b, "so here it is") ||
				strings.Contains(b, "You do not need to read it again")) {
			offers++
		}
	}
	t.Logf("released: turns=%d offers=%d", *turns, offers)
	if offers != 2 {
		t.Errorf("%d recoveries across a release, want one before and one after", offers)
	}
}
