// Guardrails on a `text` reply, which ends the turn: a closing that defers
// work, asks the user for something, or says the agent could not do it; a
// claim about a file the run never read, or about code no read showed; and an
// answer that accounts for one file when the request asked how several
// behave. unshownSymbolCitations, which finds the code no read showed, is in
// guardrails.go.

package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// promisesMoreContent reports an answer that ends by promising content it
// never delivers — "I will now provide the specific location", "let me give
// you the exact comparison".
//
// Distinct from announcesImminentToolUse: that one catches announcing a TOOL
// call before any work has happened. This catches a reply that has done the
// work, then signs off promising the actual answer. Observed on a bug-find
// task: the model named the file, described the symptom, and ended with "I
// will now provide the specific location and the incorrect comparison as
// requested" — and the turn ended there, leaving the user a half-answer.
//
// Requires the promise to be at the END, because "I'll explain why below"
// followed by the explanation is fine.
func promisesMoreContent(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	for _, phrase := range []string{
		"i will now provide", "i'll now provide", "i will now give",
		"i'll now give", "i will provide the", "i'll provide the",
		"let me provide the", "let me give you the", "i will now show",
		"i'll now show", "here is what i will", "i will now list",
	} {
		at := strings.LastIndex(lower, phrase)
		if at < 0 {
			continue
		}
		// A promise that is FOLLOWED by the thing promised is fine —
		// "I'll provide the details: line 314 uses > where it should use <"
		// delivers in the same breath. What is broken is a promise with
		// nothing concrete after it. Digits, operators and backticks are the
		// cheap signal for "concrete", and the remaining prose after an
		// undelivered promise ("...as requested.") has none of them.
		rest := lower[at+len(phrase):]
		return !strings.ContainsAny(rest, "0123456789`=<>+*/(){}[]")
	}
	return false
}

// announcesImminentToolUse reports first-person narration of a tool call the
// model is about to make — "I need to read X", "let me look at Y", "I'll
// start by outlining".
//
// A `text` reply ends the turn, so a model that announces instead of acting
// stops with the right intent and no action. Deliberately narrow: it needs a
// first-person subject AND an action verb aimed at inspecting the workspace,
// so an ANSWER that merely mentions reading ("this function reads the file")
// does not match.
func announcesImminentToolUse(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	subjects := []string{"i need to ", "i'll ", "i will ", "let me ", "i am going to ",
		"i'm going to ", "i should ", "first, i ", "next, i "}
	// "look into" was missing and "look at" was not enough: observed on a
	// fresh workspace, "How does the contact form work?" was answered with
	// "I'll look into the contact form's implementation..." and nothing else.
	// `text` is a terminal exit, so an announcement that slips this check ends
	// the turn and the user gets a promise instead of an answer.
	verbs := []string{"read", "look at", "look into", "look through", "look over",
		"open", "inspect", "examine", "outline", "check", "search", "list",
		"start by", "investigate", "dig into", "trace through", "review the",
		"take a look"}
	for _, sub := range subjects {
		at := strings.Index(lower, sub)
		if at < 0 {
			continue
		}
		// Look only just past the subject: "I need to read" matches, while
		// "I need to explain why the code reads a file" does not.
		window := lower[at:]
		if len(window) > 80 {
			window = window[:80]
		}
		for _, v := range verbs {
			if strings.Contains(window, v) {
				return true
			}
		}
	}
	return false
}

