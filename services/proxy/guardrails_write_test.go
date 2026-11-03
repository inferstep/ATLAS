package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// proseAroundAFence is a model reply pasted into a file whole: prose, the file
// in a fence, and more prose. Which of these lines are the file is a guess.
const proseAroundAFence = "Looking at the task, I need to create a complete index.html file.\n" +
	"\n" +
	"```html\n" +
	"<!DOCTYPE html>\n" +
	"<html><body>hi</body></html>\n" +
	"```\n" +
	"\n" +
	"This file:\n" +
	"1. Renders correctly\n" +
	"2. **Includes Jinja syntax** ({{ url_for(...) }})\n"

// One fenced block and nothing else is the only shape whose file is not a
// guess: the lines inside the fence. That wrapper is removed, to a fixpoint.
func TestSanitizeFileContentStripsOnlyAnExactWholeFileWrapper(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"a plain wrapper", "```python\ndef foo():\n    pass\n```\n", "def foo():\n    pass\n"},
		{"blank lines around the wrapper", "\n```python\nx = 1\n```\n\n", "x = 1\n"},
		{"CRLF line endings", "```python\r\nx = 1\r\n```\r\n", "x = 1\r\n"},
		{"a four-backtick wrapper keeps the fences inside it",
			"````python\ns = \"\"\"\n```\nx\n```\n\"\"\"\n````\n", "s = \"\"\"\n```\nx\n```\n\"\"\"\n"},
		{"nested wrappers are removed to a fixpoint", "````\n```python\nx = 1\n```\n````\n", "x = 1\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, stripped := sanitizeFileContent("solve.py", c.in)
			if !stripped {
				t.Errorf("sanitizeFileContent did not report a strip of %q", c.in)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if again, _ := sanitizeFileContent("solve.py", got); again != got {
				t.Errorf("a second pass changed the result again: %q", again)
			}
		})
	}
}

// Every other shape is written exactly as it came: to remove anything from it
// would be a guess about which lines are the file, and a wrong guess destroys
// valid content.
func TestSanitizeFileContentLeavesEverythingElseUnchanged(t *testing.T) {
	for _, c := range []struct{ name, path, in string }{
		{"code with no fence", "foo.py", "def foo():\n    return 1\n"},
		{"prose around a fence", "templates/index.html", proseAroundAFence},
		{"a fence that never closes, after a preamble", "foo.py", "Here's the code:\n\n```python\ndef foo():\n    return 1\n"},
		{"a fenced example inside a docstring, with code around it", "example.py",
			"DOC = \"\"\"Usage example:\n```python\nx = 1\n```\nend\"\"\"\n\ndef f():\n    return 1"},
		{"a line inside that would close a three-backtick wrapper", "solve.py", "```\n```python\nx = 1\n```\n```\n"},
		{"text after the closing fence", "solve.py", "```python\nx = 1\n```\ny = 2\n"},
		{"an opening fence alone", "solve.py", "```python\n"},
		{"an empty wrapper, which would leave an empty file", "solve.py", "```\n```"},
		{"a markdown document, whose fences are its content", "README.md", "```python\nprint('hi')\n```\n"},
		{"a control character before a letter is not rewritten", "server.js", " // Serve the frontend\x0cunction serveStatic() {\n"},
		{"escaped line breaks are not decoded", "main.py", `import sys\n\ndef main():\n    sys.exit(0)\n\nmain()\n`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, stripped := sanitizeFileContent(c.path, c.in)
			if stripped {
				t.Errorf("sanitizeFileContent reports a strip of %q", c.in)
			}
			if got != c.in {
				t.Errorf("content was rewritten: %q", got)
			}
		})
	}
}

