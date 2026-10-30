package main

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
	"time"
)

// --- An executed write is identified by the bytes it proposes (C4) ----------
//
// `content:"@fenced"` is the same seven bytes each time the model sends it,
// while the sub-call that resolves it returns different bytes each time. Each
// new proposal must reach the syntax gate, not be dropped as a resend of the
// same intent. So the identity of an executed write_file is the resolved
// proposal. The fenced allowance and the repeat detector still read the raw
// intent (TestChannelIdentityStaysRaw).

const (
	c4Valid = "def solve():\n    total = 0\n    for i in range(3):\n        total += i\n    return total\n\n\nprint(solve())\n"
	c4BadA  = "def solve():\n    total = 0\n    for i in range(3:\n        total += i\n    return total\n\n\nprint(solve())\n"
	c4BadB  = "def solve():\n    vals = [1, 2]]\n    return sum(vals)\n\n\nprint(solve())\n"
	c4BadC  = "def solve():\n    d = {'k': 1\n    return d\n\n\nprint(solve())\n"
	c4Fixed = "def solve():\n    return 7\n\n\nprint(solve())\n"
)

func c4Syntax(code string) string {
	cmd := exec.Command("python3", "-c",
		"import ast,sys\ntry:\n ast.parse(sys.stdin.read())\nexcept SyntaxError as e:\n sys.stdout.write('%s (line %d)' % (e.msg, e.lineno or 0))")
	cmd.Stdin = strings.NewReader(code)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// syntaxCheck is one call the write path made to the checker, identified by
// the exact bytes it asked about. Proposal checks and baseline checks both
// arrive here, and the hash is what tells them apart.
type syntaxCheck struct {
	Hash   string
	Valid  bool
	Detail string
}

type c4Run struct {
	ctx        *AgentContext
	dir        string
	turns      int
	fetches    int
	census     map[string]int
	terminal   map[string]string
	refusals   []string // syntax-gate diagnostics, in order
	gates      []string
	calls      []string
	writes     int // successful write_file results
	checks     []syntaxCheck
	recoveries []string

	// What the stub server answers with: fencedBodies feeds the @fenced
	// sub-call in order, and plan is the scripted model. mu guards the run's
	// fields against the server and the event stream.
	fencedBodies []string
	plan         func(i int, r *c4Run) map[string]interface{}
	mu           sync.Mutex
}

// c4Fixture runs the real loop with a genuine Python syntax check. fencedBodies
// feeds the @fenced sub-call in order.
func c4Fixture(t *testing.T, fencedBodies []string,
	plan func(i int, r *c4Run) map[string]interface{}) *c4Run {
	t.Helper()
	r := &c4Run{dir: t.TempDir(), census: map[string]int{}, terminal: map[string]string{},
		fencedBodies: fencedBodies, plan: plan}
	srv := httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(srv.Close)

	r.ctx = NewAgentContext(r.dir, Tier2Medium)
	r.ctx.InferenceURL, r.ctx.SandboxURL, r.ctx.V3URL = srv.URL, srv.URL, srv.URL
	r.ctx.PermissionMode = PermissionYolo
	r.ctx.TrustMode = trustFullyTrusted
	r.ctx.VerifyOnHost = true
	r.ctx.MaxTurns = 0
	r.ctx.StreamFn = r.record
	runAgentLoop(r.ctx, "Write solve.py so it prints 7, then run it.")
	return r
}

// serve is the stub server of c4Fixture: V3 and internal services are down,
// the syntax check is a real Python parse, the sandbox runs nothing, the
// @fenced sub-call returns the next of fencedBodies, and every other model
// turn is the next call of the scripted plan.
func (r *c4Run) serve(w http.ResponseWriter, req *http.Request) {
	switch {
	case strings.HasPrefix(req.URL.Path, "/v3/"), strings.HasPrefix(req.URL.Path, "/internal/"):
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	case strings.HasSuffix(req.URL.Path, "/syntax-check"):
		var in struct{ Code string }
		json.NewDecoder(req.Body).Decode(&in)
		d := c4Syntax(in.Code)
		r.mu.Lock()
		r.checks = append(r.checks, syntaxCheck{
			Hash: hashBytes([]byte(in.Code))[:12], Valid: d == "", Detail: d})
		r.mu.Unlock()
		if d != "" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"valid": false, "errors": []string{d}})
			return
		}
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
	raw, _ := io.ReadAll(req.Body)
	w.Header().Set("Content-Type", "text/event-stream")
	if strings.Contains(string(raw), "single fenced block") {
		r.mu.Lock()
		n := r.fetches
		r.fetches++
		r.mu.Unlock()
		body := "no block"
		if n < len(r.fencedBodies) {
			body = "```python\n" + r.fencedBodies[n] + "```"
		}
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{"content": body}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
		return
	}
	r.mu.Lock()
	i := r.turns
	r.turns++
	r.mu.Unlock()
	if i >= 30 {
		http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
		return
	}
	r.mu.Lock()
	call, _ := json.Marshal(r.plan(i, r))
	r.mu.Unlock()
	d, _ := json.Marshal(map[string]interface{}{
		"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(call)}}}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
}

// record is the event stream of c4Fixture: it counts events by type and keeps
// what the tests read from them.
func (r *c4Run) record(et string, data interface{}) {
	b, _ := json.Marshal(data)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.census[et]++
	switch et {
	case "tool_result":
		var ok struct {
			Success bool
			Tool    string
		}
		if json.Unmarshal(b, &ok) == nil && ok.Success && ok.Tool == "write_file" {
			r.writes++
		}
		var tr struct{ Error string }
		if json.Unmarshal(b, &tr) == nil && strings.Contains(tr.Error, c4Marker) {
			r.recoveries = append(r.recoveries, tr.Error)
		}
		// Only a real syntax-gate evaluation counts. The resend ban
		// quotes the original refusal back, so it carries the same
		// phrase without any proposal having been looked at.
		if json.Unmarshal(b, &tr) == nil && strings.Contains(tr.Error, "it was NOT written") &&
			!strings.Contains(tr.Error, "byte for byte") &&
			!strings.Contains(tr.Error, "no longer available") {
			r.refusals = append(r.refusals, tr.Error)
		}
	case "gate":
		var g struct{ Gate, Reason string }
		if json.Unmarshal(b, &g) == nil {
			r.gates = append(r.gates, g.Gate+": "+g.Reason)
		}
	case "done":
		var m map[string]string
		json.Unmarshal(b, &m)
		for k, v := range m {
			r.terminal[k] = v
		}
	case "tool_call":
		r.calls = append(r.calls, string(b))
	}
}

// subject names the bytes a check was about, so a proposal check can be told
// from the baseline check the write path makes of what is already on disk.
func (r *c4Run) subject(hash string) string {
	for name, body := range map[string]string{
		"valid(known-good)": c4Valid, "BadA": c4BadA, "BadB": c4BadB,
		"BadC": c4BadC, "Fixed": c4Fixed,
	} {
		if hashBytes([]byte(body))[:12] == hash {
			return name
		}
	}
	return "other"
}

func (r *c4Run) disk(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(r.dir, "solve.py"))
	return string(b)
}