// replyClosing returns the part of a reply that decides whether it is an
// answer: its last paragraph, capped to the final ~320 characters, with any
// sentence that offers optional follow-up to the USER removed.
//
// The closing, not the whole reply, because "I'll start by describing the
// flow" followed by the description is an answer, while the same words as
// the last thing said are a promise. Offers are removed because "If you'd
// like, I can fix it" and "let me know if you want me to look at scoring.py"
// hand the next step to the user; they do not leave the agent's own work
// undone.
func replyClosing(text string) string {
	t := strings.TrimSpace(text)
	if i := strings.LastIndex(t, "\n\n"); i >= 0 && len(strings.TrimSpace(t[i:])) > 0 {
		t = strings.TrimSpace(t[i:])
	}
	if len(t) > 320 {
		t = t[len(t)-320:]
	}
	var kept []string
	for _, sentence := range replySentenceRe.FindAllString(t, -1) {
		if replyOfferRe.MatchString(sentence) {
			continue
		}
		kept = append(kept, sentence)
	}
	return strings.ToLower(strings.Join(kept, " "))
}

var (
	replySentenceRe = regexp.MustCompile(`[^.!?\n]+(?:[.!?]+|$)`)
	replyOfferRe    = regexp.MustCompile(`(?i)\blet me know\b|\bif you(?:'d| would)? (?:like|want|prefer)\b|\bwould you like\b|\bwant me to\b|\bshall i\b|\bhappy to\b|\bi can also\b`)
)

// replyDefersWork reports a reply whose closing hands off to work the agent
// has not done: an announced next action ("I will now check the
// generate_plan function"), a result it says is still pending ("Please wait
// while I verify the exact line"), or promised content never given.
//
// A `text` reply ends the session, so a reply that closes this way is not an
// answer -- the work it names will never happen. Measured: every
// bugfind_tiebreak session across four benchmark runs (8 of 8) ended on such
// a closing and was reported completed. announcesImminentToolUse already
// caught the first shape, but only before any tool had run, which is exactly
// when a model that has read one file and means to read another is not.
func replyDefersWork(text string) bool {
	closing := replyClosing(text)
	if closing == "" {
		return false
	}
	for _, wait := range []string{"please wait", "one moment", "hold on", "stand by", "bear with me"} {
		if strings.Contains(closing, wait) {
			return true
		}
	}
	for _, pending := range []string{"once i verify", "once i confirm", "once i check",
		"after i verify", "after i confirm", "after i check", "to confirm the exact",
		"to verify the exact"} {
		if strings.Contains(closing, pending) {
			return true
		}
	}
	subjects := []string{"i will ", "i'll ", "let me ", "i need to ", "i'm going to ",
		"i am going to ", "next, i ", "now i ", "i am now ", "i'm now ", "which i will "}
	verbs := []string{"read", "look at", "look into", "open", "inspect", "examine", "outline",
		"check", "search", "investigate", "dig into", "trace", "review the", "take a look",
		"verify", "confirm", "run ", "test", "locate", "find ", "determine", "analyze",
		"analyse", "compare", "debug", "reproduce"}
	for _, sub := range subjects {
		if subjectAnnouncesAction(closing, sub, verbs) {
			return true
		}
	}
	return promisesMoreContent(text)
}

// subjectAnnouncesAction reports whether a use of the subject sub in closing
// is followed, within 60 characters and past any lead-in words, by one of
// verbs: "i will check", "let me now read".
func subjectAnnouncesAction(closing, sub string, verbs []string) bool {
	for from := 0; ; {
		at := strings.Index(closing[from:], sub)
		if at < 0 {
			break
		}
		at += from
		window := closing[at+len(sub):]
		if len(window) > 60 {
			window = window[:60]
		}
		window = trimReplyLeadIns(window) // "i will try to read" defers as "i will read" does
		for _, v := range verbs {
			if strings.HasPrefix(window, v) {
				return true
			}
		}
		from = at + len(sub)
	}
	return false
}

