// Gates: the checks the agent loop runs at its decision points — before a
// write reaches disk, and before a `done` is accepted.
//
// In file order:
//
//	Completion-claim verification — bounces a `done` whose summary claims
//	  universal success ("all routes work") while the workspace still shows
//	  a structural gap. Needs both halves, so a narrow summary or a clean
//	  workspace passes untouched.
//	Structural-unresolved gate — asks v3-service whether a write or edit
//	  leaves an undefined name behind, and rejects only the unresolved names
//	  that this change introduced.
//	Syntax gate — routes fallback writes through the sandbox's
//	  /syntax-check. These are the writes that skipped the V3 pipeline, so
//	  nothing else in the loop has parsed them.
//	Embedded-script gate — parses the JS/CSS inside <script>/<style> blocks
//	  in HTML files and in Python string literals, which every other gate is
//	  structurally blind to.
//	Plan adherence — matches each tool call against the pre-flight plan and
//	  counts the off-plan streak, regenerating the plan once the streak runs
//	  long. Advisory: it never blocks a call.
//	Plan-progress reminder — renders the compact step-progress block
//	  injected ahead of each LLM call so a long multi-file task doesn't
//	  lose track of what's left.
//	Asset-graph lint — cross-file coherence for small web projects: a
//	  template no route renders, an href to a file that isn't there, a fetch
//	  to a route that doesn't exist. Advisory notes, deduped per session.
//
// They share a shape, not a subject. Each one inspects agent output or the
// workspace at a single point in the loop and returns a string; the blocking
// four return it as a rejection the model must answer, the advisory three
// return it as a [system note]. They hold no state in common — this is a
// policy surface, not a pipeline, and gates can be read and changed one at a
// time.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Completion-claim verification.
//
// Background: the agent's done.summary often makes universal claims
// ("all routes work", "fixed all bugs", "verified everything") that
// we can structurally check against the workspace. The May 2026 flask
// run had the model claim "All routes are functioning properly" while
// only 3 of 7 needed templates existed. The verification gate only
// checks "did you run a verification command at all?" — it does
// NOT check whether the claim in the summary matches reality.
//
// Two-stage filter:
//   1. claimsUniversal(summary) — does the wording make a global
//      assertion? Quiet pass for narrow done summaries ("added /admin
//      route" — model said nothing about the rest of the app).
//   2. verifyCompletionClaims(workingDir) — cheap structural
//      checks for the failure modes we know about. Returns a directive
//      to the model when a gap is found, "" otherwise.
//
// Conservative on false positives. Universal claims with no gap pass
// silently; narrow claims pass even when there ARE gaps elsewhere.
// Only the AND case (claim + gap) bounces. The model can override by
// using narrower wording or by calling out the gap explicitly.

// multiIssueWords trips when the USER PROMPT (not the summary) signals
// "fix multiple things." Models bypass claimsUniversal by writing a
// narrow done summary ("fixed the product route") even when the user
// clearly asked for the whole app to work. Catching it on the prompt
// side handles that.
var multiIssueWords = []string{
	"lots of", "ton of", "tons of", "many", "multiple", "several",
	"all bugs", "all the bugs", "all issues", "all the issues",
	"all errors", "all the errors", "all routes", "all endpoints",
	"all tests", "all pages", "all the routes", "all the endpoints",
	"fix all", "fix everything", "fix the bugs", "fix the issues",
	"all of the", "everything", "nothing works",
	"it does not work", "doesn't work", "isn't working",
}

// promptIsMultiIssue returns true when the user explicitly framed the
// task as "fix multiple things" or "make this whole thing work."
// Used as an alternate trigger for the claim-check gate so a narrow
// done summary still gets verified against the workspace.
func promptIsMultiIssue(prompt string) bool {
	lower := strings.ToLower(prompt)
	for _, w := range multiIssueWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// claimWords trips the universal-claim filter. The phrases below are
// what models actually emit when they oversummarize ("all", "every",
// "no errors", "everything works", "fully functional", etc.).
var claimWords = []string{
	"all routes", "all endpoints", "all pages", "all tests",
	"all bugs", "all issues", "all errors",
	"every route", "every endpoint", "every page",
	"all routes are", "all endpoints are",
	"fully functional", "fully working", "fully operational",
	"completely fixed", "completely working", "completely done",
	"no errors", "no issues", "no bugs", "no problems",
	"everything works", "everything is working", "everything is fixed",
	"fixed all", "verified all", "tested all",
	"functioning properly", "functioning correctly", "working properly",
}

// claimsUniversal returns true when the summary contains a global
// assertion the structural checks should validate. Case-insensitive.
func claimsUniversal(summary string) bool {
	lower := strings.ToLower(summary)
	for _, w := range claimWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// verifyCompletionClaims returns a non-empty directive when the model's
// universal claim doesn't match reality. The directive is shaped as
// a tool-result error, so it lands back in the model's context as
// "your done was bounced because X."
//
// The structural evidence comes from assetLintFindings — the same
// bounded workspace walk the advisory lint uses — filtered down to the
// hard gaps: template references (render_template('X') in .py,
// {% extends/include %} in templates) whose target does not exist.
// Those are blocking because a missing render_template target is a
// guaranteed 500 at runtime; the rest of the lint stays advisory.
func verifyCompletionClaims(workingDir string) string {
	gaps := completionClaimGaps(workingDir)
	if len(gaps) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"Your `done` summary claims the work is complete, but a structural check of the workspace found gaps:\n\n%s\n\nFix the missing files (or correct your summary to acknowledge what's not done) before declaring done.",
		strings.Join(gaps, "\n"))
}

// completionClaimGaps are the hard gaps verifyCompletionClaims bounces on:
// template references whose target does not exist.
func completionClaimGaps(workingDir string) []string {
	if workingDir == "" {
		return nil
	}
	var gaps []string
	for _, f := range assetLintFindings(workingDir) {
		if strings.Contains(f, "references template ") {
			gaps = append(gaps, f)
		}
	}
	return gaps
}

// Structural gate for the edit and write paths (issue #147). The V3
// structural veto hard-rejects generated candidates whose direct-identifier
// calls resolve to no local def, import, or builtin — but the edit path
// (improveContentWithV3) frequently sent no project_context, so the
// in-pipeline veto was gated off, and even when it fired the pipeline's
// baseline fallback resurrected the model's own edit. Result observed in
// 2026-07-18 dogfooding: a structural_edit replaced a route with a body calling
// render_template while the file imported only render_template_string; it
// passed V3 verification, landed as verified, and every request 500'd
// (NameError). structural_edit had no syntax gate at all; edit_file's syntax gate
// catches parse failures but a NameError parses fine.
//
// This proxy-side gate closes the hole where it can't be bypassed: it
// resolves the COMPOSED post-change file through v3-service's structural
// checker and refuses landing content that INTRODUCES an unresolved direct
// call — the same healthy->broken rule as the syntax gate (a change that
// leaves a pre-existing unresolved name in place, i.e. a repair-in-
// progress, is allowed). Wired into edit_file, structural_edit, and every
// write_file branch (V3 winner, V3-error fallback, iteration fast-path,
// T0/T1 direct), on every request. Python-only and fail-open: if v3-service is unreachable, the file
// isn't .py, or tree-sitter is unavailable, the write proceeds — the gate
// only blocks on a POSITIVE, newly-introduced unresolved call.

// checkStructuralUnresolved returns the direct-identifier calls in
// `content` that resolve to nothing (no local def, import, builtin, or
// supplied project symbol), and ok=true only when the check actually ran.
// Fail-open: (nil, false) for a non-.py file, an empty V3 URL, a network
// failure, or a tree-sitter/parse error on the far side.
func checkStructuralUnresolved(ctx *AgentContext, path, content string) ([]string, bool) {
	if ctx == nil || ctx.V3URL == "" {
		return nil, false
	}
	if strings.ToLower(filepath.Ext(path)) != ".py" {
		return nil, false
	}
	payload := map[string]interface{}{"path": path, "source": content}
	// Pass the OTHER files the model has read as project context so a call
	// to a symbol defined elsewhere in the project is credited (more
	// lenient = fewer false blocks). Crucially, EXCLUDE the file being
	// edited: SnapshotFilesRead still holds its PRE-EDIT body, which would
	// credit a top-level def the edit just deleted and let a genuine
	// NameError through (#147 review finding #2). The edited file's current
	// symbols come from `source`, which structural_score parses directly.
	cleanTarget := filepath.Clean(path)
	rel := make(map[string]string)
	addContext := func(p, c string) {
		if filepath.Clean(p) == cleanTarget {
			return // don't credit the pre-edit self
		}
		r, err := filepath.Rel(ctx.WorkingDir, p)
		if err != nil || r == "" {
			r = p
		}
		// Only .py files carry resolvable symbols, and entries are
		// truncated like the V3 request builders — read_file snapshots
		// can be 200 KB each, and this body is POSTed per gated write.
		if strings.ToLower(filepath.Ext(r)) != ".py" {
			return
		}
		if len(c) > 4000 {
			c = c[:4000] + "\n... (truncated)"
		}
		rel[r] = c
	}
	for p, c := range ctx.SnapshotFilesRead() {
		addContext(p, c)
	}
	// Files the session WROTE are leniency context too — write_file paths
	// never RecordFileRead, so without these a sibling the session just
	// created is invisible here while the in-pipeline veto (which merges
	// SessionWrites) credits it, making this gate strictly stricter than
	// the veto it backstops. Disk content wins over any stale snapshot.
	for w := range ctx.SessionWrites {
		if w == "" || strings.ToLower(filepath.Ext(w)) != ".py" {
			continue // only .py carries symbols — skip the disk read otherwise
		}
		abs := resolveAgentPath(ctx, w)
		if filepath.Clean(abs) == cleanTarget {
			continue // pre-edit self; skip the read
		}
		if data, err := os.ReadFile(abs); err == nil {
			addContext(abs, string(data))
		}
	}
	if len(rel) > 0 {
		payload["project_context"] = rel
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	// ctx.Ctx may be nil on paths constructed without a request context;
	// the gate must fail open (or keep working), never panic.
	base := ctx.Ctx
	if base == nil {
		base = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(base, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST",
		ctx.V3URL+"/internal/structural_check", bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		noteCheckServiceUnavailable(ctx, "unresolved-name", "service unreachable")
		return nil, false // fail-open
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		noteCheckServiceUnavailable(ctx, "unresolved-name", "response unreadable")
		return nil, false
	}
	if resp.StatusCode != http.StatusOK {
		noteCheckServiceUnavailable(ctx, "unresolved-name", fmt.Sprintf("service returned %d", resp.StatusCode))
		return nil, false
	}
	var r struct {
		OK         bool     `json:"ok"`
		Unresolved []string `json:"unresolved"`
	}
	if json.Unmarshal(raw, &r) != nil || !r.OK {
		return nil, false // parse error / tree-sitter missing -> fail-open
	}
	return r.Unresolved, true
}

// editIntroducesUnresolved returns the names an edit NEWLY makes
// unresolved — present in the edited file's unresolved set but not the
// original's. Mirrors the syntax gate's healthy->broken rule: an edit must
// not INTRODUCE a NameError, but a pre-existing unresolved name (the model
// is mid-repair) is allowed to remain. Returns nil when the check couldn't
// run (fail-open) or nothing new was introduced.
func editIntroducesUnresolved(ctx *AgentContext, path, original, edited string) []string {
	editedUnres, ok := checkStructuralUnresolved(ctx, path, edited)
	if !ok || len(editedUnres) == 0 {
		return nil
	}
	origUnres, ok := checkStructuralUnresolved(ctx, path, original)
	if !ok {
		// One retry: the edited-side call just succeeded, so a failure
		// here is a transient blip on the second back-to-back request.
		origUnres, ok = checkStructuralUnresolved(ctx, path, original)
	}
	if !ok {
		// The original-side check couldn't run (transient service failure;
		// tree-sitter-missing would have failed the edited side first, and
		// malformed Python does NOT trigger this — tree-sitter parses it
		// tolerantly and returns a partial extraction). Without a baseline
		// the healthy->broken comparison is meaningless, and counting
		// EVERY unresolved name as newly introduced would block the model
		// from fixing one error at a time — fail open instead.
		return nil
	}
	was := make(map[string]bool, len(origUnres))
	for _, n := range origUnres {
		was[n] = true
	}
	var introduced []string
	for _, n := range editedUnres {
		if !was[n] {
			introduced = append(introduced, n)
		}
	}
	return introduced
}

// readOriginalForGate returns the on-disk original for the healthy->broken
// comparison. A missing file is a first write (empty original — every
// unresolved call counts as introduced). Any OTHER read failure means the
// original is unknowable, so the caller must skip the gate (fail open)
// rather than treat the file as empty and count pre-existing unresolved
// calls as newly introduced.
func readOriginalForGate(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", true
		}
		return "", false
	}
	return string(data), true
}

// structuralRejection builds the tool error handed back when the gate
// blocks an edit — names the offending calls and the recovery.
// structuralWriteRejection is the write_file variant: the recovery must
// name the operation the model actually issued — an "edit" steer on a
// blocked NEW-file write sends the model to edit_file against a file
// that doesn't exist.
func structuralRejection(path string, introduced []string) string {
	return fmt.Sprintf(
		"edit for %s calls %s, which the file neither imports, defines, nor "+
			"gets from builtins — running it would raise NameError. The file was "+
			"NOT modified. Add the missing import (or correct the name to one that "+
			"IS in scope), then re-issue the edit.",
		path, quoteNames(introduced))
}

func structuralWriteRejection(path string, introduced []string) string {
	return fmt.Sprintf(
		"write_file for %s calls %s, which the file neither imports, defines, "+
			"nor gets from builtins — running it would raise NameError. Nothing "+
			"was written. Add the missing import (or correct the name to one that "+
			"IS in scope), then re-issue the write_file with the full corrected "+
			"content.",
		path, quoteNames(introduced))
}

func quoteNames(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + n + "`"
	}
	return strings.Join(quoted, ", ")
}

// Syntax gate for unverified fallback writes. When a V3 call fails or
// times out, the fallback used to write the model's raw baseline to disk
// with success=true — and a truncated tool call (content cut mid-string)
// landed as a file with a SyntaxError while the agent believed the write
// succeeded. Observed twice in the 2026-07-18 mini-bench (t06, t09):
// V3 hit its 3-minute cap, the fallback wrote a 362-byte truncated
// baseline, the follow-up run failed, and the loop breakers stopped a
// session whose "productive change" was a broken file.
//
// The gate routes fallback content through the sandbox's /syntax-check
// (the same checker V3's smoke pass uses). Fail-open by design: if the
// sandbox is unreachable or the file type unsupported, the write
// proceeds — the gate only blocks KNOWN-broken content.

// syntaxGateLanguages maps extensions to the sandbox's language names.
// Only types the sandbox's /syntax-check has a checker for are listed —
// anything else passes through ungated. The checkers judge syntax only, one
// file at a time, and the HTML one is lenient: it requires markup and a
// document that does not end inside a tag, a comment or a <script>, and
// nothing more.
// syntaxLanguage is what this registry knows about an extension: which language
// the sandbox checker uses, and whether the artifact is something a command can
// meaningfully EXECUTE.
//
// The second fact was missing, and its absence had a cost. A completion rule
// that scoped itself by registry membership treated index.html and config.yaml
// as code that must be run, and demanded an execution naming them -- an
// obligation no command can discharge, so ordinary static work could never
// finish. Parseable and runnable are different questions; the registry that
// answers the first is the right place to answer the second, so no second
// extension list can drift away from it.
type syntaxLanguage struct {
	Language string
	// Executable is true for source code a run can exercise. Markup, config
	// and data parse but do not run.
	Executable bool
}

var syntaxGateLanguages = map[string]syntaxLanguage{
	".py":   {Language: "python", Executable: true},
	".js":   {Language: "javascript", Executable: true},
	".ts":   {Language: "typescript", Executable: true},
	".go":   {Language: "go", Executable: true},
	".java": {Language: "java", Executable: true},
	".kt":   {Language: "kotlin", Executable: true},
	".rb":   {Language: "ruby", Executable: true},
	".php":  {Language: "php", Executable: true},
	".sh":   {Language: "bash", Executable: true},
	// Unrouted, a run that wrote one could never demonstrate it (audit
	// GB-4#5). The check keeps the module type through the filename it is
	// sent with. Rust and C/C++ stay out until their checks judge syntax
	// alone: the sandbox compiles the lone file, so a sibling header or
	// module would read as a syntax error, as it did for Java.
	".mjs":  {Language: "javascript", Executable: true},
	".cjs":  {Language: "javascript", Executable: true},
	".json": {Language: "json"},
	".yaml": {Language: "yaml"},
	".yml":  {Language: "yaml"},
	".html": {Language: "html"},
	".htm":  {Language: "html"},
	".xml":  {Language: "xml"},
}

// checkFallbackSyntax returns ("", true) when `content` is safe to write
// as a fallback: it parsed cleanly, or it could not be checked (sandbox
// down, unsupported extension). Returns (firstError, false) when a checker
// confirmed the content does not parse.
//
// Two checkers run, whole-file first: the sandbox parses the file in its own
// language, then checkEmbeddedScript parses the JavaScript/CSS that lives
// INSIDE it (a <script> block in an .html file, or in a Python string handed
// to render_template_string). The sandbox's checker sees the Python or the
// markup only, so a stray `)` in embedded JavaScript passes it.

// checkOutcome is the structured result of ONE validation check. Status is the
// single source of truth: applicability and whether execution was attempted
// are derived from it, never stored separately, so the two cannot contradict.
//
//	not_applicable  the check did not apply to this content
//	not_run         it applied, but evidence was unavailable
//	passed/failed   it applied and was attempted
//
// ValidationUnknown is an invalid internal state here and is never emitted
// intentionally.
type checkOutcome struct {
	Status ValidationStatus
	Detail string
	// ProducerUnavailable says the check could not run because the thing that
	// runs it was not reachable, as distinct from not being attempted.
	//
	// Both are `not_run` and neither is a failure, but they are different
	// facts about different problems: one says the service is down, the other
	// says nobody asked. A caller that has to explain itself truthfully needs
	// to tell them apart, and parsing Detail for prose would be both fragile
	// and a way for that prose to reach somewhere it should not.
	ProducerUnavailable bool
}

func (o checkOutcome) applicable() bool { return o.Status != ValidationNotApplicable }
func (o checkOutcome) attempted() bool {
	return o.Status == ValidationPassed || o.Status == ValidationFailed
}

// fallbackSyntaxOutcome keeps the two checks as named observations rather than
// flattening them early: a caller may need to know that whole-file syntax
// passed while an applicable embedded check could not run.
type fallbackSyntaxOutcome struct {
	WholeFile checkOutcome
	Embedded  checkOutcome
}

