package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The V3 service-error fallback: generation was entered, it failed, and the
// model's own baseline is written instead. The syntax check there is a FRESH
// one on purpose. The preflight examined these bytes before generation, and
// generation can run for minutes; re-checking immediately before the write
// means the observation describes the bytes that land, with no window in
// between. That is why these fixtures expect two syntax requests and assert
// their order around /v3/generate rather than collapsing them into one.
//
// Cancellation is NOT this branch: the turn was aborted, nothing lands, and no
// fallback check runs. Its classification is a later slice.

const fbSyntaxDetail = "SyntaxError: invalid syntax (line 4)"

const fbTier2Python = `import math


def area(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return math.pi * radius * radius


def perimeter(radius):
    for _ in range(1):
        pass
    return 2 * math.pi * radius
`

const fbUnresolvedPython = `import math


def area(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return missing_helper(radius)


def perimeter(radius):
    for _ in range(1):
        pass
    return 2 * math.pi * radius
`

type fallbackStub struct {
	mu       sync.Mutex
	events   []string // "syntax" / "structural" / "v3", in arrival order
	unexpect []string
	target   string // absolute destination, for diagnostics the route builds from it
}

func (s *fallbackStub) record(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, kind)
}

func (s *fallbackStub) seq() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

type fallbackOpts struct {
	rel      string
	prior    string // content on disk before the write; "" leaves it absent
	proposal string

	// syntaxVerdicts answers /syntax-check in order: true = parses. A shorter
	// list repeats its last entry. nil means every check passes.
	syntaxVerdicts []bool
	// syntaxDown serves a non-200 from /syntax-check, so the check applies but
	// cannot run.
	syntaxDown bool
	// introduces makes structural_check report this symbol as newly unresolved.
	introduces string
	// cancelOnGenerate aborts the request context when generation is entered.
	cancelOnGenerate bool
	before           func(t *testing.T, dir string)
}

