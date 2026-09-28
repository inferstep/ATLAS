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

// The active edit-test-fix route: write_file on a file the session already
// wrote, immediately after a FAILED run that named it. V3 is skipped so the
// model can iterate at test speed, and this branch runs the gates itself.
//
// Every production fixture below pins the real predicate rather than assuming
// it: session-owned existing file, most recent tool message a failed
// run_command whose payload carries "success":false and names the target
// basename as a whole filename token, Tier2+ content, a non-empty V3URL, and
// isActiveDebugIteration true. Tier2+ with V3 configured is what makes the
// routing proof mean something -- the V3 branch's every other condition holds,
// so the predicate is the ONLY reason generation does not run, and a
// /v3/generate request fails the test. TestActiveDebugNegativeControl flips
// the one bit and shows V3 taking over.

// debugSyntaxErrorMark is the shape the sandbox stub calls unparseable. It is
// content, not a filename, so the same fixture can carry it on either side.
const debugSyntaxErrorMark = "def area(radius:"

// baselineOnlyMark identifies the on-disk content to the stub, so a case can
// make the checker unavailable for the BASELINE while it still answers for the
// proposal.
const baselineOnlyMark = "# baseline of record"

const debugBaselineHealthy = baselineOnlyMark + `
import math


def area(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return math.pi * radius * radius


def perimeter(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return 2 * math.pi * radius
`

const debugBaselineBroken = baselineOnlyMark + `
import math


` + debugSyntaxErrorMark + `
    if radius < 0:
        raise ValueError("negative radius")
    return math.pi * radius * radius


def perimeter(radius):
    return 2 * math.pi * radius
`

const debugProposalHealthy = `import math


def area(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return math.pi * radius * radius


def perimeter(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return 2 * math.pi * radius


def diameter(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return 2 * radius
`

const debugProposalBroken = `import math


` + debugSyntaxErrorMark + `
    if radius < 0:
        raise ValueError("negative radius")
    return math.pi * radius * radius


def perimeter(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return 2 * math.pi * radius
`

// Parses cleanly and calls a name the file neither imports nor defines.
const debugProposalUnresolved = `import math


def area(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return missing_helper(radius)


def perimeter(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return 2 * math.pi * radius
`

type debugRouteStub struct {
	mu             sync.Mutex
	syntaxCode     []string
	embeddedCalls  int
	structuralCode []string
	v3Calls        int
	unexpected     []string
}

func (s *debugRouteStub) syntaxCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.syntaxCode)
}

func (s *debugRouteStub) checkerCalls() (syntax, embedded, structural int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.syntaxCode), s.embeddedCalls, len(s.structuralCode)
}

type debugRouteOpts struct {
	rel      string // path as the model sends it
	baseline string // content on disk before the write
	proposal string // incoming content

	noSandbox      bool   // sandbox not configured: the checker cannot run
	unavailableFor string // content carrying this gets a non-200 from /syntax-check
	introduces     string // structural_check reports this symbol as newly unresolved
	passingRun     bool   // negative control: the last run SUCCEEDED
	absent         bool   // the session owns the path but the file is gone
	before         func(t *testing.T, dir string)
}

