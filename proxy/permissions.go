package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Permission system — controls which tool calls require user confirmation
// ---------------------------------------------------------------------------

// denyCommandReason reports why a shell command is blocked, or "" if allowed.
// It is validateShellCommand: one policy for every tool that runs a command.
func denyCommandReason(cmd string) string {
	return validateShellCommand(cmd)
}

// denyWritePathReason reports why writing to a path is blocked, or "" if
// allowed. Matching is on the base name so nested paths (certs/server.pem) are
// caught, while template files (.env.example) and unrelated names (staging.env)
// are not.
func denyWritePathReason(path string) string {
	if path == "" {
		return ""
	}
	base := filepath.Base(filepath.Clean(path))
	switch {
	case base == ".env":
		return "blocked by safety rule: writing .env"
	case strings.HasSuffix(base, ".pem"):
		return "blocked by safety rule: writing a .pem key"
	case strings.HasSuffix(base, ".key"):
		return "blocked by safety rule: writing a .key file"
	case strings.Contains(base, "credentials"):
		return "blocked by safety rule: writing a credentials file"
	}
	return ""
}

// denyReadPathReason reports why READING a path into model context is
// blocked, or "" if allowed. Credential stores are excluded from model
// context by default: their contents would otherwise flow into prompts,
// logs and session files. Matching is on base
// name (plus the .ssh/.aws/.kube parent-dir cases) so templates
// (.env.example) and unrelated names stay readable. Explicit override:
// ATLAS_ALLOW_CREDENTIAL_READS=1 (the refusal message says so).
func denyReadPathReason(path string) string {
	if path == "" || os.Getenv("ATLAS_ALLOW_CREDENTIAL_READS") == "1" {
		return ""
	}
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	parent := filepath.Base(filepath.Dir(clean))
	blocked := ""
	switch {
	case base == ".env" || (strings.HasPrefix(base, ".env.") && base != ".env.example"):
		blocked = base
	case base == ".netrc" || base == "_netrc":
		blocked = base
	case base == ".npmrc" || base == ".pypirc":
		blocked = base
	case strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key"):
		blocked = base
	case base == "id_rsa" || base == "id_ecdsa" || base == "id_ed25519" || base == "id_dsa":
		blocked = base
	case parent == ".ssh" && !strings.HasSuffix(base, ".pub"):
		blocked = ".ssh/" + base
	case parent == ".aws" && (base == "credentials" || base == "config"):
		blocked = ".aws/" + base
	case parent == ".kube" && base == "config":
		blocked = ".kube/" + base
	case parent == ".docker" && base == "config.json":
		blocked = ".docker/" + base
	case base == "service-token" && parent == "secrets":
		blocked = "secrets/" + base
	case base == "api-keys.json" && parent == "secrets":
		blocked = "secrets/" + base
	case strings.Contains(base, "credentials"):
		blocked = base
	}
	if blocked == "" {
		return ""
	}
	return fmt.Sprintf("blocked by safety rule: reading %s into model "+
		"context (credential file). If this file is intentionally "+
		"non-sensitive, set ATLAS_ALLOW_CREDENTIAL_READS=1 on the proxy "+
		"and retry.", blocked)
}

// shouldDenyToolCall checks if a tool call is blocked by the built-in safety
// rules. These apply in every permission mode.
//
// The rules follow what a tool does, not its name. Keyed on names, they
// missed run_background (a command), insert_after and replace_lines (writes),
// and a move_file whose source was a credential file.
func shouldDenyToolCall(toolName string, args json.RawMessage) (bool, string) {
	tool := getTool(toolName)
	if tool == nil {
		return false, ""
	}
	switch {
	case tool.Effect == ToolEffectCommandUnobserved:
		if reason := denyCommandReason(ledgerArgPath(args, "command")); reason != "" {
			return true, reason
		}
	case toolName == "move_file":
		var input MoveFileInput
		if json.Unmarshal(args, &input) != nil {
			return false, ""
		}
		// Moving a credential file would let it be read under another name.
		if denyReadPathReason(input.Source) != "" || denyWritePathReason(input.Source) != "" {
			return true, "blocked by safety rule: moving a credential file (" +
				filepath.Base(filepath.Clean(input.Source)) + ")"
		}
		if reason := denyWritePathReason(input.Destination); reason != "" {
			return true, reason
		}
	case tool.Effect == ToolEffectDirectMutation && toolName != "delete_file":
		// delete_file has its own per-object approval.
		if reason := denyWritePathReason(ledgerArgPath(args, "path")); reason != "" {
			return true, reason
		}
	case toolName == "read_file" || toolName == "outline_file":
		if reason := denyReadPathReason(ledgerArgPath(args, "path")); reason != "" {
			return true, reason
		}
	}
	return false, ""
}