// aggregate collapses the observations for callers that need one verdict.
//
//	any demonstrated failure            -> failed (decisive)
//	else any Unknown observation        -> unknown
//	else any applicable-but-unavailable -> not_run
//	else >=1 pass, rest not_applicable  -> passed
//	else all not_applicable             -> not_applicable
//
// Unknown surfaces AS Unknown rather than being softened to not_run. not_run
// is a deliberate statement that an applicable check could not be executed;
// Unknown means no observation was recorded at all, which is a defect in the
// producer. Collapsing the two would let an unclassified check masquerade as
// a considered one. The legacy wrapper stays fail-open for Unknown, but a
// structured consumer sees Unknown and therefore cannot claim validation
// occurred.
func (f fallbackSyntaxOutcome) aggregate() checkOutcome {
	checks := []checkOutcome{f.WholeFile, f.Embedded}
	for _, c := range checks {
		if c.Status == ValidationFailed {
			return c
		}
	}
	for _, c := range checks {
		if c.Status == ValidationUnknown {
			return checkOutcome{Status: ValidationUnknown, Detail: c.Detail}
		}
	}
	for _, c := range checks {
		if c.Status == ValidationNotRun {
			return checkOutcome{Status: ValidationNotRun, Detail: c.Detail,
				ProducerUnavailable: c.ProducerUnavailable}
		}
	}
	for _, c := range checks {
		if c.Status == ValidationPassed {
			return checkOutcome{Status: ValidationPassed}
		}
	}
	return checkOutcome{Status: ValidationNotApplicable}
}

// baselineAllowsRepair reports whether a DEMONSTRATED failure on the proposed
// bytes may still land, given what is known about the baseline on disk. Only a
// demonstrated baseline failure unlocks it: refusing an imperfect fix to an
// already-broken file guarantees the broken version survives, which is the one
// case the carveout exists for.
//
// Every other status refuses. passed is a working file to protect; not_run and
// not_applicable are absences of evidence, not evidence that the file was
// already broken; Unknown means no producer spoke at all. Treating any of them
// as "already broken" would let an unverifiable baseline unlock a regression --
// the exact direction this gate must not fail in.
func baselineAllowsRepair(baseline checkOutcome) bool {
	return baseline.Status == ValidationFailed
}

func checkFallbackSyntax(ctx *AgentContext, path, content string) (string, bool) {
	agg := fallbackSyntaxOutcomeFor(ctx, path, content).aggregate()
	return agg.Detail, agg.Status != ValidationFailed
}

// checkSandboxSyntax is the whole-file half of checkFallbackSyntax: the
// sandbox's /syntax-check in the file's own language.
func checkSandboxSyntax(ctx *AgentContext, path, content string) (string, bool) {
	o := sandboxSyntaxOutcome(ctx, path, content)
	return o.Detail, o.Status != ValidationFailed
}

// sandboxSyntaxOutcome is the structured core. APPLICABILITY IS EVALUATED
// FIRST: the extension decides whether this check applies at all, before any
// service availability question, so a missing sandbox cannot mask genuinely
// non-applicable content. That changes the structured evidence only -- the
// wrapper's allow/refuse decision is identical either way.
// sandboxSyntaxRequest builds the /syntax-check body, carrying the filename
// only in the workspace-relative form the sandbox accepts.
//
// The sandbox refuses an absolute path, a "..", or a backslash-escaped name
// (_safe_overlay_path) with HTTP 400. A refused request is indistinguishable
// from a service fault at this layer -- both land as ValidationNotRun -- so a
// path the sandbox cannot take must not be sent at all. Omitting it costs only
// the path-SCOPED checks (the Jinja-template parse); the whole-file syntax
// check still runs and can still return ValidationPassed, which is what the
// completion decision needs.
func sandboxSyntaxRequest(ctx *AgentContext, path, lang, content string) map[string]string {
	body := map[string]string{"code": content, "language": lang}
	if rel, ok := workspaceRelativeName(ctx, path); ok {
		body["filename"] = rel
	}
	return body
}

// workspaceRelativeName converts a caller's path into the relative name the
// sandbox will accept, or reports that none exists. Callers pass either the
// model's own relative path or a resolved absolute one, and both have to work.
func workspaceRelativeName(ctx *AgentContext, path string) (string, bool) {
	rel := path
	if filepath.IsAbs(rel) {
		if ctx == nil || ctx.WorkingDir == "" {
			return "", false
		}
		r, err := filepath.Rel(ctx.WorkingDir, rel)
		if err != nil {
			return "", false
		}
		rel = r
	}
	rel = filepath.Clean(rel)
	// Anything that still escapes the workspace, or is not a plain relative
	// name, is exactly what the sandbox rejects.
	if rel == "" || rel == "." || filepath.IsAbs(rel) ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		strings.Contains(rel, "\\") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func sandboxSyntaxOutcome(ctx *AgentContext, path, content string) checkOutcome {
	meta, gated := syntaxGateLanguages[strings.ToLower(filepath.Ext(path))]
	if !gated {
		return checkOutcome{Status: ValidationNotApplicable}
	}
	lang := meta.Language
	if ctx == nil || ctx.SandboxURL == "" {
		return checkOutcome{Status: ValidationNotRun, Detail: "no sandbox configured", ProducerUnavailable: true}
	}
	// The path travels too: the sandbox scopes its Jinja-template check to
	// files that are actually templates (a templates/ directory, a .jinja
	// name), which it cannot tell from the bytes alone.
	//
	// It must be WORKSPACE-RELATIVE. The sandbox writes the check file under a
	// temp workspace and refuses an absolute path or one containing ".."
	// (_safe_overlay_path) with HTTP 400, which this function maps to
	// ValidationNotRun. Callers differ: the write gates pass the model's
	// relative path, but the completion check passes a RESOLVED absolute one,
	// so sending the caller's spelling unchanged made every deliverable's
	// syntax verdict NotRun -- never ValidationPassed -- and
	// deliverablesDemonstrablyValid could never be satisfied. Measured: zero
	// successful terminals across 35 sessions, while correct artifacts were
	// reported unverified. Relativise here, at the one boundary that talks to
	// the sandbox, so every caller is fixed at once; when no safe relative
	// form exists, omit the filename and keep the check itself (the
	// pre-existing behaviour, which only forgoes path-scoped checks).
	body, err := json.Marshal(sandboxSyntaxRequest(ctx, path, lang, content))
	if err != nil {
		return checkOutcome{Status: ValidationNotRun, Detail: "request could not be built"}
	}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("POST", ctx.SandboxURL+"/syntax-check", bytes.NewReader(body))
	if err != nil {
		return checkOutcome{Status: ValidationNotRun, Detail: "request could not be built"}
	}
	req.Header.Set("Content-Type", "application/json")
	if serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+serviceToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return checkOutcome{Status: ValidationNotRun, Detail: "sandbox unreachable", ProducerUnavailable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return checkOutcome{Status: ValidationNotRun, Detail: "sandbox returned a non-200", ProducerUnavailable: true}
	}
	var out struct {
		Valid   bool     `json:"valid"`
		Errors  []string `json:"errors"`
		Status  string   `json:"status"`
		Outcome string   `json:"outcome"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return checkOutcome{Status: ValidationNotRun, Detail: "sandbox response was undecodable", ProducerUnavailable: true}
	}
	// The sandbox answered, and its checker was stopped before a verdict (a
	// wall-clock or memory ceiling, or it never started). Not a pass, not a
	// syntax error, and not the service being down.
	if out.Status == "not_run" {
		return checkOutcome{Status: ValidationNotRun, Detail: "the syntax checker stopped before a verdict (" + out.Outcome + ")"}
	}
	if out.Valid {
		return checkOutcome{Status: ValidationPassed}
	}
	first := "syntax error"
	if len(out.Errors) > 0 {
		first = out.Errors[0]
	}
	return checkOutcome{Status: ValidationFailed, Detail: first}
}

// ---------------------------------------------------------------------------
// Embedded-script gate
// ---------------------------------------------------------------------------
//
// 2026-08-01 dogfooding: the model edited a Flask app whose whole UI is one
// HTML string (`HTML_TEMPLATE = """..."""` → render_template_string) and left
// a stray closing paren inside its <script> block:
//
//	else if(key === 'ArrowDown' && direction !== 'UP') nextDirection = 'DOWN');
//
// Every gate on the write path was structurally blind to it. The Python
// compiles (the JavaScript is string content), so the sandbox's /syntax-check
// passes; the server starts and `curl /` returns 200, so the
// verification gate passes and `done` is accepted — while the game is dead in
// the browser. Nothing in the loop ever parses the JavaScript.
//
// checkEmbeddedScript closes that by routing the content through
// v3-service /internal/embedded_script_check, which tree-sitter-parses the
// JS/CSS inside <script>/<style> blocks — in .html/.htm/.jinja/.jinja2 files
// and in Python string literals.
//
// Fail-soft everywhere: no V3 URL, an unreachable service, a missing grammar,
// a parse timeout or an unsupported file type all mean "no finding", never a
// blocked write. The far side is conservative in the same direction — an
// ambiguous block (template statement tags, `<script src>`, a non-JS `type`,
// an escaped Python string) reports nothing rather than guessing.

// embeddedScriptErrPrefix marks a checkFallbackSyntax error as an
// embedded-script finding. The message is pre-formatted for the model, so
// callers hand it back verbatim instead of wrapping it in generic
// "does not parse / check your old_str" advice that would be wrong here.
const embeddedScriptErrPrefix = "embedded-script: "

// embeddedScriptExts are the file types that can CARRY an embedded script.
// Anything else short-circuits before any network call.
var embeddedScriptExts = map[string]bool{
	".py": true, ".html": true, ".htm": true, ".jinja": true, ".jinja2": true,
}

// embeddedScriptFinding mirrors one entry of the v3-service response.
type embeddedScriptFinding struct {
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Kind    string `json:"kind"`    // "javascript" | "css"
	Where   string `json:"where"`   // "the <script> block inside the Python string HTML_TEMPLATE"
	Defect  string `json:"defect"`  // "" (syntax) | "stopped_loop" | "redeclaration"
	Message string `json:"message"` // "unexpected `)`"
	// For a missing closer: the line where the unclosed block STARTS. The
	// parser reports the absence at the point it gave up, which is usually a
	// line the edit never touched.
	OpenedLine int    `json:"opened_line"`
	OpenedText string `json:"opened_text"`
	Hint       string `json:"hint"` // how to fix it
	Text       string `json:"text"` // the offending source line
}

// checkEmbeddedScript returns ("", true) when `content` has no broken embedded
// script, or when the check could not run. Returns (prefixed rejection, false)
// when v3-service confirms the embedded JavaScript/CSS does not parse.
//
// `previous` is the pre-edit file, or "" when there isn't one. With it the
// service also reports a render loop the edit stopped driving — a function a
// repeating timer used to call that now fires once and never re-arms. That
// comparison is why it lives on the far side rather than here: the service
// already has the JavaScript parsed.
func checkEmbeddedScript(ctx *AgentContext, path, content, previous string) (string, bool) {
	o := embeddedScriptOutcome(ctx, path, content, previous)
	return o.Detail, o.Status != ValidationFailed
}

// embeddedScriptOutcome is the structured core. APPLICABILITY FIRST: the
// extension and a cheap local scan decide whether this check applies at all,
// before any service question, so an absent V3 cannot mask content that never
// needed checking.
func embeddedScriptOutcome(ctx *AgentContext, path, content, previous string) checkOutcome {
	if !embeddedScriptExts[strings.ToLower(filepath.Ext(path))] {
		return checkOutcome{Status: ValidationNotApplicable}
	}
	// Cheap local pre-filter: no <script/<style anywhere means nothing to
	// check and no network call. Most gated writes stop here.
	low := strings.ToLower(content)
	if !strings.Contains(low, "<script") && !strings.Contains(low, "<style") {
		return checkOutcome{Status: ValidationNotApplicable}
	}
	if ctx == nil {
		return checkOutcome{Status: ValidationNotRun, Detail: "no agent context"}
	}
	if ctx.V3URL == "" {
		return checkOutcome{Status: ValidationNotRun, Detail: "no embedded-script service configured", ProducerUnavailable: true}
	}
	body, err := json.Marshal(map[string]string{
		"path": path, "source": content, "previous": previous})
	if err != nil {
		return checkOutcome{Status: ValidationNotRun, Detail: "request could not be built"}
	}
	base := ctx.Ctx
	if base == nil {
		base = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(base, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST",
		ctx.V3URL+"/internal/embedded_script_check", bytes.NewReader(body))
	if err != nil {
		return checkOutcome{Status: ValidationNotRun, Detail: "request could not be built"}
	}
	req.Header.Set("Content-Type", "application/json")
	if serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+serviceToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		noteCheckServiceUnavailable(ctx, "embedded-script", "service unreachable")
		return checkOutcome{Status: ValidationNotRun, Detail: "service unreachable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		noteCheckServiceUnavailable(ctx, "embedded-script", fmt.Sprintf("service returned %d", resp.StatusCode))
		return checkOutcome{Status: ValidationNotRun, Detail: "service returned a non-200"}
	}
	var out struct {
		OK       bool                    `json:"ok"`
		Findings []embeddedScriptFinding `json:"findings"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return checkOutcome{Status: ValidationNotRun, Detail: "response was undecodable"}
	}
	if !out.OK {
		return checkOutcome{Status: ValidationNotRun, Detail: "grammar missing or content not analysable"}
	}
	if len(out.Findings) == 0 {
		return checkOutcome{Status: ValidationPassed}
	}
	return checkOutcome{
		Status: ValidationFailed,
		Detail: embeddedScriptErrPrefix + formatEmbeddedScriptRejection(path, out.Findings[0]),
	}
}

// fallbackSyntaxOutcomeFor is the production producer of both observations.
// Whole-file syntax runs first and its failure is decisive: the existing short
// circuit is preserved, so no embedded request is made. The skipped embedded
// observation is then recorded from LOCAL applicability only.
func fallbackSyntaxOutcomeFor(ctx *AgentContext, path, content string) fallbackSyntaxOutcome {
	whole := sandboxSyntaxOutcome(ctx, path, content)
	if whole.Status == ValidationFailed {
		return fallbackSyntaxOutcome{WholeFile: whole, Embedded: embeddedLocalApplicability(path, content)}
	}
	return fallbackSyntaxOutcome{WholeFile: whole, Embedded: embeddedScriptOutcome(ctx, path, content, "")}
}

// embeddedLocalApplicability answers "would this check have applied?" without
// touching the service, for the skipped-after-failure case.
func embeddedLocalApplicability(path, content string) checkOutcome {
	if !embeddedScriptExts[strings.ToLower(filepath.Ext(path))] {
		return checkOutcome{Status: ValidationNotApplicable}
	}
	low := strings.ToLower(content)
	if !strings.Contains(low, "<script") && !strings.Contains(low, "<style") {
		return checkOutcome{Status: ValidationNotApplicable}
	}
	return checkOutcome{Status: ValidationNotRun, Detail: "skipped after decisive whole-file failure"}
}

// embeddedScriptGate applies the healthy->broken rule the other write gates
// use and returns the rejection text, or "" to allow the write. A file whose
// embedded script was ALREADY broken before the change is a repair-in-progress
// and is left alone; only a change that newly breaks it is blocked. Fail-soft:
// "" whenever the check couldn't run.
func embeddedScriptGate(ctx *AgentContext, path, original, edited string) string {
	synErr, ok := checkEmbeddedScript(ctx, path, edited, original)
	if ok {
		return ""
	}
	if _, wasHealthy := checkEmbeddedScript(ctx, path, original, ""); !wasHealthy {
		return ""
	}
	msg, _ := embeddedScriptRejectionFor(synErr)
	return msg
}

// liveBackgroundJobNote reports background jobs still running as the turn
// ends, for appending to the done summary.
//
// Jobs deliberately outlive the agent loop: a loop is one user message, so
// killing them here would break "start the dev server" followed by "now curl
// it". What is wrong is that they outlive it SILENTLY — the sandbox has no
// session concept and only reaps after two hours, so the next turn's
// `python app.py` fails on a bound port with no indication of why, and the
// user is never told anything is still running. Naming them keeps the
// behaviour and removes the surprise.
func liveBackgroundJobNote(ctx *AgentContext) string {
	if ctx == nil || len(ctx.BackgroundJobs) == 0 {
		return ""
	}
	ids := make([]string, 0, len(ctx.BackgroundJobs))
	for id := range ctx.BackgroundJobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var sb strings.Builder
	sb.WriteString("\n\nStill running in the sandbox:")
	for _, id := range ids {
		fmt.Fprintf(&sb, "\n  %s — %s", id, truncateStr(ctx.BackgroundJobs[id], 80))
	}
	sb.WriteString("\nThese keep their ports until stopped. Use stop_background to end them.")
	return sb.String()
}

// backgroundStopMessage is the exit gate's instruction when the run's own
// background jobs are still live: the jobs by id, and the one call that
// resolves each.
func backgroundStopMessage(ctx *AgentContext, live []string) string {
	var sb strings.Builder
	for _, id := range live {
		fmt.Fprintf(&sb, "Background job %s (%s) is still running — stop it with "+
			"stop_background {\"job_id\": %q} before finishing.\n",
			id, truncateStr(ctx.BackgroundJobs[id], 80), id)
	}
	sb.WriteString("The run cannot be confirmed finished while a process of its own may still " +
		"be writing to the workspace. If you started it only to verify your work, stop it and " +
		"then finish. If the user asked for it to stay running, say so in your summary and " +
		"finish; the run will then be reported as unfinished with the job named.")
	return sb.String()
}

// ownBackgroundJobHint names the model's own background job when a command
// just failed because that job is holding the resource.
//
// The sandbox has no session concept — a job lives until stop_background or
// the two-hour reaper — so a server the model started to verify its own work
// keeps the port, and its next `python app.py` fails against "another
// program" it has no way to identify. An observed session spent its remaining
// turns on that conflict. Returns "" when nothing is running or the failure is
// unrelated, so the common case is unchanged.
func ownBackgroundJobHint(ctx *AgentContext, errMsg string) string {
	if ctx == nil {
		return ""
	}
	if !strings.Contains(strings.ToLower(errMsg), "address already in use") &&
		!strings.Contains(strings.ToLower(errMsg), "port is already allocated") {
		return ""
	}
	if len(ctx.BackgroundJobs) == 0 {
		// Nothing this session started explains it, and the registry is
		// process-wide: a server an EARLIER session left running holds the
		// port under an id this session never saw. Ask the sandbox.
		return foreignBackgroundJobHint(ctx)
	}
	ids := make([]string, 0, len(ctx.BackgroundJobs))
	for id := range ctx.BackgroundJobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var sb strings.Builder
	sb.WriteString("\n\nThat port is held by a background job YOU started in this session")
	for _, id := range ids {
		fmt.Fprintf(&sb, "\n  job %s: %s", id, truncateStr(ctx.BackgroundJobs[id], 80))
	}
	sb.WriteString("\nStop it with stop_background before re-running, or probe the " +
		"already-running service instead of starting a second copy.")
	return sb.String()
}

