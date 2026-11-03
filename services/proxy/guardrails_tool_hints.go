// Notes that help the model repair a tool call that failed or was refused:
// find_file with the filename in the wrong argument, an old_str with a
// character the file does not contain, and a tool that is banned for a file
// after the same call was rejected and sent again unchanged. toolBanNoteFor,
// which adds the file's selectors to that last note, is in guardrails.go.

package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// findFileArgSwapHint catches find_file called with the filename in `path`
// and `pattern` empty, and returns the corrected call.
//
// Observed live: the model sent {"path":"app.py"}, got "pattern cannot be
// empty", and sent {"path":".*app\\.py.*"} — moving the regex into `path`
// rather than into `pattern`, because the error named the field it had left
// blank and not the one it had filled. A `path` that carries an extension and
// no separator is a filename, so say which argument it belongs in.
func findFileArgSwapHint(path string) string {
	p := strings.TrimSpace(path)
	if p == "" || strings.ContainsAny(p, `/\`) {
		return "" // a real directory, or nothing to go on
	}
	ext := filepath.Ext(p)
	if ext == "" || len(ext) > 6 {
		return ""
	}
	// Escape it into the regex the caller meant.
	quoted := regexp.QuoteMeta(p)
	return fmt.Sprintf(
		"find_file: `pattern` is empty and `path` is %q, which is a filename, not a directory. "+
			"`path` is WHERE to search (a directory, default the project root) and `pattern` is a "+
			"regex matching the FILENAME. Retry as: find_file {\"pattern\":%q}",
		p, quoted+"$")
}

// foreignRunes reports characters in old_str that appear NOWHERE in the file,
// restricted to non-ASCII. A mismatch caused by one corrupted character is
// invisible in a diff of two long strings, and every other hint sends the model
// to re-copy text it already copied correctly.
//
// Observed live: the model sent `if(headX === food.x ℘ headY ===food.y)` for a
// line reading `&&`. U+2118 SCRIPT CAPITAL P is not a typo, it is a decode
// artefact, and the rejection it got ("your old_str is 9 lines long") was true
// and useless — the length was not why it failed.
//
// ASCII is excluded deliberately: a wrong ASCII character is an ordinary
// mis-copy the closest-line hint already handles. A non-ASCII rune that the
// file does not contain anywhere is nearly always corruption.
func foreignRunes(oldStr, fileContent string) []rune {
	var out []rune
	seen := map[rune]bool{}
	for _, r := range oldStr {
		if r < 0x80 || seen[r] {
			continue
		}
		seen[r] = true
		if !strings.ContainsRune(fileContent, r) {
			out = append(out, r)
		}
	}
	return out
}

// describeForeignRunes renders foreignRunes for the model: the character, its
// codepoint, and where it sits, so the fix is mechanical rather than a re-copy.
func describeForeignRunes(runes []rune) string {
	parts := make([]string, 0, len(runes))
	for _, r := range runes {
		parts = append(parts, fmt.Sprintf("%q (U+%04X)", r, r))
	}
	return strings.Join(parts, ", ")
}

// toolBanNote tells the model a tool is gone for a file and names what is
// left. Written as a fact rather than a suggestion, because the suggestion
// form was measured to be ignored: an explicit "re-sending will not help,
// use structural_edit" was followed by the identical call on the next turn.
func toolBanNote(tool, path string) string {
	// Never name the tool that was just banned. The alternatives used to be
	// chosen by file extension alone, so banning structural_edit on a .py
	// file produced "structural_edit is no longer available for pricing.py
	// ... Use `structural_edit`" -- the one instruction the model cannot
	// follow. Measured on acceptance task L: the session was handed exactly
	// that and ended repeated_refusal with nothing written to disk.
	structuralFits := false
	if ext := strings.ToLower(filepath.Ext(path)); ext == ".py" || ext == ".html" || ext == ".htm" {
		structuralFits = true
	}
	candidates := []struct{ name, how string }{
		{"structural_edit", "`structural_edit` (the selector of the node you are changing plus its new body — no old_str to reproduce)"},
		{"replace_lines", "`replace_lines` (assert only the first and last line of the range)"},
		{"write_file", "`write_file` with the complete new contents"},
	}
	var alts []string
	for _, c := range candidates {
		if c.name == tool {
			continue
		}
		if c.name == "structural_edit" && !structuralFits {
			continue
		}
		alts = append(alts, c.how)
	}
	return fmt.Sprintf(
		"%s is no longer available for %s in this session: it was sent and rejected unchanged, so it is not a path to a working edit here. Use %s.",
		tool, path, strings.Join(alts, " or "))
}
