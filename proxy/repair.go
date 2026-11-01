package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// Repair in progress.
//
// A file this session left unparseable is an open repair until a later change
// leaves it parsing, or it is deleted or moved. While one is open:
//   - every tool result names it, with its parse error and the lines around
//     the error (open_repair), so the work goes on with the error in view;
//   - the session is not finished: a done is sent back, bounded like the other
//     exit gates (repair_gate);
//   - writes to other files stay allowed.
//
// A session that ends with one still open is never reported completed. Its
// final message hands the file to the user: each attempt and the error it
// left, the error that remains, why the session ended, and a request to take
// a look (repairHandoff). It says what was tried, never why the fix was not
// found. The `repair` events record the same steps, so a later analysis can
// tell a harness gap from a real limit.
//
// A repair opens only on a tool's own verdict: the edit tools refuse to break
// a file that parsed, so an edit leaves a file unparseable only when it
// already was, and write_file lands a new unparseable file with a warning. A
// file a command breaks is not tracked here; the unresolved-work rules still
// report it.

// maxRepairAttemptsShown bounds the attempts the final message lists.
const maxRepairAttemptsShown = 8

// maxRepairsNamed bounds the files one message names.
const maxRepairsNamed = 3

// repairAttempt is one call that touched a file while its repair was open,
// including the call that opened it.
type repairAttempt struct {
	Turn   int
	Tool   string
	Landed bool   // the call changed the file
	Error  string // the parse error the call left, or why it did not land
}

type repairState struct {
	Rel          string // the path as the model named it when the repair opened
	Key          string // ledgerKey of Rel
	OpenedTurn   int
	OpenedBy     string
	BrokenBefore bool   // opened by an edit, so the file already failed before it
	Error        string // the parse error of the bytes on disk
	Hash         string // the hash of the bytes Error describes
	Attempts     []repairAttempt
	Open         bool
}

// repairEditTools edit a file in place. Each refuses an edit that breaks a
// file that parsed, so one of them leaves a file unparseable only if it
// already was.
var repairEditTools = map[string]bool{
	"edit_file": true, "structural_edit": true, "insert_after": true, "replace_lines": true,
}

// landedUnparseable reports whether a call landed bytes that fail the syntax
// check.
func landedUnparseable(r *ToolResult) bool {
	return r != nil && r.MutationStatus == MutationApplied &&
		r.ValidationKind == ValidationKindSyntax && r.ValidationStatus == ValidationFailed
}

// observeRepair updates the repairs after one tool call.
func (s *runState) observeRepair(ctx *AgentContext, turn int, name string, args json.RawMessage, result *ToolResult) {
	if ctx == nil || result == nil {
		return
	}
	if name == "run_command" || name == "run_background" {
		s.refreshRepairs(ctx, turn, name)
		return
	}
	for _, target := range mutationIntentTargets(ctx, name, args) {
		key := ledgerKey(ctx, target.Rel)
		r := s.repairs[key]
		open := r != nil && r.Open
		switch {
		case name == "delete_file" || name == "move_file":
			if open && result.Success {
				s.closeRepair(ctx, r, turn, name, "removed")
			} else if open {
				s.recordRepairAttempt(ctx, r, turn, name, false, result.Error)
			}
		case landedUnparseable(result):
			if !open {
				r = s.openRepair(ctx, key, target.Rel, turn, name)
			}
			s.recordRepairAttempt(ctx, r, turn, name, true, result.ValidationDetail)
		case result.MutationStatus == MutationApplied:
			if open {
				how := "parses"
				if result.ValidationStatus != ValidationPassed {
					how = "not checked"
				}
				s.closeRepair(ctx, r, turn, name, how)
			}
		case open:
			s.recordRepairAttempt(ctx, r, turn, name, false, result.Error)
		}
	}
}

// refreshRepairs re-checks each open repair whose bytes changed since its
// error was taken: a command can rewrite, fix or remove a file. A check that
// cannot run fails open, like the edit gates, and closes the repair.
func (s *runState) refreshRepairs(ctx *AgentContext, turn int, tool string) {
	if ctx == nil || (ctx.Ctx != nil && ctx.Ctx.Err() != nil) {
		return
	}
	for _, key := range s.repairOrder {
		r := s.repairs[key]
		if r == nil || !r.Open {
			continue
		}
		if _, err := os.Stat(key); os.IsNotExist(err) {
			s.closeRepair(ctx, r, turn, tool, "removed")
			continue
		}
		data, ok := readLedgerBytes(key)
		if !ok || hashBytes(data) == r.Hash {
			continue
		}
		o := fallbackSyntaxOutcomeFor(ctx, r.Rel, string(data)).aggregate()
		if o.Status == ValidationFailed {
			s.recordRepairAttempt(ctx, r, turn, tool, true, o.Detail)
			continue
		}
		how := "parses"
		if o.Status != ValidationPassed {
			how = "not checked"
		}
		s.closeRepair(ctx, r, turn, tool, how)
	}
}