// foreignBackgroundJobHint names the sandbox jobs THIS session did not start,
// so the model can stop the one holding the port.
//
// "Either identify and stop that program" is the sandbox's own advice on a
// bind failure, and until now it was unfollowable: /jobs/{id} needs an id, and
// a job from a previous session was never announced to this one. GET /jobs
// lists the registry whole. Fail-soft — an unreachable sandbox or an empty
// list adds nothing, leaving the bare bind error the model already had.
func foreignBackgroundJobHint(ctx *AgentContext) string {
	if ctx.SandboxURL == "" {
		return ""
	}
	base := ctx.Ctx
	if base == nil {
		base = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(base, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", ctx.SandboxURL+"/jobs", nil)
	if err != nil {
		return ""
	}
	if serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+serviceToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var out struct {
		Jobs []struct {
			JobID   string `json:"job_id"`
			Command string `json:"command"`
			Running bool   `json:"running"`
		} `json:"jobs"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return ""
	}
	var sb strings.Builder
	for _, j := range out.Jobs {
		if !j.Running {
			continue
		}
		fmt.Fprintf(&sb, "\n  job %s: %s", j.JobID, truncateStr(j.Command, 80))
	}
	if sb.Len() == 0 {
		return ""
	}
	return "\n\nThat port is held by a background job left running in this sandbox by an " +
		"earlier session:" + sb.String() +
		"\nStop it with stop_background and re-run, or probe the already-running service " +
		"instead of starting a second copy."
}

// isUnreadOverwrite reports whether a write_file would replace an existing
// file this session has never read and did not itself create.
//
// Separate from the ">5 lines" rule, which asks whether a surgical edit is
// cheaper than a rewrite. This asks whether the file should be replaced at
// all, and the answer is no when nobody has looked at it: edit_file and
// structural_edit already require a read first, and this was the one write
// path that did not.
func isUnreadOverwrite(ctx *AgentContext, resolvedPath string, corrupted, sessionOwned bool) bool {
	if ctx == nil || corrupted || sessionOwned {
		return false
	}
	return !ctx.WasFileRead(resolvedPath)
}

// strayCarriageReturns counts CRs that are not part of a CRLF pair.
//
// A CRLF file is normal and must not be flagged, but bare or repeated CRs in
// an old_str are a reliable signature of a model that degenerated partway
// through copying a block — observed across three sessions, one of which
// emitted a literal `\rVert` (a LaTeX fragment) in the middle of JavaScript.
func strayCarriageReturns(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '\r' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '\n' {
			i++ // a well-formed CRLF, skip its LF
			continue
		}
		n++
	}
	return n
}

// reLineNumberPrefix matches read_file's display prefix: "N<tab>".
var reLineNumberPrefix = regexp.MustCompile(`(?m)^[ \t]*\d+\t`)

// lineNumberPrefixedLines counts lines in s that still carry read_file's
// "N<tab>" prefix.
//
// read_file already says the numbers are presentation and not in the file, but
// that notice is at read time and the mistake happens at edit time, often many
// turns later. Observed live: a model read a file, then sent
// old_str="76\t const ctx = canvas.getContext('2d');" — the real line with the
// prefix still attached. Nothing matched, and the generic "not found" plus a
// closest-line hint showed it the correct text without ever naming what it had
// actually done wrong, so its next attempt kept the prefix and corrupted the
// rest of the line.
func lineNumberPrefixedLines(s string) int {
	return len(reLineNumberPrefix.FindAllString(s, -1))
}

// stripLineNumberPrefixes removes read_file's "N<tab>" display prefix.
//
// The prefix is this harness's own addition, so taking it back off is a
// mechanical undo rather than a guess about intent. Callers accept the result
// only when it then matches the file, so a strip that was not wanted cannot
// change an edit.
func stripLineNumberPrefixes(s string) string {
	return reLineNumberPrefix.ReplaceAllString(s, "")
}

// allLinesLineNumbered reports whether every non-blank line of s carries the
// prefix — the signature of a wholesale paste of read_file output, as opposed
// to a block that happens to contain a numbered line.
func allLinesLineNumbered(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !reLineNumberPrefix.MatchString(line) {
			return false
		}
	}
	return true
}

// selectorGuidance says which structural_edit selectors exist in a file, read
// from the file itself, or how to find out. It never offers a placeholder: a
// refusal that suggested "a selector (`function:NAME` or `class:NAME`)" was
// answered with the literal selector `function:NAME` and a correct whole-file
// fix as content, which was then refused too (stabilization cycle 1, W1
// aoc_slope rep1). Returns "" for a language structural_edit does not handle.
func selectorGuidance(path, source string) string {
	if !structuralLanguage(path) {
		return ""
	}
	name := filepath.Base(path)
	sels := selectorsInFile(path, source)
	if len(sels) == 0 {
		return fmt.Sprintf("%s has no top-level function, class or element a selector can name; "+
			"run outline_file on %s to see its structure", name, name)
	}
	more := ""
	if len(sels) > 8 {
		more = fmt.Sprintf(" and %d more (outline_file lists them)", len(sels)-8)
		sels = sels[:8]
	}
	return fmt.Sprintf("selectors that exist in %s: `%s`%s", name, strings.Join(sels, "`, `"), more)
}

func structuralLanguage(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py", ".html", ".htm", ".go", ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		return true
	}
	return false
}

// selectorsInFile lists selectors that name exactly one top-level node in the
// source: column-0 definitions for code, and elements that occur once for
// HTML. A name defined twice is left out, since its selector would be
// ambiguous.
func selectorsInFile(path, source string) []string {
	ext := strings.ToLower(filepath.Ext(path))
	var out []string
	if ext == ".html" || ext == ".htm" {
		low := strings.ToLower(source)
		for _, tag := range []string{"html", "head", "body", "main", "header", "nav", "footer", "script", "style"} {
			n := 0
			for _, sep := range []string{"<" + tag + ">", "<" + tag + " ", "<" + tag + "\n"} {
				n += strings.Count(low, sep)
			}
			if n == 1 {
				out = append(out, "<"+tag+">")
			}
		}
		return out
	}
	count := map[string]int{}
	var order []string
	for _, sym := range outlineByRegex(path, source) {
		prefix := sym.Kind
		if prefix == "type" && ext != ".go" {
			continue
		}
		sel := prefix + ":" + sym.Name
		if count[sel] == 0 {
			order = append(order, sel)
		}
		count[sel]++
	}
	for _, sel := range order {
		if count[sel] == 1 {
			out = append(out, sel)
		}
	}
	return out
}

// moduleLevelContentNote explains a structural_edit refusal whose content is
// not one node at all: Python content with module-level code (an import, a
// `if __name__` block, an assignment at column 0) reads as a whole file, and
// structural_edit replaces a single function or class. The note names the
// line that shows it and the tool that replaces a whole file, or edits outside
// a node. Python only, where column-0 code outside a def/class is unambiguous.
func moduleLevelContentNote(ctx *AgentContext, path, content string) string {
	if strings.ToLower(filepath.Ext(path)) != ".py" {
		return ""
	}
	for i, line := range strings.Split(content, "\n") {
		t := strings.TrimRight(line, " \t\r")
		if t == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "@") || strings.HasPrefix(t, "def ") ||
			strings.HasPrefix(t, "async def ") || strings.HasPrefix(t, "class ") ||
			t == ")" || t == "]" || t == "}" || strings.HasPrefix(t, strings.Repeat("\"", 3)) || strings.HasPrefix(t, strings.Repeat("'", 3)) {
			continue
		}
		name := filepath.Base(path)
		how := "use replace_lines with the line numbers read_file shows, or edit_file anchored on one unique line"
		if ctx != nil && ctx.SessionWrites[path] {
			how = fmt.Sprintf("write_file with the complete file is the tool that replaces all of %s", name)
		}
		return fmt.Sprintf(" Your content is not a single function or class: line %d (`%s`) is module-level code, "+
			"so it reads as a whole file, and structural_edit replaces one named node. To change the whole file, %s.",
			i+1, truncateStr(t, 60), how)
	}
	return ""
}

// v3CandidateRegression reports why a V3 candidate is worse than the content
// it was generated from, or "" when the candidate is safe to adopt. It runs
// the same healthy->broken checks the write paths enforce, but scored against
// the caller's content rather than the file on disk, so a candidate that
// regresses the model's work is dropped at the V3 boundary instead of
// surfacing downstream as a rejection the model has no way to act on.
func v3CandidateRegression(ctx *AgentContext, path, baseline, candidate string) string {
	if synErr, ok := checkFallbackSyntax(ctx, path, candidate); !ok {
		if _, baseOK := checkFallbackSyntax(ctx, path, baseline); baseOK {
			return fmt.Sprintf("it does not parse (%s)", truncateStr(synErr, 120))
		}
	}
	if embeddedScriptGate(ctx, path, baseline, candidate) != "" {
		return "it breaks an embedded script"
	}
	if introduced := editIntroducesUnresolved(ctx, path, baseline, candidate); len(introduced) > 0 {
		return fmt.Sprintf("it introduces unresolved call(s) %v", logPaths(introduced))
	}
	return ""
}

// embeddedScriptRejectionFor unwraps a checkFallbackSyntax error that turned
// out to be an embedded-script finding. (message, true) when it is one — the
// message is already model-ready — else ("", false).
func embeddedScriptRejectionFor(syntaxErr string) (string, bool) {
	if !strings.HasPrefix(syntaxErr, embeddedScriptErrPrefix) {
		return "", false
	}
	return strings.TrimPrefix(syntaxErr, embeddedScriptErrPrefix), true
}

// formatEmbeddedScriptRejection names the file, the line, the offending
// construct and the fix — and spells out WHY the model's usual verification
// missed it, because "I ran it and curled the page" is exactly the evidence
// that made the broken snake game look done.
func formatEmbeddedScriptRejection(path string, f embeddedScriptFinding) string {
	lang, block, breakage := "JavaScript", "<script>", "the browser stops running the script at that point, so the page loads but nothing on it responds"
	if f.Kind == "css" {
		lang, block, breakage = "CSS", "<style>", "the browser drops the rest of the stylesheet, so the page loads unstyled"
	}
	host := "HTML"
	if strings.ToLower(filepath.Ext(path)) == ".py" {
		host = "Python"
	}
	var sb strings.Builder
	if f.Defect == "redeclaration" {
		// Parses, but never runs: a repeated let/const is an early
		// SyntaxError, so the engine throws out the whole script before
		// executing a line of it. Every handler on the page is dead, which
		// is worse than the stray-paren case and looks identical from the
		// server side.
		fmt.Fprintf(&sb, "Your content for %s declares the same name twice in %s — it was NOT written, and %s on disk is unchanged.\n", path, f.Where, path)
		fmt.Fprintf(&sb, "line %d: %s\n", f.Line, f.Message)
		if f.Text != "" {
			fmt.Fprintf(&sb, "  %d | %s\n", f.Line, f.Text)
		}
		if f.Hint != "" {
			fmt.Fprintf(&sb, "%s\n", f.Hint)
		}
		sb.WriteString("Running the file will NOT surface this: the Python compiles, the server " +
			"still starts and the page still returns 200 — the browser refuses the script " +
			"outright, so nothing on the page responds. Re-send the edit without the second " +
			"declaration.")
		return sb.String()
	}
	if f.Defect == "stopped_loop" {
		// Not a syntax error: the JavaScript parses. The edit left a render
		// loop scheduled exactly once, so the page draws one frame and
		// freezes. Nothing downstream can see it — the file compiles, the
		// server starts, the page returns 200.
		fmt.Fprintf(&sb, "Your content for %s stops a render loop in %s — it was NOT written, and %s on disk is unchanged.\n", path, f.Where, path)
		fmt.Fprintf(&sb, "line %d: %s\n", f.Line, f.Message)
		if f.Text != "" {
			fmt.Fprintf(&sb, "  %d | %s\n", f.Line, f.Text)
		}
		if f.Hint != "" {
			fmt.Fprintf(&sb, "%s\n", f.Hint)
		}
		sb.WriteString("Running the file will NOT surface this: the JavaScript is valid, " +
			"the server still starts and the page still returns 200 — it just freezes " +
			"after one frame. Re-send the edit with the loop rescheduling itself.")
		return sb.String()
	}
	// Blame the SUBMISSION, not the file. The write was refused, so the file
	// on disk is clean — and "app.py has a JavaScript syntax error" sends the
	// model hunting a bug in app.py that is not there. Measured as an H2
	// false rejection on flask_pause: the gate was right, the wording was not.
	fmt.Fprintf(&sb, "Your content for %s has a %s syntax error in %s — it was NOT written, "+
		"and %s on disk is unchanged.\n", path, lang, f.Where, path)
	fmt.Fprintf(&sb, "line %d: %s\n", f.Line, f.Message)
	if f.OpenedLine > 0 {
		// Point at the block, not at the line the parser stopped on. Given
		// only the latter, an observed session tried to "add a `}`" to an
		// untouched `setInterval(...)` line, twice, and never converged.
		fmt.Fprintf(&sb, "  %d | %s   <- this block is never closed\n", f.OpenedLine, f.OpenedText)
		fmt.Fprintf(&sb, "  %d | %s   <- the parser gave up here\n", f.Line, f.Text)
		fmt.Fprintf(&sb, "The `}` belongs at the end of the block that opens on line %d. "+
			"Line %d is where the missing brace was noticed, not where it goes — "+
			"re-check the block you just rewrote, not that line.\n", f.OpenedLine, f.Line)
	} else {
		if f.Text != "" {
			fmt.Fprintf(&sb, "  %d | %s\n", f.Line, f.Text)
		}
		if f.Hint != "" {
			fmt.Fprintf(&sb, "%s\n", f.Hint)
		}
	}
	fmt.Fprintf(&sb,
		"Running the file will NOT surface this: the %s syntax is valid, the server "+
			"still starts and the page still returns 200 — but %s. Fix line %d inside "+
			"the %s block and re-send the corrected content; do NOT resend it unchanged.",
		host, breakage, f.Line, block)
	return sb.String()
}

// reSyntaxLineNo pulls a 1-based line number out of a Python syntax error
// message ("... (file, line 13)" or "at line 13"), when present.
var reSyntaxLineNo = regexp.MustCompile(`line (\d+)`)

// fallbackSyntaxRejection builds the tool error handed back to the model
// when the gate blocks a write. It DISTINGUISHES the two failure shapes,
// because the old one-size message ("truncated — resend complete content")
// is actively wrong for a genuine syntax bug in COMPLETE content and made
// the model reassert the same broken text (observed 2026-07-20 on a
// pytorch-model-recovery task: an f-string with nested quotes resent 5×):
//   - truncation shape (unterminated string / unexpected EOF / "never
//     closed") → the content really is cut off; resend it complete.
//   - a mid-content syntax bug → point at the offending line (quoted from
//     `content` when the error carries a line number) and tell the model to
//     FIX that line, explicitly forbidding an identical resend.
//
// fusedLineRe finds a literal backslash-n inside a comment with code-shaped
// text after it: `# ...it's a transition\n        if current_run[0][1] != ...`.
// One mis-escaped newline in the model's JSON traps the next statement inside
// the comment. The file often still parses (comments swallow anything), or
// dies later with an IndentationError whose reported line is a downstream
// casualty — measured: a rejection quoted "line 45", which was blank, while
// the fused comment sat at line 40; the model re-sent the identical content
// until the repetition breaker ended the session. The pattern is the
// two-character sequence backslash-n, not a newline.
var fusedLineRe = regexp.MustCompile(`#[^\n]*?\\n\s*(?:if |for |while |return |def |class |[A-Za-z_][A-Za-z0-9_]*\s*[=(+])`)

// fusedLineHint names the first comment-trapped statement, or "".
func fusedLineHint(content string) string {
	for i, line := range strings.Split(content, "\n") {
		if fusedLineRe.MatchString(line) {
			return fmt.Sprintf(
				" Before anything else, look at line %d: its comment contains a "+
					"literal \\n followed by code, so the statement after it is "+
					"trapped INSIDE the comment. You escaped a newline that should "+
					"be real — split that into two lines.", i+1)
		}
	}
	return ""
}

// locateSyntaxLine resolves the 1-based line a syntax error points at, and
// renders that line for quoting back. Returns (0, "") when the error carries
// no location or the location is outside the content.
//
// The model cannot fix what it cannot see. Handing back its own broken line
// is worth more than any guess at the cause: a session was told the likely
// problem was nested quotes in an f-string when it had actually dropped
// `self` from a method signature, and it re-sent the same signature four
// times.
func locateSyntaxLine(content, syntaxErr string) (int, string) {
	m := reSyntaxLineNo.FindStringSubmatch(syntaxErr)
	if m == nil {
		return 0, ""
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return 0, ""
	}
	lines := strings.Split(content, "\n")
	if n > len(lines) {
		return 0, ""
	}
	return n, fmt.Sprintf(" The offending line %d is:\n%s\n",
		n, strings.TrimRight(lines[n-1], " \t"))
}

// lastContentLine is the 1-based index of the last non-blank line, which is
// where a genuinely truncated write stops. Trailing newlines would otherwise
// make every file look like it ends several lines after its real content.
func lastContentLine(content string) int {
	lines := strings.Split(content, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return i + 1
		}
	}
	return 0
}

func fallbackSyntaxRejection(path, content, syntaxErr string) string {
	// An embedded-script finding arrives pre-formatted (it already names the
	// line and the fix); the truncation/syntax-bug fork below is about the
	// host file and would give wrong advice for JavaScript inside a string.
	if msg, isEmbedded := embeddedScriptRejectionFor(syntaxErr); isEmbedded {
		return msg
	}
	low := strings.ToLower(syntaxErr)
	truncationShape := strings.Contains(low, "unexpected eof") ||
		strings.Contains(low, "was never closed") ||
		strings.Contains(low, "unterminated") ||
		strings.Contains(low, "expected an indented block")

	// Locate the offending line before choosing the advice: it decides which
	// advice is even true, and it is the one concrete thing to hand back
	// either way. The syntax check now carries "(line N)" for Python, so this
	// resolves where it used to come up empty.
	lineNo, quoted := locateSyntaxLine(content, syntaxErr)
	quoted += fusedLineHint(content)

	// These error shapes are ambiguous. An unclosed brace on line 44 of a
	// 60-line file is a real bug; the same message on the LAST line means the
	// content really was cut off. Telling the model it was truncated when it
	// was not sends it to resend the identical file, which is precisely what
	// it did: three write_file calls, then the same call byte for byte until
	// the repetition breaker ended the session. Only claim truncation when
	// the failure is at the end, where truncation would put it.
	if truncationShape && (lineNo == 0 || lineNo >= lastContentLine(content)-1) {
		return fmt.Sprintf(
			"Your content for %s does not parse (%s) — this looks like a "+
				"truncated tool call (content cut off mid-way).%s Retry write_file "+
				"with the COMPLETE file content; if it is long, write it in full, "+
				"not in fragments.", path, truncateStr(syntaxErr, 200), quoted)
	}
	// An f-string error names a real problem inside the braces. The sandbox
	// that checks the file runs Python 3.13, where nesting the same quote
	// (`f"{d["k"]}"`, PEP 701) is valid, so the old advice -- "this
	// environment runs an older Python" -- was false, and it sent the model
	// after quotes when the error was a bracket. Point at the braces instead,
	// on the quoted line. Observed live: a session hit the wall clock
	// re-sending the same content because the advice sat in a parenthetical
	// after two other sentences, so it still leads.
	// "unexpected character after line continuation character" is Python
	// telling you a backslash is followed by something other than a newline.
	// The wording names the mechanism, not the mistake, and a model that has
	// started emitting stray backslashes cannot act on it — observed three
	// times in one session, all on the same file, until the wall clock ran
	// out. Name the character and where it is.
	lowerErr := strings.ToLower(syntaxErr)
	if strings.Contains(lowerErr, "line continuation character") {
		return fmt.Sprintf(
			"Your content for %s has a stray backslash (%s) — it was NOT "+
				"written.%s A backslash only means line-continuation when it is "+
				"the LAST character on its line; anywhere else Python rejects "+
				"the file. Remove it, or write \\\\ if you meant a literal "+
				"backslash. Do NOT resend the same content unchanged.",
			path, truncateStr(syntaxErr, 200), quoted)
	}
	if strings.Contains(lowerErr, "f-string") {
		return fmt.Sprintf(
			"Your content for %s has an f-string error (%s) — it was NOT "+
				"written.%s Look inside the braces on that line: a bracket or "+
				"brace that is never closed, an empty {}, or a conversion other "+
				"than !r, !s or !a. If the expression is long, pull it into a "+
				"variable first. Do NOT resend the same content unchanged.",
			path, truncateStr(syntaxErr, 200), quoted)
	}
	return fmt.Sprintf(
		"Your content for %s has a syntax error (%s) — it was NOT written. The "+
			"content is NOT truncated; it is complete but INVALID.%s Fix THAT "+
			"specific error on the line quoted. Do NOT resend the same content "+
			"unchanged; it will fail identically.", path, truncateStr(syntaxErr, 200), quoted)
}

// ---------------------------------------------------------------------------
// Plan adherence — track tool calls against the pre-flight plan
// ---------------------------------------------------------------------------
//
// Adherence is advisory by default: we record which planned steps a tool
// call satisfies and emit metric events, but we don't block the model.
// Hard-blocking off-plan calls would be brittle when the plan was
// suboptimal — the model often discovers correct work the planner
// missed.
//
// What we DO actively do: count the off-plan streak, and once it
// crosses planAutoReviseThreshold we regenerate the plan with whatever
// context the agent has discovered so far. That's the "plan_revise
// escape" — the agent doesn't have to know about a plan_revise tool;
// the loop notices the divergence and re-plans for it.
//
// Adherence rules:
//   - A tool call satisfies the FIRST unsatisfied plan step whose
//     action verb matches the tool name (read_file ↔ "read_file" or
//     "read", run_command ↔ "run_command" or "run").
//   - If the planned step has a target, we additionally require the
//     tool's path/command to mention that target. Loose substring
//     match — paths normalize to basename so /workspace/app.py and
//     ./app.py both match a step targeting "app.py".
//   - Steps are matched in order (first unsatisfied wins) so the
//     model can revisit a planned action without re-satisfying earlier
//     steps. Out-of-order is fine; off-plan is what we count.

const (
	// planAutoReviseThreshold is the number of consecutive off-plan
	// tool calls before we auto-revise the plan. Bumped 3→5 alongside
	// the recon-tool neutrality fix below: even with recon excluded,
	// 3 was firing on routine exploration patterns (the May 6 session
	// hit it twice on read_file/list_directory chains for templates
	// the model was hunting). 5 unmatched non-recon calls is a real
	// off-plan signal; 3 was thrashing on normal agent behavior.
	planAutoReviseThreshold = 5

	// planMaxRevisions caps how many times we'll regenerate per loop.
	// After this we give up and run plan-free for the remainder.
	planMaxRevisions = 2
)

// isReconTool returns true for tools that gather information without
// taking action. These calls are neutral for plan adherence — they
// neither satisfy plan steps (a plan rarely lists "read_file app.py"
// as a step) nor count as off-plan (recon between planned actions is
// expected and shouldn't burn the off-streak counter).
//
// Without this, the agent's natural "look around before changing
// anything" pattern triggered plan revisions purely from exploratory
// reads — visible in the May 6 session as 2 revisions fired purely
// from read_file/list_directory chains.
func isReconTool(name string) bool {
	switch name {
	case "read_file", "list_directory", "find_file", "search_files":
		return true
	}
	return false
}

// matchPlanStep returns the index of the first unsatisfied plan step
// that the tool call (toolName, args) satisfies, or -1 if no match.
// satisfied must be the same length as plan.Steps.
func matchPlanStep(plan *Plan, satisfied []bool, toolName string, args json.RawMessage) int {
	if plan == nil || len(plan.Steps) == 0 {
		return -1
	}
	if len(satisfied) != len(plan.Steps) {
		return -1
	}
	target := extractToolTarget(toolName, args)
	for i, step := range plan.Steps {
		if satisfied[i] {
			continue
		}
		if !actionMatchesTool(step.Action, toolName) {
			continue
		}
		// Target match is advisory — if the step has no target field
		// or the tool args don't carry an obvious target, the action
		// match alone is enough.
		if step.Target != "" && target != "" {
			if !targetsOverlap(step.Target, target) {
				continue
			}
		}
		return i
	}
	return -1
}

// verifyStepSatisfiedBy returns the index of the plan's unsatisfied verify
// step when command verifies (it probes a service or runs the program, as
// classifyCommandEvidence judges it) and runs the same program as the step's
// planned command; -1 otherwise.
func verifyStepSatisfiedBy(plan *Plan, satisfied []bool, command string) int {
	if plan == nil || plan.VerifyStep == "" || len(satisfied) != len(plan.Steps) {
		return -1
	}
	if !classifyCommandEvidence(command).Kind.verifies() {
		return -1
	}
	for i, step := range plan.Steps {
		if step.ID != plan.VerifyStep || satisfied[i] || !actionMatchesTool(step.Action, "run_command") {
			continue
		}
		if p := commandProgram(step.Target); p != "" && p == commandProgram(command) {
			return i
		}
	}
	return -1
}

// setupCommands start a line without being what it runs.
var setupCommands = map[string]bool{"cd": true, "export": true, "source": true, ".": true,
	"set": true, "pushd": true}

// commandProgram is the program a command line exists to run: the first
// segment that is not setup (`cd x &&`), past assignments and prefix words,
// as a base name, with an interpreter's version dropped (python3.12 is
// python) and `python -m X` read as X.
func commandProgram(command string) string {
	for _, seg := range commandSegments(command) {
		fields := strings.Fields(seg)
		i := commandPosition(fields)
		if i >= len(fields) {
			continue
		}
		head := filepath.Base(fields[i])
		if setupCommands[head] {
			continue
		}
		if strings.HasPrefix(head, "python") {
			head = "python"
			if i+2 < len(fields) && fields[i+1] == "-m" {
				return fields[i+2]
			}
		}
		return head
	}
	return ""
}

// inlineInterpreterFlags are the flags that make an interpreter run code
// given on its command line rather than a file.
var inlineInterpreterFlags = map[string][]string{
	"python": {"-c"},
	"node":   {"-e", "--eval", "-p", "--print"},
	"ruby":   {"-e"},
	"perl":   {"-e", "-E"},
	"php":    {"-r"},
}

// inlineCode returns the code an interpreter segment runs from its command
// line (`python -c "..."`), and whether the segment is such a run. The code
// is cut from the raw segment, so its own line breaks survive.
func inlineCode(segment string) (string, bool) {
	fields := strings.Fields(segment)
	i := commandPosition(fields)
	if i >= len(fields) {
		return "", false
	}
	prog := filepath.Base(fields[i])
	if strings.HasPrefix(prog, "python") {
		prog = "python"
	}
	for j := i + 1; j < len(fields); j++ {
		// Past the script, the module or stdin, a flag is the program's own
		// argument: `node app.js -e` runs app.js.
		if tok := strings.Trim(fields[j], `"'`); tok == "-" || tok == "-m" ||
			(!strings.HasPrefix(tok, "-") && filepath.Ext(tok) != "") {
			return "", false
		}
		for _, f := range inlineInterpreterFlags[prog] {
			if fields[j] != f {
				continue
			}
			code := strings.Join(fields[j+1:], " ")
			if k := flagOffset(segment, f); k >= 0 {
				code = segment[k+len(f):]
			}
			return strings.Trim(strings.TrimSpace(code), `"'`), true
		}
	}
	return "", false
}

// flagOffset is the byte offset of flag as a whole word in segment, or -1.
func flagOffset(segment, flag string) int {
	for from := 0; ; {
		k := strings.Index(segment[from:], flag)
		if k < 0 {
			return -1
		}
		k += from
		end := k + len(flag)
		if (k == 0 || unicode.IsSpace(rune(segment[k-1]))) &&
			(end == len(segment) || unicode.IsSpace(rune(segment[end]))) {
			return k
		}
		from = end
	}
}

// inlineCodeTouchesNothing reports whether every segment a passing command
// binds files from runs inline code that neither names nor imports a file
// this session changed. Such a run exercised nothing the work is made of. A
// `cd` among the segments only places the run.
func inlineCodeTouchesNothing(ctx *AgentContext, ev commandEvidence) bool {
	changed := changedPathsForCoverage(ctx)
	ran := false
	for _, seg := range ev.Covering {
		fields := strings.Fields(seg)
		if k := commandPosition(fields); k < len(fields) && fields[k] == "cd" {
			continue
		}
		code, ok := inlineCode(seg)
		if !ok {
			return false
		}
		ran = true
		// The code runs to the end of the segment, so a file passed to it as
		// an argument is in it too.
		for _, p := range changed {
			for _, stmt := range strings.Split(code, ";") {
				if sourceReferencesPath(strings.TrimSpace(stmt), p) {
					return false
				}
			}
		}
	}
	return ran
}

// actionMatchesTool reports whether step.Action describes the same
// operation as a tool call named toolName. We check both directions
// (action→tool and tool→action) and normalize underscores so plans
// written as "read file" or "read_file" both match read_file.
func actionMatchesTool(action, toolName string) bool {
	if action == "" || toolName == "" {
		return false
	}
	a := strings.ToLower(strings.ReplaceAll(action, "_", " "))
	t := strings.ToLower(strings.ReplaceAll(toolName, "_", " "))
	if a == t || strings.Contains(a, t) {
		return true
	}
	// Also allow the verb stem ("read" matches "read_file" tool).
	verb := strings.SplitN(t, " ", 2)[0]
	if verb != "" && strings.HasPrefix(a, verb) {
		return true
	}
	// The edit tools are one family. A plan step that says "edit_file
	// stats.py" is satisfied by editing stats.py, whichever edit tool did it
	// — the step describes the change, not the spelling of the call.
	//
	// Measured: add_function planned `s1: edit_file`, added the function with
	// insert_after, and was told at the exit that s1 had not landed. The
	// statement was false: the file had been edited, the test passed, and the
	// session spent its three plan-gate bounces arguing about it.
	//
	// This does not loosen the gate. Family membership only decides the ACTION
	// half; the target half still has to agree, and it now can, because these
	// tools finally report their path above. An insert_after on some other
	// file satisfies nothing.
	if editToolFamily[toolName] && editToolFamily[strings.ReplaceAll(a, " ", "_")] {
		return true
	}
	return false
}

// editToolFamily is the set of tools that change part of an existing file.
//
// write_file is deliberately NOT a member. Replacing a whole file is a
// different act from editing one, and TestActionMatchesTool has pinned
// "write_file" against the edit_file tool since before this: a plan that says
// to rewrite a file is not satisfied by a one-line edit, or the other way
// round. That contract is untouched here.
var editToolFamily = map[string]bool{
	"edit_file": true, "structural_edit": true,
	"insert_after": true, "replace_lines": true,
}

// targetsOverlap reports whether two paths/targets refer to the same
// thing. For paths: equality or path-suffix match (so
// "templates/index.html" matches "/workspace/templates/index.html").
// For commands (anything with a space or non-path char): loose
// substring match so "curl http://localhost:5000/" matches a plan
// target of "curl http://localhost:5000/hello".
//
// Path-shaped strings require a path-component boundary: without it,
// "app.py" would match "tests/test_app.py" and reads of the test file
// would tick off the source-file plan step.
func targetsOverlap(planTarget, toolTarget string) bool {
	a := strings.ToLower(strings.TrimSpace(planTarget))
	b := strings.ToLower(strings.TrimSpace(toolTarget))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	a = strings.TrimPrefix(a, "./")
	b = strings.TrimPrefix(b, "./")
	if a == b {
		return true
	}
	// Path-suffix match: basename or last-N-components alignment.
	if strings.HasSuffix(b, "/"+a) || strings.HasSuffix(a, "/"+b) {
		return true
	}
	// Heuristic: anything with a space looks like a command rather
	// than a filename. Allow substring there so partial command
	// matches still count.
	if strings.ContainsAny(a, " \t") || strings.ContainsAny(b, " \t") {
		return strings.Contains(a, b) || strings.Contains(b, a)
	}
	return false
}

// extractToolTarget returns the most useful "target" string for a
// tool call: file path for file tools, command string for run_command,
// path for list_directory. Empty when the tool has no clear target
// (e.g. plan_revise itself).
func extractToolTarget(toolName string, args json.RawMessage) string {
	switch toolName {
	case "read_file", "delete_file":
		var x struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(args, &x) == nil {
			return x.Path
		}
	case "write_file":
		var x WriteFileInput
		if json.Unmarshal(args, &x) == nil {
			return x.Path
		}
	case "edit_file", "structural_edit", "insert_after", "replace_lines":
		// Every edit tool takes `path`. Only edit_file was listed, so a step
		// satisfied by structural_edit/insert_after/replace_lines produced no
		// target at all, and the target check was skipped for exactly the
		// tools whose file it most needed to know.
		var x struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(args, &x) == nil {
			return x.Path
		}
	case "run_command":
		var x RunCommandInput
		if json.Unmarshal(args, &x) == nil {
			return x.Command
		}
	case "list_directory":
		var x struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(args, &x) == nil {
			return x.Path
		}
	}
	return ""
}

// recordPlanAdherence is called from the agent loop after each
// tool-call dispatch. It updates ctx.PlanStepsSatisfied and
// ctx.PlanOffStreak, emits a "plan_adherence" metric, and returns
// true if the off-streak crossed the auto-revise threshold (caller
// should regenerate the plan).
func recordPlanAdherence(ctx *AgentContext, toolName string, args json.RawMessage, success bool) bool {
	if ctx.Plan == nil {
		return false
	}
	if ctx.PlanStepsSatisfied == nil {
		ctx.PlanStepsSatisfied = make([]bool, len(ctx.Plan.Steps))
	}

	idx := matchPlanStep(ctx.Plan, ctx.PlanStepsSatisfied, toolName, args)

	// A write that landed with an unresolved parse warning has not finished
	// its step, whatever the tool result says.
	//
	// Measured (family P, cycles 9 and 10). `write_file app.py` landed at
	// t=41 s carrying "does not parse (SyntaxError: unmatched ')')" — a
	// successful tool call, so s1 ticked over. From that turn on every request
	// carried "plan progress 4/5 … Done: s1 … Stay on the current step; don't
	// jump ahead and don't re-explore finished work", while app.py sat broken
	// and never run. The run spent the rest of its budget elsewhere and
	// delivered that file unchanged. The step is the work, not the call.
	if idx >= 0 && success && writeLikePlanAction(toolName) {
		if path := extractToolTarget(toolName, args); path != "" && currentValidationDetail(ctx, path) != "" {
			log.Printf("[agent] plan step %q not satisfied: %s is on disk with a failed check",
				ctx.Plan.Steps[idx].ID, logPath(path))
			success = false
		}
	}
	// The plan's verify step is satisfied by a passing verification of the
	// same program, whatever its arguments. The planner names that command by
	// guess: the smoke run of 2026-09-27 planned `curl http://127.0.0.1:5000`,
	// the app served on 5001, and the model's passing `curl -sf
	// http://127.0.0.1:5001/` did not count. The plan gate then demanded the
	// literal step after the server had been stopped, and a finished, verified
	// task ended "stopped".
	if idx < 0 && success && toolName == "run_command" {
		idx = verifyStepSatisfiedBy(ctx.Plan, ctx.PlanStepsSatisfied, extractToolTarget(toolName, args))
	}
	// Only successful tool calls count toward step satisfaction.
	// A failed run_command shouldn't tick off the verify_step.
	if idx >= 0 && success {
		ctx.PlanStepsSatisfied[idx] = true
		ctx.PlanOffStreak = 0
		ctx.Stream("plan_adherence", map[string]interface{}{
			"matched":     true,
			"step_index":  idx,
			"step_id":     ctx.Plan.Steps[idx].ID,
			"step_action": ctx.Plan.Steps[idx].Action,
			"satisfied":   countTrue(ctx.PlanStepsSatisfied),
			"total":       len(ctx.PlanStepsSatisfied),
		})
		return false
	}

	// Recon tools (read_file / list_directory / find_file / search_files)
	// are neutral: they don't satisfy steps but they don't extend the
	// off-streak either. The agent's natural exploration pattern
	// shouldn't trigger plan revisions.
	if isReconTool(toolName) {
		ctx.Stream("plan_adherence", map[string]interface{}{
			"matched":    false,
			"neutral":    true,
			"tool":       toolName,
			"off_streak": ctx.PlanOffStreak, // unchanged
			"satisfied":  countTrue(ctx.PlanStepsSatisfied),
			"total":      len(ctx.PlanStepsSatisfied),
		})
		return false
	}

	// No matching step (or the call failed) — extend the off-streak.
	ctx.PlanOffStreak++
	ctx.Stream("plan_adherence", map[string]interface{}{
		"matched":    false,
		"tool":       toolName,
		"off_streak": ctx.PlanOffStreak,
		"satisfied":  countTrue(ctx.PlanStepsSatisfied),
		"total":      len(ctx.PlanStepsSatisfied),
	})

	// Threshold check — caller should auto-revise when this returns
	// true. We also cap at planMaxRevisions so a chronically
	// off-plan run doesn't loop forever calling /v3/plan.
	if ctx.PlanOffStreak >= planAutoReviseThreshold && ctx.PlanRevisions < planMaxRevisions {
		return true
	}
	return false
}

// revisePlan regenerates the plan with whatever the agent has
// discovered since the original plan was made. The user message
// passed in is the ORIGINAL one (the goal hasn't changed); we
// suffix a short note explaining why we're re-planning so the
// planner can adjust shape.
// samePlanSteps reports whether two plans describe the same work. Compares
// action+target per step, not prose: a planner that rewords a rationale while
// producing the same steps has still produced the same plan.
func samePlanSteps(a, b *Plan) bool {
	if a == nil || b == nil || len(a.Steps) != len(b.Steps) {
		return false
	}
	for i := range a.Steps {
		if a.Steps[i].Action != b.Steps[i].Action ||
			a.Steps[i].Target != b.Steps[i].Target {
			return false
		}
	}
	return true
}

func revisePlan(ctx *AgentContext, originalUserMessage string, reason string) {
	if ctx.Plan == nil || ctx.PlanRevisions >= planMaxRevisions {
		return
	}
	// Compose a revision-aware user message. The planner prompt is
	// goal-oriented, so we keep the user's original goal verbatim
	// and append a "what we learned" note. This lets the planner
	// re-shape the plan around the new info rather than starting
	// from zero.
	noted := originalUserMessage
	if reason != "" {
		noted = fmt.Sprintf("%s\n\n[Re-planning context: %s]", originalUserMessage, reason)
	}
	log.Printf("[agent] revising plan (revision %d/%d): %s",
		ctx.PlanRevisions+1, planMaxRevisions, reason)
	ctx.Stream("plan_revise", map[string]interface{}{
		"reason":   reason,
		"revision": ctx.PlanRevisions + 1,
	})

	// Carry forward what the agent has read so far — it's the most
	// concrete signal of "what the agent knows now" beyond the
	// original priority-files sample.
	pctx := samplePlanContext(ctx.WorkingDir, 6, 2000)
	for path, content := range ctx.SnapshotFilesRead() {
		if len(pctx) >= 8 {
			break
		}
		// Use relative path if possible so the planner key matches
		// what the agent will pass to read_file/edit_file later.
		rel := path
		if strings.HasPrefix(path, ctx.WorkingDir+"/") {
			rel = strings.TrimPrefix(path, ctx.WorkingDir+"/")
		}
		s := content
		if len(s) > 2000 {
			s = s[:2000] + "\n... (truncated)"
		}
		pctx[rel] = s
	}

	req := V3PlanRequest{
		UserMessage:    noted,
		WorkingDir:     ctx.WorkingDir,
		ProjectContext: pctx,
		NCandidates:    3,
	}
	plan, err := callV3PlanStreaming(ctx.Ctx, ctx.V3URL, req, func(stage, detail string, data map[string]interface{}) {
		switch stage {
		case "token", "llm_start", "llm_end":
			return
		}
		payload := map[string]interface{}{"stage": stage, "detail": detail, "revision": ctx.PlanRevisions + 1}
		for k, v := range data {
			payload[k] = v
		}
		ctx.Stream("v3_plan", payload)
	})
	ctx.PlanRevisions++
	if err != nil || plan == nil {
		log.Printf("[agent] plan revision failed: %v — continuing with previous plan", err)
		return
	}
	// A revision that comes back identical is not a revision. Observed live:
	// the model went off-plan for five calls, the gate re-planned, and the
	// planner returned the same three steps — so the streak reset and the
	// same plan kept bouncing it. When re-planning cannot produce anything
	// different, the plan is not what is wrong: the model cannot execute it.
	// Dropping it beats gating the rest of the turn on a plan already proven
	// unfollowable.
	if samePlanSteps(ctx.Plan, plan) {
		log.Printf("[agent] plan revision returned the same steps — dropping the plan")
		ctx.Plan = nil
		ctx.PlanStepsSatisfied = nil
		ctx.PlanOffStreak = 0
		ctx.Stream("plan_revise", map[string]interface{}{
			"reason":   "revision produced the same steps; continuing without a plan",
			"revision": ctx.PlanRevisions,
			"dropped":  true,
		})
		return
	}

	ctx.Plan = plan
	ctx.PlanStepsSatisfied = make([]bool, len(plan.Steps))
	ctx.PlanOffStreak = 0

	// Re-emit the full plan structure so renderers replace the
	// previous plan view with the revised one. Same shape as the
	// initial generatePlan emission so consumers can use one code
	// path for both.
	planPayload := map[string]interface{}{
		"steps":         plan.Steps,
		"verify_step":   plan.VerifyStep,
		"rationale":     plan.Rationale,
		"winning_score": plan.WinningScore,
		"revision":      ctx.PlanRevisions,
	}
	ctx.Stream("plan_loaded", planPayload)
}

func countTrue(bs []bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// Plan-progress reminder injection. May 10 2026.
//
// Long multi-file tasks (e.g. "redo all 10 templates to match a SaaS
// design") lose sight of the plan once conversation trimming kicks in.
// The plan is generated up front via /v3/plan and stashed on ctx.Plan,
// and PlanStepsSatisfied tracks which steps have been hit — but neither
// surfaces back to the model after the original plan-rendering message
// drops out of the trim window.
//
// Fix: at the START of each LLM call we render a compact plan-progress
// "[system note]: ..." line and prepend it to the messages slice
// passed to callLLMOnce. The note is EPHEMERAL — it's not appended to
// ctx.Messages, so it doesn't accumulate or get re-trimmed. Every
// turn, the model sees: "step 3 of 7 — currently working on edit
// templates/dashboard.html; done: index.html, contact.html; remaining:
// pricing.html, services.html, ...".
//
// Cost: ~150 chars per turn. Cheap compared to letting the model
// re-read all the templates to remember what's done.

// buildPlanReminder returns a one-line "[system note]" string with
// plan progress, or "" if no plan is active. The caller prepends this
// to the messages slice passed to a single LLM call — it's not added
// to ctx.Messages, so it doesn't bloat history.
func buildPlanReminder(ctx *AgentContext) string {
	if ctx.Plan == nil || len(ctx.Plan.Steps) == 0 {
		return ""
	}
	if ctx.PlanStepsSatisfied == nil {
		ctx.PlanStepsSatisfied = make([]bool, len(ctx.Plan.Steps))
	}

	total := len(ctx.Plan.Steps)
	doneCount := 0
	doneIDs := make([]string, 0, total)
	remainingIDs := make([]string, 0, total)
	var current *PlanStep

	for i := range ctx.Plan.Steps {
		step := &ctx.Plan.Steps[i]
		if i < len(ctx.PlanStepsSatisfied) && ctx.PlanStepsSatisfied[i] {
			doneCount++
			doneIDs = append(doneIDs, step.ID)
		} else {
			if current == nil {
				current = step
			}
			remainingIDs = append(remainingIDs, step.ID)
		}
	}

	if current == nil {
		// All steps satisfied — the model should be on the verify step
		// or about to emit done. Surface that explicitly.
		return fmt.Sprintf(
			"[system note]: plan complete (%d/%d steps satisfied). Verify your work via `%s` if you haven't already, then emit `done` with a summary of what landed.",
			doneCount, total, planVerifyHint(ctx.Plan))
	}

	doneFrag := "none yet"
	if len(doneIDs) > 0 {
		doneFrag = strings.Join(doneIDs, ", ")
	}
	return fmt.Sprintf(
		"[system note]: plan progress %d/%d — currently on step %q (%s %s). Done: %s. Remaining: %s. Stay on the current step until it's complete; don't jump ahead and don't re-explore finished work.",
		doneCount, total, current.ID, current.Action, current.Target,
		doneFrag, strings.Join(remainingIDs, ", "),
	)
}

func planVerifyHint(p *Plan) string {
	if p == nil || p.VerifyStep == "" {
		return "the appropriate test/curl/run command"
	}
	return p.VerifyStep
}

// Asset-graph lint: cross-file coherence checks for small web projects.
// The sandbox verifies each file in isolation (compile, run, HTTP 200),
// so a project can pass every check while its files ignore each other —
// a template no route renders, a static script no page loads, an href
// to a file that doesn't exist. All three shapes appeared in the
// 2026-07-18 snake-game session. Findings are advisory text handed back
// to the model as [system note]s; nothing here blocks a write or a
// done — the stuck-pattern detectors stay the only loop-breakers.

const (
	// assetLintMaxFiles bounds the workspace walk. Past this the project
	// is no longer "small", reference search gets quadratic-ish, and a
	// framework's asset pipeline makes textual matching wrong anyway.
	assetLintMaxFiles = 400
	// assetLintMaxFileBytes skips huge files during content search.
	assetLintMaxFileBytes = 256 * 1024
)

var (
	// src/href values worth resolving as local paths. Skips externals,
	// anchors, data URIs, protocol-relative, and templated values.
	reSrcHref = regexp.MustCompile(`(?i)\b(?:src|href)\s*=\s*["']([^"']+)["']`)
	// url_for('static', filename='x.js') — Flask's canonical static ref.
	reURLFor = regexp.MustCompile(`url_for\(\s*['"]static['"]\s*,\s*filename\s*=\s*['"]([^'"]+)['"]`)
	// render_template('name.html') — referenced template must exist.
	reRenderTemplate = regexp.MustCompile(`render_template\(\s*['"]([^'"]+)['"]`)
	// {% extends "base.html" %} / {% include "nav.html" %}.
	reJinjaRef = regexp.MustCompile(`\{%\s*(?:extends|include)\s+['"]([^'"]+)['"]`)
	// fetch('/path'...) with a local absolute path (quote or backtick).
	reFetchURL = regexp.MustCompile("fetch\\(\\s*[`'\"](/[^`'\"?#{]*)")
	// <form action="/path">.
	reFormAction = regexp.MustCompile(`(?i)\baction\s*=\s*["'](/[^"'?#{]*)["']`)
	// @app.route('/path') / @bp.route(...).
	reFlaskRoute = regexp.MustCompile(`@\w+\.route\(\s*['"]([^'"]+)['"]`)
)

// assetLintFindings walks the project under workingDir and returns
// advisory findings about the template/static/reference graph. Returns
// nil for big projects (bounded walk) and on any filesystem trouble —
// this is a best-effort advisory pass, never a blocker.
// lintFile is one workspace source file the asset lint reasons about.
type lintFile struct {
	rel     string
	content string
}

// assetLintFiles loads the files the lint reasons about. false means the
// project is too large to judge, and every lint answer is then "nothing".
func assetLintFiles(workingDir string) ([]lintFile, bool) {
	var files []lintFile
	count := 0
	dir := openConfinedDir(workingDir) // reads stay inside the workspace
	defer dir.Close()
	_ = dir.Walk(func(rel string, entry fs.DirEntry, err error) error { // its one error, "too large", is in count
		if err != nil {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if strings.HasPrefix(name, ".") || name == "node_modules" ||
				name == "venv" || name == "__pycache__" {
				return fs.SkipDir
			}
			return nil
		}
		count++
		if count > assetLintMaxFiles {
			return fmt.Errorf("project too large")
		}
		if info, ierr := entry.Info(); ierr != nil || info.Size() > assetLintMaxFileBytes {
			return nil
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case ".py", ".html", ".htm", ".js", ".css", ".jinja", ".jinja2":
			data, rerr := dir.ReadFile(rel)
			if rerr != nil {
				return nil
			}
			files = append(files, lintFile{rel: filepath.ToSlash(rel), content: string(data)})
		}
		return nil
	})
	if count > assetLintMaxFiles {
		return nil, false
	}
	return files, true
}

// assetLintFindings is every finding, advisory and contract alike, in one
// list. The mutation-time note and the exit gate ask the two halves
// separately; this keeps the single-list callers unchanged.
func assetLintFindings(workingDir string) []string {
	files, ok := assetLintFiles(workingDir)
	if !ok {
		return nil
	}
	advisory, contract := assetLintFindingsFor(workingDir, files)
	return append(advisory, contract...)
}

// routeContractFindings is only the findings that describe a page which
// cannot work: a form action or fetch() target that no declared route serves.
// These are not advice. A user clicking that form gets a 404 whatever else the
// project does right, so the exit gate asks for this list by itself.
func routeContractFindings(workingDir string) []string {
	files, ok := assetLintFiles(workingDir)
	if !ok {
		return nil
	}
	_, contract := assetLintFindingsFor(workingDir, files)
	return contract
}

// assetLintFindingsFor separates what is advice (an orphaned template, a
// dangling href) from what is a broken contract between two files the model
// wrote (a submit target with no handler).
func assetLintFindingsFor(workingDir string, files []lintFile) (advisory, contract []string) {
	var findings []string
	allOther := func(self string) string {
		var b strings.Builder
		for _, f := range files {
			if f.rel != self {
				b.WriteString(f.content)
				b.WriteByte('\n')
			}
		}
		return b.String()
	}
	htmlCount := 0
	routeSet := []*regexp.Regexp{}
	routeRaw := []string{}
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.rel))
		if ext == ".html" || ext == ".htm" {
			htmlCount++
		}
		if ext == ".py" {
			for _, m := range reFlaskRoute.FindAllStringSubmatch(f.content, -1) {
				raw := strings.TrimSuffix(m[1], "/")
				if raw == "" {
					raw = "/"
				}
				// '<int:id>'-style segments match any single path segment.
				var b strings.Builder
				b.WriteString("^")
				for _, seg := range strings.Split(raw, "/") {
					if seg == "" {
						continue
					}
					b.WriteString("/")
					if strings.HasPrefix(seg, "<") && strings.HasSuffix(seg, ">") {
						b.WriteString("[^/]+")
					} else {
						b.WriteString(regexp.QuoteMeta(seg))
					}
				}
				if b.String() == "^" {
					b.WriteString("/")
				}
				b.WriteString("/?$")
				if re, err := regexp.Compile(b.String()); err == nil {
					routeSet = append(routeSet, re)
					routeRaw = append(routeRaw, m[1])
				}
			}
		}
	}
	routeMatches := func(target string) bool {
		t := strings.TrimSuffix(target, "/")
		if t == "" {
			t = "/"
		}
		for _, re := range routeSet {
			if re.MatchString(t) {
				return true
			}
		}
		return false
	}

	for _, f := range files {
		switch {
		case strings.HasPrefix(f.rel, "templates/"):
			base := filepath.Base(f.rel)
			if others := allOther(f.rel); !strings.Contains(others, base) {
				msg := fmt.Sprintf(
					"%s is referenced by nothing (no render_template call or include names %q).",
					f.rel, base)
				// render_template_string elsewhere is the smell that
				// pairs with an orphaned template (2026-07-18 snake
				// session: model inlined the page and orphaned both
				// the template and its static script).
				if strings.Contains(others, "render_template_string") {
					msg += " A .py file builds its page inline with render_template_string instead — either render this template or delete it."
				}
				findings = append(findings, msg)
			}
		case strings.HasPrefix(f.rel, "static/"):
			base := filepath.Base(f.rel)
			relUnderStatic := strings.TrimPrefix(f.rel, "static/")
			others := allOther(f.rel)
			if !strings.Contains(others, base) && !strings.Contains(others, relUnderStatic) {
				findings = append(findings, fmt.Sprintf(
					"%s is referenced by nothing (no <script src>, <link href>, or url_for('static', ...) names it).",
					f.rel))
			}
		default:
			// Flat-layout orphans: a .js/.css living beside .html files
			// (no templates/static dirs) is subject to the same rule —
			// three mini-bench tasks inlined a duplicate <script> and
			// orphaned the companion file, invisible to the prefix-keyed
			// rules above. Only fires when the project has HTML at all
			// (a pure node/python lib's entry file is legitimately
			// unreferenced).
			ext := strings.ToLower(filepath.Ext(f.rel))
			if (ext == ".js" || ext == ".css") && htmlCount > 0 {
				if !strings.Contains(allOther(f.rel), filepath.Base(f.rel)) {
					findings = append(findings, fmt.Sprintf(
						"%s is referenced by nothing — if a page should load it, add the <script src>/<link href>; if its content was inlined instead, delete the file.",
						f.rel))
				}
			}
		}

		// Referenced-but-missing templates: render_template('x') in .py,
		// {% extends/include %} in templates. The snake fix session
		// shipped an errorhandler rendering templates/404.html that did
		// not exist — every 404 became a 500.
		ext := strings.ToLower(filepath.Ext(f.rel))
		var tmplRefs []string
		if ext == ".py" {
			for _, m := range reRenderTemplate.FindAllStringSubmatch(f.content, -1) {
				tmplRefs = append(tmplRefs, m[1])
			}
		}
		if ext == ".html" || ext == ".htm" || ext == ".jinja" || ext == ".jinja2" {
			for _, m := range reJinjaRef.FindAllStringSubmatch(f.content, -1) {
				tmplRefs = append(tmplRefs, m[1])
			}
		}
		for _, name := range tmplRefs {
			if strings.Contains(name, "{{") {
				continue
			}
			rel := filepath.FromSlash(name)
			// A name escaping templates/ is dangling by definition (Jinja
			// loaders refuse traversal) — report it WITHOUT the Stat probe,
			// which stays contained to workingDir.
			dangling := !filepath.IsLocal(rel)
			if !dangling {
				_, err := os.Stat(filepath.Join(workingDir, "templates", rel))
				dangling = err != nil
			}
			if dangling {
				findings = append(findings, fmt.Sprintf(
					"%s references template %q, but templates/%s does not exist.",
					f.rel, name, name))
			}
		}

		// Route-contract check: fetch()/form-action URLs must correspond
		// to a declared Flask route. Mini-bench t01 generated a JS
		// frontend calling REST endpoints in a style the backend half
		// implemented differently — page loads, halves can't talk.
		if len(routeSet) > 0 && (ext == ".js" || ext == ".html" || ext == ".htm") {
			seen := map[string]bool{}
			for _, re := range []*regexp.Regexp{reFetchURL, reFormAction} {
				for _, m := range re.FindAllStringSubmatch(f.content, -1) {
					target := m[1]
					if seen[target] || strings.HasPrefix(target, "/static/") {
						continue
					}
					seen[target] = true
					if !routeMatches(target) {
						contract = append(contract, fmt.Sprintf(
							"%s calls %q, but no Flask route matches it (routes: %s).",
							f.rel, target, strings.Join(routeRaw, ", ")))
					}
				}
			}
		}
	}

	// Dangling local references: src/href/url_for pointing at files that
	// don't exist in the workspace.
	seenDangling := map[string]bool{}
	for _, f := range files {
		for _, m := range reSrcHref.FindAllStringSubmatch(f.content, -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "//") ||
				strings.HasPrefix(target, "#") || strings.HasPrefix(target, "data:") ||
				strings.HasPrefix(target, "mailto:") || strings.Contains(target, "{{") ||
				strings.Contains(target, "{%") {
				continue
			}
			target = strings.SplitN(target, "?", 2)[0]
			target = strings.SplitN(target, "#", 2)[0]
			if target == "" || target == "/" {
				continue
			}
			rel := filepath.FromSlash(strings.TrimPrefix(target, "/"))
			// A target escaping the workspace can't be served from it —
			// report as dangling without the Stat probe (contained to
			// workingDir).
			dangling := !filepath.IsLocal(rel)
			if !dangling {
				_, err := os.Stat(filepath.Join(workingDir, rel))
				dangling = err != nil
			}
			if dangling {
				key := f.rel + "→" + target
				if !seenDangling[key] {
					seenDangling[key] = true
					findings = append(findings, fmt.Sprintf(
						"%s references %q, which does not exist in the workspace.", f.rel, target))
				}
			}
		}
		for _, m := range reURLFor.FindAllStringSubmatch(f.content, -1) {
			rel := filepath.FromSlash(m[1])
			// A filename escaping static/ 404s at runtime (Flask refuses
			// traversal) — report as dangling without the Stat probe.
			dangling := !filepath.IsLocal(rel)
			if !dangling {
				_, err := os.Stat(filepath.Join(workingDir, filepath.Join("static", rel)))
				dangling = err != nil
			}
			if dangling {
				key := f.rel + "→" + m[1]
				if !seenDangling[key] {
					seenDangling[key] = true
					findings = append(findings, fmt.Sprintf(
						"%s references url_for('static', filename=%q), but static/%s does not exist.",
						f.rel, m[1], m[1]))
				}
			}
		}
	}

	sort.Strings(findings)
	return findings, contract
}

