package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Refusal guidance names selectors that exist, never a placeholder.
//
// Captured in stabilization cycle 1 (W1 aoc_slope rep1, relay req-00034 and
// req-00039): the model's solve.py ended `if __name__ == '__main__'):`. An
// edit_file miss was refused with "use structural_edit with a selector
// (`function:NAME` or `class:NAME`)"; the model then sent selector
// `function:NAME` with the corrected whole file as content, which was refused
// as a missing symbol, and the session ended stopped/repeated_refusal.

// The file as the session left it (relay req-00034, decoded).
const capturedSolve = "import sys\n\ndef solve():\n    with open('input.txt', 'r') as f:\n        grid = [line.strip() for line in f]\n\n    height = len(grid)\n    width = max(len(row) for row in grid)\n\n    slopes = [\n        (1, 1),\n        (3, 1),\n        (5, 1),\n        (7, 1),\n        (1, 2)\n    ]\n\n    results = []\n    for dx, dy in slopes:\n        count = 0\n        y = 0\n        while y < height:\n            x = y * dx\n            # The pattern repeats infinitely to the right\n            # So we use modulo to find the character in the original row\n            # However, the row might be shorter than x, so we use modulo width\n            row = grid[y]\n            char_idx = x % len(row)\n            if row[char_idx] == '#':\n                count += 1\n            y += dy\n        results.append(count)\n\n    product = 1\n    for r in results:\n        product *= r\n\n    print(product)\n\nif __name__ == '__main__'):\n    solve()\n"

// The structural_edit content the model sent (relay req-00039, decoded).
const capturedStructuralContent = "import sys\n\ndef solve():\n    with open('input.txt', 'r') as f:\n        grid = [line.strip() for line in f]\n\n    height = len(grid)\n    width = max(len(row) for row in grid)\n\n    slopes = [\n        (1, 1),\n        (3, 1),\n        (5, 1),\n        (7, 1),\n        (1, 2)\n    ]\n\n    results = []\n    for dx, dy in slopes:\n        count = 0\n        y = 0\n        while y < height:\n            x = y * dx\n            row = grid[y]\n            char_idx = x % len(row)\n            if row[char_idx] == '#':\n                count += 1\n            y += dy\n        results.append(count)\n\n    product = 1\n    for r in results:\n        product *= r\n\n    print(product)\n\nif __name__ == '__main__':\n    solve()\n"

// The v3-service refusal the session received for that call, verbatim.
const capturedZeroMatch = "selector 'function:NAME' matched 0 nodes in solve.py — that symbol does not exist in this file. If you are ADDING it, structural_edit is the wrong tool: it replaces a node that is already there. Use insert_after with the line number read_file printed to put the new code in, or edit_file anchored on one unique nearby line. This file defines: function:solve. Use one of these exact selectors, or read the file to confirm."

var placeholderSelector = regexp.MustCompile(`(?:function|class|type):NAME\b|<tag>`)

func capturedSolveWorld(t *testing.T, structural func(w http.ResponseWriter, r *http.Request)) *AgentContext {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "solve.py")
	if err := os.WriteFile(path, []byte(capturedSolve), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/internal/structural_edit" && structural != nil:
			structural(w, r)
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "unresolved": []string{}, "functions": []interface{}{}})
		}
	}))
	t.Cleanup(srv.Close)
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.V3URL, ctx.SandboxURL = srv.URL, srv.URL
	ctx.SessionWrites["solve.py"] = true
	ctx.RecordFileRead(path, capturedSolve)
	ctx.RecordBodySeen(path)
	return ctx
}

// The captured edit_file miss: guidance names `function:solve`, the one
// selector solve.py has, and no placeholder.
func TestAnEditMissNamesSelectorsThatExist(t *testing.T) {
	ctx := capturedSolveWorld(t, nil)
	args, _ := json.Marshal(map[string]string{"path": "solve.py",
		"old_str": "if __name__ == '__main__':\n    solve()", "new_str": "if __name__ == '__main__':\n    solve()"})
	res := executeToolCall("edit_file", args, ctx)
	if res.Success {
		t.Fatal("the mismatched edit landed")
	}
	if placeholderSelector.MatchString(res.Error) {
		t.Errorf("the refusal still offers a placeholder selector:\n%s", res.Error)
	}
	if !strings.Contains(res.Error, "`function:solve`") {
		t.Errorf("the refusal does not name the selector that exists:\n%s", res.Error)
	}
}