// replyAsksUser reports a reply whose last sentence asks the USER to supply
// something -- a fact, a choice, a file, where to look -- rather than offering
// optional follow-up or checking in. It says nothing about whether asking was
// right; exitGates decides that from what the run could inspect.
//
// Only the final sentence that is not an offer or a courtesy counts: "Which
// approach is safer? The second, because..." answers its own question, while
// "Could you tell me which files handle the pricing cache?" leaves the turn
// with the user. Measured on c5927b3 acceptance family S: that reply, with no
// tool call in a workspace of five source files, was reported completed.
func replyAsksUser(text string) bool {
	t := strings.TrimSpace(text)
	if i := strings.LastIndex(t, "\n\n"); i >= 0 && len(strings.TrimSpace(t[i:])) > 0 {
		t = strings.TrimSpace(t[i:])
	}
	if len(t) > 400 {
		t = t[len(t)-400:]
	}
	sentences := replySentenceRe.FindAllString(t, -1)
	for k := len(sentences) - 1; k >= 0; k-- {
		sen := strings.ToLower(strings.TrimSpace(sentences[k]))
		if sen == "" {
			continue
		}
		if userRequestRe.MatchString(sen) {
			return true
		}
		if replyOfferRe.MatchString(sen) || replyCourtesyRe.MatchString(sen) {
			continue
		}
		return strings.HasSuffix(sen, "?") && userQuestionRe.MatchString(sen)
	}
	return false
}

var (
	// Asking the user to provide or identify something, in any sentence form.
	userRequestRe = regexp.MustCompile(`\b(?:could|can|would|will) you(?: please)? (?:tell|share|provide|paste|send|upload|show|point|specify|clarify|confirm|describe|give|list|identify|indicate|let me know)\b|\bplease (?:tell|share|provide|paste|send|upload|show|point|specify|clarify|confirm|describe|give|list|identify|indicate)\b|\blet me know (?:which|what|where|how|whether|the)\b`)
	// A direct question about the user's situation or preference.
	userQuestionRe = regexp.MustCompile(`(?:^|[:;,]\s*)(?:which|what|where|how|when|who|do you|did you|have you|are you|were you|is it|is this|is that|should the|should it|can you|could you|would you)\b`)
	// Checking in after an answer: not a request for anything.
	replyCourtesyRe = regexp.MustCompile(`\b(?:does (?:that|this) (?:help|answer|make sense)|makes? sense\?|any (?:other|more|further) questions|anything else|is (?:that|this) what you)\b`)
)

// replyDeclaresInability reports a reply whose closing says, in the first
// person, that the agent could not accomplish what was asked -- "I could not
// determine the cause from these files". That is an honest outcome, and it is
// not a completed answer: the session ends incomplete instead of reporting
// success. Deliberately narrow: an environment limitation stated alongside an
// answer ("I couldn't run the tests, but the fix is ...") names no outcome
// verb and does not match, and third-person descriptions of code ("the
// function is unable to handle an empty list") have no first-person subject.
func replyDeclaresInability(text string) bool {
	closing := replyClosing(text)
	if closing == "" {
		return false
	}
	subjects := []string{"i could not ", "i couldn't ", "i was unable to ", "i am unable to ",
		"i'm unable to ", "i cannot ", "i can't ", "i was not able to ", "i wasn't able to ",
		"i am not able to ", "i'm not able to "}
	outcomes := []string{"determine", "find", "identify", "locate", "complete", "finish",
		"answer", "figure out", "work out", "pin down", "resolve", "tell"}
	for _, sub := range subjects {
		at := strings.Index(closing, sub)
		if at < 0 {
			continue
		}
		window := closing[at+len(sub):]
		if len(window) > 40 {
			window = window[:40]
		}
		for _, o := range outcomes {
			if strings.HasPrefix(window, o) || strings.HasPrefix(window, "fully "+o) ||
				strings.HasPrefix(window, "reliably "+o) {
				return true
			}
		}
	}
	return false
}

