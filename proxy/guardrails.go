// Guardrails for the agent loop. Centralises the checks that bounce
// model output before it touches disk or the host filesystem.
//
// Why separate files: the rules accumulate (output sanitisation,
// shell-op blocking, protected paths) and live downstream of multiple
// tool handlers. Keeping them apart from the tool handlers, in
// guardrails*.go, makes the policy auditable — reviewers don't have to
// chase three call sites to know what we reject.
//
// Background: ATLAS runs against compact local coding models that are
// weaker than the API frontier models. Claude-Code-style "trust the
// model + permission prompts" doesn't hold for us; the model will
// reliably emit markdown-fenced code with prose preamble and reach
// for shell `mv`/`rm` against source files mid-task. Server-side
// gates are how we keep the workspace usable.
//
// The guardrails are split by subject into guardrails*.go. This file holds
// the completion policy: what a request demands, and what a run must show
// before `done` is accepted. That is the reading of the user's request (fix,
// action, explain-only, read-only), the expected outputs, the action demand,
// the work-contract verification demand with the coverage evidence it reads,
// and the plan-completion gate. Tests read this file by name for the two
// demands and their helpers, so they stay here.
//
// Five functions of other subjects stay here too, where they were before the
// split, because Sonar or CodeQL records a finding on one of their lines:
// isCatastrophicDeleteTarget (the shell policy), driftedSinceVerification
// (the verification gate), unshownSymbolCitations (a `text` reply), the
// placeholder-stub check looksLikeStub (the content of a write) and
// toolBanNoteFor (a refused tool call). The other files:
//
//	guardrails_write.go              the content a write or an edit sends
//	guardrails_shell.go              the shell command policy
//	guardrails_reply.go              checks on a `text` reply
//	guardrails_verification_gate.go  which runs count as verification
//	guardrails_summaries.go          what a run that did not finish says
//	guardrails_tool_hints.go         how to repair a refused tool call

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// isCatastrophicDeleteTarget reports whether a recursive-rm target is a root /
// home / project-root / glob-everything path.
func isCatastrophicDeleteTarget(t string) bool {
	t = strings.Trim(t, `"'`)
	switch t {
	case "/", "/*", "~", "~/", "~/*", "$HOME", "${HOME}", "$HOME/*", "${HOME}/*",
		".", "./", "./*", "*", "..", "../", "../*",
		"/workspace", "/workspace/", "/workspace/*":
		return true
	}
	return false
}

// fixIntentWords tracks vocabulary that signals "the user wants
// something repaired or verified." Reused by the verification gate
// to decide when "done" needs a build/test/run before it passes.
// Kept in sync with classifyAgentTier's fix-intent list.
var fixIntentWords = []string{
	"fix", "broken", "doesn't work", "doesn't", "does not work", "does not",
	"not working", "isn't working", "isn't", "is not", "aren't", "wasn't",
	"didn't", "won't", "can't", "bug", "issue", "problem", "error",
	"failed", "fails", "failing", "incorrect", "wrong", "verify",
	"render", "renders", "rendering", "load", "loads", "loading",
}

