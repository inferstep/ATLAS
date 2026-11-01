package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A `text` reply ends the turn, so a reply that announces a tool call instead
// of making one stops with the intent and no action. An answer that only
// mentions reading is not an announcement.
func TestAnnouncesImminentToolUse(t *testing.T) {
	for _, c := range []struct {
		text      string
		announces bool
	}{
		{"I need to read the `orders.py` file to explain what find_duplicates does.", true},
		{"I'll start by outlining the file to locate the function.", true},
		{"Let me look at the implementation first.", true},
		{"I'm going to check store.py before answering.", true},
		{"I'll look into the contact form's implementation to see how it handles submissions and where the data is sent.", true},
		{"Let me look through the source to find where that is set.", true},
		{"I'm going to investigate how routing works here.", true},
		{"First, I will read app.py to understand the structure.", true},
		{"I'll dig into the handler and report back.", true},
		{"find_duplicates is O(n^2): it reads the list inside a nested loop.", false},
		{"I looked at both files: routing is handled by Flask's @app.route decorators.", false},
		{"", false},
	} {
		t.Run(c.text, func(t *testing.T) {
			if got := announcesImminentToolUse(c.text); got != c.announces {
				t.Errorf("announcesImminentToolUse = %v, want %v", got, c.announces)
			}
		})
	}
}

// A reply that signs off promising the answer leaves the user half of one. A
// promise followed by what it promised is a complete answer.
func TestPromisesMoreContent(t *testing.T) {
	for _, c := range []struct {
		text     string
		promises bool
	}{
		{"The bug is in planning.py. I will now provide the specific location and the incorrect comparison as requested.", true},
		{"I've analysed the file. Let me give you the exact line.", true},
		{"Here's the summary so far. I'll now show the failing comparison.", true},
		{"planning.py breaks ties with n_steps > best_steps, so a tie keeps the longer plan.", false},
		{"I'll provide the details: the comparison on line 314 uses > where it should use <.", false},
		{"", false},
	} {
		t.Run(c.text, func(t *testing.T) {
			if got := promisesMoreContent(c.text); got != c.promises {
				t.Errorf("promisesMoreContent = %v, want %v", got, c.promises)
			}
		})
	}
}

// An investigation answers for what it opened. For a read-only run whose
// request names several files, an answer that accounts for fewer than two of
// the files the run opened goes back once; if it does not improve, the run
// ends incomplete. The evidence is the user's own words and the run's own
// reads: no plan, no task name, no judgement of whether the answer is right.

// The start of the message that sends a partial answer back.
const scopeBounce = "The request asks about how several files behave together"

const spanRequest = "walk me through how the pricing cache works across these files and tell me why we " +
	"might be seeing a lower hit rate than expected. dont change anything yet"

func spanWorld(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"cache.py":   "_store = {}\n\n\ndef get(key):\n    return _store.get(key)\n\n\ndef put(key, value):\n    _store[key] = value\n",
		"keys.py":    "def write_key(sku, customer):\n    return f\"{sku}:{customer.id}\"\n\n\ndef read_key(sku, customer):\n    return f\"{sku}:{customer.tier}\"\n",
		"pricing.py": "import cache\nimport keys\n\n\ndef price(sku, customer):\n    hit = cache.get(keys.read_key(sku, customer))\n    if hit is not None:\n        return hit\n    value = sku.base * 2\n    cache.put(keys.write_key(sku, customer), value)\n    return value\n",
	}
}

func spanReadThree() []string {
	return []string{
		toolCall("read_file", map[string]interface{}{"path": "cache.py"}),
		toolCall("read_file", map[string]interface{}{"path": "keys.py"}),
		toolCall("read_file", map[string]interface{}{"path": "pricing.py"}),
	}
}

func spanText(s string) string {
	b, _ := json.Marshal(map[string]string{"type": "text", "content": s})
	return string(b)
}

// One relevant fact, found in one file, is not the walk-through that was asked
// for: it goes back once, and a reply that does not improve ends incomplete.
func TestAPartialInvestigationDoesNotCompleteOnOneFact(t *testing.T) {
	thin := spanText("The hit rate is low because of a key mismatch in `keys.py`: write_key uses the customer id " +
		"and read_key uses the tier, so lookups miss.")
	turns := append(spanReadThree(), thin, thin, thin, thin, thin)
	run := integrityLoop(t, spanRequest, spanWorld(t), turns, nil)
	if !run.told(scopeBounce) {
		t.Error("the model was never sent back for the coverage the request asked for")
	}
	if !run.told("cache.py, keys.py, pricing.py") {
		t.Error("the message does not name the files the run opened")
	}
	if got := run.terminal["reason"]; got != "investigation_scope_unmet" {
		t.Errorf("terminal %s / %s, want incomplete / investigation_scope_unmet", run.terminal["status"], got)
	}
	for name, body := range spanWorld(t) {
		if got, _ := run.disk(t, name); got != body {
			t.Errorf("%s changed during a read-only request", name)
		}
	}
}

