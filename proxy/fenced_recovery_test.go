package main

import (
	"context"
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
	"time"
)

// --- Recovery from the run-first state (C5) ---------------------------------
//
// A warned file is on disk, the run-first gate wants it run, and the model
// answers with the same raw @fenced write. When that happens again, the gate
// hands back the file as it is, once, and holds the useless call back.

// c5Stub scripts a model through the C5 state. plan decides what the model
// sends on each turn, and every fenced sub-call returns c5Broken.
func c5Stub(t *testing.T, dir string, turns, fences *int,
	plan func(i int, prompt string) map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLoopServices(w, r, dir) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "single fenced block") {
			*fences++
			serveReply(w, "```python\n"+c5Broken+"```")
			return
		}
		i := *turns
		*turns++
		call, _ := json.Marshal(plan(i, string(raw)))
		serveReply(w, string(call))
	}))
}

const (
	c5Broken = "def solve():\n    return [1, 2]]\n"
	c5Fixed  = "def solve():\n    return [1, 2]  # fixed_marker\n"
)

func c5Ctx(t *testing.T, dir, url string, census map[string]int,
	terminal map[string]string, bounces *[]string) *AgentContext {
	t.Helper()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = url, url, url
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 16
	var mu sync.Mutex
	ctx.StreamFn = func(eventType string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		census[eventType]++
		// Both, tagged. bounceToolCall emits a gate AND the tool_result that
		// answers the call: the gate's reason is truncated for the event
		// stream, so counting happens on the gate and the text is read from
		// the result the model actually receives.
		if eventType == "gate" || eventType == "tool_result" || eventType == "tool_call" {
			*bounces = append(*bounces, eventType+"|"+string(b))
		}
		if eventType == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	return ctx
}

// The model enters the C5 state, is given the file, then does something
// useful with it and finishes with bytes that validate.
func TestC5RecoveryReachesAVerifiedCompletion(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	srv := c5Stub(t, dir, &turns, &fences,
		// The scripted model is conditional, and that is the point. It
		// repeats the same whole-file write until it can see the file; once
		// the source is in front of it, it runs the code and then sends a
		// targeted correction. A model that would have corrected itself
		// anyway proves nothing about recovery.
		func(i int, prompt string) map[string]interface{} {
			sawSource := strings.Contains(prompt, "twice without anything changing on disk")
			ranIt := strings.Contains(prompt, "ran_solve_marker")
			// A token rather than the source text: the model's own tool call
			// is embedded as a JSON string inside the request body, so the
			// content arrives double-escaped.
			sentFix := strings.Contains(prompt, "fixed_marker")
			reran := strings.Contains(prompt, "reran_marker")
			switch {
			case !sawSource:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
			case !ranIt:
				// The command the recovery itself suggested. Pass or fail,
				// running it is what clears the run-first demand.
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "echo ran_solve_marker; python3 solve.py"}}
			case !sentFix:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "solve.py", "content": c5Fixed}}
			case !reran:
				// The last run of solve.py failed. Until the corrected file
				// runs, nothing shows it works, and completion waits for that.
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "echo reran_marker; python3 solve.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "fixed the bracket in solve.py"}
			}
		})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	var bounces []string
	ctx := c5Ctx(t, dir, srv.URL, census, terminal, &bounces)
	if err := runAgentLoop(ctx, "Write solve.py that returns the list."); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	recoveries, recovery := 0, ""
	for _, b := range bounces {
		if !strings.Contains(b, "twice without anything changing on disk") {
			continue
		}
		if strings.HasPrefix(b, "gate|") {
			recoveries++
		} else {
			recovery = b // the untruncated text the model receives
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	t.Logf("turns=%d fenced=%d recoveries=%d status=%q reason=%q disk=%q",
		turns, fences, recoveries, terminal["status"], terminal["reason"], string(got))
	for _, b := range bounces {
		if strings.HasPrefix(b, "tool_call|") {
			t.Logf("  %.130s", b)
		}
	}

	// Recovery fired exactly once, and gave the source with the guidance.
	if recoveries != 1 {
		t.Fatalf("recovery fired %d times, want exactly 1", recoveries)
	}
	if recovery == "" {
		t.Fatal("the recovery never reached the model as a tool result")
	}
	for _, want := range []string{"solve.py", "return [1, 2]]", "run_command", "read_file"} {
		if !strings.Contains(recovery, want) {
			t.Errorf("the recovery context is missing %q", want)
		}
	}
	// A completed terminal over validated bytes.
	if terminal["status"] != string(TerminalCompleted) {
		t.Fatalf("recovery did not reach a verified completion: status=%q reason=%q summary=%s",
			terminal["status"], terminal["reason"], terminal["summary"])
	}
	if string(got) != c5Fixed {
		t.Errorf("disk = %q, want the corrected file", got)
	}
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Fatal("the ledger does not describe the final bytes")
	}
	if k, s := d.CurrentValidation(); s != ValidationPassed || k != ValidationKindSyntax {
		t.Errorf("completion authorized over %v/%v rather than a current syntax pass", k, s)
	}
	// Nothing was run for the model: each command was its own.
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result invariant broken: %d vs %d",
			census["tool_call"], census["tool_result"])
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

