package main

// Edit-route mutation accounting, through the real agent loop.
//
// The work contract's verification demand binds a passing run to the files the
// session changed: a run that names a changed path covers it, and completion
// requires every changed code deliverable to be covered by a run over its
// CURRENT bytes. The demand takes its deliverables from the ledger, where every
// mutation tool's landing is recorded canonically. The coverage that answered
// it read only the raw-path session-write map, which edit_file and
// structural_edit never wrote to and no edit tool wrote to for a delivered
// candidate. The demand then asked for a bound verification of a file the
// coverage said nobody changed, and every edit task under task_mode work ended
// `verification_demanded_unmet` whatever the model ran. Coverage now reads the
// same ledger identity the demand reads (changedPathsForCoverage).
//
// These tests drive the loop end to end: a scripted model reads, edits, runs
// the file (host execution, real python3) and declares done. They are the RED
// for the defect and the GREEN for the fix, and the matrix below pins the
// behaviour the fix must not change.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// accountingSeed is a module the edit route consults the candidate producer
// for: a code extension, over the 80-line floor editWarrantsV3 applies when no
// complexity figure is available. The V3 service in the fixture is unavailable
// unless a winner is configured, so by default the route ends
// producer_unavailable and the caller's own bytes land.
var accountingSeed = accountingPadding() + `"""A small module the accounting tests edit."""
import sys


def total(values):
    out = 0
    for v in values:
        out += v
    return out


def helper():
    return 1


if __name__ == "__main__":
    print(total([1, 2, 3]), helper())
`

// accountingPadding is seventy lines of module constants, so the seed clears
// the edit route's line floor without changing what the tests edit.
func accountingPadding() string {
	var sb strings.Builder
	for i := 0; i < 70; i++ {
		fmt.Fprintf(&sb, "PAD_%02d = %d\n", i, i)
	}
	return sb.String()
}

// accountingSeedSmall is Tier1 (under ten lines): the edit pipeline is
// bypassed and the tool lands its own bytes directly.
const accountingSeedSmall = "def helper():\n    return 1\n\n\nprint(helper())\n"

const tuiStrictWork = `{"task_mode":"work","candidate_policy":"strict"}`

type editLoop struct {
	dir      string
	turns    int
	census   map[string]int
	terminal map[string]string
	seq      []string
	results  []map[string]interface{}
	prompts  []string
	ctx      *AgentContext
	mu       sync.Mutex
}

type editLoopOptions struct {
	// v3Winner, when set, makes /v3/generate return an authorized winner with
	// a complete evidence envelope; otherwise V3 is unavailable (503).
	v3Winner string
	// cancelBeforeTool cancels the request context when the named tool is
	// about to run, so the route observes a cancelled request.
	cancelBeforeTool string
	// lensOff points the run at a lens that answers every score with no
	// model loaded, so the per-write scoring finds the lens down.
	lensOff bool
	// v3LensUnavailable makes /v3/generate report that the lens could not
	// score, with this reason.
	v3LensUnavailable string
}

func requirePython3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not on PATH; the loop verifies on the host")
	}
}

