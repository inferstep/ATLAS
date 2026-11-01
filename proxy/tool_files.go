// The tools that remove or relocate a whole file: delete_file and move_file.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

func deleteFileTool() *ToolDef {
	return &ToolDef{
		Name:        "delete_file",
		Effect:      ToolEffectDirectMutation,
		Description: "Delete a file or empty directory. Use for removing files that are no longer needed.",
		InputSchema: DeleteFileInput{},
		ReadOnly:    false,
		Destructive: true,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input DeleteFileInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
			}

			// Reject empty path — same reasoning as read_file.
			if strings.TrimSpace(input.Path) == "" {
				return noMutation("delete_file: path cannot be empty. Provide the path of the file you want to delete."), nil
			}

			path := resolveAgentPath(ctx, input.Path)
			// The approval is spent by this attempt whatever happens next --
			// a preflight refusal included -- so one answer can never carry
			// to a later call, and the reference it holds lives exactly as
			// long as the attempt. `approved` is only ever non-empty when the
			// user answered this exact call through the permission endpoint.
			approved, userApproved := takeDeleteApproval(ctx, path)
			if userApproved {
				defer approved.release()
			}
			info, err := os.Stat(path)
			if err != nil {
				return nil, errNoMutation(fmt.Errorf("file not found: %s", input.Path))
			}
			if info.IsDir() {
				entries, _ := os.ReadDir(path)
				if len(entries) > 0 {
					// A guard declining a target it could otherwise remove.
					return refusedNoCheck(fmt.Sprintf("directory not empty: %s (%d entries) — delete_file only removes files or empty directories", input.Path, len(entries))), nil
				}
			}
			return removeDeleteTarget(ctx, rawInput, input.Path, path, approved, userApproved), nil
		},
	}
}

// removeDeleteTarget removes what delete_file found, checking an approval again
// right next to the removal.
func removeDeleteTarget(ctx *AgentContext, rawInput json.RawMessage, shownPath, path string, approved deleteTarget, userApproved bool) *ToolResult {
	// The approval was granted against an inspected object. Re-inspect
	// now, next to the removal, and compare: bytes, type, link text,
	// emptiness. If any of it moved while the prompt was on screen,
	// the user's answer was about a different thing and is spent
	// rather than reused. A fresh tool call can ask again.
	//
	// This narrows the window between check and remove; it does not
	// close it. There is no portable way to remove a directory entry
	// conditional on the inode behind it, so a change landing inside
	// that window is still possible. What is ruled out is honouring an
	// approval for bytes that were already different when the user
	// answered.
	// The approved route.
	if userApproved {
		// The re-inspection's own reference is needed only for the
		// comparison and is released with the attempt.
		now, refusal := inspectDeleteTarget(ctx, rawInput)
		defer now.release()
		if refusal != "" || !approved.identityMatches(now) {
			log.Printf("[delete_file] approval is stale for %s — not deleting", logPath(shownPath))
			return refusedNoCheck(fmt.Sprintf(
				"%s changed after you were asked about it, so the approval no longer "+
					"describes what is on disk and nothing was deleted. Ask again if "+
					"you still want it removed.", shownPath))
		}
		// Room to account for it, reserved before anything is
		// removed: a destructive mutation this session could not
		// track is worse than one it declines.
		if !reserveDeletionSlot(ctx, path) {
			log.Printf("[delete_file] no room to track %s — refusing", logPath(shownPath))
			return refusedNoCheck(fmt.Sprintf(
				"this session is already tracking as many removals as it can account "+
					"for, so %s was not deleted.", shownPath))
		}
	}
	if rmErr := os.Remove(path); rmErr != nil {
		// The removal was attempted and the entry may or may not
		// still be there, so the producer says failed, not none.
		return &ToolResult{
			Success:          false,
			Error:            fmt.Sprintf("delete_file: %v", rmErr),
			MutationStatus:   MutationFailed,
			ValidationKind:   ValidationKindNone,
			ValidationStatus: ValidationNotApplicable,
		}
	}

	out := DeleteFileOutput{Deleted: true}
	outBytes, _ := json.Marshal(out)
	// The approval, the revalidation and the successful removal are
	// all true at this instant, so they are recorded together rather
	// than reconstructed later from separate booleans. Absence and the
	// tombstone are the ledger's to confirm, and it promotes this.
	if userApproved {
		noteDeletionAttempt(ctx, permCallIDFor(ctx), approved)
	}

	// Removal demonstrated. A delete validates nothing: there are no
	// bytes left to have an opinion about.
	//
	// The delete returns like any other call, so a task that asks for
	// more than the delete still gets finished. It cannot authorise a
	// completion: the tombstone rule at the real terminal stands, and
	// is fail-closed.
	return &ToolResult{Success: true, Data: outBytes,
		MutationStatus:   MutationApplied,
		ValidationKind:   ValidationKindNone,
		ValidationStatus: ValidationNotApplicable}
}

// ---------------------------------------------------------------------------
// move_file — relocate / rename a file within the workspace.
//
// A pure move is not a content change, so it bypasses the V3 / surgical-edit
// gate — content is preserved verbatim. Refuses to clobber an existing
// destination file so a relocation can't silently destroy data.
// ---------------------------------------------------------------------------