func (r *c4Run) log(t *testing.T, name string) {
	t.Helper()
	d := r.ctx.Ledger[ledgerKey(r.ctx, "solve.py")]
	kind, status := ValidationKindUnknown, ValidationUnknown
	hashOK := false
	if d != nil {
		kind, status = d.CurrentValidation()
		hashOK = d.CurrentHash == hashBytes([]byte(r.disk(t)))
	}
	pass, fail := 0, 0
	for _, c := range r.checks {
		if c.Valid {
			pass++
		} else {
			fail++
		}
	}
	t.Logf("%s: turns=%d fetches=%d checks=%d (pass=%d fail=%d) refusals=%d recoveries=%d status=%q reason=%q hash_matches=%v validation=%s/%s",
		name, r.turns, r.fetches, len(r.checks), pass, fail, len(r.refusals), len(r.recoveries),
		r.terminal["status"], r.terminal["reason"], hashOK, kind, status)
	for i, c := range r.checks {
		t.Logf("   check#%d %s %-5v %s [%s]", i+1, c.Hash, c.Valid, c.Detail, r.subject(c.Hash))
	}
}

func c4Write(path, content string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "write_file",
		"args": map[string]string{"path": path, "content": content}}
}

// The raw intent never changes and the resolved proposals all differ: every
// one of them must reach the syntax gate.
func TestFencedProposalsAreEvaluatedNotDeduplicatedByIntent(t *testing.T) {
	r := c4Fixture(t, []string{c4BadA, c4BadB, c4BadC, c4Fixed},
		func(i int, r *c4Run) map[string]interface{} {
			switch {
			case i == 0:
				return c4Write("solve.py", c4Valid)
			case r.writes < 2: // still trying to replace the known-good file
				return c4Write("solve.py", "@fenced")
			}
			return map[string]interface{}{"type": "done", "summary": "solve.py prints 7"}
		})
	r.log(t, "B")

	// Accounting, stated exactly. `refusals` counts REJECTED proposals only;
	// the checker sees more than that, and the distinction is the point.
	seen := map[string]bool{}
	var proposalFail, proposalPass int
	for _, c := range r.checks {
		if r.subject(c.Hash) == "valid(known-good)" {
			continue // the baseline the write path compares against
		}
		seen[c.Hash] = true
		if c.Valid {
			proposalPass++
		} else {
			proposalFail++
		}
	}
	if len(seen) != 4 {
		t.Errorf("%d distinct proposals reached the checker, want 4 (BadA, BadB, BadC, Fixed); "+
			"the rest were fetched and discarded on raw-intent identity", len(seen))
	}
	if proposalFail != 3 {
		t.Errorf("%d failed proposal checks, want 3", proposalFail)
	}
	if proposalPass == 0 {
		t.Error("the valid proposal was never checked")
	}
	if len(r.refusals) != 3 {
		t.Fatalf("%d rejected proposals, want 3", len(r.refusals))
	}
	// The valid proposal was checked BEFORE it was written: its passing check
	// precedes the successful write, and no write succeeded between the last
	// failing check and it.
	lastFail, firstPass := -1, -1
	for i, c := range r.checks {
		if r.subject(c.Hash) == "valid(known-good)" {
			continue
		}
		if !c.Valid {
			lastFail = i
		} else if firstPass < 0 {
			firstPass = i
		}
	}
	// The checks below read the passing check, so without one there is nothing
	// more to compare.
	if firstPass < 0 {
		t.Fatalf("no proposal passed its check (%d checks, last failing %d)", len(r.checks), lastFail)
	}
	if firstPass <= lastFail {
		t.Errorf("the accepted proposal was not checked after the last rejection "+
			"(lastFail=%d firstPass=%d)", lastFail, firstPass)
	}
	if hashBytes([]byte(r.disk(t)))[:12] != r.checks[firstPass].Hash {
		t.Error("the bytes on disk are not the bytes that passed the check")
	}
	// Valid bytes on disk with more than one replacement refused against them
	// is also the refused-replacement shape, so that recovery may fire here
	// too. The run does not depend on it (the model advances on r.writes),
	// and it fires at most once.
	if n := len(r.recoveries); n > 1 {
		t.Errorf("%d recoveries in one generation, want at most one", n)
	}
	// Each evaluation describes its OWN bytes, as the checker reported them.
	for i, body := range []string{c4BadA, c4BadB, c4BadC} {
		want := c4Syntax(body)
		if want == "" {
			t.Fatalf("fixture body %d is not actually invalid", i+1)
		}
		if i < len(r.refusals) && !strings.Contains(r.refusals[i], want) {
			t.Errorf("evaluation %d does not describe its own proposal (want %q): %.140s",
				i+1, want, r.refusals[i])
		}
	}
	// No rejected proposal ever reached disk: what is there is either the
	// original known-good bytes or the valid correction.
	got := r.disk(t)
	if got != c4Valid && got != c4Fixed {
		t.Errorf("disk holds neither the known-good bytes nor the valid correction: %q", got)
	}
	if c4Syntax(got) != "" {
		t.Errorf("invalid bytes reached disk: %q", got)
	}
}