// An answer that accounts for the files the run read completes on the first
// try. The guard counts the files an answer accounts for, not whether it is
// right, so naming every file without the relationship also passes:
// correctness cannot be decided from the workspace.
func TestAnAnswerThatAccountsForTheFilesItReadCompletes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
	}{
		{
			name: "names the files and how they work together",
			answer: "`pricing.py` looks the price up in `cache.py` before computing it, and builds both keys " +
				"through `keys.py`. The write path stores under write_key (the customer id) and the read path looks up " +
				"read_key (the customer tier), so the two never agree and cache.py almost always misses.",
		},
		{
			// Names one file, but explains the relationship through the
			// symbols of the others it read.
			name: "explains through the symbols it read",
			answer: "The price lookup calls read_key to build the cache key, but the value was stored " +
				"under write_key — read_key uses the customer tier where write_key used the id, so get() never finds " +
				"what put() wrote, and every lookup in keys.py misses.",
		},
		{
			name: "names the files without the relationship",
			answer: "I looked at cache.py, keys.py and pricing.py. They are all part of the pricing path. " +
				"Something in there is causing the misses.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := integrityLoop(t, spanRequest, spanWorld(t), append(spanReadThree(), spanText(tc.answer)), nil)
			if run.told(scopeBounce) {
				t.Error("the answer was sent back")
			}
			if run.terminal["status"] != "completed" || run.terminal["reason"] != "text_reply" {
				t.Errorf("terminal %s / %s, want completed / text_reply", run.terminal["status"], run.terminal["reason"])
			}
		})
	}
}

// A question about one named file is not a multi-file investigation, so a
// one-file answer stands.
func TestASingleFileQuestionIsUnaffected(t *testing.T) {
	run := integrityLoop(t, "In keys.py, what does read_key return? Just explain — do not change any code.",
		spanWorld(t), []string{
			toolCall("read_file", map[string]interface{}{"path": "keys.py"}),
			spanText("`read_key` returns the string \"<sku>:<customer tier>\"."),
		}, nil)
	if run.terminal["status"] != "completed" {
		t.Errorf("terminal %s / %s, want completed", run.terminal["status"], run.terminal["reason"])
	}
}

// The rule reads the user's words and the run's reads. A plan that invented
// extra steps cannot make a complete answer incomplete.
func TestAnInventedPlanStepDoesNotMakeACompleteAnswerIncomplete(t *testing.T) {
	full := spanText("`pricing.py` reads through `cache.py` and builds its keys with `keys.py`; the write and " +
		"read key shapes differ, so lookups miss.")
	run := integrityLoopWith(t, spanRequest, spanWorld(t), append(spanReadThree(), full), nil, func(ctx *AgentContext) {
		ctx.Plan = &Plan{VerifyStep: "s5", Steps: []PlanStep{
			{ID: "s1", Action: "read_file", Target: "cache.py"},
			{ID: "s2", Action: "read_file", Target: "keys.py"},
			{ID: "s3", Action: "read_file", Target: "pricing.py"},
			{ID: "s4", Action: "read_file", Target: "orders.py"},
			{ID: "s5", Action: "write_file", Target: "notes.md"},
		}}
	})
	if run.terminal["status"] != "completed" {
		t.Errorf("terminal %s / %s, want completed despite unfinished plan steps",
			run.terminal["status"], run.terminal["reason"])
	}
}

// A reply that asks the user keeps its own outcome: a question is not a thin
// answer, and the two must stay distinguishable.
func TestAClarificationKeepsItsOwnOutcome(t *testing.T) {
	ask := spanText("Which of these files holds the cache you mean, and should I look at the request path too?")
	run := integrityLoop(t, spanRequest, spanWorld(t), append(spanReadThree(), ask, ask, ask, ask, ask), nil)
	if got := run.terminal["reason"]; got != "clarification_requested" && got != "investigation_handed_back" {
		t.Errorf("terminal %s / %s, want a clarification outcome", run.terminal["status"], got)
	}
}

