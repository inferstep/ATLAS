package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// AUDIT of the SUCCESSFUL writeFileWithV3 state machine: V3 answered, and the
// route now decides which bytes land, what evidence covers those exact bytes,
// and whether V3 provenance is authorized.
//
// These fixtures RECORD; they add no classification. Every one runs the real
// write_file dispatch, so what they show is routing, not a reading of the
// source. The stub hashes the body of every check request, which is what makes
// "the evidence covers the bytes that landed" a measured claim rather than an
// assumption: a check whose hash is not the final on-disk hash did not examine
// what the model got.
//
// Terminology, kept apart on purpose:
//
//	authorization  may the candidate's BYTES replace the caller's content
//	evidence       what a checker demonstrated, and about WHICH bytes
//	local gates    the syntax/structural/embedded checks this route runs
//	provenance     the V3 metadata attached to the delivered artifact

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

type auditCheck struct {
	kind string // "syntax" | "structural" | "embedded"
	hash string
}

type auditStub struct {
	mu     sync.Mutex
	checks []auditCheck
	v3Hits int
}

func (s *auditStub) add(kind, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = append(s.checks, auditCheck{kind, shortHash(body)})
}

func (s *auditStub) snapshot() []auditCheck {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]auditCheck(nil), s.checks...)
}

// verifiedEnvelopeFor re-stamps the golden verified-winner envelope onto other
// bytes. The shape stays the one the real Python serialiser produced -- the
// golden document is still the only description of the wire -- while the
// hashes name the candidate this fixture is about.
func verifiedEnvelopeFor(t *testing.T, code string) map[string]interface{} {
	t.Helper()
	var payload struct {
		Evidence map[string]interface{} `json:"evidence"`
	}
	if err := json.Unmarshal(readFixture(t, "01_verified_winner"), &payload); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(code))
	h := hex.EncodeToString(sum[:])
	payload.Evidence["identity"].(map[string]interface{})["candidate_content_hash"] = h
	delivery := payload.Evidence["delivery"].(map[string]interface{})
	delivery["delivered_content_hash"] = h
	delivery["describes_delivered_candidate"] = true
	return payload.Evidence
}

type auditCase struct {
	name      string
	rel       string
	prior     string // on-disk content before the write ("" = absent)
	baseline  string // what the MODEL sends to write_file
	candidate string
	passed    bool // V3's top-level passed flag

	// Verdicts take the request's ordinal as well as its body: several
	// transitions ask the SAME bytes twice (the preflight before generation,
	// a gate after it), and the second answer is the one under audit.
	unresolvedFor func(src string) []string // structural_check verdict
	// embeddedBadFor also receives `previous`, because the service reports a
	// class of defect only a before/after comparison can see (a render loop the
	// edit stopped driving). That class is exactly what the embedded GATE can
	// still catch after the previous-less final-byte check has passed.
	embeddedBadFor     func(src, previous string, call int) bool
	syntaxBadFor       func(src string, call int) bool // /syntax-check verdict
	cancelOnStructural bool
	readOnlyDir        bool
	// noEvidence sends the legacy response shape: passed=true with no
	// envelope. The envelope authorizes nothing either way now; the field
	// stays because a response without one is still a shape the bridge has to
	// read.
	noEvidence bool
	// declaredCommand makes the client require a command the audit's executor
	// never answers, so the candidate's floor goes unmet and the proxy refuses
	// it. It is how this walk still covers a genuine refusal now that a
	// service verdict cannot cause one.
	declaredCommand string
}

type auditResult struct {
	res    *ToolResult
	disk   string
	checks []auditCheck
	events []string
}

