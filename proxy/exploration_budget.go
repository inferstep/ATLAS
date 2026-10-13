package main

import "log"

// Notes for a run of read-only calls. When the request demands a change, the
// run has read enough to act, so the note asks for the write. When it does not
// (a question, or "do not change any code"), reading is the work and a write
// would be refused, so the note asks for the answer: the write note told a
// bug-finding session to stop reading one function short.
const (
	explorationWarning   = "You have full project context in the system prompt. Do not read more files. Emit a write_file or edit_file tool call now."
	explorationEscalated = "You already have this information in context — reading more files will not help. Write your changes now. Use write_file or edit_file."
	explorationAnswer    = "You have made several read-only calls in a row. If you have what the question needs, answer it now in a single text reply. If one specific part is still missing, read only that part, then answer."
)

// explorationNudge returns the note for consecutiveReads read-only calls in a
// row, or "" below four. From five on it escalates and drops the count to two,
// so the notes continue at the warning level. writeDemanded is false for a
// request whose deliverable is an answer (answerOnlyRequest).
func explorationNudge(consecutiveReads *int, turn int, writeDemanded bool) string {
	if *consecutiveReads < 4 {
		return ""
	}
	escalated := *consecutiveReads >= 5
	if escalated {
		*consecutiveReads = 2
		log.Printf("[agent] exploration budget: escalated nudge at turn %d", turn)
	} else {
		log.Printf("[agent] exploration budget: warning at turn %d", turn)
	}
	switch {
	case !writeDemanded:
		return explorationAnswer
	case escalated:
		return explorationEscalated
	}
	return explorationWarning
}