// The gate is scoped to read-only investigation: a run that changed files is
// judged by its deliverables, not by how many files its prose mentions.
func TestAWriteRunIsNotJudgedByHowManyFilesItMentions(t *testing.T) {
	run := integrityLoop(t, "across these files, make read_key match write_key", spanWorld(t), []string{
		toolCall("read_file", map[string]interface{}{"path": "keys.py"}),
		toolCall("edit_file", map[string]interface{}{"path": "keys.py",
			"old_str": "return f\"{sku}:{customer.tier}\"", "new_str": "return f\"{sku}:{customer.id}\""}),
		`{"type":"done","summary":"read_key now builds the same key as write_key."}`,
	}, nil)
	if run.told(scopeBounce) {
		t.Error("the scope gate fired on a run that changed files")
	}
	want := "def write_key(sku, customer):\n    return f\"{sku}:{customer.id}\"\n\n\ndef read_key(sku, customer):\n    return f\"{sku}:{customer.id}\"\n"
	if got, _ := run.disk(t, "keys.py"); got != want {
		t.Errorf("the edit did not land: %q", got)
	}
}

// The bounce names the files the run opened and asks for coverage. It says
// nothing about what is in them.
func TestTheScopeMessageCarriesNoAnswer(t *testing.T) {
	msg := investigationScopeMessage([]string{"cache.py", "keys.py", "pricing.py"}, []string{"keys.py"})
	for _, leak := range []string{"mismatch", "customer", "tier", "hit rate", "write_key", "read_key"} {
		if strings.Contains(strings.ToLower(msg), leak) {
			t.Errorf("the bounce message leaks %q: %s", leak, msg)
		}
	}
	if !strings.Contains(msg, "cache.py, keys.py, pricing.py") {
		t.Errorf("the message does not name the files read: %s", msg)
	}
}

// The workspace is untouched by the gate itself.
func TestTheScopeGateWritesNothing(t *testing.T) {
	dir := t.TempDir()
	for name, body := range spanWorld(t) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	for name := range spanWorld(t) {
		ctx.RecordBodySeen(filepath.Join(dir, name))
	}
	before, _ := os.ReadDir(dir)
	read, cited, unmet := investigationScopeUnmet(ctx, spanRequest, "keys.py explains it", true, 3)
	if !unmet || len(read) != 3 || len(cited) != 1 {
		t.Fatalf("read=%v cited=%v unmet=%v", read, cited, unmet)
	}
	if after, _ := os.ReadDir(dir); len(after) != len(before) {
		t.Error("the check changed the workspace")
	}
}

