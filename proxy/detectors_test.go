package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// syntaxStub answers the whole-file syntax check the terminal consults.
// Without it every observation is not_run, which is fail-closed and correct
// but cannot exercise the passed branch.
func syntaxStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/syntax-check") {
			http.NotFound(w, r)
			return
		}
		var in struct{ Code string }
		json.NewDecoder(r.Body).Decode(&in)
		valid := !strings.Contains(in.Code, "[1,") && !strings.Contains(in.Code, "]]")
		out := map[string]interface{}{"valid": valid}
		if !valid {
			out["errors"] = []string{"SyntaxError: invalid syntax"}
		}
		json.NewEncoder(w).Encode(out)
	}))
}

// The repeat detector's terminal is an honest stop. A productive change
// earlier in the run is a progress hint: it may describe what happened, never
// claim the task is done. Valid bytes change what the terminal discloses,
// never whether it claims success.
func TestEveryTerminalDisclosureRefusesCompletion(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ok.py"), []byte("x = 1\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "bad.py"), []byte("x = [1,\n"), 0o644)
	srv := syntaxStub(t)
	defer srv.Close()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = srv.URL

	for _, tc := range []struct {
		name     string
		expected []string
		wrote    bool
		want     []string
	}{
		{"valid bytes", []string{"ok.py"}, true, []string{"parses", "verification did not complete"}},
		{"invalid bytes", []string{"bad.py"}, true, []string{"not shown to be valid"}},
		{"a declared file that is not there", []string{"gone.py"}, true, []string{"not shown to be valid"}},
		{"nothing declared", nil, true, []string{"not shown to be valid"}},
		{"nothing written", []string{"ok.py"}, false, []string{"nothing was written"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := repeatTerminalSummary(ctx, tc.expected, tc.wrote, nil)
			if !strings.HasPrefix(got, "Stopped:") {
				t.Errorf("not an honest stop:\n%s", got)
			}
			if strings.Contains(got, "Made your change") {
				t.Errorf("completion claim:\n%s", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing disclosure %q:\n%s", want, got)
				}
			}
		})
	}
}

// The model's own `done` is untouched: it is not a breaker terminal and this
// change must not alter ordinary clean completion.
func TestModelIssuedDoneIsUnchanged(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// The model-issued done path streams the model's own summary verbatim.
	if !strings.Contains(body, `case "done":`) {
		t.Fatal("model-issued done branch is gone")
	}
	// repeatTerminalSummary is reachable from exactly one call site: the
	// repeat detector's terminal. It must not have leaked into the ordinary
	// completion path.
	if n := strings.Count(body, "repeatTerminalSummary("); n != 1 {
		t.Errorf("repeatTerminalSummary has %d call sites, want exactly 1", n)
	}
}

// --- The repeat-detector terminal through the real loop ----------------------
//
// Every write_file succeeds: the model rewrites the same file with slightly
// different content each turn. The runaway-write backstop fires twice with
// "you have fully rewritten solve.py N times", and the second detection
// reaches the terminal. Nothing is ever refused, so the refusal-ban terminal
// never engages.
//
// The deliverable stays syntactically invalid throughout: the first write
// creates it (a new file has no healthy prior state, so it lands with a parse
// warning) and every later write is broken->broken, which the healthy->broken
// policy allows as repair in progress.

const repeatedRewriteBroken = "def solve():\n    return [1, 2]]\n"

func repeatedRewriteStubs(t *testing.T, dir, rel string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLoopServices(w, r, dir) {
			return
		}
		i := *calls
		*calls++
		// write_file signatures carry a content fingerprint, so new content
		// every turn is never a repeat. Two variants alternate, the model
		// re-emitting the same broken file from memory: each recurs three
		// times inside the eight-call window, so the detector fires twice.
		variant := "# a\n" + repeatedRewriteBroken
		if i%2 == 1 {
			variant = "# b\n" + repeatedRewriteBroken
		}
		body, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": rel, "content": variant},
		})
		serveReply(w, string(body))
	}))
}

func TestRepeatedRewritesOfABrokenFileReachTheRepeatDetectorTerminal(t *testing.T) {
	dir := t.TempDir()
	rel := "solve.py"
	calls := 0
	srv := repeatedRewriteStubs(t, dir, rel, &calls)
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 40

	var summary string
	interventions, acceptedWrites, banEntries, rescues := 0, 0, 0, 0
	ctx.StreamFn = func(eventType string, data interface{}) {
		b, _ := json.Marshal(data)
		switch eventType {
		case "agent_repeat_intervention":
			interventions++
		case "tool_result":
			if strings.Contains(string(b), `"success":true`) &&
				strings.Contains(string(b), "bytes_written") {
				acceptedWrites++
			}
		case "gate":
			if strings.Contains(string(b), "no longer available") {
				banEntries++
			}
		case "done":
			summary = string(b)
		}
		if strings.Contains(string(b), "named deliverable") ||
			strings.Contains(string(b), "was never created") {
			rescues++
		}
	}
	if err := runAgentLoop(ctx, "Create solve.py that solves the task."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	// --- routing premises: this run reached the repeat-DETECTOR terminal ---
	if interventions < 2 {
		t.Fatalf("second-detection condition not reached: %d repeat interventions", interventions)
	}
	if acceptedWrites == 0 {
		t.Fatal("no write landed, so the productive-change hint was never set")
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("expected output missing, so output-rescue would have fired: %v", err)
	}
	if rescues > 0 {
		t.Fatalf("output-rescue fired (%d); this is not the terminal under test", rescues)
	}
	if banEntries > 0 {
		t.Fatalf("refusal-ban fired (%d); wrong terminal", banEntries)
	}
	if strings.Contains(summary, "re-sent after being refused") {
		t.Fatalf("terminal came from the refusal ban, not the repeat detector:\n%s", summary)
	}
	if !strings.Contains(summary, "kept repeating") && !strings.Contains(summary, "Made your change") {
		t.Fatalf("terminal did not come from the repeat-detector call site:\n%s", summary)
	}

	// --- the behaviour under test ---
	if strings.Contains(summary, "Made your change") {
		t.Errorf("completion claimed over invalid bytes:\n%s", summary)
	}
	if !strings.Contains(summary, "Stopped:") {
		t.Errorf("terminal must be an honest stop:\n%s", summary)
	}
	after, _ := os.ReadFile(filepath.Join(dir, rel))
	if string(after) != string(onDisk) {
		t.Errorf("termination must not rewrite disk")
	}
	if !strings.Contains(string(after), "]]") {
		t.Errorf("the fixture did not leave an invalid deliverable: %q", string(after))
	}
	t.Logf("interventions=%d accepted_writes=%d summary=%s", interventions, acceptedWrites, summary)
}

// --- Restoration at the repeat-detector terminal -----------------------------
//
// A version of the deliverable is written and shown to parse; a shell command
// then rewrites those exact bytes into something that does not; the model
// loops on a verification that keeps failing and the repeat detector stops the
// run. The terminal puts back the last version shown to be valid, says so, and
// still stops.

const restoreGood = "def solve():\n    return [1, 2]\n"

func restoreScenarioStubs(t *testing.T, dir, rel string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLoopServices(w, r, dir) {
			return
		}
		i := *calls
		*calls++

		var body []byte
		switch i {
		case 0:
			body, _ = json.Marshal(map[string]interface{}{
				"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": rel, "content": restoreGood}})
		case 1:
			// The corruption comes from OUTSIDE the edit tools, which is the
			// case no gate can catch: a command rewrites the file after it
			// was shown to parse.
			body, _ = json.Marshal(map[string]interface{}{
				"type": "tool_call", "name": "run_command",
				"args": map[string]string{
					"command": "printf 'def solve():\\n    return [1, 2]]\\n' > " + rel}})
		default:
			// The model then loops on the same verification, which is what
			// stops the run.
			body, _ = json.Marshal(map[string]interface{}{
				"type": "tool_call", "name": "run_command",
				"args": map[string]string{"command": "test -s " + rel}})
		}
		serveReply(w, string(body))
	}))
}

