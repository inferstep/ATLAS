package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Steers read a failed command's output and tell the model the one thing that
// gets it out of a loop. Each steer must name what it found, and must stay
// silent on output it does not recognize: a steer on the wrong output sends
// the model after a problem it does not have.

// A missing module names the package to install; a missing file names the
// real file when only the case differs. Neither invents an answer.
func TestMissingModuleAndMissingFileSteers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steer func(*AgentContext, string) string
		files map[string]string // written into the workspace first
		out   string
		want  []string // empty: no steer at all
	}{
		{
			name:  "python -m with a missing package",
			steer: missingModuleSteer,
			out:   "/usr/local/bin/python3: No module named flask\n",
			want:  []string{"flask", "pip install"},
		},
		{
			name:  "an import of a submodule names the top-level package",
			steer: missingModuleSteer,
			out:   "ModuleNotFoundError: No module named 'flask.cli'\n",
			want:  []string{"pip install flask"},
		},
		{
			name:  "a requirements.txt is installed as a whole",
			steer: missingModuleSteer,
			files: map[string]string{"requirements.txt": "flask\n"},
			out:   "No module named flask\n",
			want:  []string{"pip install -r requirements.txt"},
		},
		{
			name:  "output with no missing module",
			steer: missingModuleSteer,
			out:   "Total: 42\n",
		},
		{
			name:  "a file named with the wrong case",
			steer: missingFileSteer,
			files: map[string]string{"requirements.txt": "flask\n"},
			out:   "ERROR: Could not open requirements file: [Errno 2] No such file or directory: 'Requirements.txt'\n",
			want:  []string{"requirements.txt", "case"},
		},
		{
			name:  "a shell error with the file name before the colon",
			steer: missingFileSteer,
			files: map[string]string{"main.py": "print(1)\n"},
			out:   "python: Main.py: No such file or directory\n",
			want:  []string{"main.py"},
		},
		{
			name:  "a file that is absent in every case",
			steer: missingFileSteer,
			out:   "cat: nope.txt: No such file or directory\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := tc.steer(&AgentContext{WorkingDir: dir}, tc.out)
			if len(tc.want) == 0 && got != "" {
				t.Errorf("want no steer, got %q", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("steer lacks %q: %q", want, got)
				}
			}
		})
	}
}

// A traceback steer points at the deepest frame in the project, the line the
// model can fix.
func TestTracebackSteer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     string
		want    []string
		notWant []string
	}{
		{
			name: "a project frame",
			out: "Traceback (most recent call last):\n" +
				"  File \"/workspace/_agenttest/app.py\", line 14, in get_item\n" +
				"    return jsonify(items[item_id + 1])\n" +
				"IndexError: list index out of range\n",
			want: []string{"get_item", "line 14", "IndexError", "function:get_item"},
		},
		{
			name: "a library frame below a project frame",
			out: "Traceback (most recent call last):\n" +
				"  File \"/workspace/app.py\", line 5, in main\n" +
				"    data = json.loads(raw)\n" +
				"  File \"/usr/lib/python3.9/json/__init__.py\", line 346, in loads\n" +
				"    return _default_decoder.decode(s)\n" +
				"ValueError: Expecting value\n",
			want:    []string{"app.py", "function:main"},
			notWant: []string{"json/__init__"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tracebackSteer(&AgentContext{WorkingDir: "/workspace"}, tc.out)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("steer lacks %q:\n%s", want, got)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("steer points at %q:\n%s", notWant, got)
				}
			}
		})
	}
}

// A missing package is an environment problem, not a fix site, and output with
// no traceback has nothing to point at: no steer at all.
func TestTracebackSteerStaysSilent(t *testing.T) {
	for _, tc := range []struct{ name, out string }{
		{
			name: "a missing package",
			out: "Traceback (most recent call last):\n" +
				"  File \"/workspace/snake_game.py\", line 1, in <module>\n" +
				"    import pygame\n" +
				"ModuleNotFoundError: No module named 'pygame'\n",
		},
		{
			name: "no traceback",
			out:  "Total inventory value: $237\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tracebackSteer(&AgentContext{WorkingDir: "/workspace"}, tc.out); got != "" {
				t.Errorf("want no steer, got %q", got)
			}
		})
	}
}

// A missing binary cannot be installed in the sandbox (no root, read-only), so
// the steer names it and says so.
func TestMissingCommandSteer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     string
		want    []string
		notWant []string
	}{
		{
			name:    "bash with a line number",
			out:     "bash: line 1: git: command not found\n",
			want:    []string{"`git`", "CANNOT be installed"},
			notWant: []string{"apt-get install"},
		},
		{
			name: "bash without a line number",
			out:  "bash: git: command not found\n",
			want: []string{"`git`"},
		},
		{
			name: "sh abbreviates the message",
			out:  "sh: 1: sqlite3: not found\n",
			want: []string{"`sqlite3`"},
		},
		{
			name: "a full path is reduced to its base name",
			out:  "bash: line 3: /usr/local/bin/terraform: command not found\n",
			want: []string{"`terraform`"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := missingCommandSteer(tc.out)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("steer lacks %q: %q", want, got)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("steer suggests %q: %q", notWant, got)
				}
			}
		})
	}
}

// The missing-command steer fires only on a real shell diagnostic, never on
// program output that happens to print the same words.
func TestMissingCommandSteerStaysSilent(t *testing.T) {
	for _, tc := range []struct{ name, out string }{
		{
			name: "a not-found line with no shell prefix",
			out:  "config.yaml: not found\n",
		},
		{
			name: "program output that prints the phrase",
			out:  `print("mytool: command not found")` + "\nmytool: command not found\n",
		},
		{
			name: "clean output",
			out:  "all tests passed\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := missingCommandSteer(tc.out); got != "" {
				t.Errorf("want no steer, got %q", got)
			}
		})
	}
}

// A multi-statement script on a `python3 -c` line cannot parse, and the
// SyntaxError is in the command, not the solution. The steer moves the test to
// a file. A syntax error in a real file is a solution bug, and a -c line that
// runs another file's code is not the broken part.
func TestBrokenInlineScriptSteer(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
		out  string
		want []string // empty: no steer at all
	}{
		{
			name: "a multi-statement -c line",
			cmd:  `python3 -c "import json, re; def all_legal_next_positions(fen): return []"`,
			out:  "  File \"<string>\", line 1\n    import json, re; def all_legal\n                     ^\nSyntaxError: invalid syntax",
			want: []string{"inline `-c`", ".py"},
		},
		{
			// The sandbox clipped the output before the SyntaxError line.
			name: "output cut before the SyntaxError line",
			cmd:  `python3 -c "import json, re; def all_legal(fen): return []"`,
			out:  `  File "<string>", line 1` + "\n" + `    import json, re; def all_legal(fen): return []`,
			want: []string{"inline `-c`"},
		},
		{
			name: "a syntax error in a real file",
			cmd:  `python3 solution.py`,
			out:  "  File \"solution.py\", line 12\n    def f(\n         ^\nSyntaxError: invalid syntax",
		},
		{
			name: "no syntax error",
			cmd:  `python3 -c "print(1)"`,
			out:  "1\n",
		},
		{
			name: "a -c line that runs another file's code",
			cmd:  `python3 -c "exec(open('solution.py').read())"`,
			out:  "  File \"<string>\", line 1\n    def broken(\nSyntaxError: invalid syntax",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := brokenInlineScriptSteer(tc.cmd, tc.out)
			if len(tc.want) == 0 && got != "" {
				t.Errorf("want no steer, got %q", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("steer lacks %q: %q", want, got)
				}
			}
		})
	}
}