// Identity is the proposal: the same file bytes sent inline and through the
// fenced channel are one call; different bytes are not.
func TestProposalIdentityIsTheResolvedBytes(t *testing.T) {
	inline := json.RawMessage(`{"path":"solve.py","content":` + c4JSON(c4BadA) + `}`)
	resolved := json.RawMessage(`{"path":"solve.py","content":` + c4JSON(c4BadA) + `}`)
	alias := json.RawMessage(`{"path":"./solve.py","content":` + c4JSON(c4BadA) + `}`)
	other := json.RawMessage(`{"path":"solve.py","content":` + c4JSON(c4BadB) + `}`)
	bare := json.RawMessage(`{"path":"solve.py","content":"@fenced"}`)

	same := retryIdentityArgs("write_file", bare, resolved)
	if toolCallSignature("write_file", same) != toolCallSignature("write_file", inline) {
		t.Error("the same bytes inline and through the channel are not one call")
	}
	if toolCallSignature("write_file", same) != toolCallSignature("write_file", alias) {
		t.Error("path aliases do not share proposal identity")
	}
	if toolCallSignature("write_file", same) == toolCallSignature("write_file", other) {
		t.Error("materially different bytes share identity")
	}
	// Channel identity is still the raw intent, and other tools are untouched.
	if got := retryIdentityArgs("write_file", bare, nil); string(got) != string(bare) {
		t.Errorf("with nothing resolved the identity must stay the intent: %s", got)
	}
	ed := json.RawMessage(`{"path":"solve.py","old_str":"a","new_str":"b"}`)
	if got := retryIdentityArgs("edit_file", ed, resolved); string(got) != string(ed) {
		t.Errorf("edit_file identity moved: %s", got)
	}
}

func c4JSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The two bounds that must survive the identity change, each owned by a
// different mechanism: identical resolved bytes hit the resend ban, and
// endlessly changing invalid proposals hit the failure ceiling.
func TestIdenticalInvalidProposalsHitTheResendBan(t *testing.T) {
	r := c4Fixture(t, nil, func(i int, _ *c4Run) map[string]interface{} {
		if i == 0 {
			return c4Write("solve.py", c4Valid)
		}
		return c4Write("solve.py", c4BadA)
	})
	r.log(t, "A")
	if r.turns >= 30 {
		t.Fatalf("%d turns without a terminal", r.turns)
	}
	if r.terminal["reason"] != "repeated_refusal" {
		t.Errorf("reason=%q, want repeated_refusal", r.terminal["reason"])
	}
	if len(r.refusals) != 1 {
		t.Errorf("%d syntax evaluations of one unchanging proposal, want 1", len(r.refusals))
	}
	if r.disk(t) != c4Valid {
		t.Errorf("the known-good bytes did not survive: %q", r.disk(t))
	}
}