func TestCorruptedDeliverableIsRestoredAtTheRepeatTerminal(t *testing.T) {
	dir := t.TempDir()
	rel := "solve.py"
	calls := 0
	srv := restoreScenarioStubs(t, dir, rel, &calls)
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.V3URL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 40

	var summary string
	interventions := 0
	census := map[string]int{}
	ctx.StreamFn = func(eventType string, data interface{}) {
		b, _ := json.Marshal(data)
		census[eventType]++
		switch eventType {
		case "agent_repeat_intervention":
			interventions++
		case "done":
			summary = string(b)
		}
	}
	if err := runAgentLoop(ctx, "Create solve.py that solves the task."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	// --- routing premises -------------------------------------------------
	if interventions < 2 {
		t.Fatalf("second-detection condition not reached: %d repeat interventions", interventions)
	}
	if !strings.Contains(summary, "kept repeating") {
		t.Fatalf("terminal did not come from the repeat detector:\n%s", summary)
	}

	// --- the behaviour under test ----------------------------------------
	onDisk, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("deliverable missing: %v", err)
	}
	t.Logf("final bytes: %q", string(onDisk))
	t.Logf("summary: %s", summary)
	if string(onDisk) != restoreGood {
		t.Errorf("the demonstrated-broken bytes were left on disk:\n%q", string(onDisk))
	}
	if !strings.Contains(summary, "Put back the last version shown to be valid") {
		t.Errorf("recovery was not disclosed:\n%s", summary)
	}
	if !strings.Contains(summary, rel) {
		t.Errorf("the disclosure does not name the file it recovered:\n%s", summary)
	}
	// Recovery is not completion, in the prose and in the contract.
	var term struct{ Summary, Status, Reason string }
	if err := json.Unmarshal([]byte(summary), &term); err != nil {
		t.Fatalf("terminal payload is not decodable: %v", err)
	}
	if !strings.HasPrefix(term.Summary, "Stopped:") {
		t.Errorf("terminal stopped being a stop:\n%s", term.Summary)
	}
	if term.Status != "stopped" {
		t.Errorf("a restored run reported status %q", term.Status)
	}
	if term.Reason != "repeat_detector" {
		t.Errorf("terminal reason = %q", term.Reason)
	}
	for _, claim := range []string{"Made your change", "the change is on disk"} {
		if strings.Contains(summary, claim) {
			t.Errorf("a restored run claimed %q:\n%s", claim, summary)
		}
	}
	// The completion clause must still be the REFUSAL of completion. After a
	// restore the file does parse again, so this is the branch that is most
	// tempting to misread as success.
	if !strings.Contains(summary, "cannot say the task is done") {
		t.Errorf("the terminal stopped refusing completion:\n%s", summary)
	}
	// Never presented as a transaction.
	if strings.Contains(summary, "rolled back the workspace") {
		t.Errorf("recovery implied transactionality:\n%s", summary)
	}
	// Recovery is not a tool call.
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("recovery broke the call/result invariant: %d vs %d",
			census["tool_call"], census["tool_result"])
	}
}

// Phase 3B is scoped to ONE terminal. Twelve other done emitters exist, and
// silently attaching recovery to them would turn an evidence-bound action
// into a routine one.
func TestRestorationIsWiredToExactlyOneTerminal(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// Two sites, both deliberate: the repeat detector's terminal (Phase 3B)
	// and the work-deadline finaliser (Phase 2B), which reuses the same
	// eligibility rules rather than relaxing them for a timeout.
	if n := strings.Count(body, "restoreSaferDeliverables("); n != 2 {
		t.Errorf("restoration has %d call sites, want exactly 2", n)
	}
	i := strings.Index(body, "restoreSaferDeliverables(")
	if i < 0 {
		t.Fatal("restoration call site not found")
	}
	if !strings.Contains(body[i:min(len(body), i+400)], "repeatTerminalSummary(") {
		t.Error("restoration is no longer adjacent to the repeat-detector terminal")
	}
	j := strings.LastIndex(body, "restoreSaferDeliverables(")
	if !strings.Contains(body[max(0, j-900):j], "finalizeOnWorkDeadline") {
		t.Error("the second restoration site is not the work-deadline finaliser")
	}
	// The other terminal producers must not have gained it. They all route
	// through the one emitter now, so the count to hold is theirs.
	if n := strings.Count(body, "emitTerminal("); n < 12 {
		t.Errorf("found %d emitTerminal call sites, expected the full set of "+
			"producers; if one was removed, re-check this scope deliberately", n)
	}
	if n := strings.Count(body, `Stream("done"`); n != 1 {
		t.Errorf("found %d direct done payloads, want exactly 1 (the emitter)", n)
	}
}

// --- The atomic terminal contract -------------------------------------------

// Every producer routes through the one emitter, and the emitter is the only
// place a done payload is built. A half-migrated producer would emit a
// terminal with no status, which consumers must read as incomplete -- so the
// defect would be invisible in behaviour and visible only here.
func TestEveryTerminalProducerGoesThroughTheOneEmitter(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if n := strings.Count(body, `Stream("done"`); n != 1 {
		t.Errorf("%d direct done payloads outside the emitter, want 0", n-1)
	}
	// 12 producers: 8 direct plus the 4 that share endStream. It was 13 until
	// the delete pre-empt was removed -- delete_file no longer ends the
	// session on the spot, so file_operation_no_task_intent has no producer.
	producers := strings.Count(body, "emitTerminal(") - 1 // the definition
	if producers < 12 {
		t.Errorf("found %d terminal producers, want at least 12; a producer "+
			"that stopped emitting is a session that ends in silence", producers)
	}
	for _, reason := range []string{
		"workspace_misaligned", "inference_failed", "text_instead_of_work",
		"unusable_model_output",
		"failure_ceiling", "same_target_failures", "turn_budget_exhausted",
		"oversized_tool_content", "repeated_refusal", "repeat_detector",
	} {
		if !strings.Contains(body, `"`+reason+`"`) {
			t.Errorf("terminal reason %q is gone; reasons are a stable "+
				"machine-readable contract", reason)
		}
	}
}

func TestUnclassifiedStatusFailsClosedAtTheEmitter(t *testing.T) {
	for _, raw := range []string{"", "COMPLETED", "done", "success", "ok", "finished"} {
		if NormalizeTerminalStatus(raw).Completed() {
			t.Errorf("consumer read %q as completed", raw)
		}
		if NormalizeTerminalStatus(raw) != TerminalIncomplete {
			t.Errorf("consumer read %q as %q, want incomplete",
				raw, NormalizeTerminalStatus(raw))
		}
	}
	if !NormalizeTerminalStatus("completed").Completed() {
		t.Error("a real completion stopped being one")
	}

	// A producer that names nothing must not imply an outcome.
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	var got map[string]string
	ctx.StreamFn = func(eventType string, data interface{}) {
		if eventType == "done" {
			b, _ := json.Marshal(data)
			json.Unmarshal(b, &got)
		}
	}
	emitTerminal(ctx, nil, TerminalStatus("nonsense"), "made_up", "whatever")
	if got["status"] != string(TerminalIncomplete) {
		t.Errorf("emitter accepted an unclassified status: %v", got)
	}
	if got["reason"] != "unclassified_producer" {
		t.Errorf("reason = %q, want the producer defect named", got["reason"])
	}
}

// Exactly one terminal per session, whatever races.
func TestOnlyOneTerminalEventPerSession(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	var mu sync.Mutex
	var terminals []map[string]string
	ctx.StreamFn = func(eventType string, data interface{}) {
		if eventType != "done" {
			return
		}
		b, _ := json.Marshal(data)
		var m map[string]string
		json.Unmarshal(b, &m)
		mu.Lock()
		terminals = append(terminals, m)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				emitTerminal(ctx, nil, TerminalTimedOut, "work_deadline", "timed out")
			} else {
				emitTerminal(ctx, nil, TerminalCompleted, "deliverables_demonstrated", "done")
			}
		}(i)
	}
	wg.Wait()
	if len(terminals) != 1 {
		t.Fatalf("%d terminal events for one session", len(terminals))
	}
	// Whichever won, the recorded outcome and the emitted one agree.
	if terminals[0]["status"] != string(ctx.TerminalStatus) {
		t.Errorf("emitted %q but recorded %q", terminals[0]["status"], ctx.TerminalStatus)
	}
}

// The legacy key keeps its exact meaning and position, so a consumer that
// never learned about status reads what it always read.
func TestLegacyConsumersStillSeeSummaryOnly(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	var raw string
	ctx.StreamFn = func(eventType string, data interface{}) {
		if eventType == "done" {
			b, _ := json.Marshal(data)
			raw = string(b)
		}
	}
	emitTerminal(ctx, nil, TerminalStopped, "repeat_detector", "Stopped: because.")

	var legacy struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		t.Fatalf("a legacy decoder cannot read the payload: %v", err)
	}
	if legacy.Summary != "Stopped: because." {
		t.Errorf("summary changed for legacy readers: %q", legacy.Summary)
	}
}

