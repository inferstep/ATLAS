// Gates: the checks the agent loop runs at its decision points — before a
// write reaches disk, and before a `done` is accepted.
//
// In this file, in order:
//
//	Structural-unresolved gate — asks v3-service whether a write or edit
//	  leaves an undefined name behind, and rejects only the unresolved names
//	  that this change introduced.
//	Syntax gate — routes fallback writes through the sandbox's
//	  /syntax-check. These are the writes that skipped the V3 pipeline, so
//	  nothing else in the loop has parsed them.
//	Embedded-script gate — parses the JS/CSS inside <script>/<style> blocks
//	  in HTML files and in Python string literals, which every other gate is
//	  structurally blind to.
//
// Also here: the V3 candidate checks (a candidate worse than the content it
// was made from, one that rewrote lines the edit left alone, one no longer in
// the file's language), the asset lint's findings (assetLintFindings,
// routeContractFindings, and assetLintFindingsFor, which runs the checks over
// the project's files), the exit check for functions a run added that nothing
// calls, invalidateTrackedValidation (what a shell effect does to the
// deliverable ledger), readLedgerBytes (how the ledger reads a file),
// recordLedgerEffect (where each resolved tool call reaches the ledger), and
// the note that a mandatory check could not run.
//
// The other gates have files of their own: gates_completion.go (the claim
// check on a `done` summary), gates_plan.go (plan adherence and the
// plan-progress reminder) and gates_asset_lint.go (the rest of the asset
// lint). The checks on what an edit or write tool was sent are in
// gates_edit_requests.go, and the text of a syntax refusal is in
// gates_syntax_rejection.go.
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
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

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
	rel := structuralProjectContext(ctx, path)
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
	raw, err := io.ReadAll(serviceAnswer(resp.Body))
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
	data, err := readRegularFile(path)
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

// structuralProjectContext is the project context the structural check is
// sent: the session's other .py files, read or written, each truncated and
// keyed by its workspace-relative path. The file being checked is left out.
func structuralProjectContext(ctx *AgentContext, path string) map[string]string {
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
		rel[r] = contextEntry(c, 4000)
	}
	for p, c := range ctx.SnapshotFilesRead() {
		addContext(p, c)
	}
	addSessionWrites(ctx, cleanTarget, addContext)
	return rel
}

// addSessionWrites hands addContext each .py file the session wrote, as it is
// on disk now, except the file being checked.
func addSessionWrites(ctx *AgentContext, cleanTarget string, addContext func(p, c string)) {
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
		if data, err := readRegularFile(abs); err == nil {
			addContext(abs, string(data))
		}
	}
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

// syntaxGateLanguages maps extensions to the sandbox's language names.
// Only types the sandbox's /syntax-check has a checker for are listed —
// anything else passes through ungated. The checkers judge syntax only, one
// file at a time, and the HTML one is lenient: it requires markup and a
// document that does not end inside a tag, a comment or a <script>, and
// nothing more.
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
func checkFallbackSyntax(ctx *AgentContext, path, content string) (string, bool) {
	agg := fallbackSyntaxOutcomeFor(ctx, path, content).aggregate()
	return agg.Detail, agg.Status != ValidationFailed
}

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

// sandboxSyntaxOutcome is the whole-file half of checkFallbackSyntax: the
// sandbox's /syntax-check in the file's own language. APPLICABILITY IS EVALUATED
// FIRST: the extension decides whether this check applies at all, before any
// service availability question, so a missing sandbox cannot mask genuinely
// non-applicable content. That changes the structured evidence only -- the
// allow/refuse decision is identical either way.
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
	if json.NewDecoder(serviceAnswer(resp.Body)).Decode(&out) != nil {
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
	if json.NewDecoder(serviceAnswer(resp.Body)).Decode(&out) != nil {
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

// assetLintFindings is every finding, advisory and contract alike, in one
// list. The mutation-time note and the exit gate ask the two halves
// separately; this keeps the single-list callers unchanged.
//
// It walks the project under workingDir, and returns nil for a project too
// large to judge.
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
	htmlCount, routeSet, routeRaw := pagesAndRoutes(files)

	for _, f := range files {
		switch {
		case strings.HasPrefix(f.rel, "templates/"):
			findings = append(findings, orphanedTemplateFindings(f, allOther)...)
		case strings.HasPrefix(f.rel, "static/"):
			findings = append(findings, orphanedStaticFindings(f, allOther)...)
		default:
			findings = append(findings, orphanedFlatFindings(f, allOther, htmlCount)...)
		}

		// Referenced-but-missing templates: render_template('x') in .py,
		// {% extends/include %} in templates. The snake fix session
		// shipped an errorhandler rendering templates/404.html that did
		// not exist — every 404 became a 500.
		ext := strings.ToLower(filepath.Ext(f.rel))
		tmplRefs := templateReferences(f, ext)
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
			contract = append(contract, unservedRouteFindings(f, routeSet, routeRaw)...)
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
	if json.NewDecoder(serviceAnswer(resp.Body)).Decode(&out) != nil {
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
	b, err := readRegularFile(key)
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

// recordLedgerEffect records each resolved tool call's effect in the ledger,
// called at the shared tool boundary once a call has fully resolved.
// applyShellChanges (workspace_observation.go) adds what a shell command changed.
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
		recordBackgroundStart(ctx, args, result)

	case "stop_background":
		recordBackgroundStop(ctx, result)
	}
}

// recordBackgroundStart records a run_background call: every tracked verdict
// is re-proved, and the job it may have started is held as a workspace hazard.
func recordBackgroundStart(ctx *AgentContext, args json.RawMessage, result *ToolResult) {
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
}

// recordBackgroundStop records a stop_background call: every tracked verdict
// is re-proved, and the job's hazard is lowered only on a reaped exit code.
func recordBackgroundStop(ctx *AgentContext, result *ToolResult) {
	invalidateTrackedValidation(ctx)
	// A reaped exit code is the only proof the writer is gone. SIGTERM
	// sent, SIGKILL sent, and "stop returned" are all compatible with a
	// process still flushing.
	var out StopBackgroundOutput
	if len(result.Data) > 0 && json.Unmarshal(result.Data, &out) == nil && out.ExitCode != nil {
		clearWorkspaceHazard(ctx, out.JobID)
	}
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
