package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A reply is an answer only if its closing does not hand off to work that
// never happened. Every bugfind_tiebreak session across four benchmark runs
// (8 of 8) ended on such a closing and was reported completed; two of those
// closings are used verbatim below.

func TestReplyClosingThatDefersWorkIsDetected(t *testing.T) {
	for _, reply := range []string{
		// verbatim, 84296fd bugfind_tiebreak rep1/rep2
		"I have analyzed the code in `planning.py` and `scoring.py`.   The issue is in `planning.py` within the `_score_plan` function.   I will now check the `generate_plan` function in `planning.py` to see the sorting key.",
		// verbatim, f9c89df bugfind_tiebreak rep1/rep2
		"I have analyzed the code in `planning.py`. The issue is located in the `generate_plan` function (which I will now read to confirm the exact line). Based on the logic described, the tie-breaking comparison is likely using a 'greater than' instead of a 'less than' for the number of steps. Please wait while I verify the exact line.",
		"The cache keys look inconsistent. Let me verify this by reading orders.py.",
		"Next, I will locate where the cache key is built.",
		"I found the loop in scoring.py. Now I need to check how planning.py consumes it.",
		"That narrows it to two functions. I'll run the tests to confirm which one.",
		// verbatim closing sentence, ce68470 #273 proof run 09, ask_bug: reported completed
		"Since the search returned the file, I will try to read it using the exact string provided by the search tool, but I suspect the filesystem might be behaving unexpectedly.",
		"The key functions disagree somewhere. Let me attempt to verify that in keys.py.",
		"I'll go ahead and check the call site in pricing.py next.",
	} {
		if !replyDefersWork(reply) {
			t.Errorf("a reply that closes by deferring work was not detected:\n  %q", reply)
		}
	}
}

func TestSubstantiveAnswersAreNotTreatedAsDeferrals(t *testing.T) {
	for _, reply := range []string{
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
	} {
		if replyDefersWork(reply) {
			t.Errorf("a substantive answer was treated as deferring work:\n  %q", reply)
		}
	}
}

func TestReplyDeclaringInabilityIsDetected(t *testing.T) {
	yes := []string{
		"I could not determine the cause from these files.",
		"I read all three modules. I wasn't able to find where the cache key is built.",
		"I'm unable to identify which comparison is wrong without the missing module.",
	}
	no := []string{
		"The function is unable to handle an empty list, which is the bug.",
		"I couldn't run the tests here, but the fix is to change `>` to `>=` on line 6.",
		"The cause is in keys.py: read_key uses the tier and write_key uses the id.",
	}
	for _, r := range yes {
		if !replyDeclaresInability(r) {
			t.Errorf("an explicit first-person inability was not detected: %q", r)
		}
	}
	for _, r := range no {
		if replyDeclaresInability(r) {
			t.Errorf("an answer was treated as declaring inability: %q", r)
		}
	}
}

// --- through the real loop -------------------------------------------------

// Same shape as the benchmark prompt ("Tell me which file and which comparison
// is wrong -- do not change any code"), which production classifies read-only.
const unfinishedQ = "Tell me which comparison in plans.py decides ties between plans — do not change any code."

func plansFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := "def pick(plans):\n    best = None\n    for p in plans:\n        if best is None or p.steps > best.steps:\n            best = p\n    return best\n"
	if err := os.WriteFile(filepath.Join(dir, "plans.py"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readPlans() map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "read_file",
		"args": map[string]interface{}{"path": "plans.py"}}
}

func textReply(s string) map[string]interface{} {
	return map[string]interface{}{"type": "text", "content": s}
}

const deferral = "plans.py decides ties in pick(). I will now check the comparison on line 4. Please wait while I verify the exact line."
const answer = "In plans.py, line 4 of pick() compares `p.steps > best.steps`, so a tie on score keeps the plan with MORE steps; it should be `<`."

// Unfinished promise after work -> sent back once -> acts -> answers -> completed.
func TestAReplyThatDefersWorkAfterAToolIsContinuedNotCompleted(t *testing.T) {
	dir := plansFixture(t)
	ctx, turns, census, terminal := termFixture(t, dir, unfinishedQ, termCeiling,
		func(i int, _ string) map[string]interface{} {
			switch i {
			case 0:
				return readPlans()
			case 1:
				return textReply(deferral)
			case 2:
				return readPlans()
			default:
				return textReply(answer)
			}
		})
	if err := runAgentLoop(ctx, unfinishedQ); err != nil {
		t.Fatal(err)
	}
	if census["gate"] < 1 {
		t.Errorf("the deferring reply was not sent back (gate events=%d)", census["gate"])
	}
	if terminal["status"] != string(TerminalCompleted) || terminal["reason"] != "text_reply" {
		t.Errorf("after acting and answering, want completed/text_reply, got %s/%s", terminal["status"], terminal["reason"])
	}
	if *turns != 4 {
		t.Errorf("want 4 model turns (read, defer, read, answer), got %d", *turns)
	}
}