func TestEverChangingInvalidProposalsHitTheFailureCeiling(t *testing.T) {
	bad := []string{c4BadA, c4BadB, c4BadC}
	r := c4Fixture(t, nil, func(i int, _ *c4Run) map[string]interface{} {
		if i == 0 {
			return c4Write("solve.py", c4Valid)
		}
		// Never repeats a proposal: a fresh trailing comment each time.
		return c4Write("solve.py",
			bad[(i-1)%len(bad)]+fmt.Sprintf("# attempt %d\n", i))
	})
	r.log(t, "B2")
	if r.turns >= 30 {
		t.Fatalf("%d turns without a terminal", r.turns)
	}
	if r.terminal["reason"] != "failure_ceiling" {
		t.Errorf("reason=%q, want failure_ceiling — the bound for proposals that never repeat",
			r.terminal["reason"])
	}
	if NormalizeTerminalStatus(r.terminal["status"]).Completed() {
		t.Errorf("a run that never landed a valid replacement reported %q", r.terminal["status"])
	}
	if r.disk(t) != c4Valid {
		t.Errorf("the known-good bytes did not survive: %q", r.disk(t))
	}
}

// Neither bound gets in the way of a model that corrects itself.
func TestAnInvalidThenValidProposalStillCompletes(t *testing.T) {
	r := c4Fixture(t, nil, func(i int, _ *c4Run) map[string]interface{} {
		switch i {
		case 0:
			return c4Write("solve.py", c4Valid)
		case 1:
			return c4Write("solve.py", c4BadA)
		case 2:
			return c4Write("solve.py", c4Fixed)
		case 3:
			return map[string]interface{}{"type": "tool_call", "name": "run_command",
				"args": map[string]string{"command": "python3 solve.py"}}
		}
		return map[string]interface{}{"type": "done", "summary": "solve.py prints 7"}
	})
	r.log(t, "C")
	if r.terminal["status"] != string(TerminalCompleted) {
		t.Errorf("terminal %q/%q, want completed",
			r.terminal["status"], r.terminal["reason"])
	}
	if r.disk(t) != c4Fixed {
		t.Errorf("the correction did not land: %q", r.disk(t))
	}
}

// --- Recovery for refused replacements of known-good bytes ------------------
//
// The scripted models below correct themselves only after the recovery text
// arrives. Without the recovery they never do, so a completed run shows that
// the recovery worked, not that turns passed.

const c4Marker = "The working version is still there"

// c4RecoveryPlan repeats invalid replacements until it sees the recovery (if
// it reads it at all), then sends one valid correction and runs it. bodies is
// cycled; a single entry sends the identical proposal each time.
func c4RecoveryPlan(readsRecovery bool, bodies []string) func(int, *c4Run) map[string]interface{} {
	corrected, verified := false, false
	return func(i int, r *c4Run) map[string]interface{} {
		// r.recoveries holds only results that carry c4Marker.
		sawRecovery := len(r.recoveries) > 0
		switch {
		case i == 0:
			return c4Write("solve.py", c4Valid)
		case readsRecovery && sawRecovery && !corrected:
			corrected = true
			return c4Write("solve.py", c4Fixed)
		case corrected && !verified:
			verified = true
			return map[string]interface{}{"type": "tool_call", "name": "run_command",
				"args": map[string]string{"command": "python3 solve.py"}}
		case corrected:
			return map[string]interface{}{"type": "done", "summary": "solve.py prints 7"}
		default:
			return c4Write("solve.py", bodies[(i-1)%len(bodies)])
		}
	}
}

// c4RefusedReplacements are the two ways the scripted model of
// c4RecoveryPlan keeps sending a replacement that the syntax gate refuses.
var c4RefusedReplacements = []struct {
	name   string
	bodies []string
}{
	{"the same invalid proposal, repeated", []string{c4BadA}},
	{"two distinct invalid proposals", []string{c4BadA, c4BadB}},
}

// The recovery is offered once, quotes the diagnostic of the proposal that
// triggered it, and the model that reads it completes the run.
func TestRefusedReplacementRecovery(t *testing.T) {
	for _, c := range c4RefusedReplacements {
		t.Run(c.name, func(t *testing.T) {
			r := c4Fixture(t, nil, c4RecoveryPlan(true, c.bodies))
			r.log(t, c.name)

			if n := len(r.recoveries); n != 1 {
				t.Fatalf("%d recoveries, want exactly one", n)
			}
			// The recovery quotes the diagnostic for the bytes just refused.
			last := c4Syntax(c.bodies[len(c.bodies)-1])
			if !strings.Contains(r.recoveries[0], last) {
				t.Errorf("the recovery does not carry the diagnostic for the "+
					"proposal that triggered it (want %q):\n%.400s", last, r.recoveries[0])
			}
			if r.terminal["status"] != string(TerminalCompleted) ||
				r.terminal["reason"] != "deliverables_demonstrated" {
				t.Fatalf("terminal %q/%q, want completed/deliverables_demonstrated",
					r.terminal["status"], r.terminal["reason"])
			}
		})
	}
}

