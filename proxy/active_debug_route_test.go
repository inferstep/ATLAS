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

// writeRouteSyntaxErrorMark is the text the stub syntax check calls
// unparseable. It is content, not a file name, so a baseline or a proposal can
// carry it.
const writeRouteSyntaxErrorMark = "def area(radius:"

const writeRouteSyntaxDetail = "SyntaxError: invalid syntax (line 4)"

// writeRouteBaselineMark marks the content on disk, so a case can make the
// syntax check unavailable for the baseline alone.
const writeRouteBaselineMark = "# baseline of record"

// writeRouteServices stands in for the sandbox and the V3 service on the
// write routes. It counts every call and records any endpoint it does not know.
// The route tables here, in direct_write_route_test.go and in
// newfile_route_test.go use it.
type writeRouteServices struct {
	URL string

	mu              sync.Mutex
	syntaxCalls     int
	embeddedCalls   int
	structuralCalls int
	v3Calls         int
	unexpected      []string
}

// writeRouteCalls is a copy of the counts, taken under the lock.
type writeRouteCalls struct {
	syntax, embedded, structural, v3 int
	unexpected                       []string
}

func (s *writeRouteServices) calls() writeRouteCalls {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeRouteCalls{s.syntaxCalls, s.embeddedCalls, s.structuralCalls, s.v3Calls, s.unexpected}
}

