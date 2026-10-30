package main

import (
	"context"
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
	"time"
)

// --- a successful delete does not end the session ---------------------------
//
// Asked to delete a.py AND write report.py, a run that deletes first goes on
// to write report.py, so the outcome does not depend on tool order. That does
// not make a deletion completable on its own: the tombstone rule at the real
// terminal still decides, and the run is judged on all of its work.

// delSymlinkTo marks a seed entry that should be created as a relative
// symlink to real.py rather than as a file.
const delSymlinkTo = "\x00symlink\x00"

const delSeed = "def solve():\n    return 7\n\n\nprint(solve())\n"

type delLoop struct {
	dir      string
	turns    int
	census   map[string]int
	terminal map[string]string
	seq      []string
	events   []delEvent
	ctx      *AgentContext
}

// delEvent is one stream event of a fixture run, as the client receives it.
type delEvent struct {
	kind string
	data string
}

// delLoopDecision is how the fixture answers each deletion prompt: "allow",
// "deny", "cancel" (end the request while the prompt waits), or "" (leave the
// prompt unanswered until it times out). It is package-level so most call
// sites need not pass it; delLoopFixtureAnswering sets it for one run.
var delLoopDecision = "allow"

func delLoopFixtureApprove(t *testing.T, seed map[string]string, prompt string,
	plan func(i int) map[string]interface{}, approve bool) *delLoop {
	t.Helper()
	decision := "deny"
	if approve {
		decision = "allow"
	}
	return delLoopFixtureAnswering(t, seed, prompt, plan, decision)
}

func delLoopFixtureAnswering(t *testing.T, seed map[string]string, prompt string,
	plan func(i int) map[string]interface{}, decision string) *delLoop {
	t.Helper()
	prev := delLoopDecision
	delLoopDecision = decision
	t.Cleanup(func() { delLoopDecision = prev })
	return delLoopFixture(t, seed, prompt, plan)
}