func TestAFileCutForTheModelSaysSo(t *testing.T) {
	for _, tc := range []struct {
		truncated bool
		want      string
	}{
		{false, "a.py currently contains:\n\nx = 1\n"},
		{true, fmt.Sprintf("a.py currently contains (first %d lines):\n\nx = 1\n", fencedRecoveryMaxLines)},
	} {
		var sb strings.Builder
		writeCurrentSource(&sb, "a.py", "x = 1\n", tc.truncated)
		if sb.String() != tc.want {
			t.Errorf("truncated=%v: %q, want %q", tc.truncated, sb.String(), tc.want)
		}
	}
}

// A model that answers the recovery with the same blocked intent gets an
// honest stop, and the bytes that were there are still there.
func TestC5RecoveryThatIsIgnoredStopsHonestly(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	srv := c5Stub(t, dir, &turns, &fences,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
		})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	var bounces []string
	ctx := c5Ctx(t, dir, srv.URL, census, terminal, &bounces)
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	t.Logf("turns=%d fenced=%d status=%q reason=%q", turns, fences,
		terminal["status"], terminal["reason"])

	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("an ignored recovery reported %q", terminal["status"])
	}
	if completionClaimIn(terminal["summary"]) != "" {
		t.Errorf("the terminal claimed success:\n%s", terminal["summary"])
	}
	// The blocked repeats opened no channel: fewer generations than turns.
	if fences >= turns {
		t.Errorf("%d generations across %d turns: blocked repeats still paid for one",
			fences, turns)
	}
	if string(got) != c5Broken {
		t.Errorf("the bytes on disk changed under an ignored recovery: %q", got)
	}
}

// One recovery per canonical path: a different spelling is the same file.
func TestC5RecoveryIsNotResetByAliasOrTurn(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	spellings := []string{"solve.py", "./solve.py"}
	srv := c5Stub(t, dir, &turns, &fences,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{
					"path": spellings[i%len(spellings)], "content": "@fenced"}}
		})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	var bounces []string
	ctx := c5Ctx(t, dir, srv.URL, census, terminal, &bounces)
	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatal(err)
	}
	recoveries := 0
	for _, b := range bounces {
		if strings.HasPrefix(b, "gate|") &&
			strings.Contains(b, "twice without anything changing on disk") {
			recoveries++
		}
	}
	t.Logf("turns=%d recoveries=%d status=%q", turns, recoveries, terminal["status"])
	if recoveries != 1 {
		t.Errorf("alias spellings got %d recoveries, want exactly 1", recoveries)
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("status %q", terminal["status"])
	}
}

// Two files are two problems and keep independent recovery state.
func TestC5RecoveryIsPerPath(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	// Three attempts each: land a warned version, take the gate's bounce,
	// then reach the recurrence that recovery answers.
	paths := []string{"a.py", "a.py", "a.py", "b.py", "b.py", "b.py"}
	srv := c5Stub(t, dir, &turns, &fences,
		func(i int, _ string) map[string]interface{} {
			if i >= len(paths) {
				return map[string]interface{}{"type": "done", "summary": "stopping"}
			}
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": paths[i], "content": "@fenced"}}
		})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	var bounces []string
	ctx := c5Ctx(t, dir, srv.URL, census, terminal, &bounces)
	if err := runAgentLoop(ctx, "Write two files."); err != nil {
		t.Fatal(err)
	}
	perPath := map[string]int{}
	for _, b := range bounces {
		if !strings.HasPrefix(b, "gate|") ||
			!strings.Contains(b, "twice without anything changing on disk") {
			continue
		}
		for _, p := range []string{"a.py", "b.py"} {
			if strings.Contains(b, p) {
				perPath[p]++
			}
		}
	}
	t.Logf("recoveries per path: %v", perPath)
	for _, p := range []string{"a.py", "b.py"} {
		if perPath[p] != 1 {
			t.Errorf("%s got %d recoveries, want exactly 1", p, perPath[p])
		}
	}
}