// describeToolCall generates a human-readable description of a tool call.
func describeToolCall(toolName string, args json.RawMessage) string {
	switch toolName {
	// A command is shown whole. Approval is the only per-command control, and
	// a prompt cut at 100 bytes hid the tail of a chain (`... && rm -rf src`).
	case "run_command":
		var input RunCommandInput
		if json.Unmarshal(args, &input) == nil {
			return "Run command: " + input.Command
		}
	case "run_background":
		var input RunBackgroundInput
		if json.Unmarshal(args, &input) == nil {
			return "Run in the background: " + input.Command
		}
	case "stop_background":
		if id := ledgerArgPath(args, "job_id"); id != "" {
			return "Stop background job " + id
		}
	case "write_file":
		var input WriteFileInput
		if json.Unmarshal(args, &input) == nil {
			return "Write file: " + input.Path + " (" + formatSize(len(input.Content)) + ")"
		}
	case "edit_file":
		var input EditFileInput
		if json.Unmarshal(args, &input) == nil {
			return "Edit file: " + input.Path
		}
	}
	return toolName
}

// formatSize formats byte count as human-readable.
func formatSize(bytes int) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d bytes", bytes)
	}
	return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
}

// Interactive permission approval.
//
// In default and accept-edits modes a destructive tool call pauses the agent
// loop, emits a "permission_request" SSE event, and blocks until the client
// POSTs a decision to /v1/permission. This mirrors the /cancel topology: the
// agent loop is mid-turn on one HTTP request while the decision arrives on a
// separate request, correlated through a package-level sync.Map keyed by
// session id + tool-call id.

// permDecision is the client's answer to a permission_request.
type permDecision struct {
	allow bool
	// scope "session" (from the client's "allow for the rest of the session"
	// choice) additionally whitelists the tool for the remainder of the
	// current turn so a repeated call does not prompt again.
	scope string
}

// pendingPermission is the pendingPermissions map value.
type pendingPermission struct {
	decision chan permDecision
}

// pendingPermissions correlates an in-flight permission_request with the
// /v1/permission POST that answers it. Keyed by permKey(sessionID, callID).
var pendingPermissions sync.Map

func permKey(sessionID, callID string) string {
	return sessionID + "|" + callID
}