// runDebugRoute dispatches one write_file through the real tool path and
// returns the result, the bytes on disk afterwards, and the stub's record.
func runDebugRoute(t *testing.T, o debugRouteOpts) (*ToolResult, string, *debugRouteStub) {
	t.Helper()
	dir := t.TempDir()
	st := &debugRouteStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"):
			st.mu.Lock()
			st.v3Calls++
			st.mu.Unlock()
			http.Error(w, "V3 generation reached", http.StatusTeapot)
		case r.URL.Path == "/internal/cyclomatic_complexity":
			// A legitimate call on this path: tier refinement, not a gate.
			json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
		case r.URL.Path == "/internal/embedded_script_check":
			// Counted, not rejected: reaching it is a real answer about
			// applicability, and a fixture that asserts zero must be able to
			// say so rather than die on an unexpected-endpoint fatal.
			st.mu.Lock()
			st.embeddedCalls++
			st.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "findings": []interface{}{}})
		case r.URL.Path == "/internal/structural_check":
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			src, _ := body["source"].(string)
			st.mu.Lock()
			st.structuralCode = append(st.structuralCode, src)
			st.mu.Unlock()
			out := map[string]interface{}{"ok": true, "unresolved": []string{}}
			if o.introduces != "" && strings.Contains(src, o.introduces+"(") {
				out["unresolved"] = []string{o.introduces}
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			st.mu.Lock()
			st.syntaxCode = append(st.syntaxCode, in.Code)
			st.mu.Unlock()
			if o.unavailableFor != "" && strings.Contains(in.Code, o.unavailableFor) {
				http.Error(w, "checker unavailable", http.StatusInternalServerError)
				return
			}
			valid := !strings.Contains(in.Code, debugSyntaxErrorMark)
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{debugSyntaxDetail}
			}
			json.NewEncoder(w).Encode(out)
		default:
			st.mu.Lock()
			st.unexpected = append(st.unexpected, r.Method+" "+r.URL.Path)
			st.mu.Unlock()
			http.Error(w, "unexpected endpoint", http.StatusTeapot)
		}
	}))
	defer srv.Close()

	target := filepath.Join(dir, o.rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if !o.absent {
		if err := os.WriteFile(target, []byte(o.baseline), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	// Generation runs only where a candidate could be delivered (C2); declared
	// outputs make this session one, so the fixture still reaches the V3 route.
	ctx.TaskContract = declaredOutputs(o.rel)
	ctx.StreamFn = func(string, interface{}) {}
	ctx.V3URL = srv.URL
	if !o.noSandbox {
		ctx.SandboxURL = srv.URL
	}
	// Session ownership plus a failing run that names the file: the two halves
	// of the predicate.
	ctx.SessionWrites[o.rel] = true
	base := filepath.Base(o.rel)
	outcome := `"success":false`
	if o.passingRun {
		outcome = `"success":true`
	}
	runMsg := `{` + outcome + `,"data":{"stderr":"Traceback (most recent call last):\n  File \"./` +
		base + `\", line 4\nSyntaxError: invalid syntax"}}`
	ctx.Messages = []AgentMessage{
		{Role: "user", Content: "fix the failing module"},
		{Role: "assistant", Content: "running it again"},
		{Role: "tool", ToolName: "run_command", Content: runMsg},
	}

	// --- predicate, pinned rather than assumed ------------------------------
	if _, err := os.Stat(target); o.absent != os.IsNotExist(err) {
		t.Fatalf("destination state does not match the fixture (absent=%v): %v", o.absent, err)
	}
	if !ctx.SessionWrites[o.rel] {
		t.Fatal("the session must own the file")
	}
	if last := ctx.Messages[len(ctx.Messages)-1]; last.Role != "tool" || last.ToolName != "run_command" {
		t.Fatalf("the most recent message must be a run_command tool result, got %s/%s",
			last.Role, last.ToolName)
	}
	if !o.passingRun && !strings.Contains(runMsg, `"success":false`) {
		t.Fatal("the run must carry a literal failure")
	}
	if !mentionsFilename(runMsg, base) {
		t.Fatalf("the run output must name %q as a whole filename token", base)
	}
	if tier := classifyFileTier(o.rel, o.proposal); tier < Tier2Medium {
		t.Fatalf("fixture must be Tier2+ so the predicate is the ONLY thing "+
			"holding V3 off, got %v", tier)
	}
	if ctx.V3URL == "" {
		t.Fatal("V3 must be configured and unbypassed for the routing proof")
	}
	if got := isActiveDebugIteration(ctx, o.rel); got != !o.passingRun {
		t.Fatalf("isActiveDebugIteration = %v, want %v", got, !o.passingRun)
	}

	if o.before != nil {
		o.before(t, dir)
	}
	args, _ := json.Marshal(map[string]string{"path": o.rel, "content": o.proposal})
	res := executeToolCall("write_file", args, ctx)

	if len(st.unexpected) > 0 {
		t.Fatalf("unexpected endpoints were called: %v", st.unexpected)
	}
	if !o.passingRun && st.v3Calls != 0 {
		t.Fatalf("V3 generation ran (%d calls); this is not the active-debug route", st.v3Calls)
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

const debugSyntaxDetail = "SyntaxError: invalid syntax (line 4)"

// 1. The proposal parses: the write lands and the route reports what it saw.
// The baseline is never consulted -- exactly one checker call.
func TestActiveDebugProposalPasses(t *testing.T) {
	res, disk, st := runDebugRoute(t, debugRouteOpts{
		rel: "mod.py", baseline: debugBaselineHealthy, proposal: debugProposalHealthy})

	if !res.Success || disk != debugProposalHealthy {
		t.Fatalf("write did not land: success=%v", res.Success)
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
	if st.syntaxCalls() != 1 {
		t.Errorf("syntax-check calls = %d, want exactly 1 -- the proposal is "+
			"evaluated once and a passing proposal never consults the baseline",
			st.syntaxCalls())
	}
}

// 2. The check applied and could not run. Fail-open is unchanged -- the bytes
// land -- and the result says not_run, never passed.
func TestActiveDebugCheckerUnavailable(t *testing.T) {
	res, disk, st := runDebugRoute(t, debugRouteOpts{
		rel: "mod.py", baseline: debugBaselineHealthy, proposal: debugProposalHealthy,
		noSandbox: true})

	if !res.Success || disk != debugProposalHealthy {
		t.Fatal("fail-open write did not land")
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
	if st.syntaxCalls() != 0 {
		t.Errorf("syntax-check calls = %d, want 0 with no sandbox configured", st.syntaxCalls())
	}
}

// 3. The proposal demonstrably fails and the baseline demonstrably passes:
// the regression is refused, the bytes on disk are untouched, and the refusal
// says which check decided it. Both sides are evaluated exactly once.
func TestActiveDebugFailedProposalOverHealthyBaselineIsRefused(t *testing.T) {
	res, disk, st := runDebugRoute(t, debugRouteOpts{
		rel: "mod.py", baseline: debugBaselineHealthy, proposal: debugProposalBroken})

	if res.Success {
		t.Fatal("a newly introduced syntax error must be refused")
	}
	if disk != debugBaselineHealthy {
		t.Fatalf("refused write changed the file: %q", disk)
	}
	if want := fallbackSyntaxRejection("mod.py", debugProposalBroken, debugSyntaxDetail); res.Error != want {
		t.Errorf("rejection text changed:\n got %q\nwant %q", res.Error, want)
	}
	if res.MutationStatus != MutationRefused ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationFailed {
		t.Errorf("got %q/%q/%q, want refused/syntax/failed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if res.MutationStatus.Applied() {
		t.Error("a refusal must never read as applied")
	}
	if res.ValidationDetail != debugSyntaxDetail {
		t.Errorf("ValidationDetail = %q, want the checker's finding", res.ValidationDetail)
	}
	if !res.Classified() {
		t.Error("result not fully classified")
	}
	if st.syntaxCalls() != 2 {
		t.Errorf("syntax-check calls = %d, want exactly 2 (proposal, then baseline once)",
			st.syntaxCalls())
	}
}

// 4. Both sides demonstrably fail: the repair-in-progress carveout stands, the
// bytes land, and the result reports honestly that they do not parse.
func TestActiveDebugFailedProposalOverBrokenBaselineLands(t *testing.T) {
	res, disk, st := runDebugRoute(t, debugRouteOpts{
		rel: "mod.py", baseline: debugBaselineBroken, proposal: debugProposalBroken})

	if !res.Success {
		t.Fatalf("a repair attempt on already-broken content must land: %q", res.Error)
	}
	if disk != debugProposalBroken {
		t.Fatalf("bytes on disk are not the proposal: %q", disk)
	}
	if res.MutationStatus != MutationApplied ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationFailed {
		t.Errorf("got %q/%q/%q, want applied/syntax/failed -- applied and failed "+
			"are orthogonal: the file landed AND it does not parse",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if res.ValidationStatus.Passed() {
		t.Error("landed bytes that do not parse must never read as passed")
	}
	if !res.Classified() {
		t.Error("result not fully classified")
	}
	if st.syntaxCalls() != 2 {
		t.Errorf("syntax-check calls = %d, want exactly 2 (proposal, then baseline once)",
			st.syntaxCalls())
	}
}

// 5. The proposal demonstrably fails and the baseline evidence is unavailable.
// Absence of evidence is not evidence of a broken baseline, so the failed
// proposal is refused -- the same answer a passing baseline gives.
func TestActiveDebugUnavailableBaselineRefusesFailedProposal(t *testing.T) {
	res, disk, st := runDebugRoute(t, debugRouteOpts{
		rel: "mod.py", baseline: debugBaselineHealthy, proposal: debugProposalBroken,
		unavailableFor: baselineOnlyMark})

	if res.Success {
		t.Fatal("an unverifiable baseline must not unlock the failed proposal")
	}
	if disk != debugBaselineHealthy {
		t.Fatalf("refused write changed the file: %q", disk)
	}
	if res.MutationStatus != MutationRefused ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationFailed {
		t.Errorf("got %q/%q/%q, want refused/syntax/failed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if st.syntaxCalls() != 2 {
		t.Errorf("syntax-check calls = %d, want exactly 2", st.syntaxCalls())
	}
}

// 6. Negative control. One bit of the predicate flips -- the last run
// SUCCEEDED -- and the same fixture routes to V3 instead. Without this the
// other cases could be passing on some unrelated path.
func TestActiveDebugNegativeControl(t *testing.T) {
	_, _, st := runDebugRoute(t, debugRouteOpts{
		rel: "mod.py", baseline: debugBaselineHealthy, proposal: debugProposalHealthy,
		passingRun: true})

	if st.v3Calls == 0 {
		t.Fatal("with the predicate false the Tier2 write must reach V3 generation; " +
			"the active-debug cases are therefore held off V3 by the predicate alone")
	}
}

// 7. Mutation failure and validation success are orthogonal: bytes that were
// checked and passed can still fail to land.
func TestActiveDebugPassedThenWriteFailure(t *testing.T) {
	res, disk, _ := runDebugRoute(t, debugRouteOpts{
		rel: "ro/mod.py", baseline: debugBaselineHealthy, proposal: debugProposalHealthy,
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
	if disk != debugBaselineHealthy {
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
}

// 8. Syntax passed on these exact bytes and the structural check refused them,
// so structural is the decisive outcome. Recording syntax/failed here would
// assert the opposite of what happened, and the baseline is never consulted.
func TestActiveDebugStructuralRefusalOutranksPassingSyntax(t *testing.T) {
	res, disk, st := runDebugRoute(t, debugRouteOpts{
		rel: "mod.py", baseline: debugBaselineHealthy, proposal: debugProposalUnresolved,
		introduces: "missing_helper"})

	if res.Success {
		t.Fatal("the structural gate did not refuse")
	}
	if disk != debugBaselineHealthy {
		t.Fatalf("refused write changed the file: %q", disk)
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
	if !res.Classified() {
		t.Error("result not fully classified")
	}
	if st.syntaxCalls() != 1 {
		t.Errorf("syntax-check calls = %d, want exactly 1 -- a passing proposal "+
			"never consults the baseline", st.syntaxCalls())
	}
}

// 9. Residual case, and the only one that reaches the branch's OWN syntax
// refusal. With the destination present, the existing-file regression gate
// upstream decides first (cases 3 and 5); the branch decides only when that
// gate could not -- here the session owns the path but a failing run removed
// the file, so there is no baseline to read. readOriginalForGate yields "",
// the checker finds it healthy, and the regression is refused rather than
// landing unexamined. Behavior is unchanged; the refusal is now classified.
func TestActiveDebugRefusesWhenNoBaselineCanBeRead(t *testing.T) {
	res, disk, st := runDebugRoute(t, debugRouteOpts{
		rel: "mod.py", baseline: debugBaselineHealthy, proposal: debugProposalBroken,
		absent: true})

	if res.Success {
		t.Fatal("a demonstrably broken proposal must not land on an unreadable baseline")
	}
	if disk != "" {
		t.Fatalf("a refusal created the file: %q", disk)
	}
	if res.MutationStatus != MutationRefused ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationFailed {
		t.Errorf("got %q/%q/%q, want refused/syntax/failed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if res.ValidationDetail != debugSyntaxDetail {
		t.Errorf("ValidationDetail = %q, want the checker's finding", res.ValidationDetail)
	}
	if !res.Classified() {
		t.Error("result not fully classified")
	}
	if st.syntaxCalls() != 2 {
		t.Errorf("syntax-check calls = %d, want exactly 2 (proposal, then the "+
			"empty baseline once)", st.syntaxCalls())
	}
}

// 10. Neither check applies. Rust is the fixture because the applicability
// question has to be answered by the CONTENT TYPE, not by a small file or an
// absent service: .rs is in neither syntaxGateLanguages (no sandbox checker)
// nor embeddedScriptExts (it cannot carry a <script> block), while a real
// module of it classifies Tier2+ on its own logic. The routing premise is
// therefore intact -- Tier2+, V3 configured, predicate true -- and the answer
// is not_applicable rather than not_run: nothing here was checkable, which is
// a different fact from "a checker was unavailable" (case 2).
const debugRustBaseline = `pub fn area(radius: f64) -> f64 {
    if radius < 0.0 {
        panic!("negative radius");
    }
    std::f64::consts::PI * radius * radius
}

pub fn total(radii: &[f64]) -> f64 {
    let mut sum = 0.0;
    for r in radii {
        sum += area(*r);
    }
    sum
}
`

const debugRustProposal = `pub fn area(radius: f64) -> f64 {
    if radius < 0.0 {
        panic!("negative radius");
    }
    std::f64::consts::PI * radius * radius
}

pub fn total(radii: &[f64]) -> f64 {
    let mut sum = 0.0;
    for r in radii {
        sum += area(*r);
    }
    sum
}

pub fn describe(radius: f64) -> String {
    match radius {
        r if r < 0.0 => String::from("invalid"),
        _ => format!("area {}", area(radius)),
    }
}
`

func TestActiveDebugNoCheckApplies(t *testing.T) {
	if tier := classifyFileTier("engine.rs", debugRustProposal); tier < Tier2Medium {
		t.Fatalf("the fixture must classify Tier2+ on its own content, got %v -- "+
			"a sub-Tier2 file would prove nothing about this route", tier)
	}
	if _, gated := syntaxGateLanguages[".rs"]; gated {
		t.Fatal(".rs gained a sandbox checker; pick a type neither check applies to")
	}
	if embeddedScriptExts[".rs"] {
		t.Fatal(".rs gained an embedded-script check; pick a type neither check applies to")
	}

	res, disk, st := runDebugRoute(t, debugRouteOpts{
		rel: "engine.rs", baseline: debugRustBaseline, proposal: debugRustProposal})

	if !res.Success {
		t.Fatalf("the write did not land: %q", res.Error)
	}
	if disk != debugRustProposal {
		t.Fatalf("bytes on disk are not the proposal: %q", disk)
	}
	if res.MutationStatus != MutationApplied ||
		res.ValidationKind != ValidationKindNone ||
		res.ValidationStatus != ValidationNotApplicable {
		t.Errorf("got %q/%q/%q, want applied/none/not_applicable",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if res.ValidationStatus.Passed() {
		t.Error("nothing was checked, so nothing may read as passed")
	}
	if !res.Classified() {
		t.Error("result not fully classified")
	}
	syntax, embedded, structural := st.checkerCalls()
	if syntax != 0 || embedded != 0 {
		t.Errorf("checker calls = %d syntax / %d embedded, want 0/0 -- "+
			"applicability is decided locally, before any service question",
			syntax, embedded)
	}
	if structural != 0 {
		t.Errorf("structural_check calls = %d, want 0 (the gate is .py-scoped)", structural)
	}
}

// 11. The policy itself: only a DEMONSTRATED baseline failure unlocks a failed
// proposal. Passed, not_run, not_applicable and Unknown all refuse -- absence
// of evidence is not evidence that the file was already broken.
func TestBaselineAllowsRepairOnlyOnDemonstratedFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		in   checkOutcome
		want bool
	}{
		{"failed", checkOutcome{Status: ValidationFailed, Detail: "SyntaxError"}, true},
		{"passed", checkOutcome{Status: ValidationPassed}, false},
		{"not_run", checkOutcome{Status: ValidationNotRun, Detail: "sandbox unreachable"}, false},
		{"not_applicable", checkOutcome{Status: ValidationNotApplicable}, false},
		{"unknown", checkOutcome{Status: ValidationUnknown}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := baselineAllowsRepair(c.in); got != c.want {
				t.Errorf("baselineAllowsRepair(%q) = %v, want %v", c.in.Status, got, c.want)
			}
		})
	}
}

// Landing unparseable bytes silently is the same as lying about them.
//
// The route above asserts the SERVER-SIDE verdict: applied + syntax + failed.
// Nothing asserted what the model is handed, and the model is the only party
// that can act on it. It was handed {"bytes_written":N} and nothing else,
// which reads as a clean write.
//
// Measured on multifile_cli rep2. The model rewrote test_store.py containing
// `store.add("Buy milk"")`, was told the write succeeded, ran pytest, could
// not reconcile the failure with a write it had been told was fine, and
// resent content until the repetition breaker closed the path. The file was
// still unparseable when the session ended, and the sandbox had been
// answering valid:false for those exact bytes the whole time.
func TestLandedUnparseableBytesTellTheModelSo(t *testing.T) {
	res, disk, _ := runDebugRoute(t, debugRouteOpts{
		rel: "mod.py", baseline: debugBaselineBroken, proposal: debugProposalBroken})

	if !res.Success || disk != debugProposalBroken {
		t.Fatalf("precondition: the repair attempt must land (success=%v)", res.Success)
	}
	var out WriteFileOutput
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatalf("write_file output is not WriteFileOutput: %v (%s)", err, res.Data)
	}
	if out.Warning == "" {
		t.Fatal("the model was told the write succeeded and nothing else, " +
			"over bytes the checker had just reported as invalid")
	}
	if !strings.Contains(out.Warning, "does not parse") {
		t.Errorf("warning does not say the file is broken: %q", out.Warning)
	}
	// The same fact, and it must not disagree with the server-side verdict.
	if res.ValidationStatus != ValidationFailed {
		t.Errorf("warned the model but recorded %q", res.ValidationStatus)
	}
}

// Negative control: a rewrite that parses must NOT be warned about.
func TestALandedParsingRewriteCarriesNoWarning(t *testing.T) {
	res, _, _ := runDebugRoute(t, debugRouteOpts{
		rel: "mod.py", baseline: debugBaselineBroken, proposal: debugProposalHealthy})
	if !res.Success {
		t.Fatalf("a parsing repair must land: %q", res.Error)
	}
	var out WriteFileOutput
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatalf("write_file output is not WriteFileOutput: %v (%s)", err, res.Data)
	}
	if out.Warning != "" {
		t.Errorf("a clean rewrite was warned about: %q", out.Warning)
	}
}