// With the budget nearly gone, recovery is skipped and the run stops honestly
// rather than spending what is left on context nobody can act on.
func TestC5RecoverySkippedWhenTheBudgetIsGone(t *testing.T) {
	dir := t.TempDir()
	turns, fences := 0, 0
	srv := c5Stub(t, dir, &turns, &fences,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
		})
	defer srv.Close()

	census, terminal := map[string]int{}, map[string]string{}
	var bounces []string
	ctx := c5Ctx(t, dir, srv.URL, census, terminal, &bounces)
	// A work deadline inside the recovery floor.
	workCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ctx.Ctx = workCtx

	if err := runAgentLoop(ctx, "Write solve.py."); err != nil {
		t.Fatal(err)
	}
	for _, b := range bounces {
		if strings.HasPrefix(b, "gate|") &&
			strings.Contains(b, "twice without anything changing on disk") {
			t.Fatal("recovery ran with less budget than it needs to be acted on")
		}
	}
	if NormalizeTerminalStatus(terminal["status"]).Completed() {
		t.Errorf("status %q", terminal["status"])
	}
}

// --- Recovery for the spent fenced channel ----------------------------------
//
// The allowance stops the generations but tells the model nothing it can act
// on. The recovery offers the way out once, before anything tries another
// resolution.

const fencedRecoveryMark = "fenced-content channel for"

func TestFencedChannelRecoveryReachesAVerifiedCompletion(t *testing.T) {
	dir := t.TempDir()
	const fixed = "def solve():\n    return 7  # inline_fix\n\nprint(solve())\n"
	var bounces []string
	var mu sync.Mutex

	ctx, turns, subcalls, census, terminal := fencedLoopFixture(t, dir, fencedTurnCeiling,
		// The scripted model is conditional: it keeps asking for the fenced
		// channel until it is told the channel is spent, and only then picks
		// an allowed alternative. A model that would have switched anyway
		// proves nothing about recovery.
		func(i int, prompt string) map[string]interface{} {
			sawRecovery := strings.Contains(prompt, fencedRecoveryMark)
			sentInline := strings.Contains(prompt, "inline_fix")
			ranIt := strings.Contains(prompt, "ran_marker")
			switch {
			case !sawRecovery:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
			case !sentInline:
				// The alternative the recovery named: the whole file inline.
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "solve.py", "content": fixed}}
			case !ranIt:
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "echo ran_marker; python3 solve.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "wrote solve.py inline and ran it"}
			}
		})
	inner := ctx.StreamFn
	ctx.StreamFn = func(et string, data interface{}) {
		if et == "gate" || et == "tool_result" {
			b, _ := json.Marshal(data)
			mu.Lock()
			bounces = append(bounces, et+"|"+string(b))
			mu.Unlock()
		}
		inner(et, data)
	}
	if err := runAgentLoop(ctx, "Create solve.py."); err != nil {
		t.Fatalf("loop error: %v", err)
	}

	recoveries, recoveryText := 0, ""
	for _, b := range bounces {
		if !strings.Contains(b, fencedRecoveryMark) {
			continue
		}
		if strings.HasPrefix(b, "gate|") {
			recoveries++
		} else {
			recoveryText = b
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	t.Logf("turns=%d sub-generations=%d recoveries=%d status=%q reason=%q disk=%q",
		*turns, *subcalls, recoveries, terminal["status"], terminal["reason"], string(got))

	if recoveries != 1 {
		t.Fatalf("recovery fired %d times, want exactly 1", recoveries)
	}
	if recoveryText == "" {
		t.Fatal("the recovery never reached the model as a tool result")
	}
	for _, want := range []string{"solve.py", "write_file", "edit_file", "read_file", "run_command"} {
		if !strings.Contains(recoveryText, want) {
			t.Errorf("the recovery context never names %q", want)
		}
	}
	// A completed terminal over validated bytes.
	if terminal["status"] != string(TerminalCompleted) {
		t.Fatalf("recovery did not reach a verified completion: status=%q reason=%q summary=%s",
			terminal["status"], terminal["reason"], terminal["summary"])
	}
	if terminal["reason"] != "deliverables_demonstrated" {
		t.Errorf("completion reason = %q", terminal["reason"])
	}
	if string(got) != fixed {
		t.Errorf("disk = %q, want the inline correction", got)
	}
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	if d == nil || d.CurrentHash != hashBytes(got) {
		t.Fatal("the ledger does not describe the final bytes")
	}
	if k, s := d.CurrentValidation(); s != ValidationPassed || k != ValidationKindSyntax {
		t.Errorf("completion authorized over %v/%v rather than a current syntax pass", k, s)
	}
	// The allowance is unchanged and recovery started nothing.
	if *subcalls > maxFencedFailuresPerPath {
		t.Errorf("%d sub-generations, allowance is %d", *subcalls, maxFencedFailuresPerPath)
	}
	if census["tool_call"] != census["tool_result"] {
		t.Errorf("call/result invariant broken: %d vs %d", census["tool_call"], census["tool_result"])
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

func TestFencedChannelRecoveryIgnoredStopsHonestly(t *testing.T) {
	dir := t.TempDir()
	ctx, turns, subcalls, census, terminal := fencedLoopFixture(t, dir, fencedTurnCeiling,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
		})
	if err := runAgentLoop(ctx, "Create solve.py."); err != nil {
		t.Fatal(err)
	}
	t.Logf("ignored: turns=%d sub-generations=%d status=%q reason=%q",
		*turns, *subcalls, terminal["status"], terminal["reason"])
	if *turns >= fencedTurnCeiling {
		t.Errorf("%d turns — recovery did not stay bounded", *turns)
	}
	st := NormalizeTerminalStatus(terminal["status"])
	if st.Completed() {
		t.Errorf("an ignored recovery reported %q", terminal["status"])
	}
	if completionClaimIn(terminal["summary"]) != "" {
		t.Errorf("the terminal claimed success:\n%s", terminal["summary"])
	}
	// Recovery launched no generation of its own.
	if *subcalls > maxFencedFailuresPerPath {
		t.Errorf("%d sub-generations after recovery, allowance is %d",
			*subcalls, maxFencedFailuresPerPath)
	}
	if census["done"] != 1 {
		t.Errorf("%d terminal events", census["done"])
	}
}

// The offer is made once per canonical path, whatever spelling arrives.
func TestFencedChannelRecoveryCannotBeResetByAnAlias(t *testing.T) {
	dir := t.TempDir()
	spellings := []string{"solve.py", "./solve.py"}
	var seen int
	var mu sync.Mutex
	ctx, _, _, _, _ := fencedLoopFixture(t, dir, fencedTurnCeiling,
		func(i int, _ string) map[string]interface{} {
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{
					"path": spellings[i%len(spellings)], "content": "@fenced"}}
		})
	inner := ctx.StreamFn
	ctx.StreamFn = func(et string, data interface{}) {
		if et == "gate" {
			if b, _ := json.Marshal(data); strings.Contains(string(b), fencedRecoveryMark) {
				mu.Lock()
				seen++
				mu.Unlock()
			}
		}
		inner(et, data)
	}
	if err := runAgentLoop(ctx, "Create solve.py."); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("alias spellings produced %d recoveries, want 1", seen)
	}
}

