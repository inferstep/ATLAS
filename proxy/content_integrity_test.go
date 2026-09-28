package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Content integrity through the real write paths.
//
// Contract: a valid payload lands on disk as exactly the bytes intended --
// the decoded JSON string for an inline write, the body between the opening
// fence line and the last closing fence line (ending in the newline that
// precedes that closing fence) for a fenced write, the stated replacement for
// an edit. Ambiguous or incomplete input is refused explicitly with a retry
// path; it is never "repaired" by guessing the intended bytes.
//
// Everything is driven through runAgentLoop with a stub model, so JSON
// decoding, fenced resolution, sanitisation, tier/V3 routing (the stub V3 is
// unavailable, as when it fails), gates and the final write all run.

type integrityRun struct {
	dir      string
	results  []map[string]interface{}
	errors   []map[string]interface{}
	terminal map[string]string
	fencedN  int
	prompts  []string // main-loop request bodies, in order: what the model was told
}

func integrityLoop(t *testing.T, request string, preexisting map[string]string, turns []string,
	fencedReply func(prompt string) string) *integrityRun {
	t.Helper()
	return integrityLoopWith(t, request, preexisting, turns, fencedReply, nil)
}

// integrityLoopWith is integrityLoop with a hook that adjusts the session
// before it runs (a task contract, as the request boundary would set it).
func integrityLoopWith(t *testing.T, request string, preexisting map[string]string, turns []string,
	fencedReply func(prompt string) string, setup func(*AgentContext)) *integrityRun {
	t.Helper()
	dir := t.TempDir()
	for p, c := range preexisting {
		full := filepath.Join(dir, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := &integrityRun{dir: dir, terminal: map[string]string{}}
	var mu sync.Mutex
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		case strings.HasSuffix(r.URL.Path, "/execute"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			if strings.Contains(in.Code, ".atlas-mount-probe") {
				b, _ := os.ReadFile(filepath.Join(dir, ".atlas-mount-probe"))
				json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "stdout": string(b), "exit_code": 0})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "stdout": "", "exit_code": 0})
			return
		case strings.HasPrefix(r.URL.Path, "/v3/"), strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(s string) {
			d, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{{"delta": map[string]string{"content": s}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
		}
		if strings.Contains(string(raw), "single fenced block") {
			mu.Lock()
			run.fencedN++
			mu.Unlock()
			if fencedReply == nil {
				send("no block here")
				return
			}
			send(fencedReply(string(raw)))
			return
		}
		mu.Lock()
		k := i
		i++
		run.prompts = append(run.prompts, string(raw))
		mu.Unlock()
		if k >= 40 {
			http.Error(w, "turn ceiling", http.StatusInsufficientStorage)
			return
		}
		if k < len(turns) {
			send(turns[k])
			return
		}
		send(`{"type":"done","summary":"wrote the file"}`)
	}))
	t.Cleanup(srv.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.TrustMode = trustFullyTrusted
	ctx.VerifyOnHost = true
	ctx.MaxTurns = 0
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		defer mu.Unlock()
		switch et {
		case "tool_result":
			run.results = append(run.results, m)
		case "error":
			run.errors = append(run.errors, m)
		case "done":
			for k, v := range m {
				run.terminal[k] = fmt.Sprint(v)
			}
		}
	}
	if setup != nil {
		setup(ctx)
	}
	t.Setenv("ATLAS_FENCED_STALL_SEC", "2")
	t.Setenv("ATLAS_FENCED_FIRST_CONTENT_SEC", "3")
	if err := runAgentLoop(ctx, request); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	return run
}

func toolCall(name string, args map[string]interface{}) string {
	b, _ := json.Marshal(map[string]interface{}{"type": "tool_call", "name": name, "args": args})
	return string(b)
}