// routeContractMessage is the exit gate's instruction when a page submits to
// a route the server never defines. It carries the lint's own finding, which
// already names the target and lists the routes that do exist.
func routeContractMessage(findings []string) string {
	var sb strings.Builder
	sb.WriteString("Before finishing: a page you wrote submits to a route the server does not define, " +
		"so that part of the site cannot work.\n\n")
	for _, f := range findings {
		sb.WriteString("  " + f + "\n")
	}
	// How, not just what. Answering this with a whole-file write_file of a
	// file the session created is allowed, and it is the expensive path: the
	// fenced channel, then the full candidate pipeline again on a file that
	// already passed it and ran. Measured 2026-09-15: one added route that way
	// cost 183 s of a 570 s budget and introduced a 500 the run had 7 s left to
	// find. Adding a handler is an insert; changing an action is an edit.
	sb.WriteString("\nEither add a handler for that path -- with insert_after or structural_edit on the " +
		"existing file, not a whole-file write_file -- or change the form action / fetch URL to a " +
		"route that exists with edit_file. Then verify it with a request against the running " +
		"server. If the route is registered in a way this check cannot see, say so in your " +
		"summary and finish.")
	return sb.String()
}

// assetLintNote runs the lint and formats findings the model has not
// seen yet as one [system note] body ("" when quiet). Dedup state lives
// in ctx.AssetLintSeen so a persistent orphan is mentioned once, not
// after every subsequent write.
func assetLintNote(ctx *AgentContext) string {
	files, ok := assetLintFiles(ctx.WorkingDir)
	if !ok {
		return ""
	}
	advisory, contract := assetLintFindingsFor(ctx.WorkingDir, files)
	if len(advisory)+len(contract) == 0 {
		return ""
	}
	if ctx.AssetLintSeen == nil {
		ctx.AssetLintSeen = make(map[string]bool)
	}
	fresh := func(in []string) []string {
		var out []string
		for _, f := range in {
			if !ctx.AssetLintSeen[f] {
				ctx.AssetLintSeen[f] = true
				out = append(out, f)
			}
		}
		return out
	}
	advisory, contract = fresh(advisory), fresh(contract)
	if len(advisory)+len(contract) == 0 {
		return ""
	}
	var sb strings.Builder
	if len(advisory) > 0 {
		sb.WriteString("Project structure check: " + strings.Join(advisory, " ") +
			" This is advisory — fix it if these files are meant to work together.")
	}
	// A submit target with no handler was delivered under the same
	// "advisory" wording as an orphaned file, and a model reading it as
	// advice shipped a form that 404s (acceptance run, 2026-09-15). It is
	// a defect, and the run will be asked about it before it can finish.
	if len(contract) > 0 {
		if sb.Len() > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString("Route check: " + strings.Join(contract, " ") +
			" This is a defect, not advice: that page cannot submit. Add the route or " +
			"fix the target before finishing.")
	}
	return sb.String()
}