func (s *runState) openRepair(ctx *AgentContext, key, rel string, turn int, tool string) *repairState {
	if s.repairs == nil {
		s.repairs = map[string]*repairState{}
	}
	if _, seen := s.repairs[key]; !seen {
		s.repairOrder = append(s.repairOrder, key)
	}
	r := &repairState{Rel: rel, Key: key, OpenedTurn: turn, OpenedBy: tool,
		BrokenBefore: repairEditTools[tool], Open: true}
	s.repairs[key] = r
	log.Printf("[agent] repair opened: %s by %s at turn %d", rel, tool, turn)
	return r
}

// recordRepairAttempt appends one attempt. A landed attempt also takes the
// file's current error and hash.
func (s *runState) recordRepairAttempt(ctx *AgentContext, r *repairState, turn int, tool string, landed bool, errText string) {
	r.Attempts = append(r.Attempts, repairAttempt{Turn: turn, Tool: tool, Landed: landed, Error: errText})
	event := "attempt"
	if len(r.Attempts) == 1 {
		event = "opened"
	}
	if landed {
		r.Error = errText
		if data, ok := readLedgerBytes(r.Key); ok {
			r.Hash = hashBytes(data)
		}
	}
	payload := map[string]interface{}{
		"event": event, "path": r.Rel, "turn": turn, "tool": tool, "landed": landed,
		"error": truncateStr(errText, 200), "line": syntaxErrorLine(errText),
		"attempts": len(r.Attempts),
	}
	if event == "opened" {
		payload["broken_before"] = r.BrokenBefore
	}
	ctx.Stream("repair", payload)
}

func (s *runState) closeRepair(ctx *AgentContext, r *repairState, turn int, tool, how string) {
	r.Open = false
	log.Printf("[agent] repair closed: %s (%s) at turn %d after %d attempt(s)", r.Rel, how, turn, len(r.Attempts))
	ctx.Stream("repair", map[string]interface{}{
		"event": "closed", "path": r.Rel, "turn": turn, "tool": tool, "how": how,
		"attempts": len(r.Attempts),
	})
}

// openRepairs are the repairs still open, in the order they opened.
func (s *runState) openRepairs() []*repairState {
	if s == nil {
		return nil
	}
	var out []*repairState
	for _, key := range s.repairOrder {
		if r := s.repairs[key]; r != nil && r.Open {
			out = append(out, r)
		}
	}
	return out
}

// repairOpenFor reports whether path names a file with an open repair.
func (s *runState) repairOpenFor(ctx *AgentContext, path string) bool {
	r := s.repairs[ledgerKey(ctx, path)]
	return r != nil && r.Open
}

