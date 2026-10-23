package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
		// A phrase the summary must hold; empty checks nothing.
		wantSummaryPhrase string
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
			wantSummaryPhrase: "Waiting for your answer",
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
			wantSummaryPhrase: "Waiting for your answer",
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
			if !strings.Contains(terminal["summary"], tc.wantSummaryPhrase) {
				t.Errorf("the summary does not say %q: %q", tc.wantSummaryPhrase, terminal["summary"])
			}
		})
	}
}