// permissionTimeout is the fail-safe: if no decision arrives (and the client
// neither disconnects nor cancels), the tool call is denied rather than
// hanging the turn forever. Overridable via ATLAS_PERMISSION_TIMEOUT_SEC.
func permissionTimeout() time.Duration {
	if v := os.Getenv("ATLAS_PERMISSION_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 10 * time.Minute
}

// awaitPermission emits a permission_request for a destructive tool call and
// blocks until the client answers, the request context is cancelled (client
// disconnect or /cancel), or the fail-safe timeout elapses. It returns true if
// the call is allowed. On an allow with session scope the tool is added to the
// turn's in-context allowlist so subsequent calls in this turn skip the prompt.
func awaitPermission(ctx *AgentContext, toolName, callID string, args json.RawMessage) bool {
	// No session id means no channel back from the client to answer a
	// prompt. Failing open here would make mode:"default" silently
	// yolo-equivalent for any client that omits session_id — deny
	// instead. Clients that want unattended destructive tools opt in
	// explicitly with mode:"yolo" (or pre-approve via
	// session_allowed_tools); interactive clients pass session_id and
	// answer /v1/permission.
	if ctx.PassID == "" {
		log.Printf("[permission] %s requires approval but the request has no session_id — denying. Pass session_id and answer POST /v1/permission, pre-approve via session_allowed_tools, or use mode \"yolo\".", toolName)
		return false
	}

	// A deletion is confirmed against an inspected target, not against a call.
	// Everything the tool would refuse is refused HERE, before the user is
	// asked: a path escape, a blank or malformed path, a deny-listed target, a
	// missing file, a non-empty directory and an unsupported type all return
	// without a prompt, because asking about an operation the proxy will then
	// decline teaches the user nothing and trains them to click through.
	var target deleteTarget
	if toolName == "delete_file" {
		t, refusal := inspectDeleteTarget(ctx, args)
		if refusal != "" {
			log.Printf("[permission] not asking about %s: %s", toolName, refusal)
			return false
		}
		target = t
	}

	entry := &pendingPermission{decision: make(chan permDecision, 1)}
	key := permKey(ctx.PassID, callID)
	pendingPermissions.Store(key, entry)
	defer pendingPermissions.CompareAndDelete(key, entry)

	req := PermissionRequest{
		ToolName:   toolName,
		Args:       args,
		Message:    describeToolCall(toolName, args),
		ToolCallID: callID,
	}
	if toolName == "delete_file" {
		req.Message = describeDeleteTarget(target)
		req.CanonicalPath = target.Canonical
		req.TargetType = string(target.Kind)
		req.ContentSHA256 = target.SHA256
		req.OneTimeOnly = true
	}
	ctx.Stream("permission_request", req)

	select {
	case d := <-entry.decision:
		if !d.allow {
			target.release()
			return false
		}
		if toolName == "delete_file" {
			// Session scope is refused for deletion, and refused by
			// downgrading rather than denying: the user said yes to THIS
			// file, and that answer is honoured for exactly this attempt.
			// Adding delete_file to the turn allowlist would let one answer
			// authorise every later deletion, which is the thing this exists
			// to prevent.
			if d.scope == "session" {
				log.Printf("[permission] delete_file approval is one-time; " +
					"session scope downgraded for this call")
			}
			grantDeleteApproval(ctx, callID, target)
			return true
		}
		if d.scope == "session" {
			ctx.allowToolForTurn(toolName)
		}
		return true
	case <-ctx.Ctx.Done():
		target.release()
		return false
	case <-time.After(permissionTimeout()):
		log.Printf("[permission] %s timed out for session %q — denying", toolName, ctx.PassID)
		target.release()
		return false
	}
}

// handlePermission receives a client's approve/deny decision and signals the
// blocked agent loop. Idempotent: a decision for an unknown/already-answered
// key returns 404, mirroring /cancel.
func handlePermission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, ErrUnsupported, "method not allowed")
		return
	}
	var req struct {
		SessionID  string `json:"session_id"`
		ToolCallID string `json:"tool_call_id"`
		Decision   string `json:"decision"` // "allow" or "deny"
		Scope      string `json:"scope"`    // "once" (default) or "session"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidInput, "invalid request body")
		return
	}
	if req.SessionID == "" || req.ToolCallID == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidInput, "session_id and tool_call_id required")
		return
	}

	v, ok := pendingPermissions.LoadAndDelete(permKey(req.SessionID, req.ToolCallID))
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]bool{"delivered": false})
		return
	}
	entry, ok := v.(*pendingPermission)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad permission entry"})
		return
	}
	// Buffered channel (cap 1) + LoadAndDelete guarantees exactly one send.
	entry.decision <- permDecision{allow: req.Decision == "allow", scope: req.Scope}
	log.Printf("[permission] %q %q for session %q (scope %q)",
		req.ToolCallID, req.Decision, req.SessionID, req.Scope)
	_ = json.NewEncoder(w).Encode(map[string]bool{"delivered": true})
}

// permCallID is the tool-call identifier used to correlate a permission
// request with its decision. It matches the ToolCallID the loop assigns to
// tool messages so the ids line up across the turn.
func permCallID(turn int) string {
	return fmt.Sprintf("call_%d", turn)
}

// Trust modes govern whether — and where — model-authored commands may
// execute. A newly-opened repository is untrusted content; running its
// build/test commands is a decision the operator makes explicitly.
//
//   untrusted     — no command execution at all (run_command refused).
//   trusted       — commands run in the isolated sandbox container
//                   (the default; host execution is downgraded to sandbox).
//   fully-trusted — advanced: host execution (ATLAS_VERIFY_IN=host) is
//                   honored, dropping the container backstop.
//
// Set via ATLAS_TRUST_MODE. The default is "trusted": commands run, but
// only in the sandbox. This keeps the out-of-box behavior safe (isolated
// execution) while making "run nothing" and "run on the host" both
// explicit, deliberate choices.