// syntaxVerdictCtx is a session in a fresh folder whose sandbox answers every
// syntax check with the same verdict and refuses everything else.
func syntaxVerdictCtx(t *testing.T, valid bool) (*AgentContext, string) {
	t.Helper()
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/syntax-check") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"valid": valid})
	}))
	t.Cleanup(srv.Close)
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	return ctx, dir
}

// The completion rule, stated as the outcomes it must produce.
func TestCompletionRequiresADemonstratedObligation(t *testing.T) {
	t.Run("nothing declared and nothing written", func(t *testing.T) {
		ctx, _ := syntaxVerdictCtx(t, true)
		ok, reason := terminalCompletionAllowed(ctx, nil)
		if !ok || reason != "no_file_obligation" {
			t.Errorf("ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("declared deliverable that parses", func(t *testing.T) {
		ctx, dir := syntaxVerdictCtx(t, true)
		os.WriteFile(filepath.Join(dir, "solve.py"), []byte("A = 1\n"), 0o644)
		if ok, _ := terminalCompletionAllowed(ctx, []string{"solve.py"}); !ok {
			t.Error("a demonstrated deliverable was refused")
		}
	})

	t.Run("declared deliverable that does not parse", func(t *testing.T) {
		ctx, dir := syntaxVerdictCtx(t, false)
		os.WriteFile(filepath.Join(dir, "solve.py"), []byte("def f(\n"), 0o644)
		ok, reason := terminalCompletionAllowed(ctx, []string{"solve.py"})
		if ok {
			t.Error("invalid bytes authorized completion")
		}
		if reason != "deliverables_not_demonstrated" {
			t.Errorf("reason = %q", reason)
		}
	})

	t.Run("declared deliverable that is not there", func(t *testing.T) {
		ctx, _ := syntaxVerdictCtx(t, true)
		if ok, _ := terminalCompletionAllowed(ctx, []string{"missing.py"}); ok {
			t.Error("a missing deliverable authorized completion")
		}
	})

	t.Run("validation unavailable", func(t *testing.T) {
		ctx, dir := syntaxVerdictCtx(t, true)
		ctx.SandboxURL = "http://127.0.0.1:1"
		os.WriteFile(filepath.Join(dir, "solve.py"), []byte("A = 1\n"), 0o644)
		if ok, _ := terminalCompletionAllowed(ctx, []string{"solve.py"}); ok {
			t.Error("an unknown verdict authorized completion")
		}
	})
}

// What the session did through its own tools counts too: a file it wrote but
// never declared, and a deliverable it deleted.
func TestTheSessionsOwnToolCallsCanBlockCompletion(t *testing.T) {
	t.Run("a file the session wrote but never declared", func(t *testing.T) {
		ctx, _ := syntaxVerdictCtx(t, false)
		args, _ := json.Marshal(map[string]string{"path": "side.py", "content": "def f(\n"})
		executeToolCall("write_file", args, ctx)
		if ok, _ := terminalCompletionAllowed(ctx, nil); ok {
			t.Error("an undeclared broken file the run wrote authorized completion")
		}
	})

	t.Run("deleting the deliverable cannot authorize completion", func(t *testing.T) {
		ctx, _ := syntaxVerdictCtx(t, true)
		w, _ := json.Marshal(map[string]string{"path": "solve.py", "content": "A = 1\n"})
		executeToolCall("write_file", w, ctx)
		d, _ := json.Marshal(map[string]string{"path": "solve.py"})
		executeToolCall("delete_file", d, ctx)
		ok, reason := terminalCompletionAllowed(ctx, []string{"solve.py"})
		if ok {
			t.Fatal("removing the deliverable authorized completion")
		}
		if reason != "delete_intent_unestablished" {
			t.Errorf("reason = %q, want the delete intent named", reason)
		}
	})
}

// --- The summary a non-completed run may carry -----------------------------
//
// `summary` is the only field an older client reads. On a run that did not
// complete it must not carry the model's claim of success over work nothing
// verified, and it must never be empty.

// falseSuccessSummaries are real summaries of that kind, verbatim.
var falseSuccessSummaries = map[string]string{
	"debounce5": "Made your change. The follow-up verification command kept repeating and " +
		"failing (often a typo in the command, not the edit) — the change is on disk; run it " +
		"yourself to confirm.",
	"ledger2": "I have verified the contents of input.txt and confirmed that the logic in " +
		"solve.py correctly processes the data. The script identifies 4 settle events and " +
		"determines that the final balance is correct.",
	"overlay3": "I have successfully implemented the interval priority logic in `solve.py`. " +
		"The script correctly processes `input.txt`, identifies the highest priority at each " +
		"point, and calculates the total.",
	"ring5": "I wrote solve.py which implements a ring buffer of capacity 6. It correctly " +
		"handles 'push' (with overwrites), 'pop', 'rot' (rotating the oldest K items to the " +
		"end), and prints the final state.",
}

func TestFalseSuccessSummariesNeverShipOnANonCompletedRun(t *testing.T) {
	for task, prose := range falseSuccessSummaries {
		t.Run(task, func(t *testing.T) {
			// The claim is detected as a claim.
			if completionClaimIn(prose) == "" {
				t.Fatalf("no completion claim detected in the summary:\n%s", prose)
			}
			ctx := NewAgentContext(t.TempDir(), Tier2Medium)
			st := &runState{madeProductiveChange: true}
			got := honestTerminalSummary(ctx, st, TerminalIncomplete, "deliverables_not_demonstrated", prose)
			if completionClaimIn(got) != "" {
				t.Errorf("a completion claim survived:\n%s", got)
			}
			if !hasHonestMarker(got) {
				t.Errorf("the replacement does not say the run did not finish:\n%s", got)
			}
			if strings.Contains(got, prose) {
				t.Error("the model's account was reproduced verbatim")
			}
		})
	}
}

// Negated language is a report of failure, not a claim, and must survive.
func TestHonestReportsAreNotMistakenForClaims(t *testing.T) {
	for _, s := range []string{
		"Nothing was written to disk in this run, and no verification command completed successfully.",
		"Changes were written to disk, but NOTHING in this run verified them.",
		"Stopped: the same tool call kept repeating without making progress. Your work is on disk " +
			"and parses, but the verification did not complete, so this run cannot say the task is done.",
		"Stopped: the session ran out of time before the work finished, and nothing was written to disk.",
		"I ran out of turns for this request before finishing. Nothing was written to disk.",
		"Stopped after 3 tool failures on the same target with no successful changes.",
	} {
		if claim := completionClaimIn(s); claim != "" {
			t.Errorf("honest report flagged as the claim %q:\n%s", claim, s)
		}
	}
}

func TestEveryNonCompletedTerminalIsHonest(t *testing.T) {
	// One row per terminal producer, with the status and reason it emits.
	producers := []struct {
		reason  string
		status  TerminalStatus
		summary string
	}{
		{"workspace_misaligned", TerminalFailed, "proxy and sandbox workspaces are not aligned"},
		{"inference_failed", TerminalFailed, "Stopped: the model call failed, so the run could not continue."},
		{"text_instead_of_work", TerminalIncomplete, "The reply was cut short — it had begun repeating itself."},
		{"unusable_model_output", TerminalStopped, "Stopped after 3 unparseable responses."},
		{"deliverables_not_demonstrated", TerminalIncomplete, ""},
		{"delete_intent_unestablished", TerminalIncomplete, ""},
		{"text_reply", TerminalIncomplete, ""},
		{"failure_ceiling", TerminalStopped, "Stopped after 9 failed tool calls with nothing landing on disk."},
		{"same_target_failures", TerminalStopped, "Wrote your changes to disk; couldn't verify them automatically."},
		{"turn_budget_exhausted", TerminalIncomplete, "I ran out of turns for this request before finishing."},
		{"oversized_tool_content", TerminalStopped, "Stopped: content too large for tool calls."},
		{"repeated_refusal", TerminalStopped, "Stopped: the same `write_file` call was re-sent after being refused."},
		{"repeat_detector", TerminalStopped, "Stopped: the same tool call kept repeating without making progress."},
		{"work_deadline", TerminalTimedOut, "Stopped: the session ran out of time before the work finished."},
		{"cancelled", TerminalIncomplete, "Stopped: the run was cancelled before the work finished."},
		{"unclassified_producer", TerminalIncomplete, ""},
		{"clarification_requested", TerminalIncomplete, ""},
		{"investigation_handed_back", TerminalIncomplete, ""},
	}
	// Each producer runs twice: with nothing written, and after a write.
	for _, p := range producers {
		for _, run := range []struct {
			nameSuffix string
			wrote      bool
		}{
			{"", false},
			{"/wrote", true},
		} {
			t.Run(p.reason+run.nameSuffix, func(t *testing.T) {
				dir := t.TempDir()
				ctx := NewAgentContext(dir, Tier2Medium)
				st := &runState{madeProductiveChange: run.wrote}
				got := honestTerminalSummary(ctx, st, p.status, p.reason, p.summary)
				if strings.TrimSpace(got) == "" {
					t.Fatal("a terminal shipped with no summary at all")
				}
				if claim := completionClaimIn(got); claim != "" {
					t.Errorf("completion claim %q on a %s terminal:\n%s", claim, p.status, got)
				}
				if !hasHonestMarker(got) {
					t.Errorf("summary never says the run did not finish:\n%s", got)
				}
			})
		}
	}
}

// The fallback reports the artifact state it can actually establish.
func TestFallbackDescribesTheArtifactItCanSee(t *testing.T) {
	t.Run("timed out with a valid artifact", func(t *testing.T) {
		ctx, dir := syntaxVerdictCtx(t, true)
		os.WriteFile(filepath.Join(dir, "solve.py"), []byte("A = 1\n"), 0o644)
		st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
		got := honestTerminalSummary(ctx, st, TerminalTimedOut, "work_deadline", "")
		if !strings.Contains(got, "parses") {
			t.Errorf("valid bytes not disclosed: %s", got)
		}
		if completionClaimIn(got) != "" || !hasHonestMarker(got) {
			t.Errorf("not honest: %s", got)
		}
	})

	t.Run("timed out with unverified bytes", func(t *testing.T) {
		ctx, dir := syntaxVerdictCtx(t, false)
		os.WriteFile(filepath.Join(dir, "solve.py"), []byte("def f(\n"), 0o644)
		st := &runState{madeProductiveChange: true, expectedOutputs: []string{"solve.py"}}
		got := honestTerminalSummary(ctx, st, TerminalTimedOut, "work_deadline", "")
		if !strings.Contains(got, "unverified") {
			t.Errorf("invalid bytes not disclosed as unverified: %s", got)
		}
	})

	t.Run("stopped with nothing on disk", func(t *testing.T) {
		ctx, _ := syntaxVerdictCtx(t, true)
		st := &runState{}
		got := honestTerminalSummary(ctx, st, TerminalStopped, "repeat_detector", "")
		if !strings.Contains(got, "Nothing was written") {
			t.Errorf("empty workspace not disclosed: %s", got)
		}
	})
}

// A completed run keeps its account, because the gate already agreed with it.
func TestCompletedRunKeepsItsSummary(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	st := &runState{madeProductiveChange: true}
	prose := "I have successfully implemented the interval priority logic in solve.py."
	got := honestTerminalSummary(ctx, st, TerminalCompleted, "deliverables_demonstrated", prose)
	if got != prose {
		t.Errorf("a verified completion had its summary rewritten:\n%s", got)
	}
	if modelProseIfAuthorized(TerminalCompleted, prose) != prose {
		t.Error("authorized prose was withheld")
	}
	if modelProseIfAuthorized(TerminalIncomplete, prose) != "" {
		t.Error("unauthorized prose passed through")
	}
}

// The two kinds of client must not disagree about failure. A legacy client
// reads only `summary`; a structured client reads `status`.
//
// The property is one-directional on purpose. A legacy reader seeing a success
// claim while the status says otherwise is the defect this rule exists to
// prevent. A legacy reader failing to recognise a genuine completion is not:
// prose is not a protocol, and under-claiming is safe. So the assertion is
// that a claim NEVER appears on a non-completed status, and that an authorised
// completion's claim is still allowed through.
func TestLegacyReaderNeverSeesSuccessOnANonCompletedRun(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	cases := []struct {
		status  TerminalStatus
		reason  string
		summary string
	}{
		{TerminalCompleted, "deliverables_demonstrated", "I have successfully implemented solve.py and all tests pass."},
		{TerminalIncomplete, "deliverables_not_demonstrated", falseSuccessSummaries["overlay3"]},
		{TerminalIncomplete, "delete_intent_unestablished", ""},
		{TerminalStopped, "repeat_detector", "Stopped: the same tool call kept repeating."},
		{TerminalTimedOut, "work_deadline", ""},
		{TerminalFailed, "inference_failed", ""},
	}
	for _, c := range cases {
		t.Run(c.reason+"/"+string(c.status), func(t *testing.T) {
			st := &runState{madeProductiveChange: true}
			summary := honestTerminalSummary(ctx, st, c.status, c.reason, c.summary)

			// A legacy client has one signal: does the prose claim success?
			legacySeesClaim := completionClaimIn(summary) != ""
			structuredSaysDone := NormalizeTerminalStatus(string(c.status)).Completed()
			if legacySeesClaim && !structuredSaysDone {
				t.Errorf("legacy reader sees a success claim while the status is %q:\n%s",
					c.status, summary)
			}
			if structuredSaysDone && !legacySeesClaim {
				t.Errorf("an authorised completion had its claim stripped:\n%s", summary)
			}
		})
	}
}

// The production shape, through the real loop: the model declares done with a
// success claim over a file that does not parse.
func TestModelSuccessClaimOverAnInvalidArtifact(t *testing.T) {
	dir := t.TempDir()
	const broken = "def solve():\n    return [1, 2]]\n"
	const claim = "I have successfully implemented solve.py. The script correctly processes input.txt."

	ctx, _, census, terminal := termFixture(t, dir, "Write solve.py.", termCeiling,
		func(i int, _ string) map[string]interface{} {
			switch i {
			case 0:
				// A write that lands with a parse warning, as write_file does for
				// a new file that does not compile.
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "solve.py", "content": broken}}
			case 1:
				// A command that exits zero. The run has a passing verification
				// on record, so the verification gate is satisfied, and only the
				// summary rule stands between the model's confident prose and
				// the client, over a file that does not parse.
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "echo checked"}}
			default:
				return map[string]interface{}{"type": "done", "summary": claim}
			}
		})
	ctx.MaxTurns = 12
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	onDisk, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	t.Logf("summary: %s", terminal["summary"])
	t.Logf("status=%q reason=%q tool_calls=%d disk=%q",
		terminal["status"], terminal["reason"], census["tool_call"], string(onDisk))

	// The behaviour under test: the model's claim does not reach the client.
	for _, phrase := range []string{"successfully implemented", "correctly processes"} {
		if strings.Contains(terminal["summary"], phrase) {
			t.Errorf("the model's claim %q shipped over an invalid artifact:\n%s",
				phrase, terminal["summary"])
		}
	}
	if terminal["status"] == "completed" {
		t.Errorf("invalid bytes reported as completed")
	}
	if strings.TrimSpace(terminal["summary"]) == "" {
		t.Error("the terminal shipped with no summary")
	}
	// Everything else is unchanged: the bytes the model wrote are still there,
	// and the run still made the same calls.
	if string(onDisk) != broken {
		t.Errorf("disk changed: %q", onDisk)
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result invariant broken: %d vs %d",
			census["tool_call"], census["tool_result"])
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

