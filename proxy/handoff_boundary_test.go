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
)

// The handoff between a delivered candidate and the turn that follows it.
//
// These drive the real agent loop: a stub model on /v1/chat/completions, a
// producer on /v3/generate that returns its OWN version of the file, and the
// live write route between them. What is asserted is what the next turn was
// actually sent and what the next edit actually operated on -- not what a
// helper returns in isolation.
//
// The sessions that motivated them are stabilization12/PAIRS2 01-O-B-rep1 and
// 05-O-B-rep2: byte-identical submissions, byte-identical adoptions, and in
// both the model overwrote the delivered candidate on its next turn without
// ever being shown it.

// What the model sends. Tier2 by the classifier: a .py file over ten lines
// with logic.
const handoffSubmitted = `ITEMS = {}


def add(item):
    ITEMS[item] = True


def find(name):
    for item in ITEMS:
        if item == name:
            return item
    return None


def count():
    return len(ITEMS)
`

// What the producer delivers instead -- same interface, different body, and a
// line the submission does not contain.
const handoffDelivered = `ITEMS = {}


def add(item):
    ITEMS[item] = True


def find(name):
    return name if name in ITEMS else None


def count():
    return len(ITEMS)
`

type handoffWorld struct {
	dir      string
	ctx      *AgentContext
	prompts  []string
	terminal map[string]string
	v3Calls  int
	mu       sync.Mutex
}

// prompt returns the body of the n-th call the loop made to the model.
func (w *handoffWorld) prompt(t *testing.T, n int) string {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if n >= len(w.prompts) {
		t.Fatalf("only %d model calls were made; wanted #%d", len(w.prompts), n)
	}
	return w.prompts[n]
}

// lastPrompt is the final context the loop assembled.
func (w *handoffWorld) lastPrompt(t *testing.T) string {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.prompts) == 0 {
		t.Fatal("the loop never called the model")
	}
	return w.prompts[len(w.prompts)-1]
}