// A real partial answer, byte for byte, against the workspace it was given:
// the rule must catch the real reply, not only a paraphrase of it.
func TestARecordedRealPartialAnswerIsCaughtByTheRule(t *testing.T) {
	answer, err := os.ReadFile("testdata/s_answer.txt")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// The real workspace, byte for byte, so the symbols are the real ones.
	for _, name := range []string{"cache.py", "catalog.py", "keys.py", "orders.py", "pricing.py"} {
		body, err := os.ReadFile(filepath.Join("testdata", "s_fixture", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	// What that run opened: three of the five modules.
	for _, name := range []string{"cache.py", "pricing.py", "keys.py"} {
		full := filepath.Join(dir, name)
		body, _ := os.ReadFile(full)
		ctx.RecordFileRead(full, string(body))
		ctx.RecordBodySeen(full)
	}
	read, cited, unmet := investigationScopeUnmet(ctx, spanRequest, string(answer), true, 4)
	if !unmet {
		t.Errorf("the captured answer passed: read=%v cited=%v", read, cited)
	}
	if len(cited) != 1 || cited[0] != "keys.py" {
		t.Errorf("cited = %v, want [keys.py] — the one file that answer accounts for", cited)
	}
}

// Two files of one base name were read. An answer through the symbols of
// either one is credited every time: map order must not decide it.
func TestASymbolOfEitherOfTwoFilesWithOneNameIsCreditedEachTime(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.RecordFileRead(filepath.Join(dir, "client", "keys.py"), "def read_key(customer):\n    return customer.tier\n")
	ctx.RecordFileRead(filepath.Join(dir, "server", "keys.py"), "def write_key(customer):\n    return customer.id\n")
	for try := 1; try <= 100; try++ {
		if !answerNamesSymbolFrom(ctx, "keys.py", "The value was stored under write_key, by the id.") {
			t.Fatalf("try %d: an answer through the symbol of one of the two files was not credited", try)
		}
	}
}

// An answer that names a file the run never opened is a guess, and it reads
// exactly like knowledge. The evidence gate finds such citations and asks the
// model to read the file before it answers.

func evidenceCtx(t *testing.T, files map[string]string) *AgentContext {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &AgentContext{
		WorkingDir:    root,
		BodySeen:      map[string]bool{},
		FilesRead:     map[string]string{},
		FileReadTimes: map[string]time.Time{},
	}
}

// A reply cites a workspace file whose body the run never saw. Names that are
// not workspace files are not citations.
func TestUnreadFileCitations(t *testing.T) {
	onlyRealPy := map[string]string{"real.py": "x = 1\n"}
	for _, tc := range []struct {
		name   string
		files  map[string]string
		seen   []string
		answer string
		want   []string
	}{
		{
			// A file, a function and a line range, none of which the model
			// was ever shown.
			name: "an unread file is cited",
			files: map[string]string{
				"scoring.py":  "def score_candidate():\n    pass\n",
				"planning.py": "def select_best():\n    pass\n",
			},
			answer: "The issue is in `scoring.py` within the `score_candidate` " +
				"function (lines 134-142).",
			want: []string{"scoring.py"},
		},
		{
			// Every read, write and edit path records the body as seen.
			name:   "a file whose body was seen is not cited",
			files:  map[string]string{"scoring.py": "x = 1\n"},
			seen:   []string{"scoring.py"},
			answer: "The bug is in scoring.py",
		},
		{name: "a version number", files: onlyRealPy, answer: "upgrade to V3.2 for this"},
		{name: "a decimal number", files: onlyRealPy, answer: "the score dropped to 0.34"},
		{name: "a module path", files: onlyRealPy, answer: "import os.path at the top"},
		{name: "a file not in the workspace", files: onlyRealPy, answer: "see requirements.txt for the pins"},
		{name: "an empty reply", files: onlyRealPy, answer: "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := evidenceCtx(t, tc.files)
			for _, name := range tc.seen {
				ctx.RecordBodySeen(resolveAgentPath(ctx, name))
			}
			if got := unreadFileCitations(ctx, tc.answer); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("cited %v, want %v", got, tc.want)
			}
		})
	}
}

// The distinction the gate rests on: outline_file caches the whole source for
// staleness tracking, so FilesRead says "read" for a file the model has only
// ever seen the signatures of.
func TestOutlineDoesNotCountAsSeeingTheBody(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"scoring.py": "def f():\n    pass\n"})
	path := resolveAgentPath(ctx, "scoring.py")
	// What outline_file does: cache the source, show the model signatures.
	ctx.RecordFileRead(path, "def f():\n    pass\n")
	if !ctx.WasFileRead(path) {
		t.Fatal("outline still populates the read cache, by design")
	}
	if ctx.WasBodySeen(path) {
		t.Fatal("an outline shows no code and must not count as seeing the body")
	}
	if got := unreadFileCitations(ctx, "the bug is in scoring.py"); len(got) != 1 {
		t.Fatalf("outlined-only file should still be cited, got %v", got)
	}
}

func TestTheRejectionNamesTheFileAndTheFix(t *testing.T) {
	msg := unreadCitationMessage([]string{"scoring.py"})
	for _, want := range []string{"scoring.py", "read_file", "outline_file", "search_files"} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection should mention %q: %s", want, msg)
		}
	}
}

func TestTheRejectionIsCappedAndReadable(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{
		"a.py": "1", "b.py": "1", "c.py": "1", "d.py": "1",
	})
	got := unreadFileCitations(ctx, "look at a.py, b.py, c.py and d.py")
	if len(got) != maxCitedPaths {
		t.Fatalf("want %d cited paths, got %v", maxCitedPaths, got)
	}
	msg := unreadCitationMessage(got)
	for _, p := range got {
		if !strings.Contains(msg, p) {
			t.Errorf("cited %s but the rejection does not name it: %s", p, msg)
		}
	}
	// Plural subject, plural object: "read_file on it" is wrong for three files.
	if !strings.Contains(msg, "those files") || !strings.Contains(msg, "each of them") {
		t.Errorf("plural rejection reads as singular: %s", msg)
	}
	if strings.Contains(msg, ",,") || strings.Contains(msg, " and ,") {
		t.Errorf("list punctuation is malformed: %s", msg)
	}
}

// The outline_file description must not sell an outline as a cheaper read.
// A model told to reach for an outline first, instead of reading the file,
// follows that advice and then answers from signatures it never saw the code
// of. read_file already caps itself on a file too large to load, so frugality
// advice adds no guard and costs correctness.
func TestOutlineIsNotSoldAsACheaperRead(t *testing.T) {
	def := outlineFileTool()
	for _, banned := range []string{"almost no context", "instead of reading the whole"} {
		if strings.Contains(def.Description, banned) {
			t.Errorf("outline_file must not be pitched as a cheaper read: %q", banned)
		}
	}
	// It has to say what it withholds, or an outline reads as having read.
	if !strings.Contains(def.Description, "NO code") {
		t.Errorf("outline_file must state that it returns no code: %s", def.Description)
	}
}

