// How much work a request is: the tier a message earns, and whether it asks a question.

package main

import (
	"strings"
)

// classifyAgentTier decides whether a request is conversational.
//
// The message tier has exactly two behaviours, despite the four-value Tier
// type. TierMaxTurns caps T0 at 12 turns and leaves T1/T2/T3 uncapped
// alike; shouldGeneratePlan tests only Tier0Conversational; and the tier
// travels to v3-service where it is read into a log line and never branched
// on. V3 activation is driven by classifyFileTier, which scores the file
// being edited and does use T1/T2/T3. So the only question here is
// conversational or not, and the non-T0 value is Tier2Medium because that
// is what every consumer treats every non-T0 value as.
//
// The costs are asymmetric, which sets the direction of the default.
// Misreading chat as a task wastes one planner call on a message the model
// answers and closes in a single turn. Misreading a task as chat caps it at
// 12 turns and skips planning, and a capped task fails.
//
// So T0 requires positive evidence, and the absence of a recognized task
// word is not evidence: "the snake is still moving way too fast, please
// slow it down" names no task word and is still a task. Describing desired
// software behaviour is open vocabulary with no closed list to match
// against, while greetings are short and questions are a closed grammatical
// class. Both of those are things a message can be shown to BE, rather than
// shown not to be.
func classifyAgentTier(message string) Tier {
	trimmed := strings.TrimSpace(message)

	// Task language wins outright, at any length and in any shape. "can
	// you fix the login bug?" is a question and a task; the task reading
	// is the one whose failure mode is expensive.
	if isActionIntentMessage(trimmed) || isFixIntentMessage(trimmed) {
		return Tier2Medium
	}

	// An explicit "explain this, do not edit anything" is conversational by
	// definition, whether or not it is phrased as a question. Without this,
	// "Explain how the retry logic works, without editing anything." carries
	// no question mark and no question-word opener, so it fell through to a
	// work tier and got the write pipeline.
	if isExplainOnlyMessage(strings.ToLower(trimmed)) {
		return Tier0Conversational
	}

	// Greeting or acknowledgement. The floor matches shouldGeneratePlan's
	// own, so the two agree on what is too short to plan for.
	if len(trimmed) < 12 {
		return Tier0Conversational
	}

	if isQuestionMessage(trimmed) {
		return Tier0Conversational
	}

	return Tier2Medium
}

// declaredTier is the tier for a request whose client declared its mode.
//
// A client that declared work gets a work tier. The message classifier is the
// fallback for callers that declare nothing: read as a question, a verb-less
// work request ("Whenever a user submits the form, store the entry in
// entries.json") was capped at the conversational turn limit and never
// planned. A declared question keeps the classifier's tier: some questions
// need the project read at length, and the tier only scales effort.
func declaredTier(tc *TaskContract, classified Tier) Tier {
	if tc != nil && tc.TaskMode == TaskModeWork && classified == Tier0Conversational {
		return Tier2Medium
	}
	return classified
}

// questionStarters is the set of words an English interrogative can open
// with. Unlike task vocabulary, which is unbounded, this is a closed
// grammatical class. The wh-words are matched as whole words: as prefixes,
// "Whole-number inputs...", "Whenever a user submits...", "Whatever port is
// free..." and "However you structure it..." all read as questions, and a
// work request classified as a question is capped and never planned.
var questionStarters = []string{
	"why", "what", "when", "where", "who", "whom", "which", "how",
	"is ", "are ", "does ", "do ", "did ", "can ", "could ",
	"would ", "should ", "will ", "won't", "isn't", "aren't",
}

// isQuestionMessage reports whether a message is shaped as a question: a
// question mark that closes a clause, or an interrogative opener for
// questions written without one.
func isQuestionMessage(message string) bool {
	trimmed := strings.TrimSpace(message)
	// A question mark ANYWHERE a clause can end, not only at the end. People
	// ask and then qualify — "what does find_duplicates do, and what is its
	// complexity? Just explain." ends in a period, so a suffix-only check
	// read it as not-a-question and it was handed the full write pipeline.
	// classifyAgentTier checks action and fix intent first, so "fix the bug
	// in foo.py? or bar.py?" still classifies as work.
	if hasClauseQuestionMark(trimmed) {
		return true
	}
	lower := strings.ToLower(trimmed)
	return opensWithQuestion(lower) || hasMidMessageQuestion(lower)
}

// hasMidMessageQuestion reports a wh-word or fronted auxiliary opening a
// MID-message clause: "In orders.py, what does X do" is a question with no "?".
func hasMidMessageQuestion(lower string) bool {
	for _, opener := range []string{", ", ". "} {
		for _, w := range questionStarters {
			idx := strings.Index(lower, opener+w)
			if idx >= 0 && midClauseAsks(w, lower[idx+len(opener)+len(w):]) {
				return true
			}
		}
	}
	return false
}

// midClauseAsks reads the words after a question starter in mid-message.
//
// For a wh-word this counts ONLY when the clause is INVERTED -- the verb
// follows the wh-word ("what DOES x do", "where IS it"). A relative clause
// keeps subject-verb order ("what it is", "where it was found") and is
// descriptive, not a question. Requiring inversion stops a field list like
// "post an item (what it is, where it was found)" from being read as a question
// and misrouted to the 12-turn conversational tier, which caps the run and
// skips planning. A fronted auxiliary opener (", is the sandbox read-only") is
// already inverted.
//
// A fronted auxiliary mid-message is a question only when a subject follows it
// (". Can you", ", is the sandbox"). Without that, the imperative ". Do not
// hardcode the answer." and declaratives such as ". Will be ..." read as
// questions.
func midClauseAsks(starter, after string) bool {
	if !whClauseWords[starter] {
		return startsWithSubject(after)
	}
	// wordEnds: "whenever" and "however" are not the wh-word.
	return wordEnds(after) && startsWithInterrogativeVerb(after)
}

