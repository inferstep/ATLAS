package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #215: an edit_file old_str that has stopped matching its file, and keeps
// going, is cut while it streams. One that matches is never cut.

// oldStrTarget is a file shaped like smallrung_toml's executor_server.py: a
// comment block the model copied, then kept cycling with read_file line
// numbers (fc8321d run a, rep 1, turn 3: 25,333 characters, 322 s).
func oldStrTarget() string {
	var sb strings.Builder
	sb.WriteString("import subprocess\n\n\ndef _syntax_check_impl(lang, code, workspace, filename=None):\n")
	sb.WriteString("    errors = []\n")
	for _, lang := range []string{"python", "javascript", "typescript", "go", "java", "kotlin", "rust"} {
		fmt.Fprintf(&sb, "    elif lang == %q:\n", lang)
		sb.WriteString("        # We use a simple python script to call the parser.\n")
		sb.WriteString("        # This script is written to a temporary file.\n")
		sb.WriteString("        # Then we run the script.\n")
		sb.WriteString("        # If the script fails, we report the error.\n")
		fmt.Fprintf(&sb, "        result = _run_check([%q, str(fpath)], timeout=5, cwd=workspace)\n", lang)
		sb.WriteString("        if result[\"returncode\"] != 0:\n")
		sb.WriteString("            errors.append(result.get(\"stderr\", \"\").strip())\n\n")
	}
	sb.WriteString("    return errors\n")
	return sb.String()
}

func oldStrWorld(t *testing.T, content string) (*AgentContext, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "executor_server.py"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	return ctx, dir
}

// editCallPrefix is how a streaming edit_file call begins.
func editCallPrefix(path string) string {
	return `{"type":"tool_call","name":"edit_file","args":{"path":"` + path + `","old_str":"`
}

// jsonStringBody is s as the inside of a JSON string, without the quotes.
func jsonStringBody(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// streamThroughWatch feeds a response to the watch the way the stream loop
// does: the whole buffer, checked every 200 bytes once past 600.
func streamThroughWatch(w *oldStrWatch, response string) (cutAt int) {
	last := 0
	for n := 7; n <= len(response); n += 7 {
		if n > 600 && n-last > 200 {
			last = n
			if w.runaway(response[:n]) {
				return n
			}
		}
	}
	return -1
}

func TestAnOldStrThatMatchesIsNeverCut(t *testing.T) {
	file := oldStrTarget()
	// Every line from `errors = []` to the end: over 2,500 characters.
	start := strings.Index(file, "    errors = []")
	long := file[start:]
	if len(long) < 2500 {
		t.Fatalf("fixture old_str is only %d characters", len(long))
	}
	cases := map[string]string{
		"exact":          long,
		"indent drift":   strings.ReplaceAll(long, "        ", "    "),
		"tabs for space": strings.ReplaceAll(long, "    ", "\t"),
		"curly quotes":   strings.ReplaceAll(long, `"`, "\u201c"),
		"read_file line numbers": func() string {
			var sb strings.Builder
			for i, l := range strings.Split(strings.TrimSuffix(long, "\n"), "\n") {
				fmt.Fprintf(&sb, "%d\t%s\n", 5+i, l)
			}
			return sb.String()
		}(),
	}
	for name, oldStr := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, _ := oldStrWorld(t, file)
			w := newOldStrWatch(ctx)
			response := editCallPrefix("executor_server.py") + jsonStringBody(oldStr) + `","new_str":"x"}}`
			if at := streamThroughWatch(w, response); at >= 0 {
				t.Fatalf("a matching old_str was cut at %d of %d bytes (bad line %q)", at, len(response), w.FirstBadLine)
			}
		})
	}
}

func TestACountedOldStrLoopIsCut(t *testing.T) {
	// The smallrung shape: the comment block, copied with read_file line
	// numbers, then copied again and again with the numbers still counting.
	file := oldStrTarget()
	ctx, _ := oldStrWorld(t, file)
	block := []string{
		"        # We use a simple python script to call the parser.",
		"        # This script is written to a temporary file.",
		"        # Then we run the script.",
		"        # If the script fails, we report the error.",
	}
	var old strings.Builder
	n := 1596
	for cycle := 0; cycle < 60; cycle++ {
		for _, l := range block {
			fmt.Fprintf(&old, "%d\t%s\n", n, l)
			n++
		}
	}
	response := editCallPrefix("executor_server.py") + jsonStringBody(old.String())
	w := newOldStrWatch(ctx)
	at := streamThroughWatch(w, response)
	if at < 0 {
		t.Fatalf("a %d-byte counted old_str loop was never cut", len(response))
	}
	// One pass of the block is text in the file (numbers aside); the second
	// pass is not. The cut lands within the run-on allowance after that.
	onePass := len(jsonStringBody(strings.Join(block, "\n")))
	if at > 3*onePass+oldStrRunOn+400 {
		t.Errorf("cut at %d bytes, want soon after the block repeats (~%d)", at, 2*onePass)
	}
	if w.MatchedLines != 4 {
		t.Errorf("matched %d lines, want the 4 of the first pass", w.MatchedLines)
	}
	if !strings.Contains(w.FirstBadLine, "# We use a simple python script") {
		t.Errorf("first bad line %q, want the block's first line again", w.FirstBadLine)
	}
}

