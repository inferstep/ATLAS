package main

import (
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
	src, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, "writeFileDirect(") {
			continue
		}
		// Its definition and the one call inside the recorder are the only
		// legitimate mentions.
		if strings.Contains(line, "func writeFileDirect") ||
			strings.Contains(line, "res, err := writeFileDirect(path, content)") {
			continue
		}
		t.Errorf("tools.go:%d calls writeFileDirect directly, so a write there "+
			"never records the body and the evidence gate will fire on a file "+
			"the model just wrote: %s", i+1, strings.TrimSpace(line))
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