// Two different files get an offer each.
func TestFencedChannelRecoveryIsPerPath(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"a.py", "a.py", "a.py", "b.py", "b.py", "b.py"}
	var gates []string
	var mu sync.Mutex
	ctx, _, _, _, _ := fencedLoopFixture(t, dir, fencedTurnCeiling,
		func(i int, _ string) map[string]interface{} {
			if i >= len(paths) {
				return map[string]interface{}{"type": "done", "summary": "stopping"}
			}
			return map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": paths[i], "content": "@fenced"}}
		})
	inner := ctx.StreamFn
	ctx.StreamFn = func(et string, data interface{}) {
		if et == "gate" {
			b, _ := json.Marshal(data)
			mu.Lock()
			gates = append(gates, string(b))
			mu.Unlock()
		}
		inner(et, data)
	}
	if err := runAgentLoop(ctx, "Create two files."); err != nil {
		t.Fatal(err)
	}
	perPath := map[string]int{}
	for _, g := range gates {
		for _, p := range []string{"a.py", "b.py"} {
			if strings.Contains(g, fencedRecoveryMark) && strings.Contains(g, p) {
				perPath[p]++
			}
		}
	}
	t.Logf("recoveries per path: %v", perPath)
	for _, p := range []string{"a.py", "b.py"} {
		if perPath[p] != 1 {
			t.Errorf("%s got %d recoveries, want exactly 1", p, perPath[p])
		}
	}
}

func TestFencedChannelRecoveryInventsNoSourceForAnAbsentTarget(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	st := &runState{}
	ctx.FencedFailures = map[string]int{fencedKey(ctx, "missing.py"): maxFencedFailuresPerPath}
	msg := fencedChannelRecovery(ctx, st, "missing.py")
	if msg == "" {
		t.Fatal("no recovery offered for an exhausted path")
	}
	if !strings.Contains(msg, "not on disk yet") {
		t.Errorf("absent target not disclosed:\n%s", msg)
	}
	if strings.Contains(msg, "currently contains") {
		t.Errorf("source was invented for a file that is not there:\n%s", msg)
	}
}