func delLoopFixture(t *testing.T, seed map[string]string, prompt string,
	plan func(i int) map[string]interface{}) *delLoop {
	t.Helper()
	r := &delLoop{dir: t.TempDir(), census: map[string]int{}, terminal: map[string]string{}}
	for n, b := range seed {
		p := filepath.Join(r.dir, n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if b == delSymlinkTo {
			os.Symlink("real.py", p)
			continue
		}
		os.WriteFile(p, []byte(b), 0o644)
	}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasPrefix(req.URL.Path, "/v3/"), strings.HasPrefix(req.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(req.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(req.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(req.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(r.dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "stdout": "", "exit_code": 0})
			return
		case !strings.HasSuffix(req.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, req)
			return
		}
		io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		mu.Lock()
		i := r.turns
		r.turns++
		mu.Unlock()
		if i >= 20 {
			http.Error(w, "ceiling", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(srv.Close)

	r.ctx = NewAgentContext(r.dir, Tier2Medium)
	r.ctx.InferenceURL, r.ctx.SandboxURL, r.ctx.V3URL = srv.URL, srv.URL, srv.URL
	r.ctx.PermissionMode = PermissionYolo
	r.ctx.TrustMode = trustFullyTrusted
	r.ctx.VerifyOnHost = true
	r.ctx.MaxTurns = 0
	request, endRequest := context.WithCancel(context.Background())
	t.Cleanup(endRequest)
	r.ctx.Ctx = request
	r.ctx.PassID = "delloop-" + filepath.Base(r.dir)
	if delLoopDecision != "" {
		autoAnswerDeletes(t, r.ctx.PassID, delLoopDecision, endRequest)
	}
	r.ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		r.census[et]++
		r.events = append(r.events, delEvent{et, string(b)})
		switch et {
		case "tool_call":
			var tc struct{ Name string }
			json.Unmarshal(b, &tc)
			r.seq = append(r.seq, tc.Name)
		case "done":
			var m map[string]string
			json.Unmarshal(b, &m)
			maps.Copy(r.terminal, m)
		}
	}
	runAgentLoop(r.ctx, prompt)
	return r
}

func (r *delLoop) present() []string {
	var out []string
	filepath.Walk(r.dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(r.dir, p)
		if !strings.HasPrefix(rel, ".") {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (r *delLoop) tombstones() []string {
	var out []string
	for k, d := range r.ctx.Ledger {
		if d.Tombstoned {
			out = append(out, fmt.Sprintf("%s{%s,prohibited=%v}",
				filepath.Base(k), d.TombstoneReason, d.RestoreProhibited))
		}
	}
	sort.Strings(out)
	return out
}

func (r *delLoop) check(t *testing.T, name string) {
	t.Helper()
	t.Logf("%s: seq=%v disk=%v tombs=%v status=%q reason=%q calls=%d results=%d",
		name, r.seq, r.present(), r.tombstones(), r.terminal["status"], r.terminal["reason"],
		r.census["tool_call"], r.census["tool_result"])
	if r.census["done"] != 1 {
		t.Errorf("%d terminal events", r.census["done"])
	}
	if r.census["tool_call"] != r.census["tool_result"] {
		t.Errorf("call/result imbalance %d vs %d", r.census["tool_call"], r.census["tool_result"])
	}
}

func dlDel(p string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "delete_file",
		"args": map[string]string{"path": p}}
}
func dlWrite(p, c string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "write_file",
		"args": map[string]string{"path": p, "content": c}}
}

// The requested write after the delete is still attempted.
func TestDeleteDoesNotSwallowTheRestOfTheTask(t *testing.T) {
	needsObjectHold(t)
	r := delLoopFixture(t, map[string]string{"a.py": delSeed},
		"Delete a.py and create report.py with the summary.",
		func(i int) map[string]interface{} {
			switch i {
			case 0:
				return dlDel("a.py")
			case 1:
				return dlWrite("report.py", delSeed)
			}
			return map[string]interface{}{"type": "done", "summary": "deleted a.py, wrote report.py"}
		})
	r.check(t, "delete-first")

	if len(r.seq) < 2 || r.seq[1] != "write_file" {
		t.Fatalf("the run ended at the delete: %v — the requested write was never attempted", r.seq)
	}
	got := r.present()
	if len(got) != 1 || got[0] != "report.py" {
		t.Errorf("disk is %v, want only report.py", got)
	}
	// The deletion itself: confirmed absence, tombstoned, restoration
	// prohibited.
	tomb := r.tombstones()
	if len(tomb) != 1 || !strings.Contains(tomb[0], "a.py{deleted,prohibited=true}") {
		t.Errorf("tombstone state changed: %v", tomb)
	}
	// This fixture approves the deletion, so the run is judged on all of its
	// work: the removal the user authorised AND the file it was asked to
	// write. The subject here is that the write happened at all. Nothing ran
	// report.py, so the deliverable's reason is the parse-only one.
	if r.terminal["reason"] != "deliverables_parse_only" {
		t.Errorf("reason=%q, want the deliverable to own the terminal", r.terminal["reason"])
	}
}

// script (edit_route_accounting_test.go) builds a plan from the model's turns
// in order: turn i gets the i-th step, and the last step repeats on every
// later turn.

// Every other delete shape: the loop continues, and the terminal is decided at
// the real terminal point by the rules that already exist.
func TestDeleteContinuationMatrix(t *testing.T) {
	needsObjectHold(t)
	for _, c := range []struct {
		name, prompt string
		seed         map[string]string
		plan         func(i int) map[string]interface{}
		wantSeq      int
		wantReason   string
	}{
		{"write then delete", "Create report.py with the summary.",
			map[string]string{"a.py": delSeed},
			script(dlWrite("report.py", delSeed), dlDel("a.py"), stepDone("wrote report.py")),
			2, "deliverables_parse_only"},
		{"delete only", "Delete a.py.",
			map[string]string{"a.py": delSeed},
			script(dlDel("a.py"), stepDone("deleted a.py")),
			1, "approved_deletions_demonstrated"},
		{"delete then a read that only happens if the loop continues", "Delete a.py.",
			map[string]string{"a.py": delSeed, "b.py": delSeed},
			script(dlDel("a.py"), stepRead("b.py"), stepDone("deleted a.py")),
			2, "approved_deletions_demonstrated"},
		// The model asks for the same deletion on every turn.
		{"repeated delete of a now-absent path", "Delete a.py.",
			map[string]string{"a.py": delSeed},
			script(dlDel("a.py")),
			2, ""},
		// The structured intent is recorded before the target is judged, so a
		// refused deletion still owes something and cannot report completed.
		{"non-empty directory is refused", "Delete pkg.",
			map[string]string{"pkg/mod.py": delSeed},
			script(dlDel("pkg"), stepDone("removed pkg")),
			1, "unresolved_mutation_debt"},
		{"missing path fails", "Delete gone.py.",
			map[string]string{},
			script(dlDel("gone.py"), stepDone("not there")),
			1, "no_file_obligation"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := delLoopFixture(t, c.seed, c.prompt, c.plan)
			r.check(t, c.name)
			if len(r.seq) < c.wantSeq {
				t.Errorf("%d tool calls, want at least %d — the loop stopped early", len(r.seq), c.wantSeq)
			}
			if c.wantReason != "" && r.terminal["reason"] != c.wantReason {
				t.Errorf("reason=%q, want %q", r.terminal["reason"], c.wantReason)
			}
			if r.turns >= 20 {
				t.Errorf("%d turns without a terminal", r.turns)
			}
		})
	}
}

// --- A demonstrated move is not a deletion ------------------------------------
//
// The tombstone rule refuses completion whenever anything was removed, because
// whether removing it was the task is not knowable. That is right for a plain
// delete. A move is a different fact: the source is gone AND the bytes are
// somewhere the ledger can point at, and TombstoneReason already records where
// (`moved:<canonical destination>`). Nothing about user intent has to be
// guessed to see that, so a move whose destination is demonstrated is not
// treated as an unexplained removal. A plain deletion is still refused.

func dlMove(src, dst string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "move_file",
		"args": map[string]string{"source": src, "destination": dst}}
}

func TestDemonstratedMoveDoesNotBlockCompletion(t *testing.T) {
	for _, c := range []struct {
		name, prompt string
		seed         map[string]string
		plan         func(i int) map[string]interface{}
		// Deletions are approved only where the row says so: the other rows
		// show that an unapproved removal still blocks.
		approveDeletes bool
		wantStatus     TerminalStatus
	}{
		{"a validated move completes", "Rename old.py to new.py.",
			map[string]string{"old.py": delSeed},
			script(dlMove("old.py", "new.py"), stepDone("renamed old.py to new.py")),
			true, TerminalCompleted},
		{"several demonstrated moves complete", "Rename a.py to x.py and b.py to y.py.",
			map[string]string{"a.py": delSeed, "b.py": delSeed},
			script(dlMove("a.py", "x.py"), dlMove("b.py", "y.py"), stepDone("renamed both")),
			true, TerminalCompleted},
		// An unapproved deletion never reaches a tombstone (nothing is
		// removed), so the debt it still owes stops it earlier. The property
		// is that it cannot complete; which gate says so is not the subject.
		{"an unapproved delete still cannot complete", "Delete a.py.",
			map[string]string{"a.py": delSeed},
			script(dlDel("a.py"), stepDone("deleted a.py")),
			false, TerminalIncomplete},
		{"an unapproved unrequested delete still cannot complete", "Make the tests pass.",
			map[string]string{"a.py": delSeed},
			script(dlDel("a.py"), stepDone("removed the failing file")),
			false, TerminalIncomplete},
		{"a move and a delete together still cannot complete", "Rename a.py to x.py.",
			map[string]string{"a.py": delSeed, "b.py": delSeed},
			script(dlMove("a.py", "x.py"), dlDel("b.py"), stepDone("renamed and cleaned up")),
			false, TerminalIncomplete},
		{"destination deleted after the move", "Rename old.py to new.py.",
			map[string]string{"old.py": delSeed},
			script(dlMove("old.py", "new.py"), dlDel("new.py"), stepDone("renamed")),
			false, TerminalIncomplete},
		{"text exit after a demonstrated move", "Rename old.py to new.py.",
			map[string]string{"old.py": delSeed},
			script(dlMove("old.py", "new.py"),
				map[string]interface{}{"type": "text", "content": "Renamed old.py to new.py."}),
			true, TerminalCompleted},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := delLoopFixtureApprove(t, c.seed, c.prompt, c.plan, c.approveDeletes)
			r.check(t, c.name)
			t.Logf("   summary=%.140s", r.terminal["summary"])
			got := NormalizeTerminalStatus(r.terminal["status"])
			if got != c.wantStatus {
				t.Errorf("status=%q reason=%q, want %s",
					r.terminal["status"], r.terminal["reason"], c.wantStatus)
			}
			// Restoration stays prohibited on every tombstone, always.
			for k, d := range r.ctx.Ledger {
				if d.Tombstoned && !d.RestoreProhibited {
					t.Errorf("%s is tombstoned without RestoreProhibited", filepath.Base(k))
				}
			}
			// An incomplete move must never claim nothing was written while
			// its destination is on disk. A denied deletion is different: the
			// seed file is still there because nothing happened, so saying
			// nothing was written is the truth.
			movedSomething := slices.Contains(r.seq, "move_file")
			if movedSomething && !got.Completed() && len(r.present()) > 0 &&
				claimsNothingWritten(r.terminal["summary"]) {
				t.Errorf("summary claims nothing was written while %v is on disk:\n%s",
					r.present(), r.terminal["summary"])
			}
		})
	}
}

// The predicate itself, on the cases a loop fixture cannot stage cleanly.
func TestMoveTombstoneDemonstrationPredicate(t *testing.T) {
	for _, c := range []struct {
		name   string
		reason string
		mutate func(dir string, src, dst *DeliverableState)
		want   bool // true = still blocks completion
	}{
		{"demonstrated move", "moved:<dst>", nil, false},
		{"plain deletion", "deleted", nil, true},
		{"destination missing", "moved:<dst>", func(dir string, _, _ *DeliverableState) {
			os.Remove(filepath.Join(dir, "new.py"))
		}, true},
		{"destination stale in the ledger", "moved:<dst>", func(_ string, _, dst *DeliverableState) {
			dst.CurrentHash = hashBytes([]byte("other"))
		}, true},
		// move_file deliberately records the destination as unknown -- a pass
		// earned under the old name says nothing about this path -- so the
		// demonstration is read from disk, not inherited. Unknown plus a
		// matching hash plus bytes that pass IS demonstrated; what is not
		// demonstrated is covered by the stale-hash and bad-bytes cases.
		{"unknown ledger verdict, bytes still demonstrate", "moved:<dst>",
			func(_ string, _, dst *DeliverableState) {
				dst.ValidationStatus, dst.ValidatedHash = ValidationUnknown, ""
			}, false},
		{"source still exists", "moved:<dst>", func(dir string, _, _ *DeliverableState) {
			os.WriteFile(filepath.Join(dir, "old.py"), []byte(delSeed), 0o644)
		}, true},
		{"malformed move reason", "moved:", nil, true},
		{"move reason naming nothing on disk", "moved:/nowhere/gone.py", nil, true},
		{"destination bytes no longer parse", "moved:<dst>", func(dir string, _, dst *DeliverableState) {
			bad := []byte("def solve(:\n")
			os.WriteFile(filepath.Join(dir, "new.py"), bad, 0o644)
			dst.CurrentHash, dst.ValidatedHash = hashBytes(bad), hashBytes(bad)
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			// The demonstration re-reads from disk and asks the checker, so
			// the predicate needs one to answer.
			syn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var in struct{ Code string }
				json.NewDecoder(req.Body).Decode(&in)
				valid := !strings.Contains(in.Code, "def solve(:")
				out := map[string]interface{}{"valid": valid}
				if !valid {
					out["errors"] = []string{"invalid syntax (line 1)"}
				}
				json.NewEncoder(w).Encode(out)
			}))
			t.Cleanup(syn.Close)
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.SandboxURL = syn.URL
			if ctx.Ledger == nil {
				ctx.Ledger = map[string]*DeliverableState{}
			}
			os.WriteFile(filepath.Join(dir, "new.py"), []byte(delSeed), 0o644)
			h := hashBytes([]byte(delSeed))
			dst := &DeliverableState{Path: "new.py", CurrentHash: h, Generation: 1,
				ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
				ValidatedHash: h}
			src := &DeliverableState{Path: "old.py", Generation: 1, Tombstoned: true,
				RestoreProhibited: true,
				TombstoneReason:   strings.Replace(c.reason, "<dst>", ledgerKey(ctx, "new.py"), 1)}
			ctx.Ledger[ledgerKey(ctx, "old.py")] = src
			ctx.Ledger[ledgerKey(ctx, "new.py")] = dst
			if c.mutate != nil {
				c.mutate(dir, src, dst)
			}

			got := blockingTombstone(ctx)
			t.Logf("%s: blocks=%v", c.name, got)
			if got != c.want {
				t.Errorf("blocks=%v, want %v", got, c.want)
			}
		})
	}
}