func (w *handoffWorld) disk(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.dir, "inv.py"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func v3Envelope(winner string) map[string]interface{} {
	h := contentSHA256(winner)
	return map[string]interface{}{
		// "phase1" is what the live pipeline reported in the two sessions this
		// file reconstructs, and it is what verifiedPhase accepts -- the nudge
		// under test only fires for a phase the guard recognises.
		"code": winner, "passed": true, "phase_solved": "phase1",
		"candidates_tested": 3, "winning_score": 0.42,
		"evidence": map[string]interface{}{
			"wire_version": "1.0.0", "record_schema_version": "1.1.0",
			"identity": map[string]interface{}{
				"contract_id": "c.v1", "contract_version": "1",
				"adapter_id": "python_compile", "adapter_version": "0.1.0-prototype",
				"artifact_scope": "inv.py", "evaluation_context_hash": "ctx",
				"candidate_content_hash": h,
			},
			"evaluation": map[string]interface{}{
				"execution_status": "ok", "supported": true,
				"evidence_strength": "syntax", "requirements_complete": true,
				"closure_eligible": false,
				"quality": map[string]interface{}{
					"required_coverage": 1.0, "optional_quality": 1.0, "overall": 1.0},
			},
			"coverage":  map[string]interface{}{"required": []string{}, "demonstrated": []string{}},
			"selection": map[string]interface{}{"status": "best_not_closure_eligible", "reason": "highest"},
			"delivery": map[string]interface{}{
				"delivered_content_hash": h, "describes_delivered_candidate": true},
		},
	}
}

// runHandoffWorld plays one script per turn through the real loop, with a
// producer that delivers `winner` to whatever the model writes.
func runHandoffWorld(t *testing.T, winner string, scripts []string) *handoffWorld {
	t.Helper()
	w := &handoffWorld{dir: t.TempDir(), terminal: map[string]string{}}
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v3/generate"):
			w.mu.Lock()
			w.v3Calls++
			w.mu.Unlock()
			body, _ := json.Marshal(v3Envelope(winner))
			rw.Header().Set("Content-Type", "text/event-stream")
			fl, _ := rw.(http.Flusher)
			for _, line := range []string{"event: result", "data: " + string(body), "", "data: [DONE]", ""} {
				fmt.Fprint(rw, line+"\n")
				if fl != nil {
					fl.Flush()
				}
			}
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(rw).Encode(map[string]interface{}{"valid": true})
			return
		case r.URL.Path == "/internal/structural_check":
			json.NewEncoder(rw).Encode(map[string]interface{}{"ok": true, "unresolved": []string{}})
			return
		case r.URL.Path == "/internal/cyclomatic_complexity":
			json.NewEncoder(rw).Encode(map[string]interface{}{"functions": []interface{}{}})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"), strings.HasSuffix(r.URL.Path, "/shell"):
			var in struct{ Code, Command string }
			json.NewDecoder(r.Body).Decode(&in)
			out := ""
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(w.dir, ".atlas-mount-probe"))
				out = string(b)
			}
			json.NewEncoder(rw).Encode(map[string]interface{}{
				"success": true, "stdout": out, "exit_code": 0})
			return
		case strings.HasPrefix(r.URL.Path, "/internal/"):
			json.NewEncoder(rw).Encode(map[string]interface{}{"ok": true})
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(rw, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		k := turn
		turn++
		w.prompts = append(w.prompts, string(body))
		w.mu.Unlock()
		rw.Header().Set("Content-Type", "text/event-stream")
		send := func(s string) {
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{{"delta": map[string]string{"content": s}}}})
			fmt.Fprintf(rw, "data: %s\n\n", d)
			if f, ok := rw.(http.Flusher); ok {
				f.Flush()
			}
		}
		script := `{"type":"done","summary":"finished"}`
		if k < len(scripts) {
			script = scripts[k]
		}
		send(script)
		fmt.Fprint(rw, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(w.dir, Tier2Medium)
	// Every real request carries one; the mutation scope refuses to exist
	// without it, and a candidate with no scope is vetoed before any of this
	// is reached.
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-handoff-loop")
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	// The interactive request: work, automatic_v3, and nothing about the task.
	ctx.TaskContract = mustContract(t, w.dir, `{"task_mode":"work","candidate_policy":"automatic_v3"}`)
	ctx.StreamFn = func(et string, data interface{}) {
		if et != "done" {
			return
		}
		b, _ := json.Marshal(data)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		w.mu.Lock()
		defer w.mu.Unlock()
		for k, v := range m {
			w.terminal[k] = fmt.Sprint(v)
		}
	}
	w.ctx = ctx
	if err := runAgentLoop(ctx, "keep track of the items we lend out"); err != nil {
		t.Fatal(err)
	}
	return w
}

func writeInv(content string) string {
	return toolCall("write_file", map[string]interface{}{"path": "inv.py", "content": content})
}

// --- 1. Adoption reports the actual delivered state -------------------------

func TestAdoptionReportsTheActualDeliveredState(t *testing.T) {
	w := runHandoffWorld(t, handoffDelivered, []string{
		writeInv(handoffSubmitted),
		`{"type":"done","summary":"done"}`,
	})
	if w.v3Calls != 1 {
		t.Fatalf("the producer was consulted %d times, want 1", w.v3Calls)
	}
	if got := w.disk(t); got != handoffDelivered {
		t.Fatalf("precondition: the candidate did not land")
	}
	next := w.prompt(t, 1)

	// The model is told, in the turn that follows, that what landed is not
	// what it sent.
	if !strings.Contains(next, "are NOT the ones you sent") {
		t.Error("the next turn was not told its content was replaced")
	}
	if !strings.Contains(next, "inv.py") {
		t.Error("the note does not name the file")
	}
	// And it is not told to stay away from the only copy that matters.
	if strings.Contains(next, "do not re-read the file to double-check") {
		t.Error("the model was told not to re-read a file it has never seen")
	}
	// The session's own view of the path is the delivered content, so every
	// downstream reader -- staleness, restatement, edit authorization -- is
	// about what is on disk.
	if got := w.ctx.FilesRead[filepath.Join(w.dir, "inv.py")]; got != handoffDelivered {
		t.Errorf("read state holds %d bytes, want the delivered %d", len(got), len(handoffDelivered))
	}
	if p, c := w.ctx.LastRead(); c != handoffDelivered {
		t.Errorf("LastRead is %s (%d bytes), not the delivered version", p, len(c))
	}
}