// Every write path has to record the body, not just the one I checked.
//
// The gate shipped recording only on the T1 direct write, so files written
// through the V3 pipeline stayed unmarked and the gate fired on code the
// model had authored moments earlier. Measured on run 18: 11 of 12 firings
// were solve.py or test_stats.py, every one a V3-path write. This asserts
// the funnel rather than the call sites, since the bug was a call site
// nobody updated.
func TestEveryWritePathRunsThroughTheRecorder(t *testing.T) {
	for _, file := range toolFiles {
		for i, line := range strings.Split(readSourceForTest(t, file), "\n") {
			if !strings.Contains(line, "writeFileDirect(") {
				continue
			}
			// Its definition and the one call inside the recorder are the only
			// legitimate mentions.
			if strings.Contains(line, "func writeFileDirect") ||
				strings.Contains(line, "res, err := writeFileDirect(path, content)") {
				continue
			}
			t.Errorf("%s:%d calls writeFileDirect directly, so a write there "+
				"never records the body and the evidence gate will fire on a file "+
				"the model just wrote: %s", file, i+1, strings.TrimSpace(line))
		}
	}
}

// A citation that climbs out of the workspace is never looked up, so a reply
// cannot learn whether a file outside it exists.
func TestACitationOutsideTheWorkspaceIsNotLookedUp(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"real.py": "x = 1\n"})
	outside := filepath.Join(filepath.Dir(ctx.WorkingDir), "secret.txt")
	if err := os.WriteFile(outside, []byte("s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := unreadFileCitations(ctx, "the key is in x/../../secret.txt"); len(got) != 0 {
		t.Fatalf("a file outside the workspace was looked up: %v", got)
	}
}

// The script a command names is read only inside the workspace.
func TestAScriptOutsideTheWorkspaceIsNotRead(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"app.py": "inside\n"})
	outside := filepath.Join(filepath.Dir(ctx.WorkingDir), "server.py")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	read := workspaceFileReader(ctx)
	if got, ok := read("app.py"); !ok || got != "inside\n" {
		t.Fatalf("app.py = %q, %v", got, ok)
	}
	if got, ok := read("../server.py"); ok {
		t.Fatalf("read a script outside the workspace: %q", got)
	}
}