func (r *integrityRun) disk(t *testing.T, rel string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, rel))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// told reports whether any request the model received after its first turn
// contains every one of the phrases.
func (r *integrityRun) told(phrases ...string) bool {
	for _, p := range r.prompts[min(1, len(r.prompts)):] {
		ok := true
		for _, ph := range phrases {
			// Request bodies are JSON: compare against the encoded phrase.
			enc, _ := json.Marshal(ph)
			if !strings.Contains(p, strings.Trim(string(enc), `"`)) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func (r *integrityRun) feedback() string {
	b, _ := json.Marshal(map[string]interface{}{"results": r.results, "errors": r.errors})
	return string(b)
}

const writeReq = "Create the file exactly as specified."

// --- payloads ---------------------------------------------------------------

var integrityPayloads = []struct {
	name, path, body string
}{
	{"quotes and triple quotes", "q.py",
		"s = \"a \\\"quoted\\\" 'single' `tick`\"\nDOC = \"\"\"\nline \"one\" and ''' inner\n\"\"\"\n"},
	{"backslashes and literal backslash-n", "b.py",
		"import re\npath = \"C:\\\\Users\\\\x\"\nPAT = re.compile(r\"\\d+\\.\\w+\")\nmsg = \"a\\nb\"  # two characters: backslash, n\n"},
	{"unicode", "u.py",
		"# café 日本語 🎉 e\u0301 שלום a\u200db\nNAME = \"naïve — ok\"\n"},
	{"tabs and CRLF", "t.py",
		"def f():\r\n\treturn 1\r\n"},
	{"no final newline", "n.py",
		"X = 1\nY = 2"},
	{"form-feed page break before code", "ff.py",
		"def a():\n    pass\n\x0cdef b():\n    pass\n"},
	{"fences inside a python docstring", "doc.py",
		"def f():\n    \"\"\"Example:\n\n    ```python\n    f()\n    ```\n    \"\"\"\n    return 1\n"},
	{"fence inside a YAML block scalar", "c.yaml",
		"name: tool\ndescription: |\n  ```bash\n  ls -la\n  ```\nversion: 2\n"},
	{"one-line minified JSON with escapes", "data.json",
		`{"a":"line1\nline2\nline3\nline4","b":"` + strings.Repeat("x", 120) + `"}`},
	{"markdown with code blocks", "README.md",
		"# Tool\n\nRun:\n\n```bash\npython app.py\n```\n\nThen open it.\n"},
}

func longPayload() string {
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "V%04d = \"line %d \\\\ with \\\"quotes\\\" and ünïcode\"\n", i, i)
	}
	return b.String()
}

// Payloads whose bytes are ambiguous in the JSON channel: refused inline with
// the named reason, and preserved exactly by the fenced sub-call below.
var inlineRefused = map[string]string{
	"form-feed page break before code": "form feed",
	"fence inside a YAML block scalar": "only the file's own",
}

// Inline JSON channel: the decoded string is the file.
func TestInlineWritesLandExactlyTheDecodedBytes(t *testing.T) {
	cases := append([]struct{ name, path, body string }{}, integrityPayloads...)
	cases = append(cases, struct{ name, path, body string }{"5000-line file", "long.py", longPayload()})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := integrityLoop(t, writeReq, nil,
				[]string{toolCall("write_file", map[string]interface{}{"path": c.path, "content": c.body})}, nil)
			if why, refuse := inlineRefused[c.name]; refuse {
				if got, ok := run.disk(t, c.path); ok {
					t.Fatalf("ambiguous inline content was written instead of refused: %q", got)
				}
				if !run.told("NOT performed", why, "@fenced") {
					t.Errorf("the model was not told why and how to resend (%q)", why)
				}
				return
			}
			got, ok := run.disk(t, c.path)
			if !ok {
				t.Fatalf("not written; feedback: %.400s", run.feedback())
			}
			if got != c.body {
				t.Errorf("bytes changed on the way to disk\n want %q\n  got %q", truncateStr(c.body, 300), truncateStr(got, 300))
			}
		})
	}
}

// An identifier that merely starts with "@fenced" is content, not the sentinel.
func TestAnAtFencedDecoratorIsNotTheSentinel(t *testing.T) {
	body := "@fenced_route(\"/x\")\ndef handler():\n    return 1\n"
	run := integrityLoop(t, writeReq, nil,
		[]string{toolCall("write_file", map[string]interface{}{"path": "routes.py", "content": body})}, nil)
	if got, _ := run.disk(t, "routes.py"); got != body {
		t.Errorf("decorator content was treated as the @fenced sentinel\n want %q\n  got %q", body, got)
	}
	if run.fencedN != 0 {
		t.Errorf("a fenced sub-call was made for inline content (%d)", run.fencedN)
	}
}