func TestFencedChannelRecoverySkippedWhenTheBudgetIsTooSmallToActOn(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	st := &runState{}
	ctx.FencedFailures = map[string]int{fencedKey(ctx, "solve.py"): maxFencedFailuresPerPath}
	workCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ctx.Ctx = workCtx
	if msg := fencedChannelRecovery(ctx, st, "solve.py"); msg != "" {
		t.Errorf("recovery ran with less budget than it needs to be acted on: got %v", msg)
	}
}

func TestFencedChannelRecoveryNotOfferedWhileTheAllowanceRemains(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	st := &runState{}
	if msg := fencedChannelRecovery(ctx, st, "solve.py"); msg != "" {
		t.Errorf("recovery offered before the channel was spent: got %v", msg)
	}
	ctx.FencedFailures = map[string]int{fencedKey(ctx, "solve.py"): 1}
	if msg := fencedChannelRecovery(ctx, st, "solve.py"); msg != "" {
		t.Errorf("recovery offered with allowance remaining: got %v", msg)
	}
}

// The alternatives the recovery names must actually work after the channel is
// spent: a targeted edit, and simply verifying an artifact that is already
// correct rather than rewriting it.
func TestAfterFencedExhaustionAMateriallyDifferentEditLands(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "solve.py"),
		[]byte("def solve():\n    return 1\n\nprint(solve())\n"), 0o644)
	// Steps are scripted rather than sniffed from the prompt: the model
	// still only STARTS them once it has been told the channel is spent,
	// which is the conditional part that matters.
	step := 0
	ctx, turns, subcalls, _, terminal := fencedLoopFixture(t, dir, fencedTurnCeiling,
		func(i int, prompt string) map[string]interface{} {
			if !strings.Contains(prompt, fencedRecoveryMark) {
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
			}
			step++
			switch step {
			case 1:
				return map[string]interface{}{"type": "tool_call", "name": "read_file",
					"args": map[string]string{"path": "solve.py"}}
			case 2:
				return map[string]interface{}{"type": "tool_call", "name": "edit_file",
					"args": map[string]string{"path": "solve.py",
						"old_str": "return 1", "new_str": "return 42"}}
			case 3:
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "python3 solve.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "changed the return value"}
			}
		})
	if err := runAgentLoop(ctx, "Change solve.py to return 42."); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	t.Logf("edit route: turns=%d sub-generations=%d status=%q disk=%q",
		*turns, *subcalls, terminal["status"], string(got))
	if !strings.Contains(string(got), "return 42") {
		t.Errorf("the targeted edit never landed: %q", got)
	}
	if *subcalls > maxFencedFailuresPerPath {
		t.Errorf("%d sub-generations after exhaustion", *subcalls)
	}
}

func TestAfterFencedExhaustionACorrectArtifactIsVerifiedNotRewritten(t *testing.T) {
	dir := t.TempDir()
	const good = "def solve():\n    return 3\n\nprint(solve())\n"
	os.WriteFile(filepath.Join(dir, "solve.py"), []byte(good), 0o644)
	before := hashBytes([]byte(good))
	ctx, turns, subcalls, _, terminal := fencedLoopFixture(t, dir, fencedTurnCeiling,
		func(i int, prompt string) map[string]interface{} {
			saw := strings.Contains(prompt, fencedRecoveryMark)
			ran := strings.Contains(prompt, "ran_marker")
			switch {
			case !saw:
				return map[string]interface{}{"type": "tool_call", "name": "write_file",
					"args": map[string]string{"path": "solve.py", "content": "@fenced"}}
			case !ran:
				// The recovery showed the file; it is already right, so
				// the model verifies instead of rewriting.
				return map[string]interface{}{"type": "tool_call", "name": "run_command",
					"args": map[string]string{"command": "echo ran_marker; python3 solve.py"}}
			default:
				return map[string]interface{}{"type": "done", "summary": "solve.py already returns 3; ran it"}
			}
		})
	if err := runAgentLoop(ctx, "Make sure solve.py prints 3."); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "solve.py"))
	t.Logf("verify route: turns=%d sub-generations=%d status=%q reason=%q",
		*turns, *subcalls, terminal["status"], terminal["reason"])
	if hashBytes(got) != before {
		t.Errorf("a correct artifact was rewritten: %q", got)
	}
	if *subcalls > maxFencedFailuresPerPath {
		t.Errorf("%d sub-generations after exhaustion", *subcalls)
	}
	if !TerminalStatus(terminal["status"]).Classified() {
		t.Error("no classified terminal")
	}
}