type trustMode string

const (
	trustUntrusted    trustMode = "untrusted"
	trustTrusted      trustMode = "trusted"
	trustFullyTrusted trustMode = "fully-trusted"
)

// resolveTrustMode reads ATLAS_TRUST_MODE, defaulting to trusted. An
// unrecognized value falls back to the safe default rather than failing
// open to host execution.
func resolveTrustMode() trustMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ATLAS_TRUST_MODE"))) {
	case "untrusted":
		return trustUntrusted
	case "fully-trusted", "fully_trusted":
		return trustFullyTrusted
	case "trusted", "":
		return trustTrusted
	default:
		return trustTrusted
	}
}

// commandsAllowed reports whether run_command may execute at all.
func (m trustMode) commandsAllowed() bool {
	return m != trustUntrusted
}

// hostExecutionAllowed reports whether host execution (bypassing the
// sandbox) is permitted. Only fully-trusted honors it; trusted downgrades
// a host request to sandbox execution so an ATLAS_VERIFY_IN=host setting
// can't quietly escalate below the intended trust level.
func (m trustMode) hostExecutionAllowed() bool {
	return m == trustFullyTrusted
}

// untrustedRefusal is the message returned when run_command is called
// under the untrusted mode.
const untrustedRefusal = "command execution is disabled: ATLAS_TRUST_MODE=untrusted. " +
	"This repository's commands are treated as untrusted content. Set " +
	"ATLAS_TRUST_MODE=trusted to run them in the isolated sandbox, or " +
	"fully-trusted to allow host execution."

// --- Deleting a named target ------------------------------------------------
//
// The permission flow confirms a CALL. For a deletion that is not enough: the
// user was shown the bare string "delete_file" with no path, was asked before
// the target had been canonicalised or bounded -- so a path escape, a blank
// path and a non-empty directory all produced prompts for operations the proxy
// would then refuse -- and one session-scoped answer put delete_file on the
// turn allowlist, so approving a.py authorised deleting b.py without asking.
// Nothing noticed the file changing while the prompt sat on screen.
//
// So a deletion is confirmed against an inspected TARGET, once. Everything
// here is structured: a resolved path, a stat, a hash. No sentence the user or
// the model wrote takes part in the decision.

// deleteTargetKind is the set of things this confirmation can honour. Anything
// else fails closed rather than asking for approval it cannot bind.
type deleteTargetKind string

const (
	deleteTargetFile     deleteTargetKind = "regular file"
	deleteTargetSymlink  deleteTargetKind = "symlink"
	deleteTargetEmptyDir deleteTargetKind = "empty directory"
)

// deleteTarget is what the user was actually shown, and what must still be
// true at execution. Identity is the exact bytes for a file, the exact link
// text for a symlink, and emptiness for a directory.
type deleteTarget struct {
	Canonical string // absolute, workspace-validated
	Rel       string // as the model spelled it, for the message
	Kind      deleteTargetKind
	Size      int64
	SHA256    string // regular files only
	LinkText  string // symlinks only

	// info is the inspection's own lstat of the path. It supplies the kind
	// and, at revalidation, the current entry's device and inode for the held
	// object to be compared against. Bytes are not identity: a file deleted
	// and rewritten with the same contents is a different object, and an
	// approval for the first must not remove the second.
	info os.FileInfo
	// handle is the held kernel reference to the inspected object, taken at
	// inspection and kept until whoever owns the approval releases it. It is
	// what makes the identity a binding rather than two readings of numbers a
	// filesystem may recycle. Kept internal -- descriptors, inode and device
	// numbers never reach the event, the model, the logs, or the wire.
	handle *objectHandle
}

// release lets go of the held object. Safe on a zero target and safe to call
// more than once; the handle itself is idempotent.
func (d deleteTarget) release() { d.handle.release() }

// withoutHandle is the target as a record: what was approved and removed,
// with no reference left to hold.
func (d deleteTarget) withoutHandle() deleteTarget { d.handle = nil; return d }