// Fenced sub-call: the body between the fence lines is the file.
func TestFencedSubCallWritesLandExactlyTheBody(t *testing.T) {
	for _, c := range integrityPayloads {
		if !strings.HasSuffix(c.body, "\n") || strings.Contains(c.body, "\r\n") {
			continue // the fenced contract ends the file with the LF before the closing fence
		}
		t.Run(c.name, func(t *testing.T) {
			fence := "```"
			if strings.Contains(c.body, "```") {
				fence = "````"
			}
			reply := fence + "\n" + c.body + fence
			run := integrityLoop(t, writeReq, nil,
				[]string{toolCall("write_file", map[string]interface{}{"path": c.path, "content": "@fenced"})},
				func(string) string { return reply })
			got, ok := run.disk(t, c.path)
			if !ok {
				t.Fatalf("not written; feedback: %.400s", run.feedback())
			}
			if got != c.body {
				t.Errorf("fenced body changed on the way to disk\n want %q\n  got %q", c.body, got)
			}
		})
	}
}

// Trailing prose after the closing fence must not truncate a body that
// contains its own fences.
func TestFencedBodyWithInteriorFencesSurvivesTrailingProse(t *testing.T) {
	body := "def f():\n    \"\"\"Example:\n\n    ```python\n    f()\n    ```\n    \"\"\"\n    return 1\n"
	reply := "```python\n" + body + "```\n\nThat is the complete file."
	run := integrityLoop(t, writeReq, nil,
		[]string{toolCall("write_file", map[string]interface{}{"path": "doc.py", "content": "@fenced"})},
		func(string) string { return reply })
	if got, _ := run.disk(t, "doc.py"); got != body {
		t.Errorf("interior fence cut the file\n want %q\n  got %q", body, got)
	}
}

// Two separate blocks are ambiguous: which one is the file? Refuse, never
// merge the prose and the second block into the file.
func TestFencedReplyWithTwoBlocksIsRefusedNotMerged(t *testing.T) {
	reply := "```python\nprint('a')\n```\n\nRun it with:\n\n```bash\npython a.py\n```\n"
	run := integrityLoop(t, writeReq, nil,
		[]string{toolCall("write_file", map[string]interface{}{"path": "a.py", "content": "@fenced"})},
		func(string) string { return reply })
	if got, ok := run.disk(t, "a.py"); ok {
		t.Errorf("an ambiguous two-block reply was written: %q", got)
	}
}

// @fenced with an inline fenced body in the same reply.
func TestInlineFencedBodyLandsExactly(t *testing.T) {
	body := "import re\nPAT = re.compile(r\"\\d+\")\nS = \"tab\\there\"\n"
	content := "@fenced\n```python\n" + body + "```"
	run := integrityLoop(t, writeReq, nil,
		[]string{toolCall("write_file", map[string]interface{}{"path": "inl.py", "content": content})}, nil)
	if got, _ := run.disk(t, "inl.py"); got != body {
		t.Errorf("inline fenced body changed\n want %q\n  got %q", body, got)
	}
}

// --- ambiguous JSON-channel content: refused with a retry path ---------------

// A long single line whose only structure is literal \n, arriving through the
// JSON channel for a line-structured file, is almost certainly double-escaped
// -- but not certainly. Refuse and say how to resend; never decode it.
func TestDoubleEscapedBodyIsRefusedNotDecoded(t *testing.T) {
	// 150 characters: over the 120-character floor below which a one-line
	// body with escapes is treated as a plausible single line.
	body := `# Tool\n\nThis tool does things.\n\n## Install\n\nRun pip install tool and then start it with python app.py.\n\nOpen http://127.0.0.1:5000 after.\n`
	run := integrityLoop(t, writeReq, nil,
		[]string{toolCall("write_file", map[string]interface{}{"path": "GUIDE.rst", "content": body})}, nil)
	if got, ok := run.disk(t, "GUIDE.rst"); ok {
		t.Errorf("a double-escaped body was written instead of refused: %q", got)
	}
	if !run.told("NOT performed", "escaped once", "@fenced") {
		t.Error("the refusal did not reach the model with a retry path")
	}
}