// fileCitationRe matches a filename as it appears in prose: `scoring.py`,
// planning.py, src/app/main.go. The extension must start with a letter and run
// 1-5 characters, so version strings ("V3.2") and decimals ("0.34") are not
// read as paths. Anything it over-matches is discarded by the existence check
// in unreadFileCitations.
var fileCitationRe = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_./-]*\.[A-Za-z][A-Za-z0-9]{0,4}`)

// maxCitedPaths caps how many unread files one rejection names. Listing every
// one turns the correction into a chore; the model needs the shape and a
// couple of concrete targets.
const maxCitedPaths = 3

// unreadFileCitations returns files in the workspace that the reply makes a
// claim about without the run ever having been shown their contents.
//
// A reply that names a file it never opened is guessing, and the guess reads
// exactly like knowledge. Measured on a diagnostic question across three
// modules: 12 of 12 sessions ran list_directory, outlined ONE file, and
// answered. Which file they guessed decided the outcome — scoring.py wrong
// 11/11, planning.py right 1/1 — because the prompt said "scored" and the
// filename matched. One session cited "lines 134-142" of a file whose body it
// had never seen.
//
// The predicate is existence plus absence of evidence, not a judgement about
// the claim: the file has to be real, and the run has to have never read it.
// A file the model wrote is evidence enough, since it authored the contents,
// and so is one it moved or deleted: naming it reports the operation, not a
// guess about code it never saw.
func unreadFileCitations(ctx *AgentContext, text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range fileCitationRe.FindAllString(text, -1) {
		name := strings.Trim(m, "./-")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		resolved, info, err := statWorkspaceFile(ctx, name) // "x/../../y" is never looked up
		if err != nil || info.IsDir() {
			continue
		}
		if ctx.WasBodySeen(resolved) || ledgerTracks(ctx, resolved) {
			continue
		}
		out = append(out, name)
		if len(out) == maxCitedPaths {
			break
		}
	}
	return out
}

// unreadCitationMessage tells the model to look before it answers.
//
// It names outline_file explicitly because that is the tool the failure runs
// through: an outline lists signatures and line ranges with no bodies, which
// is enough scaffolding to state a confident, specific, wrong claim about what
// those lines contain.
func unreadCitationMessage(paths []string) string {
	var b strings.Builder
	b.WriteString("Your reply makes a claim about ")
	for i, p := range paths {
		switch {
		case i == 0:
		case i == len(paths)-1:
			b.WriteString(" and ")
		default:
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "`%s`", p)
	}
	subject, object := "that file", "it"
	if len(paths) > 1 {
		subject, object = "those files", "each of them"
	}
	fmt.Fprintf(&b, ", but this session has never seen the contents of %s. ", subject)
	fmt.Fprintf(&b, "outline_file lists signatures and line ranges only — it shows you no code, "+
		"so anything you say about what those lines do is a guess. "+
		"Call read_file on %s now, and on any other file you are about to name as the cause, "+
		"then answer from what the code actually says. ", object)
	b.WriteString(
		"If you are not sure which file holds the problem, search_files for the relevant symbol " +
			"across the whole directory rather than picking the file whose name matches the question.")
	return b.String()
}

// codeRefRe matches a code reference in backticks: `name`, `name()`,
// `module.name`.
var codeRefRe = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_]*(?:\\.[A-Za-z_][A-Za-z0-9_]*)*)(?:\\(\\))?`")

// fileExtensionWords are the last parts of a backticked file name
// (`config.json`), which name a file, not code.
var fileExtensionWords = map[string]bool{
	"json": true, "yaml": true, "yml": true, "toml": true, "html": true, "txt": true,
	"java": true, "tsx": true, "jsx": true, "cpp": true, "php": true, "css": true,
}

// unshownSymbol is code a reply names whose definition no read showed.
type unshownSymbol struct {
	Name  string
	File  string // as the workspace names it
	Line  int    // the definition's line
	Spans [][2]int
}

// definitionLines returns the 1-based lines of source that define name: a
// def, class, func, function, fn or type line, at any indent.
func definitionLines(source, name string) []int {
	re := regexp.MustCompile(`^\s*(?:export\s+)?(?:(?:async|static|public|private|protected|pub)\s+)*` +
		`(?:def|class|function|fn|type|func(?:\s*\([^)]*\))?)\s+` + regexp.QuoteMeta(name) + `\b`)
	var out []int
	for i, ln := range strings.Split(source, "\n") {
		if re.MatchString(ln) {
			out = append(out, i+1)
		}
	}
	return out
}