func runAudit(t *testing.T, c auditCase) auditResult {
	t.Helper()
	dir := t.TempDir()
	st := &auditStub{}
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var syntaxCalls, embeddedCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v3/generate":
			st.mu.Lock()
			st.v3Hits++
			st.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			body := map[string]interface{}{
				"code": c.candidate, "passed": c.passed,
				"phase_solved": "phase1", "candidates_tested": 3,
				"winning_score": 0.87,
				"verification_evidence": []map[string]interface{}{
					{"verifier": "sandbox", "status": "passed"},
				},
			}
			// A service that verified this candidate says so in the envelope.
			// Without one nothing is authorized, which several cases below
			// exercise deliberately.
			if c.passed && !c.noEvidence {
				body["evidence"] = verifiedEnvelopeFor(t, c.candidate)
			}
			payload, _ := json.Marshal(body)
			for _, line := range []string{"event: result", "data: " + string(payload), "", "data: [DONE]", ""} {
				fmt.Fprint(w, line+"\n")
				if fl != nil {
					fl.Flush()
				}
			}
		case r.URL.Path == "/internal/cyclomatic_complexity":
			json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
		case r.URL.Path == "/internal/structural_check":
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			src, _ := body["source"].(string)
			st.add("structural", src)
			if c.cancelOnStructural {
				cancel()
			}
			out := map[string]interface{}{"ok": true, "unresolved": []string{}}
			if c.unresolvedFor != nil {
				if names := c.unresolvedFor(src); len(names) > 0 {
					out["unresolved"] = names
				}
			}
			json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/internal/embedded_script_check":
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			src, _ := body["source"].(string)
			prev, _ := body["previous"].(string)
			st.add("embedded", src)
			embeddedCalls++
			out := map[string]interface{}{"ok": true, "findings": []interface{}{}}
			if c.embeddedBadFor != nil && c.embeddedBadFor(src, prev, embeddedCalls) {
				out["findings"] = []map[string]interface{}{{
					"line": 4, "column": 1, "kind": "javascript",
					"where": "the <script> block", "message": "unexpected `)`",
					"text": "bad();)", "hint": "remove it",
				}}
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			st.add("syntax", in.Code)
			syntaxCalls++
			bad := c.syntaxBadFor != nil && c.syntaxBadFor(in.Code, syntaxCalls)
			out := map[string]interface{}{"valid": !bad}
			if bad {
				out["errors"] = []string{"SyntaxError: invalid syntax (line 2)"}
			}
			json.NewEncoder(w).Encode(out)
		default:
			http.Error(w, "unexpected endpoint "+r.URL.Path, http.StatusTeapot)
		}
	}))
	defer srv.Close()

	target := filepath.Join(dir, c.rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if c.prior != "" {
		if err := os.WriteFile(target, []byte(c.prior), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var events []string
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(event string, _ interface{}) { events = append(events, event) }
	ctx.Ctx = context.WithValue(reqCtx, requestIDKey, "req-audit")
	// The client declares the artifact it asked for. Without that there is no
	// target to authorize against, no licence to mint and no candidate
	// delivery to walk -- which is the point of this audit.
	contract := `{"task_mode":"work","output_knowledge":"declared","expected_outputs":[` +
		mustJSONString(c.rel) + `]`
	if c.declaredCommand != "" {
		contract += `,"verification_knowledge":"declared","verification":[` +
			mustJSONString(c.declaredCommand) + `]`
	}
	ctx.TaskContract = mustContract(t, dir, contract+`}`)
	ctx.V3URL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.SessionWrites[c.rel] = true
	ctx.Messages = []AgentMessage{
		{Role: "user", Content: "build it"},
		{Role: "tool", ToolName: "read_file", Content: c.prior},
	}

	// Routing premise: only a Tier2+ file with V3 configured, unbypassed and
	// outside an edit-test-fix loop reaches the pipeline at all.
	if tier := classifyFileTier(c.rel, c.baseline); tier < Tier2Medium {
		t.Fatalf("fixture must be Tier2+ to enter the pipeline, got %v", tier)
	}
	if isActiveDebugIteration(ctx, c.rel) {
		t.Fatal("the active-debug predicate must be false, or the fast path takes this write")
	}

	if c.readOnlyDir {
		sub := filepath.Dir(target)
		if err := os.Chmod(sub, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(sub, 0o755) })
	}

	args, _ := json.Marshal(map[string]string{"path": c.rel, "content": c.baseline})
	res := executeToolCall("write_file", args, ctx)

	if st.v3Hits == 0 {
		t.Fatal("V3 generation was never entered; this is not the pipeline route")
	}
	after, _ := os.ReadFile(target)
	ents, _ := os.ReadDir(filepath.Dir(target))
	for _, e := range ents {
		if strings.Contains(e.Name(), ".atlas.tmp") {
			t.Errorf("temporary artifact survived: %s", e.Name())
		}
	}
	return auditResult{res: res, disk: string(after), checks: st.snapshot(), events: events}
}

// ---------------------------------------------------------------------------
// Fixture content
// ---------------------------------------------------------------------------

const auBaselinePy = `import math


def area(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return math.pi * radius * radius


def perimeter(radius):
    for _ in range(1):
        pass
    return 2 * math.pi * radius
`

const auCandidatePy = `import math


def area(radius):
    if radius < 0:
        raise ValueError("bad radius")
    return math.pi * radius ** 2


def perimeter(radius):
    while False:
        pass
    return math.tau * radius
`

// A BASELINE that itself calls a name the file never binds: the model's own
// content is not automatically the safe option.
const auBaselineUnresolved = `import math


def area(radius):
    if radius < 0:
        raise ValueError("negative radius")
    return base_helper(radius)


def perimeter(radius):
    for _ in range(1):
        pass
    return 2 * math.pi * radius
`

// A candidate that calls a name the file never binds.
const auCandidateUnresolved = `import math


def area(radius):
    if radius < 0:
        raise ValueError("bad radius")
    return missing_helper(radius)


def perimeter(radius):
    while False:
        pass
    return math.tau * radius
`

// A candidate carrying a second module entrypoint.
const auCandidateDoubleMain = auCandidatePy + `

if __name__ == "__main__":
    print(area(1))


if __name__ == "__main__":
    print(perimeter(1))
`

const auBaselineHTML = `<!doctype html>
<html>
<head><title>Board</title></head>
<body>
  <div id="board"></div>
  <script>
    const cells = [];
    for (let i = 0; i < 9; i++) { cells.push(i); }
    document.getElementById('board').textContent = cells.join(',');
  </script>
</body>
</html>
`

// What is already on disk: healthy, and not byte-identical to the model's
// proposal (an echoed write is refused before the pipeline is ever entered).
const auPriorHTML = `<!doctype html>
<html>
<head><title>Board</title></head>
<body>
  <div id="board"></div>
  <script>
    const cells = [];
    for (let i = 0; i < 4; i++) { cells.push(i); }
    document.getElementById('board').textContent = cells.join('-');
  </script>
</body>
</html>
`

const auCandidateHTML = `<!doctype html>
<html>
<head><title>Board</title></head>
<body>
  <div id="board"></div>
  <script>
    const cells = [];
    for (let i = 0; i < 16; i++) { cells.push(i * 2); }
    document.getElementById('board').textContent = cells.join(' ');
  </script>
</body>
</html>
`

// No tags at all: the language-swap shape that replaced an HTML document with
// a pile of JavaScript.
const auCandidateNotHTML = `const cells = [];
for (let i = 0; i < 16; i++) {
  cells.push(i * 2);
}
console.log(cells.join(' '));
const board = cells.map(function (c) { return c + 1; });
console.log(board.length);
if (board.length > 0) { console.log('ok'); }
for (const c of board) { console.log(c); }
`

// ---------------------------------------------------------------------------
// The trace
// ---------------------------------------------------------------------------

func TestV3SuccessStateMachineAudit(t *testing.T) {
	for _, c := range auditCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := runAudit(t, c)
			sanitized, wasSanitized := sanitizeFileContent(c.rel, c.candidate)

			label := func(h string) string {
				switch h {
				case shortHash(c.baseline):
					return "BASE(" + h + ")"
				case shortHash(c.candidate):
					return "CAND(" + h + ")"
				case shortHash(sanitized):
					return "SANI(" + h + ")"
				case shortHash(""):
					return "EMPTY"
				case shortHash(c.prior):
					return "PRIOR(" + h + ")"
				}
				return "OTHER(" + h + ")"
			}
			var checkLines []string
			for _, ch := range got.checks {
				checkLines = append(checkLines, ch.kind+":"+label(ch.hash))
			}
			finalOnDisk := "NOTHING"
			if got.disk != "" {
				finalOnDisk = label(shortHash(got.disk))
			}
			// authorizedV3 and fellBack are internal; provenance is their
			// observable projection, which is the property under audit.
			t.Logf("\n"+
				"  baseline        %s\n"+
				"  candidate       %s (passed=%v)\n"+
				"  sanitized       %s (changed=%v)\n"+
				"  final on disk   %s\n"+
				"  checks          %v\n"+
				"  success         %v\n"+
				"  mutation/valid  %q / %q %q\n"+
				"  provenance      v3_used=%v candidates=%d score=%.2f phase=%q evidence=%d\n"+
				"  sse             %v\n"+
				"  error           %s",
				label(shortHash(c.baseline)),
				label(shortHash(c.candidate)), c.passed,
				label(shortHash(sanitized)), wasSanitized,
				finalOnDisk, checkLines,
				got.res.Success,
				got.res.MutationStatus, got.res.ValidationKind, got.res.ValidationStatus,
				got.res.V3Used, got.res.CandidatesTested, got.res.WinningScore,
				got.res.PhaseSolved, len(got.res.VerificationEvidence),
				got.events, truncateStr(got.res.Error, 90))

			// THE INVARIANT: every artifact this state machine delivers carries
			// a structured syntax observation of the exact bytes on disk. A
			// syntax-gated file that landed without a check of its final hash
			// means the delivered artifact was never examined.
			// "Delivered" means this route wrote something, not that a file
			// happens to exist: a refusal leaves the prior artifact in place.
			if got.res.Success {
				if _, gated := syntaxGateLanguages[strings.ToLower(filepath.Ext(c.rel))]; gated {
					seen := false
					for _, ch := range got.checks {
						if ch.kind == "syntax" && ch.hash == shortHash(got.disk) {
							seen = true
						}
					}
					if !seen {
						t.Errorf("delivered %s with no syntax check of those bytes; "+
							"checks = %v", label(shortHash(got.disk)), checkLines)
					}
				}
				if !got.res.ValidationStatus.Classified() {
					t.Errorf("delivered bytes with an unclassified validation status")
				}
				if got.res.MutationStatus != MutationApplied {
					t.Errorf("MutationStatus = %q, want applied", got.res.MutationStatus)
				}
			}

			// The one invariant this audit exists to protect: provenance may
			// only describe bytes V3 authored and this route delivered.
			if got.res.V3Used {
				if got.disk != sanitized {
					t.Errorf("V3 provenance attached to bytes that are not the "+
						"delivered candidate: disk=%s sanitized=%s",
						label(shortHash(got.disk)), label(shortHash(sanitized)))
				}
			}
			if got.disk == c.baseline && got.res.V3Used {
				t.Error("baseline bytes were delivered with V3 provenance")
			}
			if !got.res.Success && got.res.V3Used {
				t.Error("a non-success result carries V3 provenance")
			}
		})
	}
}