// --- Terminal authorization ---------------------------------------------------
//
// Both completion paths decide success from the same evidence the summary
// uses. Otherwise one terminal event contradicts itself: status "completed",
// reason "no_file_obligation", summary "Nothing was written — no file was
// created or changed in this run."

// termFixture drives the real loop with MaxTurns=0 and a server-side ceiling,
// so nothing sleeps and an unbounded loop fails immediately.
func termFixture(t *testing.T, dir, request string, ceiling int,
	plan func(i int, prompt string) map[string]interface{}) (*AgentContext, *int, map[string]int, map[string]string) {
	t.Helper()
	turns := 0
	census := map[string]int{}
	terminal := map[string]string{}
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "]]")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: unmatched ']'"}
			}
			json.NewEncoder(w).Encode(out)
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
		if strings.Contains(string(raw), "single fenced block") {
			// No fenced block ever arrives: the resolution fails fast.
			serveReply(w, "Sure, here it is.")
			return
		}
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= ceiling {
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i, string(raw)))
		serveReply(w, string(call))
	}))
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
		if et == "done" {
			json.Unmarshal(b, &terminal) // adds the payload's keys to the map
		}
	}
	return ctx, &turns, census, terminal
}

const termCeiling = 30

// Four writes that never land cannot complete, whatever the model claims.
func TestUnnamedDeliverablesNeverCompleteWithNothingWritten(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"a.py", "b.py", "c.py", "d.py"}
	ctx, turns, census, terminal := termFixture(t, dir, "Write four files.", termCeiling,
		func(i int, _ string) map[string]interface{} {
			if i >= len(paths) {
				return map[string]interface{}{"type": "done",
					"summary": "I have successfully written all four files."}
			}
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": paths[i], "content": "@fenced"}}
		})
	if err := runAgentLoop(ctx, "Write four files."); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	t.Logf("turns=%d status=%q reason=%q summary=%.100s",
		*turns, terminal["status"], terminal["reason"], terminal["summary"])

	if terminal["status"] != string(TerminalIncomplete) {
		t.Fatalf("status = %q, want incomplete", terminal["status"])
	}
	if terminal["reason"] != "action_demanded_unmet" {
		t.Errorf("reason = %q, want action_demanded_unmet", terminal["reason"])
	}
	if completionClaimIn(terminal["summary"]) != "" {
		t.Errorf("the model's claim reached the summary:\n%s", terminal["summary"])
	}
	if !hasHonestMarker(terminal["summary"]) {
		t.Errorf("summary is not honest:\n%s", terminal["summary"])
	}
	if len(ctx.Ledger) != 0 {
		t.Errorf("a path the session never wrote entered the ledger: %v", ctx.Ledger)
	}
	var found []string
	filepath.Walk(dir, func(p string, i os.FileInfo, e error) error {
		if e == nil && i != nil && !i.IsDir() && !strings.Contains(p, "mount-probe") {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Errorf("files on disk: %v", found)
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result balance: %d vs %d", census["tool_call"], census["tool_result"])
	}
}

// The text exit starts at completed and only downgrades.
func TestTextExitCannotCompleteAnUnmetActionRequest(t *testing.T) {
	dir := t.TempDir()
	ctx, _, census, terminal := termFixture(t, dir, "Write four files.", termCeiling,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "text",
				"content": "I have successfully implemented all four files as requested."}
		})
	if err := runAgentLoop(ctx, "Write four files."); err != nil {
		t.Fatal(err)
	}
	t.Logf("text exit: status=%q reason=%q summary=%.100s",
		terminal["status"], terminal["reason"], terminal["summary"])
	if terminal["status"] != string(TerminalIncomplete) {
		t.Fatalf("status = %q, want incomplete", terminal["status"])
	}
	if terminal["reason"] != "action_demanded_unmet" {
		t.Errorf("reason = %q", terminal["reason"])
	}
	if completionClaimIn(terminal["summary"]) != "" {
		t.Errorf("the model's completion prose reached the summary:\n%s", terminal["summary"])
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

// Verification demanded and unmet, and the precedence when both are unmet.
func TestUnmetVerificationDowngradesAndActionWins(t *testing.T) {
	t.Run("verification unmet after a real write", func(t *testing.T) {
		dir := t.TempDir()
		const body = "def solve():\n    return 1\n"
		ctx, _, _, terminal := termFixture(t, dir,
			"Create solve.py and run the tests to verify it.", termCeiling,
			func(i int, _ string) map[string]interface{} {
				if i == 0 {
					return map[string]interface{}{"type": "tool_call", "name": "write_file",
						"args": map[string]string{"path": "solve.py", "content": body}}
				}
				return map[string]interface{}{"type": "done", "summary": "wrote it"}
			})
		if err := runAgentLoop(ctx, "Create solve.py and run the tests to verify it."); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
		t.Logf("verify-unmet: status=%q reason=%q disk=%q",
			terminal["status"], terminal["reason"], string(got))
		if terminal["status"] == string(TerminalCompleted) {
			t.Errorf("completed with verification demanded and unmet")
		}
		if terminal["reason"] != "verification_demanded_unmet" {
			t.Errorf("reason = %q, want verification_demanded_unmet", terminal["reason"])
		}
	})

	t.Run("both unmet: action wins", func(t *testing.T) {
		dir := t.TempDir()
		ctx, _, _, terminal := termFixture(t, dir,
			"Create some files and run the tests to verify them.", termCeiling,
			func(i int, _ string) map[string]interface{} {
				return map[string]interface{}{"type": "done", "summary": "all set"}
			})
		if err := runAgentLoop(ctx, "Create some files and run the tests to verify them."); err != nil {
			t.Fatal(err)
		}
		t.Logf("both-unmet: status=%q reason=%q", terminal["status"], terminal["reason"])
		if terminal["status"] != string(TerminalIncomplete) {
			t.Fatalf("status = %q", terminal["status"])
		}
		if terminal["reason"] != "action_demanded_unmet" {
			t.Errorf("reason = %q, want action to take precedence", terminal["reason"])
		}
	})
}

// A genuine question still completes, through both exits.
func TestReadOnlyRequestsStillComplete(t *testing.T) {
	for _, exit := range []string{"done", "text"} {
		t.Run(exit, func(t *testing.T) {
			dir := t.TempDir()
			const q = "What does this project do?"
			ctx, _, _, terminal := termFixture(t, dir, q, termCeiling,
				func(i int, _ string) map[string]interface{} {
					if exit == "text" {
						return map[string]interface{}{"type": "text",
							"content": "It is a small solver: solve.py reads input.txt and prints a total."}
					}
					return map[string]interface{}{"type": "done",
						"summary": "It is a small solver that reads input.txt and prints a total."}
				})
			if err := runAgentLoop(ctx, q); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s exit: status=%q reason=%q", exit, terminal["status"], terminal["reason"])
			if terminal["status"] != string(TerminalCompleted) {
				t.Errorf("a read-only question did not complete: status=%q reason=%q",
					terminal["status"], terminal["reason"])
			}
		})
	}
}

// The successful paths complete.
func TestSuccessfulCompletionsAreUnchanged(t *testing.T) {
	// The model writes solve.py, runs it, and claims it is done (the claim
	// repeats on any later turn).
	plan := script(
		stepWrite("solve.py", "def solve():\n    return 1\n\nprint(solve())\n"),
		stepRun("python3 solve.py"),
		stepDone("I have successfully created solve.py and ran it."))
	run := func(t *testing.T, request string) (map[string]string, *AgentContext, string) {
		dir := t.TempDir()
		ctx, _, _, terminal := termFixture(t, dir, request, termCeiling,
			func(i int, _ string) map[string]interface{} { return plan(i) })
		if err := runAgentLoop(ctx, request); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
		return terminal, ctx, string(got)
	}

	t.Run("named deliverable", func(t *testing.T) {
		terminal, ctx, got := run(t, "Create solve.py that prints 1, then run it.")
		t.Logf("named: status=%q reason=%q", terminal["status"], terminal["reason"])
		if terminal["status"] != string(TerminalCompleted) {
			t.Fatalf("status=%q reason=%q", terminal["status"], terminal["reason"])
		}
		d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
		if d == nil || d.CurrentHash != hashBytes([]byte(got)) {
			t.Error("ledger does not describe the final bytes")
		}
		if k, s := d.CurrentValidation(); s != ValidationPassed || k != ValidationKindSyntax {
			t.Errorf("completion over %v/%v", k, s)
		}
		// A genuinely completed run keeps the model's account.
		if completionClaimIn(terminal["summary"]) == "" {
			t.Errorf("an authorised completion had its claim stripped:\n%s", terminal["summary"])
		}
	})

	t.Run("unnamed deliverable actually written", func(t *testing.T) {
		terminal, _, _ := run(t, "Write a small script and run it.")
		t.Logf("unnamed: status=%q reason=%q", terminal["status"], terminal["reason"])
		if terminal["status"] != string(TerminalCompleted) {
			t.Errorf("a run that wrote and verified did not complete: status=%q reason=%q",
				terminal["status"], terminal["reason"])
		}
	})
}

// A deliverable that is written, "checked" by a command that proves nothing,
// and claimed done ends with the most specific reason there is. An
// unparseable file the session wrote is an open repair: repair_unfinished
// outranks deliverables_not_demonstrated like that reason outranks the demand
// reasons. With nothing to repair, deliverables_not_demonstrated still wins:
// a deliverable no check applies to cannot be shown valid.
func TestAnUnfinishedDeliverableEndsWithTheMostSpecificReason(t *testing.T) {
	for _, c := range []struct {
		name, request, path, content, wantReason string
	}{
		{"a broken artifact ends repair_unfinished", "Create solve.py that prints the list.",
			"solve.py", "def solve():\n    return [1, 2]]\n", "repair_unfinished"},
		{"deliverables_not_demonstrated still wins when it applies", "Create solve.dat that holds the list.",
			"solve.dat", "1 2\n", "deliverables_not_demonstrated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx, _, _, terminal := termFixture(t, dir, c.request, termCeiling,
				func(i int, _ string) map[string]interface{} {
					switch i {
					case 0:
						return map[string]interface{}{"type": "tool_call", "name": "write_file",
							"args": map[string]string{"path": c.path, "content": c.content}}
					case 1:
						return map[string]interface{}{"type": "tool_call", "name": "run_command",
							"args": map[string]string{"command": "echo checked"}}
					default:
						return map[string]interface{}{"type": "done", "summary": "done"}
					}
				})
			if err := runAgentLoop(ctx, c.request); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: status=%q reason=%q", c.path, terminal["status"], terminal["reason"])
			if terminal["status"] == string(TerminalCompleted) {
				t.Fatalf("%s completed", c.path)
			}
			if terminal["reason"] != c.wantReason {
				t.Errorf("reason = %q, want %q — a more specific existing failure was replaced",
					terminal["reason"], c.wantReason)
			}
		})
	}
}