// workspaceName is how the model names a file: relative to the workspace.
func workspaceName(ctx *AgentContext, path string) string {
	if rel, err := filepath.Rel(ctx.WorkingDir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return filepath.Base(path)
}

// unshownSymbolMessage tells the model which code it described without being
// shown it, and where that code is.
func unshownSymbolMessage(gaps []unshownSymbol) string {
	var b strings.Builder
	b.WriteString("Your reply describes code this session has not shown you. ")
	for _, g := range gaps {
		fmt.Fprintf(&b, "`%s` is defined at line %d of %s, and the reads so far showed only %s of that file. ",
			g.Name, g.Line, g.File, lineSpanText(g.Spans))
	}
	b.WriteString("A truncated or ranged read shows only part of a file, so what you say about the rest " +
		"is a guess. Read the lines you are describing (read_file with offset and limit), then answer " +
		"from what the code actually says.")
	return b.String()
}

// symbolNames lists the names in gaps, for logs and the unresolved note.
func symbolNames(gaps []unshownSymbol) string {
	names := make([]string, len(gaps))
	for i, g := range gaps {
		names[i] = "`" + g.Name + "`"
	}
	return strings.Join(names, ", ")
}

// lineSpanText renders line spans as "lines 1-189 and 400-420".
func lineSpanText(spans [][2]int) string {
	if len(spans) == 0 {
		return "no lines"
	}
	parts := make([]string, len(spans))
	for i, s := range spans {
		if s[0] == s[1] {
			parts[i] = strconv.Itoa(s[0])
		} else {
			parts[i] = fmt.Sprintf("%d-%d", s[0], s[1])
		}
	}
	if len(parts) == 1 {
		return "lines " + parts[0]
	}
	return "lines " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// reInvestigationScope matches a request whose subject is more than one file:
// "these files", "across the modules", "the codebase", "this directory". It
// reads the user's own words and nothing else -- no plan, no task name, no
// knowledge of any particular project.
var reInvestigationScope = regexp.MustCompile(`(?i)\b(?:these|those)\s+(?:\w+\s+){0,2}(?:files|modules|scripts|services|packages)\b` +
	`|\bacross\b[^.]{0,40}?\b(?:files|modules|codebase|project|services|packages)\b` +
	`|\bthe\s+(?:codebase|repo|repository)\b` +
	`|\bthis\s+(?:directory|repo|repository)\b` +
	`|\b(?:each|every|all)\s+(?:of\s+the\s+)?(?:files|modules)\b`)

// investigationScopeUnmet answers one question about a read-only run: the user
// asked how several files behave, so does the answer account for more than one
// of the files this run actually opened?
//
// Measured (stabilization cycle 3, family S): asked to walk through how the
// pricing cache works ACROSS five modules and why the hit rate is low, the run
// read three of them and answered with one function pair in one file. Every
// existing exit gate passed -- nothing was written, nothing was deferred, no
// question was asked, no file was cited unread -- so the reply was finalized
// completed. Half an investigation and a whole one were indistinguishable.
//
// The evidence is the ordinary request and the run's own reads. Nothing here
// judges whether the answer is correct, or good: that is not decidable from
// the workspace, and a rule that pretended otherwise would be a guess with a
// status attached.
func investigationScopeUnmet(ctx *AgentContext, userMessage, claimText string,
	readOnly bool, toolsRun int) (read, cited []string, unmet bool) {
	if ctx == nil || !readOnly || toolsRun == 0 || strings.TrimSpace(claimText) == "" {
		return nil, nil, false
	}
	if !reInvestigationScope.MatchString(userMessage) {
		return nil, nil, false
	}
	read = bodiesSeenInRun(ctx)
	if len(read) < 2 {
		// One file opened cannot span anything; the answer is judged by the
		// gates that already exist.
		return read, nil, false
	}
	for _, name := range read {
		if strings.Contains(claimText, name) || answerNamesSymbolFrom(ctx, name, claimText) {
			cited = append(cited, name)
		}
	}
	return read, cited, len(cited) < 2
}

// bodiesSeenInRun lists the base names of files whose contents this run was
// actually shown, sorted, so a message about them reads the same way twice.
func bodiesSeenInRun(ctx *AgentContext) []string {
	seen := map[string]bool{}
	ctx.mu.Lock()
	for path, ok := range ctx.BodySeen {
		if ok {
			seen[filepath.Base(path)] = true
		}
	}
	ctx.mu.Unlock()
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// answerNamesSymbolFrom reports whether the answer accounts for a file by
// naming something defined in it. Every read file of that base name counts:
// the caller counts files by base name, and taking the first of two would
// leave the answer to whichever one the map gave first.
//
// Measured (stabilization cycle 5, audit of this guard): an answer that traced
// the fault correctly through read_key and write_key, naming one file, was
// sent back as a partial investigation. Explaining a relationship through the
// functions involved is how such an answer is normally written, and the
// symbols come from the bytes THIS run read — not from any task knowledge.
//
// It makes the guard more permissive, which is the intended direction: the
// guard is a floor against answering for one file out of several, not a test
// of whether an answer is correct.
func answerNamesSymbolFrom(ctx *AgentContext, base, claimText string) bool {
	ctx.mu.Lock()
	sources := map[string]string{}
	for p, content := range ctx.FilesRead {
		if filepath.Base(p) == base {
			sources[p] = content
		}
	}
	ctx.mu.Unlock()
	for path, source := range sources {
		for _, sym := range outlineByRegex(path, source) {
			// Short names ("get", "id") appear in ordinary prose; requiring four
			// characters keeps the match to something the file actually declares.
			// Whole-word only: "price" must not be credited to the word "pricing".
			if len(sym.Name) >= 4 && mentionsWord(claimText, sym.Name) {
				return true
			}
		}
	}
	return false
}

// mentionsWord reports whether text contains name as a whole identifier —
// neither a prefix of a longer word nor a suffix of one.
func mentionsWord(text, name string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], name)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(name)
		beforeOK := start == 0 || !isIdentRune(rune(text[start-1]))
		afterOK := end == len(text) || !isIdentRune(rune(text[end]))
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
	}
}

