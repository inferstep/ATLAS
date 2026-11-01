// The read-only tools: read_file, outline_file, search_files, list_directory and find_file.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

func readFileTool() *ToolDef {
	return &ToolDef{
		Name:        "read_file",
		Effect:      ToolEffectReadOnly,
		Description: "Read the contents of a file. Returns numbered lines. Use offset and limit for large files.",
		InputSchema: ReadFileInput{},
		ReadOnly:    true,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input ReadFileInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}

			// Empty path → resolves to the working dir, which is a
			// directory, which fails with a confusing error the model
			// can't recover from. Reject early with a hint at how to
			// discover the file.
			if strings.TrimSpace(input.Path) == "" {
				return &ToolResult{
					Success: false,
					Error:   "read_file: path cannot be empty. Call list_directory with path \".\" to see what files exist, or find_file with a name regex (e.g. \"snake_game\\.py\").",
				}, nil
			}

			path := resolveAgentPath(ctx, input.Path)

			data, err := readRegularFile(path)
			if err != nil {
				return nil, fmt.Errorf("cannot read %s: %w", input.Path, err)
			}

			// Binary-file guard: a compiled binary, image or archive
			// read as text gives the model bytes it cannot use, and it
			// then gives up or loops. Point it at the inspection tools
			// instead of returning the raw bytes.
			if isBinaryContent(data) {
				return &ToolResult{
					Success: false,
					Error: fmt.Sprintf("read_file: %q is a binary file, not text — reading it as text is not useful. Inspect it with run_command instead: `strings %q` (printable strings), `readelf -a %q` or `objdump -d %q` (ELF headers / disassembly), `nm %q` (symbols), `file %q` (type), or `xxd %q | head` (hex dump).",
						input.Path, input.Path, input.Path, input.Path, input.Path, input.Path, input.Path),
				}, nil
			}

			lines := strings.Split(string(data), "\n")
			totalLines := len(lines)

			start, end := readFileWindow(input, totalLines)

			content := numberedReadFileLines(lines, start, end)
			content, truncated, shownEnd := capReadFileBytes(content, input, data, start, end, totalLines)
			// On a truncated read the model only
			// saw the head, so record ONLY that — otherwise the redundant-read
			// dedup later asserts the whole file is in context and short-
			// circuits a real re-read. Untruncated reads record the full bytes.
			recorded := string(data)
			if truncated {
				recorded = strings.Join(lines[start:shownEnd], "\n")
			}
			ctx.RecordFileRead(path, recorded)
			ctx.RecordBodyRead(path, start+1, shownEnd, totalLines)

			content = withCallGraphFooter(ctx, input, data, start, content)

			out := ReadFileOutput{
				Content:    content,
				TotalLines: totalLines,
				StartLine:  start + 1,
				EndLine:    shownEnd, // actual last line returned, not the pre-truncation end
			}
			outBytes, _ := json.Marshal(out)
			return &ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

// readFileWindow is the range of lines a read returns: from the offset, for at
// most the limit, within the file.
func readFileWindow(input ReadFileInput, totalLines int) (int, int) {
	start := 0
	if input.Offset != nil {
		start = *input.Offset
		if start < 0 {
			start = 0
		}
		if start > totalLines {
			start = totalLines
		}
	}

	end := totalLines
	if input.Limit != nil {
		end = start + *input.Limit
		if end > totalLines {
			end = totalLines
		}
	}
	return start, end
}

// numberedReadFileLines renders lines start to end in cat -n form, under a
// header saying the numbers are not in the file.
func numberedReadFileLines(lines []string, start, end int) string {
	// Build numbered output, in cat -n format.
	//
	// The "N<tab>" prefix is presentation, and the payload has to say so,
	// or a model reasonably concludes the file itself is tab-delimited and
	// parses every line as `line.split('\t')[1]`. Say it once, up front,
	// for any read a program might be written against.
	var sb strings.Builder
	sb.WriteString("(Line numbers and the tab after them are added by " +
		"read_file for reference. They are NOT in the file — code that " +
		"reads this file must not parse them.)\n")
	for i := start; i < end; i++ {
		fmt.Fprintf(&sb, "%d\t%s\n", i+1, lines[i])
	}
	return sb.String()
}

// capReadFileBytes cuts a read's text at a line boundary within the byte cap
// and appends the notice that says so. It returns the text, whether it was
// cut, and the line after the last one it still shows.
func capReadFileBytes(content string, input ReadFileInput, data []byte, start, end, totalLines int) (string, bool, int) {
	// Cap the returned bytes so one huge read cannot blow the model's
	// context window: a single message that alone overflows the window
	// is something the force-trim retry cannot fix. The cap is
	// unconditional, because a line `limit` does not bound bytes (100k
	// lines of G-code is enormous). Truncate at a line boundary and
	// append the notice from readFileTruncationNotice.
	truncated := false
	shownEnd := end // 1-past the last line actually returned
	if len(content) > maxReadFileBytes {
		cut := maxReadFileBytes
		if nl := strings.LastIndexByte(content[:cut], '\n'); nl > 0 {
			cut = nl + 1
		}
		// content opens with the one-line "line numbers are added"
		// header, so its newline count is one MORE than the number of
		// numbered rows actually returned. Counting the header made
		// the window label overshoot by a line and made shownEnd
		// record a line the model never saw.
		shown := strings.Count(content[:cut], "\n") - 1
		if shown < 0 {
			shown = 0
		}
		content = content[:cut] + readFileTruncationNotice(
			start, shown, totalLines, len(data), fileIsSourceCode(input.Path))
		truncated = true
		shownEnd = start + shown
		if shownEnd > totalLines {
			shownEnd = totalLines
		}
	}
	return content, truncated, shownEnd
}

// withCallGraphFooter appends the file's call graph to a .py read that starts
// at the top of the file.
func withCallGraphFooter(ctx *AgentContext, input ReadFileInput, data []byte, start int, content string) string {
	// Call-graph footer. The model reads a
	// file far more often than it outlines one, so attach the
	// intra-file call edges to a .py read where the localization
	// decision happens. Fire on any read that starts at the top of
	// the file (start == 0) — that covers both a whole-file read and
	// the model's common "offset:0, limit:N" first look; it computes
	// the graph from the full file on disk regardless of the page
	// shown, and skips mid-file pages so a model scrolling a big file
	// doesn't get the footer repeated.
	if start == 0 && strings.HasSuffix(input.Path, ".py") {
		if footer := callGraphFooter(ctx, input.Path, string(data)); footer != "" {
			content += footer
		}
	}
	return content
}

// ---------------------------------------------------------------------------
// outline_file — cheap structural index of a file (names + line ranges, no
// bodies). The surgical-read entry point: outline to see what's in a file
// for a few hundred bytes, then read_file with offset/limit to pull just
// the one function you need. Saves the model from dumping a whole file into
// context (and re-reading it) just to find one bug.
// ---------------------------------------------------------------------------

func outlineFileTool() *ToolDef {
	return &ToolDef{
		Name:   "outline_file",
		Effect: ToolEffectReadOnly,
		Description: "List a file's top-level functions and classes with their " +
			"line ranges. Returns NO code: an outline tells you WHERE something is " +
			"defined and never what it does, so you cannot answer a question, " +
			"diagnose a bug, or judge whether code is correct from one. " +
			"Use it to locate a target inside a file too large to read whole: " +
			"outline_file to find the function, then read_file with offset/limit " +
			"for its lines, then structural_edit (selector function:NAME / " +
			"class:NAME) or edit_file to change it. For a file you can simply " +
			"read, go straight to read_file. " +
			"Python is parsed precisely (tree-sitter, decorator-aware); other " +
			"languages get a best-effort definition scan.",
		InputSchema: OutlineInput{},
		ReadOnly:    true,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input OutlineInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			if strings.TrimSpace(input.Path) == "" {
				return &ToolResult{Success: false,
					Error: "outline_file: path cannot be empty. Pass {\"path\":\"<file>\"}."}, nil
			}
			path := resolveAgentPath(ctx, input.Path)
			data, err := readRegularFile(path)
			if err != nil {
				return nil, fmt.Errorf("cannot read %s: %w", input.Path, err)
			}
			src := string(data)
			totalLines := strings.Count(src, "\n") + 1

			// Prefer the v3 tree-sitter outline for .py (accurate, matches
			// structural_edit selectors). Fall back to a language-agnostic regex
			// scan for everything else and whenever v3 is unavailable.
			var syms []OutlineSymbol
			var regions []EmbeddedRegion
			if strings.HasSuffix(input.Path, ".py") {
				if v3, ok, emb := outlineViaV3(ctx, input.Path, src); ok {
					syms, regions = v3, emb
				}
			}
			engine := "tree-sitter"
			if syms == nil {
				syms = outlineByRegex(input.Path, src)
				engine = "scan"
			}

			ctx.RecordFileRead(path, src)

			out := OutlineOutput{Symbols: syms, Supported: len(syms) > 0, EmbeddedRegions: regions,
				Outline: outlineText(input.Path, totalLines, syms, engine) + embeddedRegionNote(regions)}
			outBytes, _ := json.Marshal(out)
			return &ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

// outlineText renders the outline the model reads: one line per symbol, with
// its call edges when V3 found any.
func outlineText(path string, totalLines int, syms []OutlineSymbol, engine string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s — %d lines, %d symbols (%s)\n",
		path, totalLines, len(syms), engine)
	if len(syms) == 0 {
		sb.WriteString("(no top-level functions/classes found — read_file to view it directly)\n")
	}
	hasGraph := false
	for _, s := range syms {
		fmt.Fprintf(&sb, "L%d-%d\t%s %s\n", s.StartLine, s.EndLine, s.Kind, s.Name)
		if len(s.Calls) > 0 {
			fmt.Fprintf(&sb, "\tcalls: %s\n", strings.Join(s.Calls, ", "))
			hasGraph = true
		}
		if len(s.CalledBy) > 0 {
			fmt.Fprintf(&sb, "\tcalled by: %s\n", strings.Join(s.CalledBy, ", "))
			hasGraph = true
		}
	}
	if hasGraph {
		// Steer the model to use the structure for localization: a wrong
		// value a function returns may come from a function it calls, not
		// from the function itself.
		sb.WriteString("\nNote: if a function returns a wrong value, the bug may be in a function it `calls`, not in the function itself — follow the call edges to the root cause before editing.\n")
	}
	return sb.String()
}

// callGraphFooter renders a compact intra-file call-graph summary for a
// whole-file read, reusing the same v3 outline (which carries calls/called_by).
// Returns "" when there are no edges, so a file with no internal calls doesn't
// get a noisy empty section.
func callGraphFooter(ctx *AgentContext, path, source string) string {
	syms, ok, _ := outlineViaV3(ctx, path, source)
	if !ok {
		return ""
	}
	var sb strings.Builder
	hasEdges := false
	for _, s := range syms {
		if len(s.Calls) == 0 && len(s.CalledBy) == 0 {
			continue
		}
		if !hasEdges {
			// The footer is concatenated onto the file content the model
			// reads, so without an explicit boundary it looks like the last
			// lines of the file, and a model can anchor edit_file's old_str on
			// text that is not on disk. Say where the file ends and that what
			// follows is not it.
			fmt.Fprintf(&sb, "\n\n--- end of %s ---\n", path)
			sb.WriteString("The lines below are ATLAS analysis, NOT part of the file. " +
				"Never copy them into old_str.\n")
			sb.WriteString("## Call graph (within this file)\n")
			hasEdges = true
		}
		sb.WriteString("- " + s.Name)
		if len(s.Calls) > 0 {
			sb.WriteString(" calls: " + strings.Join(s.Calls, ", "))
		}
		if len(s.CalledBy) > 0 {
			sb.WriteString("; called by: " + strings.Join(s.CalledBy, ", "))
		}
		sb.WriteString("\n")
	}
	if hasEdges {
		sb.WriteString("If a function returns a wrong value, the bug may be in a function it calls — follow the edges to the root cause before editing.\n")
	}
	return sb.String()
}

// outlineViaV3 asks v3-service for a tree-sitter outline. Returns (nil,false)
// on any failure so the caller can fall back to the regex scan.
func outlineViaV3(ctx *AgentContext, path, source string) ([]OutlineSymbol, bool, []EmbeddedRegion) {
	if ctx.V3URL == "" {
		return nil, false, nil
	}
	body, _ := json.Marshal(map[string]string{"path": path, "source": source})
	req, err := http.NewRequestWithContext(ctx.Ctx, "POST",
		ctx.V3URL+"/internal/outline", bytes.NewReader(body))
	if err != nil {
		return nil, false, nil
	}
	req.Header.Set(headerContentType, contentTypeJSON)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, false, nil
	}
	var out OutlineOutput
	if json.NewDecoder(serviceAnswer(resp.Body)).Decode(&out) != nil || !out.Supported {
		return nil, false, nil
	}
	return out.Symbols, true, out.EmbeddedRegions
}

// embeddedRegionNote renders the foreign-language regions of a file for the
// outline the model reads.
//
// It names the symbols AND says they are unreachable by selector, because
// naming them alone would invite exactly the call it is meant to prevent.
func embeddedRegionNote(regions []EmbeddedRegion) string {
	if len(regions) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\nEmbedded code (a different language, inside this file):\n")
	for _, r := range regions {
		fmt.Fprintf(&sb, "L%d-%d\t%s in %s\n", r.StartLine, r.EndLine, r.Kind, r.Where)
		if len(r.Symbols) > 0 {
			fmt.Fprintf(&sb, "\tdefines: %s\n", strings.Join(r.Symbols, ", "))
		}
	}
	sb.WriteString("These are NOT selectable. To the host grammar the whole block is one " +
		"string literal or one raw text node, so `structural_edit` cannot address anything " +
		"listed above — a selector naming any of them reports that the symbol does not exist. Change " +
		"this code with replace_lines (the line numbers above), edit_file on one unique line, " +
		"or insert_after.\n")
	return sb.String()
}

// outlineByRegex is the language-agnostic fallback: any line starting (at
// column 0, allowing common keywords) with a definition. Good enough to give
// the model line anchors to read from; not as precise as tree-sitter.
func outlineByRegex(path, source string) []OutlineSymbol {
	type pat struct {
		re   *regexp.Regexp
		kind string
	}
	// Ordered, language-agnostic-ish. Name is capture group 1.
	pats := []pat{
		{regexp.MustCompile(`^(?:async\s+)?def\s+([A-Za-z_]\w*)`), "function"},
		{regexp.MustCompile(`^class\s+([A-Za-z_]\w*)`), "class"},
		{regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)`), "function"},
		{regexp.MustCompile(`^type\s+([A-Za-z_]\w*)`), "type"},
		{regexp.MustCompile(`^(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)`), "function"},
		{regexp.MustCompile(`^(?:export\s+)?class\s+([A-Za-z_$][\w$]*)`), "class"},
	}
	lines := strings.Split(source, "\n")
	var out []OutlineSymbol
	for i, ln := range lines {
		for _, p := range pats {
			if m := p.re.FindStringSubmatch(ln); m != nil {
				out = append(out, OutlineSymbol{
					Name: m[1], Kind: p.kind,
					StartLine: i + 1, EndLine: i + 1, // single-line anchor; body follows
				})
				break
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// search_files
// ---------------------------------------------------------------------------

func searchFilesTool() *ToolDef {
	return &ToolDef{
		Name:        "search_files",
		Effect:      ToolEffectReadOnly,
		Description: "Search for a regex pattern inside file CONTENTS. Returns matching lines with file paths and line numbers. Use glob to filter by filename pattern. To find a file by its name (not contents), use find_file or list_directory instead.",
		InputSchema: SearchFilesInput{},
		ReadOnly:    true,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input SearchFilesInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}

			// Reject empty pattern: same reasoning as find_file. An empty
			// regex matches every line in every file.
			if strings.TrimSpace(input.Pattern) == "" {
				return &ToolResult{
					Success: false,
					Error:   "search_files: pattern cannot be empty. Provide a regex to grep file contents for (e.g. \"def main\" or \"TODO\\(.*\\)\").",
				}, nil
			}

			searchPath := ctx.WorkingDir
			if input.Path != "" {
				searchPath = resolveAgentPath(ctx, input.Path)
			}

			re, err := regexp.Compile(input.Pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid regex: %w", err)
			}

			maxMatches := 200
			matches, skippedCredentials, err := searchFileContents(ctx, input, searchPath, re, maxMatches)

			if err != nil && len(matches) == 0 {
				return nil, fmt.Errorf("search error: %w", err)
			}

			out := SearchFilesOutput{
				Matches:                matches,
				TotalCount:             len(matches),
				Truncated:              len(matches) >= maxMatches,
				SkippedCredentialFiles: skippedCredentials,
			}
			outBytes, _ := json.Marshal(out)
			return &ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

// searchFileContents walks searchPath and collects the lines that match re, up
// to maxMatches, from every file the search may read. It also returns how many
// credential files it left out.
func searchFileContents(ctx *AgentContext, input SearchFilesInput, searchPath string,
	re *regexp.Regexp, maxMatches int) ([]SearchMatch, int, error) {
	var matches []SearchMatch
	skippedCredentials := 0

	err := filepath.WalkDir(searchPath, func(path string, d fs.DirEntry, walkErr error) error {
		read, skipDir := searchReadsEntry(input, d, walkErr)
		if skipDir {
			return filepath.SkipDir
		}
		if !read {
			return nil
		}

		relPath, _ := filepath.Rel(ctx.WorkingDir, path)
		if relPath == "" {
			relPath = path
		}
		// Credential files stay out of model context whichever tool
		// asks. search_files read them while read_file refused.
		if denyReadPathReason(relPath) != "" {
			skippedCredentials++
			return nil
		}

		matches = appendFileMatches(matches, re, relPath, path, maxMatches)

		if len(matches) >= maxMatches {
			return filepath.SkipAll
		}
		return nil
	})
	return matches, skippedCredentials, err
}

// searchReadsEntry says whether the search reads an entry of its walk, and
// whether it passes over the entry's whole folder instead.
func searchReadsEntry(input SearchFilesInput, d fs.DirEntry, walkErr error) (read, skipDir bool) {
	if walkErr != nil {
		return false, false // skip unreadable dirs
	}
	if d.IsDir() {
		if isUnsearchedDir(d.Name()) {
			return false, true
		}
		return false, false
	}
	// A symlink can point outside the workspace. read_file refuses
	// to follow one, and so does search.
	if d.Type()&fs.ModeSymlink != 0 {
		return false, false
	}

	// Apply glob filter
	if input.Glob != "" {
		matched, _ := filepath.Match(input.Glob, d.Name())
		if !matched {
			return false, false
		}
	}

	// Skip binary/large files
	info, err := d.Info()
	if err != nil || info.Size() > 1<<20 { // 1MB max
		return false, false
	}
	return true, false
}

// appendFileMatches adds the lines of one file that match re, and stops once
// there are maxMatches in all. A file it cannot read adds none.
func appendFileMatches(matches []SearchMatch, re *regexp.Regexp, relPath, path string, maxMatches int) []SearchMatch {
	data, err := readRegularFile(path)
	if err != nil {
		return matches
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if re.MatchString(line) {
			matches = append(matches, SearchMatch{
				File:    relPath,
				Line:    lineNum,
				Content: truncateStr(line, 200),
			})
			if len(matches) >= maxMatches {
				break
			}
		}
	}
	return matches
}

// ---------------------------------------------------------------------------
// list_directory
// ---------------------------------------------------------------------------

func listDirectoryTool() *ToolDef {
	return &ToolDef{
		Name:        "list_directory",
		Effect:      ToolEffectReadOnly,
		Description: "List the contents of a directory. Returns file names, types (file/dir/symlink), and sizes.",
		InputSchema: ListDirectoryInput{},
		ReadOnly:    true,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input ListDirectoryInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}

			dirPath := resolveAgentPath(ctx, input.Path)

			entries, err := os.ReadDir(dirPath)
			if err != nil {
				return nil, fmt.Errorf("cannot list %s: %w", input.Path, err)
			}

			var dirEntries []DirEntry
			for _, e := range entries {
				entryType := "file"
				if e.IsDir() {
					entryType = "dir"
				} else if e.Type()&os.ModeSymlink != 0 {
					entryType = "symlink"
				}

				var size int64
				if info, err := e.Info(); err == nil {
					size = info.Size()
				}

				dirEntries = append(dirEntries, DirEntry{
					Name: e.Name(),
					Type: entryType,
					Size: size,
				})
			}

			out := ListDirectoryOutput{
				Entries: dirEntries,
				Path:    dirPath,
			}
			outBytes, _ := json.Marshal(out)
			return &ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

// readFileByteCap returns the byte cap for a single read_file result.
// Derived from the per-slot context so one read cannot overflow it, and
// sized for the worst-case tokenization of ~1 token/char (dense content:
// G-code, minified JS, base64), not the chars/4 average: a fixed byte cap
// that is safe for prose can still overflow the slot with dense content.
// Half the per-slot context in bytes guarantees even 1-token/char content
// uses at most half the window, leaving room for the system prompt, tools,
// and reply; the force-trim retry then handles any residual pressure since
// no single message is huge. The derived value is clamped to [2 KB, 200
// KB]. ATLAS_MAX_READ_BYTES overrides it, unclamped.
func readFileByteCap() int {
	if v := envOr("ATLAS_MAX_READ_BYTES", ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	perSlot := 32768
	if b := conversationTokenBudget(); b > 0 {
		// budget already reserves reply + margin; treat it as a
		// worst-case char cap (1 token/char) and take half for a
		// single read so other context still fits.
		perSlot = b
	}
	// Half the per-slot budget: a single read uses at most half the window
	// even at ~1 token/char. The lower clamp must stay AT OR BELOW that
	// half — a fixed 16 KB floor could exceed a small-context slot's whole
	// budget and re-introduce the overflow this cap exists to prevent.
	// 2 KB is a sane minimum and, since the token budget floors at 4000,
	// never actually raises the derived value.
	byteCap := perSlot / 2
	if byteCap < 2_000 {
		byteCap = 2_000
	}
	if byteCap > 200_000 {
		byteCap = 200_000
	}
	return byteCap
}

var maxReadFileBytes = readFileByteCap()

// isBinaryContent reports whether data looks like a binary (non-text)
// file. A NUL byte in the head is the reliable signal — text encodings
// (UTF-8/ASCII) never contain NUL, while compiled binaries, images, and
// archives are full of them. Scans a bounded head so a large file is cheap.
func isBinaryContent(data []byte) bool {
	// UTF-16 and UTF-32 text legitimately contains NUL bytes; a Unicode
	// BOM identifies it as text, not binary. Checked: the UTF-16 LE/BE
	// and UTF-8 BOMs (a UTF-32 LE BOM starts with the UTF-16 LE bytes;
	// UTF-32 BE is not recognized).
	if len(data) >= 2 {
		b0, b1 := data[0], data[1]
		if (b0 == 0xFF && b1 == 0xFE) || (b0 == 0xFE && b1 == 0xFF) { // UTF-16 LE/BE
			return false
		}
	}
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF { // UTF-8 BOM
		return false
	}
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// readFileTruncationNotice explains a read the byte cap cut short.
//
// The advice depends on the kind of file, and the label always names the real
// window, because "the first N" at another offset makes pagination look broken.
//
// A DATA file is meant to be opened by the program at runtime: advice to process
// it with shell pipelines points the model at stdin, and the caller then runs
// `python solve.py` with no stdin and gets 0.
//
// SOURCE code gets where the window is and how to find a symbol: a default read
// can show a decoy and not the routine the request describes, and telling the
// model that the head is enough keeps it from looking further.
func readFileTruncationNotice(start, shown, totalLines, totalBytes int, isSource bool) string {
	where := fmt.Sprintf("lines %d-%d of %d (%d bytes total)",
		start+1, start+shown, totalLines, totalBytes)
	if isSource {
		return fmt.Sprintf(
			"\n... [read_file truncated: showing %s. The rest of this file is NOT "+
				"shown, and what you are looking for may be in it. To find a symbol "+
				"use `search_files` or `outline_file` \u2014 do not assume the part you "+
				"can see is the part the request is about. To read a different range, "+
				"read again with offset/limit.]",
			where)
	}
	return fmt.Sprintf(
		"\n... [read_file truncated: showing %s. If you are writing code that reads "+
			"this file when it runs, you do not need the rest here \u2014 have your "+
			"program open it. To look at a different part of the file, read it again "+
			"with offset/limit.]",
		where)
}

// ---------------------------------------------------------------------------
// find_file — locate files by NAME (vs search_files which greps contents).
// Separate from search_files, which greps contents: a search for a filename
// there finds nothing, and the model concludes the file does not exist.
// ---------------------------------------------------------------------------

func findFileTool() *ToolDef {
	return &ToolDef{
		Name:        "find_file",
		Effect:      ToolEffectReadOnly,
		Description: "Find files by NAME using a regex against the filename or relative path. Use this to check whether a file exists or to locate it. For searching inside file contents, use search_files instead.",
		InputSchema: FindFileInput{},
		ReadOnly:    true,
		Destructive: false,
		Execute: func(rawInput json.RawMessage, ctx *AgentContext) (*ToolResult, error) {
			var input FindFileInput
			if err := json.Unmarshal(rawInput, &input); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}

			// Reject empty pattern: it matches every filename, returns the
			// 200-match cap full of unrelated files, and confuses the model
			// into thinking it found nothing useful.
			if strings.TrimSpace(input.Pattern) == "" {
				// A model can put the filename in `path` and leave `pattern`
				// empty. The generic "pattern cannot be empty" is correct but
				// never mentions the argument the model actually filled in. When
				// `path` looks like a filename rather than a directory, hand
				// back the exact call.
				if hint := findFileArgSwapHint(input.Path); hint != "" {
					return &ToolResult{Success: false, Error: hint}, nil
				}
				return &ToolResult{
					Success: false,
					Error:   "find_file: pattern cannot be empty. Provide a regex matching the filename you want to locate (e.g. \"snake_game\\.py\" or \"^main\\.\").",
				}, nil
			}

			searchPath := ctx.WorkingDir
			if input.Path != "" {
				searchPath = resolveAgentPath(ctx, input.Path)
			}

			re, err := regexp.Compile(input.Pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid regex: %w", err)
			}

			matches, err := findFilesByName(ctx, searchPath, re)
			if err != nil && len(matches) == 0 {
				return nil, fmt.Errorf("find error: %w", err)
			}

			out := FindFileOutput{
				Matches:    matches,
				TotalCount: len(matches),
				Truncated:  len(matches) >= maxFindFileMatches,
			}
			outBytes, _ := json.Marshal(out)
			return &ToolResult{Success: true, Data: outBytes}, nil
		},
	}
}

// maxFindFileMatches caps one find_file answer.
const maxFindFileMatches = 200

// findFilesByName walks searchPath for files whose name or workspace-relative
// path matches re, up to maxFindFileMatches.
func findFilesByName(ctx *AgentContext, searchPath string, re *regexp.Regexp) ([]FindFileMatch, error) {
	var matches []FindFileMatch
	err := filepath.WalkDir(searchPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if isUnsearchedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		relPath, _ := filepath.Rel(ctx.WorkingDir, path)
		if relPath == "" {
			relPath = path
		}
		if re.MatchString(d.Name()) || re.MatchString(relPath) {
			matches = append(matches, FindFileMatch{Path: relPath, Name: d.Name()})
			if len(matches) >= maxFindFileMatches {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return matches, err
}

// isUnsearchedDir names the folders search_files and find_file never enter.
func isUnsearchedDir(name string) bool {
	return name == ".git" || name == "node_modules" || name == "__pycache__" || name == ".next" || name == "target"
}

// resolveAgentPath is the path resolver every tool handler should
// use. It first translates host-side absolute paths into the
// container path (when HostWorkingDir is set and the input falls
// inside that prefix), then resolves the result against
// ctx.WorkingDir. This is what makes the agent forgiving when the
// user pastes "/home/isaac/snake/app.py" into a prompt — the model
// copies the absolute path, the proxy rewrites it to /workspace/app.py,
// and read_file actually finds the file.
// redundantReadShortCircuit returns a compact synthetic result when the
// model asks to read a file it has ALREADY read this session and the
// content on disk is unchanged. A weak model frequently re-reads the same
// file several times before acting; each re-read re-injects the full file
// into the conversation and — on sliding-window-attention models that
// can't reuse llama.cpp's KV cache — forces a full re-encode of the whole
// prompt, costing tens of seconds for zero new information. Serving a
// short "already in context, act now" pointer keeps the working set small
// and steers toward the edit.
//
// Only whole-file reads short-circuit (a paged read with offset/limit is
// the model deliberately fetching a different span, so let it through),
// and only when the on-disk content byte-matches what was already shown
// (so a re-read after an edit still returns the new content). Returns nil
// to mean "no short-circuit — execute the read normally." Model-agnostic:
// keys off whether this exact content was already served, not the model.
func redundantReadShortCircuit(name string, args json.RawMessage, ctx *AgentContext) *ToolResult {
	if name != "read_file" {
		return nil
	}
	if envOr("ATLAS_DEDUP_READS", "1") == "0" {
		return nil
	}
	var input ReadFileInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil
	}
	if input.Offset != nil || input.Limit != nil || strings.TrimSpace(input.Path) == "" {
		return nil
	}
	path, err := resolveWorkspacePath(ctx, input.Path)
	if err != nil {
		return &ToolResult{Success: false, Error: "read_file: " + err.Error()}
	}
	ctx.mu.Lock()
	prev, ok := ctx.FilesRead[path]
	cacheEntries := len(ctx.FilesRead)
	ctx.mu.Unlock()
	if !ok {
		// Diagnostic: a re-read that SHOULD have been cached but wasn't.
		// Logged only when some other entry exists (first read of a file
		// is the normal case and not worth a line).
		if cacheEntries > 0 {
			log.Printf("[read-dedup] no cache entry for %q (have %d entries) — serving real read", truncateStr(path, 240), cacheEntries)
		}
		return nil
	}
	data, _, err := readWorkspaceFile(ctx, input.Path)
	if err != nil || string(data) != prev {
		if err == nil {
			log.Printf("[read-dedup] %q changed on disk since last read (%dB -> %dB) — serving real read", truncateStr(path, 240), len(prev), len(data))
		}
		return nil // changed or unreadable — let the real read run
	}
	// CRITICAL: only short-circuit to a pointer if the content is STILL in
	// the live conversation. After conversation trimming drops the original
	// read, "its content is above" becomes a lie — the model has nothing,
	// edits blind, and (if weak) hallucinates symbols/lines that aren't in
	// the file. When the content has been trimmed out, return nil so the
	// real read re-serves the full file. Verified failure mode: a model editing
	// function:count_items / old_str="return len(items)" against a file
	// containing neither, reasoning "I don't see the file content."
	if !fileContentInContext(ctx, prev) {
		log.Printf("[read-dedup] %q content was trimmed from context — re-serving full read", truncateStr(path, 240))
		return nil
	}
	out := ReadFileOutput{
		Content:    fmt.Sprintf("(You already read %s earlier in this session and it has not changed — its full content is above in the conversation. Do not read it again. Make your edit now with structural_edit or edit_file.)", input.Path),
		TotalLines: strings.Count(prev, "\n") + 1,
		StartLine:  1,
		EndLine:    strings.Count(prev, "\n") + 1,
	}
	b, _ := json.Marshal(out)
	return &ToolResult{Success: true, Data: b}
}

// fileContentInContext reports whether a file's content is still present in
// the live (post-trim) conversation. Conservative: an empty/short-content file
// probes as "present" (nothing to lose).
//
// The probe must survive JSON escaping. Tool results are stored in the
// conversation via ToolResult.MarshalText (json.Marshal), so the file content
// lives in ctx.Messages with `"`→`\"`, newlines→`\n`, tabs→`\t`. A raw line
// would fail for any file whose longest line holds a double quote, giving a false
// "trimmed" verdict that makes the dedup re-serve the whole file every read and
// the model loop on read_file. So probe with the longest maximal run of
// characters JSON does NOT escape (no `"`, `\`, or \n/\r/\t); that run is byte-
// identical in both the raw file and the escaped conversation copy.
func fileContentInContext(ctx *AgentContext, content string) bool {
	probe := ""
	for _, run := range strings.FieldsFunc(content, func(r rune) bool {
		return r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t'
	}) {
		t := strings.TrimSpace(run)
		if len(t) > len(probe) {
			probe = t
		}
	}
	if len(probe) < 12 {
		return true // too short to probe reliably; don't churn re-reads
	}
	for _, m := range ctx.Messages {
		if strings.Contains(m.Content, probe) {
			return true
		}
	}
	return false
}