// A reply is an answer only if its closing does not hand off to work that
// never happened. Three detectors read the reply's text: one for a closing
// that defers work to a step that never comes, one for a plain statement that
// the work could not be done, and one for a question only the user can answer.
// Each must leave substantive answers alone, including answers that offer
// follow-up work.
func TestReplyDetectorsSeparateUnfinishedRepliesFromAnswers(t *testing.T) {
	for _, tc := range []struct {
		name        string
		detect      func(string) bool
		detected    []string
		notDetected []string
	}{
		{
			name:   "defers work",
			detect: replyDefersWork,
			detected: []string{
				// The first two and the seventh are real closings, word for
				// word, of runs that were reported completed.
				"I have analyzed the code in `planning.py` and `scoring.py`.   The issue is in `planning.py` within the `_score_plan` function.   I will now check the `generate_plan` function in `planning.py` to see the sorting key.",
				"I have analyzed the code in `planning.py`. The issue is located in the `generate_plan` function (which I will now read to confirm the exact line). Based on the logic described, the tie-breaking comparison is likely using a 'greater than' instead of a 'less than' for the number of steps. Please wait while I verify the exact line.",
				"The cache keys look inconsistent. Let me verify this by reading orders.py.",
				"Next, I will locate where the cache key is built.",
				"I found the loop in scoring.py. Now I need to check how planning.py consumes it.",
				"That narrows it to two functions. I'll run the tests to confirm which one.",
				"Since the search returned the file, I will try to read it using the exact string provided by the search tool, but I suspect the filesystem might be behaving unexpectedly.",
				"The key functions disagree somewhere. Let me attempt to verify that in keys.py.",
				"I'll go ahead and check the call site in pricing.py next.",
				// A closing that waits on a check still to come.
				"The two key functions disagree. I'll give you the exact path once I verify it.",
				"The cache misses on every read. I can name the line after I check keys.py.",
				"Both writes use the tier field; I am reading pricing.py to confirm the exact key.",
			},
			notDetected: []string{
				"The bug is on line 536 of planning.py: `n_steps > best_steps` should be `n_steps < best_steps`.",
				"Line 536 of planning.py inverts the tie-break. If you'd like, I can fix it.",
				"read_key in keys.py builds the key from the tier while write_key uses the id, so every lookup misses. Let me know if you want me to look at the other modules.",
				"I'll explain the flow first. cache.py stores entries under keys from keys.py; pricing.py reads with read_key and writes with write_key, which disagree, so nothing is ever found.",
				"I checked planning.py and scoring.py. The comparison on line 536 is inverted: ties go to the plan with more steps.",
				"I read the file and traced the call from orders.py into pricing.py. The miss happens because the two key functions use different customer fields.",
				"I couldn't run the tests here, but the fix is to change `>` to `>=` on line 6 of filters.py.",
				"Would you like me to check the other modules as well? The cause in keys.py is that read_key and write_key use different fields.",
				"apply_discount returns 90.0 because `/` always returns a float. If you'd like, I will try to add a test for it.",
				"It is not a bug: `/` always returns a float in Python 3, so try to use `//` if you need an integer.",
			},
		},
		{
			name:   "declares inability",
			detect: replyDeclaresInability,
			detected: []string{
				"I could not determine the cause from these files.",
				"I read all three modules. I wasn't able to find where the cache key is built.",
				"I'm unable to identify which comparison is wrong without the missing module.",
			},
			notDetected: []string{
				"The function is unable to handle an empty list, which is the bug.",
				"I couldn't run the tests here, but the fix is to change `>` to `>=` on line 6.",
				"The cause is in keys.py: read_key uses the tier and write_key uses the id.",
			},
		},
		{
			name:   "asks the user",
			detect: replyAsksUser,
			detected: []string{
				handoffReply,
				"Which web framework should the site use?",
				"I read app.py. Please share the error message you see when the form is submitted.",
				"Before I start: are you deploying this on Linux or Windows?",
				"Let me know which database you want the records stored in.",
			},
			notDetected: []string{
				plansAnswer,
				plansAnswer + " If you'd like, I can fix it.",
				"It decides between two candidates that score equally, usually by a secondary key such as length. Let me know if you want an example.",
				"Would you like me to check the other modules as well? The cause in keys.py is that read_key and write_key use different fields.",
				"Which approach is safer? The second, because it never overwrites the file.",
				"read_key uses the tier and write_key uses the id, so lookups miss. Does that help?",
				"The cause is in keys.py. Would you like me to fix it?",
				"The cache is keyed per customer tier. Any other questions?",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, reply := range tc.detected {
				if !tc.detect(reply) {
					t.Errorf("not detected:\n  %q", reply)
				}
			}
			for _, reply := range tc.notDetected {
				if tc.detect(reply) {
					t.Errorf("an answer was detected:\n  %q", reply)
				}
			}
		})
	}
}

// --- through the real loop -------------------------------------------------

// plans.py: on a tie in score, pick() keeps the plan with more steps.
var plansWorkspace = map[string]string{
	"plans.py": "def pick(plans):\n    best = None\n    for p in plans:\n        if best is None or p.steps > best.steps:\n            best = p\n    return best\n",
}

// The read key comes from the tier and the write key from the id, so the
// cache never hits.
var cacheWorkspace = map[string]string{
	"cache.py": "_store = {}\ndef get(k):\n    return _store.get(k)\ndef put(k, v):\n    _store[k] = v\n",
	"keys.py":  "def read_key(c):\n    return c['tier']\ndef write_key(c):\n    return c['id']\n",
}

// Both requests forbid changes, so production classifies them read-only.
const plansQuestion = "Tell me which comparison in plans.py decides ties between plans — do not change any code."
const cacheQuestion = "walk me through how the pricing cache works across these files and tell me why we might be seeing a lower hit rate than expected. dont change anything yet"

const deferringReply = "plans.py decides ties in pick(). I will now check the comparison on line 4. Please wait while I verify the exact line."
const plansAnswer = "In plans.py, line 4 of pick() compares `p.steps > best.steps`, so a tie on score keeps the plan with MORE steps; it should be `<`."
const cacheAnswer = "keys.py builds the read key from the customer's tier but the write key from the id, so a lookup never finds what was stored, and the hit rate stays near zero."

// A real reply, word for word, that called no tool while five source files
// sat in the workspace. It was reported completed.
const handoffReply = "To walk you through the pricing cache logic and diagnose the hit rate issue, I first need to locate the relevant files. Could you tell me which files handle the pricing cache, or should I start by searching for 'cache' or 'pricing' in the codebase?"