// Content that is usually an escaping slip is refused before it runs, with a
// retry that cannot slip the same way (@fenced). It is never rewritten.
func TestJSONChannelContentFeedback(t *testing.T) {
	fenceRefusal := []string{"NOT performed", "@fenced", "only the file's own"}
	// A README whose escapes were doubled: one long line in which every line
	// break is the two characters \n.
	escapedReadme := `# Running Club Website\n\nThis is a simple web application to track runs.\n\n## Setup and Installation\n\n1. **Install dependencies**:\n   ` +
		"```bash\\npip install -r requirements.txt\\n```" +
		`\n\n2. **Run the application**:\n   ` +
		"```bash\\npython3 app.py\\n```" +
		`\n\nOnce started, the site is at http://127.0.0.1:5000.`
	for _, c := range []struct {
		name, tool, field, path, body string
		refusal                       []string // nil: the call goes ahead
	}{
		{"prose before and after a fence", "write_file", "content", "templates/index.html", proseAroundAFence, fenceRefusal},
		{"prose that mentions a comment marker, then a fence", "write_file", "content", "app.js",
			"Here's app.js with the /* config */ block rewritten:\n```javascript\nconst x = 1;\nexport default x;\n```", fenceRefusal},
		{"a fence that never closes, after a preamble", "write_file", "content", "foo.py",
			"Here's the code:\n\n```python\ndef foo():\n    return 1\n", fenceRefusal},
		{"a body whose escapes were doubled", "write_file", "content", "README.md", escapedReadme,
			[]string{"NOT performed", "escaped once", "@fenced"}},
		{"a form feed before a letter", "write_file", "content", "server.js",
			" // Serve the frontend\x0cunction serveStatic() {\n  app.get('/', (req, res) => {\n",
			[]string{"NOT performed", "form feed", "line 1", "@fenced"}},
		{"a backspace before a letter", "write_file", "content", "server.js", "x = 1\x08ar = 2\n", []string{"backspace"}},
		{"a carriage return before a letter, in a file of LF lines", "write_file", "content", "server.js", "  }\return x;\n",
			[]string{"carriage return"}},
		{"a control character in an edit's replacement", "edit_file", "new_str", "server.js", "a\x0cb\n", []string{"form feed"}},
		{"a control character in inserted lines", "insert_after", "content", "server.js", "a\x0cb\n", []string{"form feed"}},

		{"a multi-line file that writes \\n in a string", "write_file", "content", "main.py",
			"import sys\n\ndef main():\n    sys.stdout.write(\"a\\nb\\n\")\n    return 0\n", nil},
		{"a short one-liner that holds escapes", "write_file", "content", "run.sh", `printf 'a\nb\nc\n'`, nil},
		{"one-line minified JSON", "write_file", "content", "data.json",
			`{"a":"x\ny","b":"p\nq","c":"r\ns","d":"` + strings.Repeat("z", 120) + `"}`, nil},
		{"the @fenced sentinel", "write_file", "content", "README.md", "@fenced", nil},
		{"a page-break form feed on its own line", "write_file", "content", "server.js",
			"def a():\n    pass\n\x0c\ndef b():\n    pass\n", nil},
		{"a form feed before a space", "write_file", "content", "server.js", "text \x0c more\n", nil},
		{"CRLF line endings", "write_file", "content", "server.js", "line one\r\nline two\r\n", nil},
		{"a file of CR-only lines", "write_file", "content", "server.js", "line one\rline two\r", nil},
		{"ordinary source with a tab", "write_file", "content", "server.js", "def f():\n\treturn 1\n", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			args, err := json.Marshal(map[string]string{"path": c.path, c.field: c.body})
			if err != nil {
				t.Fatal(err)
			}
			msg, refused := jsonChannelContentFeedback(c.tool, args)
			if refused != (c.refusal != nil) {
				t.Fatalf("refused=%v, want %v:\n%s", refused, c.refusal != nil, msg)
			}
			for _, want := range c.refusal {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal lacks %q:\n%s", want, msg)
				}
			}
		})
	}
}

// A replacement far smaller than what it replaces is usually a cut-off reply
// or a doctype alone, and it would destroy the file. Small originals are not
// checked: a line edit legitimately shrinks.
func TestValidateNotSuspiciouslyShrunk(t *testing.T) {
	for _, c := range []struct {
		name             string
		tool             string
		oldSize, newSize int
		refusal          []string // nil: allowed
	}{
		{"a doctype alone in place of a 120B file", "structural_edit", 120, 16,
			[]string{"structural_edit refused", "16B", "120B", "FULL", "templates/index.html"}},
		{"a 32B stub in place of a 2KB page", "structural_edit", 2199, 32, []string{"refused"}},
		{"60B, just under the 64B floor", "structural_edit", 2000, 60, []string{"refused"}},
		{"edit_file is held to the same rule", "edit_file", 200, 8, []string{"edit_file refused"}},
		{"an original under 100B is not checked", "structural_edit", 80, 20, nil},
		{"64B is enough", "structural_edit", 1500, 64, nil},
		{"a 5KB function refactored to an 80B one-liner", "structural_edit", 5000, 80, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := validateNotSuspiciouslyShrunk(c.tool, "templates/index.html", c.oldSize, c.newSize)
			if (got != "") != (c.refusal != nil) {
				t.Fatalf("got %q, want a refusal: %v", got, c.refusal != nil)
			}
			for _, want := range c.refusal {
				if !strings.Contains(got, want) {
					t.Errorf("refusal lacks %q: %s", want, got)
				}
			}
		})
	}
}

