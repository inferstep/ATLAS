// The arguments each tool takes from the model and the output it returns, as
// the JSON the model reads and writes. write_file's two types are in types.go.

package main

// -- read_file --

type ReadFileInput struct {
	Path   string `json:"path"`
	Offset *int   `json:"offset,omitempty"` // line offset (0-based)
	Limit  *int   `json:"limit,omitempty"`  // max lines to read
}

type ReadFileOutput struct {
	Content    string `json:"content"`
	TotalLines int    `json:"total_lines"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
}

// -- outline_file --

type OutlineInput struct {
	Path string `json:"path"`
}

type OutlineSymbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	// Intra-file call-graph neighborhood (from v3-service's
	// outline of a Python file). Calls = functions this symbol
	// invokes; CalledBy = functions that invoke it. Lets the model follow a
	// symptom to its callee-rooted cause instead of editing where the
	// symptom surfaces.
	Calls    []string `json:"calls,omitempty"`
	CalledBy []string `json:"called_by,omitempty"`
}

type OutlineOutput struct {
	Symbols   []OutlineSymbol `json:"symbols"`
	Supported bool            `json:"supported"`
	// Outline is the rendered human-readable listing (header, L<start>-<end>
	// lines, call edges). The model reads this; Symbols carries the same
	// data structurally.
	Outline string `json:"outline,omitempty"`
	// Regions holding code in ANOTHER language — <script>/<style> blocks,
	// including inside Python string literals. The host grammar cannot see
	// into a string, so these are exactly the symbols a selector CANNOT
	// reach, and naming them is what stops the model reaching for them.
	EmbeddedRegions []EmbeddedRegion `json:"embedded_regions"`
}

// EmbeddedRegion is one foreign-language block inside a file.
type EmbeddedRegion struct {
	Where     string   `json:"where"`
	Kind      string   `json:"kind"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Symbols   []string `json:"symbols"`
}

// -- edit_file --

type EditFileInput struct {
	Path       string `json:"path"`
	OldStr     string `json:"old_str"`
	NewStr     string `json:"new_str"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// InsertAfterInput places new lines after a line the model NAMES rather than
// reproduces. read_file already returns "N<tab>content", so the model cites a
// number it can see instead of transcribing an anchor byte-for-byte — the step
// that measurably fails on long spans.
type InsertAfterInput struct {
	Path string `json:"path"`
	// 0 inserts at the top of the file; N inserts after line N (1-based).
	Line    int    `json:"line"`
	Content string `json:"content"`
}

// ReplaceLinesInput is insert_after's rationale extended to REPLACEMENT, which
// is where the verbatim burden actually bites. edit_file needs the old text
// reproduced byte-for-byte; on a 9-13 line span this model corrupts one token
// somewhere in it essentially every time. Here the address is a pair of numbers
// read_file already printed.
//
// ExpectedFirstLine/ExpectedLastLine are the safety belt, and they are not
// optional. A line range degrades catastrophically and SILENTLY: an off-by-one
// still applies cleanly and produces plausible corruption, where a bad anchor
// merely fails to match. Every surveyed implementation that ships a line-range
// replace carries some form of this assertion. One line each is the length
// regime this model is reliable in.
type ReplaceLinesInput struct {
	Path string `json:"path"`
	// 1-based, inclusive on both ends — the convention read_file prints.
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
	// Verbatim text of the first and last line being replaced, without the
	// "N<tab>" display prefix. Whitespace-trimmed before comparison.
	ExpectedFirstLine string `json:"expected_first_line"`
	ExpectedLastLine  string `json:"expected_last_line"`
	Content           string `json:"content"`
}

type EditFileOutput struct {
	OK           bool   `json:"ok"`
	DiffPreview  string `json:"diff_preview,omitempty"`
	LinesAdded   int    `json:"lines_added,omitempty"`
	LinesRemoved int    `json:"lines_removed,omitempty"`
}

// -- structural_edit --
//
// Friendly-selector structural edits via tree-sitter. Replaces a single named
// node (function, class, type, HTML element) with new content. The selector
// grammar is per-language and intentionally narrow, because a model asked for
// raw tree-sitter s-expressions gets the intended match wrong more often than
// right.
//
//   Selectors:
//     python:     function:NAME, class:NAME (decorator-aware: replaces
//                 the decorated_definition wrapper when present)
//     html:       <tag> (top-level tag-name match)
//     go:         function:NAME (func or method), type:NAME
//     javascript: function:NAME, class:NAME
//     typescript: function:NAME, class:NAME

type StructuralEditInput struct {
	Path     string `json:"path"`
	Selector string `json:"selector"`
	Content  string `json:"content"`
}

type StructuralEditOutput struct {
	OK       bool   `json:"ok"`
	Selector string `json:"selector"`
	Language string `json:"language,omitempty"`
	BytesOld int    `json:"bytes_old,omitempty"`
	BytesNew int    `json:"bytes_new,omitempty"`
	// Set when the spliced file no longer parses. The edit still landed --
	// this tool does not refuse after the rename -- but the model is told,
	// the same way write_file tells it.
	Warning string `json:"warning,omitempty"`
}

// -- delete_file --

type DeleteFileInput struct {
	Path string `json:"path"`
}

type DeleteFileOutput struct {
	Deleted bool `json:"deleted"`
}

// -- move_file --
//
// Relocate or rename a file within the workspace. A pure move/rename is not a
// content change, so it does NOT go through the V3 / surgical-edit gate the way
// write_file/edit_file do — it exists so "reorganize the files" (e.g. move
// index.html into templates/) is a single tool call instead of a
// read→write→delete dance the model can't reliably compose.

type MoveFileInput struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

type MoveFileOutput struct {
	Moved       bool   `json:"moved"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// -- run_command --

type RunCommandInput struct {
	Command string `json:"command"`
	Timeout *int   `json:"timeout,omitempty"` // seconds, default 30
	Cwd     string `json:"cwd,omitempty"`
}

type RunCommandOutput struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`

	// TimedOut is the executor's own structural answer, decoded rather than
	// read out of the stderr text. A killed command's exit status is whatever
	// the signal left behind, so a caller that only looked at the code would
	// be guessing about the one case where guessing is worst.
	TimedOut bool `json:"timed_out,omitempty"`
	// OutputTruncated says the transcript this result carries is not the whole
	// one. It does not change the exit status, and is recorded so no consumer
	// has to infer completeness from length.
	OutputTruncated bool `json:"output_truncated,omitempty"`
	// Outcome is HOW the command ended, from the executor's closed vocabulary.
	// A command stopped at a resource ceiling exits non-zero exactly like a
	// failing one, so the exit code alone cannot tell a reader whether the
	// command answered the question it was asked. Absent or unrecognised is
	// read as unclassified, never as completed.
	Outcome ExecutionOutcome `json:"outcome,omitempty"`
	// PeakMemoryBytes is what the command and everything it spawned actually
	// reached. Telemetry only: no decision reads it, and it is never shown to
	// the model.
	PeakMemoryBytes int `json:"-"`
}

// -- background commands --
//
// Three tools wrap the sandbox /jobs/* endpoints so the model can
// run a server, probe it from another command, and clean up. Used
// for the "verify HTTP routes" workflow that foreground run_command
// can't satisfy (server doesn't exit).

type RunBackgroundInput struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"`
	// SettleMs gives the process time to print initial output before
	// we return — typical use is "wait 1500ms for the dev server's
	// startup banner so the model can confirm it bound the port."
	// Default 1500. Capped at 10000 server-side.
	SettleMs *int `json:"settle_ms,omitempty"`
}