func auditCases() []auditCase {
	unresolvedIn := func(marker string) func(string) []string {
		return func(src string) []string {
			if strings.Contains(src, marker+"(") {
				return []string{marker}
			}
			return nil
		}
	}
	return []auditCase{
		// 1. Non-passing response that still carries code. Authorization is
		// Passed, not the presence of Code.
		{name: "01_unauthorized_code_retained_baseline",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: false,
			declaredCommand: "pytest -q"},

		// 2. Authorized candidate survives every gate.
		{name: "02_authorized_candidate_delivered",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: true},

		// 3. Language-swap gate revokes to the baseline.
		{name: "03_language_swap_revoked",
			rel: "index.html", baseline: auBaselineHTML, candidate: auCandidateNotHTML, passed: true},

		// 4. Sanitization rewrites an authorized candidate after V3 verified it.
		{name: "04_sanitization_rewrites_candidate",
			rel: "solve.py", baseline: auBaselinePy,
			candidate: "```python\n" + auCandidatePy + "```\n", passed: true,
			declaredCommand: "pytest -q"},

		// 5. Structural gate rejects the candidate; the baseline is clean.
		{name: "05_structural_revokes_to_clean_baseline",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidateUnresolved,
			passed: true, unresolvedFor: unresolvedIn("missing_helper")},

		// 5b. Same, but the baseline does not parse: the remaining legacy
		// checkFallbackSyntax call decides, and nothing lands.
		{name: "05b_structural_revocation_blocked_by_broken_baseline",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidateUnresolved,
			passed: true, unresolvedFor: unresolvedIn("missing_helper"),
			// The preflight (call 1) passes the baseline; the revocation's own
			// re-check (call 2) finds it broken. Only a sequenced stub can
			// separate those two, which is the point of the ordinal.
			syntaxBadFor: func(src string, call int) bool {
				return call > 1 && src == auBaselinePy
			}},

		// 4b. The sanitized candidate does not parse; the baseline does.
		{name: "04b_sanitized_candidate_fails_baseline_clean",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: true,
			syntaxBadFor: func(src string, _ int) bool { return src == auCandidatePy }},

		// 4c. Neither the sanitized candidate nor the baseline parses.
		{name: "04c_sanitized_candidate_and_baseline_fail",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: true,
			// The preflight (call 1) passes the baseline; every later check fails.
			syntaxBadFor: func(_ string, call int) bool { return call > 1 }},

		// 6. Structural gate finds BOTH unacceptable.
		{name: "06_structural_refuses_both",
			rel: "solve.py", baseline: auBaselineUnresolved, candidate: auCandidateUnresolved,
			passed: true, unresolvedFor: func(src string) []string {
				var names []string
				for _, n := range []string{"missing_helper", "base_helper"} {
					if strings.Contains(src, n+"(") {
						names = append(names, n)
					}
				}
				return names
			}},

		// 7. Embedded-script gate revokes the candidate to the baseline.
		// The finding is previous-dependent, which is what keeps this
		// transition reachable: the final-byte check asks previous-less and
		// sees nothing, and the gate's before/after comparison is what refuses.
		{name: "07_embedded_revokes_to_baseline",
			rel: "index.html", prior: auPriorHTML,
			baseline: auBaselineHTML, candidate: auCandidateHTML, passed: true,
			embeddedBadFor: func(src, previous string, _ int) bool {
				return previous != "" && strings.Contains(src, "i < 16")
			}},

		// 8. Embedded-script gate refuses both.
		{name: "08_embedded_refuses_both",
			rel: "index.html", prior: auPriorHTML,
			baseline: auBaselineHTML, candidate: auCandidateHTML, passed: true,
			// Previous-dependent again, and true for both sides: the gate
			// condemns the candidate AND the baseline it would fall back to.
			embeddedBadFor: func(_, previous string, _ int) bool { return previous != "" }},

		// 9. Duplicate-main guard refuses whatever is about to land.
		{name: "09_duplicate_main_refused",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidateDoubleMain, passed: true},

		// 10. Cancellation during the post-generation gates.
		{name: "10_cancelled_during_gates",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: true,
			cancelOnStructural: true},

		// 11. The candidate write itself fails.
		{name: "11_candidate_write_fails",
			rel: "ro/solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: true,
			readOnlyDir: true},

		// 12. The revoked-to-baseline write fails.
		{name: "12_baseline_write_fails",
			rel: "ro/solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: false,
			declaredCommand: "pytest -q", readOnlyDir: true},
	}
}