// v3RewroteBeyondTheEdit reports whether a V3 candidate changed text the
// caller's own edit had left alone.
//
// V3 improves a whole FILE: the edit tools splice their change, then hand the
// composed file to v3-service, which regenerates it. On a small file that is
// a retype of everything the edit never touched, and a 4-bit model retyping
// 7 KB drifts — an observed session came back with `#e94562` written as
// `#e94162` and `<h1 id="msg">` as `<h1 id=" msg">`, the second of which
// makes getElementById('msg') return null at runtime. Neither is a syntax
// error, so the parse and embedded-script checks pass them.
//
// The rule: a line the edit did not remove must survive V3 intact. Compares
// line multisets so a moved or duplicated line is not mistaken for a rewrite,
// and returns the first casualty for the log. Fail-soft — an empty candidate
// or an unchanged edit reports nothing, leaving the existing behaviour.
func v3RewroteBeyondTheEdit(original, edited, improved string) string {
	if improved == "" || original == edited {
		return ""
	}
	editedCount := lineCounts(edited)
	improvedCount := lineCounts(improved)
	for _, line := range strings.Split(original, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Lines the edit kept: present in both the pre-edit file and the
		// spliced result. Anything V3 drops from that set is out-of-scope.
		if editedCount[line] > 0 && improvedCount[line] == 0 {
			return fmt.Sprintf("it rewrote a line the edit never touched: %q", truncateStr(strings.TrimSpace(line), 60))
		}
	}
	return ""
}