type RunBackgroundOutput struct {
	JobID    string   `json:"job_id"`
	PID      int      `json:"pid"`
	Stdout   []string `json:"stdout"` // initial output captured during settle
	Stderr   []string `json:"stderr"`
	Running  bool     `json:"running"` // false if the process exited within settle window
	ExitCode *int     `json:"exit_code,omitempty"`
}

type TailBackgroundInput struct {
	JobID string `json:"job_id"`
	Lines *int   `json:"lines,omitempty"` // default 50, max 500
}

type TailBackgroundOutput struct {
	JobID      string   `json:"job_id"`
	Running    bool     `json:"running"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	Stdout     []string `json:"stdout"`
	Stderr     []string `json:"stderr"`
	ElapsedSec float64  `json:"elapsed_sec"`
	Command    string   `json:"command"`
}

type StopBackgroundInput struct {
	JobID string `json:"job_id"`
}

type StopBackgroundOutput struct {
	JobID    string   `json:"job_id"`
	Killed   bool     `json:"killed"`
	ExitCode *int     `json:"exit_code,omitempty"`
	Stdout   []string `json:"stdout"`
	Stderr   []string `json:"stderr"`
}

// -- search_files --

type SearchFilesInput struct {
	Pattern string `json:"pattern"`        // regex pattern
	Path    string `json:"path,omitempty"` // directory to search in
	Glob    string `json:"glob,omitempty"` // file glob filter (e.g., "*.go")
}

type SearchMatch struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Content string `json:"content"`
}

type SearchFilesOutput struct {
	Matches    []SearchMatch `json:"matches"`
	TotalCount int           `json:"total_count"`
	Truncated  bool          `json:"truncated,omitempty"`
	// SkippedCredentialFiles counts files not searched because they hold
	// credentials (see denyReadPathReason).
	SkippedCredentialFiles int `json:"skipped_credential_files,omitempty"`
}

// -- find_file --

type FindFileInput struct {
	Pattern string `json:"pattern"`        // regex matched against filename or relative path
	Path    string `json:"path,omitempty"` // directory to search in (defaults to working dir)
}

type FindFileMatch struct {
	Path string `json:"path"` // relative path from working dir
	Name string `json:"name"` // basename
}

type FindFileOutput struct {
	Matches    []FindFileMatch `json:"matches"`
	TotalCount int             `json:"total_count"`
	Truncated  bool            `json:"truncated,omitempty"`
}

// -- list_directory --

type ListDirectoryInput struct {
	Path string `json:"path"`
}

type DirEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file", "dir", "symlink"
	Size int64  `json:"size,omitempty"`
}

type ListDirectoryOutput struct {
	Entries []DirEntry `json:"entries"`
	Path    string     `json:"path"`
}