// Q9, isolated: an unauthorized but non-empty Code must not reach disk, must
// not be checked by any gate, and must not colour any decision. The hash
// recording is what proves it -- no check examined the candidate's bytes.
func TestUnauthorizedCandidateNeverInfluencesAnything(t *testing.T) {
	got := runAudit(t, auditCase{
		rel: "solve.py", baseline: auBaselinePy, candidate: auCandidateUnresolved,
		passed: false,
		// If the unauthorized candidate were ever consulted, this verdict
		// would fire on it and change the outcome.
		unresolvedFor: func(src string) []string {
			if strings.Contains(src, "missing_helper(") {
				return []string{"missing_helper"}
			}
			return nil
		}})

	if got.disk != auBaselinePy {
		t.Fatalf("unauthorized candidate influenced the delivered bytes: %q", got.disk)
	}
	// The candidate IS examined -- that is how the machine learns it is
	// unusable, and the structural verdict on it is what refuses it. What must
	// never happen is that it lands, or that the baseline is restored without
	// being checked itself.
	baseHash := shortHash(auBaselinePy)
	checkedBaseline := false
	for _, ch := range got.checks {
		if ch.hash == baseHash {
			checkedBaseline = true
		}
	}
	if !checkedBaseline {
		t.Error("the restored baseline was written without being checked")
	}
	if got.res.V3Used {
		t.Error("unauthorized delivery carries V3 provenance")
	}
}