// The correction the model sends after the recovery lands, and the ledger
// holds a current syntax pass for exactly those bytes.
func TestRefusedReplacementCorrectionLandsValidated(t *testing.T) {
	for _, c := range c4RefusedReplacements {
		t.Run(c.name, func(t *testing.T) {
			r := c4Fixture(t, nil, c4RecoveryPlan(true, c.bodies))
			r.log(t, c.name)

			if r.disk(t) != c4Fixed {
				t.Errorf("the correction did not land: %q", r.disk(t))
			}
			d := r.ctx.Ledger[ledgerKey(r.ctx, "solve.py")]
			if d == nil || d.CurrentHash != hashBytes([]byte(r.disk(t))) {
				t.Fatal("the ledger does not describe the bytes on disk")
			}
			if kind, status := d.CurrentValidation(); status != ValidationPassed ||
				kind != ValidationKindSyntax {
				t.Errorf("validation on the current hash is %s/%s, want syntax/passed", kind, status)
			}
		})
	}
}

// The same fixture, with a model that never reads the recovery: it never
// corrects itself, so the run ends without a completion and the known-good
// bytes stay.
func TestRefusedReplacementWithoutTheRecoveryNeverCompletes(t *testing.T) {
	for _, c := range c4RefusedReplacements {
		t.Run(c.name, func(t *testing.T) {
			r := c4Fixture(t, nil, c4RecoveryPlan(false, c.bodies))
			r.log(t, c.name+"/ignored")
			if r.turns >= 30 {
				t.Fatalf("%d turns without a terminal", r.turns)
			}
			if NormalizeTerminalStatus(r.terminal["status"]).Completed() {
				t.Errorf("a run that never landed a valid replacement reported %q",
					r.terminal["status"])
			}
			if r.disk(t) != c4Valid {
				t.Errorf("the known-good bytes did not survive: %q", r.disk(t))
			}
		})
	}
}

// Whether the model reads the recovery or not, the run ends with one terminal
// event, and every call has its result.
func TestRefusedReplacementRunEndsOnceWithEveryCallAnswered(t *testing.T) {
	for _, c := range c4RefusedReplacements {
		for _, readsRecovery := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, readsRecovery=%v", c.name, readsRecovery), func(t *testing.T) {
				r := c4Fixture(t, nil, c4RecoveryPlan(readsRecovery, c.bodies))
				if r.census["done"] != 1 {
					t.Errorf("%d terminal events", r.census["done"])
				}
				if r.census["tool_call"] != r.census["tool_result"] {
					t.Errorf("call/result balance: %d vs %d",
						r.census["tool_call"], r.census["tool_result"])
				}
			})
		}
	}
}

// c4State builds the one state C4 reads: a session-written file on disk whose
// exact current bytes are syntax/passed, plus one refused proposal recorded
// against them. Each case then breaks exactly one clause.
func c4State(t *testing.T, mutate func(*AgentContext, *DeliverableState, string)) (
	*AgentContext, *runState, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "solve.py")
	os.WriteFile(path, []byte(c4Valid), 0o644)
	ctx := NewAgentContext(dir, Tier2Medium)
	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*DeliverableState{}
	}
	h := hashBytes([]byte(c4Valid))
	d := &DeliverableState{
		Path: "solve.py", CurrentHash: h, CurrentSize: len(c4Valid), Generation: 1,
		ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
		ValidatedHash: h,
	}
	ctx.Ledger[ledgerKey(ctx, "solve.py")] = d
	if mutate != nil {
		mutate(ctx, d, path)
	}
	return ctx, &runState{}, hashBytes([]byte(c4BadA))
}

func c4Refusal(args string) (json.RawMessage, *ToolResult) {
	return json.RawMessage(args), &ToolResult{
		Success:          false,
		MutationStatus:   MutationRefused,
		ValidationKind:   ValidationKindSyntax,
		ValidationStatus: ValidationFailed,
		ValidationDetail: "invalid syntax (line 3)",
	}
}

