package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sort"
	"strings"
)

// When V3 fails on a write -- it runs out of its time budget or cannot be
// reached -- the model's own bytes land, after the gates every write passes
// (ADR 0004). The tool result says so at that moment. The run's final summary
// did not, so a user who read only the summary could not tell which files V3
// never checked. Decision (2026-09-27): keep writing, and name those files in
// the final summary.
//
// A lens that cannot score is not this case: the run stops instead
// (lens_required.go).

// v3UncheckedWrite is one write that V3 did not check: why, and the bytes.
type v3UncheckedWrite struct {
	reason string
	hash   string // sha256 of the bytes written
}

// v3FailureReason says why V3 did not check a write, for the summary.
func v3FailureReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "V3 ran out of time"
	}
	return "V3 was unavailable"
}

// noteV3Unchecked records that content, about to be written to path, was not
// checked by V3. The summary names the file only while these exact bytes are
// on disk: a write that V3 checked, or any other change, replaces them.
func (c *AgentContext) noteV3Unchecked(path, reason, content string) {
	if c == nil {
		return
	}
	sum := sha256.Sum256([]byte(content))
	c.v3UncheckedMu.Lock()
	defer c.v3UncheckedMu.Unlock()
	if c.v3Unchecked == nil {
		c.v3Unchecked = map[string]v3UncheckedWrite{}
	}
	c.v3Unchecked[ledgerKey(c, path)] = v3UncheckedWrite{reason: reason, hash: hex.EncodeToString(sum[:])}
}

// clearV3Unchecked forgets path: V3 answered for its latest write.
func (c *AgentContext) clearV3Unchecked(path string) {
	if c == nil {
		return
	}
	c.v3UncheckedMu.Lock()
	defer c.v3UncheckedMu.Unlock()
	delete(c.v3Unchecked, ledgerKey(c, path))
}

// v3FallbackNote is the summary's account of the files whose bytes on disk
// are still ones V3 did not check, or "" when there are none.
func v3FallbackNote(ctx *AgentContext) string {
	if ctx == nil {
		return ""
	}
	ctx.v3UncheckedMu.Lock()
	writes := make(map[string]v3UncheckedWrite, len(ctx.v3Unchecked))
	for k, w := range ctx.v3Unchecked {
		writes[k] = w
	}
	ctx.v3UncheckedMu.Unlock()

	var named []string
	for key, w := range writes {
		if fileSHA256(ctx, key) != w.hash {
			continue
		}
		rel := key
		if r, err := filepath.Rel(ctx.WorkingDir, key); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		named = append(named, rel+" ("+w.reason+")")
	}
	if len(named) == 0 {
		return ""
	}
	sort.Strings(named)
	return "\n\nV3 did not check these files: " + strings.Join(named, ", ") +
		". The model's own version of each file was written."
}
