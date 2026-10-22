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

// A model that sends one fenced call again and again must be seen to repeat
// it. Fenced resolution puts the fetched body into the call's arguments, and
// the body can differ every turn while the model's call stays the same. So
// the repeat detector and the retry ban key on the call as the model sent it
// (its intent), not on the fetched bytes.

// fencedVaryingBodyStub answers every fenced sub-call with a different body,
// as a model does that iterates on the file: the same intent, different
// resolved bytes.
func fencedVaryingBodyStub(t *testing.T, dir string, turns, fences *int, script func(int) map[string]interface{}) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLoopServices(w, r, dir) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "single fenced block") {
			mu.Lock()
			n := *fences
			*fences++
			mu.Unlock()
			serveReply(w, fmt.Sprintf("```python\nVALUE = %d\n%s\n```", n, strings.Repeat("# pad\n", n)))
			return
		}
		mu.Lock()
		i := *turns
		*turns++
		mu.Unlock()
		call, _ := json.Marshal(script(i))
		serveReply(w, string(call))
	}))
}

// repetitionCtx is a loop context that counts events by type and keeps the
// terminal's fields.
func repetitionCtx(t *testing.T, dir, url string, census map[string]int, terminal map[string]string) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = url, url, url
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 14
	var mu sync.Mutex
	ctx.StreamFn = func(eventType string, data interface{}) {
		mu.Lock()
		defer mu.Unlock()
		census[eventType]++
		if eventType == "done" {
			b, _ := json.Marshal(data)
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	return ctx
}

// The same raw call every turn, a different fetched body every turn: one
// signature, so the repeat detector steps in. The fetched bytes still land,
// and the run ends with one terminal that is not a completion.
func TestIdenticalFencedIntentIsOneSignature(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	srv := fencedVaryingBodyStub(t, dir, &turns, &fences, func(i int) map[string]interface{} {
		return map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
	})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	ctx := repetitionCtx(t, dir, srv.URL, census, terminal)
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	t.Logf("turns=%d fenced=%d interventions=%d status=%q reason=%q",
		turns, fences, census["agent_repeat_intervention"],
		terminal["status"], terminal["reason"])

	if census["agent_repeat_intervention"] < 2 {
		t.Errorf("%d repeat interventions across %d identical calls; the detector "+
			"is reading the resolved bytes rather than the model's call",
			census["agent_repeat_intervention"], turns)
	}
	if turns > 10 {
		t.Errorf("%d turns before the breaker engaged", turns)
	}
	got, err := os.ReadFile(filepath.Join(dir, "solve.py"))
	if err != nil {
		t.Fatalf("the resolved file never landed: %v", err)
	}
	if !strings.HasPrefix(string(got), "VALUE = ") {
		t.Errorf("disk does not hold a fetched body: %q", got)
	}
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Error("the ledger does not describe the bytes on disk")
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("a repeat-broken run reported %q", terminal["status"])
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result invariant broken: %d vs %d",
			census["tool_call"], census["tool_result"])
	}
}

// Spelling the same target two ways does not buy a fresh budget: solve.py
// and ./solve.py are one signature for the repeat detector.
// TestDifferentFencedTargetsStayIndependent is the other half: distinct
// targets do not collapse the same way.
func TestCanonicalSpellingsShareTheIntentSignature(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	spellings := []string{"solve.py", "./solve.py", "solve.py", "./solve.py"}
	srv := fencedVaryingBodyStub(t, dir, &turns, &fences, func(i int) map[string]interface{} {
		return map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{
				"path": spellings[i%len(spellings)], "content": "@fenced"}}
	})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	ctx := repetitionCtx(t, dir, srv.URL, census, terminal)
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatal(err)
	}
	t.Logf("turns=%d interventions=%d status=%q reason=%q fences=%d", turns,
		census["agent_repeat_intervention"], terminal["status"], terminal["reason"], fences)
	if census["agent_repeat_intervention"] < 2 {
		t.Errorf("alternating spellings evaded repetition tracking (%d interventions in %d turns)",
			census["agent_repeat_intervention"], turns)
	}
	if turns >= 30 {
		t.Fatalf("%d turns without a terminal", turns)
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("a run that only ever re-sent one call reported %q", terminal["status"])
	}
}