// A form feed / backspace / CR directly before a letter, from the JSON
// channel, is what an eaten "\n" looks like -- or real content. Refuse
// explicitly rather than rewrite it; the fenced path preserves it.
func TestJSONChannelControlCharacterBeforeALetterIsRefusedNotRewritten(t *testing.T) {
	body := "def a():\n    return 1\n\x0cunction b():\n"
	run := integrityLoop(t, writeReq, nil,
		[]string{toolCall("write_file", map[string]interface{}{"path": "cc.js", "content": body})}, nil)
	got, ok := run.disk(t, "cc.js")
	if ok && got != body {
		t.Errorf("control character was rewritten instead of refused: %q", got)
	}
	if ok {
		t.Errorf("an eaten-escape shape was written without an explicit refusal")
	}
	if !run.told("NOT performed", "form feed", "line 3") {
		t.Errorf("refusal does not name the problem and where it is: %.300s", run.feedback())
	}
}

// Prose around a fence is refused; exactly one wrapping fence is removed and
// the model is told so in the result.
func TestAWrapperIsRemovedOnlyWhenExactAndNeverSilently(t *testing.T) {
	body := "def f():\n    return 1\n"
	wrapped := integrityLoop(t, writeReq, nil,
		[]string{toolCall("write_file", map[string]interface{}{"path": "w.py", "content": "```python\n" + body + "```\n"})}, nil)
	if got, _ := wrapped.disk(t, "w.py"); got != body {
		t.Errorf("exact wrapper: want %q got %q", body, got)
	}
	if !strings.Contains(wrapped.feedback(), "only the lines inside the fence were written") {
		t.Errorf("the wrapper removal was silent: %.400s", wrapped.feedback())
	}

	prose := integrityLoop(t, writeReq, nil,
		[]string{toolCall("write_file", map[string]interface{}{"path": "p.py",
			"content": "Here is the file:\n```python\n" + body + "```\nIt returns 1."})}, nil)
	if got, ok := prose.disk(t, "p.py"); ok {
		t.Errorf("prose around a fence was written: %q", got)
	}
	if !prose.told("NOT performed", "only the file's own") {
		t.Error("prose-wrapped content was not refused with a retry path")
	}
}

// A markdown file whose own code blocks use ``` inside a ``` wrapper cannot be
// split unambiguously. It is refused with the four-backtick retry, and nothing
// partial lands.
func TestAFencedMarkdownWithUnwidenedInteriorFencesIsRefused(t *testing.T) {
	body := "# Tool\n\nRun:\n\n```bash\npython app.py\n```\n\nThen open it.\n"
	run := integrityLoop(t, writeReq, nil,
		[]string{toolCall("write_file", map[string]interface{}{"path": "README.md", "content": "@fenced"})},
		func(string) string { return "```markdown\n" + body + "```" })
	if got, ok := run.disk(t, "README.md"); ok {
		t.Errorf("an ambiguous fenced reply was written: %q", got)
	}
}

// --- truncated and malformed tool calls: refused, never executed -------------

func TestATruncatedWriteFileIsRefusedNotExecuted(t *testing.T) {
	cut := `{"type":"tool_call","name":"write_file","args":{"path":"cut.py","content":"def f():\n    return [{\"user\": 1`
	run := integrityLoop(t, writeReq, nil, []string{cut}, nil)
	if got, ok := run.disk(t, "cut.py"); ok {
		t.Errorf("a truncated write_file was executed: %q", got)
	}
	if len(run.errors) == 0 && !strings.Contains(run.feedback(), "truncat") {
		t.Errorf("no explicit refusal surfaced: %.300s", run.feedback())
	}
}

func TestAMalformedWriteFileIsRefusedNotExecuted(t *testing.T) {
	// complete JSON shape, invalid escape \d inside the string
	bad := `{"type":"tool_call","name":"write_file","args":{"path":"bad.py","content":"import re\nP = re.compile(\"\d+\")\n"}}`
	run := integrityLoop(t, writeReq, nil, []string{bad}, nil)
	if got, ok := run.disk(t, "bad.py"); ok {
		t.Errorf("a malformed write_file was executed with guessed unescaping: %q", got)
	}
}