func runFallback(t *testing.T, o fallbackOpts) (*ToolResult, string, *fallbackStub, []string) {
	t.Helper()
	dir := t.TempDir()
	st := &fallbackStub{}
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var syntaxSeen int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v3/generate":
			st.record("v3")
			if o.cancelOnGenerate {
				cancel()
			}
			// A non-200 is the ordinary service-error case: generation was
			// entered and failed, which is exactly this branch's trigger.
			http.Error(w, "V3 service error", http.StatusBadGateway)
		case r.URL.Path == "/internal/cyclomatic_complexity":
			json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
		case r.URL.Path == "/internal/structural_check":
			st.record("structural")
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			src, _ := body["source"].(string)
			out := map[string]interface{}{"ok": true, "unresolved": []string{}}
			if o.introduces != "" && strings.Contains(src, o.introduces+"(") {
				out["unresolved"] = []string{o.introduces}
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			st.record("syntax")
			if o.syntaxDown {
				http.Error(w, "checker unavailable", http.StatusInternalServerError)
				return
			}
			mu.Lock()
			i := syntaxSeen
			syntaxSeen++
			mu.Unlock()
			valid := true
			if len(o.syntaxVerdicts) > 0 {
				if i >= len(o.syntaxVerdicts) {
					i = len(o.syntaxVerdicts) - 1
				}
				valid = o.syntaxVerdicts[i]
			}
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{fbSyntaxDetail}
			}
			json.NewEncoder(w).Encode(out)
		default:
			st.mu.Lock()
			st.unexpect = append(st.unexpect, r.Method+" "+r.URL.Path)
			st.mu.Unlock()
			http.Error(w, "unexpected endpoint", http.StatusTeapot)
		}
	}))
	defer srv.Close()

	target := filepath.Join(dir, o.rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if o.prior != "" {
		if err := os.WriteFile(target, []byte(o.prior), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st.target = target

	var events []string
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	// Generation runs only where a candidate could be delivered (C2); declared
	// outputs make this session one, so the fixture still reaches the V3 route.
	ctx.TaskContract = declaredOutputs(o.rel)
	ctx.StreamFn = func(event string, _ interface{}) { events = append(events, event) }
	ctx.Ctx = reqCtx
	ctx.V3URL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.SessionWrites[o.rel] = true
	ctx.Messages = []AgentMessage{
		{Role: "user", Content: "write the module"},
		{Role: "tool", ToolName: "read_file", Content: o.prior},
	}

	// --- activation condition, pinned rather than assumed -------------------
	if tier := classifyFileTier(o.rel, o.proposal); tier < Tier2Medium {
		t.Fatalf("fixture must be Tier2+ to enter the V3 pipeline, got %v", tier)
	}
	if ctx.V3URL == "" {
		t.Fatal("V3 must be configured and unbypassed")
	}
	if isActiveDebugIteration(ctx, o.rel) {
		t.Fatal("the active-debug predicate must be false, or the fast path takes this write")
	}

	if o.before != nil {
		o.before(t, dir)
	}
	args, _ := json.Marshal(map[string]string{"path": o.rel, "content": o.proposal})
	res := executeToolCall("write_file", args, ctx)

	if len(st.unexpect) > 0 {
		t.Fatalf("unexpected endpoints were called: %v", st.unexpect)
	}
	seq := st.seq()
	var sawV3 bool
	for _, e := range seq {
		if e == "v3" {
			sawV3 = true
		}
	}
	if !sawV3 {
		t.Fatalf("V3 generation was never entered; sequence = %v", seq)
	}
	after, _ := os.ReadFile(target)
	ents, _ := os.ReadDir(filepath.Dir(target))
	for _, e := range ents {
		if strings.Contains(e.Name(), ".atlas.tmp") {
			t.Errorf("temporary artifact survived: %s", e.Name())
		}
	}
	return res, string(after), st, events
}

func assertSeq(t *testing.T, st *fallbackStub, want ...string) {
	t.Helper()
	got := st.seq()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("request sequence = %v, want %v", got, want)
	}
}

// A fallback write is the model's own bytes: no provenance may claim otherwise.
func assertNoV3Provenance(t *testing.T, res *ToolResult) {
	t.Helper()
	if res.V3Used || res.CandidatesTested != 0 || res.WinningScore != 0 ||
		res.PhaseSolved != "" || len(res.VerificationEvidence) != 0 {
		t.Errorf("V3 provenance attached to a baseline write: %+v", res)
	}
}

// 1. Ordinary service error, fresh check passes: the baseline lands and the
// result reports the check that ran on it. Two syntax requests, one on each
// side of generation -- the second is the point of the branch.
func TestFallbackPassesAndWritesBaseline(t *testing.T) {
	res, disk, st, events := runFallback(t, fallbackOpts{
		rel: "solve.py", proposal: fbTier2Python})

	if !res.Success || disk != fbTier2Python {
		t.Fatalf("fallback write did not land: success=%v", res.Success)
	}
	if res.MutationStatus != MutationApplied ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationPassed {
		t.Errorf("got %q/%q/%q, want applied/syntax/passed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if !res.Classified() {
		t.Error("result not fully classified")
	}
	assertNoV3Provenance(t, res)
	// Preflight, generation, the FRESH check, then the structural gate.
	assertSeq(t, st, "syntax", "v3", "syntax", "structural")
	// Existing projections: the pipeline announces itself before generation,
	// and the fallback announces that it is writing the model's version.
	if len(events) != 2 || events[0] != "v3_progress" || events[1] != "text" {
		t.Errorf("SSE projections = %v, want [v3_progress text]", events)
	}
}

// 2. The fresh check applies and cannot run. Fail-open is unchanged: the bytes
// land, and not_run is reported rather than the stronger claim.
func TestFallbackCheckerUnavailableStillWrites(t *testing.T) {
	res, disk, st, _ := runFallback(t, fallbackOpts{
		rel: "solve.py", proposal: fbTier2Python, syntaxDown: true})

	if !res.Success || disk != fbTier2Python {
		t.Fatal("fail-open fallback write did not land")
	}
	if res.MutationStatus != MutationApplied ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationNotRun {
		t.Errorf("got %q/%q/%q, want applied/syntax/not_run",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if res.ValidationStatus.Passed() {
		t.Error("an unavailable checker must never read as passed")
	}
	assertNoV3Provenance(t, res)
	assertSeq(t, st, "syntax", "v3", "syntax", "structural")
}

// 3. Neither check applies: no service is asked anything, and the bytes land
// with the honest answer that there was nothing to check.
func TestFallbackNoCheckApplies(t *testing.T) {
	res, disk, st, _ := runFallback(t, fallbackOpts{
		rel: "engine.rs", proposal: debugRustProposal})

	if !res.Success || disk != debugRustProposal {
		t.Fatalf("fallback write did not land: success=%v", res.Success)
	}
	if res.MutationStatus != MutationApplied ||
		res.ValidationKind != ValidationKindNone ||
		res.ValidationStatus != ValidationNotApplicable {
		t.Errorf("got %q/%q/%q, want applied/none/not_applicable",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if !res.Classified() {
		t.Error("result not fully classified")
	}
	assertNoV3Provenance(t, res)
	// .rs reaches neither checker, and structural_check is .py-scoped.
	assertSeq(t, st, "v3")
}

// 4. The fresh check earns its keep: the preflight passed these bytes before
// generation, the post-generation check finds them broken, and nothing lands.
// The sequenced stub is what proves the second check is a real second look.
func TestFallbackFreshCheckRefusesBrokenBaseline(t *testing.T) {
	res, disk, st, events := runFallback(t, fallbackOpts{
		rel: "solve.py", proposal: fbTier2Python,
		syntaxVerdicts: []bool{true, false}})

	if res.Success {
		t.Fatal("content the checker rejects must not land")
	}
	if disk != "" {
		t.Fatalf("refused fallback wrote bytes: %q", disk)
	}
	// The fallback builds its diagnostic from the resolved destination, which
	// is what the model is handed here.
	if want := fallbackSyntaxRejection(st.target, fbTier2Python, fbSyntaxDetail); res.Error != want {
		t.Errorf("rejection text changed:\n got %q\nwant %q", res.Error, want)
	}
	if res.MutationStatus != MutationRefused ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationFailed {
		t.Errorf("got %q/%q/%q, want refused/syntax/failed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if res.ValidationDetail != fbSyntaxDetail {
		t.Errorf("ValidationDetail = %q, want the checker's finding", res.ValidationDetail)
	}
	if !res.Classified() {
		t.Error("result not fully classified")
	}
	// Refused before the structural gate and before the fallback notice.
	assertSeq(t, st, "syntax", "v3", "syntax")
	if len(events) != 1 || events[0] != "v3_progress" {
		t.Errorf("SSE = %v, want only the pre-generation notice: a refusal "+
			"must not announce a write it did not make", events)
	}
}

// 5. Syntax passed on these exact bytes and the structural gate refused them,
// so structural is the decisive outcome; the passing syntax evidence must not
// overwrite it.
func TestFallbackStructuralRefusalOutranksPassingSyntax(t *testing.T) {
	res, disk, st, events := runFallback(t, fallbackOpts{
		rel: "solve.py", prior: fbTier2Python, proposal: fbUnresolvedPython,
		introduces: "missing_helper"})

	if res.Success {
		t.Fatal("the structural gate did not refuse")
	}
	if disk != fbTier2Python {
		t.Fatalf("refused fallback changed the file: %q", disk)
	}
	if !strings.Contains(res.Error, "missing_helper") {
		t.Fatalf("refusal came from a different gate: %q", res.Error)
	}
	if res.MutationStatus != MutationRefused ||
		res.ValidationKind != ValidationKindStructural ||
		res.ValidationStatus != ValidationFailed {
		t.Errorf("got %q/%q/%q, want refused/structural/failed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if !strings.Contains(res.ValidationDetail, "missing_helper") {
		t.Errorf("ValidationDetail must name the unresolved symbol, got %q", res.ValidationDetail)
	}
	if len(events) != 1 || events[0] != "v3_progress" {
		t.Errorf("SSE = %v, want only the pre-generation notice: a refusal "+
			"must not announce a write it did not make", events)
	}
	// Preflight, generation, the fresh check, then the structural pair the
	// gate needs before it may refuse (edited side, then original side).
	assertSeq(t, st, "syntax", "v3", "syntax", "structural", "structural")
}

// 6. The fallback write is a mutation like any other and can fail. The
// observation on those exact bytes survives, the error stays non-nil, and the
// file that was there is untouched.
func TestFallbackPassedThenWriteFailure(t *testing.T) {
	res, disk, st, _ := runFallback(t, fallbackOpts{
		rel: "ro/solve.py", prior: fbUnresolvedPython, proposal: fbTier2Python,
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
	if disk != fbUnresolvedPython {
		t.Fatalf("prior bytes changed on a failed write: %q", disk)
	}
	if res.MutationStatus != MutationFailed {
		t.Errorf("MutationStatus = %q, want failed", res.MutationStatus)
	}
	if res.ValidationKind != ValidationKindSyntax || res.ValidationStatus != ValidationPassed {
		t.Errorf("validation = %q/%q, want syntax/passed -- the observation on "+
			"those exact bytes survives the mutation failure",
			res.ValidationKind, res.ValidationStatus)
	}
	if !res.Classified() {
		t.Error("result not fully classified")
	}
	assertNoV3Provenance(t, res)
	assertSeq(t, st, "syntax", "v3", "syntax", "structural")
}

// 7. Cancellation is not a fallback. The turn was aborted, so no fresh check
// runs, nothing lands, and the model is told plainly. Classification of this
// path is deliberately left as it is: aborting a turn is a different fact from
// a gate refusing, and it gets its own slice.
func TestFallbackCancellationChecksNothingAndWritesNothing(t *testing.T) {
	res, disk, st, _ := runFallback(t, fallbackOpts{
		rel: "solve.py", proposal: fbTier2Python, cancelOnGenerate: true})

	if res.Success {
		t.Fatal("a cancelled write must not report success")
	}
	if !strings.Contains(res.Error, "cancelled") {
		t.Fatalf("cancellation must say so, got %q", res.Error)
	}
	if disk != "" {
		t.Fatalf("cancelled write landed bytes: %q", disk)
	}
	assertNoV3Provenance(t, res)
	// The preflight ran before generation; nothing ran after it.
	assertSeq(t, st, "syntax", "v3")
}