// opensWithQuestion reports whether a message opens with an interrogative.
//
// A wh-word counts only as a whole word, never as the start of a longer one
// ("whole", "whenever", "however"). "when" and "where" also open ordinary
// subordinate clauses in requests ("When the timer hits zero the page should
// flash red"), so they count only when inverted ("when does", "where is").
// A fronted auxiliary is a question already ("is the sandbox read-only").
func opensWithQuestion(lower string) bool {
	for _, w := range questionStarters {
		if strings.HasPrefix(lower, w) && openingStarterAsks(w, lower[len(w):]) {
			return true
		}
	}
	return false
}

// openingStarterAsks reads the words after the question starter a message opens with.
func openingStarterAsks(starter, after string) bool {
	if starter == "do " {
		// "Do the same thing for the /users route" is an imperative. As a
		// question opener, "do" takes a pronoun ("do you", "do we").
		return startsWithPronoun(after)
	}
	if !whClauseWords[starter] {
		return true
	}
	if !wordEnds(after) {
		return false
	}
	if starter == "when" || starter == "where" {
		return strings.HasPrefix(after, "'s ") || startsWithInterrogativeVerb(after) || startsWithPastAuxiliary(after)
	}
	return true
}

// wordEnds reports whether the text right after a matched word leaves that
// word whole: the message ends, or the next character is not a letter.
func wordEnds(after string) bool {
	if after == "" {
		return true
	}
	c := after[0]
	return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z')
}

// startsWithPastAuxiliary covers the past-tense inversions an OPENING
// wh-word may take ("when did", "where was"). They stay out of
// interrogativeVerbs, where a mid-message field list "(what was lost, where
// were they found)" would read as inverted.
func startsWithPastAuxiliary(s string) bool {
	s = strings.TrimLeft(s, " ")
	for _, v := range []string{"did ", "was ", "were ", "had "} {
		if strings.HasPrefix(s, v) {
			return true
		}
	}
	return false
}

// startsWithSubject reports whether the words after a fronted auxiliary
// begin with a subject: a pronoun or a determiner. "Do not", "will be" and
// "is fine" begin with none, and are not questions.
func startsWithSubject(s string) bool {
	if startsWithPronoun(s) {
		return true
	}
	s = strings.TrimLeft(s, " ")
	for _, w := range []string{
		"this ", "that ", "these ", "those ", "the ", "a ", "an ",
		"my ", "your ", "our ", "their ", "its ",
	} {
		if strings.HasPrefix(s, w) {
			return true
		}
	}
	return false
}

// startsWithPronoun reports whether the text begins with a subject pronoun.
func startsWithPronoun(s string) bool {
	s = strings.TrimLeft(s, " ")
	for _, w := range []string{"you ", "i ", "we ", "it ", "they ", "he ", "she ", "there "} {
		if strings.HasPrefix(s, w) {
			return true
		}
	}
	return false
}

// hasClauseQuestionMark reports whether a "?" ends a clause: it is followed
// by the end of the message, whitespace or a closing quote, and it sits
// outside brackets and backticks. A "?" in a URL query, a regex, an optional
// marker "(optional?)" or inline code asks nothing.
func hasClauseQuestionMark(s string) bool {
	depth, code := 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '`':
			code = !code
		case '(', '[', '{':
			if !code {
				depth++
			}
		case ')', ']', '}':
			if !code && depth > 0 {
				depth--
			}
		case '?':
			if !code && depth == 0 && endsAClause(s[i+1:]) {
				return true
			}
		}
	}
	return false
}

// endsAClause reports whether the text right after a "?" lets it end a clause.
func endsAClause(after string) bool {
	return after == "" || strings.IndexByte(" \t\n\r\"'", after[0]) >= 0
}

// whClauseWords are the interrogative words that ALSO open ordinary relative
// clauses, so a mid-message occurrence is a question only when inverted.
var whClauseWords = map[string]bool{
	"why": true, "what": true, "when": true,
	"where": true, "who": true, "whom": true, "which": true, "how": true,
}

// interrogativeVerbs are the auxiliaries/copulas that immediately follow the
// wh-word in an inverted question ("what DOES x do", "where IS it"). The
// PAST-tense auxiliaries (was/were/did/had) are deliberately absent: a field
// list like "(what was lost, where were they found)" reads as inverted with
// them and would be misrouted to the capped conversational tier, and the
// classifier's asymmetry makes that the expensive error (a task capped and
// unplanned fails) while the cost of missing a rare past-tense mid-clause
// question with no "?" is one wasted planner call. Present-tense and modal
// questions -- the common forms -- are unaffected.
var interrogativeVerbs = []string{
	"is ", "are ", "does ", "do ", "can ", "could ", "will ",
	"would ", "should ", "shall ", "has ", "have ", "am ",
	"may ", "might ", "'s ",
}

// startsWithInterrogativeVerb reports whether the text right after a wh-word
// begins with an auxiliary/copula -- the mark of question inversion.
func startsWithInterrogativeVerb(s string) bool {
	s = strings.TrimLeft(s, " ")
	for _, v := range interrogativeVerbs {
		if strings.HasPrefix(s, v) {
			return true
		}
	}
	return false
}