// Q4/Q5: sanitisation rewrites the bytes after the service produced its
// record, so that record describes text that will never exist on disk. It was
// never an authorization and is not one now -- what decides is the proxy's own
// evidence about the sanitised bytes, measured against the floor the client
// declared. With a declared command the executor never answers, that floor
// goes unmet and the caller's own content is what lands.
func TestSanitizedCandidateLosesItsAuthorization(t *testing.T) {
	fenced := "```python\n" + auCandidatePy + "```\n"
	got := runAudit(t, auditCase{
		rel: "solve.py", baseline: auBaselinePy, candidate: fenced, passed: true,
		declaredCommand: "pytest -q"})

	sanitized, changed := sanitizeFileContent("solve.py", fenced)
	if !changed {
		t.Fatal("fixture did not exercise sanitization")
	}
	if got.disk != auBaselinePy {
		t.Fatalf("delivered bytes are not the baseline: %q", got.disk)
	}
	if got.disk == sanitized || got.disk == fenced {
		t.Fatal("unauthorized candidate bytes reached disk")
	}
	if got.res.V3Used {
		t.Error("provenance survived a revoked authorization")
	}
	// The baseline that IS delivered still gets its own observation.
	if got.res.ValidationKind != ValidationKindSyntax ||
		got.res.ValidationStatus != ValidationPassed {
		t.Errorf("validation = %q/%q, want syntax/passed",
			got.res.ValidationKind, got.res.ValidationStatus)
	}
}

