package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The V3 preflight gate: the syntax question asked BEFORE the pipeline is
// entered, on a Tier2+ write with V3 configured. Content that does not parse
// gets the answer now rather than after the timeout -- V3 improves a working
// candidate, it does not guess what a malformed one meant.
//
// Every fixture pins the activation condition rather than assuming it:
// Tier2+ content, non-empty V3URL, BypassV3 false, isActiveDebugIteration
// false. The stub records requests IN ORDER, so "one preflight request" is
// measured as the requests that arrive BEFORE /v3/generate. What the
// pipeline and its fallback do afterwards is not checked here.

const preflightSyntaxMark = "def area(radius:"
const preflightSyntaxDetail = "SyntaxError: invalid syntax (line 4)"
const preflightBaselineMark = "# baseline of record"

const preflightHealthy = `import math


def area(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return math.pi * radius * radius


def perimeter(radius):
    for _ in range(1):
        pass
    return 2 * math.pi * radius
`

const preflightBroken = `import math


` + preflightSyntaxMark + `
    if radius < 0:
        raise ValueError("negative radius")
    return math.pi * radius * radius


def perimeter(radius):
    for _ in range(1):
        pass
    return 2 * math.pi * radius
`

// The baselines are the same two programs with a marker line on top, so the
// stub can tell the file on disk from the proposal.
const preflightBaselineHealthy = preflightBaselineMark + "\n" + preflightHealthy
const preflightBaselineBroken = preflightBaselineMark + "\n" + preflightBroken

type preflightStub struct {
	mu       sync.Mutex
	events   []string // "syntax" / "embedded" / "structural" / "v3", in arrival order
	unexpect []string

	unavailableFor string // content carrying this gets a non-200 from /syntax-check
}

func (s *preflightStub) record(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, kind)
}

type preflightOpts struct {
	rel      string
	baseline string // "" leaves the destination absent
	proposal string

	unavailableFor string // content carrying this gets a non-200 from /syntax-check
	before         func(t *testing.T, dir string)
}

// serve is the stub server of runPreflight: it records each request in order,
// answers every check as passing except a syntax check of content that does
// not parse, answers a non-200 to a syntax check of content that carries
// unavailableFor, and records any other endpoint as unexpected.
func (st *preflightStub) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/v3/"):
		st.record("v3")
		// A 418 sends writeFileWithV3 down its fallback. That is fine here:
		// everything this fixture asserts is measured BEFORE this point.
		http.Error(w, "V3 generation stub", http.StatusTeapot)
	case r.URL.Path == "/internal/cyclomatic_complexity":
		json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
	case r.URL.Path == "/internal/embedded_script_check":
		st.record("embedded")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "findings": []interface{}{}})
	case r.URL.Path == "/internal/structural_check":
		st.record("structural")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "unresolved": []string{}})
	case strings.HasSuffix(r.URL.Path, "/syntax-check"):
		st.record("syntax")
		var in struct{ Code string }
		json.NewDecoder(r.Body).Decode(&in)
		if st.unavailableFor != "" && strings.Contains(in.Code, st.unavailableFor) {
			http.Error(w, "checker unavailable", http.StatusInternalServerError)
			return
		}
		valid := !strings.Contains(in.Code, preflightSyntaxMark)
		out := map[string]interface{}{"valid": valid}
		if !valid {
			out["errors"] = []string{preflightSyntaxDetail}
		}
		json.NewEncoder(w).Encode(out)
	default:
		st.mu.Lock()
		st.unexpect = append(st.unexpect, r.Method+" "+r.URL.Path)
		st.mu.Unlock()
		http.Error(w, "unexpected endpoint", http.StatusTeapot)
	}
}