// Keeps deferring -> bounces spent -> incomplete, never completed.
func TestAReplyThatKeepsDeferringEndsIncompleteWhenBouncesAreSpent(t *testing.T) {
	dir := plansFixture(t)
	ctx, turns, census, terminal := termFixture(t, dir, unfinishedQ, termCeiling,
		func(i int, _ string) map[string]interface{} {
			if i == 0 {
				return readPlans()
			}
			return textReply(deferral)
		})
	if err := runAgentLoop(ctx, unfinishedQ); err != nil {
		t.Fatal(err)
	}
	if terminal["status"] == string(TerminalCompleted) {
		t.Fatalf("a reply that never stopped deferring was reported completed: %v", terminal)
	}
	if terminal["reason"] != "reply_left_work_outstanding" {
		t.Errorf("want reason reply_left_work_outstanding, got %s/%s", terminal["status"], terminal["reason"])
	}
	if census["gate"] != maxGateBounces {
		t.Errorf("continuation must be bounded by maxGateBounces=%d, got %d gate events", maxGateBounces, census["gate"])
	}
	if *turns != 1+maxGateBounces+1 {
		t.Errorf("want %d turns (read + %d bounced + final), got %d", 2+maxGateBounces, maxGateBounces, *turns)
	}
}

// A substantive answer, even one offering follow-up, completes with no extra work.
func TestASubstantiveAnswerAfterWorkCompletesWithoutContinuation(t *testing.T) {
	dir := plansFixture(t)
	ctx, turns, census, terminal := termFixture(t, dir, unfinishedQ, termCeiling,
		func(i int, _ string) map[string]interface{} {
			if i == 0 {
				return readPlans()
			}
			return textReply(answer + " If you'd like, I can fix it.")
		})
	if err := runAgentLoop(ctx, unfinishedQ); err != nil {
		t.Fatal(err)
	}
	if terminal["status"] != string(TerminalCompleted) || terminal["reason"] != "text_reply" {
		t.Errorf("want completed/text_reply, got %s/%s", terminal["status"], terminal["reason"])
	}
	if census["gate"] != 0 || *turns != 2 {
		t.Errorf("a complete answer must not be sent back: gate events=%d turns=%d", census["gate"], *turns)
	}
}

// A question that can be answered directly needs no tool call.
func TestADirectAnswerNeedsNoToolUse(t *testing.T) {
	dir := t.TempDir()
	const q = "What does a tie-break comparison do?"
	ctx, turns, census, terminal := termFixture(t, dir, q, termCeiling,
		func(i int, _ string) map[string]interface{} {
			return textReply("It decides between two candidates that score equally, usually by a secondary key such as length. Let me know if you want an example.")
		})
	if err := runAgentLoop(ctx, q); err != nil {
		t.Fatal(err)
	}
	if terminal["status"] != string(TerminalCompleted) || census["gate"] != 0 || *turns != 1 {
		t.Errorf("a direct answer must complete in one turn with no gate: %s/%s gates=%d turns=%d",
			terminal["status"], terminal["reason"], census["gate"], *turns)
	}
}

// Honest inability ends incomplete at once -- not completed, not sent back.
func TestHonestInabilityEndsIncompleteWithoutBeingSentBack(t *testing.T) {
	dir := plansFixture(t)
	ctx, turns, census, terminal := termFixture(t, dir, unfinishedQ, termCeiling,
		func(i int, _ string) map[string]interface{} {
			if i == 0 {
				return readPlans()
			}
			return textReply("I read plans.py. I could not determine which comparison decides ties from this file alone.")
		})
	if err := runAgentLoop(ctx, unfinishedQ); err != nil {
		t.Fatal(err)
	}
	if terminal["status"] != string(TerminalIncomplete) || terminal["reason"] != "reply_declared_incomplete" {
		t.Errorf("want incomplete/reply_declared_incomplete, got %s/%s", terminal["status"], terminal["reason"])
	}
	if census["gate"] != 0 || *turns != 2 {
		t.Errorf("an honest inability must not be sent back: gate events=%d turns=%d", census["gate"], *turns)
	}
}