// The rest of the move matrix: partial work and aliases.
func TestDemonstratedMoveMatrixRemaining(t *testing.T) {
	t.Run("move plus an unmet named deliverable", func(t *testing.T) {
		r := delLoopFixture(t, map[string]string{"old.py": delSeed},
			"Rename old.py to new.py and create report.py with the summary.",
			script(dlMove("old.py", "new.py"), stepDone("renamed old.py")))
		r.check(t, "unmet deliverable")
		t.Logf("   summary=%.160s", r.terminal["summary"])
		if NormalizeTerminalStatus(r.terminal["status"]).Completed() {
			t.Errorf("completed with report.py never written: %q", r.terminal["reason"])
		}
		// The unmet obligation is what it names, not the move.
		if r.terminal["reason"] == "delete_intent_unestablished" {
			t.Error("the move is still being reported as an unexplained removal")
		}
		if claimsNothingWritten(r.terminal["summary"]) {
			t.Errorf("summary claims nothing was written while %v is on disk:\n%s",
				r.present(), r.terminal["summary"])
		}
	})

	t.Run("several moves, one not demonstrated", func(t *testing.T) {
		r := delLoopFixture(t, map[string]string{"a.py": delSeed, "b.py": delSeed},
			"Rename a.py to x.py and b.py to y.py.",
			script(dlMove("a.py", "x.py"), dlMove("b.py", "y.py"),
				// y.py is removed again, so that move is no longer shown.
				dlDel("y.py"),
				stepDone("renamed both")))
		r.check(t, "one incomplete")
		if NormalizeTerminalStatus(r.terminal["status"]).Completed() {
			t.Errorf("completed with one move undemonstrated: %q", r.terminal["reason"])
		}
	})

	t.Run("alias spellings resolve to one pair", func(t *testing.T) {
		r := delLoopFixture(t, map[string]string{"old.py": delSeed},
			"Rename old.py to new.py.",
			script(dlMove("./old.py", "./new.py"), stepDone("renamed")))
		r.check(t, "aliases")
		if r.terminal["status"] != string(TerminalCompleted) {
			t.Errorf("alias spellings did not resolve: %q/%q",
				r.terminal["status"], r.terminal["reason"])
		}
		if len(r.tombstones()) != 1 {
			t.Errorf("aliases produced %d tombstones: %v", len(r.tombstones()), r.tombstones())
		}
	})
}