// syntaxErrorLine is the line a parse error names, or 0.
func syntaxErrorLine(errText string) int {
	m := reSyntaxLineNo.FindStringSubmatch(errText)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// repairStatus is one file's current state: its parse error and the lines
// around it, read from disk now.
func repairStatus(r *repairState) string {
	return r.Rel + " does not parse: " + repairError(r)
}

// repairError is a file's parse error as a sentence, with the lines around
// it when the error names one.
func repairError(r *repairState) string {
	e := strings.TrimSpace(truncateStr(r.Error, 160))
	if !strings.HasSuffix(e, ".") && !strings.HasSuffix(e, "?") && !strings.HasSuffix(e, "!") {
		e += "."
	}
	if data, ok := readLedgerBytes(r.Key); ok {
		e += offendingLineNote(string(data), r.Error)
	}
	return e
}

// repairStatuses names up to maxRepairsNamed files.
func repairStatuses(open []*repairState) string {
	var parts []string
	for i, r := range open {
		if i == maxRepairsNamed {
			parts = append(parts, fmt.Sprintf("(%d more files do not parse.)", len(open)-i))
			break
		}
		parts = append(parts, repairStatus(r))
	}
	return strings.Join(parts, "\n")
}

// repairNote is what every tool result carries while a repair is open.
func (s *runState) repairNote() string {
	open := s.openRepairs()
	if len(open) == 0 {
		return ""
	}
	return repairStatuses(open) + "\nA file you wrote does not parse, so the work is not " +
		"finished. Fix it before you finish. You can still write other files."
}

// repairGateMessage is what a done gets back while a repair is open.
func repairGateMessage(open []*repairState) string {
	return "Not finished: " + repairStatuses(open) + "\nA file you wrote does not parse, " +
		"so the session cannot finish yet. Read the lines around the error, fix them " +
		"(edit_file or replace_lines, or rewrite the file), and finish once it parses."
}

// repairNames joins the open files for a sentence: "a.py", "a.py and b.py",
// "a.py, b.py and c.py".
func repairNames(open []*repairState) string {
	names := make([]string, 0, len(open))
	for _, r := range open {
		names = append(names, r.Rel)
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// repairEndingWhy says, in the user's words, why a session with an open
// repair ended.
func repairEndingWhy(st *runState, reason string) string {
	switch reason {
	case "repair_unfinished":
		if n := st.gateBounces["repair_gate"]; n > 0 {
			return fmt.Sprintf("the agent was sent back to fix it %d times and it still did not parse", n)
		}
		return "there was not enough time left to send the agent back to fix it"
	case "work_deadline":
		return "it ran out of time"
	case "turn_budget_exhausted":
		return "it used all of its turns"
	case "same_target_failures", "failure_ceiling", "repeated_refusal":
		return "tool calls kept failing"
	case "unusable_model_output":
		return "the model's replies could not be used"
	case "inference_failed":
		return "the model service failed"
	case "lens_unavailable":
		return "the lens service failed"
	case "cancelled":
		return "it was cancelled"
	}
	return "it stopped (" + reason + ")"
}

// repairHandoff is the final message's account of the files still open when a
// session ends, or "" when none is. It lists what was tried and the error
// that remains, says why the session ended, and asks the user to take a
// look. Facts only: attempts and errors, no account of why the fix was not
// found. Turns are counted from 1, as the TUI shows them.
func repairHandoff(st *runState, reason string) string {
	open := st.openRepairs()
	if len(open) == 0 {
		return ""
	}
	verb := "does"
	if len(open) > 1 {
		verb = "do"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Stopped: %s still %s not parse, so the work is not finished.", repairNames(open), verb)
	for i, r := range open {
		if i == maxRepairsNamed {
			fmt.Fprintf(&sb, "\n\n%d more files are in the same state.", len(open)-i)
			break
		}
		fmt.Fprintf(&sb, "\n\nWhat was tried on %s:", r.Rel)
		shown := r.Attempts
		if len(shown) > maxRepairAttemptsShown {
			fmt.Fprintf(&sb, "\n- (%d earlier attempts not shown)", len(shown)-maxRepairAttemptsShown)
			shown = shown[len(shown)-maxRepairAttemptsShown:]
		}
		for _, a := range shown {
			if a.Landed {
				fmt.Fprintf(&sb, "\n- turn %d, %s: %s", a.Turn+1, a.Tool, truncateStr(a.Error, 160))
			} else {
				fmt.Fprintf(&sb, "\n- turn %d, %s, not applied: %s", a.Turn+1, a.Tool, truncateStr(a.Error, 160))
			}
		}
		if r.BrokenBefore {
			fmt.Fprintf(&sb, "\nIt already did not parse before the edit at turn %d.", r.OpenedTurn+1)
		}
		fmt.Fprintf(&sb, "\nThe error that remains: %s", repairError(r))
	}
	fmt.Fprintf(&sb, "\n\nThe session ended because %s. Please take a look at %s.",
		repairEndingWhy(st, reason), repairNames(open))
	return sb.String()
}

// emitRepairHandoff records each open repair at the terminal.
func (s *runState) emitRepairHandoff(ctx *AgentContext, reason string) {
	for _, r := range s.openRepairs() {
		ctx.Stream("repair", map[string]interface{}{
			"event": "handoff", "path": r.Rel, "turn": s.turn, "reason": reason,
			"attempts": len(r.Attempts), "error": truncateStr(r.Error, 200),
			"line": syntaxErrorLine(r.Error), "broken_before": r.BrokenBefore,
		})
	}
}