func readFileReply(path string) map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "read_file",
		"args": map[string]interface{}{"path": path}}
}

func textReply(s string) map[string]interface{} {
	return map[string]interface{}{"type": "text", "content": s}
}

// Each kind of final reply ends the run with its own status and reason. A
// reply that defers work, or hands an avoidable question back to the user, is
// sent back to the model ("gate" events) a bounded number of times, and never
// when too little time is left to act on it.
func TestEachReplyKindEndsTheRunWithItsOwnStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace map[string]string
		request   string
		// The model's reply on turn i is replies[i]; the last one repeats.
		replies []map[string]interface{}
		// Less time is left than one continuation needs.
		shortBudget bool
		wantStatus  TerminalStatus
		wantReason  string
		wantGates   int
		wantTurns   int
	}{
		{
			name:      "deferral after a tool is sent back, then answered",
			workspace: plansWorkspace, request: plansQuestion,
			replies: []map[string]interface{}{
				readFileReply("plans.py"), textReply(deferringReply), readFileReply("plans.py"), textReply(plansAnswer),
			},
			wantStatus: TerminalCompleted, wantReason: "text_reply", wantGates: 1, wantTurns: 4,
		},
		{
			name:      "deferral that persists ends incomplete when the bounces are spent",
			workspace: plansWorkspace, request: plansQuestion,
			replies:    []map[string]interface{}{readFileReply("plans.py"), textReply(deferringReply)},
			wantStatus: TerminalIncomplete, wantReason: "reply_left_work_outstanding",
			wantGates: maxGateBounces, wantTurns: 1 + maxGateBounces + 1,
		},
		{
			name:      "deferral with no time left ends incomplete at once",
			workspace: plansWorkspace, request: plansQuestion,
			replies:     []map[string]interface{}{readFileReply("plans.py"), textReply(deferringReply)},
			shortBudget: true,
			wantStatus:  TerminalIncomplete, wantReason: "reply_left_work_outstanding", wantGates: 0, wantTurns: 2,
		},
		{
			name:      "answer with an offer after a tool completes",
			workspace: plansWorkspace, request: plansQuestion,
			replies: []map[string]interface{}{
				readFileReply("plans.py"), textReply(plansAnswer + " If you'd like, I can fix it."),
			},
			wantStatus: TerminalCompleted, wantReason: "text_reply", wantGates: 0, wantTurns: 2,
		},
		{
			name:      "direct answer needs no tool",
			workspace: nil, request: "What does a tie-break comparison do?",
			replies: []map[string]interface{}{
				textReply("It decides between two candidates that score equally, usually by a secondary key such as length. Let me know if you want an example."),
			},
			wantStatus: TerminalCompleted, wantReason: "text_reply", wantGates: 0, wantTurns: 1,
		},
		{
			name:      "honest inability ends incomplete without a send-back",
			workspace: plansWorkspace, request: plansQuestion,
			replies: []map[string]interface{}{
				readFileReply("plans.py"),
				textReply("I read plans.py. I could not determine which comparison decides ties from this file alone."),
			},
			wantStatus: TerminalIncomplete, wantReason: "reply_declared_incomplete", wantGates: 0, wantTurns: 2,
		},
		{
			name:      "handoff with unopened files is sent back once, then answered",
			workspace: cacheWorkspace, request: cacheQuestion,
			replies: []map[string]interface{}{
				textReply(handoffReply), readFileReply("keys.py"), textReply(cacheAnswer),
			},
			wantStatus: TerminalCompleted, wantReason: "text_reply", wantGates: 1, wantTurns: 3,
		},
		{
			name:      "handoff that persists ends incomplete after one send-back",
			workspace: cacheWorkspace, request: cacheQuestion,
			replies:    []map[string]interface{}{textReply(handoffReply)},
			wantStatus: TerminalIncomplete, wantReason: "investigation_handed_back", wantGates: 1, wantTurns: 2,
		},
		{
			name:      "handoff with no time left ends incomplete at once",
			workspace: cacheWorkspace, request: cacheQuestion,
			replies:     []map[string]interface{}{textReply(handoffReply)},
			shortBudget: true,
			wantStatus:  TerminalIncomplete, wantReason: "investigation_handed_back", wantGates: 0, wantTurns: 1,
		},
		{
			// Nothing to inspect, so a question is the right reply.
			name:      "clarification in an empty workspace awaits the user",
			workspace: nil, request: "help me choose a database for my club's membership app",
			replies: []map[string]interface{}{
				textReply("That depends on scale and hosting. How many members do you expect, and will it run on a hosted service or your own machine?"),
			},
			wantStatus: TerminalIncomplete, wantReason: "clarification_requested", wantGates: 0, wantTurns: 1,
		},
		{
			// Asked after looking: legitimate as far as the run can tell.
			name:      "clarification after reading awaits the user",
			workspace: cacheWorkspace, request: cacheQuestion,
			replies: []map[string]interface{}{
				readFileReply("keys.py"),
				textReply("I read keys.py. Which customer field is supposed to identify a cached price, the tier or the id?"),
			},
			wantStatus: TerminalIncomplete, wantReason: "clarification_requested", wantGates: 0, wantTurns: 2,
		},
		{
			name:      "answer with an offer in the cache workspace completes",
			workspace: cacheWorkspace, request: cacheQuestion,
			replies: []map[string]interface{}{
				readFileReply("keys.py"), textReply(cacheAnswer + " Would you like me to fix it?"),
			},
			wantStatus: TerminalCompleted, wantReason: "text_reply", wantGates: 0, wantTurns: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, src := range tc.workspace {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ctx, turns, census, terminal := termFixture(t, dir, tc.request, termCeiling,
				func(i int, _ string) map[string]interface{} {
					return tc.replies[min(i, len(tc.replies)-1)]
				})
			if tc.shortBudget {
				work, cancel := context.WithTimeout(context.Background(), replyContinuationFloor/2)
				defer cancel()
				ctx.Ctx = work
			}

			if err := runAgentLoop(ctx, tc.request); err != nil {
				t.Fatal(err)
			}

			if terminal["status"] != string(tc.wantStatus) || terminal["reason"] != tc.wantReason {
				t.Errorf("want %s/%s, got %s/%s", tc.wantStatus, tc.wantReason, terminal["status"], terminal["reason"])
			}
			if census["gate"] != tc.wantGates || *turns != tc.wantTurns {
				t.Errorf("want %d gate events in %d turns, got %d in %d", tc.wantGates, tc.wantTurns, census["gate"], *turns)
			}
		})
	}
}