// A move that only half happened: the source is gone but the destination
// never landed.
func TestAMoveWhoseDestinationNeverLandedCannotComplete(t *testing.T) {
	needsObjectHold(t)
	// The destination is removed straight after, which is the observable
	// end state of a copy that did not survive.
	r := delLoopFixture(t, map[string]string{"old.py": delSeed},
		"Rename old.py to new.py.",
		script(dlMove("old.py", "new.py"), dlDel("new.py"), stepDone("renamed")))
	r.check(t, "destination gone")
	if NormalizeTerminalStatus(r.terminal["status"]).Completed() {
		t.Errorf("completed with neither source nor destination present: %q",
			r.terminal["reason"])
	}
	if len(r.present()) != 0 {
		t.Errorf("disk should be empty, got %v", r.present())
	}
}

// claimsNothingWritten matches every phrasing the terminal uses to say the run
// left disk untouched. A summary that says this while a deliverable is on disk
// is false whichever wording it reaches for.
func claimsNothingWritten(summary string) bool {
	for _, phrase := range []string{
		"Nothing was written to disk",
		"Nothing was written —",
		"no file was created or changed",
	} {
		if strings.Contains(summary, phrase) {
			return true
		}
	}
	return false
}

// autoAnswerDeletes answers every pending prompt for this session with the
// given decision. Deleting always asks, even in yolo mode, and a fixture
// testing the loop must not wait out the real fail-safe. The decision
// "cancel" ends the request instead of answering.
func autoAnswerDeletes(t *testing.T, sessionID, decision string, endRequest context.CancelFunc) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			pendingPermissions.Range(func(k, _ any) bool {
				key, _ := k.(string)
				if !strings.HasPrefix(key, sessionID+"|") {
					return true
				}
				if decision == "cancel" {
					endRequest()
					return false
				}
				callID := strings.TrimPrefix(key, sessionID+"|")
				body := fmt.Sprintf(
					`{"session_id":%q,"tool_call_id":%q,"decision":%q,"scope":"once"}`,
					sessionID, callID, decision)
				r := httptest.NewRequest(http.MethodPost, "/v1/permission",
					strings.NewReader(body))
				handlePermission(httptest.NewRecorder(), r)
				return true
			})
		}
	}()
}