// identityMatches reports whether the object inspected now is the object the
// user was asked about, in the same state: the object itself, then the same
// path, kind, bytes or link text, and size.
//
// The object question is answered by the held reference, not by comparing
// numbers from two inspections. os.SameFile was the previous answer, and it
// was wrong on ext4: a file removed and rewritten with the same bytes came
// back with the same inode number, and an approval for the original removed
// the replacement. The held reference keeps the approved object alive and
// reads its link count and identity from the object; a replacement cannot
// pass both. Where no reference is held -- an unsupported platform, a failed
// pin, or a reference already released -- this refuses rather than guesses.
//
// The content, kind, link-text and size checks are unchanged and still catch
// the in-place changes a held reference does not see: bytes rewritten, a link
// retargeted, a directory that gained a child (it is no longer empty), a type
// swapped under the same name.
func (d deleteTarget) identityMatches(other deleteTarget) bool {
	if other.info == nil || !d.handle.stillTheObjectAt(other.info) {
		return false
	}
	return d.Canonical == other.Canonical && d.Kind == other.Kind &&
		d.SHA256 == other.SHA256 && d.LinkText == other.LinkText &&
		d.Size == other.Size
}

// maxDeleteHashBytes bounds the identity hash. A target larger than this is
// not refused -- it is hashed in full, streamed, never loaded whole -- this is
// only the read buffer.
const deleteHashBufferBytes = 64 * 1024

// inspectDeleteTarget runs every non-mutating check delete_file itself would
// run, and returns the target the user should be asked about.
//
// It is the same policy, not a restatement: resolveWorkspacePath for
// containment, denyWritePathReason for the safety list, and the tool's own
// rules for what is deletable. Nothing here opens mutation debt, touches the
// ledger, tombstones anything, or writes to disk.
func inspectDeleteTarget(ctx *AgentContext, args json.RawMessage) (deleteTarget, string) {
	var input DeleteFileInput
	if err := json.Unmarshal(args, &input); err != nil {
		return deleteTarget{}, "delete_file: arguments are not usable"
	}
	if strings.TrimSpace(input.Path) == "" {
		return deleteTarget{}, "delete_file: path cannot be empty"
	}
	canonical, err := resolveWorkspacePath(ctx, input.Path)
	if err != nil {
		return deleteTarget{}, "delete_file: " + err.Error()
	}
	if reason := denyWritePathReason(input.Path); reason != "" {
		return deleteTarget{}, "delete_file: " + reason
	}
	// Lstat, never Stat: a symlink is its own object and deleting it must
	// never be presented as deleting whatever it points at.
	info, err := os.Lstat(canonical)
	if err != nil {
		return deleteTarget{}, fmt.Sprintf("file not found: %s", input.Path)
	}
	// Hold the object before describing it, following nothing, so that what
	// is described is what is held. A platform that cannot hold it gets no
	// prompt and no deletion: an approval that could bind to nothing is worse
	// than no approval.
	handle, perr := pinObjectFn(canonical)
	if perr != nil {
		return deleteTarget{}, "delete_file: " + perr.Error()
	}
	t := deleteTarget{Canonical: canonical, Rel: input.Path, Size: info.Size(), info: info, handle: handle}
	// Every refusal below happens after the reference was taken, so every
	// refusal lets go of it. The caller owns the reference only on success.
	refuse := func(msg string) (deleteTarget, string) {
		t.release()
		return deleteTarget{}, msg
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		link, lerr := os.Readlink(canonical)
		if lerr != nil {
			return refuse("delete_file: the link could not be read")
		}
		t.Kind, t.LinkText = deleteTargetSymlink, link
	case info.IsDir():
		entries, derr := os.ReadDir(canonical)
		if derr != nil {
			return refuse("delete_file: the directory could not be read")
		}
		if len(entries) > 0 {
			return refuse(fmt.Sprintf(
				"directory not empty: %s (%d entries) — delete_file only removes files or empty directories",
				input.Path, len(entries)))
		}
		t.Kind, t.Size = deleteTargetEmptyDir, 0
	case info.Mode().IsRegular():
		sum, size, herr := hashFileIdentity(ctx, canonical)
		if herr != nil {
			return refuse("delete_file: the file could not be read")
		}
		t.Kind, t.SHA256, t.Size = deleteTargetFile, sum, size
	default:
		// Devices, sockets, pipes: an approval this cannot honour is worse
		// than no approval.
		return refuse("delete_file: unsupported target type")
	}
	return t, ""
}