// Two different files are two different problems and keep their own budgets.
func TestDifferentFencedTargetsStayIndependent(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	paths := []string{"a.py", "b.py", "c.py", "d.py", "e.py", "f.py"}
	srv := fencedVaryingBodyStub(t, dir, &turns, &fences, func(i int) map[string]interface{} {
		if i >= len(paths) {
			return map[string]interface{}{"type": "done", "summary": "wrote them"}
		}
		return map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": paths[i], "content": "@fenced"}}
	})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	ctx := repetitionCtx(t, dir, srv.URL, census, terminal)
	if err := runAgentLoop(ctx, "Write six files."); err != nil {
		t.Fatal(err)
	}
	t.Logf("turns=%d interventions=%d status=%q reason=%q summary=%.120s",
		turns, census["agent_repeat_intervention"], terminal["status"],
		terminal["reason"], terminal["summary"])
	if census["agent_repeat_intervention"] != 0 {
		t.Errorf("%d interventions for six distinct targets", census["agent_repeat_intervention"])
	}
	// Not every file lands: once a.py and b.py exist, the sibling-pattern
	// hint asks the model to read one before it creates another. That guard
	// has nothing to do with the signature, so this checks only that the
	// files that did land hold their own fetched body.
	landed := 0
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			continue
		}
		landed++
		if !strings.HasPrefix(string(b), "VALUE = ") {
			t.Errorf("%s does not hold a fetched body: %q", p, b)
		}
	}
	if landed < 2 {
		t.Fatalf("only %d of the distinct targets landed; the fixture proves nothing", landed)
	}
}

// A model genuinely revising a file inline must not be read as repetition.
func TestMateriallyDifferentInlineWritesAreNotRepetition(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	srv := fencedVaryingBodyStub(t, dir, &turns, &fences, func(i int) map[string]interface{} {
		if i >= 6 {
			return map[string]interface{}{"type": "done", "summary": "done revising"}
		}
		body := fmt.Sprintf("def solve():\n    total = %d\n    return total\n", i*7+1)
		return map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": "solve.py", "content": body}}
	})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	ctx := repetitionCtx(t, dir, srv.URL, census, terminal)
	if err := runAgentLoop(ctx, "Revise solve.py."); err != nil {
		t.Fatal(err)
	}
	if fences != 0 {
		t.Errorf("an inline write opened the fenced channel %d times", fences)
	}
	if census["agent_repeat_intervention"] != 0 {
		t.Errorf("%d interventions for six materially different revisions",
			census["agent_repeat_intervention"])
	}
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	if !strings.Contains(string(got), "total = 36") {
		t.Errorf("the last revision did not land: %q", got)
	}
}

