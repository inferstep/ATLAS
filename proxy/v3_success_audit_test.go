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

// The SUCCESSFUL writeFileWithV3 state machine: V3 answered, and the
// route decides which bytes land, what evidence covers those exact bytes,
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
	// a gate after it), and the second answer is the one under test.
	unresolvedFor func(src string) []string // structural_check verdict
	// embeddedBadFor also receives `previous`, because the service reports a
	// class of defect only a before/after comparison can see (a render loop the
	// edit stopped driving). That class is exactly what the embedded GATE can
	// still catch after the previous-less final-byte check has passed.
	embeddedBadFor     func(src, previous string, call int) bool
	syntaxBadFor       func(src string, call int) bool // /syntax-check verdict
	cancelOnStructural bool
	readOnlyDir        bool
	// declaredCommand makes the client require a command the stub executor
	// never answers, so the candidate's floor goes unmet and the proxy refuses
	// it. A service verdict alone cannot cause a refusal; this can.
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
			if c.passed {
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
	// delivery to walk -- which is the point of these tests.
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
	st.mu.Lock()
	checks := append([]auditCheck(nil), st.checks...)
	st.mu.Unlock()
	return auditResult{res: res, disk: string(after), checks: checks, events: events}
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
// The transitions
// ---------------------------------------------------------------------------
//
// Every artifact this state machine delivers carries a structured syntax
// observation of the exact bytes written; provenance may only describe bytes
// V3 authored and this route delivered. Each row pins the whole shape of one
// transition: which bytes each check examined and in what order, which bytes
// landed, whether V3 may be named as their author, the classification, and
// the SSE. A change that keeps the classification while quietly checking
// different bytes still fails.

type transitionWant struct {
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
	unresolvedIn := func(markers ...string) func(string) []string {
		return func(src string) []string {
			var names []string
			for _, m := range markers {
				if strings.Contains(src, m+"(") {
					names = append(names, m)
				}
			}
			return names
		}
	}
	for _, tc := range []struct {
		in   auditCase
		want transitionWant
	}{
		// A response that did not pass but still carries code. Authorization is
		// Passed, not the presence of Code. The candidate IS checked (that is how
		// the route learns what it has), and the caller's content is checked
		// again before it is restored.
		{auditCase{name: "01_unauthorized_code_retained_baseline",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: false,
			declaredCommand: "pytest -q"},
			transitionWant{
				checks:  []string{"syntax:BASE", "syntax:CAND", "syntax:BASE", "structural:BASE"},
				disk:    "BASE",
				success: true, mutation: MutationApplied,
				kind: ValidationKindSyntax, status: ValidationPassed,
				sse: []string{"v3_progress"}}},

		// An authorized candidate survives every gate: checked once, after
		// sanitisation, before any downstream gate.
		{auditCase{name: "02_authorized_candidate_delivered",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: true},
			transitionWant{
				checks:  []string{"syntax:BASE", "syntax:CAND", "structural:CAND"},
				disk:    "CAND",
				success: true, mutation: MutationApplied,
				kind: ValidationKindSyntax, status: ValidationPassed,
				v3Used: true, sse: []string{"v3_progress", "v3_progress"}}},

		// The language-swap gate revokes to the baseline. It selects the baseline
		// BEFORE the common final-byte check, so that check is the one covering
		// the delivery.
		{auditCase{name: "03_language_swap_revoked",
			rel: "index.html", baseline: auBaselineHTML, candidate: auCandidateNotHTML, passed: true},
			transitionWant{
				checks:  []string{"syntax:BASE", "embedded:BASE", "syntax:BASE", "embedded:BASE", "embedded:BASE"},
				disk:    "BASE",
				success: true, mutation: MutationApplied,
				kind: ValidationKindSyntax, status: ValidationPassed,
				sse: []string{"v3_progress"}}},

		// Sanitisation rewrites the bytes after the service produced its record,
		// so that record describes text that will never exist on disk. What
		// decides is the proxy's own evidence about the sanitised bytes, against
		// the floor the client declared. The declared command is never answered,
		// so the floor goes unmet and the caller's own content lands.
		{auditCase{name: "04_sanitization_rewrites_candidate",
			rel: "solve.py", baseline: auBaselinePy,
			candidate: "```python\n" + auCandidatePy + "```\n", passed: true,
			declaredCommand: "pytest -q"},
			transitionWant{
				checks:  []string{"syntax:BASE", "syntax:SANI", "syntax:BASE", "structural:BASE"},
				disk:    "BASE",
				success: true, mutation: MutationApplied,
				kind: ValidationKindSyntax, status: ValidationPassed,
				sse: []string{"v3_progress"}}},

		// The candidate fails its own final-byte check; the baseline parses. The
		// baseline is checked before it is restored, and it is delivered without
		// provenance.
		{auditCase{name: "04b_sanitized_candidate_fails_baseline_clean",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: true,
			syntaxBadFor: func(src string, _ int) bool { return src == auCandidatePy }},
			transitionWant{
				checks:  []string{"syntax:BASE", "syntax:CAND", "syntax:BASE", "structural:BASE"},
				disk:    "BASE",
				success: true, mutation: MutationApplied,
				kind: ValidationKindSyntax, status: ValidationPassed,
				sse: []string{"v3_progress"}}},

		// Neither the candidate nor the baseline parses: nothing is written, and
		// the refusal names the baseline's failure, the alternative that was
		// considered and rejected.
		{auditCase{name: "04c_sanitized_candidate_and_baseline_fail",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: true,
			// The preflight (call 1) passes the baseline; every later check fails.
			syntaxBadFor: func(_ string, call int) bool { return call > 1 }},
			transitionWant{
				checks:  []string{"syntax:BASE", "syntax:CAND", "syntax:BASE"},
				disk:    "",
				success: false, mutation: MutationRefused,
				kind: ValidationKindSyntax, status: ValidationFailed,
				sse: []string{"v3_progress"}}},

		// The structural gate rejects the candidate and the baseline is clean.
		// The baseline's verdict is kept, not spent on the allow decision and
		// discarded.
		{auditCase{name: "05_structural_revokes_to_clean_baseline",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidateUnresolved,
			passed: true, unresolvedFor: unresolvedIn("missing_helper")},
			transitionWant{
				checks: []string{"syntax:BASE", "syntax:CAND", "structural:CAND",
					"structural:EMPTY", "syntax:BASE", "structural:BASE"},
				disk:    "BASE",
				success: true, mutation: MutationApplied,
				kind: ValidationKindSyntax, status: ValidationPassed,
				sse: []string{"v3_progress", "v3_progress"}}},

		// Same, but the baseline does not parse: the revocation's baseline check
		// decides, and it is a SYNTAX refusal.
		{auditCase{name: "05b_structural_revocation_blocked_by_broken_baseline",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidateUnresolved,
			passed: true, unresolvedFor: unresolvedIn("missing_helper"),
			// The preflight (call 1) passes the baseline; the revocation's own
			// re-check (call 2) finds it broken. Only a sequenced stub can
			// separate those two, which is the point of the ordinal.
			syntaxBadFor: func(src string, call int) bool {
				return call > 1 && src == auBaselinePy
			}},
			transitionWant{
				checks: []string{"syntax:BASE", "syntax:CAND", "structural:CAND",
					"structural:EMPTY", "syntax:BASE"},
				disk:    "",
				success: false, mutation: MutationRefused,
				kind: ValidationKindSyntax, status: ValidationFailed,
				sse: []string{"v3_progress"}}},

		// Neither the candidate nor the baseline resolves its calls. Syntax
		// passed on both; the structural failure is the decisive one.
		{auditCase{name: "06_structural_refuses_both",
			rel: "solve.py", baseline: auBaselineUnresolved, candidate: auCandidateUnresolved,
			passed: true, unresolvedFor: unresolvedIn("missing_helper", "base_helper")},
			transitionWant{
				checks: []string{"syntax:BASE", "syntax:CAND", "structural:CAND", "structural:EMPTY",
					"syntax:BASE", "structural:BASE", "structural:EMPTY"},
				disk:    "",
				success: false, mutation: MutationRefused,
				kind: ValidationKindStructural, status: ValidationFailed,
				sse: []string{"v3_progress"}}},

		// The embedded-script gate revokes the candidate to the baseline. The
		// finding is previous-dependent, which keeps this transition reachable:
		// the final-byte check asks without a previous and sees nothing, and the
		// gate's before/after comparison is what refuses. The baseline it
		// restores is freshly checked first.
		{auditCase{name: "07_embedded_revokes_to_baseline",
			rel: "index.html", prior: auPriorHTML,
			baseline: auBaselineHTML, candidate: auCandidateHTML, passed: true,
			embeddedBadFor: func(src, previous string, _ int) bool {
				return previous != "" && strings.Contains(src, "i < 16")
			}},
			transitionWant{
				checks: []string{"syntax:BASE", "embedded:BASE", "syntax:CAND", "embedded:CAND",
					"embedded:CAND", "embedded:PRIOR", "embedded:BASE", "syntax:BASE", "embedded:BASE"},
				disk:    "BASE",
				success: true, mutation: MutationApplied,
				kind: ValidationKindSyntax, status: ValidationPassed,
				sse: []string{"v3_progress"}}},

		// The comparative embedded gate condemns the candidate AND the baseline it
		// would fall back to. The finding is a before/after regression, not a
		// standalone parse failure (which the final-byte check owns), so it is
		// classified structural rather than syntax.
		{auditCase{name: "08_embedded_refuses_both",
			rel: "index.html", prior: auPriorHTML,
			baseline: auBaselineHTML, candidate: auCandidateHTML, passed: true,
			embeddedBadFor: func(_, previous string, _ int) bool { return previous != "" }},
			transitionWant{
				checks: []string{"syntax:BASE", "embedded:BASE", "syntax:CAND", "embedded:CAND",
					"embedded:CAND", "embedded:PRIOR", "embedded:BASE", "embedded:PRIOR"},
				disk:    "",
				success: false, mutation: MutationRefused,
				kind: ValidationKindStructural, status: ValidationFailed,
				sse: []string{"v3_progress"}}},

		// The duplicate-main guard refuses whatever is about to land. Structural
		// in the same sense: the file parses and its module-level structure is
		// wrong.
		{auditCase{name: "09_duplicate_main_refused",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidateDoubleMain, passed: true},
			transitionWant{
				checks:  []string{"syntax:BASE", "syntax:CAND", "structural:CAND"},
				disk:    "",
				success: false, mutation: MutationRefused,
				kind: ValidationKindStructural, status: ValidationFailed,
				sse: []string{"v3_progress"}}},

		// Cancelled during the post-generation gates, after the final-byte
		// observation and before any mutation began. No attempt was made, so
		// MutationNone, and the observation earned on those exact bytes is still
		// true. Nothing runs a checker after the cancel: the last request is the
		// structural call during which it arrived.
		{auditCase{name: "10_cancelled_during_gates",
			rel: "solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: true,
			cancelOnStructural: true},
			transitionWant{
				checks:  []string{"syntax:BASE", "syntax:CAND", "structural:CAND"},
				disk:    "",
				success: false, mutation: MutationNone,
				kind: ValidationKindSyntax, status: ValidationPassed,
				sse: []string{"v3_progress"}}},

		// An authorized candidate whose write fails: the licence was spent on
		// bytes that never reached disk, so the delivery owner refuses rather
		// than reporting a mutation. Nothing moved and nothing is claimed.
		{auditCase{name: "11_candidate_write_fails",
			rel: "ro/solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: true,
			readOnlyDir: true},
			transitionWant{
				checks:  []string{"syntax:BASE", "syntax:CAND", "structural:CAND"},
				disk:    "",
				success: false, mutation: MutationRefused,
				kind: ValidationKindSyntax, status: ValidationPassed,
				sse: []string{"v3_progress", "v3_progress"}}},

		// The fallback to the model's own bytes fails to land.
		{auditCase{name: "12_baseline_write_fails",
			rel: "ro/solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: false,
			declaredCommand: "pytest -q", readOnlyDir: true},
			transitionWant{
				checks:  []string{"syntax:BASE", "syntax:CAND", "syntax:BASE", "structural:BASE"},
				disk:    "",
				success: false, mutation: MutationFailed,
				kind: ValidationKindSyntax, status: ValidationPassed,
				sse: []string{"v3_progress"}}},
	} {
		c, w := tc.in, tc.want
		t.Run(c.name, func(t *testing.T) {
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

// A candidate the service did not pass is examined (that is how the route
// learns it is unusable, and the structural verdict on it is what refuses
// it), but it must never land, and the baseline must never be restored
// without being checked itself.
func TestUnauthorizedCandidateNeverInfluencesAnything(t *testing.T) {
	got := runAudit(t, auditCase{
		rel: "solve.py", baseline: auBaselinePy, candidate: auCandidateUnresolved,
		passed: false,
		// If the unauthorized candidate decided anything, this verdict would
		// fire on it and change the outcome.
		unresolvedFor: func(src string) []string {
			if strings.Contains(src, "missing_helper(") {
				return []string{"missing_helper"}
			}
			return nil
		}})

	if got.disk != auBaselinePy {
		t.Fatalf("unauthorized candidate influenced the delivered bytes: %q", got.disk)
	}
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

// A write cancelled during the gates says so. Which checks ran and what
// landed is pinned by the 10_cancelled_during_gates transition.
func TestACancelledWriteSaysItWasCancelled(t *testing.T) {
	got := runAudit(t, auditCase{
		rel: "solve.py", baseline: auBaselinePy, candidate: auCandidatePy, passed: true,
		cancelOnStructural: true})

	if !strings.Contains(got.res.Error, "cancelled") {
		t.Fatalf("cancellation must say so: %q", got.res.Error)
	}
}