// An ordinary write -- one the producer did not replace -- says nothing new
// and keeps the original nudge. The correction is about supersession, not
// about V3 having run.
func TestAnUnreplacedWriteSaysNothingAboutSupersession(t *testing.T) {
	w := runHandoffWorld(t, handoffSubmitted, []string{ // the producer returns the model's own bytes
		writeInv(handoffSubmitted),
		`{"type":"done","summary":"done"}`,
	})
	if got := w.disk(t); got != handoffSubmitted {
		t.Fatalf("precondition: the model's own bytes are not on disk")
	}
	if strings.Contains(w.prompt(t, 1), "are NOT the ones you sent") {
		t.Error("an unreplaced write claimed its content was replaced")
	}
}

// --- 2. A later edit cannot unknowingly operate against superseded contents -

func TestALaterEditCannotOperateAgainstSupersededContents(t *testing.T) {
	// `for item in ITEMS:` exists only in what the model SENT. An edit keyed
	// to it is an edit against a version that is not on disk.
	w := runHandoffWorld(t, handoffDelivered, []string{
		writeInv(handoffSubmitted),
		toolCall("edit_file", map[string]interface{}{
			"path": "inv.py", "old_str": "    for item in ITEMS:\n        if item == name:\n            return item\n    return None\n",
			"new_str": "    return None\n"}),
		`{"type":"done","summary":"done"}`,
	})
	if got := w.disk(t); got != handoffDelivered {
		t.Errorf("an edit against superseded text changed the delivered file:\n%q", got)
	}
	// It failed, and the failure reached the model rather than passing
	// silently.
	last := w.lastPrompt(t)
	if !strings.Contains(last, "old_str") && !strings.Contains(last, "not found") &&
		!strings.Contains(last, "no match") {
		t.Error("the model was not told its edit did not match what is on disk")
	}
}

// --- 3. Legitimate modifications remain possible ----------------------------

func TestLegitimateModificationsToAnAdoptedCandidateRemainPossible(t *testing.T) {
	// This `find` body exists only in the DELIVERED version. Editing it proves
	// the file is not locked and that the model can work from what landed.
	w := runHandoffWorld(t, handoffDelivered, []string{
		writeInv(handoffSubmitted),
		toolCall("edit_file", map[string]interface{}{
			"path": "inv.py", "old_str": "    return name if name in ITEMS else None\n",
			"new_str": "    return name if name in ITEMS else None  # revised\n"}),
		`{"type":"done","summary":"done"}`,
	})
	got := w.disk(t)
	if !strings.Contains(got, "else None  # revised\n") {
		t.Fatalf("a legitimate edit to the adopted candidate did not land:\n%q", got)
	}
	if got == handoffDelivered {
		t.Error("the delivered candidate was locked against revision")
	}
	// And the session's view keeps up with the revision.
	if w.ctx.FilesRead[filepath.Join(w.dir, "inv.py")] != got {
		t.Error("read state did not follow the edit")
	}
}

// --- 4. Relevant modifications invalidate prior verification ----------------