// A model that sends one rejected fenced intent again and again must not pay
// a fresh generation every turn, and must not run on. Each fetched body here
// is invalid and different, while the call is the same as the model wrote it.
// Which mechanism ends the run does not matter: every body fails the same way,
// so the failure streak never resets and a breaker ends it.
func TestRejectedFencedIntentIsRecognisedOnResend(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLoopServices(w, r, dir) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "single fenced block") {
			n := fences
			fences++
			serveReply(w, fmt.Sprintf("```python\nVALUE = %d]]\n```", n))
			return
		}
		i := turns
		turns++
		args := map[string]string{"path": "solve.py", "content": "@fenced"}
		if i == 0 {
			// A healthy file the session owns comes first. Without it the
			// broken writes are new-file writes, which land with a warning
			// rather than being refused.
			args = map[string]string{"path": "solve.py",
				"content": "def solve():\n    return 1\n"}
		}
		call, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "write_file", "args": args})
		serveReply(w, string(call))
	}))
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	ctx := repetitionCtx(t, dir, srv.URL, census, terminal)
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}
	t.Logf("turns=%d fenced=%d status=%q reason=%q", turns, fences,
		terminal["status"], terminal["reason"])

	switch terminal["reason"] {
	case "same_target_failures", "repeated_refusal", "repeat_detector", "failure_ceiling":
	default:
		t.Errorf("reason=%q: repeated channel use is not bounded by any "+
			"convergence mechanism", terminal["reason"])
	}
	if turns > 10 {
		t.Errorf("%d turns of an identical rejected intent before the run ended", turns)
	}
	// Fewer generations than turns: the later re-sends are answered without
	// opening the channel again.
	if fences >= turns {
		t.Errorf("%d generations for %d turns: the re-send still paid for a "+
			"fresh generation every time", fences, turns)
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("a run whose every write was refused reported %q", terminal["status"])
	}
	// Every broken rewrite was refused before disk.
	got, err := os.ReadFile(filepath.Join(dir, "solve.py"))
	if err != nil {
		t.Fatalf("the healthy write did not land: %v", err)
	}
	if string(got) != "def solve():\n    return 1\n" {
		t.Errorf("a refused rewrite reached disk: %q", got)
	}
}

// The three sites that make up the ban must agree on the key, or a recorded
// rejection can never be found again.
func TestRetryBanKeysOnIntentAcrossRecordLookupAndClear(t *testing.T) {
	ctx := &AgentContext{}
	intent := json.RawMessage(`{"path":"solve.py","content":"@fenced"}`)
	resolved := json.RawMessage(`{"path":"solve.py","content":"VALUE = 1\n"}`)
	alias := json.RawMessage(`{"path":"./solve.py","content":"@fenced"}`)

	recordFailedToolCall(ctx, "write_file", intent, "rejected: would not parse")
	if identicalRetryRefusal(ctx, "write_file", intent) == "" {
		t.Fatal("a recorded rejection is not found under its own intent")
	}
	if identicalRetryRefusal(ctx, "write_file", alias) == "" {
		t.Error("./solve.py evaded the ban recorded for solve.py")
	}
	if identicalRetryRefusal(ctx, "write_file", resolved) != "" {
		t.Error("the resolved body matched an intent-keyed record; the two " +
			"representations must not be interchangeable")
	}
	clearFailedToolCall(ctx, "write_file", alias)
	if identicalRetryRefusal(ctx, "write_file", intent) != "" {
		t.Error("clearing under an equivalent spelling did not clear the record")
	}

	// A materially different inline write is a different call.
	a := json.RawMessage(`{"path":"solve.py","content":"def f():\n    return 1\n"}`)
	b := json.RawMessage(`{"path":"solve.py","content":"def f():\n    return 2\n"}`)
	recordFailedToolCall(ctx, "write_file", a, "rejected")
	if identicalRetryRefusal(ctx, "write_file", b) != "" {
		t.Error("a genuine revision was refused as an identical re-send")
	}
	// And a different target keeps its own record.
	other := json.RawMessage(`{"path":"other.py","content":"@fenced"}`)
	if identicalRetryRefusal(ctx, "write_file", other) != "" {
		t.Error("a different path inherited another path's ban")
	}
}

// --- Turns spent on a fenced call that keeps failing ------------------------
//
// The allowance stops the fenced generations, but each refused call still
// costs a main-loop turn. The refusal counts as a failure for the breakers,
// so the turns are bounded too.

// fencedLoopFixture drives the real loop with MaxTurns=0, as production runs
// it. Every fenced sub-call answers fast with no block. The server enforces
// the turn bound, so nothing here sleeps.
func fencedLoopFixture(t *testing.T, dir string, maxTurns int,
	plan func(i int, prompt string) map[string]interface{}) (*AgentContext, *int, *int, map[string]int, map[string]string) {
	t.Helper()
	turns, subcalls := 0, 0
	census := map[string]int{}
	terminal := map[string]string{}
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLoopServices(w, r, dir) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "single fenced block") {
			mu.Lock()
			subcalls++
			mu.Unlock()
			serveReply(w, "Sure, here is the file.")
			return
		}
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= maxTurns {
			// Past the bound the loop is unbounded, and the fixture says so
			// at once.
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
	ctx.MaxTurns = 0 // production
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		census[et]++
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	return ctx, &turns, &subcalls, census, terminal
}