// editLoopFixture runs the real loop with a scripted model. Tool calls the
// script emits are executed by the real tools against a temp workspace; V3 and
// the sandbox are served by an httptest server; run_command executes on the
// host so a verification actually runs the edited file.
func editLoopFixture(t *testing.T, seed map[string]string, contract, prompt string,
	plan func(i int) map[string]interface{}, opt editLoopOptions) *editLoop {
	t.Helper()
	requirePython3(t)
	r := &editLoop{dir: t.TempDir(), census: map[string]int{}, terminal: map[string]string{}}
	for n, b := range seed {
		p := filepath.Join(r.dir, n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var cancel context.CancelFunc
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/internal/lens/score-per-step" && opt.lensOff:
			json.NewEncoder(w).Encode(map[string]interface{}{"enabled": false})
			return
		case req.URL.Path == "/v3/generate" && opt.v3LensUnavailable != "":
			res, _ := json.Marshal(map[string]interface{}{
				"code": "", "passed": false, "phase_solved": "lens_unavailable",
				"lens_unavailable": opt.v3LensUnavailable})
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: result\ndata: %s\n\ndata: [DONE]\n\n", res)
			return
		case req.URL.Path == "/v3/generate":
			if opt.v3Winner == "" {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			serveV3Winner(w, opt.v3Winner)
			return
		case req.URL.Path == "/internal/structural_edit":
			serveStructuralEdit(w, req)
			return
		case req.URL.Path == "/internal/structural_check":
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "unresolved": []string{}})
			return
		case req.URL.Path == "/internal/cyclomatic_complexity":
			json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
			return
		case strings.HasPrefix(req.URL.Path, "/v3/"), strings.HasPrefix(req.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(req.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(req.Body).Decode(&in)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"valid": !strings.Contains(in.Code, "def broken(:")})
			return
		case !strings.HasSuffix(req.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, req)
			return
		}
		body, _ := io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		r.mu.Lock()
		i := r.turns
		r.turns++
		r.prompts = append(r.prompts, string(body))
		r.mu.Unlock()
		if i >= 20 {
			http.Error(w, "ceiling", http.StatusInsufficientStorage)
			return
		}
		step := plan(i)
		if opt.cancelBeforeTool != "" && step["name"] == opt.cancelBeforeTool && cancel != nil {
			cancel()
		}
		call, _ := json.Marshal(step)
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(call)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(srv.Close)
	r.ctx = NewAgentContext(r.dir, Tier2Medium)
	r.ctx.InferenceURL, r.ctx.SandboxURL, r.ctx.V3URL = srv.URL, srv.URL, srv.URL
	if opt.lensOff {
		r.ctx.LensURL = srv.URL
	}
	r.ctx.PermissionMode = PermissionYolo
	r.ctx.TrustMode = trustFullyTrusted
	r.ctx.VerifyOnHost = true
	r.ctx.MaxTurns = 0
	// Every production request carries an id; the automatic route's identity
	// checks refuse a candidate whose binding has none.
	base := context.WithValue(context.Background(), requestIDKey, "req-edit-loop")
	if opt.cancelBeforeTool != "" {
		var c context.Context
		c, cancel = context.WithCancel(base)
		base = c
		t.Cleanup(cancel)
	}
	r.ctx.Ctx = base
	if contract != "" {
		r.ctx.TaskContract = mustContract(t, r.dir, contract)
	}
	r.ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.census[et]++
		switch et {
		case "tool_call":
			var tc struct{ Name string }
			json.Unmarshal(b, &tc)
			r.seq = append(r.seq, tc.Name)
		case "tool_result":
			var m map[string]interface{}
			json.Unmarshal(b, &m)
			r.results = append(r.results, m)
		case "done":
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				r.terminal[k] = v
			}
		}
	}
	runAgentLoop(r.ctx, prompt)
	return r
}