func TestARelevantModificationInvalidatesPriorVerification(t *testing.T) {
	w := runHandoffWorld(t, handoffDelivered, []string{
		writeInv(handoffSubmitted),
		`{"type":"done","summary":"done"}`,
	})
	path := filepath.Join(w.dir, "inv.py")
	// A verification record bound to the bytes that were on disk when it ran.
	rec := VerificationRecord{
		Command: "python inv.py",
		Covered: map[string]string{"inv.py": fileSHA256(w.ctx, "inv.py")},
	}
	if _, ok := evidenceIsCurrent(w.ctx, rec); !ok {
		t.Fatal("precondition: evidence bound to the current file is not current")
	}
	// Any relevant change -- the model's, or a later delivery's -- moves the
	// bytes, and the record stops covering them.
	if err := os.WriteFile(path, []byte(handoffDelivered+"\n# revised\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := evidenceIsCurrent(w.ctx, rec); ok {
		t.Error("a modified file still counted as verified by the earlier run")
	}
}

// --- 5. Adoption is not a finished plan step and not a finished request -----

func TestCandidateAdoptionDoesNotCertifyTheRequest(t *testing.T) {
	w := runHandoffWorld(t, handoffDelivered, []string{
		writeInv(handoffSubmitted),
		`{"type":"done","summary":"done"}`,
	})
	if got := w.disk(t); got != handoffDelivered {
		t.Fatalf("precondition: the candidate did not land")
	}
	// The pipeline's build check is not evidence that the user's request is
	// finished, and the turn after an adoption must not be told it is.
	next := w.prompt(t, 1)
	if strings.Contains(next, "respond NOW with") {
		t.Error("the model was told to declare the request done off a build check " +
			"on bytes it has not seen")
	}
	if !strings.Contains(next, "not evidence that the user's request is finished") {
		t.Error("the nudge does not distinguish a build check from a finished request")
	}
	// And the completion boundary is not persuaded either.
	status, reason := finalizeCompletion(w.ctx, &runState{}, "keep track of the items we lend out", "")
	if status == TerminalCompleted {
		t.Errorf("an adopted candidate was read as a finished request (%q)", reason)
	}
	// Adoption does not mark the plan finished. (A world with no plan has
	// nothing to satisfy, which is not evidence either way.)
	if w.ctx.Plan != nil && len(w.ctx.Plan.Steps) > 0 &&
		countTrue(w.ctx.PlanStepsSatisfied) == len(w.ctx.Plan.Steps) {
		t.Error("adoption satisfied every plan step")
	}
}

// The lens corrective is about the bytes the model sent. When those bytes were
// replaced, telling it "do not re-issue that write" is advice about a file
// that does not exist -- and it was the instruction both adopting sessions
// acted on. The score itself is still taken and still recorded.
func TestALensCorrectiveAboutReplacedBytesIsNotDelivered(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.Ctx = context.WithValue(context.Background(), requestIDKey, "req-handoff")
	path := filepath.Join(dir, "inv.py")
	if err := os.WriteFile(path, []byte(handoffDelivered), 0o644); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"path": "inv.py", "content": handoffSubmitted})
	s, superseded := deliveredDiffersFromSubmitted(ctx, "write_file", args, &ToolResult{Success: true})
	if !superseded {
		t.Fatal("a write whose bytes differ from disk was not reported as superseded")
	}
	if s.Delivered != handoffDelivered || s.Submitted != handoffSubmitted {
		t.Error("the owner did not carry both versions")
	}
	note := adoptDeliveredContent(ctx, s)
	for _, want := range []string{"inv.py", "are NOT the ones you sent", "Nothing here says which is better"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note omits %q: %s", want, note)
		}
	}
	// It states no preference. "Better", "improved", "verified" about the
	// candidate would all be claims nothing here can support.
	for _, forbidden := range []string{"improved", "better version", "higher quality"} {
		if strings.Contains(strings.ToLower(note), forbidden) {
			t.Errorf("the note claims %q", forbidden)
		}
	}
	if ctx.FilesRead[path] != handoffDelivered {
		t.Error("read state does not track the delivered version")
	}
	// Identical bytes are not a supersession.
	same, _ := json.Marshal(map[string]string{"path": "inv.py", "content": handoffDelivered})
	if _, got := deliveredDiffersFromSubmitted(ctx, "write_file", same, &ToolResult{Success: true}); got {
		t.Error("an unchanged write was reported as superseded")
	}
	// Neither is a failed one.
	if _, got := deliveredDiffersFromSubmitted(ctx, "write_file", args, &ToolResult{Success: false}); got {
		t.Error("a failed write was reported as superseded")
	}
}
