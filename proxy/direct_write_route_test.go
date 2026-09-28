package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The ordinary existing-file direct write: the model rewrites a file this
// session already wrote, and it is NOT mid edit-test-fix loop. Tier1 content
// keeps it off the V3 branch even with V3 configured, which is what makes the
// routing proof mean something -- V3URL is non-empty in every fixture and a
// /v3/generate request fails the test.
//
// The observation is made once, by the shared existing-file regression gate,
// and carried to the result. The final write block re-runs nothing: the
// request counts below are what pin that, since a recomputed check would show
// up as an extra syntax-check request on exactly the same bytes.

const directSyntaxMark = "def area(radius:"
const directSyntaxDetail = "SyntaxError: invalid syntax (line 3)"
const directBaselineMark = "# baseline of record"

const directBaselineHealthy = directBaselineMark + `
import math

RADIUS = 2.0
AREA = math.pi * RADIUS * RADIUS
`

const directBaselineBroken = directBaselineMark + `
import math

` + directSyntaxMark + `
`

const directProposalHealthy = `import math

RADIUS = 2.0
AREA = math.pi * RADIUS * RADIUS
CIRCUMFERENCE = 2 * math.pi * RADIUS
`

const directProposalBroken = `import math

` + directSyntaxMark + `
    return math.pi * radius * radius
`

// Parses, and calls a name the file neither imports nor defines.
const directProposalUnresolved = `import math

RADIUS = 2.0
AREA = missing_helper(RADIUS)
`

type directWriteStub struct {
	mu             sync.Mutex
	syntaxCode     []string
	embeddedCalls  int
	structuralCode []string
	v3Calls        int
	unexpected     []string
}

func (s *directWriteStub) counts() (syntax, embedded, structural int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.syntaxCode), s.embeddedCalls, len(s.structuralCode)
}

type directWriteOpts struct {
	rel      string // path as the model sends it
	baseline string // content on disk before the write
	proposal string // incoming content

	unavailableFor string       // content carrying this gets a non-200 from /syntax-check
	introduces     string       // structural_check reports this symbol as newly unresolved
	lastTool       AgentMessage // most recent tool message; zero value means a read_file
	before         func(t *testing.T, dir string)
}

// runDirectWrite dispatches one write_file through the real tool path and
// returns the result, the bytes on disk afterwards, the stub's record, and
// every SSE projection the route produced.
func runDirectWrite(t *testing.T, o directWriteOpts) (*ToolResult, string, *directWriteStub, []string) {
	t.Helper()
	dir := t.TempDir()
	st := &directWriteStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"):
			st.mu.Lock()
			st.v3Calls++
			st.mu.Unlock()
			http.Error(w, "V3 generation reached", http.StatusTeapot)
		case r.URL.Path == "/internal/cyclomatic_complexity":
			json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
		case r.URL.Path == "/internal/embedded_script_check":
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
			valid := !strings.Contains(in.Code, directSyntaxMark)
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{directSyntaxDetail}
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
	if err := os.WriteFile(target, []byte(o.baseline), 0o644); err != nil {
		t.Fatal(err)
	}

	var events []string
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(event string, _ interface{}) { events = append(events, event) }
	ctx.V3URL = srv.URL
	ctx.SandboxURL = srv.URL
	// Session-owned: the model may rewrite its own draft.
	ctx.SessionWrites[o.rel] = true
	last := o.lastTool
	if last.Role == "" {
		// The ordinary shape: the model read the file and is rewriting it.
		last = AgentMessage{Role: "tool", ToolName: "read_file", Content: o.baseline}
	}
	ctx.Messages = []AgentMessage{
		{Role: "user", Content: "extend the module"},
		{Role: "assistant", Content: "rewriting it"},
		last,
	}

	// --- route identity, pinned rather than assumed -------------------------
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("this route needs an EXISTING destination: %v", err)
	}
	if !ctx.SessionWrites[o.rel] {
		t.Fatal("the session must own the file")
	}
	if isActiveDebugIteration(ctx, o.rel) {
		t.Fatal("the active-debug predicate must be FALSE on this route")
	}
	// What holds V3 off is the tier and only the tier: the fixture is Tier1,
	// so the ordinary direct route is selected with V3 configured.
	if tier := classifyFileTier(o.rel, o.proposal); tier >= Tier2Medium {
		t.Fatalf("fixture must be Tier1 so the ordinary direct route is selected "+
			"with V3 configured, got %v", tier)
	}
	if ctx.V3URL == "" {
		t.Fatal("V3URL must be non-empty for the routing proof")
	}

	if o.before != nil {
		o.before(t, dir)
	}
	args, _ := json.Marshal(map[string]string{"path": o.rel, "content": o.proposal})
	res := executeToolCall("write_file", args, ctx)

	if len(st.unexpected) > 0 {
		t.Fatalf("unexpected endpoints were called: %v", st.unexpected)
	}
	if st.v3Calls != 0 {
		t.Fatalf("V3 generation ran (%d calls); this is not the ordinary direct route", st.v3Calls)
	}
	after, _ := os.ReadFile(target)
	ents, _ := os.ReadDir(filepath.Dir(target))
	for _, e := range ents {
		if strings.Contains(e.Name(), ".atlas.tmp") {
			t.Errorf("temporary artifact survived: %s", e.Name())
		}
	}
	// SSE projection: this route emits nothing of its own. Only the V3 branch
	// streams (v3_progress / v3_token), and reaching it here would be a
	// routing failure, not a new event.
	if len(events) != 0 {
		t.Errorf("SSE projections changed: %v", events)
	}
	return res, string(after), st, events
}