// hashFileIdentity streams the file, so a large target costs a buffer rather
// than its own size in memory, and a cancelled run stops promptly.
func hashFileIdentity(ctx *AgentContext, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	// The handle and the inspected path must describe the same object.
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("not a regular file")
	}
	h := sha256.New()
	buf := make([]byte, deleteHashBufferBytes)
	var total int64
	for {
		if ctx != nil && ctx.Ctx != nil && ctx.Ctx.Err() != nil {
			return "", 0, ctx.Ctx.Err()
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			total += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", 0, rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), total, nil
}

// describeDeleteTarget is protocol presentation over an inspected object. It
// is assembled from the stat, never from anything anyone wrote.
func describeDeleteTarget(t deleteTarget) string {
	if t.Kind == deleteTargetSymlink {
		return fmt.Sprintf("ATLAS wants to delete %s (symlink to %q — the link is removed, "+
			"not what it points at). Allow this one deletion?", t.Canonical, t.LinkText)
	}
	return fmt.Sprintf("ATLAS wants to delete %s (%s). Allow this one deletion?",
		t.Canonical, t.Kind)
}

// approvedDeletion is the one-shot grant. It is consumed by the first
// execution attempt whatever the outcome, so an approval can never authorise
// a second deletion.
type approvedDeletion struct {
	target deleteTarget
	callID string
}

// grantDeleteApproval records the user's decision against the exact target.
// The grant owns the target's held reference from here until the tool takes
// it. A grant that was never taken is released when the next one replaces it
// and, failing that, when the loop exits (releaseDeleteApproval).
func grantDeleteApproval(ctx *AgentContext, callID string, t deleteTarget) {
	if ctx == nil {
		t.release()
		return
	}
	ctx.mu.Lock()
	previous := ctx.approvedDelete
	ctx.approvedDelete = &approvedDeletion{target: t, callID: callID}
	ctx.lastDeleteCallID = callID
	ctx.mu.Unlock()
	if previous != nil {
		previous.target.release()
	}
}

// releaseDeleteApproval lets go of an approval no tool ever consumed. It runs
// at the loop's exit, so a turn that was approved and then ended -- cancelled,
// timed out, stopped, or finished without the call being executed -- leaves
// no reference behind. It grants nothing and records nothing.
func releaseDeleteApproval(ctx *AgentContext) {
	if ctx == nil {
		return
	}
	ctx.mu.Lock()
	held := ctx.approvedDelete
	ctx.approvedDelete = nil
	ctx.mu.Unlock()
	if held != nil {
		held.target.release()
	}
}

// takeDeleteApproval consumes the grant for a canonical path. It returns the
// approved target and whether one was held; either way the grant is gone. On
// success the caller owns the target's held reference and must release it
// when its attempt is over; a grant for a different path is released here,
// because it is spent either way.
func takeDeleteApproval(ctx *AgentContext, canonical string) (deleteTarget, bool) {
	if ctx == nil {
		return deleteTarget{}, false
	}
	ctx.mu.Lock()
	held := ctx.approvedDelete
	ctx.approvedDelete = nil
	ctx.mu.Unlock()
	if held == nil {
		return deleteTarget{}, false
	}
	if held.target.Canonical != canonical {
		held.target.release()
		return deleteTarget{}, false
	}
	return held.target, true
}

// --- What the user authorised, and what actually happened --------------------
//
// Four facts have to stay apart: the user approved this exact object, the model
// attempted the removal, the path is absent, and the task is finished. A
// successful tool call establishes the middle two and nothing else. These
// records exist so the last one can eventually be decided from the first,
// without asking any English word what the user meant.

// maxTrackedDeletions bounds live deletion records, matching the ceiling
// convention the debt tracker already uses. A session removing more paths than
// this is not helped by remembering more of them, and reserving space BEFORE
// the mutation is what keeps an untrackable deletion from happening at all.
const maxTrackedDeletions = 64

// deletionAttempt is written by the delete tool at the only moment all of its
// facts are true together: a user approval consumed for this exact call, that
// approval revalidated against the object immediately before removal, and
// os.Remove reporting success. It is not yet authority -- absence and the
// tombstone are the ledger's to confirm.
type deletionAttempt struct {
	CallID  string
	Target  deleteTarget
	Removed bool
}

// fulfilledDeletion is a deletion the user approved and the system completed,
// bound to the ledger generation that recorded it. Binding the generation is
// what stops an old record from speaking for a path that has since come back:
// a recreated file is a different generation and needs its own approval.
type fulfilledDeletion struct {
	CallID     string
	Canonical  string
	Kind       deleteTargetKind
	SHA256     string
	LinkText   string
	Generation int
}

// reserveDeletionSlot makes room for a record before anything is removed.
// Failing here refuses the deletion; the alternative is a destructive mutation
// this session cannot account for.
func reserveDeletionSlot(ctx *AgentContext, canonical string) bool {
	if ctx == nil {
		return false
	}
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	if ctx.deletionAttempts == nil {
		ctx.deletionAttempts = map[string]*deletionAttempt{}
	}
	if _, exists := ctx.deletionAttempts[canonical]; exists {
		return true // this path already has its slot
	}
	if len(ctx.deletionAttempts)+len(ctx.fulfilledDeletions) >= maxTrackedDeletions {
		return false
	}
	return true
}

// noteDeletionAttempt records the approved-and-removed facts together.
func noteDeletionAttempt(ctx *AgentContext, callID string, t deleteTarget) {
	if ctx == nil {
		return
	}
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	if ctx.deletionAttempts == nil {
		ctx.deletionAttempts = map[string]*deletionAttempt{}
	}
	ctx.deletionAttempts[t.Canonical] = &deletionAttempt{
		CallID: callID, Target: t.withoutHandle(), Removed: true,
	}
}

// promoteFulfilledDeletion is the single promotion point, and it re-checks
// every fact at once rather than trusting a trail of booleans set elsewhere:
// an approved attempt exists for this exact path, the path is absent NOW, the
// ledger holds a `deleted` tombstone for it with restoration prohibited, and
// the generation is the one that deletion produced.
//
// Anything missing leaves no record, so a denial, a timeout, a cancellation, a
// stale identity, a tool failure, an unapproved removal, an absence caused by
// something else, or a move-source tombstone can never become authority.
func promoteFulfilledDeletion(ctx *AgentContext, canonical string) {
	if ctx == nil {
		return
	}
	ctx.mu.Lock()
	attempt := ctx.deletionAttempts[canonical]
	ctx.mu.Unlock()
	if attempt == nil || !attempt.Removed {
		return
	}
	if _, err := os.Lstat(canonical); !os.IsNotExist(err) {
		return // still there: nothing was fulfilled
	}
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[canonical]
	var ok bool
	var gen int
	if d != nil {
		ok = d.Tombstoned && d.TombstoneReason == "deleted" && d.RestoreProhibited
		gen = d.Generation
	}
	ctx.LedgerMu.Unlock()
	if !ok {
		return
	}
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	if ctx.fulfilledDeletions == nil {
		ctx.fulfilledDeletions = map[string]*fulfilledDeletion{}
	}
	ctx.fulfilledDeletions[canonical] = &fulfilledDeletion{
		CallID: attempt.CallID, Canonical: canonical, Kind: attempt.Target.Kind,
		SHA256: attempt.Target.SHA256, LinkText: attempt.Target.LinkText,
		Generation: gen,
	}
	delete(ctx.deletionAttempts, canonical)
}

// permCallIDFor returns the tool-call id the outstanding approval was granted
// under. The grant is consumed before this is read, so it reports the id the
// approval carried rather than guessing from the turn.
func permCallIDFor(ctx *AgentContext) string {
	if ctx == nil {
		return ""
	}
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	return ctx.lastDeleteCallID
}

// fulfilledDeletionFor returns the record for a path, if the user approved that
// exact deletion and the system carried it out.
func fulfilledDeletionFor(ctx *AgentContext, canonical string) (*fulfilledDeletion, bool) {
	if ctx == nil {
		return nil, false
	}
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	f, ok := ctx.fulfilledDeletions[canonical]
	return f, ok
}
