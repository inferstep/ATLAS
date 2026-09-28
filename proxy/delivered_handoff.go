package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
)

// What the model is told when the bytes that landed are not the bytes it sent.
//
// A write can succeed and install something else. The candidate pipeline is
// the case that matters: it generates, scores and delivers its own content to
// the model's target, byte-exactly, and the model's next turn continues from a
// context in which that never happened.
//
// Measured on the two exposed sessions that adopted a candidate
// (stabilization12/PAIRS2, 01-O-B-rep1 and 05-O-B-rep2, byte-identical
// submissions and byte-identical adoptions). After the delivery the model was
// sent, in this order:
//
//	tool result  {"success":true,"data":{"bytes_written":4465,"v3_used":true,
//	              "candidates_tested":1,"winning_score":0.06,"phase_solved":"phase1"}}
//	lens alert   "the geometric lens scored YOUR LAST WRITE at gx_min=0.271 ...
//	              STOP and try a different approach ... DO NOT re-issue the
//	              same write"
//	V3 nudge     "The fix is on disk and build-checked ... do not re-read the
//	              file to double-check"
//
// Three things are wrong with that, and none of them is the model:
//
//   - The only trace of the substitution is a byte count the model would have
//     to compare against a length it never computed. It was never shown the
//     delivered source and never told one word about what changed.
//   - The lens scored the SUBMITTED bytes, before execution, and those bytes
//     are no longer on disk. The alert describes them as "your last write" and
//     forbids re-issuing them, which is advice about a file that does not
//     exist.
//   - The nudge tells it not to re-read the file, which is the one action that
//     would have shown it what it actually has.
//
// Both sessions overwrote the delivered candidate on the very next turn, 5.8 s
// and 7.0 s later, and neither ever saw it. The candidate's bytes were not
// preserved by any component, so whether it was better cannot be established
// at all.
//
// This owner answers one question -- did this write install something other
// than what was submitted -- and does three things with the answer, all
// through existing mechanisms:
//
//  1. the delivered content becomes the session's read state for that path, so
//     LastRead, the restatement the sub-call already performs, and every
//     downstream reader see what is on disk rather than what was proposed;
//  2. the model is told plainly, once, that the file differs and that the copy
//     it is about to be shown is the authority;
//  3. a lens corrective about the superseded bytes is dropped, because it is
//     not about anything that exists.
//
// It does not lock the file, does not prefer the candidate, does not re-score
// anything and does not introduce a judge. A later edit is as legitimate as it
// ever was -- it just operates against the version that is really there.

// supersededWrite is what a completed write turned out to be.
type supersededWrite struct {
	// Path is the resolved target, as the filesystem knows it.
	Path string
	// Rel is the model's own spelling, for anything shown to it.
	Rel string
	// Submitted is what the model sent.
	Submitted string
	// Delivered is what is on disk now.
	Delivered string
}

// deliveredDiffersFromSubmitted reports whether a successful write installed
// content other than the caller's, reading the filesystem for the answer.
//
// write_file sends a whole file, which compares with disk directly. The four
// edit tools send a fragment, so their whole-file proposal comes from the
// delivery that replaced it (ToolResult.Substituted). Without that, an edit a
// V3 candidate replaced reached the model as its own: the note that the bytes
// differ was never sent, and the model went on editing a file it had not seen.
//
// A false answer is the overwhelmingly common case and costs one read of a
// file the write just touched.
func deliveredDiffersFromSubmitted(ctx *AgentContext, tool string,
	args json.RawMessage, result *ToolResult) (supersededWrite, bool) {
	if ctx == nil || result == nil || !result.Success {
		return supersededWrite{}, false
	}
	var submitted, rel, path string
	switch {
	case tool == "write_file":
		content, ok := extractScorableContent(tool, args)
		rel = extractToolTarget(tool, args)
		if !ok || content == "" || rel == "" {
			return supersededWrite{}, false
		}
		submitted, path = content, resolveAgentPath(ctx, rel)
	case result.Substituted != nil:
		submitted, rel, path = result.Substituted.Submitted, result.Substituted.Rel, result.Substituted.Path
	default:
		return supersededWrite{}, false
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		// Nothing to compare against. Saying nothing is right: a claim about
		// the delivered artifact has to be able to read it.
		return supersededWrite{}, false
	}
	delivered := string(onDisk)
	if contentSHA256(delivered) == contentSHA256(submitted) {
		return supersededWrite{}, false
	}
	return supersededWrite{Path: path, Rel: rel, Submitted: submitted, Delivered: delivered}, true
}

// supersededWriteNote is the sentence the model gets. Facts only: which file,
// that it differs, and what is authoritative now.
//
// It deliberately does not say the delivered version is better. Nothing here
// knows that, the score that selected it is not a correctness measure, and
// telling a model its own work was improved is exactly the claim this program
// refuses to make without evidence.
func supersededWriteNote(s supersededWrite) string {
	return fmt.Sprintf(
		"[system note]: %s was written, but the bytes on disk are NOT the ones you "+
			"sent — the candidate pipeline delivered its own version to that path "+
			"(%d lines, %d bytes; yours was %d lines, %d bytes). Nothing here says "+
			"which is better. What is on disk is what the user gets, so work from "+
			"it: read %s before you change it, and base any edit on its current "+
			"contents rather than on what you sent.",
		s.Rel,
		lineCountOf(s.Delivered), len(s.Delivered),
		lineCountOf(s.Submitted), len(s.Submitted),
		s.Rel)
}

func lineCountOf(s string) int {
	if s == "" {
		return 0
	}
	n := 1
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			n++
		}
	}
	if len(s) > 0 && s[len(s)-1] == '\n' {
		n--
	}
	return n
}

// adoptDeliveredContent makes the delivered artifact the session's current
// view of that path and returns the note to show the model.
//
// The read-state owner is the existing one (RecordFileRead), so this changes
// no rule about staleness, restatement or edit authorization -- it changes
// what they are about. RecordBodySeen is set for the same reason it is set on
// an ordinary write: the model is being shown these bytes.
func adoptDeliveredContent(ctx *AgentContext, s supersededWrite) string {
	ctx.RecordFileRead(s.Path, s.Delivered)
	ctx.RecordBodySeen(s.Path)
	log.Printf("[agent] %s on disk is not what was submitted (%dB delivered vs %dB sent) — "+
		"read state now tracks the delivered version",
		logPath(s.Path), len(s.Delivered), len(s.Submitted))
	return supersededWriteNote(s)
}