// Eligibility is a conjunction. Each case removes one clause and must record
// nothing and recover nothing.
func TestRefusedReplacementEligibility(t *testing.T) {
	badA := `{"path":"solve.py","content":` + c4JSON(c4BadA) + `}`
	for _, c := range []struct {
		name   string
		mutate func(*AgentContext, *DeliverableState, string)
		result func(*ToolResult)
	}{
		{"surviving validation unknown", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus, d.ValidationKind = ValidationUnknown, ValidationKindUnknown
		}, nil},
		{"surviving validation not_run", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus = ValidationNotRun
		}, nil},
		{"surviving validation not_applicable", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus = ValidationNotApplicable
		}, nil},
		{"surviving validation failed", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidationStatus = ValidationFailed
		}, nil},
		{"surviving verdict describes older bytes", func(_ *AgentContext, d *DeliverableState, _ string) {
			d.ValidatedHash = hashBytes([]byte("other"))
		}, nil},
		{"disk moved under the ledger", func(_ *AgentContext, _ *DeliverableState, path string) {
			os.WriteFile(path, []byte(c4Fixed), 0o644)
		}, nil},
		{"path unreadable", func(_ *AgentContext, _ *DeliverableState, path string) {
			os.Remove(path)
		}, nil},
		{"no known-good baseline was ever kept", func(ctx *AgentContext, _ *DeliverableState, _ string) {
			delete(ctx.Ledger, ledgerKey(ctx, "solve.py"))
		}, nil},
		{"the write was not refused", nil, func(r *ToolResult) { r.MutationStatus = MutationApplied }},
		{"the failure was not a syntax failure", nil, func(r *ToolResult) {
			r.ValidationKind = ValidationKindUnknown
		}},
		{"no diagnostic for those bytes", nil, func(r *ToolResult) { r.ValidationDetail = "" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, st, _ := c4State(t, c.mutate)
			args, res := c4Refusal(badA)
			if c.result != nil {
				c.result(res)
			}
			key, sha, n := noteRejectedProposal(ctx, st, "write_file", args, res)
			if key != "" || n != 0 {
				t.Fatalf("recorded evidence on %q: key=%q n=%d", c.name, key, n)
			}
			if msg := rejectedProposalRecovery(ctx, st, "solve.py", key, sha); msg != "" {
				t.Errorf("recovered on %q: %.100s", c.name, msg)
			}
			if len(st.c4Rejected) != 0 {
				t.Errorf("state was written anyway: %v", st.c4Rejected)
			}
		})
	}
}

// The write_file arguments of the refused proposals the state tests below
// record: BadA and BadB for solve.py, and BadA under another spelling of it.
var (
	c4BadAArgs      = `{"path":"solve.py","content":` + c4JSON(c4BadA) + `}`
	c4BadBArgs      = `{"path":"solve.py","content":` + c4JSON(c4BadB) + `}`
	c4BadAAliasArgs = `{"path":"./solve.py","content":` + c4JSON(c4BadA) + `}`
)

// The positive path, and what the state must and must not do, one test each.
func TestRefusedReplacementDiagnosticIsThisProposalsOwn(t *testing.T) {
	ctx, st, _ := c4State(t, nil)
	a, ra := c4Refusal(c4BadAArgs)
	noteRejectedProposal(ctx, st, "write_file", a, ra)
	b, rb := c4Refusal(c4BadBArgs)
	rb.ValidationDetail = "unmatched ']' (line 2)"
	key, shaB, n := noteRejectedProposal(ctx, st, "write_file", b, rb)
	if n != 2 {
		t.Fatalf("distinct proposals = %d, want 2", n)
	}
	msg := rejectedProposalRecovery(ctx, st, "solve.py", key, shaB)
	if !strings.Contains(msg, "unmatched ']' (line 2)") {
		t.Errorf("the recovery does not carry this proposal's diagnostic:\n%s", msg)
	}
	if strings.Contains(msg, "invalid syntax (line 3)") {
		t.Errorf("the recovery carried the OTHER proposal's diagnostic:\n%s", msg)
	}
}

func TestRefusedReplacementUnknownProposalHashGetsNoRecovery(t *testing.T) {
	ctx, st, _ := c4State(t, nil)
	a, ra := c4Refusal(c4BadAArgs)
	key, _, _ := noteRejectedProposal(ctx, st, "write_file", a, ra)
	if msg := rejectedProposalRecovery(ctx, st, "solve.py", key,
		hashBytes([]byte(c4BadC))); msg != "" {
		t.Errorf("bytes that were never refused drew a recovery:\n%.150s", msg)
	}
}

func TestRefusedReplacementAliasesShareOneGenerationAndOneRecovery(t *testing.T) {
	ctx, st, _ := c4State(t, nil)
	a, ra := c4Refusal(c4BadAArgs)
	k1, _, _ := noteRejectedProposal(ctx, st, "write_file", a, ra)
	al, ral := c4Refusal(c4BadAAliasArgs)
	k2, sha, n := noteRejectedProposal(ctx, st, "write_file", al, ral)
	if k1 != k2 {
		t.Errorf("alias spellings built separate generations:\n%q\n%q", k1, k2)
	}
	if n != 1 {
		t.Errorf("the same bytes under another spelling counted as a new proposal (n=%d)", n)
	}
	if rejectedProposalRecovery(ctx, st, "solve.py", k2, sha) == "" {
		t.Fatal("no recovery offered")
	}
	if again := rejectedProposalRecovery(ctx, st, "./solve.py", k2, sha); again != "" {
		t.Errorf("the alias bought a second recovery: got %v", again)
	}
}