// A refused deletion owes something, and nothing about the refusal retires it.
//
// The structured intent is recorded once the path parses, canonicalises and
// clears the workspace boundary -- before the target is judged and before the
// user is asked -- so a preflight refusal, a denial, a timeout and a cancel
// all leave the debt standing. Recording the model's tool intent is not user
// authorisation, and no word in the prompt is consulted anywhere here.
func TestRefusedDeletionCannotAuthoriseCompletion(t *testing.T) {
	for _, c := range []struct {
		name       string
		seed       map[string]string
		plan       func(i int) map[string]interface{}
		wantStatus TerminalStatus
		wantReason string
		wantOnDisk []string
	}{
		{"denied deletion then done", map[string]string{"a.py": delSeed},
			script(dlDel("a.py"), stepDone("removed a.py")),
			TerminalIncomplete, "unresolved_mutation_debt", []string{"a.py"}},

		{"denied deletion then unrelated success", map[string]string{"a.py": delSeed},
			script(dlDel("a.py"), dlWrite("report.py", delSeed), stepDone("wrote report.py")),
			TerminalIncomplete, "unresolved_mutation_debt",
			[]string{"a.py", "report.py"}},

		// A rewrite is not a retirement of a delete debt: that debt retires on
		// the path being confirmed ABSENT, and rewriting it leaves it present.
		// The existing causal rule converts content-then-delete, not the other
		// way round, so a denied deletion followed by a same-path write still
		// owes the deletion and the run stays incomplete. Fail-closed, and the
		// alternative would let any later write launder a denied removal.
		{"denied deletion then a same-path rewrite still owes the deletion",
			map[string]string{"a.py": delSeed},
			script(dlDel("a.py"),
				dlWrite("a.py", "def solve():\n    return 8\n\n\nprint(solve())\n"),
				stepDone("rewrote a.py")),
			TerminalIncomplete, "unresolved_mutation_debt", []string{"a.py"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := delLoopFixtureApprove(t, c.seed, "Tidy the workspace.", c.plan, false)
			r.check(t, c.name)
			got := NormalizeTerminalStatus(r.terminal["status"])
			if got != c.wantStatus {
				t.Errorf("status=%q reason=%q, want %s",
					r.terminal["status"], r.terminal["reason"], c.wantStatus)
			}
			if r.terminal["reason"] != c.wantReason {
				t.Errorf("reason=%q, want %q", r.terminal["reason"], c.wantReason)
			}
			for _, want := range c.wantOnDisk {
				if _, err := os.Stat(filepath.Join(r.dir, want)); err != nil {
					t.Errorf("%s is missing: %v", want, err)
				}
			}
		})
	}
}