// serveV3Winner answers /v3/generate with an authorized winner and the
// evidence envelope the proxy requires to identify the selection.
func serveV3Winner(w http.ResponseWriter, winner string) {
	h := contentSHA256(winner)
	body, _ := json.Marshal(map[string]interface{}{
		"code": winner, "passed": true, "phase_solved": "phase_one",
		"candidates_tested": 3, "winning_score": 0.9,
		"evidence": map[string]interface{}{
			"wire_version": "1.0.0", "record_schema_version": "1.1.0",
			"identity": map[string]interface{}{
				"contract_id": "c.v1", "contract_version": "1",
				"adapter_id": "python_compile", "adapter_version": "0.1.0-prototype",
				"artifact_scope": "mod.py", "evaluation_context_hash": "ctx",
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
	})
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	for _, line := range []string{"event: result", "data: " + string(body), "", "data: [DONE]", ""} {
		fmt.Fprint(w, line+"\n")
		if fl != nil {
			fl.Flush()
		}
	}
}

// serveStructuralEdit is a stateless stand-in for v3-service's
// /internal/structural_edit: it replaces the top-level `def NAME` block named
// by a `function:NAME` selector with the supplied content.
func serveStructuralEdit(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Path, Source, Selector, Content string
	}
	json.NewDecoder(req.Body).Decode(&in)
	name := strings.TrimPrefix(in.Selector, "function:")
	lines := strings.Split(in.Source, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "def "+name+"(") {
			start = i
			break
		}
	}
	if start < 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false,
			"error": "selector not found: " + in.Selector})
		return
	}
	end := len(lines)
	for j := start + 1; j < len(lines); j++ {
		l := lines[j]
		if l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			end = j
			break
		}
	}
	// Keep exactly the blank lines that separated the old block from what
	// follows, so the edit stays inside the node it named.
	oldBlock := strings.Join(lines[start:end], "\n")
	trailing := 0
	for k := end - 1; k > start && lines[k] == ""; k-- {
		trailing++
	}
	newBlock := strings.TrimRight(in.Content, "\n") + strings.Repeat("\n", trailing)
	out := strings.Join(lines[:start], "\n") + "\n" + newBlock
	if end < len(lines) {
		out += "\n" + strings.Join(lines[end:], "\n")
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "language": "python", "new_content": out,
		"old_size": len(oldBlock), "new_size": len(newBlock),
	})
}

func (r *editLoop) disk(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tracked reports whether the ledger holds a current, untombstoned entry for
// the path: the canonical fact that this session changed it.
func (r *editLoop) tracked(rel string) bool {
	r.ctx.LedgerMu.Lock()
	defer r.ctx.LedgerMu.Unlock()
	d := r.ctx.Ledger[ledgerKey(r.ctx, rel)]
	return d != nil && d.Generation > 0 && !d.Tombstoned
}

func (r *editLoop) ledgerHash(rel string) string {
	r.ctx.LedgerMu.Lock()
	defer r.ctx.LedgerMu.Unlock()
	if d := r.ctx.Ledger[ledgerKey(r.ctx, rel)]; d != nil {
		return d.CurrentHash
	}
	return ""
}

func (r *editLoop) lastResultSuccess(tool string) (bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.results) - 1; i >= 0; i-- {
		if r.results[i]["tool"] == tool {
			ok, _ := r.results[i]["success"].(bool)
			return ok, true
		}
	}
	return false, false
}

func (r *editLoop) describe() string {
	return fmt.Sprintf("seq=%v status=%q reason=%q coverable=%v",
		r.seq, r.terminal["status"], r.terminal["reason"], changedPathsForCoverage(r.ctx))
}

// --- scripted steps ------------------------------------------------------------

func stepRead(p string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "read_file",
		"args": map[string]string{"path": p}}
}

func stepEdit(p, old, new string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "edit_file",
		"args": map[string]string{"path": p, "old_str": old, "new_str": new}}
}

func stepStructural(p, selector, content string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "structural_edit",
		"args": map[string]string{"path": p, "selector": selector, "content": content}}
}

func stepInsertAfter(p string, line int, content string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "insert_after",
		"args": map[string]interface{}{"path": p, "line": line, "content": content}}
}

func stepReplaceLines(p string, start, end int, first, last, content string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "replace_lines",
		"args": map[string]interface{}{"path": p, "start_line": start, "end_line": end,
			"expected_first_line": first, "expected_last_line": last, "content": content}}
}

func stepRun(cmd string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "run_command",
		"args": map[string]string{"command": cmd}}
}

func stepWrite(p, c string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "write_file",
		"args": map[string]string{"path": p, "content": c}}
}

func stepDone(summary string) map[string]interface{} {
	return map[string]interface{}{"type": "done", "summary": summary}
}

func script(steps ...map[string]interface{}) func(int) map[string]interface{} {
	return func(i int) map[string]interface{} {
		if i < len(steps) {
			return steps[i]
		}
		return steps[len(steps)-1]
	}
}

// lineOf returns the 1-based line number of the first line equal to want.
func lineOf(t *testing.T, body, want string) int {
	t.Helper()
	for i, l := range strings.Split(body, "\n") {
		if l == want {
			return i + 1
		}
	}
	t.Fatalf("line %q not in seed", want)
	return 0
}