func TestATruncatedEditFileIsRefusedNotExecuted(t *testing.T) {
	orig := "def f():\n    return 1\n"
	cut := `{"type":"tool_call","name":"edit_file","args":{"path":"e.py","old_str":"return 1","new_str":"return [1, 2`
	run := integrityLoop(t, writeReq, map[string]string{"e.py": orig},
		[]string{toolCall("read_file", map[string]interface{}{"path": "e.py"}), cut}, nil)
	if got, _ := run.disk(t, "e.py"); got != orig {
		t.Errorf("a truncated edit_file was executed: %q", got)
	}
}

// --- edits ------------------------------------------------------------------

func TestEditReplacementIsWrittenAsGiven(t *testing.T) {
	orig := "html = '<p>a &lt; b</p>'\n"
	// old_str arrives entity-encoded; the replacement intentionally holds &amp;
	run := integrityLoop(t, writeReq, map[string]string{"h.py": orig},
		[]string{
			toolCall("read_file", map[string]interface{}{"path": "h.py"}),
			toolCall("edit_file", map[string]interface{}{"path": "h.py",
				"old_str": "html = '&lt;p&gt;a &amp;lt; b&lt;/p&gt;'", "new_str": "html = '<p>a &amp; b</p>'"}),
		}, nil)
	got, _ := run.disk(t, "h.py")
	if strings.Contains(got, "<p>a & b</p>") {
		t.Errorf("new_str was entity-decoded, changing intended bytes: %q", got)
	}
	if got != orig {
		t.Errorf("an edit whose old_str matched only after decoding was applied: %q", got)
	}
	if !run.told("HTML-entity-encoded") {
		t.Error("the refusal did not name the entity encoding")
	}
}

func TestInsertAndReplaceLinesLandExactly(t *testing.T) {
	orig := "a = 1\nb = 2\nc = 3\n"
	ins := "\tx = \"ünï \\\\ code\"\n"
	run := integrityLoop(t, writeReq, map[string]string{"l.py": orig},
		[]string{
			toolCall("read_file", map[string]interface{}{"path": "l.py"}),
			toolCall("insert_after", map[string]interface{}{"path": "l.py", "line": 1, "content": ins}),
		}, nil)
	want := "a = 1\n" + ins + "b = 2\nc = 3\n"
	if got, _ := run.disk(t, "l.py"); got != want {
		t.Errorf("insert_after changed bytes\n want %q\n  got %q", want, got)
	}
}

// --- post-write rewrites ------------------------------------------------------

// A literal the user spelled out, which the model placed with different
// indentation, must not be silently re-indented after the write succeeded.
func TestAStatedLiteralIsNotSilentlyReindentedAfterAWrite(t *testing.T) {
	req := "Create app.py. It must contain exactly this block:\n```\nif ready:\nprint(\"go\")\n```"
	body := "def main(ready):\n    if ready:\n        print(\"go\")\n"
	run := integrityLoop(t, req, nil,
		[]string{toolCall("write_file", map[string]interface{}{"path": "app.py", "content": body})}, nil)
	if got, _ := run.disk(t, "app.py"); got != body {
		t.Errorf("file silently rewritten after a successful write\n want %q\n  got %q", body, got)
	}
}

// Spacing drift inside a stated literal's lines is still restored, but the
// model is told, and the session's own follow-up edit is not refused as a
// foreign modification.
func TestIntraLineLiteralDriftIsRestoredReportedAndStaysEditable(t *testing.T) {
	req := "Create app.py. It must contain exactly this line:\n```\nBANNER = \"ready\"\n```"
	body := "BANNER = \" ready\"\nprint(BANNER)\n"
	run := integrityLoop(t, req, nil, []string{
		toolCall("write_file", map[string]interface{}{"path": "app.py", "content": body}),
		toolCall("edit_file", map[string]interface{}{"path": "app.py",
			"old_str": "print(BANNER)", "new_str": "print(BANNER.upper())"}),
	}, nil)
	want := "BANNER = \"ready\"\nprint(BANNER.upper())\n"
	if got, _ := run.disk(t, "app.py"); got != want {
		t.Errorf("want %q\n got %q\nfeedback: %.600s", want, got, run.feedback())
	}
	if !run.told("spacing inside 1 line(s) was changed") {
		t.Error("the literal repair was silent")
	}
}