func TestAShortMisCopyIsLeftToEditFile(t *testing.T) {
	// One wrong character early, then the old_str runs on for less than the
	// allowance and closes: edit_file's own refusal, with its closest-line
	// hint, is the better answer, so no cut. Checked at every length the
	// value passes through while open, so no check point can miss it.
	file := oldStrTarget()
	ctx, _ := oldStrWorld(t, file)
	w := newOldStrWatch(ctx)
	oldStr := "        # We use a simple pyhton script to call the parser.\n" +
		"        # This script is written to a temporary file.\n" +
		"        # Then we run the script.\n" +
		"        # If the script fails, we report the error.\n        result"
	if len(oldStr) >= oldStrRunOn+20 {
		t.Fatalf("fixture old_str is %d bytes; it must end inside the allowance", len(oldStr))
	}
	body := jsonStringBody(oldStr)
	prefix := editCallPrefix("executor_server.py")
	for n := 1; n <= len(body); n++ {
		if w.runaway(prefix + body[:n]) {
			t.Fatalf("a short mis-copy was cut %d bytes into old_str (bad line %q)", n, w.FirstBadLine)
		}
	}
	// The same mis-copy, run on past the allowance, is cut.
	long := prefix + jsonStringBody(oldStr+strings.Repeat("\n        # and more text the file never had", 12))
	if !newOldStrWatch(ctx).runaway(long) {
		t.Error("the mis-copy run on past the allowance was not cut")
	}
}

func TestAWhitespaceRunIsLeftToTheLoopCut(t *testing.T) {
	// A line that is only whitespace can still become any line under the
	// whitespace-tolerant rule, so the watch never proves it unmatchable.
	file := oldStrTarget()
	ctx, _ := oldStrWorld(t, file)
	w := newOldStrWatch(ctx)
	response := editCallPrefix("executor_server.py") + jsonStringBody("\n"+strings.Repeat("\t", 900))
	if at := streamThroughWatch(w, response); at >= 0 {
		t.Fatalf("a whitespace run was cut by the old_str watch at %d bytes", at)
	}
}

func TestOnlyAnOpenEditFileOldStrIsWatched(t *testing.T) {
	file := oldStrTarget()
	ctx, _ := oldStrWorld(t, file)
	junk := jsonStringBody(strings.Repeat("no such text anywhere\n", 80))
	for name, response := range map[string]string{
		"write_file content": `{"type":"tool_call","name":"write_file","args":{"path":"executor_server.py","content":"` + junk,
		"structural_edit":    `{"type":"tool_call","name":"structural_edit","args":{"path":"executor_server.py","selector":"function:x","content":"` + junk,
		"new_str":            editCallPrefix("executor_server.py") + `import subprocess","new_str":"` + junk,
		"missing file":       editCallPrefix("nope.py") + junk,
	} {
		t.Run(name, func(t *testing.T) {
			if at := streamThroughWatch(newOldStrWatch(ctx), response); at >= 0 {
				t.Fatalf("cut at %d bytes", at)
			}
		})
	}
}