// The captured structural_edit: the selector was the placeholder and the
// content the whole corrected file. The refusal says the content is module
// level and names the tool that replaces a whole file this session wrote.
func TestAWholeFileSentAsOneNodeIsExplained(t *testing.T) {
	ctx := capturedSolveWorld(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": capturedZeroMatch})
	})
	args, _ := json.Marshal(map[string]string{"path": "solve.py", "selector": "function:NAME",
		"content": capturedStructuralContent})
	res := executeToolCall("structural_edit", args, ctx)
	if res.Success {
		t.Fatal("the structural edit landed")
	}
	for _, want := range []string{"function:solve", "line 1 (`import sys`) is module-level code", "write_file"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("refusal lacks %q:\n%s", want, res.Error)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(ctx.WorkingDir, "solve.py")); string(b) != capturedSolve {
		t.Error("the file changed")
	}

	// A file this session did not write is not steered to a whole-file write.
	delete(ctx.SessionWrites, "solve.py")
	res = executeToolCall("structural_edit", args, ctx)
	if strings.Contains(res.Error, "write_file") || !strings.Contains(res.Error, "replace_lines") {
		t.Errorf("guidance for a file the session did not write:\n%s", res.Error)
	}
}

// Ordinary, legitimate node content gets no module-level note, and a real
// selector passes through to the service untouched.
func TestLegitimateNodeContentAndSelectorsAreUntouched(t *testing.T) {
	var sent string
	ctx := capturedSolveWorld(t, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Selector string }
		json.NewDecoder(r.Body).Decode(&in)
		sent = in.Selector
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false,
			"error": "structural_edit: the replacement makes solve.py invalid Python"})
	})
	node := "# the entry point\n@cache\ndef solve():\n    return 1\n\n\nclass Grid:\n    pass\n"
	args, _ := json.Marshal(map[string]string{"path": "solve.py", "selector": "function:solve", "content": node})
	res := executeToolCall("structural_edit", args, ctx)
	if sent != "function:solve" {
		t.Errorf("selector sent to the service = %q", sent)
	}
	if strings.Contains(res.Error, "module-level code") {
		t.Errorf("node content was called module-level:\n%s", res.Error)
	}
	if note := moduleLevelContentNote(ctx, "solve.py", node); note != "" {
		t.Errorf("decorators, comments, defs and classes are node content: %q", note)
	}
	if note := moduleLevelContentNote(ctx, "app.js", "import x from 'y'\n"); note != "" {
		t.Errorf("the note is scoped to Python: %q", note)
	}
}

// Selectors come from the file: every language structural_edit handles, names
// defined once only, and a pointer to outline_file when there is nothing to name.
func TestSelectorsComeFromTheFile(t *testing.T) {
	for _, c := range []struct {
		path, src string
		want      []string
		absent    []string
	}{
		{"store.py", "import json\n\nclass Store:\n    def add(self):\n        pass\n\ndef load():\n    pass\n\ndef load():\n    pass\n\nasync def fetch():\n    pass\n",
			[]string{"`class:Store`", "`function:fetch`"}, []string{"function:load", "function:add"}},
		{"main.go", "package main\n\ntype Todo struct{}\n\nfunc (t *Todo) Done() {}\n\nfunc main() {}\n",
			[]string{"`type:Todo`", "`function:Done`", "`function:main`"}, nil},
		{"app.js", "export function draw() {}\nclass Game {}\n", []string{"`function:draw`", "`class:Game`"}, nil},
		{"index.html", "<html>\n<body>\n<script>let x = 1;</script>\n<script src=\"a.js\"></script>\n</body>\n</html>\n",
			[]string{"`<html>`", "`<body>`"}, []string{"<script>"}},
	} {
		g := selectorGuidance(c.path, c.src)
		for _, w := range c.want {
			if !strings.Contains(g, w) {
				t.Errorf("%s: guidance lacks %s: %q", c.path, w, g)
			}
		}
		for _, a := range c.absent {
			if strings.Contains(g, a) {
				t.Errorf("%s: guidance names %s, which is ambiguous or not selectable: %q", c.path, a, g)
			}
		}
		if placeholderSelector.MatchString(g) {
			t.Errorf("%s: placeholder in %q", c.path, g)
		}
	}
	if g := selectorGuidance("config.py", "DEBUG = True\nPORT = 8080\n"); !strings.Contains(g, "outline_file on config.py") {
		t.Errorf("nothing to name should point at outline_file: %q", g)
	}
	if g := selectorGuidance("notes.txt", "hello"); g != "" {
		t.Errorf("no structural language, no guidance: %q", g)
	}
}

// The step-restriction note and the tool-ban note name real selectors too.
func TestSteeringNotesNameRealSelectors(t *testing.T) {
	ctx := capturedSolveWorld(t, nil)
	ctx.Messages = []AgentMessage{
		{Role: "user", Content: "fix it"},
		{Role: "tool", ToolName: "write_file", Content: `{"success":false,"error":"File solve.py already exists (41 lines). Use structural_edit"}`},
	}
	msgs, _ := buildStepRequest(ctx)
	note := msgs[len(msgs)-1].Content
	if placeholderSelector.MatchString(note) || !strings.Contains(note, "`function:solve`") {
		t.Errorf("step-restriction note:\n%s", note)
	}
	ban := toolBanNoteFor(ctx, "edit_file", "solve.py")
	if strings.Contains(ban, "function:update") || !strings.Contains(ban, "`function:solve`") {
		t.Errorf("tool-ban note:\n%s", ban)
	}
}