// Refused unsafe mutations cannot become success by leaving no trace.
func TestRefusedUnsafeMutationsCannotComplete(t *testing.T) {
	for _, c := range []struct{ name, path string }{
		{"deny-listed", ".env"},
		{"path escape", "../outside.py"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, ".env"), []byte("S=1\n"), 0o644)
			// The model tries the refused write twice, then says it is done.
			plan := script(stepWrite(c.path, "SECRET=2\n"), stepWrite(c.path, "SECRET=2\n"),
				stepDone("wrote the config"))
			ctx, _, _, terminal := termFixture(t, dir, "Write the config file.", termCeiling,
				func(i int, _ string) map[string]interface{} { return plan(i) })
			if err := runAgentLoop(ctx, "Write the config file."); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: status=%q reason=%q", c.name, terminal["status"], terminal["reason"])
			if terminal["status"] == string(TerminalCompleted) {
				t.Errorf("a refused unsafe mutation completed: reason=%q", terminal["reason"])
			}
			if got, _ := os.ReadFile(filepath.Join(dir, ".env")); string(got) != "S=1\n" {
				t.Errorf(".env changed: %q", got)
			}
			if len(ctx.Ledger) != 0 {
				t.Errorf("a refused path entered the ledger: %v", ctx.Ledger)
			}
		})
	}
}