// fileSHA256 hashes the current on-disk bytes of a session path, "" when
// the file can't be read.
func fileSHA256(ctx *AgentContext, path string) string {
	data, err := os.ReadFile(resolveAgentPath(ctx, path))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// changedPathsForCoverage is every path a passing run may cover: the raw
// paths write_file registered as session writes, and the canonical paths of
// every code deliverable the ledger recorded this session -- which is where an
// edit tool's landing is written down.
//
// The verification demand (decideVerificationDemand -> codeDeliverablesFor)
// already asks about the ledger's deliverables. The coverage that answered it
// read only the session-write map, which edit_file and structural_edit never
// wrote to and no edit tool wrote to for a delivered candidate, so a run that
// named an edited file covered nothing and every edit task under task_mode
// work ended verification_demanded_unmet. Reading both sides from the ledger's
// canonical identity closes that asymmetry without touching what the
// session-write map means to its other readers: the overwrite guard's notion
// of the model's own draft, the debug fast path, the manifest note and the V3
// project context are unchanged.
//
// Coverage states only that a run named a path whose current bytes hash to
// this value. It proves nothing about correctness, and the ledger's own
// validation still settles mutation debt.
func changedPathsForCoverage(ctx *AgentContext) []string {
	if ctx == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for p := range ctx.SessionWrites {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, p := range codeDeliverablesFor(ctx, nil) {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// sessionWriteHashes snapshots the sha256 of every file this session wrote,
// keyed by the path as the model sent it. Taken at the moment a verifying
// run succeeds, it records WHICH bytes that run vouched for.
func sessionWriteHashes(ctx *AgentContext) map[string]string {
	if ctx == nil || len(ctx.SessionWrites) == 0 {
		return nil
	}
	out := make(map[string]string, len(ctx.SessionWrites))
	for p := range ctx.SessionWrites {
		if h := fileSHA256(ctx, p); h != "" {
			out[p] = h
		}
	}
	return out
}

// commandNamesPath reports whether the command line names the file as a
// whole token (path, basename, or */basename) in any shell segment. Within
// a command already classified as verification, a named file participated
// in what was verified — passed to an interpreter, a test runner, or a
// grader as an argument. Substring matching is deliberately avoided:
// "solve.py" must not match "solve.py.bak".
//
// Three common shapes named their files without a plain token, so a run that
// verified them bound nothing: a subshell, `(cd app && python main.py)`, whose
// token keeps the parenthesis; a glob, `javac *.java`; and a Java class run,
// `java Main` or `java com.example.Main`, which names the class, not
// Main.java. Shell punctuation is trimmed from tokens, a glob token matches
// the files it would expand to, and after `java` a class name matches its
// source file.
func commandNamesPath(command, path string) bool {
	base := filepath.Base(path)
	class := ""
	if strings.HasSuffix(base, ".java") {
		class = strings.TrimSuffix(base, ".java")
	}
	for _, segment := range splitShellSegments(command) {
		fields := strings.Fields(segment)
		javaRun := false
		for i, tok := range fields {
			tok = strings.Trim(tok, "\"'()`{};")
			if tok == "" {
				continue
			}
			if tok == path || tok == base || strings.HasSuffix(tok, "/"+base) {
				return true
			}
			if strings.ContainsAny(tok, "*?[") && globNamesPath(tok, path) {
				return true
			}
			if i == 0 || (!javaRun && filepath.Base(tok) == "java") {
				javaRun = filepath.Base(tok) == "java"
				continue
			}
			if javaRun && class != "" && !strings.HasPrefix(tok, "-") &&
				(tok == class || strings.HasSuffix(tok, "."+class)) {
				return true
			}
		}
	}
	return false
}

// globNamesPath reports whether a shell glob from a command line would expand
// to path. A changed path is often absolute (the resolved form sits beside
// the one the model sent), and a relative glob is relative to where the
// command ran, so the glob is matched against as many trailing components of
// the path as it has: `src/*.java` against `.../src/Util.java`.
func globNamesPath(glob, path string) bool {
	glob = strings.TrimPrefix(glob, "./")
	if filepath.IsAbs(glob) {
		ok, _ := filepath.Match(glob, path)
		return ok
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	n := strings.Count(glob, "/") + 1
	if n > len(parts) {
		return false
	}
	ok, _ := filepath.Match(glob, strings.Join(parts[len(parts)-n:], "/"))
	return ok
}

// coverageFromLiveServer credits a probe with the files whose code is actually
// answering it.
//
// The contradiction this resolves, measured in the cycle-6 regression
// (flask_pause rep 2, R3): ATLAS tells a run to start a server with
// run_background and probe it with curl — and then asks for verification
// evidence that NAMES the deliverable. A probe names no file, so the run
// probed a live app, was credited with nothing, and finished
// verification_demanded_unmet. No command can both start a blocking server and
// return, so the obligation was unreachable by the route ATLAS prescribes.
//
// A probe is credited with the files named by a background job that is still
// running AND was started after those files were last written: that process is
// running the bytes on disk, and it answered. A job that predates the last
// edit is serving older code and is credited with nothing — which is the same
// rule the artifact gate applies to files that change after a verification.
func coverageFromLiveServer(ctx *AgentContext, command string) map[string]string {
	covered := map[string]string{}
	if ctx == nil || len(ctx.BackgroundJobs) == 0 || !isHTTPProbe(command) {
		return covered
	}
	for id, jobCmd := range ctx.BackgroundJobs {
		started, ok := ctx.BackgroundJobStarted[id]
		if !ok {
			continue
		}
		for _, p := range changedPathsForCoverage(ctx) {
			if _, already := covered[p]; already || !commandNamesPath(jobCmd, p) {
				continue
			}
			info, err := os.Stat(p)
			if err != nil || info.ModTime().After(started) {
				continue // the file changed after this process started
			}
			if h := fileSHA256(ctx, p); h != "" {
				covered[p] = h
			}
		}
	}
	return covered
}

// isHTTPProbe matches a request made against a local service — the shape the
// verification rejection prescribes for a server.
func isHTTPProbe(command string) bool {
	c := strings.ToLower(strings.TrimSpace(command))
	if !strings.Contains(c, "curl") && !strings.Contains(c, "wget") && !strings.Contains(c, "http") {
		return false
	}
	return strings.Contains(c, "localhost") || strings.Contains(c, "127.0.0.1") || strings.Contains(c, "0.0.0.0")
}

// staleServingJob names a running job that was started before a file the run
// has since changed, with that file: probing it would exercise older code.
func staleServingJob(ctx *AgentContext) (jobID, path string) {
	if ctx == nil {
		return "", ""
	}
	for _, id := range sortedKeys(ctx.BackgroundJobs) {
		started, ok := ctx.BackgroundJobStarted[id]
		if !ok {
			continue
		}
		for _, p := range changedPathsForCoverage(ctx) {
			if !commandNamesPath(ctx.BackgroundJobs[id], p) {
				continue
			}
			if info, err := os.Stat(p); err == nil && info.ModTime().After(started) {
				return id, filepath.Base(p)
			}
		}
	}
	return "", ""
}

// coverageForGreenCommand binds a green run to the files it exercised.
//
// Direct naming is the floor: a command that typed a path ran that path. But
// the standard way to verify a change is to run something that USES it -- a
// test, a driver, a reproduction script -- and coverage that stops at the
// command line cannot see through that. Measured: three tasks wrote a test
// beside the fix and ran it green (`python3 test_stats.py` printing
// "median() passed"), then were told nothing in the run verified the code,
// because the command named the test and not stats.py. Those runs had done
// exactly the right thing and were failed for it.
//
// So coverage follows the entry point into the workspace files it references.
// The inference is the same strength as the direct case, not weaker: if
// test_stats.py imports stats and the process exited 0, then stats.py was
// located, parsed and its module body executed -- which is precisely, and
// only, what `python3 stats.py` proves. Neither shows a particular function
// is correct; that was never this gate's claim. Code that nothing which ran
// refers to stays uncovered. A red run binds nothing: its record, marked
// Failed, only takes back an earlier pass over the same bytes.
func coverageForGreenCommand(ctx *AgentContext, command string) map[string]string {
	covered := coverageFromLiveServer(ctx, command)
	candidates := changedPathsForCoverage(ctx)
	var entries []string
	for _, p := range candidates {
		if commandNamesPath(command, p) {
			if h := fileSHA256(ctx, p); h != "" {
				covered[p] = h
				entries = append(entries, p)
			}
		}
	}
	// A runner that names no file still ran the ones it discovers: a bare
	// `pytest` imports every test file, and `go test ./...` builds and tests
	// every package with tests. Without this, a work request verified only by
	// a bare `pytest` could never meet its contract: nothing named the module
	// or its tests.
	if ctx != nil {
		for _, p := range runnerEntries(command, ctx.WorkingDir, candidates) {
			if _, already := covered[p]; already {
				continue
			}
			if h := fileSHA256(ctx, p); h != "" {
				covered[p] = h
				entries = append(entries, p)
			}
		}
	}
	// Breadth-first, bounded by the candidate set: each file is admitted at
	// most once, so an import cycle terminates and nothing outside the set of
	// files this session already tracks can be pulled in.
	for i := 0; i < len(entries); i++ {
		src, err := os.ReadFile(entries[i])
		if err != nil {
			continue
		}
		for _, q := range candidates {
			if _, already := covered[q]; already {
				continue
			}
			if !sourceReferencesPath(string(src), q) {
				continue
			}
			if h := fileSHA256(ctx, q); h != "" {
				covered[q] = h
				entries = append(entries, q)
			}
		}
	}
	return covered
}

// sourceReferencesPath reports whether running src would load q: it names the
// file outright, or imports it by module name. Import context is required for
// the bare module name so that a file merely mentioning the word "stats" does
// not claim to have exercised stats.py.
func sourceReferencesPath(src, q string) bool {
	base := filepath.Base(q)
	if base == "" || base == "." {
		return false
	}
	if strings.Contains(src, base) {
		return true
	}
	module := strings.TrimSuffix(base, filepath.Ext(base))
	if module == "" || module == base {
		return false
	}
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "import ") && !strings.HasPrefix(trimmed, "from ") &&
			!strings.Contains(trimmed, "require(") && !strings.Contains(trimmed, "import(") &&
			!strings.Contains(trimmed, "import_module") {
			continue
		}
		if referencesModuleWord(trimmed, module) {
			return true
		}
	}
	return false
}

// referencesModuleWord finds module as a whole identifier, so "stats" does not
// match "statsd" or "my_stats".
func referencesModuleWord(line, module string) bool {
	for i := 0; ; {
		j := strings.Index(line[i:], module)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(module)
		beforeOK := start == 0 || !isIdentByte(line[start-1])
		afterOK := end == len(line) || !isIdentByte(line[end])
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
	}
}

func isIdentByte(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// --- work-contract verification demand ---------------------------------------
//
// verificationDemandedAndUnmet asks one session-wide question: did anything
// pass. It cannot say WHAT was verified or at WHICH bytes, so `echo ok` clears
// it and a rewrite after a green run does not re-arm it. The evidence that can
// answer already exists -- ctx.VerificationEvidence records, per green command,
// the sha256 of each file the command actually NAMED -- and until now its only
// consumer was lens labelling. A completion decision is exactly the place that
// evidence was built for.
//
// Scope is deliberately narrow: a client that declared task_mode work. Nothing
// here touches contractless callers, questions, documents, deletion, moves,
// debt, hazards, tombstones, permission or timeouts.

type verificationDemand struct {
	Required bool
	Met      bool
	// Missing names the first deliverable that has no current, relevant,
	// green evidence, or the first declared command that did not run against
	// the final bytes. Empty when Met.
	Missing string
	// MissingCommand says Missing is a declared command rather than a path.
	MissingCommand bool
}

// codeDeliverablesFor is the set this demand covers: paths the client declared
// plus paths the session wrote, restricted to extensions the syntax gate knows.
// Documents keep the existing exact-current-hash rule and are not included.
func codeDeliverablesFor(ctx *AgentContext, expected []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(rel string) {
		if rel == "" {
			return
		}
		resolved := resolveAgentPath(ctx, rel)
		// Executability, not registry membership: a static or declarative
		// artifact has a checker but nothing to run, and demanding an
		// execution that names it is an obligation nothing can discharge.
		// Its bytes are still held to the existing validation contract.
		if meta, gated := syntaxGateLanguages[strings.ToLower(filepath.Ext(resolved))]; !gated ||
			!meta.Executable {
			return
		}
		if seen[resolved] {
			return
		}
		seen[resolved] = true
		out = append(out, resolved)
	}
	for _, rel := range expected {
		add(rel)
	}
	if ctx != nil {
		ctx.LedgerMu.Lock()
		for key, d := range ctx.Ledger {
			if d.Tombstoned || d.Generation == 0 {
				continue
			}
			add(key)
		}
		ctx.LedgerMu.Unlock()
	}
	sort.Strings(out)
	return out
}

// evidenceIsCurrent reports whether a green record still describes the bytes on
// disk for every path it covered. A later mutation to any covered path makes
// the record stale, which is the verify-then-modify hole stated as a rule.
// The record is keyed by the path the model wrote, the deliverable set by the
// path the client declared or the ledger owns. Both are put through
// resolveAgentPath -- the one canonicalisation rule -- so "solve.py" and
// "./solve.py" are the same file without a second normalizer.
func evidenceIsCurrent(ctx *AgentContext, rec VerificationRecord) (map[string]string, bool) {
	if len(rec.Covered) == 0 {
		return nil, false
	}
	out := make(map[string]string, len(rec.Covered))
	for p, h := range rec.Covered {
		if fileSHA256(ctx, p) != h {
			return nil, false
		}
		out[resolveAgentPath(ctx, p)] = h
	}
	return out, true
}

// contractRequiresCommand reports whether the client declared this exact
// command. isVerificationCommand recognises builds, tests, probes and runners
// by shape, and a client's own requirement may be none of those -- a linter, a
// schema check, a project script. Without this, a declared command could be
// impossible to satisfy: the demand would require it and nothing would ever
// record it running.
//
// It changes what gets RECORDED, never how strongly the record counts. The
// evidence is the same execution, bound to the same hashes; declaring a command
// says who required it, not that its passing proves more.
func contractRequiresCommand(ctx *AgentContext, command string) bool {
	if ctx == nil || ctx.TaskContract == nil {
		return false
	}
	for _, want := range ctx.TaskContract.VerificationCommands() {
		if want == command {
			return true
		}
	}
	return false
}

// decideVerificationDemand is the single owner of the work-contract demand.
//
// Fails closed everywhere: no evidence, stale evidence, evidence that names a
// different path, or a declared command that never ran against the final bytes
// all leave the demand unmet. A successful command that names nothing covers
// nothing, so `true` and `echo ok` cannot satisfy it -- not because they are
// recognised, but because they carry no binding.
func decideVerificationDemand(ctx *AgentContext, tc *TaskContract, expected []string) verificationDemand {
	if ctx == nil || tc == nil || tc.TaskMode != TaskModeWork {
		return verificationDemand{}
	}
	// Which commands this run owes, and on whose authority. A caller that
	// declared its verification is the authority on that list: the declared
	// commands are the obligation, and the legacy reading of the same field
	// applies only when nothing was declared.
	obligation := resolveVerificationObligation(ctx)
	// What the run actually wrote demands its own evidence. That demand is
	// read off the workspace rather than off the request, so declaring
	// verification knowledge -- of any shape, including none -- narrows the
	// command list and never switches this off. Code the run wrote and never
	// showed to run stays unverified whatever the caller declared.
	paths := codeDeliverablesFor(ctx, expected)
	// A declared command is a requirement in its own right. It survives when
	// the run produced nothing executable -- a client that asked for
	// `htmlhint index.html` asked for it regardless of what the registry
	// thinks can be run.
	if len(paths) == 0 && len(obligation.Items) == 0 {
		return verificationDemand{}
	}
	// Two demand sets, evaluated independently.
	//
	// Path coverage asks which artifact bytes were exercised, and only an
	// evidence record that NAMED the path can answer it. The exact-command
	// demand asks whether the client's own command ran, and a command that
	// names no file answers that perfectly well while covering nothing. They
	// were one loop over one list, so `pytest` created an obligation nothing
	// could discharge and the session could never reach settlement.
	for _, p := range paths {
		h := fileSHA256(ctx, p)
		if h == "" {
			return verificationDemand{Required: true, Missing: p}
		}
		if !pathCoverageSatisfied(ctx, p, h) {
			return verificationDemand{Required: true, Missing: p}
		}
	}
	for _, want := range obligation.Items {
		if !commandObligationSatisfied(ctx, want) {
			return verificationDemand{Required: true, Missing: want, MissingCommand: true}
		}
	}
	return verificationDemand{Required: true, Met: true}
}

// pathCoverageSatisfied answers the artifact question and only that one.
//
// Coverage comes from Covered and from nowhere else: a record that did not
// name the path cannot vouch for its bytes, however green it was.
func pathCoverageSatisfied(ctx *AgentContext, path, hash string) bool {
	if _, ok := coverageRecord(ctx, path, hash); ok {
		return true
	}
	return stagedCoverageSatisfied(ctx, path, hash)
}

// coverageRecord replays the evidence over one path's bytes, in order, and
// returns the record that vouches for them now. A run that showed the program
// working binds the bytes it covered; a later failed run over the same bytes
// takes that back, because the latest result on those bytes is the one that
// describes them. A static check, or a declared command that runs nothing,
// neither binds nor unbinds.
func coverageRecord(ctx *AgentContext, path, hash string) (VerificationRecord, bool) {
	var bound VerificationRecord
	ok := false
	if ctx == nil {
		return bound, false
	}
	for _, rec := range ctx.VerificationEvidence {
		if covered, current := evidenceIsCurrent(ctx, rec); current && covered[path] == hash {
			switch {
			case rec.Failed:
				bound, ok = VerificationRecord{}, false
			case rec.showsWorking():
				bound, ok = rec, true
			}
		}
	}
	return bound, ok
}

// commandObligationSatisfied answers the command question and only that one.
//
// Exact identity, byte for byte: no normalisation, no equivalence, no shell
// parsing. "python3  solve.py" is not "python3 solve.py".
func commandObligationSatisfied(ctx *AgentContext, want string) bool {
	// The latest current run of the command decides: a pass followed by a
	// failure on the same workspace is a failure.
	satisfied := false
	for _, rec := range ctx.VerificationEvidence {
		if rec.Command == want && commandEvidenceCurrent(ctx, rec) {
			satisfied = !rec.Failed
		}
	}
	if satisfied {
		return true
	}
	return stagedCommandSatisfied(ctx, want)
}

// commandEvidenceCurrent reports whether a direct execution still describes
// the workspace as it is now.
//
// A record that named paths is current on its coverage, exactly as before. A
// record that named none is current on the workspace identity it was stamped
// with -- which is conservative by construction: any material mutation to a
// tracked artifact moves the identity and stales every pathless record at
// once, including ones the mutation had nothing to do with.
func commandEvidenceCurrent(ctx *AgentContext, rec VerificationRecord) bool {
	if len(rec.Covered) > 0 {
		_, ok := evidenceIsCurrent(ctx, rec)
		return ok
	}
	if rec.WorkspaceStateHash == "" {
		// A record from before the stamp existed says nothing about when it
		// ran, and an unanswerable question is not a satisfied one.
		return false
	}
	generation, state := workspaceIdentity(ctx)
	return rec.WorkspaceGeneration == generation && rec.WorkspaceStateHash == state
}

// driftedSinceVerification names the first session-written file whose bytes
// no longer match the verified snapshot, or "" when everything still does.
// A file written AFTER the snapshot (absent from it) is drift by definition:
// it has never been executed.
func driftedSinceVerification(ctx *AgentContext, verified map[string]string) string {
	if ctx == nil {
		return ""
	}
	for _, p := range sortedKeys(ctx.SessionWrites) {
		want, seen := verified[p]
		if !seen {
			return p
		}
		data, err := os.ReadFile(resolveAgentPath(ctx, p))
		if err != nil {
			return p
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != want {
			return p
		}
	}
	return ""
}

// unshownSymbolCitations returns the code a reply names whose definition the
// run never showed. unreadFileCitations works per file: any read of a file
// counts there as seeing all of it, but a truncated or ranged read shows only
// part. Smoke run 2026-09-27 (bugfind_tiebreak): both reads stopped near line
// 190, the reply named `_score_plan`, which it said lay "past the provided
// snippet", and the run ended completed.
//
// The predicate matches the file-level one: existence plus absence of
// evidence. The name must be defined in a file the run read only in part, at a
// line no read showed, and nowhere a read did show it.
func unshownSymbolCitations(ctx *AgentContext, text string) []unshownSymbol {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	paths := ctx.bodySeenPaths()
	if !anyReadInPart(ctx, paths) {
		return nil
	}
	sources := map[string]string{}
	var out []unshownSymbol
	seen := map[string]bool{}
	for _, m := range codeRefRe.FindAllStringSubmatch(text, -1) {
		name := m[1]
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			name = name[i+1:]
		}
		if len(name) < 3 || seen[name] || fileExtensionWords[strings.ToLower(name)] {
			continue
		}
		seen[name] = true
		var gap *unshownSymbol
		shown := false
		for _, p := range paths {
			src, ok := sources[p]
			if !ok {
				data, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				src = string(data)
				sources[p] = src
			}
			for _, line := range definitionLines(src, name) {
				if ctx.LineWasShown(p, line) {
					shown = true
					break
				}
				if gap == nil {
					gap = &unshownSymbol{Name: name, File: workspaceName(ctx, p), Line: line,
						Spans: ctx.ShownSpans(p)}
				}
			}
			if shown {
				break
			}
		}
		if gap != nil && !shown {
			out = append(out, *gap)
			if len(out) == maxCitedPaths {
				break
			}
		}
	}
	return out
}

// anyReadInPart reports whether a read of one of paths showed only part of
// the file, the case in which a reply can name code no read showed.
func anyReadInPart(ctx *AgentContext, paths []string) bool {
	for _, p := range paths {
		if ctx.ShownSpans(p) != nil {
			return true
		}
	}
	return false
}

// isExplainOnlyMessage reports an explicit "tell me, do not touch it"
// instruction: an explain/describe request paired with a no-edit directive.
//
// Position-based negation cannot catch this. In "…whether it is actually a
// bug. Do not change the code." the intent word comes BEFORE the directive,
// so a backward scan finds nothing — yet the instruction plainly governs the
// whole message. Measured: that prompt classified T2, ran the V3 pipeline,
// and wrote to files the user had just asked it to leave alone.
//
// Both halves are required. "fix the bug but don't change the public API" has
// the directive and is still real work; without the explain half it stays
// action intent.
func isExplainOnlyMessage(lower string) bool {
	explain := false
	for _, w := range []string{"explain", "describe", "walk me through",
		"what does", "what is", "how does", "why does", "tell me"} {
		if strings.Contains(lower, w) {
			explain = true
			break
		}
	}
	if !explain {
		return false
	}
	for _, d := range []string{
		"do not change", "don't change", "dont change",
		"do not edit", "don't edit", "dont edit",
		"do not modify", "don't modify", "dont modify",
		"do not write", "don't write",
		// "fix" completes the set: the list already carries change/edit/
		// modify/write, and a user who says "tell me what is wrong -- do not
		// fix it yet" has forbidden mutation exactly as plainly. Measured: a
		// benchmark question carrying this phrasing was classified as work and
		// edited the fixture it was told to leave alone, twice.
		"do not fix", "don't fix", "dont fix",
		"without changing", "without editing", "without modifying",
		"no code changes", "just explain", "only explain", "explain only",
	} {
		if strings.Contains(lower, d) {
			return true
		}
	}
	return false
}

// isFixIntentMessage returns true when the user prompt looks like a
// repair/verification request. The verification gate uses this to
// decide whether `done` requires a real verification step. Pure
// feature requests ("add a logout button") don't trip the gate —
// adding code doesn't always need a curl/test to declare done.
func isFixIntentMessage(msg string) bool {
	lower := strings.ToLower(msg)
	if isExplainOnlyMessage(lower) {
		return false
	}
	for _, w := range fixIntentWords {
		idx := 0
		for {
			i := strings.Index(lower[idx:], w)
			if i < 0 {
				break
			}
			at := idx + i
			// Same negation rule as isActionIntentMessage: "explain whether
			// it is a bug, do not change the code" is a question about a
			// defect, not a request to repair one, and reading it as repair
			// intent handed it the write pipeline.
			if !negatedAt(lower, at) {
				return true
			}
			idx = at + len(w)
		}
	}
	return false
}

// actionIntentWords tracks verbs that signal "the user wants something
// CREATED, MODIFIED, or REPLACED on disk." Distinct from
// fixIntentWords (which is about repair/verification) — these match
// feature-build prompts where the model must emit a write_file /
// edit_file / structural_edit / delete_file before `done` is honest.
//
// May 10 2026 false-success case that motivated this: prompt was
// "Rewrite templates/dashboard.html to display a clean SaaS-style
// metrics dashboard..." Model spent 6 turns starting servers and
// curling the placeholder, never edited anything, declared `done`.
// The fix-intent gate didn't fire because "rewrite" isn't a
// fix-intent word — but it IS clearly an action-intent word that
// should have required a productive write.
var actionIntentWords = []string{
	"rewrite", "rewriting", "rewritten",
	"create", "creates", "creating", "created",
	"add", "adds", "adding", "added",
	"implement", "implements", "implementing", "implemented",
	"build", "builds", "building", "built",
	"write", "writes", "writing", "wrote",
	"refactor", "refactors", "refactoring", "refactored",
	"replace", "replaces", "replacing", "replaced",
	"update", "updates", "updating", "updated",
	"modify", "modifies", "modifying", "modified",
	"change", "changes", "changing", "changed",
	"make a", "make the", "make it",
	"convert", "converts", "converting", "converted",
	"redesign", "redesigning", "redesigned",
}

// reOutputFilenameTok matches a filename-looking token: an optional
// leading path, then name.ext (1-6 char extension). Captures group 1.
var reOutputFilenameTok = regexp.MustCompile("[`\"']?((?:[~./]|\\.\\./)?[\\w./-]*\\.[A-Za-z][A-Za-z0-9]{0,5})[`\"']?")

// reOutputWriteVerb matches the stems of verbs that mean "produce this
// file" — used to tell a prompt's OUTPUT file from an INPUT file. `read`
// is deliberately absent (it names an input).
var reOutputWriteVerb = regexp.MustCompile(`(?i)\b(sav|writ|creat|output|generat|stor|produc|recover|dump)`)

// reMustProduce matches the "<file> must exist / must contain" requirement
// phrasing (a merge-diff prompt: "the file algo.py must exist in the
// merged result"), which names a deliverable without a write verb.
// Checked in a window AFTER the filename.
var reMustProduce = regexp.MustCompile(`(?i)^\s*(must (exist|contain|include|be (creat|writt|present|generat)))`)

// reReadsTheFile matches a read verb that directly governs the filename after
// it ("reads input.txt", "loads the file config.yaml"). Such a file is the
// task's input even when a write verb sits within the window before it:
// "Write solve.py that reads input.txt" asks for solve.py alone. Only a
// determiner or "file"/"data"/"contents" may stand between the verb and the
// name, so "Write the parse results to out.json" still names an output.
var reReadsTheFile = regexp.MustCompile(`(?i)\b(read|reads|reading|load|loads|loading|parse|parses|parsing|open|opens|opening)\s+((the|a|an|its|file|data|contents?|of)\s+){0,3}$`)

// expectedOutputPaths extracts the file(s) a task prompt explicitly asks
// the model to produce: a filename token preceded within ~70 chars by a
// write/save/create/output verb. Grounded in the task text (many bench and
// real prompts say "save your solution in X", "write the output to Y",
// "create a JSON file Z"), so it can be checked against disk at the end.
// Bounded to the first 2 to avoid over-steering on a chatty prompt.
func expectedOutputPaths(msg string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range reOutputFilenameTok.FindAllStringSubmatchIndex(msg, -1) {
		path := msg[m[2]:m[3]]
		if path == "" || strings.Count(path, ".") == len(path) {
			continue
		}
		start := m[0] - 70
		if start < 0 {
			start = 0
		}
		afterEnd := m[1] + 40
		if afterEnd > len(msg) {
			afterEnd = len(msg)
		}
		// Output signal: a write verb within ~70 chars before the filename,
		// OR "must exist/contain" requirement phrasing right after it.
		if (!reOutputWriteVerb.MatchString(msg[start:m[0]]) || reReadsTheFile.MatchString(msg[start:m[0]])) &&
			!reMustProduce.MatchString(msg[m[1]:afterEnd]) {
			continue // input/incidental filename
		}
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
			if len(out) >= 2 {
				break
			}
		}
	}
	return out
}

// missingExpectedOutputs returns the expected output files that do not
// exist on disk. Checks the resolved path with os.Stat so it counts a
// file created by ANY means (write_file OR a run_command that
// redirected/generated it), not just write_file. Stat probes are
// contained to known roots — the workspace, plus the system temp dir
// (host-verify tasks legitimately name /tmp outputs). A path outside
// both is skipped: the gate only enforces deliverables it can check
// without probing arbitrary prompt-derived paths.
func missingExpectedOutputs(ctx *AgentContext, expected []string) []string {
	var missing []string
	roots := []string{filepath.Clean(ctx.WorkingDir), filepath.Clean(os.TempDir())}
	for _, p := range expected {
		resolved := resolveAgentPath(ctx, p)
		for _, root := range roots {
			rel, err := filepath.Rel(root, resolved)
			if err != nil || !filepath.IsLocal(rel) {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
				missing = append(missing, p)
			}
			break // first containing root decides
		}
	}
	return missing
}

// logPath escapes CR/LF in a request-derived value so a crafted name
// can't forge additional log lines; logPaths is the slice form.
func logPath(p string) string {
	p = strings.ReplaceAll(p, "\n", `\n`)
	return strings.ReplaceAll(p, "\r", `\r`)
}

func logPaths(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = logPath(p)
	}
	return out
}

// isActionIntentMessage returns true when the prompt clearly asks
// for a state change on disk (create/rewrite/refactor/etc.). The
// done-without-action gate uses this to bounce a `done` that wasn't
// preceded by any productive write — which would otherwise pass
// through silently because the fix-intent gate ignores feature work.
func isActionIntentMessage(msg string) bool {
	lower := strings.ToLower(msg)
	if isExplainOnlyMessage(lower) {
		return false
	}
	for _, w := range actionIntentWords {
		idx := 0
		for {
			i := strings.Index(lower[idx:], w)
			if i < 0 {
				break
			}
			at := idx + i
			if !negatedAt(lower, at) {
				return true
			}
			idx = at + len(w)
		}
	}
	return false
}

// negatedAt reports whether the action word at `at` is inside a negation —
// "do not change any code", "without editing", "no need to fix it".
//
// A plain substring scan reads "do not change any code" as a request to
// change code, so a question carrying that clause was classified T2 and got
// the whole write pipeline. Measured: the identical question scored T0
// without the clause and T2 with it, and the T2 run edited files the user had
// explicitly asked it to leave alone. Telling ATLAS not to touch anything
// made it more likely to.
//
// Scans a short window back rather than parsing: the negation always sits
// within a few words in the phrasings people actually use, and a wider window
// would start swallowing unrelated clauses ("I fixed the parser, now don't
// worry about X" must still read as action intent).
func negatedAt(lower string, at int) bool {
	const window = 24
	start := at - window
	if start < 0 {
		start = 0
	}
	before := lower[start:at]
	for _, neg := range []string{
		"do not ", "don't ", "dont ", "never ", "without ",
		"no need to ", "rather than ", "instead of ", "avoid ",
	} {
		if strings.Contains(before, neg) {
			return true
		}
	}
	return false
}

// expectedOutputMissingMessage tells the model the task's named output
// file doesn't exist yet — the deliverable, not just "some change." Names
// the file(s) so the steer is concrete and grounded in the task text.
func expectedOutputMissingMessage(missing []string) string {
	quoted := make([]string, len(missing))
	for i, p := range missing {
		quoted[i] = "`" + p + "`"
	}
	return "Before you finish — the task names " +
		strings.Join(quoted, " and ") +
		" as a deliverable, but it does not exist on disk yet. If your code PRODUCES it when run, run your code now to generate it (do NOT hand-write a fabricated stand-in). If it is a file you author directly, write your solution to it. If you have genuinely already produced it elsewhere or it is not actually required, you may proceed."
}

// actionWithoutProductiveChangeMessage tells the model to actually do
// the work the user asked for before declaring done. Concrete and
// directive — points at the missing tool call, not abstract "you
// haven't done enough." Mirror of verificationRejectionMessage's
// shape.
func actionWithoutProductiveChangeMessage(userMsg string) string {
	return "Cannot declare `done` yet — the user asked you to make a change on disk (rewrite/create/add/implement/refactor/etc.) and you haven't emitted any successful write_file / edit_file / structural_edit / delete_file in this loop. Verification (running the server, curling the page) is NOT the task — it's how you confirm AFTER the change. Re-read the user's request, identify what file needs to change, and emit the appropriate edit tool. Then verify, then done."
}

// actionDemandSource names why an action-demand decision came out as it did.
// It exists so evidence can say which authority answered, not merely what the
// answer was.
type actionDemandSource string

const (
	actionDemandContractWork     actionDemandSource = "contract_work"
	actionDemandContractQuestion actionDemandSource = "contract_question"
	actionDemandLegacy           actionDemandSource = "legacy"
	// contract_invalid_failed_closed cannot be produced by a validated
	// request; it exists so an internally malformed mode fails toward
	// requiring work rather than silently reading as a question.
	actionDemandContractInvalid actionDemandSource = "contract_invalid_failed_closed"
)

// actionDemand is one decision about whether a request demands a state change,
// together with the authority that made it and the legacy heuristic's own
// answer for comparison.
type actionDemand struct {
	Required bool
	Source   actionDemandSource
	// Legacy is what wantsStateChange said. It is always reported and is
	// authoritative ONLY when no contract is present.
	Legacy bool
}

// decideActionDemand is the single owner of "does this request demand a state
// change". Both live action-demand sites consume it and nothing else calls the
// heuristic for a live decision.
//
// Where the client declared a task mode, that mode decides. Step 3B measured
// the alternative on a frozen 105-case corpus: 25 of 101 evaluable requests
// disagreed with the client's own declaration, 19 of them work that the wording
// alone read as a question, and 110 of 115 gate-bearing requests were decided
// with inspectedWorkspace false -- i.e. on phrasing. A client that states what
// it asked for is better evidence than a guess about its English.
//
// Where no contract was sent, nothing changes: the heuristic decides exactly as
// before. That corpus says nothing about contractless clients, so it authorises
// nothing for them.
//
// The mode establishes an OBLIGATION only. It never authorises completion,
// mutation, deletion, permission or verification -- those keep their own
// evidence, and a question contract does not erase debt, hazards or broken
// deliverables.
//
// Pure: no model output, no workspace state beyond the inspection flag it is
// handed, no shadow state, and the same answer whether or not capture is on.
func decideActionDemand(tc *TaskContract, userMessage string, tier Tier,
	inspectedWorkspace bool) actionDemand {
	// A change the user explicitly forbade cannot also be required. The
	// read-only boundary in executeToolCallInner will REFUSE every mutating
	// tool for this request, so demanding one produces a run that is told to
	// do the one thing it is not allowed to do, bounces its own exit until the
	// budget runs out, and reports incomplete work it was never permitted to
	// perform. Measured: a client declared task_mode=work on prompts whose
	// text says "do not change any code", and all six such sessions ended
	// action_demanded_unmet while the guard correctly refused their edits.
	//
	// This outranks the contract deliberately. A caller's task_mode describes
	// what it believes it is asking for; the user's own words are the request.
	// When they disagree about whether the workspace may change, the safe and
	// coherent reading is the prohibition — and the run still completes, on
	// its answer, because a run that writes nothing owes no file obligation.
	if isExplainOnlyMessage(strings.ToLower(userMessage)) {
		return actionDemand{Required: false, Source: actionDemandLegacy, Legacy: false}
	}
	// Evaluated exactly once, here, for every path. Reporting it costs
	// nothing because the heuristic is pure, and having it always present
	// keeps the shadow record identical whether or not capture is enabled.
	legacy := wantsStateChange(userMessage, tier, inspectedWorkspace)
	if tc == nil {
		return actionDemand{Required: legacy, Source: actionDemandLegacy, Legacy: legacy}
	}
	switch tc.TaskMode {
	case TaskModeWork:
		return actionDemand{Required: true, Source: actionDemandContractWork, Legacy: legacy}
	case TaskModeQuestion:
		return actionDemand{Required: false, Source: actionDemandContractQuestion, Legacy: legacy}
	default:
		return actionDemand{Required: true, Source: actionDemandContractInvalid, Legacy: legacy}
	}
}

// wantsStateChange reports whether `done` should be blocked when no write,
// edit, or delete succeeded in this run.
//
// actionIntentWords alone was the test, and it is an open vocabulary that
// cannot be completed: it lists "create"/"add"/"make" but not
// "remove"/"delete", so "remove the debug logging from app.py" armed no
// gate and the model could close the turn having deleted nothing. Adding
// those two words leaves the next verb missing.
//
// The second signal is observed instead of guessed: a read-only tool
// succeeded, so the model opened the project rather than answering from the
// message alone. That covers any phrasing, including verbs no list has.
//
// It needs the tier to stay honest, because reading files is also how a
// question gets answered. "why does the game store direction as a string"
// opens the file and correctly writes nothing; classifyAgentTier calls that
// conversational, and conversational messages are never gated. What remains
// is the case worth blocking: a non-conversational message, the model went
// into the project, and nothing changed on disk.
func wantsStateChange(userMessage string, tier Tier, inspectedWorkspace bool) bool {
	if isActionIntentMessage(userMessage) {
		return true
	}
	// A request to LOOK is satisfied by looking. inspectedWorkspace turns on
	// as soon as a read-only tool succeeds, so without this "list the files"
	// bounced its own `done`: the read that answered the question was the same
	// read the gate treated as evidence that work had started. Observed live —
	// the model listed the directory, was refused `done`, and told the user it
	// was "unable to complete the 'done' state as per the system's
	// requirements", a rule nothing in the prompt states.
	if isReadOnlyRequest(userMessage) {
		return false
	}
	return inspectedWorkspace && tier != Tier0Conversational
}

// isReadOnlyRequest matches asks that are ANSWERED by reading: list, show,
// find, print. Deliberately narrow — it only decides whether a completed read
// is allowed to end the turn, so a false positive lets a real edit request
// finish without editing.
//
// Callers check isActionIntentMessage first, which covers "add a list to the
// page". The fix-intent exclusion below covers the other overlap: "find and
// fix the bug" opens with a read verb but is not read-only work.
func isReadOnlyRequest(msg string) bool {
	lower := strings.ToLower(msg)
	if isFixIntentMessage(msg) || isActionIntentMessage(msg) {
		return false
	}
	for _, w := range []string{
		"list the file", "list files", "list all", "list the director",
		"show me", "show the", "what files", "which files", "what's in",
		"whats in", "what is in", "find the file", "where is", "where are",
		"print the", "display the", "read the",
	} {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// stubHTMLRe catches `<h1>Foo Page</h1>` / `<h1>Bar Section</h1>` —
// the exact shape the model emits when it gives up and ships a
// placeholder. Matches inside <body>, allows whitespace.
var stubHTMLRe = regexp.MustCompile(
	`(?is)<h\d>\s*[A-Za-z]+\s+(page|section|title|content|view)\s*</h\d>`)

// looksLikeStub returns a non-empty rejection string when the content
// looks like a placeholder/stub. The model's lazy-completion
// failure mode is to ship 8-line skeletons that pass syntactic gates
// but ship the absolute minimum content to claim "done." Catches the
// most egregious shapes per file type; deliberately conservative —
// short content that has REAL substance (one-liner shell scripts,
// minimal Dockerfiles, single-import test files) passes through.
//
// The fix is to either model the file from a sibling (templates/index.html
// usually has the right scaffold) or — if the user really did ask for
// a placeholder — say so in the response so the user knows.
func looksLikeStub(displayPath, content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return "write_file refused: content is empty. If you mean to create an empty file, write a meaningful starting structure or `touch` it via run_command."
	}

	ext := strings.ToLower(filepath.Ext(displayPath))
	lineCount := strings.Count(trimmed, "\n") + 1

	switch ext {
	case ".html", ".htm":
		// 200 chars is the cliff — full pages don't fit under that.
		if len(trimmed) < 200 && stubHTMLRe.MatchString(trimmed) {
			return stubRejectionMessage(displayPath,
				"the body is just `<h1>X Page</h1>` with no real content")
		}
	case ".py":
		return pythonStubRejection(displayPath, trimmed, lineCount)
	case ".md", ".markdown":
		if len(trimmed) < 100 && (strings.Contains(strings.ToLower(trimmed), "todo") ||
			strings.Contains(strings.ToLower(trimmed), "placeholder")) {
			return stubRejectionMessage(displayPath,
				"the document is just a TODO/placeholder marker")
		}
	case ".js", ".ts", ".tsx", ".jsx":
		// React component / module that's just an empty fragment or
		// a `<div>Page</div>` placeholder.
		if len(trimmed) < 200 && regexp.MustCompile(`(?is)return\s*\(?\s*<[a-z0-9]+>\s*[A-Za-z]+\s+(page|section|view)\s*</[a-z0-9]+>\s*\)?`).MatchString(trimmed) {
			return stubRejectionMessage(displayPath,
				"the component just returns `<X>Foo Page</X>` with no real markup")
		}
	}
	return ""
}

// pythonStubRejection is looksLikeStub's check for a .py file of at most five
// lines whose body is only `pass` or a to-do comment, with no import, def or
// class in it.
func pythonStubRejection(displayPath, trimmed string, lineCount int) string {
	if lineCount <= 5 && (regexp.MustCompile(`(?m)^\s*pass\s*$`).MatchString(trimmed) ||
		regexp.MustCompile(`(?im)^\s*#\s*TODO\b.*$`).MatchString(trimmed)) {
		if !strings.Contains(trimmed, "import ") && !strings.Contains(trimmed, "def ") && !strings.Contains(trimmed, "class ") {
			return stubRejectionMessage(displayPath,
				"the file body is just `pass` / `# TODO` with no real implementation")
		}
	}
	return ""
}

func stubRejectionMessage(path, why string) string {
	return fmt.Sprintf(
		"write_file refused: %s looks like a placeholder stub — %s. Either (a) read a sibling file in the same directory to model the structure (the project's other %s files almost certainly have the right scaffold), or (b) if the user explicitly asked for an empty placeholder, acknowledge that in your response so they know the file needs to be filled in. Don't ship stubs and call the task done.",
		path, why, strings.TrimPrefix(filepath.Ext(path), "."))
}

// planIncompleteMessage names the plan steps that never got a matching tool
// call, or "" to let `done` through.
//
// The plan is generated up front, PlanStepsSatisfied tracks which steps have
// been hit, and buildPlanReminder shows the model its progress every turn —
// but nothing checked it at the exit. Run 4 built the variable-delay loop it
// was asked for, never added the per-food decrement, and emitted `done`: two
// required edits, one delivered. The reminder is an instruction and was
// ignored; this is the same fact used as a gate.
//
// Gated on plan quality, because a bad plan blocking a finished task is worse
// than no plan at all. A low-scoring plan, or one whose steps the matcher
// could not track, is not evidence of anything and is skipped.
func planIncompleteMessage(ctx *AgentContext) string {
	if ctx == nil || ctx.Plan == nil || len(ctx.Plan.Steps) < 2 {
		return ""
	}
	if ctx.Plan.WinningScore < planGateMinScore {
		return ""
	}
	if len(ctx.PlanStepsSatisfied) != len(ctx.Plan.Steps) {
		return ""
	}
	// Nothing matched at all means the matcher is not tracking this task, not
	// that the model did nothing.
	if countTrue(ctx.PlanStepsSatisfied) == 0 {
		return ""
	}
	var missing []string
	for i, step := range ctx.Plan.Steps {
		if !ctx.PlanStepsSatisfied[i] {
			missing = append(missing, fmt.Sprintf("  %s: %s", step.ID, step.Action))
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"Cannot declare `done` yet — %d of %d planned steps have landed, and these have not:\n%s\n"+
			"Each one needs its own tool call. Do the next one now. If a step is genuinely "+
			"unnecessary or already satisfied by an edit you made under a different step, say "+
			"which in your next `done` summary rather than leaving it silent.",
		countTrue(ctx.PlanStepsSatisfied), len(ctx.Plan.Steps), strings.Join(missing, "\n"))
}

// planOwesRun reports whether the plan gate would send this exit back for a
// step that runs a command. Such a step may need a job the run started: a
// probe of a server needs the server.
func planOwesRun(ctx *AgentContext) bool {
	if planIncompleteMessage(ctx) == "" {
		return false
	}
	for i, step := range ctx.Plan.Steps {
		if !ctx.PlanStepsSatisfied[i] && (step.Action == "run_command" || step.Action == "run_background") {
			return true
		}
	}
	return false
}

// toolBanNoteFor is toolBanNote plus the selectors that exist in the file
// when structural_edit is one of the alternatives it names, so the suggestion
// can be acted on without inventing a name.
func toolBanNoteFor(ctx *AgentContext, tool, path string) string {
	note := toolBanNote(tool, path)
	if tool == "structural_edit" || !strings.Contains(note, "`structural_edit`") {
		return note
	}
	b, err := os.ReadFile(resolveAgentPath(ctx, path))
	if err != nil {
		return note
	}
	if g := selectorGuidance(path, string(b)); g != "" {
		note += " For structural_edit, " + g + "."
	}
	return note
}

// --- explicit read-only requests --------------------------------------------

// mutationForbidden reports whether the USER forbade changing the workspace on
// this request.
//
// The signal is an explicit prohibition in the user's own words ("just explain
// — do not change any code"), not a question shape and not the task contract's
// `question` mode. Those two mean a state change is not REQUIRED, which is a
// different claim from "a state change is not PERMITTED"; treating them as a
// prohibition would refuse legitimate work on any request phrased as a
// question. Requiring the prohibition keeps the guarantee narrow enough to be
// safe and specific enough to be worth enforcing.
// Reads ctx.HumanTask -- the human's actual instruction, captured before the
// loop appends correctives, manifests or re-injected content, so a system note
// can never be mistaken for the user forbidding a change.
func mutationForbidden(ctx *AgentContext) bool {
	if ctx == nil {
		return false
	}
	return isExplainOnlyMessage(strings.ToLower(ctx.HumanTask))
}

// readOnlyRequestRefusal returns the refusal text when a mutation-capable tool
// is called under an explicit read-only request, or "" to allow the call.
//
// Keyed on the tool's declared effect, so it holds for direct mutators and for
// arbitrary shell alike. A read-only tool is always allowed: the model still
// needs to read the code to answer, and answering IS the deliverable here --
// a run that writes nothing has no file obligation to demonstrate and can
// complete on its answer.
func readOnlyRequestRefusal(tool *ToolDef, ctx *AgentContext) string {
	if tool == nil || !mutationForbidden(ctx) {
		return ""
	}
	if tool.Effect == ToolEffectReadOnly {
		return ""
	}
	what := "change the workspace"
	if tool.Effect == ToolEffectCommandUnobserved {
		what = "run a command that could change the workspace"
	}
	return fmt.Sprintf(
		"`%s` was not run: this request explicitly asked you NOT to change anything, "+
			"and %s would %s.\n\n"+
			"Answer the question instead. Read what you need (read_file, outline_file, "+
			"search_files, list_directory, find_file) and reply with "+
			"{\"type\":\"text\",\"content\":\"<your answer>\"} — an answer is the "+
			"deliverable for this request, so no file needs to change for it to be complete.",
		tool.Name, tool.Name, what)
}