const fencedTurnCeiling = 30

// The same failing @fenced write, under one spelling or two, reaches one
// honest terminal well inside the ceiling. The allowance still caps the
// sub-generations, and the target is never written.
func TestRepeatedFencedFailureReachesABoundedTerminal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		spellings []string
	}{
		{"one spelling", []string{"solve.py"}},
		{"two spellings of one file", []string{"solve.py", "./solve.py"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx, turns, subcalls, census, terminal := fencedLoopFixture(t, dir, fencedTurnCeiling,
				func(i int, _ string) map[string]interface{} {
					return map[string]interface{}{"type": "tool_call", "name": "write_file",
						"args": map[string]string{
							"path": tc.spellings[i%len(tc.spellings)], "content": "@fenced"}}
				})
			if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
				t.Fatalf("loop error: %v", err)
			}
			t.Logf("main-loop turns=%d fenced sub-generations=%d (ctx.FencedCalls=%d) status=%q reason=%q",
				*turns, *subcalls, ctx.FencedCalls, terminal["status"], terminal["reason"])

			if *turns >= fencedTurnCeiling {
				t.Fatalf("the loop consumed %d main-loop turns without a terminal; production "+
					"runs uncapped and would spend the whole session budget here", *turns)
			}
			if census["done"] != 1 {
				t.Fatalf("%d terminal events", census["done"])
			}
			st := NormalizeTerminalStatus(terminal["status"])
			if st != TerminalStopped && st != TerminalIncomplete {
				t.Errorf("terminal status = %q, want an honest stop", terminal["status"])
			}
			if st.Completed() {
				t.Error("a run that wrote nothing reported completion")
			}
			if ctx.FencedCalls > maxFencedFailuresPerPath {
				t.Errorf("%d fenced sub-generations, allowance is %d",
					ctx.FencedCalls, maxFencedFailuresPerPath)
			}
			if census["tool_call"] != census["tool_result"] {
				t.Errorf("call/result invariant broken: %d vs %d",
					census["tool_call"], census["tool_result"])
			}
			if _, err := os.Stat(filepath.Join(dir, "solve.py")); err == nil {
				t.Error("a run whose every resolution failed still mutated the target")
			}
		})
	}
}

// Distinct targets keep independent budgets and must not be collapsed.
func TestFencedFailurePathsStayIndependent(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"a.py", "b.py", "c.py", "d.py"}
	ctx, turns, subcalls, _, terminal := fencedLoopFixture(t, dir, fencedTurnCeiling,
		func(i int, _ string) map[string]interface{} {
			if i >= len(paths) {
				return map[string]interface{}{"type": "done", "summary": "gave up"}
			}
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": paths[i], "content": "@fenced"}}
		})
	if err := runAgentLoop(ctx, "Write four files."); err != nil {
		t.Fatal(err)
	}
	t.Logf("independent paths: turns=%d sub-generations=%d status=%q",
		*turns, *subcalls, terminal["status"])
	// Four paths must not be stopped as if they were one target repeated.
	if *turns < len(paths) {
		t.Errorf("only %d turns for %d distinct targets: the paths were collapsed",
			*turns, len(paths))
	}
	// Each path has its own allowance, and no target is starved by
	// another's failures.
	if *subcalls != len(paths)*maxFencedFailuresPerPath {
		t.Errorf("%d sub-generations for %d paths, want %d",
			*subcalls, len(paths), len(paths)*maxFencedFailuresPerPath)
	}
	// The terminal of a run that declared nothing and wrote nothing is the
	// completion policy's decision, so it is logged and not checked here.
	t.Logf("terminal for the four-path run: status=%q reason=%q",
		terminal["status"], terminal["reason"])
}