func isIdentRune(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// investigationScopeMessage asks for the coverage the request asked for. It
// names the files the run opened and says nothing about what is in them.
func investigationScopeMessage(read, cited []string) string {
	var sb strings.Builder
	sb.WriteString("The request asks about how several files behave together. ")
	switch len(cited) {
	case 0:
		fmt.Fprintf(&sb, "Your reply does not mention any of the files you read (%s). ", strings.Join(read, ", "))
	default:
		fmt.Fprintf(&sb, "Your reply accounts for %s, but you read %s. ", strings.Join(cited, ", "), strings.Join(read, ", "))
	}
	sb.WriteString("Answer for the files you examined: say what each one contributes and how they connect, " +
		"in a single `text` reply that stands on its own. If a file you opened turned out to be irrelevant, " +
		"say so and why. Do not change any files.")
	return sb.String()
}

// replyLeadIns are words between an announcing subject ("i will", "let me")
// and its verb that leave the announcement unchanged: "i will try to read it"
// defers the read exactly as "i will read it" does.
var replyLeadIns = []string{"now ", "first ", "next ", "then ", "try to ", "attempt to ",
	"try and ", "go ahead and "}

// trimReplyLeadIns drops leading lead-ins, in any order and number.
func trimReplyLeadIns(s string) string {
	for changed := true; changed; {
		changed = false
		for _, l := range replyLeadIns {
			if strings.HasPrefix(s, l) {
				s, changed = s[len(l):], true
			}
		}
	}
	return s
}