// structural_edit with the <html> selector replaces only the element, so a
// doctype the model sends along with it would sit under the one already there.
func TestStripLeadingDoctype(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
		stripped       bool
	}{
		{"an html5 doctype", "<!DOCTYPE html>\n<html><body></body></html>", "<html><body></body></html>", true},
		{"a lowercase doctype", "<!doctype html>\n<html></html>", "<html></html>", true},
		{"whitespace before the doctype", "  \n<!DOCTYPE html>\n<html></html>", "<html></html>", true},
		{"a verbose html4 doctype", `<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 4.01//EN">` + "\n<html></html>", "<html></html>", true},
		{"no doctype", "<html><body></body></html>", "<html><body></body></html>", false},
		{"a doctype after other content", "<html><!DOCTYPE html><body></body></html>", "<html><!DOCTYPE html><body></body></html>", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, stripped := stripLeadingDoctype(c.in)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if stripped != c.stripped {
				t.Errorf("stripped=%v, want %v", stripped, c.stripped)
			}
		})
	}
}

// A new file among files of its kind should follow them, so the write waits
// until the model has read one of them.
func TestPatternMatchHint(t *testing.T) {
	for _, c := range []struct {
		name   string
		files  []string
		target string
		read   []string // the siblings the session has read
		hint   bool
	}{
		{"a new file among three unread siblings", []string{"index.html", "about.html", "contact.html"}, "pricing.html", nil, true},
		{"one sibling read", []string{"index.html", "about.html", "contact.html"}, "pricing.html", []string{"index.html"}, false},
		{"a file that already exists", []string{"a.html", "b.html"}, "a.html", nil, false},
		{"one sibling is not a pattern", []string{"only.go"}, "new.go", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range c.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			read := map[string]string{}
			for _, name := range c.read {
				read[filepath.Join(dir, name)] = "x"
			}
			if got := patternMatchHint(filepath.Join(dir, c.target), read); (got != "") != c.hint {
				t.Errorf("hint %q, want a hint: %v", got, c.hint)
			}
		})
	}
}

// A file left on disk as prose around a fence is broken, and write_file may
// replace it. A clean file, and a markdown file with fences, are not broken.
func TestLooksCorruptedOnDisk(t *testing.T) {
	for _, c := range []struct {
		name, path, content string
		corrupted           bool
	}{
		{"prose around a fence", "templates/index.html", proseAroundAFence, true},
		{"a clean html file", "templates/index.html", "<!DOCTYPE html>\n<html><body>hi</body></html>\n", false},
		{"a markdown file with a fence", "README.md", "# Title\n\n```python\nprint('hi')\n```\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if looksCorruptedOnDisk(c.path, c.content) != c.corrupted {
				t.Errorf("looksCorruptedOnDisk = %v, want %v", !c.corrupted, c.corrupted)
			}
		})
	}
}

// Content the user spelled out is a byte-exact contract: a fenced block, or
// the one line after an "exactly:" marker.
func TestExtractLiteralBlocks(t *testing.T) {
	for _, c := range []struct {
		name, task string
		want       []string
	}{
		{"the line after an exactly marker", "Create banner.py containing exactly one line:\nBANNER = \"ready\"\nDo not modify any existing file.",
			[]string{`BANNER = "ready"`}},
		{"a fenced block", "Make it print this:\n```\nhello world one\nhello world two\n```\nthanks",
			[]string{"hello world one\nhello world two"}},
		{"a fragment too short to be a contract", "set it to exactly this value:\nx = 1\n", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := extractLiteralBlocks(c.task); !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A written literal that differs from the user's only in spacing within its
// lines is put back to the user's bytes. Anything else is left as it is.
func TestRepairLiteralDrift(t *testing.T) {
	banner := `BANNER = "ready"`
	for _, c := range []struct {
		name, content, literal, want string
	}{
		{"a space inside the string", "BANNER = \" ready\"\n", banner, "BANNER = \"ready\"\n"},
		{"a multi-line literal, in its window", "pre\na  =  1\nb =\t2\npost\n", "a = 1\nb = 2", "pre\na = 1\nb = 2\npost\n"},
		{"byte-exact content", "BANNER = \"ready\"\n", banner, "BANNER = \"ready\"\n"},
		{"the literal inside a longer line", "x = 1\nBANNER = \"ready\"  # note\n", banner, "x = 1\nBANNER = \"ready\"  # note\n"},
		{"a change beyond whitespace", "BANNER = \"redy\"\n", banner, "BANNER = \"redy\"\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var wantRepaired []string
			if c.want != c.content {
				wantRepaired = []string{c.literal}
			}
			got, repaired, changed := repairLiteralDrift(c.content, []string{c.literal})
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if changed != (wantRepaired != nil) {
				t.Errorf("changed=%v, want %v", changed, wantRepaired != nil)
			}
			if !slices.Equal(repaired, wantRepaired) {
				t.Errorf("repaired %q, want %q", repaired, wantRepaired)
			}
		})
	}
}