// Structural guard: both exits must reach the SAME finalizer, so a future
// producer cannot consult unmet-action evidence only for prose.
func TestBothCompletionPathsShareOneDecision(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if n := strings.Count(body, "finalizeCompletion("); n < 3 {
		t.Errorf("finalizeCompletion has %d references (definition + 2 exits expected); "+
			"both completion paths must share one decision", n)
	}
	// terminalCompletionAllowed is the finalizer's input, not a producer's.
	if n := strings.Count(body, "terminalCompletionAllowed("); n != 2 {
		t.Errorf("terminalCompletionAllowed has %d references, want 2 (definition + "+
			"the single call inside finalizeCompletion); a producer calling it "+
			"directly bypasses the unmet-action evidence", n)
	}
}

// --- Non-code deliverables ---------------------------------------------------
//
// A text deliverable has no checker, so its validation is not_applicable, and
// it may still complete. Treating every not_applicable as valid would be
// unsafe, because an unsupported language reports not_applicable for another
// reason. The document set (isDocumentAsset) tells the two apart.

func TestDocumentAssetClassification(t *testing.T) {
	for _, c := range []struct {
		path string
		want bool
		why  string
	}{
		{"notes.txt", true, "ordinary text"},
		{"README.md", true, "markdown document"},
		{"guide.markdown", true, "markdown document"},
		{"spec.rst", true, "restructured text"},
		{"solve.py", false, "recognized source"},
		{"lib.rs", false, "unsupported source, not prose"},
		{"main.c", false, "unsupported source"},
		{"app.jinja", false, "template with executable content"},
		{"data.json", false, "structured, has a parser"},
		{"config.yaml", false, "structured, has a parser"},
		{"page.html", false, "may carry embedded scripts"},
		{"weird.zzz", false, "unknown extension"},
		{"Makefile", false, "no extension, not prose"},
	} {
		if got := isDocumentAsset(c.path); got != c.want {
			t.Errorf("isDocumentAsset(%q) = %v, want %v (%s)", c.path, got, c.want, c.why)
		}
	}
}

// bracketCheckCtx is a session in a fresh folder whose sandbox fails the
// syntax check of code that holds "]]", passes any other code, and refuses
// everything else.
func bracketCheckCtx(t *testing.T) (*AgentContext, string) {
	t.Helper()
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/syntax-check") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		var in struct{ Code, Language string }
		json.NewDecoder(r.Body).Decode(&in)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"valid": !strings.Contains(in.Code, "]]")})
	}))
	t.Cleanup(srv.Close)
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.SandboxURL = srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	return ctx, dir
}

// Which written deliverables may demonstrate completion: a document may, code
// with no applicable check may not, and a structured format goes through its
// own parser.
func TestNonCodeDeliverableDemonstration(t *testing.T) {
	const code = "fn main() { let x = 1; }\n"
	for _, c := range []struct {
		name, path, body string
		want             bool
	}{
		{"markdown may demonstrate", "README.md", "# Title\n\nSome prose.\n", true},
		{"unsupported code cannot: lib.rs", "lib.rs", code, false},
		{"unsupported code cannot: main.c", "main.c", code, false},
		{"unsupported code cannot: weird.zzz", "weird.zzz", code, false},
		{"unsupported code cannot: app.jinja", "app.jinja", code, false},
		{"valid json passes its own parser", "data.json", "{\"a\": 1}\n", true},
		{"invalid json fails its own parser", "bad.json", "{\"a\": [1, 2]]}\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, _ := bracketCheckCtx(t)
			args, _ := json.Marshal(map[string]string{"path": c.path, "content": c.body})
			executeToolCall("write_file", args, ctx)
			if got := deliverablesDemonstrablyValid(ctx, []string{c.path}); got != c.want {
				t.Errorf("%s demonstrates completion: %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// An ordinary text file demonstrates as not_applicable, and demonstrating it
// does not relabel its record.
func TestATextDeliverableMayDemonstrate(t *testing.T) {
	ctx, _ := bracketCheckCtx(t)
	args, _ := json.Marshal(map[string]string{"path": "notes.txt", "content": "ordinary notes\n"})
	res := executeToolCall("write_file", args, ctx)
	if res.ValidationKind != ValidationKindNone || res.ValidationStatus != ValidationNotApplicable {
		t.Fatalf("expected none/not_applicable, got %s/%s",
			res.ValidationKind, res.ValidationStatus)
	}
	if !deliverablesDemonstrablyValid(ctx, []string{"notes.txt"}) {
		t.Error("a current, ordinary text deliverable could not demonstrate")
	}
	// The internal record is untouched: no fake syntax pass.
	d := ctx.Ledger[ledgerKey(ctx, "notes.txt")]
	k, s := d.CurrentValidation()
	if k != ValidationKindNone || s != ValidationNotApplicable {
		t.Errorf("the ledger was relabelled: %v/%v", k, s)
	}
}

func TestRecognizedCodeWithTheCheckerUnavailableCannotDemonstrate(t *testing.T) {
	ctx, _ := bracketCheckCtx(t)
	ctx.SandboxURL = "http://127.0.0.1:1"
	args, _ := json.Marshal(map[string]string{"path": "solve.py", "content": "A = 1\n"})
	executeToolCall("write_file", args, ctx)
	if deliverablesDemonstrablyValid(ctx, []string{"solve.py"}) {
		t.Error("an unchecked .py demonstrated completion")
	}
}

// A document that may demonstrate is still held to every other rule: its
// bytes must be the ones the session wrote, and the run's demands must be met.
func TestADemonstrableDocumentIsStillHeldToTheOtherRules(t *testing.T) {
	t.Run("bytes changed after observation cannot", func(t *testing.T) {
		ctx, dir := bracketCheckCtx(t)
		args, _ := json.Marshal(map[string]string{"path": "notes.txt", "content": "ordinary notes\n"})
		executeToolCall("write_file", args, ctx)
		os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("changed behind us\n"), 0o644)
		if deliverablesDemonstrablyValid(ctx, []string{"notes.txt"}) {
			t.Error("a stale record demonstrated completion")
		}
	})

	t.Run("a path the session never wrote cannot", func(t *testing.T) {
		ctx, dir := bracketCheckCtx(t)
		os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("pre-existing\n"), 0o644)
		if deliverablesDemonstrablyValid(ctx, []string{"notes.txt"}) {
			t.Error("a file the session never owned demonstrated completion")
		}
	})

	t.Run("unmet verification and outstanding work still block", func(t *testing.T) {
		ctx, _ := bracketCheckCtx(t)
		write, _ := json.Marshal(map[string]string{"path": "notes.txt", "content": "ordinary notes\n"})
		executeToolCall("write_file", write, ctx)
		st := &runState{madeProductiveChange: true, expectedOutputs: []string{"notes.txt"}}
		// Requested verification, never satisfied.
		st.userWantsVerification = true
		if status, reason := finalizeCompletion(ctx, st, "Write notes.txt and verify it.", ""); status.Completed() {
			t.Errorf("completed with verification unmet: reason=%q", reason)
		}
		// Outstanding work elsewhere.
		st2 := &runState{madeProductiveChange: true, expectedOutputs: []string{"notes.txt"}}
		args, _ := json.Marshal(map[string]string{"path": "other.py", "content": "@fenced"})
		noteMutationIntent(ctx, st2, "write_file", args)
		if status, reason := finalizeCompletion(ctx, st2, "Write the files.", ""); status.Completed() {
			t.Errorf("completed with unresolved work: reason=%q", reason)
		}
	})
}

// wroteIt is the whole done summary of the two loop tests below.
const wroteIt = "wrote it"

// The production shape, through the real loop: an ordinary text file
// completes, and its record stays what it was.
func TestNonCodeDeliverableCompletesThroughTheLoop(t *testing.T) {
	dir := t.TempDir()
	const req = "Write the file."
	// The model writes the file, then says it is done.
	plan := script(stepWrite("notes.txt", "the notes you asked for\n"), stepDone(wroteIt))
	ctx, _, _, terminal := termFixture(t, dir, req, termCeiling,
		func(i int, _ string) map[string]interface{} { return plan(i) })
	if err := runAgentLoop(ctx, req); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "notes.txt"))
	d := ctx.Ledger[ledgerKey(ctx, "notes.txt")]
	t.Logf("ordinary text: status=%q reason=%q hash_matches=%v",
		terminal["status"], terminal["reason"], d != nil && d.CurrentHash == hashBytes(got))
	if terminal["status"] != string(TerminalCompleted) {
		t.Errorf("status = %q, want completed (reason=%q)", terminal["status"], terminal["reason"])
	}
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Error("the ledger hash does not match disk")
	}
	if k, s := d.CurrentValidation(); k != ValidationKindNone || s != ValidationNotApplicable {
		t.Errorf("the internal record was relabelled: %v/%v", k, s)
	}
}

