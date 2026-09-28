package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A truncated read must say which lines it returned, and must not tell a model
// reading SOURCE that the part it can see is enough.
//
// Measured on smallrung_toml, 0 of 6. executor_server.py is 2026 lines with two
// routines dispatching on `lang`: normalize_language at 208 (aliases only) and
// _syntax_check_impl at 1266 (the one the request describes). A default read
// returns the first 214 lines, so the decoy is visible and the target is not.
// All six sessions edited the decoy. Two paginated with offset=214 and were
// told "showing the first 225 of 2026 lines" -- a false label that makes
// pagination look broken. None called search_files or outline_file, while the
// notice said the rest was unnecessary.
func bigSourceFile(t *testing.T, dir, name string, lines int) string {
	t.Helper()
	var sb strings.Builder
	for i := 1; i <= lines; i++ {
		// Wide enough that the byte cap bites well before the end.
		sb.WriteString(fmt.Sprintf("# line %04d %s\n", i, strings.Repeat("x", 90)))
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readWindow(t *testing.T, ctx *AgentContext, rel string, offset *int) string {
	t.Helper()
	args := map[string]interface{}{"path": rel}
	if offset != nil {
		args["offset"] = *offset
		args["limit"] = 100000 // ask for everything; the byte cap decides
	}
	b, _ := json.Marshal(args)
	res := executeToolCall("read_file", b, ctx)
	if res == nil || !res.Success {
		t.Fatalf("read_file failed: %+v", res)
	}
	var out struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatalf("not a read_file payload: %v", err)
	}
	return out.Content
}

var windowRe = regexp.MustCompile(`showing lines (\d+)-(\d+) of (\d+)`)

func TestATruncatedReadReportsTheWindowItActuallyReturned(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	bigSourceFile(t, dir, "server.py", 2026)

	// --- default read ---
	body := readWindow(t, ctx, "server.py", nil)
	if !strings.Contains(body, "truncated") {
		t.Fatal("fixture is not large enough to truncate")
	}
	m := windowRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("notice does not state the window: %q", tailOf(body))
	}
	if m[1] != "1" {
		t.Errorf("default read claims to start at line %s, want 1", m[1])
	}
	// The label must agree with the last line number actually printed.
	if got, want := lastPrintedLine(t, body), m[2]; got != want {
		t.Errorf("notice says it ends at line %s but the content ends at %s", want, got)
	}

	// --- offset read: the label must move with the offset ---
	off := 214
	body2 := readWindow(t, ctx, "server.py", &off)
	m2 := windowRe.FindStringSubmatch(body2)
	if m2 == nil {
		t.Fatalf("offset read states no window: %q", tailOf(body2))
	}
	if m2[1] != "215" {
		t.Errorf("offset=214 labelled as starting at line %s, want 215 — "+
			"this is the false 'showing the first N' that made pagination "+
			"look broken", m2[1])
	}
	if strings.Contains(body2, "the first") {
		t.Errorf("offset read still calls itself 'the first': %q", tailOf(body2))
	}
	if got, want := lastPrintedLine(t, body2), m2[2]; got != want {
		t.Errorf("offset notice says end %s, content ends %s", want, got)
	}
}

func TestTruncationAdviceMatchesTheKindOfFile(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	bigSourceFile(t, dir, "server.py", 2026)
	bigSourceFile(t, dir, "input.txt", 2026)

	src := readWindow(t, ctx, "server.py", nil)
	if !strings.Contains(src, "search_files") || !strings.Contains(src, "outline_file") {
		t.Errorf("source read does not name the tools that locate a symbol: %q", tailOf(src))
	}
	if strings.Contains(src, "you do not need the rest") {
		t.Errorf("source read still says the head is enough: %q", tailOf(src))
	}

	// The data-file advice was measured to help on the AoC tasks and must
	// survive: there, having the program open the file at runtime IS right.
	data := readWindow(t, ctx, "input.txt", nil)
	if !strings.Contains(data, "have your") || !strings.Contains(data, "program open it") {
		t.Errorf("data read lost the runtime-open advice: %q", tailOf(data))
	}
	if strings.Contains(data, "search_files") {
		t.Errorf("data read was given source advice: %q", tailOf(data))
	}
}

func tailOf(s string) string {
	if i := strings.LastIndex(s, "... ["); i >= 0 {
		return s[i:]
	}
	if len(s) > 300 {
		return s[len(s)-300:]
	}
	return s
}

// lastPrintedLine returns the line number on the last numbered content line.
func lastPrintedLine(t *testing.T, body string) string {
	t.Helper()
	cut := body
	if i := strings.LastIndex(cut, "... ["); i >= 0 {
		cut = cut[:i]
	}
	rows := strings.Split(strings.TrimRight(cut, "\n"), "\n")
	for i := len(rows) - 1; i >= 0; i-- {
		if tab := strings.IndexByte(rows[i], '\t'); tab > 0 {
			if _, err := strconv.Atoi(rows[i][:tab]); err == nil {
				return rows[i][:tab]
			}
		}
	}
	t.Fatalf("no numbered lines in body")
	return ""
}
