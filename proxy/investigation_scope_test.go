package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An investigation answers for what it opened.
//
// Measured (stabilization cycle 3, family S): asked to walk through how the
// pricing cache works across five modules and why the hit rate was low, the run
// read three of them, answered with one function pair in one file, and was
// finalized completed / text_reply. Nothing was written, nothing was deferred,
// no question was asked and no unread file was cited, so every existing gate
// passed.
//
// The rule added: for a READ-ONLY run whose request names several files, an
// answer that accounts for fewer than two of the files the run actually opened
// goes back once, and if it does not improve, the run says so. The evidence is
// the user's own words and the run's own reads — no plan, no task name, no
// judgement of whether the answer is right.

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
	if !run.told("The request asks about how several files behave together") {
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

// The same run, answering across the files it read, completes on the first try.
func TestAnAnswerThatSpansTheFilesItReadCompletes(t *testing.T) {
	full := spanText("`pricing.py` looks the price up in `cache.py` before computing it, and builds both keys " +
		"through `keys.py`. The write path stores under write_key (the customer id) and the read path looks up " +
		"read_key (the customer tier), so the two never agree and cache.py almost always misses.")
	run := integrityLoop(t, spanRequest, spanWorld(t), append(spanReadThree(), full), nil)
	if run.told("The request asks about how several files behave together") {
		t.Error("a cross-file answer was sent back anyway")
	}
	if run.terminal["status"] != "completed" || run.terminal["reason"] != "text_reply" {
		t.Errorf("terminal %s / %s, want completed / text_reply", run.terminal["status"], run.terminal["reason"])
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
	if run.told("The request asks about how several files behave together") {
		t.Error("the scope gate fired on a run that changed files")
	}
	want := "def write_key(sku, customer):\n    return f\"{sku}:{customer.id}\"\n\n\ndef read_key(sku, customer):\n    return f\"{sku}:{customer.id}\"\n"
	if got, _ := run.disk(t, "keys.py"); got != want {
		t.Errorf("the edit did not land: %q", got)
	}
}

// Nothing here reaches the model: the bounce names the files the run opened and
// asks for coverage, and says nothing about what is in them.
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

// The captured answer from the exposed run (stabilization3/E/sessions/02-S),
// byte for byte, against the workspace it was given: the rule must catch the
// real reply, not only a paraphrase of it.
func TestTheCapturedSAnswerIsCaughtByTheRule(t *testing.T) {
	answer, err := os.ReadFile("testdata/s_answer.txt")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// The frozen v3.1 fixture, byte for byte, so the symbols are the real ones.
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
	// What that run actually opened: three of the five modules.
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

// --- audit of the guard's limits (stabilization cycle 5) --------------------

// What the guard cannot do: it counts files the answer accounts for, not
// whether the answer is right. An answer that names every file the run read
// and states no relationship between them passes it. Recorded, not fixed by
// counting more: correctness is not decidable from the workspace.
func TestNamingTheFilesWithoutTheRelationshipStillPasses(t *testing.T) {
	padded := spanText("I looked at cache.py, keys.py and pricing.py. They are all part of the pricing path. " +
		"Something in there is causing the misses.")
	run := integrityLoop(t, spanRequest, spanWorld(t), append(spanReadThree(), padded), nil)
	if run.told("The request asks about how several files behave together") {
		t.Error("precondition changed: this answer is no longer passed by the guard")
	}
	if run.terminal["status"] != "completed" {
		t.Errorf("terminal %s / %s", run.terminal["status"], run.terminal["reason"])
	}
}

// The direction that matters for delivery: an answer that explains the
// relationship through the symbols it read, naming one file, must not be sent
// back as a partial investigation.
func TestAnAnswerThatExplainsViaSymbolsIsNotBounced(t *testing.T) {
	bySymbols := spanText("The price lookup calls read_key to build the cache key, but the value was stored " +
		"under write_key — read_key uses the customer tier where write_key used the id, so get() never finds " +
		"what put() wrote, and every lookup in keys.py misses.")
	run := integrityLoop(t, spanRequest, spanWorld(t), append(spanReadThree(), bySymbols), nil)
	if run.told("The request asks about how several files behave together") {
		t.Error("an answer that explains the relationship through its symbols was bounced")
	}
	if run.terminal["status"] != "completed" {
		t.Errorf("terminal %s / %s, want completed", run.terminal["status"], run.terminal["reason"])
	}
}