// newWriteRouteServices starts the stub. The syntax check fails content that
// holds writeRouteSyntaxErrorMark and answers 500 for content that holds
// unavailableFor. The structural check reports `introduces` as unresolved in
// content that calls it.
func newWriteRouteServices(t *testing.T, unavailableFor, introduces string) *writeRouteServices {
	s := &writeRouteServices{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"):
			s.v3Calls++
			http.Error(w, "V3 generation reached", http.StatusTeapot)
		case r.URL.Path == "/internal/cyclomatic_complexity":
			// Tier refinement asks this; it is not a gate.
			json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
		case r.URL.Path == "/internal/embedded_script_check":
			s.embeddedCalls++
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "findings": []interface{}{}})
		case r.URL.Path == "/internal/structural_check":
			s.structuralCalls++
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			source, _ := body["source"].(string)
			out := map[string]interface{}{"ok": true, "unresolved": []string{}}
			if introduces != "" && strings.Contains(source, introduces+"(") {
				out["unresolved"] = []string{introduces}
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			s.syntaxCalls++
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if unavailableFor != "" && strings.Contains(in.Code, unavailableFor) {
				http.Error(w, "checker unavailable", http.StatusInternalServerError)
				return
			}
			valid := !strings.Contains(in.Code, writeRouteSyntaxErrorMark)
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{writeRouteSyntaxDetail}
			}
			json.NewEncoder(w).Encode(out)
		default:
			s.unexpected = append(s.unexpected, r.Method+" "+r.URL.Path)
			http.Error(w, "unexpected endpoint", http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// The active edit-test-fix route: write_file on a file the session wrote,
// right after a failed run that names it. V3 is skipped so the model can
// iterate at test speed, and the route runs the gates itself.
//
// Every Python fixture here is Tier2 and V3 is configured, so the active-debug
// predicate is the only thing that keeps V3 off. A /v3/ request fails the
// test, and TestActiveDebugNegativeControl flips one bit to show V3 taking over.

const debugBaselineHealthy = writeRouteBaselineMark + `
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

const debugBaselineBroken = writeRouteBaselineMark + `
import math


` + writeRouteSyntaxErrorMark + `
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


` + writeRouteSyntaxErrorMark + `
    if radius < 0:
        raise ValueError("negative radius")
    return math.pi * radius * radius


def perimeter(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return 2 * math.pi * radius
`

// Parses, and calls a name the file neither imports nor defines.
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

// Rust is the "no check applies" fixture: .rs has no sandbox syntax check and
// cannot carry a <script> block, while a real module of it is Tier2 on its own.
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

// activeDebugSession is a session that wrote rel and then ran it. The run's
// output is the last message and names rel. With runSucceeded false the
// session is in the edit-test-fix loop; with true it is one bit away from it.
func activeDebugSession(dir, rel, servicesURL string, runSucceeded bool) *AgentContext {
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	// Generation runs only where a candidate could be delivered. Declared
	// outputs make this session one, so V3 would run if the route allowed it.
	ctx.TaskContract = declaredOutputs(rel)
	ctx.StreamFn = func(string, interface{}) {}
	ctx.V3URL = servicesURL
	ctx.SandboxURL = servicesURL
	ctx.SessionWrites[rel] = true
	outcome := `"success":false`
	if runSucceeded {
		outcome = `"success":true`
	}
	ctx.Messages = []AgentMessage{
		{Role: "user", Content: "fix the failing module"},
		{Role: "assistant", Content: "running it again"},
		{Role: "tool", ToolName: "run_command", Content: `{` + outcome +
			`,"data":{"stderr":"Traceback (most recent call last):\n  File \"./` + filepath.Base(rel) +
			`\", line 4\nSyntaxError: invalid syntax"}}`},
	}
	return ctx
}

func TestActiveDebugRoute(t *testing.T) {
	if _, gated := syntaxGateLanguages[".rs"]; gated {
		t.Fatal(".rs gained a sandbox checker; pick a type neither check applies to")
	}
	if embeddedScriptExts[".rs"] {
		t.Fatal(".rs gained an embedded-script check; pick a type neither check applies to")
	}

	// Structural-check calls: a syntax refusal stops before that check,
	// other writes ask it once, and a structural refusal also asks about the
	// baseline.
	for _, tc := range []struct {
		name     string
		rel      string // the path as the model sends it
		baseline string // the file on disk before the write
		proposal string // the content the model sends

		noSandbox      bool   // no sandbox is configured, so no check can run
		unavailableFor string // the syntax check answers 500 for content holding this
		introduces     string // the structural check reports this name as unresolved
		fileGone       bool   // the session wrote the path, but the file is gone
		readOnlyDir    bool   // the folder is read-only, so the write itself fails

		wantSuccess         bool
		wantDisk            string
		wantMutation        MutationStatus
		wantKind            ValidationKind
		wantStatus          ValidationStatus
		wantSyntaxCalls     int
		wantStructuralCalls int
		wantError           string // the exact error, when set
		errorHas            string // a part of the error, when set
		wantDetail          string // the exact validation detail, when set
		detailHas           string // a part of the validation detail, when set
		warns               bool   // the model is told the landed file does not parse
	}{
		// A passing proposal is checked once; the baseline is never asked.
		{name: "proposal parses",
			rel: "mod.py", baseline: debugBaselineHealthy, proposal: debugProposalHealthy,
			wantSuccess: true, wantDisk: debugProposalHealthy,
			wantMutation: MutationApplied, wantKind: ValidationKindSyntax, wantStatus: ValidationPassed,
			wantSyntaxCalls: 1, wantStructuralCalls: 1},
		// The check applies and cannot run: the write fails open, and the
		// result says not_run, never passed.
		{name: "checker unavailable",
			rel: "mod.py", baseline: debugBaselineHealthy, proposal: debugProposalHealthy,
			noSandbox:   true,
			wantSuccess: true, wantDisk: debugProposalHealthy,
			wantMutation: MutationApplied, wantKind: ValidationKindSyntax, wantStatus: ValidationNotRun,
			wantSyntaxCalls: 0, wantStructuralCalls: 1},
		// Nothing here can be checked, which is a different fact from a
		// checker that was unavailable. Applicability is decided locally,
		// before any service is asked.
		{name: "no check applies",
			rel: "engine.rs", baseline: debugRustBaseline, proposal: debugRustProposal,
			wantSuccess: true, wantDisk: debugRustProposal,
			wantMutation: MutationApplied, wantKind: ValidationKindNone, wantStatus: ValidationNotApplicable,
			wantSyntaxCalls: 0, wantStructuralCalls: 0},
		// A repair of a file that already fails may land, and the result
		// says the landed bytes do not parse.
		{name: "failed proposal over a broken baseline lands",
			rel: "mod.py", baseline: debugBaselineBroken, proposal: debugProposalBroken,
			wantSuccess: true, wantDisk: debugProposalBroken,
			wantMutation: MutationApplied, wantKind: ValidationKindSyntax, wantStatus: ValidationFailed,
			wantSyntaxCalls: 2, wantStructuralCalls: 1, warns: true},
		{name: "parsing proposal over a broken baseline lands",
			rel: "mod.py", baseline: debugBaselineBroken, proposal: debugProposalHealthy,
			wantSuccess: true, wantDisk: debugProposalHealthy,
			wantMutation: MutationApplied, wantKind: ValidationKindSyntax, wantStatus: ValidationPassed,
			wantSyntaxCalls: 1, wantStructuralCalls: 1},
		{name: "failed proposal over a healthy baseline is refused",
			rel: "mod.py", baseline: debugBaselineHealthy, proposal: debugProposalBroken,
			wantSuccess: false, wantDisk: debugBaselineHealthy,
			wantMutation: MutationRefused, wantKind: ValidationKindSyntax, wantStatus: ValidationFailed,
			wantSyntaxCalls: 2, wantStructuralCalls: 0,
			wantError:  fallbackSyntaxRejection("mod.py", debugProposalBroken, writeRouteSyntaxDetail),
			wantDetail: writeRouteSyntaxDetail},
		// No evidence about the baseline is not evidence that it was broken,
		// so the failed proposal is refused.
		{name: "unavailable baseline check refuses a failed proposal",
			rel: "mod.py", baseline: debugBaselineHealthy, proposal: debugProposalBroken,
			unavailableFor: writeRouteBaselineMark,
			wantSuccess:    false, wantDisk: debugBaselineHealthy,
			wantMutation: MutationRefused, wantKind: ValidationKindSyntax, wantStatus: ValidationFailed,
			wantSyntaxCalls: 2, wantStructuralCalls: 0},
		// The only case that reaches the route's own syntax refusal: a failing
		// run removed the file, so the baseline reads as "", which parses.
		{name: "file gone, failed proposal is refused",
			rel: "mod.py", baseline: debugBaselineHealthy, proposal: debugProposalBroken,
			fileGone:    true,
			wantSuccess: false, wantDisk: "",
			wantMutation: MutationRefused, wantKind: ValidationKindSyntax, wantStatus: ValidationFailed,
			wantSyntaxCalls: 2, wantStructuralCalls: 0, wantDetail: writeRouteSyntaxDetail},
		// Bytes that were checked and passed can still fail to land.
		{name: "check passes, then the write fails",
			rel: "ro/mod.py", baseline: debugBaselineHealthy, proposal: debugProposalHealthy,
			readOnlyDir: true,
			wantSuccess: false, wantDisk: debugBaselineHealthy,
			wantMutation: MutationFailed, wantKind: ValidationKindSyntax, wantStatus: ValidationPassed,
			wantSyntaxCalls: 1, wantStructuralCalls: 1, errorHas: "cannot write"},
		// Syntax passed on these bytes and the structural check refused them,
		// so structural is the deciding verdict.
		{name: "structural refusal outranks a passing syntax check",
			rel: "mod.py", baseline: debugBaselineHealthy, proposal: debugProposalUnresolved,
			introduces:  "missing_helper",
			wantSuccess: false, wantDisk: debugBaselineHealthy,
			wantMutation: MutationRefused, wantKind: ValidationKindStructural, wantStatus: ValidationFailed,
			wantSyntaxCalls: 1, wantStructuralCalls: 2, errorHas: "missing_helper", detailHas: "missing_helper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			services := newWriteRouteServices(t, tc.unavailableFor, tc.introduces)
			dir := t.TempDir()
			target := filepath.Join(dir, tc.rel)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if !tc.fileGone {
				if err := os.WriteFile(target, []byte(tc.baseline), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ctx := activeDebugSession(dir, tc.rel, services.URL, false)
			if tc.noSandbox {
				ctx.SandboxURL = ""
			}
			if tc.readOnlyDir {
				folder := filepath.Dir(target)
				if err := os.Chmod(folder, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(folder, 0o755) })
			}
			if tier := classifyFileTier(tc.rel, tc.proposal); tier < Tier2Medium {
				t.Fatalf("the fixture must be Tier2 so only the predicate keeps V3 off, got %v", tier)
			}
			if !isActiveDebugIteration(ctx, tc.rel) {
				t.Fatal("the fixture is not on the active-debug route")
			}

			args, _ := json.Marshal(map[string]string{"path": tc.rel, "content": tc.proposal})
			res := executeToolCall("write_file", args, ctx)

			calls := services.calls()
			if len(calls.unexpected) > 0 {
				t.Fatalf("unexpected endpoints were called: %v", calls.unexpected)
			}
			if calls.v3 != 0 {
				t.Fatalf("V3 generation ran (%d calls); this is not the active-debug route", calls.v3)
			}
			if res.Success != tc.wantSuccess {
				t.Fatalf("success = %v, want %v (error %q)", res.Success, tc.wantSuccess, res.Error)
			}
			disk, _ := os.ReadFile(target)
			if string(disk) != tc.wantDisk {
				t.Errorf("disk holds %q, want %q", disk, tc.wantDisk)
			}
			entries, _ := os.ReadDir(filepath.Dir(target))
			for _, e := range entries {
				if strings.Contains(e.Name(), ".atlas.tmp") {
					t.Errorf("a temporary file survived: %s", e.Name())
				}
			}
			if res.MutationStatus != tc.wantMutation ||
				res.ValidationKind != tc.wantKind ||
				res.ValidationStatus != tc.wantStatus {
				t.Errorf("got %q/%q/%q, want %q/%q/%q",
					res.MutationStatus, res.ValidationKind, res.ValidationStatus,
					tc.wantMutation, tc.wantKind, tc.wantStatus)
			}
			if !res.Classified() {
				t.Error("result not fully classified")
			}
			if calls.syntax != tc.wantSyntaxCalls {
				t.Errorf("syntax-check calls = %d, want %d", calls.syntax, tc.wantSyntaxCalls)
			}
			if calls.structural != tc.wantStructuralCalls {
				t.Errorf("structural_check calls = %d, want %d", calls.structural, tc.wantStructuralCalls)
			}
			// No fixture here is a type that can carry an embedded script.
			if calls.embedded != 0 {
				t.Errorf("embedded_script_check calls = %d, want 0", calls.embedded)
			}
			if tc.wantError != "" && res.Error != tc.wantError {
				t.Errorf("error text:\n got %q\nwant %q", res.Error, tc.wantError)
			}
			if !strings.Contains(res.Error, tc.errorHas) {
				t.Errorf("error %q does not name %q", res.Error, tc.errorHas)
			}
			if tc.wantDetail != "" && res.ValidationDetail != tc.wantDetail {
				t.Errorf("validation detail = %q, want %q", res.ValidationDetail, tc.wantDetail)
			}
			if !strings.Contains(res.ValidationDetail, tc.detailHas) {
				t.Errorf("validation detail %q does not name %q", res.ValidationDetail, tc.detailHas)
			}
			// The model is told when the bytes it landed do not parse, and is
			// not warned about bytes that do.
			if tc.wantSuccess {
				var out WriteFileOutput
				if err := json.Unmarshal(res.Data, &out); err != nil {
					t.Fatalf("write_file output is not WriteFileOutput: %v (%s)", err, res.Data)
				}
				if tc.warns && !strings.Contains(out.Warning, "does not parse") {
					t.Errorf("the model was not told the landed file does not parse: %q", out.Warning)
				}
				if !tc.warns && out.Warning != "" {
					t.Errorf("a write that needs no warning carries one: %q", out.Warning)
				}
			}
		})
	}
}

// With the last run passed, the same write goes to V3. Without this control
// the route cases could be passing on some unrelated path.
func TestActiveDebugNegativeControl(t *testing.T) {
	services := newWriteRouteServices(t, "", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mod.py"), []byte(debugBaselineHealthy), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := activeDebugSession(dir, "mod.py", services.URL, true)
	if isActiveDebugIteration(ctx, "mod.py") {
		t.Fatal("a passed run must not put the session on the active-debug route")
	}

	args, _ := json.Marshal(map[string]string{"path": "mod.py", "content": debugProposalHealthy})
	executeToolCall("write_file", args, ctx)

	calls := services.calls()
	if len(calls.unexpected) > 0 {
		t.Fatalf("unexpected endpoints were called: %v", calls.unexpected)
	}
	if calls.v3 == 0 {
		t.Fatal("with the predicate false the Tier2 write must reach V3 generation")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".atlas.tmp") {
			t.Errorf("a temporary file survived: %s", e.Name())
		}
	}
}

// Only a demonstrated baseline failure unlocks a failed proposal. Passed,
// not_run, not_applicable and unknown all refuse.
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