func lineCounts(s string) map[string]int {
	counts := make(map[string]int)
	for _, line := range strings.Split(s, "\n") {
		counts[line]++
	}
	return counts
}

// anyBackgroundJobID returns one job id this session has running, so the
// verification gate can point at the server the model already started
// instead of telling it to start another.
func anyBackgroundJobID(ctx *AgentContext) string {
	if ctx == nil || len(ctx.BackgroundJobs) == 0 {
		return ""
	}
	ids := make([]string, 0, len(ctx.BackgroundJobs))
	for id := range ctx.BackgroundJobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids[0]
}

// reMainGuard matches a top-level `if __name__ == "__main__":` (either quote
// style, any spacing). Anchored at column 0 so a nested one inside a function
// is not counted.
var reMainGuard = regexp.MustCompile(`(?m)^if\s+__name__\s*==\s*['"]__main__['"]\s*:`)

// duplicateMainGuard reports whether an edit left a .py file with more than
// one module entrypoint when it had at most one before.
//
// Observed live: structural_edit on a 3-line `index()` was handed content
// that carried an `if __name__ == "__main__": app.run(...)` block along with
// it, so the splice appended a second one and the file went from 209 lines to
// 388. Nothing caught it — the file still parses and still runs, because the
// first app.run() blocks and the second is simply dead code sitting under it.
// It is the signature of a whole-file blob smuggled through a node selector.
//
// Healthy->broken like the other write gates: a file that already had two is
// left alone. Returns "" for anything that is not Python.
func duplicateMainGuard(path, original, edited string) string {
	if strings.ToLower(filepath.Ext(path)) != ".py" {
		return ""
	}
	after := len(reMainGuard.FindAllString(edited, -1))
	if after < 2 || after <= len(reMainGuard.FindAllString(original, -1)) {
		return ""
	}
	return fmt.Sprintf(
		"%s would end up with %d `if __name__ == \"__main__\":` blocks — it was NOT written.\n"+
			"Your replacement carried the module's entrypoint along with it, so splicing it in "+
			"added a second copy. Only the first one ever runs; the rest is dead code.\n"+
			"Send ONLY the node you are replacing — the function or class body itself, with no "+
			"surrounding module-level code. If you meant to change the entrypoint, edit that "+
			"block directly with replace_lines or edit_file.",
		path, after)
}

// orphanedSymbol is one function the run added that nothing references.
type orphanedSymbol struct {
	Name string `json:"name"`
	Line int    `json:"line"`
}