// --- the edit that changes bytes ---------------------------------------------

// editCases are the four edit operations that land changed bytes through the
// shared edit route, each with the change it makes to accountingSeed.
type editCase struct {
	tool string
	step func(t *testing.T) map[string]interface{}
	// marker is a substring present on disk after the edit and absent before.
	marker string
	// winner is the V3 selection for this edit: the whole module, differing
	// from the tool's own result only inside the edited region, so the route's
	// beyond-the-edit gate admits it. winnerMarker is present only in it.
	winner, winnerMarker string
	// ownBytesTerminal is the terminal a run reaches after this tool lands
	// its OWN bytes and the model runs the file. edit_file, insert_after and
	// replace_lines observe the syntax of the bytes they land, so the ledger
	// holds a passing validation. structural_edit lands bytes with validation
	// deliberately not run -- its result must not read the service's ok as a
	// syntax pass, and it still does not. What settles its debt is the run
	// the script makes: `python3 mod.py` exiting 0 is recorded as an
	// execution verdict, its own kind, against the bytes that ran. So all four
	// tools complete on the same evidence. Before that, structural_edit ended
	// unresolved_mutation_debt after a passing run, and a live acceptance run
	// spent its last turns on that phantom (2026-09-15); without a real
	// passing run the debt still stands, see TestACleanExecutionSettlesContentDebt.
	ownBytesTerminal [2]string
}

func editCases() []editCase {
	seed := accountingSeed
	return []editCase{
		{"edit_file", func(t *testing.T) map[string]interface{} {
			return stepEdit("mod.py", "def helper():\n    return 1\n", "def helper():\n    return 2\n")
		}, "return 2",
			strings.Replace(seed, "    return 1\n", "    return 9\n", 1), "return 9",
			[2]string{"completed", "deliverables_demonstrated"}},
		{"structural_edit", func(t *testing.T) map[string]interface{} {
			return stepStructural("mod.py", "function:helper", "def helper():\n    return 3\n")
		}, "return 3",
			strings.Replace(seed, "    return 1\n", "    return 9\n", 1), "return 9",
			[2]string{"completed", "deliverables_demonstrated"}},
		{"insert_after", func(t *testing.T) map[string]interface{} {
			return stepInsertAfter("mod.py", lineOf(t, seed, "import sys"), "INSERTED = 4")
		}, "INSERTED = 4",
			strings.Replace(seed, "import sys\n", "import sys\nINSERTED = 44\n", 1), "INSERTED = 44",
			[2]string{"completed", "deliverables_demonstrated"}},
		{"replace_lines", func(t *testing.T) map[string]interface{} {
			n := lineOf(t, seed, "    return 1")
			return stepReplaceLines("mod.py", n, n, "    return 1", "    return 1", "    return 5")
		}, "return 5",
			strings.Replace(seed, "    return 1\n", "    return 55\n", 1), "return 55",
			[2]string{"completed", "deliverables_demonstrated"}},
	}
}

// assertLandedAndAccounted is the whole claim: the edit landed, the ledger
// describes the current bytes, the path is a session write, and the bound
// verification the model ran let the work contract complete.
func assertLandedAndAccounted(t *testing.T, r *editLoop, c editCase) {
	t.Helper()
	disk := r.disk(t, "mod.py")
	if !strings.Contains(disk, c.marker) {
		t.Fatalf("%s: the edit did not land: %s", c.tool, r.describe())
	}
	if ok, seen := r.lastResultSuccess(c.tool); !seen || !ok {
		t.Fatalf("%s: the tool did not report success: %s", c.tool, r.describe())
	}
	if h := r.ledgerHash("mod.py"); h != hashBytes([]byte(disk)) {
		t.Fatalf("%s: ledger current hash %q is not the disk bytes", c.tool, h)
	}
	if !r.tracked("mod.py") {
		t.Errorf("%s: the ledger does not track the changed path: %s", c.tool, r.describe())
	}
	// The verification demand is met for every tool: a run over the current
	// bytes covered the registered path. What the run ends as afterwards is
	// decided by the ledger's own validation contract, unchanged here.
	if r.terminal["reason"] == "verification_demanded_unmet" {
		t.Errorf("%s: the bound verification did not cover the changed path: %s", c.tool, r.describe())
	}
	if r.terminal["status"] != c.ownBytesTerminal[0] || r.terminal["reason"] != c.ownBytesTerminal[1] {
		t.Errorf("%s: terminal %q/%q, want %q/%q: %s", c.tool, r.terminal["status"],
			r.terminal["reason"], c.ownBytesTerminal[0], c.ownBytesTerminal[1], r.describe())
	}
}