// A prompt nobody answers in time, and a request that ends while the prompt
// waits, are refusals too, and leave the same debt standing.
func TestUnansweredDeletionCannotAuthoriseCompletion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision string
	}{
		{"timeout", ""},
		{"cancel", "cancel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ATLAS_PERMISSION_TIMEOUT_SEC", "1")
			r := delLoopFixtureAnswering(t, map[string]string{"a.py": delSeed},
				"Tidy the workspace.",
				func(i int) map[string]interface{} {
					if i == 0 {
						return dlDel("a.py")
					}
					return map[string]interface{}{"type": "done", "summary": "removed a.py"}
				}, tc.decision)
			r.check(t, tc.name)
			if NormalizeTerminalStatus(r.terminal["status"]).Completed() {
				t.Errorf("%s reported %q/%q", tc.name, r.terminal["status"], r.terminal["reason"])
			}
			if _, err := os.Stat(filepath.Join(r.dir, "a.py")); err != nil {
				t.Errorf("the file was removed without an answer: %v", err)
			}
		})
	}
}

// --- Completion from fulfilled, user-approved deletions ----------------------
//
// A deletion the user approved and the system carried out is work, and a run
// whose work was exactly that has finished it. An unapproved removal still
// blocks, and a move is judged by its own predicate.