// The same shape with code no check applies to does not complete.
func TestUnsupportedCodeDoesNotCompleteThroughTheLoop(t *testing.T) {
	dir := t.TempDir()
	const req = "Write the file."
	// The model writes the file, then says it is done.
	plan := script(stepWrite("lib.rs", "fn main() { let x = 1; }\n"), stepDone(wroteIt))
	ctx, _, _, terminal := termFixture(t, dir, req, termCeiling,
		func(i int, _ string) map[string]interface{} { return plan(i) })
	if err := runAgentLoop(ctx, req); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "lib.rs"))
	d := ctx.Ledger[ledgerKey(ctx, "lib.rs")]
	t.Logf("unsupported code: status=%q reason=%q hash_matches=%v",
		terminal["status"], terminal["reason"], d != nil && d.CurrentHash == hashBytes(got))
	if terminal["status"] == string(TerminalCompleted) {
		t.Errorf("unsupported code completed (reason=%q)", terminal["reason"])
	}
}

// --- Workspace-boundary refusals must be bounded -----------------------------
//
// A session that runs against a workspace root that does not exist has every
// call refused by the boundary check before dispatch. A refusal that bounces
// and continues skips the failure counter's reader, the retry ban and the
// repeat detector, so identical calls could repeat with no intervention and
// no terminal of their own. Each refusal must be accounted, so the run stops.

const wsCeiling = 30

// wsRefusalFixture drives the real loop with MaxTurns=0 against a workspace
// root that cannot be opened. The ceiling lives in the server, so nothing
// sleeps and an unbounded loop fails immediately.
func wsRefusalFixture(t *testing.T, tool string, args map[string]interface{}) (
	*AgentContext, *int, map[string]int, map[string]string, string) {
	t.Helper()
	parent := t.TempDir()
	dir := filepath.Join(parent, "missing-root") // never created
	turns := 0
	census := map[string]int{}
	terminal := map[string]string{}
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		io.Copy(io.Discard, r.Body) // the whole request first, as a real model server reads it
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= wsCeiling {
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": tool, "args": args})
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(call)}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.MaxTurns = 0
	firstRefusal := ""
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		census[et]++
		if et == "tool_result" && firstRefusal == "" {
			firstRefusal = string(b)
		}
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	runAgentLoop(ctx, "Add a /health route to app.py.")
	return ctx, &turns, census, terminal, firstRefusal
}

// A find_file against a missing workspace root, repeated.
func TestRepeatedWorkspaceRefusalIsBounded(t *testing.T) {
	ctx, turns, census, terminal, refusal := wsRefusalFixture(t, "find_file",
		map[string]interface{}{"path": "app.py"})

	t.Logf("turns=%d tool_call=%d tool_result=%d status=%q reason=%q",
		*turns, census["tool_call"], census["tool_result"],
		terminal["status"], terminal["reason"])
	t.Logf("refusal: %.140s", refusal)

	if *turns >= wsCeiling {
		t.Fatalf("the loop consumed %d main-loop turns without a terminal; production "+
			"runs uncapped and spent the whole session budget here", *turns)
	}
	if census["done"] != 1 {
		t.Fatalf("%d terminal events", census["done"])
	}
	st := NormalizeTerminalStatus(terminal["status"])
	if st.Completed() {
		t.Fatalf("a run that never dispatched anything reported %q", terminal["status"])
	}
	if terminal["reason"] != "repeated_refusal" {
		t.Errorf("reason = %q, want repeated_refusal from the existing failure policy",
			terminal["reason"])
	}
	// The refusal the model reads is unchanged.
	if !strings.Contains(refusal, "workspace root") {
		t.Errorf("the boundary diagnostic changed: %s", refusal)
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result balance: %d vs %d", census["tool_call"], census["tool_result"])
	}
	// Nothing was dispatched, created, or read.
	if _, err := os.Stat(ctx.WorkingDir); err == nil {
		t.Error("the missing workspace root was created")
	}
	if len(ctx.Ledger) != 0 {
		t.Errorf("a refused path entered the ledger: %v", ctx.Ledger)
	}
}