func TestRefusedReplacementNewSurvivingBytesAreANewGeneration(t *testing.T) {
	ctx, st, _ := c4State(t, nil)
	a, ra := c4Refusal(c4BadAArgs)
	k1, shaA, _ := noteRejectedProposal(ctx, st, "write_file", a, ra)
	if rejectedProposalRecovery(ctx, st, "solve.py", k1, shaA) == "" {
		t.Fatal("no recovery offered")
	}
	// The model lands something else valid, and a different proposal is
	// refused against the new bytes.
	os.WriteFile(filepath.Join(ctx.WorkingDir, "solve.py"), []byte(c4Fixed), 0o644)
	h := hashBytes([]byte(c4Fixed))
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	d.CurrentHash, d.ValidatedHash = h, h
	b, rb := c4Refusal(c4BadBArgs)
	k2, shaB, n := noteRejectedProposal(ctx, st, "write_file", b, rb)
	// One entry per path, so the key is stable; the generation inside it
	// changes. The old hashes and diagnostics go with the bytes they
	// described.
	if k2 != k1 {
		t.Fatalf("the path's entry moved: %q -> %q", k1, k2)
	}
	ev := st.c4Rejected[k2]
	if ev == nil || ev.diskHash != h {
		t.Fatal("the entry still describes the previous surviving bytes")
	}
	if n != 1 || len(ev.order) != 1 {
		t.Errorf("the new generation started at %d proposals, want 1", n)
	}
	if _, stale := ev.diagnostics[shaA]; stale {
		t.Error("a diagnostic from the previous generation survived")
	}
	if rejectedProposalRecovery(ctx, st, "solve.py", k2, shaB) == "" {
		t.Error("the new generation was denied its own recovery")
	}
}

func TestRefusedReplacementSeparatePathsAreIndependent(t *testing.T) {
	ctx, st, _ := c4State(t, nil)
	other := filepath.Join(ctx.WorkingDir, "helper.py")
	os.WriteFile(other, []byte(c4Valid), 0o644)
	h := hashBytes([]byte(c4Valid))
	ctx.Ledger[ledgerKey(ctx, "helper.py")] = &DeliverableState{
		Path: "helper.py", CurrentHash: h, Generation: 1,
		ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
		ValidatedHash: h,
	}
	a, ra := c4Refusal(c4BadAArgs)
	k1, sha1, _ := noteRejectedProposal(ctx, st, "write_file", a, ra)
	rejectedProposalRecovery(ctx, st, "solve.py", k1, sha1)

	o, ro := c4Refusal(`{"path":"helper.py","content":` + c4JSON(c4BadA) + `}`)
	k2, sha2, n := noteRejectedProposal(ctx, st, "write_file", o, ro)
	if k1 == k2 || n != 1 {
		t.Errorf("helper.py inherited solve.py's generation (n=%d)", n)
	}
	if rejectedProposalRecovery(ctx, st, "helper.py", k2, sha2) == "" {
		t.Error("solve.py's recovery spent helper.py's")
	}
}

func TestRefusedReplacementRunThatIsEndingOrOutOfBudgetRecoversNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*AgentContext)
	}{
		{"cancelled", func(ctx *AgentContext) {
			c, cancel := context.WithCancel(context.Background())
			cancel()
			ctx.Ctx = c
		}},
		{"under the recovery floor", func(ctx *AgentContext) {
			c, cancel := context.WithDeadline(context.Background(),
				time.Now().Add(5*time.Second))
			t.Cleanup(cancel)
			ctx.Ctx = c
		}},
	} {
		ctx, st, _ := c4State(t, nil)
		a, ra := c4Refusal(c4BadAArgs)
		key, sha, _ := noteRejectedProposal(ctx, st, "write_file", a, ra)
		tc.set(ctx)
		if msg := rejectedProposalRecovery(ctx, st, "solve.py", key, sha); msg != "" {
			t.Errorf("%s: recovered anyway: %.100s", tc.name, msg)
		}
	}
}

// The state is bounded: past maxC4Proposals refused proposals for one path,
// it holds no more than the bound, and every retained one keeps its diagnostic.
func TestRefusedReplacementStateBounds(t *testing.T) {
	ctx, st, _ := c4State(t, nil)
	for i := 0; i < maxC4Proposals+4; i++ {
		body := c4BadA + fmt.Sprintf("# %d\n", i)
		a, ra := c4Refusal(`{"path":"solve.py","content":` + c4JSON(body) + `}`)
		noteRejectedProposal(ctx, st, "write_file", a, ra)
	}
	ev := st.c4Rejected[ledgerKey(ctx, "solve.py")]
	if ev == nil {
		t.Fatal("nothing was recorded")
	}
	if len(ev.order) > maxC4Proposals {
		t.Errorf("proposal list is unbounded: %d", len(ev.order))
	}
	// Every retained hash still has its own diagnostic.
	for _, h := range ev.order {
		if ev.diagnostics[h] == "" {
			t.Errorf("proposal %.12s is retained without its diagnostic", h)
		}
	}
}