// approvedDeletionRuns are runs in which the user approves every deletion,
// under the prompt "Tidy the workspace.". Each test below runs these rows and
// checks one property; the two summary tests take only the rows that end in
// approved_deletions_demonstrated.
var approvedDeletionRuns = []struct {
	name       string
	seed       map[string]string
	plan       func(i int) map[string]interface{}
	wantStatus TerminalStatus
	wantReason string
	wantGone   []string
	wantThere  []string
}{
	{"one approved deletion", map[string]string{"a.py": delSeed},
		script(dlDel("a.py"), stepDone("removed a.py")),
		TerminalCompleted, "approved_deletions_demonstrated",
		[]string{"a.py"}, nil},

	{"two approved deletions", map[string]string{"a.py": delSeed, "b.py": delSeed},
		script(dlDel("a.py"), dlDel("b.py"), stepDone("removed both")),
		TerminalCompleted, "approved_deletions_demonstrated",
		[]string{"a.py", "b.py"}, nil},

	{"one approved deletion fails", map[string]string{"a.py": delSeed, "pkg/m.py": delSeed},
		script(dlDel("a.py"),
			dlDel("pkg"), // non-empty: refused before asking
			stepDone("tidied up")),
		TerminalIncomplete, "unresolved_mutation_debt",
		[]string{"a.py"}, []string{"pkg/m.py"}},

	{"approved delete plus a demonstrated deliverable",
		map[string]string{"a.py": delSeed},
		script(dlDel("a.py"), dlWrite("report.py", delSeed), stepDone("done")),
		TerminalCompleted, "deliverables_parse_only",
		[]string{"a.py"}, []string{"report.py"}},

	{"approved deletion then the path is recreated",
		map[string]string{"a.py": delSeed},
		script(dlDel("a.py"), dlWrite("a.py", delSeed), stepDone("removed then rewrote a.py")),
		TerminalIncomplete, "delete_intent_unestablished",
		nil, []string{"a.py"}},

	{"approved symlink deletion leaves the target",
		map[string]string{"real.py": delSeed, "link.py": delSymlinkTo},
		script(dlDel("link.py"), stepDone("removed the link")),
		TerminalCompleted, "approved_deletions_demonstrated",
		[]string{"link.py"}, []string{"real.py"}},

	{"alias spelling is one obligation", map[string]string{"a.py": delSeed},
		script(dlDel("./a.py"), stepDone("removed a.py")),
		TerminalCompleted, "approved_deletions_demonstrated",
		[]string{"a.py"}, nil},

	{"deleted, recreated, deleted again with fresh approval", map[string]string{"a.py": delSeed},
		script(dlDel("a.py"), dlWrite("a.py", delSeed), dlDel("a.py"), stepDone("removed a.py")),
		TerminalCompleted, "approved_deletions_demonstrated",
		[]string{"a.py"}, nil},
}