// 1. The proposal parses: the write lands carrying what the gate observed,
// and the observation is not restated with a second request.
func TestDirectWriteProposalPasses(t *testing.T) {
	res, disk, st, _ := runDirectWrite(t, directWriteOpts{
		rel: "draft.py", baseline: directBaselineHealthy, proposal: directProposalHealthy})

	if !res.Success || disk != directProposalHealthy {
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
	syntax, embedded, _ := st.counts()
	if syntax != 1 || embedded != 0 {
		t.Errorf("checker calls = %d syntax / %d embedded, want 1/0 -- the "+
			"regression gate's observation is carried, never recomputed",
			syntax, embedded)
	}
}

// 2. The check applied and could not run. Fail-open is unchanged, and not_run
// is reported rather than the stronger claim.
func TestDirectWriteCheckerUnavailable(t *testing.T) {
	res, disk, st, _ := runDirectWrite(t, directWriteOpts{
		rel: "draft.py", baseline: directBaselineHealthy, proposal: directProposalHealthy,
		unavailableFor: "CIRCUMFERENCE"})

	if !res.Success || disk != directProposalHealthy {
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
	if syntax, _, _ := st.counts(); syntax != 1 {
		t.Errorf("syntax-check calls = %d, want exactly 1 -- one attempt, "+
			"whose failure to run IS the observation", syntax)
	}
}

// 3. Neither check applies. Applicability is decided locally, so no service is
// asked anything at all.
func TestDirectWriteNoCheckApplies(t *testing.T) {
	const notes = "release notes\nsecond line\n"
	if _, gated := syntaxGateLanguages[".txt"]; gated {
		t.Fatal(".txt gained a sandbox checker; pick a type neither check applies to")
	}
	if embeddedScriptExts[".txt"] {
		t.Fatal(".txt gained an embedded-script check; pick a type neither check applies to")
	}
	res, disk, st, _ := runDirectWrite(t, directWriteOpts{
		rel: "NOTES.txt", baseline: "release notes\n", proposal: notes})

	if !res.Success || disk != notes {
		t.Fatalf("write did not land: success=%v disk=%q", res.Success, disk)
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
	syntax, embedded, structural := st.counts()
	if syntax != 0 || embedded != 0 || structural != 0 {
		t.Errorf("checker calls = %d syntax / %d embedded / %d structural, want 0/0/0",
			syntax, embedded, structural)
	}
}

// 4. Both sides demonstrably fail: the repair-in-progress carveout stands, and
// the landed bytes are reported as not parsing.
func TestDirectWriteFailedProposalOverBrokenBaselineLands(t *testing.T) {
	res, disk, st, _ := runDirectWrite(t, directWriteOpts{
		rel: "draft.py", baseline: directBaselineBroken, proposal: directProposalBroken})

	if !res.Success {
		t.Fatalf("a repair attempt on already-broken content must land: %q", res.Error)
	}
	if disk != directProposalBroken {
		t.Fatalf("bytes on disk are not the proposal: %q", disk)
	}
	if res.MutationStatus != MutationApplied ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationFailed {
		t.Errorf("got %q/%q/%q, want applied/syntax/failed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if res.ValidationStatus.Passed() {
		t.Error("landed bytes that do not parse must never read as passed")
	}
	if syntax, _, _ := st.counts(); syntax != 2 {
		t.Errorf("syntax-check calls = %d, want exactly 2 (proposal, then baseline once)", syntax)
	}
}

// 5. A working file is protected: the regression is refused and the bytes on
// disk are untouched.
func TestDirectWriteFailedProposalOverHealthyBaselineIsRefused(t *testing.T) {
	res, disk, st, _ := runDirectWrite(t, directWriteOpts{
		rel: "draft.py", baseline: directBaselineHealthy, proposal: directProposalBroken})

	if res.Success {
		t.Fatal("a newly introduced syntax error must be refused")
	}
	if disk != directBaselineHealthy {
		t.Fatalf("refused write changed the file: %q", disk)
	}
	if want := fallbackSyntaxRejection("draft.py", directProposalBroken, directSyntaxDetail); res.Error != want {
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
	if res.ValidationDetail != directSyntaxDetail {
		t.Errorf("ValidationDetail = %q, want the checker's finding", res.ValidationDetail)
	}
	if syntax, _, _ := st.counts(); syntax != 2 {
		t.Errorf("syntax-check calls = %d, want exactly 2", syntax)
	}
}

// 6. Absence of baseline evidence is not evidence that the file was already
// broken, so the failed proposal is refused just as a passing baseline refuses.
func TestDirectWriteUnavailableBaselineRefusesFailedProposal(t *testing.T) {
	res, disk, st, _ := runDirectWrite(t, directWriteOpts{
		rel: "draft.py", baseline: directBaselineHealthy, proposal: directProposalBroken,
		unavailableFor: directBaselineMark})

	if res.Success {
		t.Fatal("an unverifiable baseline must not unlock the failed proposal")
	}
	if disk != directBaselineHealthy {
		t.Fatalf("refused write changed the file: %q", disk)
	}
	if res.MutationStatus != MutationRefused ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationFailed {
		t.Errorf("got %q/%q/%q, want refused/syntax/failed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if syntax, _, _ := st.counts(); syntax != 2 {
		t.Errorf("syntax-check calls = %d, want exactly 2", syntax)
	}
}

// 7. Mutation failure and validation success are orthogonal: bytes that were
// checked and passed can still fail to land, and the prior file survives.
func TestDirectWritePassedThenWriteFailure(t *testing.T) {
	res, disk, st, _ := runDirectWrite(t, directWriteOpts{
		rel: "ro/draft.py", baseline: directBaselineHealthy, proposal: directProposalHealthy,
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
	if disk != directBaselineHealthy {
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
	if syntax, _, _ := st.counts(); syntax != 1 {
		t.Errorf("syntax-check calls = %d, want exactly 1", syntax)
	}
}

// 8. Syntax passed on these exact bytes and the structural gate refused them,
// so structural is the decisive outcome and the syntax verdict is not restated
// as a failure.
func TestDirectWriteStructuralRefusalOutranksPassingSyntax(t *testing.T) {
	res, disk, st, _ := runDirectWrite(t, directWriteOpts{
		rel: "draft.py", baseline: directBaselineHealthy, proposal: directProposalUnresolved,
		introduces: "missing_helper"})

	if res.Success {
		t.Fatal("the structural gate did not refuse")
	}
	if disk != directBaselineHealthy {
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
	syntax, _, structural := st.counts()
	if syntax != 1 {
		t.Errorf("syntax-check calls = %d, want exactly 1 -- a passing proposal "+
			"never consults the baseline", syntax)
	}
	if structural < 2 {
		t.Errorf("structural_check calls = %d, want both the edited and original "+
			"sides; the gate cannot refuse on one", structural)
	}
}

// 9. Active-debug near miss. The session owns the file and the most recent
// tool message IS a failed run -- but it names webapp.py, and app.py is not a
// whole filename token inside it. The predicate is therefore false and this
// ordinary route runs, with V3 never reached.
func TestDirectWriteActiveDebugNearMiss(t *testing.T) {
	failedRun := AgentMessage{Role: "tool", ToolName: "run_command",
		Content: `{"success":false,"data":{"stderr":"  File \"./webapp.py\", line 3\nSyntaxError"}}`}
	if mentionsFilename(failedRun.Content, "app.py") {
		t.Fatal("fixture is not a near miss: the run names app.py as a whole token")
	}
	res, disk, st, _ := runDirectWrite(t, directWriteOpts{
		rel: "app.py", baseline: directBaselineHealthy, proposal: directProposalHealthy,
		lastTool: failedRun})

	// The fixture already asserted isActiveDebugIteration false and zero V3
	// calls; what remains is that the ordinary route classified the result.
	if !res.Success || disk != directProposalHealthy {
		t.Fatalf("write did not land: success=%v", res.Success)
	}
	if res.MutationStatus != MutationApplied ||
		res.ValidationKind != ValidationKindSyntax ||
		res.ValidationStatus != ValidationPassed {
		t.Errorf("got %q/%q/%q, want applied/syntax/passed",
			res.MutationStatus, res.ValidationKind, res.ValidationStatus)
	}
	if syntax, _, _ := st.counts(); syntax != 1 {
		t.Errorf("syntax-check calls = %d, want exactly 1", syntax)
	}
}

// Structural pin on the producers themselves. Eight structured evaluations
// exist in tools.go, and every one of them is a case some other gate did not
// already answer:
//
//	regression gate     proposal + baseline   existing destination
//	V3 preflight        proposal + baseline   new destination / unread baseline
//	active-debug branch proposal + baseline   destination gone since the write
//	new-file gate       proposal              sub-Tier2 or unconfigured V3
//	V3 service fallback baseline              after generation failed, deliberately
//	                                          fresh: minutes may have passed
//	V3 delivery         final bytes           the artifact the pipeline delivers,
//	                                          after sanitisation
//	V3 delivery         baseline              the alternative, when the candidate
//	                                          fails its own final-byte check
//	V3 revocations      baseline              structural and embedded, before the
//	                                          baseline they restore is written
//	V3 terminal guard   final bytes           re-observes only if some branch
//	                                          changed the bytes without saying so
//	edit gate           proposal + baseline   shared by edit_file, insert_after
//	                                          and replace_lines, once each
//
// None is in the final direct-write block. Growth beyond this means a route
// recomputed an observation it already held.
func TestWriteRoutesDoNotRecomputeTheirObservation(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "tools.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var structured, legacy int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch id.Name {
		case "fallbackSyntaxOutcomeFor":
			structured++
		case "checkFallbackSyntax":
			legacy++
		}
		return true
	})
	// 18 since structural_edit began observing the bytes it splices. Every
	// site is beside the bytes it describes; a site that re-derived an
	// observation a route already holds would be the recomputation this
	// counts against. structural_edit held none: it recorded not_run for its
	// own output, which is why a file it edited carried mutation debt that
	// nothing could discharge. Asking once, about bytes no one had asked
	// about, is the opposite of recomputation.
	if structured != 18 {
		t.Errorf("fallbackSyntaxOutcomeFor call sites = %d, want 18; a new one on a "+
			"route that already holds an observation is a recomputation", structured)
	}
	// The migration is complete: no route calls the legacy wrapper any more.
	// It survives as one definition in gates.go, whose own tests still cover
	// it, and TestLegacyCheckerWrapperInventory names what is left.
	if legacy != 0 {
		t.Errorf("checkFallbackSyntax call sites = %d, want 0 -- every route now "+
			"uses the structured producer", legacy)
	}
}