// A closing can be several of these at once, and the reasons have an order: each
// stands until its own flag clears.
func TestTheReplyReasonsHaveAnOrder(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier1Simple)
	st := &runState{replyOutstanding: true, replyDeclaredIncomplete: true, replyHandedBack: true,
		replyScopeUnmet: true, replyAwaitsUser: true}
	for _, step := range []struct {
		want  string
		clear *bool
	}{
		{"reply_left_work_outstanding", &st.replyOutstanding},
		{"reply_declared_incomplete", &st.replyDeclaredIncomplete},
		{"investigation_handed_back", &st.replyHandedBack},
		{"investigation_scope_unmet", &st.replyScopeUnmet},
		{"clarification_requested", &st.replyAwaitsUser},
	} {
		if _, reason := finalizeCompletion(ctx, st, "", "text_reply"); reason != step.want {
			t.Errorf("reason = %q, want %q", reason, step.want)
		}
		*step.clear = false
	}
}

// A clarification ends the run waiting for the user, and its summary says so.
func TestAClarificationSaysItIsWaitingForTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace map[string]string
		request   string
		// The model's reply on turn i is replies[i]; the last one repeats.
		replies []map[string]interface{}
	}{
		{
			// Nothing to inspect, so a question is the right reply.
			name:      "clarification in an empty workspace awaits the user",
			workspace: nil, request: "help me choose a database for my club's membership app",
			replies: []map[string]interface{}{
				textReply("That depends on scale and hosting. How many members do you expect, and will it run on a hosted service or your own machine?"),
			},
		},
		{
			// Asked after looking: legitimate as far as the run can tell.
			name:      "clarification after reading awaits the user",
			workspace: cacheWorkspace, request: cacheQuestion,
			replies: []map[string]interface{}{
				readFileReply("keys.py"),
				textReply("I read keys.py. Which customer field is supposed to identify a cached price, the tier or the id?"),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, src := range tc.workspace {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ctx, _, _, terminal := termFixture(t, dir, tc.request, termCeiling,
				func(i int, _ string) map[string]interface{} {
					return tc.replies[min(i, len(tc.replies)-1)]
				})

			if err := runAgentLoop(ctx, tc.request); err != nil {
				t.Fatal(err)
			}

			const waiting = "Waiting for your answer"
			if !strings.Contains(terminal["summary"], waiting) {
				t.Errorf("the summary does not say %q: %q", waiting, terminal["summary"])
			}
		})
	}
}