// orphanedAdditions returns the functions this run ADDED to files it touched
// that nothing in those files references.
//
// The mirror of editIntroducesUnresolved: that blocks a call with no
// definition, this reports a definition with no callers. It runs at the exit
// rather than at the write, because adding a function and wiring it up on the
// next turn is normal — only finishing with it unwired is the defect.
//
// Observed on "add a done command that marks a task complete": `done_task` was
// written correctly and the argv dispatcher was never touched, so the feature
// was unreachable, `todo.py done 1` exited 0 doing nothing, and the agent
// reported it verified.
//
// Fail-soft: an unreachable service, an unparsed file, or a path the run never
// read yields nothing.
func orphanedAdditions(ctx *AgentContext) map[string][]orphanedSymbol {
	if ctx == nil || ctx.V3URL == "" || len(ctx.SessionWrites) == 0 {
		return nil
	}
	out := map[string][]orphanedSymbol{}
	for rel := range ctx.SessionWrites {
		path := resolveAgentPath(ctx, rel)
		if strings.ToLower(filepath.Ext(path)) != ".py" {
			continue
		}
		previous, seen := ctx.OriginalOf(path)
		if !seen {
			continue
		}
		current, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if orphans := postOrphanCheck(ctx, rel, previous, string(current)); len(orphans) > 0 {
			out[rel] = orphans
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func postOrphanCheck(ctx *AgentContext, rel, previous, current string) []orphanedSymbol {
	body, err := json.Marshal(map[string]string{
		"path": rel, "previous": previous, "source": current})
	if err != nil {
		return nil
	}
	base := ctx.Ctx
	if base == nil {
		base = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(base, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST",
		ctx.V3URL+"/internal/orphaned_symbols", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	if serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+serviceToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out struct {
		Orphans []orphanedSymbol `json:"orphans"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	return out.Orphans
}

// orphanedAdditionsMessage names what was added and never wired up.
func orphanedAdditionsMessage(byFile map[string][]orphanedSymbol) string {
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	var sb strings.Builder
	sb.WriteString("Cannot declare `done` yet — this run added code that nothing calls:\n")
	for _, f := range files {
		for _, o := range byFile[f] {
			fmt.Fprintf(&sb, "  %s:%d  %s\n", f, o.Line, o.Name)
		}
	}
	sb.WriteString("A function nothing references cannot run, so the feature it implements " +
		"is unreachable however correct the function itself is — and a command that does " +
		"nothing still exits 0, so running it proves nothing. Wire each one in where the " +
		"caller belongs (the argument dispatch, the route table, the caller you were asked " +
		"to change), then verify by observing the behaviour actually change. If one is " +
		"deliberately unused, say so in your `done` summary.")
	return sb.String()
}

// workspaceAlignment caches the last functional check of whether the proxy
// and the sandbox see the same filesystem at /workspace.
var (
	wsAlignMu      sync.Mutex
	wsAlignChecked time.Time
	wsAlignProblem string
)

const wsAlignTTL = 5 * time.Minute

// verifyWorkspaceAlignment returns "" when the proxy and the sandbox bind the
// same host directory, or a user-facing explanation when they do not.
//
// Both containers see the split as `/workspace`, so no configuration value
// either of them holds can reveal it — the divergence is in the host bind, and
// the only way to detect it from inside is to write a file on one side and
// read it from the other.
//
// It matters because nothing else notices. Every /health passes, the proxy
// writes files that the sandbox cannot see, `run_command` reports them
// missing, and the agent concludes its own work does not exist and gives up.
// `atlas doctor` has flagged this since 2026-07-18 and it recurred on
// 2026-08-03 after a power cut recreated one container from .env — which is
// exactly the case a health check has to cover, because nobody runs doctor
// after an unplanned reboot.
func verifyWorkspaceAlignment(ctx *AgentContext) string {
	if ctx == nil || ctx.SandboxURL == "" || ctx.WorkingDir == "" {
		return ""
	}
	wsAlignMu.Lock()
	if time.Since(wsAlignChecked) < wsAlignTTL {
		problem := wsAlignProblem
		wsAlignMu.Unlock()
		return problem
	}
	wsAlignMu.Unlock()

	token := fmt.Sprintf("atlas-mount-probe-%d", time.Now().UnixNano())
	probe := filepath.Join(ctx.WorkingDir, ".atlas-mount-probe")
	if err := os.WriteFile(probe, []byte(token), 0644); err != nil {
		return "" // can't probe; not evidence of a split
	}
	defer os.Remove(probe)

	problem := ""
	if got, ok := sandboxReadProbe(ctx); !ok {
		// Sandbox unreachable or the check could not run — fail soft.
		problem = ""
	} else if !strings.Contains(got, token) {
		problem = "The file tools and the shell are looking at different directories: " +
			"this proxy writes to one host directory and the sandbox that runs your " +
			"commands is bound to another, so files written here are invisible to " +
			"`run_command` and it will report them missing. Run `atlas workspace align` " +
			"to point both at the same directory, then retry."
	}
	wsAlignMu.Lock()
	wsAlignChecked, wsAlignProblem = time.Now(), problem
	wsAlignMu.Unlock()
	return problem
}

// sandboxReadProbe asks the sandbox to read the probe file from ITS
// /workspace. (contents, true) when the call completed, ("", false) when the
// check itself could not run.
func sandboxReadProbe(ctx *AgentContext) (string, bool) {
	// Read the probe at the path the PROXY wrote it to. Both containers mount
	// the same host directory at /workspace, so the container path is
	// identical on both sides — but only if the subdirectory is carried
	// across. Hardcoding /workspace made every session with a sandbox_subdir
	// look split, which refused 28 of 28 benchmark sessions before they ran.
	probe := filepath.Join(ctx.WorkingDir, ".atlas-mount-probe")
	body, err := json.Marshal(map[string]interface{}{
		"code":     fmt.Sprintf("print(open(%q).read())", probe),
		"language": "python",
		"timeout":  10,
	})
	if err != nil {
		return "", false
	}
	base := ctx.Ctx
	if base == nil {
		base = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(base, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST",
		ctx.SandboxURL+"/execute", bytes.NewReader(body))
	if err != nil {
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")
	if serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+serviceToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var out struct {
		Stdout string `json:"stdout"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return "", false
	}
	return out.Stdout, true
}

// resetWorkspaceAlignmentCache clears the cached verdict. Tests only — the
// TTL is what keeps this to one probe per session in normal use.
func resetWorkspaceAlignmentCache() {
	wsAlignMu.Lock()
	defer wsAlignMu.Unlock()
	wsAlignChecked, wsAlignProblem = time.Time{}, ""
}

// echoesExistingFile reports a write whose content is the file that is
// already on disk, or a truncated prefix of it.
//
// Reproducing data the session already has is never the task, and it is how
// the worst failure in the measured runs starts: asked to solve an Advent of
// Code puzzle, the model called write_file on input.txt — the fixture — and
// tried to retype 2000 lines of numbers from memory. It degenerated into
// repeating one line ~50 times, the content-loop detector cut the stream
// mid-JSON at 601 chars, the parse failed, and three identical retries ended
// the run. A sibling session got further and corrupted the fixture outright.
//
// Refusing the echo removes the whole chain, and it is cheap to recognise:
// the model has no reason to send back bytes it just read.
//
// Prefix rather than equality because the collapse truncates: by the time it
// is cut, the content is a partial copy. A short prefix proves nothing, so
// there is a floor.
func echoesExistingFile(existing, incoming string) bool {
	const minEcho = 200
	if len(existing) < minEcho || len(incoming) < minEcho {
		return false
	}
	a, b := strings.TrimSpace(existing), strings.TrimSpace(incoming)
	if a == b {
		return true
	}
	// A truncated retype: everything that arrived matches the head of the
	// file, and it is a real portion of it rather than a couple of lines.
	if len(b) < len(a) && strings.HasPrefix(a, b) {
		return true
	}
	return false
}

// resendsOwnWrite reports a write whose content is already what the file holds,
// at any size. It is asked only about a file this session wrote: re-sending
// those bytes changes nothing, and the next step is to use them.
//
// echoesExistingFile keeps its 200-byte floor, which exists for the prefix
// case (a short prefix proves nothing) and for files the session did not
// write. For the session's own file the floor let small test files through.
// In the ccc71fc smoke runs (2026-09-29), offbyone and add_function wrote a
// 76-byte test_chunk.py (and a small test_stats.py), re-sent it five or six
// times without running it, and passing work ended stopped/repeat_detector
// in 4 of 84 sessions.
func resendsOwnWrite(existing, incoming string) bool {
	a, b := strings.TrimSpace(existing), strings.TrimSpace(incoming)
	return b != "" && a == b
}

// echoedWriteRejection tells the model why copying a file back is refused.
//
// For a file this session wrote, the reason is different: the model already
// wrote exactly these bytes, and the next step is to use them. Smoke run
// 2026-09-28 (add_function rep 2): the test file landed, the model re-sent the
// same write three times and was told each time that it did not need to
// reproduce input data; the test never ran, and passing work ended "stopped".
func echoedWriteRejection(path string, sessionWrote bool) string {
	if sessionWrote {
		return fmt.Sprintf(
			"write_file on %s would not change it: %s already holds exactly this content. "+
				"You wrote it earlier in this session, and it is on disk. There is nothing to "+
				"write. The next step is to use it: run it (run_command), or run the tests that "+
				"exercise it, and read the output.",
			path, path)
	}
	return fmt.Sprintf(
		"write_file on %s would rewrite it with the contents it already has. You do not "+
			"need to reproduce a file to work with it — read_file has already shown it to "+
			"you, and code you write can read it at runtime.\n\n"+
			"If this is input or fixture data, leave it alone and write the program that "+
			"processes it. If you meant to change part of it, use replace_lines or "+
			"edit_file on the lines that differ. Retyping a large file from memory does "+
			"not work: the response degenerates into repetition and gets cut off.",
		path)
}

// v3SwappedTheLanguage refuses a V3 candidate whose content no longer looks
// like the file it is replacing.
//
// The smoke check that was supposed to catch this asks the wrong question.
// For .html it runs an HTML parser, and HTML parsers accept ANY text —
// JavaScript is perfectly valid "HTML" to a lenient parser, so the check
// passed. Measured on a "build me a snake game" session: the model wrote a
// correct 18-line index.html (<!DOCTYPE html><html lang="en">...), V3
// activated, generated candidates for the game, and replaced the file's
// contents with 149 lines of JavaScript. Zero HTML tags survived, the page
// could never render, and the session reported success.
//
// "Did the parser crash" is not the same question as "is this still the
// language this file is written in", and only the second one protects the
// user's file. Keyed off structural markers the language cannot do without,
// and it only fires on a healthy->broken transition, so a file that never
// had the markers is left alone.
func v3SwappedTheLanguage(path, original, improved string) string {
	if strings.TrimSpace(improved) == "" {
		return ""
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htm":
		// A tag of any kind. An HTML document without one is not HTML,
		// whatever a permissive parser says about it.
		hadTags := htmlTagRe.MatchString(original)
		hasTags := htmlTagRe.MatchString(improved)
		if hadTags && !hasTags {
			return "the candidate contains no HTML tags at all, so it is not an HTML document"
		}
	case ".css":
		if strings.Contains(original, "{") && !strings.Contains(improved, "{") {
			return "the candidate contains no CSS rule blocks"
		}
	}
	return ""
}

// htmlTagRe matches any opening tag or doctype — the minimum structure an
// HTML document cannot lack.
var htmlTagRe = regexp.MustCompile(`(?i)<(?:!doctype|html|head|body|div|span|p|canvas|script|style|link|meta|h[1-6]|ul|table|form|button)\b`)

// --- Phase 3A: observational deliverable ledger -----------------------------
//
// Records what happened to each session-owned deliverable. It observes only:
// nothing here changes what is written, refused, restored, or reported.
//
// The lock is held for map and struct mutation ONLY. Reading the file,
// hashing it and validating it all happen outside it, because a session mutex
// held across a sandbox round-trip would serialise the agent loop behind the
// network.

func ledgerKey(ctx *AgentContext, path string) string {
	return filepath.Clean(resolveAgentPath(ctx, path))
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ledgerEntry returns the entry for a canonical key, creating it if absent.
// Caller must hold LedgerMu.
func ledgerEntry(ctx *AgentContext, key string) *DeliverableState {
	if ctx.Ledger == nil {
		ctx.Ledger = map[string]*DeliverableState{}
	}
	d := ctx.Ledger[key]
	if d == nil {
		d = &DeliverableState{Path: key}
		ctx.Ledger[key] = d
	}
	return d
}

// ledgerSessionBytes sums checkpoint bytes held. Caller must hold LedgerMu.
func ledgerSessionBytes(ctx *AgentContext) int {
	n := 0
	for _, d := range ctx.Ledger {
		n += len(d.CheckpointBytes)
	}
	return n
}

// observeDeliverable records the CURRENT bytes of a path and, when the caller
// supplies an explicit passed verdict bound to those exact bytes, promotes
// them to the path's single checkpoint.
//
// Promotion happens ONLY from an explicit ValidationPassed whose hash matches
// the bytes just read. Never from legacy Success, warning absence, extension,
// error prose, not_run, not_applicable, failed, unknown, or a mismatch. A
// syntax pass establishes syntax; it is not runtime success and never task
// correctness.
func observeDeliverable(ctx *AgentContext, path string, content []byte,
	kind ValidationKind, status ValidationStatus, detail string) {
	if ctx == nil {
		return
	}
	key := ledgerKey(ctx, path)
	h := hashBytes(content)

	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	d := ledgerEntry(ctx, key)
	d.CurrentHash = h
	d.CurrentSize = len(content)
	d.Generation++
	d.Tombstoned = false
	d.TombstoneReason = ""
	d.ValidationKind = kind
	d.ValidationStatus = status
	d.ValidationDetail = detail
	d.ValidatedHash = h // the verdict describes exactly these bytes

	if status != ValidationPassed {
		return
	}
	// Same bytes already checkpointed: nothing to do.
	if d.CheckpointHash == h {
		return
	}
	// The kind travels with the bytes. Restoration will not swap a
	// structural pass in for a syntax failure.
	d.CheckpointKind = kind
	if len(content) > maxCheckpointFileBytes {
		d.CheckpointUnavailable = "exceeds the per-file checkpoint ceiling"
		return
	}
	// Deterministic admission: the replacement's cost is its own size minus
	// whatever this path already holds. Nothing else is evicted, so there is
	// no map-order dependence.
	projected := ledgerSessionBytes(ctx) - len(d.CheckpointBytes) + len(content)
	if projected > maxCheckpointSessionBytes {
		d.CheckpointUnavailable = "exceeds the per-session checkpoint ceiling"
		return
	}
	d.CheckpointBytes = append([]byte(nil), content...)
	d.CheckpointHash = h
	d.CheckpointUnavailable = ""
}

// observeRewrite records that ATLAS itself changed a file's bytes after the
// tool that wrote them was validated. The file is still the session's own, and
// no verdict is carried over to the new bytes.
func observeRewrite(ctx *AgentContext, path string, content []byte) {
	if ctx == nil {
		return
	}
	key := ledgerKey(ctx, path)
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	d := ledgerEntry(ctx, key)
	d.CurrentHash = hashBytes(content)
	d.CurrentSize = len(content)
	d.Generation++
	d.ValidatedHash = ""
	d.ValidationStatus = ValidationUnknown
	d.ValidationKind = ValidationKindUnknown
}

// tombstoneDeliverable records a deliberate removal. Checkpoint bytes are
// retained for a later policy decision, and automatic restoration is
// prohibited outright: resurrecting a file the user asked to delete is worse
// than losing a checkpoint.
func tombstoneDeliverable(ctx *AgentContext, path, reason string) {
	if ctx == nil {
		return
	}
	key := ledgerKey(ctx, path)
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	d := ledgerEntry(ctx, key)
	d.Tombstoned = true
	d.TombstoneReason = reason
	d.RestoreProhibited = true
	d.CurrentHash = ""
	d.CurrentSize = 0
	d.Generation++
	// The verdict described bytes that are no longer there.
	d.ValidationStatus = ValidationUnknown
	d.ValidationKind = ValidationKindUnknown
	d.ValidatedHash = ""
}

// raiseWorkspaceHazard marks the workspace concurrently mutable. Raised by
// run_background. stop_background does NOT lower it: a signalled process may
// still be flushing, so only a confirmed exit does.
func raiseWorkspaceHazard(ctx *AgentContext, key string) {
	if ctx == nil || key == "" {
		return
	}
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	if ctx.WorkspaceHazards == nil {
		ctx.WorkspaceHazards = map[string]bool{}
	}
	// Keyed, so observing the same job twice cannot raise twice and a
	// duplicate id cannot demand two reaps.
	ctx.WorkspaceHazards[key] = true
}

// clearWorkspaceHazard lowers the hazard on a CONFIRMED process exit.
func clearWorkspaceHazard(ctx *AgentContext, key string) {
	if ctx == nil || key == "" {
		return
	}
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	// Only this job's hazard. Idempotent, and it cannot underflow or reach
	// another job that is still live or unconfirmed.
	delete(ctx.WorkspaceHazards, key)
}

func workspaceHazardous(ctx *AgentContext) bool {
	if ctx == nil {
		return false
	}
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	return len(ctx.WorkspaceHazards) > 0
}

// invalidateTrackedValidation is what a shell effect does to the ledger: an
// arbitrary command may have rewritten anything, so every tracked path's
// verdict stops describing the current bytes unless a fresh rehash proves
// they are unchanged. Reads happen outside the lock.
func invalidateTrackedValidation(ctx *AgentContext) {
	if ctx == nil {
		return
	}
	ctx.LedgerMu.Lock()
	keys := make([]string, 0, len(ctx.Ledger))
	for k, d := range ctx.Ledger {
		if !d.Tombstoned {
			keys = append(keys, k)
		}
	}
	ctx.LedgerMu.Unlock()

	for _, k := range keys {
		b, err := os.ReadFile(k)
		if err != nil {
			ctx.LedgerMu.Lock()
			if d := ctx.Ledger[k]; d != nil {
				d.CurrentHash = ""
				d.ValidatedHash = ""
				d.ValidationStatus = ValidationUnknown
			}
			ctx.LedgerMu.Unlock()
			continue
		}
		h := hashBytes(b)
		ctx.LedgerMu.Lock()
		if d := ctx.Ledger[k]; d != nil && d.CurrentHash != h {
			// The bytes moved under us: the verdict is historical now.
			d.CurrentHash = h
			d.CurrentSize = len(b)
			d.Generation++
			d.ValidatedHash = ""
			d.ValidationStatus = ValidationUnknown
			d.ValidationKind = ValidationKindUnknown
		}
		ctx.LedgerMu.Unlock()
	}
}

// readLedgerBytes reads a canonical path for hashing, refusing anything past
// the read ceiling. Returns (bytes, ok); ok=false means "cannot speak about
// these bytes", which the caller turns into an unknown verdict, never a pass.
func readLedgerBytes(key string) ([]byte, bool) {
	fi, err := os.Stat(key)
	if err != nil || fi.IsDir() || fi.Size() > maxLedgerReadBytes {
		return nil, false
	}
	b, err := os.ReadFile(key)
	if err != nil {
		return nil, false
	}
	return b, true
}

// observePathFromDisk records what is ACTUALLY on disk after an effect, rather
// than what the tool said it wrote. The two differ exactly when it matters --
// a partial write, a refused edit, a command that rewrote the file afterwards
// -- and the ledger's only claim is about the bytes that are there now.
//
// A path that cannot be read gets an entry only if it already had one, and
// that entry's current hash is cleared so CurrentValidation fails closed.
func observePathFromDisk(ctx *AgentContext, path string, kind ValidationKind,
	status ValidationStatus, detail string) {
	key := ledgerKey(ctx, path)
	b, ok := readLedgerBytes(key)
	if !ok {
		ctx.LedgerMu.Lock()
		if d := ctx.Ledger[key]; d != nil {
			d.CurrentHash = ""
			d.ValidatedHash = ""
			d.ValidationStatus = ValidationUnknown
			d.ValidationKind = ValidationKindUnknown
		}
		ctx.LedgerMu.Unlock()
		return
	}
	observeDeliverable(ctx, path, b, kind, status, detail)
}

// ledgerTracks reports whether the session has already observed a path.
func ledgerTracks(ctx *AgentContext, path string) bool {
	key := ledgerKey(ctx, path)
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	return ctx.Ledger[key] != nil
}

// ledgerTombstoned reports whether the ledger records the path as gone: a
// delete, a move, or a removal a shell observation already charged.
func ledgerTombstoned(ctx *AgentContext, path string) bool {
	key := ledgerKey(ctx, path)
	ctx.LedgerMu.Lock()
	defer ctx.LedgerMu.Unlock()
	d := ctx.Ledger[key]
	return d != nil && d.Tombstoned
}

// ledgerArgPath pulls one string field out of a tool's raw args without
// binding the ledger to any tool's input struct.
func ledgerArgPath(args json.RawMessage, field string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	var s string
	if raw, present := m[field]; !present || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// recordLedgerEffect is the ledger's single production entry point, called at
// the shared tool boundary once a call has fully resolved.
//
// It observes; it decides nothing. It never inspects Success: a tool's own
// MutationStatus/ValidationStatus and the filesystem are the only inputs, and
// where they disagree the filesystem wins.
func recordLedgerEffect(name string, args json.RawMessage, ctx *AgentContext, result *ToolResult) {
	if ctx == nil || result == nil || ctx.WorkingDir == "" {
		return
	}
	// Two producers assert that disk did not change: none (there was never
	// anything to write) and refused (a gate declined bytes that were ready).
	// Neither has anything to record. This is not an optimisation: without it
	// a refused write to a path the session never owned would enter the
	// ledger as a deliverable, and the bytes it was refused for would look
	// like something this session put there.
	//
	// Unknown, unobserved and failed all fall through and are read from disk,
	// because each of them is compatible with partial bytes having landed.
	if result.MutationStatus == MutationNone || result.MutationStatus == MutationRefused {
		return
	}

	switch name {
	// Content mutators: the verdict the tool produced describes the bytes it
	// proposed, so it is admissible only if those bytes are what landed --
	// which observeDeliverable enforces by hashing what it re-reads.
	case "write_file", "edit_file", "structural_edit", "insert_after", "replace_lines":
		if p := ledgerArgPath(args, "path"); p != "" {
			observePathFromDisk(ctx, p, result.ValidationKind, result.ValidationStatus, name)
		}

	case "delete_file":
		p := ledgerArgPath(args, "path")
		if p == "" {
			return
		}
		// A refused or failed delete leaves the file in place. Tombstone on
		// the file being GONE, never on the call having been made.
		if _, err := os.Stat(ledgerKey(ctx, p)); err == nil {
			observePathFromDisk(ctx, p, ValidationKindUnknown, ValidationUnknown,
				"delete_file did not remove the file")
			return
		}
		// A path the ledger never observed and that the tool did not report
		// removing is not this session's deliverable; inventing a tombstone
		// for it would fabricate history from a failed call.
		if ledgerTracks(ctx, p) || result.MutationStatus.Applied() {
			tombstoneDeliverable(ctx, p, "deleted")
			// The one place that both confirms the absence and writes the
			// tombstone is the only place that can promote an approved
			// attempt into a fulfilled deletion. It re-checks every fact.
			promoteFulfilledDeletion(ctx, ledgerKey(ctx, p))
		}

	case "move_file":
		src := ledgerArgPath(args, "source")
		dst := ledgerArgPath(args, "destination")
		// move_file accepts a directory as the destination, so take the path
		// the tool actually resolved when it reported one.
		var out MoveFileOutput
		if len(result.Data) > 0 && json.Unmarshal(result.Data, &out) == nil && out.Destination != "" {
			dst = out.Destination
		}
		if dst != "" {
			// Fresh observation, unknown verdict: a syntax pass earned under
			// the old name says nothing about this path, and inheriting it
			// would let a rename manufacture evidence.
			observePathFromDisk(ctx, dst, ValidationKindUnknown, ValidationUnknown, "move_file destination")
		}
		if src == "" {
			return
		}
		if _, err := os.Stat(ledgerKey(ctx, src)); err == nil {
			observePathFromDisk(ctx, src, ValidationKindUnknown, ValidationUnknown,
				"move_file left the source in place")
			return
		}
		if ledgerTracks(ctx, src) || result.MutationStatus.Applied() {
			tombstoneDeliverable(ctx, src, "moved:"+ledgerKey(ctx, dst))
		}

	// Shell effects. An arbitrary command may have rewritten anything, so
	// every tracked verdict is re-proved by rehash or dropped.
	case "run_command":
		invalidateTrackedValidation(ctx)

	case "run_background":
		// The hazard has to describe work that may exist, not attempts that
		// were made. A call the tool refused before it could dispatch created
		// nothing to be hazardous about; a dispatch whose outcome is unknown
		// is exactly what the hazard is for.
		if !backgroundStartDispatched(ctx, args) {
			return
		}
		invalidateTrackedValidation(ctx)
		var out RunBackgroundOutput
		decoded := len(result.Data) > 0 && json.Unmarshal(result.Data, &out) == nil
		switch {
		case decoded && out.JobID != "" && out.Running:
			// A live job, owned by its own identity.
			raiseWorkspaceHazard(ctx, out.JobID)
		case decoded && out.JobID != "":
			// Dispatched and already gone. The rehash above is the settlement
			// it needs; nothing is left to clear later.
			clearWorkspaceHazard(ctx, out.JobID)
		default:
			// A process may exist and cannot be named. Uncertainty is not
			// resolved by ignoring it: this hazard is deliberately unclearable
			// by reaping, because there is nothing to reap.
			raiseWorkspaceHazard(ctx, hazardUnidentifiedJob)
		}

	case "stop_background":
		invalidateTrackedValidation(ctx)
		// A reaped exit code is the only proof the writer is gone. SIGTERM
		// sent, SIGKILL sent, and "stop returned" are all compatible with a
		// process still flushing.
		var out StopBackgroundOutput
		if len(result.Data) > 0 && json.Unmarshal(result.Data, &out) == nil && out.ExitCode != nil {
			clearWorkspaceHazard(ctx, out.JobID)
		}
	}
}

// --- Phase 3B: demonstrably-safer restoration -------------------------------
//
// Scope: ONE terminal. The repeat detector is where a run stops with a
// deliverable it has itself shown to be broken -- the seed-20260901 debounce5
// shape -- and it is the only producer wired to this. The other twelve done
// emitters are untouched, deliberately: a terminal that never demonstrated
// breakage has nothing to recover from, and attaching recovery to all of them
// would make a rare, evidence-bound action routine.
//
// Restoration is a system action, not a model mutation. It sets no progress
// hint, claims no V3 provenance, emits no tool call or tool result, and never
// turns a stopped run into a completed one.

// restoreDecision is what happened for ONE path. Recovery is per-path and is
// disclosed that way: there is no transaction, and a run that recovers two of
// three files must not read as if it recovered all three.
type restoreDecision struct {
	Path      string // workspace-relative, for disclosure
	Restored  bool
	Attempted bool   // a write was issued
	Reason    string // why not, or the real failure
}

// checkpointRestorable answers the eligibility question and nothing else. It
// takes the freshly-read current bytes so the decision is about what is on
// disk right now, not about what the ledger last heard.
//
// Every clause is a reason NOT to act. Unknown, not_run, not_applicable,
// unobserved, a hash that moved, a kind that does not compare, or missing
// bytes all end here, because the alternative is overwriting a user's file on
// a guess.
func checkpointRestorable(d *DeliverableState, currentHash string, hazardous bool) (bool, string) {
	switch {
	case d == nil || d.Generation == 0:
		return false, "not a deliverable this session wrote"
	case d.Tombstoned:
		return false, "deleted or moved on purpose"
	case d.RestoreProhibited:
		return false, "restoration prohibited for this path"
	case d.CheckpointHash == "":
		if d.CheckpointUnavailable != "" {
			return false, "no earlier valid version was kept (" + d.CheckpointUnavailable + ")"
		}
		return false, "no version of it was ever shown to be valid"
	case len(d.CheckpointBytes) == 0:
		return false, "the earlier valid version is no longer available"
	case len(d.CheckpointBytes) > maxCheckpointFileBytes:
		return false, "the earlier valid version is too large to hold"
	case hashBytes(d.CheckpointBytes) != d.CheckpointHash:
		// Held bytes and recorded hash disagree: something is wrong with the
		// ledger itself, so it is not allowed to touch the workspace.
		return false, "the earlier valid version could not be verified"
	case d.CheckpointHash == currentHash:
		return false, "the file already holds the last version shown to be valid"
	case d.ValidatedHash != currentHash:
		// The verdict describes bytes that are no longer there.
		return false, "the current contents were never checked"
	case d.ValidationStatus != ValidationFailed:
		// Only a DEMONSTRATED failure justifies replacing what is there.
		return false, "the current contents were not shown to be broken"
	case d.CheckpointKind == ValidationKindUnknown || d.ValidationKind == ValidationKindUnknown:
		return false, "the two versions were not checked the same way"
	case d.CheckpointKind != d.ValidationKind:
		return false, "the two versions were not checked the same way"
	case hazardous:
		return false, "a background job may still be writing"
	}
	return true, ""
}

// restoreDeliverable re-reads, decides, and -- only if every clause holds --
// puts the checkpoint back through the same atomic replace the write tools
// use. The write is then re-read and hashed: a restore that cannot prove it
// landed exactly is a failure, not a success.
func restoreDeliverable(ctx *AgentContext, key string) restoreDecision {
	rel := key
	if r, err := filepath.Rel(ctx.WorkingDir, key); err == nil && !strings.HasPrefix(r, "..") {
		rel = r
	}
	dec := restoreDecision{Path: rel}

	// Intent is checked BEFORE anything is observed. A fresh observation
	// clears the tombstone flag by design -- the path exists again -- and a
	// path the model deliberately deleted or moved must not become eligible
	// just because something later recreated it.
	ctx.LedgerMu.Lock()
	entry := ctx.Ledger[key]
	switch {
	case entry == nil || entry.Generation == 0:
		ctx.LedgerMu.Unlock()
		dec.Reason = "not a deliverable this session wrote"
		return dec
	case entry.Tombstoned:
		ctx.LedgerMu.Unlock()
		dec.Reason = "deleted or moved on purpose"
		return dec
	case entry.RestoreProhibited:
		ctx.LedgerMu.Unlock()
		dec.Reason = "restoration prohibited for this path"
		return dec
	case entry.CheckpointHash == "":
		reason := "no version of it was ever shown to be valid"
		if entry.CheckpointUnavailable != "" {
			reason = "no earlier valid version was kept (" + entry.CheckpointUnavailable + ")"
		}
		ctx.LedgerMu.Unlock()
		dec.Reason = reason
		return dec
	}
	ctx.LedgerMu.Unlock()

	// Read immediately before deciding. Anything the ledger believes is a
	// starting point; the file is the fact.
	current, ok := readLedgerBytes(key)
	if !ok {
		dec.Reason = "its current contents could not be read"
		return dec
	}
	currentHash := hashBytes(current)

	// Check these exact bytes NOW, through the same syntax contract the write
	// path uses, and record the result. This is what makes the failure a
	// fresh demonstration rather than a memory: a shell command that rewrote
	// the file left the ledger holding no verdict at all, and no verdict is
	// not evidence of breakage. When the checker cannot run, the observation
	// is unknown and nothing is restored.
	fresh := fallbackSyntaxOutcomeFor(ctx, key, string(current)).aggregate()
	freshKind := ValidationKindSyntax
	if fresh.Status == ValidationNotApplicable {
		freshKind = ValidationKindNone
	}
	observeDeliverable(ctx, key, current, freshKind, fresh.Status, fresh.Detail)

	// The hazard counter lives under the same mutex, so it is read before the
	// entry is locked rather than from inside the decision.
	hazardous := workspaceHazardous(ctx)

	ctx.LedgerMu.Lock()
	d := ctx.Ledger[key]
	eligible, reason := checkpointRestorable(d, currentHash, hazardous)
	var want []byte
	var wantHash string
	var wantKind ValidationKind
	var wantDetail string
	if eligible {
		want = append([]byte(nil), d.CheckpointBytes...)
		wantHash, wantKind, wantDetail = d.CheckpointHash, d.CheckpointKind, d.ValidationDetail
	}
	ctx.LedgerMu.Unlock()

	if !eligible {
		dec.Reason = reason
		return dec
	}

	dec.Attempted = true
	if err := atomicReplaceFile(key, want); err != nil {
		// The atomic path leaves the target untouched on failure, so the
		// current bytes are still there. Report the real error.
		dec.Reason = err.Error()
		return dec
	}
	// Prove it landed. Nothing is claimed from the write returning nil.
	after, ok := readLedgerBytes(key)
	if !ok {
		dec.Reason = "the restored file could not be read back"
		return dec
	}
	if h := hashBytes(after); h != wantHash {
		dec.Reason = "the file on disk does not match the version that was restored"
		return dec
	}

	// The ledger now describes the restored bytes, carrying the evidence that
	// was already earned for exactly this hash. No new verdict is invented.
	ctx.LedgerMu.Lock()
	if d := ctx.Ledger[key]; d != nil {
		d.CurrentHash = wantHash
		d.CurrentSize = len(want)
		d.Generation++
		d.ValidationKind = wantKind
		d.ValidationStatus = ValidationPassed
		d.ValidationDetail = wantDetail
		d.ValidatedHash = wantHash
		d.Recovered = true
	}
	ctx.LedgerMu.Unlock()

	dec.Restored = true
	log.Printf("[recovery] restored %s to the last version shown to be valid", logPath(rel))
	return dec
}

// restoreSaferDeliverables walks the session's deliverables in a stable order
// and decides each one independently. Returns only the paths where something
// happened or was deliberately declined after a demonstrated failure -- a
// path with nothing to say produces no disclosure.
func restoreSaferDeliverables(ctx *AgentContext) []restoreDecision {
	if ctx == nil || ctx.WorkingDir == "" {
		return nil
	}
	ctx.LedgerMu.Lock()
	keys := make([]string, 0, len(ctx.Ledger))
	for k, d := range ctx.Ledger {
		// A path with no checkpoint and no failure has no decision worth
		// reporting; skipping it here keeps the disclosure about recovery.
		if d.CheckpointHash == "" && d.ValidationStatus != ValidationFailed {
			continue
		}
		keys = append(keys, k)
	}
	ctx.LedgerMu.Unlock()
	sort.Strings(keys)

	var out []restoreDecision
	for _, k := range keys {
		dec := restoreDeliverable(ctx, k)
		if dec.Restored || dec.Attempted {
			out = append(out, dec)
			continue
		}
		// Declining is only worth saying when the file is actually broken.
		ctx.LedgerMu.Lock()
		broken := ctx.Ledger[k] != nil && ctx.Ledger[k].ValidationStatus == ValidationFailed
		ctx.LedgerMu.Unlock()
		if broken {
			out = append(out, dec)
		}
	}
	return out
}

// restorationDisclosure renders the three outcomes the user must be able to
// tell apart: a file put back, a file left alone, and a recovery that was
// tried and did not work. Per path, named, with no implication that the set
// moved together.
//
// Nothing from the ledger's shape appears here -- no hashes, no status names,
// no generation counts. Only what happened, in the terms a reader can act on.
func restorationDisclosure(decisions []restoreDecision) string {
	if len(decisions) == 0 {
		return ""
	}
	var restored, kept, failed []string
	for _, d := range decisions {
		switch {
		case d.Restored:
			restored = append(restored, d.Path)
		case d.Attempted:
			failed = append(failed, fmt.Sprintf("%s (%s)", d.Path, d.Reason))
		default:
			kept = append(kept, fmt.Sprintf("%s (%s)", d.Path, d.Reason))
		}
	}
	var sb strings.Builder
	if len(restored) > 0 {
		sb.WriteString(fmt.Sprintf(" Put back the last version shown to be valid, file by file: %s",
			strings.Join(restored, ", ")))
		sb.WriteString(" — each was decided on its own, and nothing else was rolled back.")
	}
	if len(kept) > 0 {
		sb.WriteString(fmt.Sprintf(" Left as they are: %s.", strings.Join(kept, ", ")))
	}
	if len(failed) > 0 {
		sb.WriteString(fmt.Sprintf(" Tried and could not restore: %s.", strings.Join(failed, ", ")))
	}
	return sb.String()
}

// hazardUnidentifiedJob owns the case where a background start may have
// dispatched and came back with no usable job id. Nothing can reap it, which
// is the point: the session cannot claim the workspace is quiet when it does
// not know what is running in it.
const hazardUnidentifiedJob = "\x00unidentified-background-job"

// backgroundStartDispatched reports whether a run_background call could have
// reached the sandbox at all, by asking the same questions the tool asks
// before it dispatches. A refusal here is provable: no request was sent, so
// no process exists and no hazard is owed.
func backgroundStartDispatched(ctx *AgentContext, args json.RawMessage) bool {
	var in RunBackgroundInput
	if json.Unmarshal(args, &in) != nil {
		return false
	}
	if ctx == nil || !ctx.TrustMode.commandsAllowed() {
		return false
	}
	if strings.TrimSpace(in.Command) == "" {
		return false
	}
	if reason := validateShellCommand(in.Command); reason != "" {
		return false
	}
	// Host verification runs nothing in the sandbox.
	return !ctx.VerifyOnHost
}

// editViewIsCurrent reports whether the model's view of `path` is current
// enough to edit it without a fresh read_file.
//
// A read makes the view current. So does a write THIS SESSION made -- but only
// while the bytes on disk are still the bytes we wrote. Session ownership on
// its own is deliberately NOT sufficient: a shell command this run issued, or
// anything else touching the workspace, can change the file underneath, and
// then the model's belief is stale exactly as if it had never read it. The test
// is identity, not ownership, and it is the same one the restoration path
// already applies (Generation > 0 && CurrentHash == diskHash), so the two
// cannot disagree about what "we still know this file" means.
//
// Measured on the 38eaa0a benchmark (aoc_slope): the session wrote solve.py,
// the syntax gate warned that it did not parse and quoted the offending line,
// and the model's correct one-line edit_file repair was refused with "file not
// read yet". Having no accepted way to change one line, it re-sent the whole
// file, tripped the identical-call detector, and the run ended without ever
// repairing the line it had been told about. A read would have been a pure
// round trip: the content it would return is the content this session just
// wrote.
func editViewIsCurrent(ctx *AgentContext, path string) bool {
	if ctx == nil {
		return false
	}
	if ctx.WasFileRead(path) {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// Unreadable or absent: nothing is known about it.
		return false
	}
	return diskIsOwnWrite(ctx, path, data)
}

// diskIsOwnWrite reports whether data -- the bytes now on disk at path -- are
// exactly the bytes this session last wrote there. Identity, not ownership:
// Generation > 0 && CurrentHash == hash(disk), the same test the restoration
// path applies.
func diskIsOwnWrite(ctx *AgentContext, path string, data []byte) bool {
	if ctx == nil {
		return false
	}
	disk := hashBytes(data)
	key := ledgerKey(ctx, path)
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[key]
	own := d != nil && !d.Tombstoned && d.Generation > 0 && d.CurrentHash == disk
	ctx.LedgerMu.Unlock()
	return own
}

// modifiedSinceSessionView reports whether path changed after the session last
// established what is in it -- by reading it, or by writing it. data is the
// content the caller just read from disk.
//
// This was an mtime-versus-read-time comparison inline in edit_file and
// structural_edit, and write_file is the one mutating tool that never records a
// read time. So after ANY write_file, the file's mtime is later than a read
// time that is either older or absent (zero), and a follow-up edit was refused
// "file modified since last read" although nothing but the session had touched
// it. editViewIsCurrent had already been taught identity for exactly this
// scenario; the check immediately after it had not, so the fix moved the
// refusal to a different message instead of removing it -- its test exercised
// the helper, never the tool.
//
// Measured across the saved benchmark and acceptance events: 9 refusals in 5
// sessions, and in all 9 the last thing to touch the path was the session's own
// write_file (4 directly, 5 with only a run_command such as `python3 solve.py`
// between). They include the four refusals in the multifile_cli-rep2 deadlock
// and aoc_sonar-rep1, where the model ran its broken solve.py as instructed,
// read the real traceback, and had its one-line repair refused.
//
// A genuine change still refuses: if a command or anything else altered the
// bytes, they no longer equal the session's own write, and the mtime rule
// applies exactly as before.
func modifiedSinceSessionView(ctx *AgentContext, path string, data []byte) bool {
	ctx.mu.Lock()
	lastRead := ctx.FileReadTimes[path]
	ctx.mu.Unlock()
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().After(lastRead) {
		return false
	}
	return !diskIsOwnWrite(ctx, path, data)
}

// insertionReparentsPython reports an insert_after that would move EXISTING
// Python statements into a different enclosing block, and describes the first
// one moved. insert_after only adds lines; it never re-indents the lines after
// the insertion point. So if an existing statement's enclosing block changes,
// the insertion has restructured code the model did not ask to touch, and the
// file can still parse -- which is why no syntax gate sees it.
//
// Measured on 84296fd smallrung_toml rep2: `elif lang == "toml":` inserted
// after line 1359 of executor_server.py, between `stderr = result.get(...)`
// and `for line in stderr.splitlines():`. The file parsed. The java branch's
// loop that turns compiler output into errors now belonged to the new toml
// branch's `except`, so java compile errors were no longer reported, and the
// toml branch raised on invalid TOML. The task scored PASS under a static
// check and was later corrected to fail.
//
// This is a structural invariant for an insertion, not a claim about
// behaviour: an insertion that leaves every existing statement in its block
// can still be wrong. Indentation is read from logical line starts, skipping
// continuation lines, triple-quoted strings and bracketed spans. If the
// original file cannot be scanned consistently the check is skipped rather
// than risk refusing a sound edit.
func insertionReparentsPython(path string, lines []string, at int, insert []string) (string, bool) {
	if strings.ToLower(filepath.Ext(path)) != ".py" || len(insert) == 0 || at < 0 || at > len(lines) {
		return "", false
	}
	orig, ok := pythonBlockParents(lines)
	if !ok {
		return "", false
	}
	merged := make([]string, 0, len(lines)+len(insert))
	merged = append(merged, lines[:at]...)
	merged = append(merged, insert...)
	merged = append(merged, lines[at:]...)
	upd, ok := pythonBlockParents(merged)
	if !ok {
		return "", false
	}
	const inserted = -3
	toOrig := func(u int) int {
		switch {
		case u < 0:
			return u
		case u < at:
			return u
		case u < at+len(insert):
			return inserted
		default:
			return u - len(insert)
		}
	}
	for j := at; j < len(lines); j++ {
		u := j + len(insert)
		if (orig[j] == notLogicalStart) != (upd[u] == notLogicalStart) {
			return fmt.Sprintf(
				"insert_after at line %d would change how existing line %d (`%s`) is read: the inserted text leaves "+
					"a string or bracket open, so that line would no longer be its own statement. Insert complete "+
					"statements only.", at, j+1, strings.TrimSpace(lines[j])), true
		}
		if orig[j] == notLogicalStart {
			continue
		}
		if was, now := orig[j], toOrig(upd[u]); was != now {
			from := "the top level of the file"
			if was >= 0 {
				from = fmt.Sprintf("`%s` (line %d)", strings.TrimSpace(lines[was]), was+1)
			}
			to := "the top level of the file"
			if now == inserted {
				to = fmt.Sprintf("your inserted `%s`", strings.TrimSpace(merged[upd[u]]))
			} else if now >= 0 {
				to = fmt.Sprintf("`%s` (line %d)", strings.TrimSpace(lines[now]), now+1)
			}
			return fmt.Sprintf(
				"insert_after at line %d would move existing code into a different block: line %d `%s` belongs to %s "+
					"and would now belong to %s. The file would still parse, but that existing code would silently "+
					"change behaviour. Nothing was written. Pick an insertion point where the next existing line is "+
					"not indented deeper than the code you insert -- usually after the LAST line of the block you are "+
					"adding next to, not in the middle of it.",
				at, j+1, strings.TrimSpace(lines[j]), from, to), true
		}
	}
	return "", false
}

const notLogicalStart = -2

// pythonBlockParents returns, for each line, the index of the logical line
// that encloses it by indentation (-1 for module level), or notLogicalStart
// for blank lines, comment-only lines and lines that continue a previous
// statement. ok is false when the scan ends inside a string or bracket, i.e.
// the scanner cannot vouch for the structure it found.
func pythonBlockParents(lines []string) ([]int, bool) {
	parents := make([]int, len(lines))
	type frame struct{ indent, index int }
	var stack []frame
	var triple string // `"""` or `'''` while inside a triple-quoted string
	depth := 0
	continued := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		start := triple == "" && depth == 0 && !continued && trimmed != "" && !strings.HasPrefix(trimmed, "#")
		if start {
			indent := pythonIndent(line)
			for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
				stack = stack[:len(stack)-1]
			}
			if len(stack) == 0 {
				parents[i] = -1
			} else {
				parents[i] = stack[len(stack)-1].index
			}
			stack = append(stack, frame{indent, i})
		} else {
			parents[i] = notLogicalStart
		}
		// Advance string/bracket state across this line.
		continued = false
		for k := 0; k < len(line); k++ {
			c := line[k]
			if triple != "" {
				if strings.HasPrefix(line[k:], triple) {
					k += 2
					triple = ""
				} else if c == '\\' {
					k++
				}
				continue
			}
			switch c {
			case '#':
				k = len(line)
			case '"', '\'':
				q := string(c)
				if strings.HasPrefix(line[k:], q+q+q) {
					triple = q + q + q
					k += 2
					continue
				}
				// single-line string: skip to its closing quote
				for k++; k < len(line); k++ {
					if line[k] == '\\' {
						k++
						continue
					}
					if line[k] == c {
						break
					}
				}
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				if depth > 0 {
					depth--
				}
			}
		}
		if triple == "" && depth == 0 && strings.HasSuffix(strings.TrimRight(stripPythonComment(line), " \t"), "\\") {
			continued = true
		}
	}
	return parents, triple == "" && depth == 0 && !continued
}

func pythonIndent(line string) int {
	n := 0
	for _, r := range line {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 8 - n%8
		default:
			return n
		}
	}
	return n
}

// stripPythonComment removes a trailing comment that is outside any string on
// a single line; used only to see whether a line ends in a continuation.
func stripPythonComment(line string) string {
	var q byte
	for k := 0; k < len(line); k++ {
		c := line[k]
		if q != 0 {
			if c == '\\' {
				k++
			} else if c == q {
				q = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			q = c
		} else if c == '#' {
			return line[:k]
		}
	}
	return line
}

// noteCheckServiceUnavailable records that a mandatory check could not run
// because the service behind it did not answer. Both checks fail open, which
// is right for a single write, but an outage must not be invisible: before
// optional generation was skipped for undeliverable candidates, every Tier 2
// write reached the same service and a failure surfaced as "V3 unavailable,
// writing directly". The client is told once per check per session; every
// later write's gate log line carries the running count.
func noteCheckServiceUnavailable(ctx *AgentContext, check, detail string) {
	if ctx == nil {
		return
	}
	// A request this session cancelled, or whose deadline passed, did not find
	// the service down.
	if ctx.Ctx != nil && ctx.Ctx.Err() != nil {
		return
	}
	ctx.mu.Lock()
	if ctx.CheckServiceFailures == nil {
		ctx.CheckServiceFailures = map[string]int{}
	}
	ctx.CheckServiceFailures[check]++
	first := ctx.CheckServiceFailures[check] == 1
	ctx.mu.Unlock()
	log.Printf("[gates] %s check could not run: %s — the write continues without it", check, detail)
	if first {
		ctx.Stream("text", map[string]string{"content": fmt.Sprintf(
			"  └─ %s check unavailable (%s); writes continue without it", check, detail)})
	}
}

// checkServiceFailureSummary is "embedded-script=2, unresolved-name=1", or "".
func (c *AgentContext) checkServiceFailureSummary() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.CheckServiceFailures) == 0 {
		return ""
	}
	names := make([]string, 0, len(c.CheckServiceFailures))
	for k := range c.CheckServiceFailures {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", k, c.CheckServiceFailures[k]))
	}
	return strings.Join(parts, ", ")
}

// writeLikePlanAction reports the plan actions that put bytes on disk, the
// only ones whose step can be left unfinished by a failing check.
func writeLikePlanAction(tool string) bool {
	switch tool {
	case "write_file", "edit_file", "structural_edit", "insert_after", "replace_lines":
		return true
	}
	return false
}