// An approved deletion run ends with the status and the reason its work earns.
func TestApprovedDeletionCompletionMatrix(t *testing.T) {
	needsObjectHold(t)
	for _, c := range approvedDeletionRuns {
		t.Run(c.name, func(t *testing.T) {
			r := delLoopFixtureApprove(t, c.seed, "Tidy the workspace.", c.plan, true)
			r.check(t, c.name)
			t.Logf("   summary=%.150s", r.terminal["summary"])
			if got := NormalizeTerminalStatus(r.terminal["status"]); got != c.wantStatus {
				t.Errorf("status=%q (read as %q) reason=%q, want %s",
					r.terminal["status"], got, r.terminal["reason"], c.wantStatus)
			}
			if r.terminal["reason"] != c.wantReason {
				t.Errorf("reason=%q, want %q", r.terminal["reason"], c.wantReason)
			}
		})
	}
}

// An approved deletion run removes the paths it deleted and leaves the rest.
func TestApprovedDeletionRunsLeaveTheExpectedFiles(t *testing.T) {
	needsObjectHold(t)
	for _, c := range approvedDeletionRuns {
		t.Run(c.name, func(t *testing.T) {
			r := delLoopFixtureApprove(t, c.seed, "Tidy the workspace.", c.plan, true)
			r.check(t, c.name)
			for _, p := range c.wantGone {
				if _, err := os.Lstat(filepath.Join(r.dir, p)); !os.IsNotExist(err) {
					t.Errorf("%s is still there", p)
				}
			}
			for _, p := range c.wantThere {
				if _, err := os.Lstat(filepath.Join(r.dir, p)); err != nil {
					t.Errorf("%s is missing: %v", p, err)
				}
			}
		})
	}
}

// A completed deletion run names what it removed, and never says nothing was
// written.
func TestACompletedDeletionRunNamesWhatItRemoved(t *testing.T) {
	needsObjectHold(t)
	for _, c := range approvedDeletionRuns {
		if c.wantReason != "approved_deletions_demonstrated" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			r := delLoopFixtureApprove(t, c.seed, "Tidy the workspace.", c.plan, true)
			r.check(t, c.name)
			for _, p := range c.wantGone {
				if !strings.Contains(r.terminal["summary"], p) {
					t.Errorf("the summary does not name %s: %q", p, r.terminal["summary"])
				}
			}
			if claimsNothingWritten(r.terminal["summary"]) {
				t.Errorf("the summary claims nothing was written: %q", r.terminal["summary"])
			}
		})
	}
}

// The summary of a completed deletion run carries none of the ledger's words.
func TestACompletedDeletionSummaryLeaksNoLedgerWords(t *testing.T) {
	needsObjectHold(t)
	for _, c := range approvedDeletionRuns {
		if c.wantReason != "approved_deletions_demonstrated" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			r := delLoopFixtureApprove(t, c.seed, "Tidy the workspace.", c.plan, true)
			r.check(t, c.name)
			for _, leak := range []string{"sha256", "tombstone", "RestoreProhibited",
				"generation", "inode", "permission"} {
				if strings.Contains(strings.ToLower(r.terminal["summary"]),
					strings.ToLower(leak)) {
					t.Errorf("the summary leaks %q: %q", leak, r.terminal["summary"])
				}
			}
		})
	}
}