// RED before the fix: edit_file and structural_edit land changed bytes and the
// run ends verification_demanded_unmet even though `python3 mod.py` ran and
// passed; insert_after and replace_lines only passed because they also wrote
// the raw-path session map themselves.
func TestEveryEditToolThatLandsBytesRegistersTheChangedPath(t *testing.T) {
	for _, c := range editCases() {
		c := c
		t.Run(c.tool, func(t *testing.T) {
			r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiStrictWork,
				"Change helper in mod.py and check it still runs.",
				script(stepRead("mod.py"), c.step(t), stepRun("python3 mod.py"), stepDone("edited and ran mod.py")),
				editLoopOptions{})
			assertLandedAndAccounted(t, r, c)
		})
	}
}

// The tier bypass path lands the tool's own bytes without entering the
// pipeline; the accounting must not depend on which path landed them.
func TestATierBypassedEditRegistersTheChangedPath(t *testing.T) {
	c := editCase{"edit_file", func(t *testing.T) map[string]interface{} {
		return stepEdit("mod.py", "    return 1\n", "    return 2\n")
	}, "return 2", "", "", [2]string{"completed", "deliverables_demonstrated"}}
	r := editLoopFixture(t, map[string]string{"mod.py": accountingSeedSmall}, tuiStrictWork,
		"Change helper in mod.py and check it still runs.",
		script(stepRead("mod.py"), c.step(t), stepRun("python3 mod.py"), stepDone("edited and ran mod.py")),
		editLoopOptions{})
	assertLandedAndAccounted(t, r, c)
}

// RED before the fix: under automatic_v3 the selected candidate lands through
// the shared edit route on every edit tool, the ledger describes the landed
// bytes, and the run still ends verification_demanded_unmet after a passing
// bound run, because coverage never read the ledger.
func TestADeliveredEditCandidateRegistersTheChangedPath(t *testing.T) {
	for _, c := range editCases() {
		c := c
		t.Run(c.tool, func(t *testing.T) {
			r := editLoopFixture(t, map[string]string{"mod.py": accountingSeed}, tuiAutomaticContract,
				"Change helper in mod.py and check it still runs.",
				script(stepRead("mod.py"), c.step(t), stepRun("python3 mod.py"), stepDone("edited and ran mod.py")),
				editLoopOptions{v3Winner: c.winner})
			disk := r.disk(t, "mod.py")
			if disk != c.winner {
				t.Fatalf("%s: the selected candidate did not land (disk has winner marker: %v): %s",
					c.tool, strings.Contains(disk, c.winnerMarker), r.describe())
			}
			if h := r.ledgerHash("mod.py"); h != hashBytes([]byte(disk)) {
				t.Fatalf("%s: ledger current hash %q is not the landed bytes", c.tool, h)
			}
			if deliverySettlementFor(r.ctx, filepath.Join(r.dir, "mod.py")) == nil {
				t.Fatalf("%s: no settlement record for the landed candidate", c.tool)
			}
			if !r.tracked("mod.py") {
				t.Errorf("%s: the ledger does not track the landed path: %s", c.tool, r.describe())
			}
			if r.terminal["status"] != "completed" {
				t.Errorf("%s: terminal %q/%q, want completed after a passing bound verification: %s",
					c.tool, r.terminal["status"], r.terminal["reason"], r.describe())
			}
		})
	}
}