// Q6, answered by the invariant: every revocation route now freshly checks the
// baseline it restores. Before this slice only the structural one did, and it
// discarded the verdict; the language-swap and embedded routes delivered the
// baseline with no check of their own beyond the pre-generation preflight.
func TestEveryRevocationRouteChecksTheBaselineItRestores(t *testing.T) {
	countSyntaxOn := func(r auditResult, content string) int {
		n := 0
		for _, ch := range r.checks {
			if ch.kind == "syntax" && ch.hash == shortHash(content) {
				n++
			}
		}
		return n
	}
	structural := runAudit(t, auditCase{
		rel: "solve.py", baseline: auBaselinePy, candidate: auCandidateUnresolved, passed: true,
		unresolvedFor: func(src string) []string {
			if strings.Contains(src, "missing_helper(") {
				return []string{"missing_helper"}
			}
			return nil
		}})
	swap := runAudit(t, auditCase{
		rel: "index.html", baseline: auBaselineHTML, candidate: auCandidateNotHTML, passed: true})

	// Structural: preflight, then the revocation's own check of the baseline.
	if got := countSyntaxOn(structural, auBaselinePy); got != 2 {
		t.Errorf("structural revocation: syntax checks on the baseline = %d, want 2", got)
	}
	// Language swap selects the baseline BEFORE the common final-byte check,
	// so that check is the one covering the delivery: preflight, then it.
	if got := countSyntaxOn(swap, auBaselineHTML); got != 2 {
		t.Errorf("language-swap revocation: syntax checks on the baseline = %d, "+
			"want 2 (preflight, then the common final-byte check)", got)
	}
	for _, r := range []auditResult{structural, swap} {
		if !r.res.Success {
			t.Fatalf("both revocations must deliver the baseline: %q", r.res.Error)
		}
		if r.res.V3Used {
			t.Error("a revoked delivery carries V3 provenance")
		}
		if r.res.ValidationStatus != ValidationPassed || r.res.ValidationKind != ValidationKindSyntax {
			t.Errorf("revoked delivery validation = %q/%q, want syntax/passed",
				r.res.ValidationKind, r.res.ValidationStatus)
		}
	}
}

// ---------------------------------------------------------------------------
// The invariant, transition by transition
// ---------------------------------------------------------------------------
//
// Every artifact this state machine delivers carries a structured syntax
// observation of the exact bytes written. These expectations pin the whole
// shape of each transition -- which bytes each check examined and in what
// order, which bytes landed, whether V3 may be named as their author, the
// classification, and the SSE -- so a change that keeps the classification
// while quietly checking different bytes still fails.

type transitionWant struct {
	name     string
	checks   []string // "kind:ROLE", in arrival order
	disk     string   // role of the final on-disk bytes, "" = nothing written
	success  bool
	mutation MutationStatus
	kind     ValidationKind
	status   ValidationStatus
	v3Used   bool
	sse      []string
}