// Every tool the validator guards must be bounded the same way. The tool list
// is ENUMERATED from workspacePathFields -- the registry the validator itself
// keys on -- so a tool added there without a bounded refusal fails here rather
// than silently gaining the old unaccounted behaviour.
func TestEveryWorkspaceGuardedToolRefusesBoundedly(t *testing.T) {
	// Non-path arguments a tool needs before its path is even looked at.
	// Anything enumerated below must appear here, which is what makes a new
	// tool fail loudly instead of being skipped.
	extra := map[string]map[string]interface{}{
		"read_file":       {},
		"outline_file":    {},
		"write_file":      {"content": "x = 1\n"},
		"edit_file":       {"old_str": "a", "new_str": "b"},
		"structural_edit": {"selector": "function:f", "content": "def f():\n    pass\n"},
		"insert_after":    {"line": 1, "content": "x = 1"},
		"replace_lines":   {"start_line": 1, "end_line": 1, "expected_first_line": "a", "expected_last_line": "a", "content": "b"},
		"delete_file":     {},
		"move_file":       {},
		"search_files":    {"pattern": "def "},
		"find_file":       {"pattern": "app"},
		"list_directory":  {},
		"run_command":     {"command": "ls"},
		"run_background":  {"command": "python app.py"},
	}
	if len(workspacePathFields) != 13 {
		t.Logf("validator now guards %d tools (was 13)", len(workspacePathFields))
	}
	names := make([]string, 0, len(workspacePathFields))
	for name := range workspacePathFields {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			shape, known := extra[name]
			if !known {
				t.Fatalf("%s is guarded by the validator but this table has no argument "+
					"shape for it; give it one so its refusal is proven bounded", name)
			}
			args := map[string]interface{}{}
			for k, v := range shape {
				args[k] = v
			}
			// Every path field the validator keys on, pointed at the same
			// target so repeated refusals share one failure identity.
			for _, field := range workspacePathFields[name] {
				args[field] = "app.py"
			}
			ctx, turns, census, terminal, refusal := wsRefusalFixture(t, name, args)

			t.Logf("%-16s turns=%d status=%q reason=%q", name, *turns,
				terminal["status"], terminal["reason"])
			if *turns >= wsCeiling {
				t.Fatalf("%s: %d turns without a terminal", name, *turns)
			}
			if census["done"] != 1 {
				t.Errorf("%d terminal events", census["done"])
			}
			if NormalizeTerminalStatus(terminal["status"]).Completed() {
				t.Errorf("a session that only ever refused reported %q", terminal["status"])
			}
			if census["tool_call"] != census["tool_result"] {
				t.Errorf("call/result balance: %d vs %d",
					census["tool_call"], census["tool_result"])
			}
			// Refused before dispatch: nothing ran, nothing landed, and the
			// missing root was not created.
			if _, err := os.Stat(ctx.WorkingDir); err == nil {
				t.Error("the missing workspace root was created")
			}
			if len(ctx.Ledger) != 0 {
				t.Errorf("a refused path entered the ledger: %v", ctx.Ledger)
			}
			if refusal == "" {
				t.Error("no refusal reached the model")
			}
		})
	}
}

// alignedWorkspaceRun drives the real loop with MaxTurns=0 against a healthy
// workspace that holds app.py, asking it to "Look at app.py." The ceiling
// lives in the server, so nothing sleeps and an unbounded loop fails
// immediately. It returns the session, the model turns, the event census, the
// terminal payload and the first tool result.
func alignedWorkspaceRun(t *testing.T, plan func(i int, prompt string) map[string]interface{}) (
	*AgentContext, *int, map[string]int, map[string]string, string) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.py"), []byte("A = 1\n"), 0o644)
	turns := 0
	census := map[string]int{}
	terminal := map[string]string{}
	first := ""
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLoopServices(w, r, dir) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= wsCeiling {
			http.Error(w, "ceiling", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i, string(raw)))
		serveReply(w, string(call))
	}))
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
		if et == "tool_result" && first == "" {
			first = string(b)
		}
		if et == "done" {
			json.Unmarshal(b, &terminal) // adds the payload's keys to the map
		}
	}
	runAgentLoop(ctx, "Look at app.py.")
	return ctx, &turns, census, terminal, first
}

// Healthy-workspace controls: the aligned route is untouched.
func TestAlignedWorkspaceRoutesAreUnchanged(t *testing.T) {
	t.Run("aligned find_file dispatches and succeeds", func(t *testing.T) {
		_, turns, _, terminal, first := alignedWorkspaceRun(t, func(i int, _ string) map[string]interface{} {
			if i == 0 {
				return map[string]interface{}{"type": "tool_call", "name": "find_file",
					"args": map[string]string{"pattern": "app"}}
			}
			return map[string]interface{}{"type": "done", "summary": "found app.py"}
		})
		t.Logf("aligned success: turns=%d first=%.90s", *turns, first)
		if !strings.Contains(first, `"success":true`) {
			t.Errorf("a well-formed find_file did not dispatch: %s", first)
		}
		if strings.Contains(first, "workspace root") {
			t.Error("an aligned call took the boundary-refusal path")
		}
		_ = terminal
	})

	t.Run("repeated aligned find_file uses the retry policy, not the boundary path", func(t *testing.T) {
		_, turns, _, terminal, first := alignedWorkspaceRun(t, func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "find_file",
				"args": map[string]string{"path": "app.py"}}
		})
		t.Logf("aligned repeat: turns=%d status=%q reason=%q", *turns,
			terminal["status"], terminal["reason"])
		if strings.Contains(first, "workspace root") {
			t.Fatal("an aligned repeat was refused by the boundary check")
		}
		if !strings.Contains(first, "not a directory") {
			t.Errorf("the aligned diagnostic changed: %.140s", first)
		}
		if terminal["reason"] != "repeated_refusal" {
			t.Errorf("reason = %q", terminal["reason"])
		}
	})
}

// Healthy-workspace control: the path-escape and deny-list refusals still
// reach the model in their own words.
func TestPathEscapeAndDenyListDiagnosticsAreVerbatim(t *testing.T) {
	for _, c := range []struct{ name, path, want string }{
		{"escape", "../outside.py", "outside the workspace"},
		{"deny-list", ".env", "refused"},
	} {
		_, _, _, _, first := alignedWorkspaceRun(t, func(i int, _ string) map[string]interface{} {
			if i == 0 {
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": c.path, "content": "x = 1\n"}}
			}
			return map[string]interface{}{"type": "done", "summary": "done"}
		})
		t.Logf("%s: %.120s", c.name, first)
		if !strings.Contains(strings.ToLower(first), c.want) {
			t.Errorf("%s diagnostic changed: %s", c.name, first)
		}
	}
}

// Structural guard against a fifth instance. Every pre-execution bounce in the
// tool-call branch must either be accounted (recorded and measured against the
// stopping rule) or bounded by its own budget. Anything else is listed here
// explicitly, with what is known about it, so a NEW unaccounted refusal fails
// this test rather than shipping as another unbounded loop.
//
// Keyed on the rejection's own wording rather than line numbers, so ordinary
// edits above it do not break the guard.
func TestEveryPreExecutionBounceIsAccountedOrBounded(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	start, ban := -1, -1
	for i, l := range lines {
		switch {
		case start < 0 && strings.TrimSpace(l) == `case "tool_call":` && i > 1100:
			start = i
		case start >= 0 && ban < 0 && strings.Contains(l, "identicalRetryRefusal("):
			ban = i
		}
	}
	if start < 0 || ban < 0 {
		t.Fatal("could not locate the tool-call branch and its accounting boundary")
	}

	// Bounces that are deliberately NOT failure-accounted, each with the
	// mechanism that bounds them instead. A new entry needs a deliberate
	// decision; an unlisted one fails.
	known := map[string]string{
		// Anchors are text visible in the SOURCE around the bounce, not the
		// runtime message, because several rejections are composed elsewhere.
		"foregroundServerRejectionWithSource(": "unmeasured; reported",
		"rejecting run_command":                "unmeasured; reported",
		"rejecting run_background":             "unmeasured; reported",
		"Your output was truncated":            "bounded by its own consecutiveErrors>=3 stop",
		"fencedRunFirstRecovery(":              "bounded: one recovery per canonical path",
		"fencedChannelRecovery(":               "bounded: one offer per canonical path",
		"no fenced block followed":             "accounted since the fenced-bounce fix",
		"toolBanNote(":                         "accounted by the per-path ban branch",
		"identicalRetryRefusal(":               "accounted by the retry-ban branch",
	}
	knownPhrases := slices.Collect(maps.Keys(known))

	var unlisted []string
	for i := start; i < ban; i++ {
		if !strings.Contains(lines[i], "st.bounceToolCall(") {
			continue
		}
		// The rejection text is on this line or the few around it.
		window := strings.Join(lines[max(start, i-16):min(len(lines), i+6)], "\n")
		after := strings.Join(lines[i:min(len(lines), i+30)], "\n")
		switch {
		case strings.Contains(after, "accountRefusedCall("),
			strings.Contains(after, "shouldStopForFailures("),
			strings.Contains(after, "recordFailedToolCall("):
			// Accounted right here. accountRefusedCall is the shared reader
			// every pre-dispatch refusal goes through; the two inline forms
			// predate it and still exist.
		case strings.Contains(window, "chargeBounce("):
			// Bounded by its own budget.
		case containsAny(window, knownPhrases...):
			// Listed above, with what bounds it.
		default:
			unlisted = append(unlisted, fmt.Sprintf("agent.go:%d", i+1))
		}
	}
	if len(unlisted) > 0 {
		t.Errorf("pre-execution bounces that are neither accounted nor bounded nor "+
			"listed: %v — a refusal the model can repeat forever is how the "+
			"workspace-boundary loop happened; account for it or record why it "+
			"is bounded", unlisted)
	}
	t.Logf("audited pre-execution bounces between agent.go:%d and the retry ban at agent.go:%d",
		start+1, ban+1)
}