// A path that lands new bytes again and again starts a new generation each
// time. Its own earlier generations must not fill the ceiling and cost the
// live one its recovery.
func TestRefusedReplacementGenerationsDoNotStarve(t *testing.T) {
	ctx, st, _ := c4State(t, nil)
	path := filepath.Join(ctx.WorkingDir, "solve.py")
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]

	// Nine successive generations of the SAME path: land new valid bytes,
	// have a replacement refused against them, take the recovery.
	for gen := 0; gen < 9; gen++ {
		body := fmt.Sprintf("%s# generation %d\n", c4Valid, gen)
		os.WriteFile(path, []byte(body), 0o644)
		h := hashBytes([]byte(body))
		d.CurrentHash, d.ValidatedHash = h, h

		args, res := c4Refusal(`{"path":"solve.py","content":` +
			c4JSON(c4BadA+fmt.Sprintf("# try %d\n", gen)) + `}`)
		canon, sha, n := noteRejectedProposal(ctx, st, "write_file", args, res)
		if canon == "" {
			t.Fatalf("generation %d recorded nothing", gen+1)
		}
		if n != 1 {
			t.Errorf("generation %d started at %d proposals, want 1 — the previous "+
				"generation's hashes were carried over", gen+1, n)
		}
		if rejectedProposalRecovery(ctx, st, "solve.py", canon, sha) == "" {
			t.Fatalf("generation %d was refused a recovery by the path's own history", gen+1)
		}
		if len(st.c4Rejected) != 1 {
			t.Errorf("generation %d left %d entries for one path", gen+1, len(st.c4Rejected))
		}
	}
}

// The ceiling bounds live paths, and reaching it fails closed.
func TestRefusedReplacementCeilingFailsClosed(t *testing.T) {
	ctx, st, _ := c4State(t, nil)
	mk := func(name string) {
		p := filepath.Join(ctx.WorkingDir, name)
		os.WriteFile(p, []byte(c4Valid), 0o644)
		h := hashBytes([]byte(c4Valid))
		ctx.Ledger[ledgerKey(ctx, name)] = &DeliverableState{
			Path: name, CurrentHash: h, Generation: 1,
			ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationPassed,
			ValidatedHash: h,
		}
		args, res := c4Refusal(`{"path":"` + name + `","content":` + c4JSON(c4BadA) + `}`)
		noteRejectedProposal(ctx, st, "write_file", args, res)
	}
	// solve.py plus seven more fills the ceiling with LIVE paths.
	mk("solve.py")
	for i := 0; i < maxC4Generations-1; i++ {
		mk(fmt.Sprintf("m%d.py", i))
	}
	if len(st.c4Rejected) != maxC4Generations {
		t.Fatalf("%d tracked paths, want %d", len(st.c4Rejected), maxC4Generations)
	}
	// A ninth LIVE path records nothing and recovers nothing — no diagnostic
	// is borrowed from any of the eight.
	mk("ninth.py")
	if len(st.c4Rejected) != maxC4Generations {
		t.Errorf("the ceiling was exceeded: %d", len(st.c4Rejected))
	}
	args, res := c4Refusal(`{"path":"ninth.py","content":` + c4JSON(c4BadA) + `}`)
	canon, sha, n := noteRejectedProposal(ctx, st, "write_file", args, res)
	if canon != "" || n != 0 {
		t.Errorf("the ninth live path was recorded past the ceiling: %q n=%d", canon, n)
	}
	if msg := rejectedProposalRecovery(ctx, st, "ninth.py", canon, sha); msg != "" {
		t.Errorf("a path with no evidence of its own drew a recovery:\n%.150s", msg)
	}

	// A tracked path whose bytes are gone is provably stale, and only that
	// makes room.
	os.Remove(filepath.Join(ctx.WorkingDir, "m0.py"))
	canon, sha, n = noteRejectedProposal(ctx, st, "write_file", args, res)
	if canon == "" || n != 1 {
		t.Errorf("a provably stale entry did not make room: canon=%q n=%d", canon, n)
	}
	if len(st.c4Rejected) > maxC4Generations {
		t.Errorf("eviction exceeded the ceiling: %d", len(st.c4Rejected))
	}
	if rejectedProposalRecovery(ctx, st, "ninth.py", canon, sha) == "" {
		t.Error("the ninth path was still denied after room was made")
	}
}

// Evidence recorded against bytes that have since changed is never offered.
func TestRefusedReplacementEvidenceMustMatchCurrentBytes(t *testing.T) {
	ctx, st, _ := c4State(t, nil)
	args, res := c4Refusal(`{"path":"solve.py","content":` + c4JSON(c4BadA) + `}`)
	canon, sha, _ := noteRejectedProposal(ctx, st, "write_file", args, res)
	// Disk moves without the recovery being taken.
	os.WriteFile(filepath.Join(ctx.WorkingDir, "solve.py"), []byte(c4Fixed), 0o644)
	h := hashBytes([]byte(c4Fixed))
	d := ctx.Ledger[ledgerKey(ctx, "solve.py")]
	d.CurrentHash, d.ValidatedHash = h, h
	if msg := rejectedProposalRecovery(ctx, st, "solve.py", canon, sha); msg != "" {
		t.Errorf("evidence about older bytes was offered anyway:\n%.200s", msg)
	}
}