// Too little budget to act on a deferral: end incomplete now, spend no bounce.
func TestADeferringReplyWithNoBudgetLeftEndsIncompleteImmediately(t *testing.T) {
	dir := plansFixture(t)
	ctx, turns, census, terminal := termFixture(t, dir, unfinishedQ, termCeiling,
		func(i int, _ string) map[string]interface{} {
			if i == 0 {
				return readPlans()
			}
			return textReply(deferral)
		})
	work, cancel := context.WithTimeout(context.Background(), replyContinuationFloor/2)
	defer cancel()
	ctx.Ctx = work
	if err := runAgentLoop(ctx, unfinishedQ); err != nil {
		t.Fatal(err)
	}
	if terminal["status"] != string(TerminalIncomplete) || terminal["reason"] != "reply_left_work_outstanding" {
		t.Errorf("want incomplete/reply_left_work_outstanding, got %s/%s", terminal["status"], terminal["reason"])
	}
	if census["gate"] != 0 || *turns != 2 {
		t.Errorf("with %s left no continuation may start: gate events=%d turns=%d",
			replyContinuationFloor/2, census["gate"], *turns)
	}
	_ = time.Second
}

// --- clarification and handoff ----------------------------------------------

// Verbatim, c5927b3 acceptance family S: no tool call, five source files in
// the workspace, reported completed/text_reply.
const handoffS = "To walk you through the pricing cache logic and diagnose the hit rate issue, I first need to locate the relevant files. Could you tell me which files handle the pricing cache, or should I start by searching for 'cache' or 'pricing' in the codebase?"

func TestReplyAsksUserSeparatesRequestsFromOffers(t *testing.T) {
	asks := []string{
		handoffS,
		"Which web framework should the site use?",
		"I read app.py. Please share the error message you see when the form is submitted.",
		"Before I start: are you deploying this on Linux or Windows?",
		"Let me know which database you want the records stored in.",
	}
	answers := []string{
		answer,
		answer + " If you'd like, I can fix it.",
		"It decides between two candidates that score equally, usually by a secondary key such as length. Let me know if you want an example.",
		"Would you like me to check the other modules as well? The cause in keys.py is that read_key and write_key use different fields.",
		"Which approach is safer? The second, because it never overwrites the file.",
		"read_key uses the tier and write_key uses the id, so lookups miss. Does that help?",
		"The cause is in keys.py. Would you like me to fix it?",
		"The cache is keyed per customer tier. Any other questions?",
	}
	for _, r := range asks {
		if !replyAsksUser(r) {
			t.Errorf("a reply asking the user was not detected:\n  %q", r)
		}
	}
	for _, r := range answers {
		if replyAsksUser(r) {
			t.Errorf("an answer or an offer was treated as asking the user:\n  %q", r)
		}
	}
}

func cacheFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range map[string]string{
		"cache.py": "_store = {}\ndef get(k):\n    return _store.get(k)\ndef put(k, v):\n    _store[k] = v\n",
		"keys.py":  "def read_key(c):\n    return c['tier']\ndef write_key(c):\n    return c['id']\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const cacheQ = "walk me through how the pricing cache works across these files and tell me why we might be seeing a lower hit rate than expected. dont change anything yet"
const cacheAnswer = "keys.py builds the read key from the customer's tier but the write key from the id, so a lookup never finds what was stored, and the hit rate stays near zero."

func readKeys() map[string]interface{} {
	return map[string]interface{}{"type": "tool_call", "name": "read_file",
		"args": map[string]interface{}{"path": "keys.py"}}
}

// Avoidable handoff -> sent back once -> reads -> answers -> completed.
func TestAHandoffWithUnopenedFilesIsSentBackOnceThenAnswered(t *testing.T) {
	dir := cacheFixture(t)
	ctx, turns, census, terminal := termFixture(t, dir, cacheQ, termCeiling,
		func(i int, _ string) map[string]interface{} {
			switch i {
			case 0:
				return textReply(handoffS)
			case 1:
				return readKeys()
			default:
				return textReply(cacheAnswer)
			}
		})
	if err := runAgentLoop(ctx, cacheQ); err != nil {
		t.Fatal(err)
	}
	if census["gate"] != 1 {
		t.Errorf("the handoff must be sent back exactly once, got %d gate events", census["gate"])
	}
	if terminal["status"] != string(TerminalCompleted) || terminal["reason"] != "text_reply" {
		t.Errorf("after reading and answering, want completed/text_reply, got %s/%s", terminal["status"], terminal["reason"])
	}
	if *turns != 3 {
		t.Errorf("want 3 turns (handoff, read, answer), got %d", *turns)
	}
}

// Avoidable handoff that persists -> incomplete/investigation_handed_back, one send-back.
func TestAPersistingHandoffEndsIncompleteNotCompleted(t *testing.T) {
	dir := cacheFixture(t)
	ctx, turns, census, terminal := termFixture(t, dir, cacheQ, termCeiling,
		func(i int, _ string) map[string]interface{} { return textReply(handoffS) })
	if err := runAgentLoop(ctx, cacheQ); err != nil {
		t.Fatal(err)
	}
	if terminal["status"] != string(TerminalIncomplete) || terminal["reason"] != "investigation_handed_back" {
		t.Errorf("want incomplete/investigation_handed_back, got %s/%s", terminal["status"], terminal["reason"])
	}
	if census["gate"] != 1 || *turns != 2 {
		t.Errorf("one send-back, then end: gate events=%d turns=%d", census["gate"], *turns)
	}
}

// Too little budget to look: end incomplete at once, spend no send-back.
func TestAHandoffWithNoBudgetLeftEndsIncompleteImmediately(t *testing.T) {
	dir := cacheFixture(t)
	ctx, turns, census, terminal := termFixture(t, dir, cacheQ, termCeiling,
		func(i int, _ string) map[string]interface{} { return textReply(handoffS) })
	work, cancel := context.WithTimeout(context.Background(), replyContinuationFloor/2)
	defer cancel()
	ctx.Ctx = work
	if err := runAgentLoop(ctx, cacheQ); err != nil {
		t.Fatal(err)
	}
	if terminal["status"] != string(TerminalIncomplete) || terminal["reason"] != "investigation_handed_back" {
		t.Errorf("want incomplete/investigation_handed_back, got %s/%s", terminal["status"], terminal["reason"])
	}
	if census["gate"] != 0 || *turns != 1 {
		t.Errorf("no continuation may start: gate events=%d turns=%d", census["gate"], *turns)
	}
}

// Nothing to inspect: a question is the right reply. Not sent back, not completed.
// (A work request keeps the action gate's bounded push to act first; that
// path is unchanged here.)
func TestALegitimateClarificationInAnEmptyWorkspaceAwaitsTheUser(t *testing.T) {
	dir := t.TempDir()
	const q = "help me choose a database for my club's membership app"
	const ask = "That depends on scale and hosting. How many members do you expect, and will it run on a hosted service or your own machine?"
	ctx, turns, census, terminal := termFixture(t, dir, q, termCeiling,
		func(i int, _ string) map[string]interface{} { return textReply(ask) })
	if err := runAgentLoop(ctx, q); err != nil {
		t.Fatal(err)
	}
	if terminal["status"] != string(TerminalIncomplete) || terminal["reason"] != "clarification_requested" {
		t.Errorf("want incomplete/clarification_requested, got %s/%s", terminal["status"], terminal["reason"])
	}
	if census["gate"] != 0 || *turns != 1 {
		t.Errorf("a legitimate question must not be sent back: gate events=%d turns=%d", census["gate"], *turns)
	}
	if !strings.Contains(terminal["summary"], "Waiting for your answer") {
		t.Errorf("the summary does not say an answer is awaited: %q", terminal["summary"])
	}
}

// Asked after looking: legitimate as far as the run can tell.
func TestAClarificationAfterInspectionAwaitsTheUser(t *testing.T) {
	dir := cacheFixture(t)
	const ask = "I read keys.py. Which customer field is supposed to identify a cached price, the tier or the id?"
	ctx, turns, census, terminal := termFixture(t, dir, cacheQ, termCeiling,
		func(i int, _ string) map[string]interface{} {
			if i == 0 {
				return readKeys()
			}
			return textReply(ask)
		})
	if err := runAgentLoop(ctx, cacheQ); err != nil {
		t.Fatal(err)
	}
	if terminal["status"] != string(TerminalIncomplete) || terminal["reason"] != "clarification_requested" {
		t.Errorf("want incomplete/clarification_requested, got %s/%s", terminal["status"], terminal["reason"])
	}
	if census["gate"] != 0 || *turns != 2 {
		t.Errorf("gate events=%d turns=%d", census["gate"], *turns)
	}
}

// A substantive answer about the same workspace completes, whether or not it
// offers follow-up -- the handoff check must not touch it.
func TestAnAnswerWithAnOfferInTheCacheWorkspaceCompletes(t *testing.T) {
	dir := cacheFixture(t)
	ctx, turns, census, terminal := termFixture(t, dir, cacheQ, termCeiling,
		func(i int, _ string) map[string]interface{} {
			if i == 0 {
				return readKeys()
			}
			return textReply(cacheAnswer + " Would you like me to fix it?")
		})
	if err := runAgentLoop(ctx, cacheQ); err != nil {
		t.Fatal(err)
	}
	if terminal["status"] != string(TerminalCompleted) || census["gate"] != 0 || *turns != 2 {
		t.Errorf("want completed in 2 turns with no gate, got %s/%s gates=%d turns=%d",
			terminal["status"], terminal["reason"], census["gate"], *turns)
	}
}