func runPreflight(t *testing.T, o preflightOpts) (*ToolResult, string, *preflightStub) {
	t.Helper()
	dir := t.TempDir()
	st := &preflightStub{unavailableFor: o.unavailableFor}
	srv := httptest.NewServer(http.HandlerFunc(st.serve))
	defer srv.Close()

	target := filepath.Join(dir, o.rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if o.baseline != "" {
		if err := os.WriteFile(target, []byte(o.baseline), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var events []string
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	// Generation runs only where a candidate could be delivered. Declared
	// outputs make this session one, so the fixture reaches the V3 route.
	ctx.TaskContract = declaredOutputs(o.rel)
	ctx.StreamFn = func(event string, _ interface{}) { events = append(events, event) }
	ctx.V3URL = srv.URL
	ctx.SandboxURL = srv.URL
	// Session-owned so the write is the model's own draft, and the most recent
	// tool action is a READ: the active-debug predicate must be false, or the
	// fast path would take this write instead.
	ctx.SessionWrites[o.rel] = true
	ctx.Messages = []AgentMessage{
		{Role: "user", Content: "write the module"},
		{Role: "tool", ToolName: "read_file", Content: o.baseline},
	}

	// --- activation condition, pinned rather than assumed -------------------
	if tier := classifyFileTier(o.rel, o.proposal); tier < Tier2Medium {
		t.Fatalf("fixture must be Tier2+ to reach the V3 preflight, got %v", tier)
	}
	if ctx.V3URL == "" {
		t.Fatal("V3 must be configured and unbypassed")
	}
	if isActiveDebugIteration(ctx, o.rel) {
		t.Fatal("the active-debug predicate must be false, or the fast path takes this write")
	}
	_, statErr := os.Stat(target)
	if (o.baseline == "") != os.IsNotExist(statErr) {
		t.Fatalf("destination state does not match the fixture: %v", statErr)
	}

	if o.before != nil {
		o.before(t, dir)
	}
	args, _ := json.Marshal(map[string]string{"path": o.rel, "content": o.proposal})
	res := executeToolCall("write_file", args, ctx)

	if len(st.unexpect) > 0 {
		t.Fatalf("unexpected endpoints were called: %v", st.unexpect)
	}
	after, _ := os.ReadFile(target)
	ents, _ := os.ReadDir(filepath.Dir(target))
	for _, e := range ents {
		if strings.Contains(e.Name(), ".atlas.tmp") {
			t.Errorf("temporary artifact survived: %s", e.Name())
		}
	}
	return res, string(after), st
}

// assertPreflightChecks counts only the requests that arrived before V3
// generation was entered. This keeps the preflight's budget separate from
// the pipeline's and its fallback's.
func assertPreflightChecks(t *testing.T, st *preflightStub, want int, wantV3 bool) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	var checks []string
	entered := false
	for _, e := range st.events {
		if e == "v3" {
			entered = true
			break
		}
		checks = append(checks, e)
	}
	if len(checks) != want {
		t.Errorf("preflight requests = %v (%d), want %d", checks, len(checks), want)
	}
	if entered != wantV3 {
		t.Errorf("V3 generation entered = %v, want %v", entered, wantV3)
	}
}

// A NEW file that does not parse lands with a warning: there is nothing on
// disk to protect, and the model needs the real traceback more than it needs
// our rejection. V3 is skipped -- feeding it broken content wastes the budget.
func TestPreflightWarnsOnBrokenNewFile(t *testing.T) {
	res, disk, st := runPreflight(t, preflightOpts{rel: "solve.py", proposal: preflightBroken})

	if !res.Success {
		t.Fatalf("the warned-new-file policy must land the bytes: %q", res.Error)
	}
	if disk != preflightBroken {
		t.Fatalf("warned write changed the bytes: %q", disk)
	}
	if !strings.Contains(string(res.Data), "does not parse") {
		t.Errorf("warning lost from the result payload: %s", res.Data)
	}
	if res.MutationStatus != MutationApplied ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationFailed {
		t.Errorf("got %q/%q/%q, want applied/syntax/failed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if !res.Classified() {
		t.Error("result not fully classified")
	}
	assertPreflightChecks(t, st, 1, false)
}

// Already broken on disk: there is no working version to protect, so the
// repair attempt lands with a warning rather than being refused into a loop.
func TestPreflightWarnedRepairOverBrokenBaseline(t *testing.T) {
	res, disk, st := runPreflight(t, preflightOpts{
		rel: "solve.py", baseline: preflightBaselineBroken, proposal: preflightBroken})

	if !res.Success {
		t.Fatalf("a repair on already-broken content must land: %q", res.Error)
	}
	if disk != preflightBroken {
		t.Fatalf("bytes on disk are not the proposal: %q", disk)
	}
	if res.MutationStatus != MutationApplied ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationFailed {
		t.Errorf("got %q/%q/%q, want applied/syntax/failed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	assertPreflightChecks(t, st, 2, false)
}

// Working code on disk is protected: the regression is refused, the bytes
// are untouched, and V3's budget is never spent on content that cannot parse.
func TestPreflightRefusesRegressionOverHealthyBaseline(t *testing.T) {
	res, disk, st := runPreflight(t, preflightOpts{
		rel: "solve.py", baseline: preflightBaselineHealthy, proposal: preflightBroken})

	if res.Success {
		t.Fatal("a newly introduced syntax error must be refused")
	}
	if disk != preflightBaselineHealthy {
		t.Fatalf("refused write changed the file: %q", disk)
	}
	if want := fallbackSyntaxRejection("solve.py", preflightBroken, preflightSyntaxDetail); res.Error != want {
		t.Errorf("rejection text changed:\n got %q\nwant %q", res.Error, want)
	}
	if res.MutationStatus != MutationRefused ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationFailed {
		t.Errorf("got %q/%q/%q, want refused/syntax/failed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if res.ValidationDetail != preflightSyntaxDetail {
		t.Errorf("ValidationDetail = %q, want the checker's finding", res.ValidationDetail)
	}
	if !res.Classified() {
		t.Error("result not fully classified")
	}
	assertPreflightChecks(t, st, 2, false)
}

// The baseline could not be verified. Absence of evidence is not evidence
// that the file was already broken, so the failed proposal is still refused.
func TestPreflightRefusesWhenBaselineEvidenceIsUnavailable(t *testing.T) {
	res, disk, st := runPreflight(t, preflightOpts{
		rel: "solve.py", baseline: preflightBaselineHealthy, proposal: preflightBroken,
		unavailableFor: preflightBaselineMark})

	if res.Success {
		t.Fatal("an unverifiable baseline must not unlock the failed proposal")
	}
	if disk != preflightBaselineHealthy {
		t.Fatalf("refused write changed the file: %q", disk)
	}
	if res.MutationStatus != MutationRefused ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationFailed {
		t.Errorf("got %q/%q/%q, want refused/syntax/failed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	assertPreflightChecks(t, st, 2, false)
}

// The baseline could not be READ at all -- the only way the preflight's own
// refusal is reached, since a readable baseline is decided by the regression
// gate upstream. One request: the proposal's, reused rather than repeated.
func TestPreflightRefusesWhenBaselineCannotBeRead(t *testing.T) {
	res, disk, st := runPreflight(t, preflightOpts{
		rel: "solve.py", baseline: preflightBaselineHealthy, proposal: preflightBroken,
		before: func(t *testing.T, dir string) {
			target := filepath.Join(dir, "solve.py")
			if err := os.Chmod(target, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(target, 0o644) })
		}})

	if res.Success {
		t.Fatal("an unreadable baseline must not unlock the failed proposal")
	}
	if disk != "" && disk != preflightBaselineHealthy {
		t.Fatalf("refused write changed the file: %q", disk)
	}
	if res.MutationStatus != MutationRefused ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationFailed {
		t.Errorf("got %q/%q/%q, want refused/syntax/failed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	assertPreflightChecks(t, st, 1, false)
}

// The warned write is a mutation like any other and can fail. The
// observation that justified the warning survives the failure: these exact
// bytes do not parse, whether or not they reached disk.
func TestPreflightWarnedWriteFilesystemFailure(t *testing.T) {
	res, _, st := runPreflight(t, preflightOpts{
		rel: "ro/solve.py", proposal: preflightBroken,
		before: func(t *testing.T, dir string) {
			sub := filepath.Join(dir, "ro")
			if err := os.Chmod(sub, 0o555); err != nil { // temp write must fail
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(sub, 0o755) })
		}})

	if res.Success {
		t.Fatal("a failed temp write must not report success")
	}
	if !strings.Contains(res.Error, "cannot write") {
		t.Fatalf("failure did not occur at the temp write: %q", res.Error)
	}
	if res.MutationStatus != MutationFailed {
		t.Errorf("MutationStatus = %q, want failed", res.MutationStatus)
	}
	if res.ValidationKind != ValidationKindSyntax || res.ValidationStatus != ValidationFailed {
		t.Errorf("validation = %q/%q, want syntax/failed -- the observation on "+
			"those exact bytes is not erased by the write failing",
			res.ValidationKind, res.ValidationStatus)
	}
	if res.ValidationStatus.Passed() {
		t.Error("a failed write must never claim validation passed")
	}
	if !res.Classified() {
		t.Error("result not fully classified")
	}
	assertPreflightChecks(t, st, 1, false)
}

// The gate lets content into generation unless it is known to be broken.
// "parses": one preflight request, then the pipeline. What the pipeline
// decides about its own candidate is not checked here, only the handoff.
// "checker unavailable": fail-open is the whole posture of this gate. It
// blocks KNOWN-broken content, so an unavailable checker must not cost the
// model its pipeline run.
// "no check applies": no service is asked anything.
func TestPreflightEntersGenerationUnlessTheContentIsKnownBroken(t *testing.T) {
	if _, gated := syntaxGateLanguages[".rs"]; gated {
		t.Fatal(".rs gained a sandbox checker; pick a type neither check applies to")
	}
	for _, tc := range []struct {
		name       string
		opts       preflightOpts
		wantChecks int
	}{
		{"parses", preflightOpts{rel: "solve.py", proposal: preflightHealthy}, 1},
		{"checker unavailable", preflightOpts{rel: "solve.py", proposal: preflightHealthy, unavailableFor: "def perimeter"}, 1},
		{"no check applies", preflightOpts{rel: "engine.rs", proposal: debugRustProposal}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, st := runPreflight(t, tc.opts)
			assertPreflightChecks(t, st, tc.wantChecks, true)
		})
	}
}