func TestFinalByteInvariantTransitions(t *testing.T) {
	byName := map[string]auditCase{}
	for _, c := range auditCases() {
		byName[c.name] = c
	}
	for _, w := range []transitionWant{
		// Uncertified code: the candidate IS checked -- that is how the route
		// learns what it has -- and the contractless delivery rule then keeps
		// the caller's content, which is checked again before it is restored.
		{name: "01_unauthorized_code_retained_baseline",
			// The preflight, the candidate's own final-byte check, and the
			// baseline's check before it is restored.
			checks:  []string{"syntax:BASE", "syntax:CAND", "syntax:BASE", "structural:BASE"},
			disk:    "BASE",
			success: true, mutation: MutationApplied,
			kind: ValidationKindSyntax, status: ValidationPassed,
			sse: []string{"v3_progress"}},

		// Authorized candidate: checked once, after sanitisation, before any
		// downstream gate.
		{name: "02_authorized_candidate_delivered",
			checks:  []string{"syntax:BASE", "syntax:CAND", "structural:CAND"},
			disk:    "CAND",
			success: true, mutation: MutationApplied,
			kind: ValidationKindSyntax, status: ValidationPassed,
			v3Used: true, sse: []string{"v3_progress", "v3_progress"}},

		// Sanitised candidate: the evidence described the PRE-sanitised bytes,
		// so it certifies nothing that would be written. The sanitised bytes
		// are still checked as a proposal, and the caller's own content is
		// delivered instead.
		{name: "04_sanitization_rewrites_candidate",
			checks:  []string{"syntax:BASE", "syntax:SANI", "syntax:BASE", "structural:BASE"},
			disk:    "BASE",
			success: true, mutation: MutationApplied,
			kind: ValidationKindSyntax, status: ValidationPassed,
			sse: []string{"v3_progress"}},

		// Candidate fails its own final-byte check: the baseline is checked
		// before it is restored, and it is delivered without provenance.
		{name: "04b_sanitized_candidate_fails_baseline_clean",
			checks:  []string{"syntax:BASE", "syntax:CAND", "syntax:BASE", "structural:BASE"},
			disk:    "BASE",
			success: true, mutation: MutationApplied,
			kind: ValidationKindSyntax, status: ValidationPassed,
			sse: []string{"v3_progress"}},

		// Both fail: nothing is written, and the refusal names the baseline's
		// failure -- the alternative that was considered and rejected.
		{name: "04c_sanitized_candidate_and_baseline_fail",
			checks:  []string{"syntax:BASE", "syntax:CAND", "syntax:BASE"},
			disk:    "",
			success: false, mutation: MutationRefused,
			kind: ValidationKindSyntax, status: ValidationFailed,
			sse: []string{"v3_progress"}},

		// Structural revocation: the baseline's verdict is now retained rather
		// than spent on the allow decision and discarded.
		{name: "05_structural_revokes_to_clean_baseline",
			checks: []string{"syntax:BASE", "syntax:CAND", "structural:CAND",
				"structural:EMPTY", "syntax:BASE", "structural:BASE"},
			disk:    "BASE",
			success: true, mutation: MutationApplied,
			kind: ValidationKindSyntax, status: ValidationPassed,
			sse: []string{"v3_progress", "v3_progress"}},

		// Embedded revocation: reachable only for a previous-dependent finding,
		// and the baseline it restores is freshly checked first.
		{name: "07_embedded_revokes_to_baseline",
			checks: []string{"syntax:BASE", "embedded:BASE", "syntax:CAND", "embedded:CAND",
				"embedded:CAND", "embedded:PRIOR", "embedded:BASE", "syntax:BASE", "embedded:BASE"},
			disk:    "BASE",
			success: true, mutation: MutationApplied,
			kind: ValidationKindSyntax, status: ValidationPassed,
			sse: []string{"v3_progress"}},

		// The candidate never became an artifact: no provenance, and the
		// observation of those exact bytes survives the failure.
		// An authorized candidate whose write fails: the licence was spent on
		// bytes that never reached disk, so the delivery owner refuses rather
		// than reporting a mutation. Nothing moved and nothing is claimed.
		{name: "11_candidate_write_fails",
			checks:  []string{"syntax:BASE", "syntax:CAND", "structural:CAND"},
			disk:    "",
			success: false, mutation: MutationRefused,
			kind: ValidationKindSyntax, status: ValidationPassed,
			sse: []string{"v3_progress", "v3_progress"}},

		// Row 5b: the revocation's baseline check decides, and it is a SYNTAX
		// refusal -- pinned here, unchanged by this slice.
		{name: "05b_structural_revocation_blocked_by_broken_baseline",
			checks: []string{"syntax:BASE", "syntax:CAND", "structural:CAND",
				"structural:EMPTY", "syntax:BASE"},
			disk:    "",
			success: false, mutation: MutationRefused,
			kind: ValidationKindSyntax, status: ValidationFailed,
			sse: []string{"v3_progress"}},

		// Row 6: neither the candidate nor the baseline resolves its calls.
		// Syntax passed on both; the structural failure is the decisive one.
		{name: "06_structural_refuses_both",
			checks: []string{"syntax:BASE", "syntax:CAND", "structural:CAND", "structural:EMPTY",
				"syntax:BASE", "structural:BASE", "structural:EMPTY"},
			disk:    "",
			success: false, mutation: MutationRefused,
			kind: ValidationKindStructural, status: ValidationFailed,
			sse: []string{"v3_progress"}},

		// Row 8: the comparative embedded gate condemns both. The finding is
		// previous-dependent -- a before/after regression, not a standalone
		// parse failure, which the final-byte check already owns -- so it is
		// classified structural rather than syntax.
		{name: "08_embedded_refuses_both",
			checks: []string{"syntax:BASE", "embedded:BASE", "syntax:CAND", "embedded:CAND",
				"embedded:CAND", "embedded:PRIOR", "embedded:BASE", "embedded:PRIOR"},
			disk:    "",
			success: false, mutation: MutationRefused,
			kind: ValidationKindStructural, status: ValidationFailed,
			sse: []string{"v3_progress"}},

		// Row 9: the entrypoint guard. Structural in the same sense: the file
		// parses and its module-level structure is wrong.
		{name: "09_duplicate_main_refused",
			checks:  []string{"syntax:BASE", "syntax:CAND", "structural:CAND"},
			disk:    "",
			success: false, mutation: MutationRefused,
			kind: ValidationKindStructural, status: ValidationFailed,
			sse: []string{"v3_progress"}},

		// Row 10: cancelled after the final-byte observation and before any
		// mutation began. No attempt was made, so MutationNone -- and the
		// observation earned on those exact bytes is still true.
		{name: "10_cancelled_during_gates",
			checks:  []string{"syntax:BASE", "syntax:CAND", "structural:CAND"},
			disk:    "",
			success: false, mutation: MutationNone,
			kind: ValidationKindSyntax, status: ValidationPassed,
			sse: []string{"v3_progress"}},

		// Baseline fallback that fails to land: same, for the model's bytes.
		{name: "12_baseline_write_fails",
			checks:  []string{"syntax:BASE", "syntax:CAND", "syntax:BASE", "structural:BASE"},
			disk:    "",
			success: false, mutation: MutationFailed,
			kind: ValidationKindSyntax, status: ValidationPassed,
			sse: []string{"v3_progress"}},
	} {
		w := w
		t.Run(w.name, func(t *testing.T) {
			c, ok := byName[w.name]
			if !ok {
				t.Fatalf("no audit case named %s", w.name)
			}
			got := runAudit(t, c)
			sanitized, _ := sanitizeFileContent(c.rel, c.candidate)
			role := func(h string) string {
				switch h {
				case shortHash(c.baseline):
					return "BASE"
				case shortHash(c.candidate):
					return "CAND"
				case shortHash(sanitized):
					return "SANI"
				case shortHash(""):
					return "EMPTY"
				case shortHash(c.prior):
					return "PRIOR"
				}
				return "OTHER"
			}
			var seq []string
			for _, ch := range got.checks {
				seq = append(seq, ch.kind+":"+role(ch.hash))
			}
			if fmt.Sprint(seq) != fmt.Sprint(w.checks) {
				t.Errorf("checks = %v\n want %v", seq, w.checks)
			}
			wantDisk := ""
			switch w.disk {
			case "BASE":
				wantDisk = c.baseline
			case "CAND":
				wantDisk = c.candidate
			case "SANI":
				wantDisk = sanitized
			case "PRIOR":
				wantDisk = c.prior
			}
			if w.disk == "" {
				// Nothing delivered: the prior artifact, if any, is untouched.
				if got.disk != c.prior {
					t.Errorf("disk = %s, want the untouched prior %s",
						role(shortHash(got.disk)), role(shortHash(c.prior)))
				}
			} else if got.disk != wantDisk {
				t.Errorf("disk = %s, want %s", role(shortHash(got.disk)), w.disk)
			}
			if got.res.Success != w.success {
				t.Errorf("Success = %v, want %v", got.res.Success, w.success)
			}
			if got.res.MutationStatus != w.mutation ||
				got.res.ValidationKind != w.kind ||
				got.res.ValidationStatus != w.status {
				t.Errorf("got %q/%q/%q, want %q/%q/%q",
					got.res.MutationStatus, got.res.ValidationKind, got.res.ValidationStatus,
					w.mutation, w.kind, w.status)
			}
			if got.res.V3Used != w.v3Used {
				t.Errorf("V3Used = %v, want %v", got.res.V3Used, w.v3Used)
			}
			if !w.v3Used && (got.res.CandidatesTested != 0 || got.res.PhaseSolved != "" ||
				len(got.res.VerificationEvidence) != 0) {
				t.Errorf("provenance leaked onto unauthorized bytes: %+v", got.res)
			}
			if fmt.Sprint(got.events) != fmt.Sprint(w.sse) {
				t.Errorf("SSE = %v, want %v", got.events, w.sse)
			}
		})
	}
}

// Cancellation is authoritative before the final write, and nothing runs a
// checker after it: the last request in the run is the gate call during which
// the user cancelled.
func TestCancellationRunsNoCheckerAfterwards(t *testing.T) {
	var cancelCase auditCase
	for _, c := range auditCases() {
		if strings.HasPrefix(c.name, "10_") {
			cancelCase = c
		}
	}
	got := runAudit(t, cancelCase)

	if got.res.Success || got.disk != "" {
		t.Fatalf("a cancelled write must land nothing: success=%v disk=%q",
			got.res.Success, got.disk)
	}
	if !strings.Contains(got.res.Error, "cancelled") {
		t.Fatalf("cancellation must say so: %q", got.res.Error)
	}
	last := got.checks[len(got.checks)-1]
	if last.kind != "structural" || last.hash != shortHash(cancelCase.candidate) {
		t.Errorf("last request = %s:%s, want the structural call during which "+
			"the cancel arrived", last.kind, last.hash)
	}
	if got.res.V3Used {
		t.Error("a cancelled write carries V3 provenance")
	}
}