func moveFileTool() *ToolDef {
	return &ToolDef{
		Name:        "move_file",
		Effect:      ToolEffectDirectMutation,
		Description: "Move or rename a file within the project (e.g. move index.html into templates/, or rename old.py to new.py). Use this to reorganize files — shell `mv`/`cp` are refused. If destination is an existing directory, the file is moved into it keeping its name. Content is preserved exactly.",
		InputSchema: MoveFileInput{},
		ReadOnly:    false,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input MoveFileInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, errNoMutation(fmt.Errorf("invalid input: %w", err))
			}
			if strings.TrimSpace(input.Source) == "" || strings.TrimSpace(input.Destination) == "" {
				return noMutation(`move_file: both source and destination are required. Call with {"source":"<current path>","destination":"<new path>"}.`), nil
			}

			src := resolveAgentPath(ctx, input.Source)
			srcInfo, err := os.Stat(src)
			if err != nil {
				return noMutation(fmt.Sprintf("move_file: source %s not found. Use list_directory or find_file to confirm the path before moving.", input.Source)), nil
			}

			// Resolve destination. If it names an existing directory (or ends
			// with a separator), move INTO it keeping the source basename —
			// mirrors `mv file dir/`. The relative dest is what we report back
			// so the model's mental model stays in project-relative terms.
			relDest := input.Destination
			dst := resolveAgentPath(ctx, input.Destination)
			if info, err := os.Stat(dst); (err == nil && info.IsDir()) || strings.HasSuffix(input.Destination, "/") {
				dst = filepath.Join(dst, filepath.Base(src))
				relDest = filepath.Join(input.Destination, filepath.Base(src))
			}

			if src == dst {
				return noMutation("move_file: source and destination are the same path — nothing to do."), nil
			}

			// Never clobber an existing destination file: a relocation must not
			// silently destroy data. Tell the model to pick another name or
			// delete_file the destination first if the overwrite is intended.
			if _, err := os.Stat(dst); err == nil {
				// The clobber guard declining a move it could otherwise make.
				return refusedNoCheck(fmt.Sprintf("move_file: destination %s already exists. Pick a different name, or delete_file the destination first if you mean to replace it.", relDest)), nil
			}

			return relocateFile(ctx, input, src, dst, relDest, srcInfo)
		},
	}
}

// relocateFile moves src to dst, by a rename or, across filesystems, by a copy
// and a removal. It then re-points what the session recorded about the file
// and reports the move.
func relocateFile(ctx *AgentContext, input MoveFileInput, src, dst, relDest string, srcInfo os.FileInfo) (*ToolResult, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		// The source is untouched, but MkdirAll can leave part of the
		// tree behind, so this is an attempted mutation that errored.
		return nil, errFailedMutation(
			fmt.Errorf("move_file: cannot create destination dir: %w", err),
			ValidationKindNone, ValidationNotApplicable)
	}

	// os.Rename is atomic on the same filesystem; fall back to
	// copy+remove across devices (bind mounts can straddle filesystems).
	if err := os.Rename(src, dst); err != nil {
		// Rename is all-or-nothing: a failure leaves the source where
		// it was and the destination absent.
		if srcInfo.IsDir() {
			return nil, errNoMutation(fmt.Errorf("move_file: cannot move directory across filesystems: %w", err))
		}
		data, rerr := readRegularFile(src)
		if rerr != nil {
			return nil, errNoMutation(fmt.Errorf("move_file: cannot read source: %w", rerr))
		}
		if werr := os.WriteFile(dst, data, srcInfo.Mode().Perm()); werr != nil {
			// The destination may hold a partial copy.
			return nil, errFailedMutation(
				fmt.Errorf("move_file: cannot write destination: %w", werr),
				ValidationKindNone, ValidationNotApplicable)
		}
		// A failure here leaves BOTH copies on disk. The move still
		// happened, so it is applied; the ledger sees the surviving
		// source and declines to tombstone it.
		_ = os.Remove(src)
	}
	log.Printf("[move_file] %s → %s", input.Source, relDest)

	// Keep agent bookkeeping consistent: the file the model just read
	// now lives at the new path. Re-point the recorded read and the
	// session-write set so a follow-up edit isn't bounced as blind and
	// dedup logic tracks the right path.
	if content, ok := ctx.GetFileRead(src); ok {
		ctx.RecordFileRead(dst, content)
		ctx.ForgetFileRead(src)
	}
	if ctx.SessionWrites != nil {
		if ctx.SessionWrites[input.Source] {
			delete(ctx.SessionWrites, input.Source)
		}
		ctx.SessionWrites[relDest] = true
	}

	out := MoveFileOutput{Moved: true, Source: input.Source, Destination: relDest}
	outBytes, _ := json.Marshal(out)
	// Relocation demonstrated. A move preserves content exactly, so
	// this tool checks nothing: any verdict the bytes had was earned
	// under the old name and does not travel with them.
	return &ToolResult{Success: true, Data: outBytes,
		MutationStatus:   MutationApplied,
		ValidationKind:   ValidationKindNone,
		ValidationStatus: ValidationNotApplicable}, nil
}
