// Completion-claim verification — bounces a `done` whose summary claims
// universal success ("all routes work") while the workspace still shows a
// structural gap. Needs both halves, so a narrow summary or a clean
// workspace passes untouched.

package main

import (
	"fmt"
	"strings"
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