func TestOpenOldStrDecodesWhatArrived(t *testing.T) {
	cases := []struct {
		stream, want string
		open         bool
	}{
		{editCallPrefix("a.py") + `line one\nline \"two\"\t\u00e9`, "line one\nline \"two\"\t\u00e9", true},
		{editCallPrefix("a.py") + `half an escape \`, "half an escape ", true},
		{editCallPrefix("a.py") + `half a code \u00`, "half a code ", true},
		{editCallPrefix("a.py") + `closed","new_str":"x`, "", false},
		{`{"type":"tool_call","name":"edit_file","args":{"path":"a.py"`, "", false},
	}
	for _, c := range cases {
		got, open := openOldStr(c.stream)
		if got != c.want || open != c.open {
			t.Errorf("openOldStr(%q) = %q, %v; want %q, %v", c.stream, got, open, c.want, c.open)
		}
	}
}

func TestTheStreamIsCutAndTheModelIsToldWhere(t *testing.T) {
	file := oldStrTarget()
	ctx, _ := oldStrWorld(t, file)
	var old strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&old, "%d\t        # We use a simple python script to call the parser.\n", 1596+i)
	}
	response := editCallPrefix("executor_server.py") + jsonStringBody(old.String()) + `","new_str":"x"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < len(response); i += 24 {
			end := i + 24
			if end > len(response) {
				end = len(response)
			}
			delta, _ := json.Marshal(response[i:end])
			sseWrite(w, `data: {"choices":[{"delta":{"content":`+string(delta)+`}}]}`)
		}
		sseWrite(w, "data: [DONE]")
	}))
	defer srv.Close()
	ctx.InferenceURL = srv.URL
	ctx.Messages = []AgentMessage{{Role: "user", Content: "add a toml branch"}}

	content, _, err := callLLMOnce(ctx, ctx.Messages, 0.3)
	if err != nil {
		t.Fatalf("callLLMOnce: %v", err)
	}
	if ctx.LastStreamCut != "old_str_unmatched" {
		t.Fatalf("LastStreamCut = %q, want old_str_unmatched", ctx.LastStreamCut)
	}
	if len(content) > len(response)/3 {
		t.Errorf("the stream ran to %d of %d bytes before the cut", len(content), len(response))
	}
	category, feedback := parseFailureFeedback(ctx, content, ctx.LastStreamCut)
	if category != "old_str_cut" {
		t.Errorf("category %q, want old_str_cut", category)
	}
	for _, want := range []string{"executor_server.py", "# We use a simple python script",
		"Nothing was executed", "replace_lines"} {
		if !strings.Contains(feedback, want) {
			t.Errorf("feedback lacks %q:\n%s", want, feedback)
		}
	}
}

func TestACheckInsideAMultiByteCharacterIsNotAMismatch(t *testing.T) {
	file := oldStrTarget()
	var lines []string
	for _, l := range strings.Split(file, "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	curly := strings.ReplaceAll(file[strings.Index(file, "    elif"):], `"`, "\u201c")
	cut := curly[:strings.Index(curly, "\u201c")+1] // one byte of a three-byte quote
	if got := trimIncompleteRune(cut); !strings.HasSuffix(curly[:len(got)+3], "\u201c") || len(got) != len(cut)-1 {
		t.Fatalf("trimIncompleteRune(%q) = %q", cut, got)
	}
	if !oldStrCanMatch(file, lines, trimIncompleteRune(cut)) {
		t.Error("a curly-quoted prefix cut mid-character reads as a mismatch")
	}
	if trimIncompleteRune("plain") != "plain" || trimIncompleteRune("é") != "é" {
		t.Error("a complete string was trimmed")
	}
}

func TestEveryPrefixOfAMatchingOldStrCanStillMatch(t *testing.T) {
	// The run-on allowance must never be what keeps a matching old_str alive:
	// no prefix of one may read as a mismatch.
	file := oldStrTarget()
	var lines []string
	for _, l := range strings.Split(file, "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	long := file[strings.Index(file, "    errors = []"):]
	numbered := func() string {
		var sb strings.Builder
		for i, l := range strings.Split(strings.TrimSuffix(long, "\n"), "\n") {
			fmt.Fprintf(&sb, "%d\t%s\n", 5+i, l)
		}
		return sb.String()
	}()
	for name, s := range map[string]string{
		"exact": long, "indent drift": strings.ReplaceAll(long, "        ", "    "),
		"curly quotes": strings.ReplaceAll(long, `"`, "\u201c"), "numbered": numbered,
	} {
		for n := 1; n <= len(s); n++ {
			if !oldStrCanMatch(file, lines, trimIncompleteRune(s[:n])) {
				t.Errorf("%s: the prefix of %d bytes reads as a mismatch: ...%q", name, n, s[max(0, n-40):n])
				break
			}
		}
	}
}

func TestTheWatchNeverReadsOutsideTheWorkspace(t *testing.T) {
	// A file outside the workspace, and a symlink inside it that leads there:
	// the "lines matched" feedback must not become a way to probe either.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte(oldStrTarget()), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, dir := oldStrWorld(t, "unrelated\n")
	if err := os.Symlink(outside, filepath.Join(dir, "link.py")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, "link.py", "../" + filepath.Base(filepath.Dir(outside)) + "/secret.txt"} {
		w := newOldStrWatch(ctx)
		if w.load(path) {
			t.Errorf("the watch read %s, which is outside the workspace", path)
		}
	}
	w := newOldStrWatch(ctx)
	if !w.load("executor_server.py") {
		t.Error("a file inside the workspace was not read")
	}
}
