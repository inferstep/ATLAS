package main

import (
	"slices"
	"strings"
	"testing"
)

// The shell is a project-folder jail, so the one command policy refuses only
// what would be catastrophic even there: wiping the whole project, a fork
// bomb, destroying a device. A wrapper (bash -c, eval, env -S, sudo, ...)
// does not hide such a command; the wrappers themselves are parsed in
// shell_wrapper.go.
func TestValidateShellCommandBlocksCatastrophic(t *testing.T) {
	for _, group := range []struct {
		kind string
		cmds []string
	}{
		{"a delete of the whole project, a fork bomb, a device", []string{
			"rm -rf /",
			"rm -rf /workspace",
			"rm -rf /workspace/*",
			"rm -rf $HOME",
			"rm -rf .",
			"rm -rf *",
			"cd /workspace && rm -rf .",
			"find . -name '*.tmp' -delete",
			"find . -type f -exec rm {} \\;",
			":(){ :|:& };:",
			"mkfs.ext4 /dev/sda1",
			"dd if=/dev/zero of=/dev/sda bs=1M",
			"echo x > /dev/sda",
		}},
		{"the same inside a wrapper, or behind a prefix", []string{
			`bash -c "rm -rf /"`,
			`sh -c 'rm -rf /workspace'`,
			`dash -c "find . -delete"`,
			`eval "rm -rf $HOME"`,
			`bash -c ":(){ :|:& };:"`,
			`env ATLAS_FLAG=x bash -c 'rm -rf /'`,
			`nohup env ATLAS_FLAG=x bash -c 'rm -rf /workspace'`,
			`sudo -u root bash -c 'rm -rf /'`,
			`doas -u root sh -c 'find . -delete'`,
			`env -u HOME bash -c 'mkfs.ext4 /dev/sda1'`,
			`nice -n 5 bash -c 'rm -rf /'`,
			`stdbuf -o L bash -c 'rm -rf /'`,
			`timeout --signal TERM 5s bash -c 'rm -rf /'`,
			`env -S 'bash -c "rm -rf /"'`,
			`env '-Sbash -c "rm -rf /"'`,
			`env '--split-string=bash -c "rm -rf /"'`,
			`env -iS 'bash -c "rm -rf /"'`,
			`env '-ivSbash -c "rm -rf /"'`,
			`env '--split-str=bash -c "rm -rf /"'`,
			`env --s 'bash -c "rm -rf /"'`,
			`env -S 'rm -rf /'`,
			`env -S rm -rf /`,
			`env -S '-S "bash -c '\''rm -rf /'\''"'`,
			`env ATLAS_FLAG=x bash -lc 'rm -rf /'`,
			`bash -o pipefail -c 'rm -rf /'`,
			`bash -c -l 'rm -rf /'`,
			`bash -c -- 'rm -rf /'`,
			`bash -c -o pipefail 'rm -rf /'`,
			`bash -clo pipefail 'rm -rf /'`,
			`bash -c +e 'rm -rf /'`,
			`sh -c -e 'rm -rf /'`,
			`"b"'ash' -c 'rm -rf /'`,
			`env ATLAS_FLAG=x b\ash -c 'rm -rf /'`,
			`bash -c 'bash -c '\''rm -rf /'\'''`,
			`eval 'bash -c "rm -rf /"'`,
			`eval -- 'bash -c "rm -rf /"'`,
		}},
	} {
		for _, cmd := range group.cmds {
			t.Run(cmd, func(t *testing.T) {
				if validateShellCommand(cmd) == "" {
					t.Errorf("validateShellCommand(%q) allowed %s, want a refusal", cmd, group.kind)
				}
			})
		}
	}
}

// Everything else runs: managing files, building, testing, redirecting
// output, and the same wrappers around an ordinary command. The model is not
// made to reinvent the shell as tools.
func TestValidateShellCommandAllowsEverythingElse(t *testing.T) {
	for _, group := range []struct {
		kind string
		cmds []string
	}{
		{"managing files, building, testing, redirecting output", []string{
			"mv index.html templates/",
			"cd /workspace && mv app.py src/",
			"rm app.py",
			"rm -f stale.pyc",
			"rm -rf __pycache__",
			"rm -rf node_modules build",
			"echo bad > app.py",
			"pytest tests/",
			"python -c 'print(1)' > /dev/null",
			"python app.py >> server.log",
			"pytest -v 2> errors.log",
			"python app.py 2>&1",
			"node app.js >& output.log",
			"go test ./... 2>&1 | tee out.log",
			"python app.py > flask.log 2>&1 &",
			"python app.py >flask.log 2>&1",
			"bash --version",
			"python -c 'import flask; print(flask.__version__)'",
			"node -e 'console.log(1+1)'",
			"git log -c",
		}},
		{"a wrapper around an ordinary command", []string{
			`bash -c "python app.py"`,
			`sh -c 'pytest -q'`,
			`env ATLAS_FLAG=x bash -c 'python app.py'`,
			`sudo -u root bash -c 'python app.py'`,
			`env -u HOME bash -c 'go test ./...'`,
			`env -S 'bash -c "python app.py"'`,
			`env '-Sbash -c "python app.py"'`,
			`env '--split-string=bash -c "python app.py"'`,
			`env -iS 'bash -c "python app.py"'`,
			`env '-ivSbash -c "python app.py"'`,
			`env '--split-str=bash -c "python app.py"'`,
			`env --s 'bash -c "python app.py"'`,
			`env -S 'echo "rm -rf /; rm -rf /"'`,
			`env -S echo 'rm -rf /; rm -rf /'`,
			`env -S '-S "bash -c '\''python app.py'\''"'`,
			`env ATLAS_FLAG=x bash -lc 'python app.py'`,
			`bash -o pipefail -c 'python app.py'`,
			`bash -c -l 'python app.py'`,
			`bash -c -- 'python app.py'`,
			`bash -c -o pipefail 'python app.py'`,
			`bash -clo pipefail 'python app.py'`,
			`bash -c +e 'python app.py'`,
			`sh -c -e 'python app.py'`,
			`bash -c 'echo safe' 'rm -rf /'`,
			`bash -c 'echo safe' env -S 'bash -c "rm -rf /"'`,
			`"b"'ash' -c 'python app.py'`,
			`env ATLAS_FLAG=x b\ash -c 'python app.py'`,
			`bash -c 'bash -c '\''python app.py'\'''`,
			`eval 'echo rm -rf /'`,
			`eval -- 'echo rm -rf /'`,
			`bash -c 'echo rm -rf /'`,
		}},
	} {
		for _, cmd := range group.cmds {
			t.Run(cmd, func(t *testing.T) {
				if got := validateShellCommand(cmd); got != "" {
					t.Errorf("validateShellCommand(%q) refused %s: %s", cmd, group.kind, got)
				}
			})
		}
	}
}

// Coding models drift to /workspace, a path from their training data. When
// the project is somewhere else, a command that names /workspace is refused
// with the real directory, so the model corrects itself in one turn.
func TestValidateWorkingDirReference(t *testing.T) {
	for _, c := range []struct {
		name, workingDir, cmd string
		refused               bool
	}{
		{"cd into /workspace", "/home/isaac/snake", "cd /workspace && python app.py", true},
		{"a file under /workspace", "/home/isaac/snake", "python /workspace/app.py", true},
		{"/workspace at the end of the command", "/home/isaac/snake", "ls /workspace", true},
		{"the project is /workspace", "/workspace", "cd /workspace && python app.py", false},
		{"the project is under /workspace", "/workspace/myproject", "cd /workspace/myproject && pytest", false},
		{"workspace inside a longer name", "/home/isaac/snake", "ls /home/isaac/foo_workspace_dir/", false},
		{"no working directory yet", "", "cd /workspace && python app.py", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := validateWorkingDirReference(c.cmd, c.workingDir); (got != "") != c.refused {
				t.Errorf("validateWorkingDirReference(%q) = %q, want a refusal: %v", c.cmd, got, c.refused)
			}
		})
	}
}

// run_command and run_background ask the command policy first, then the
// /workspace check, so a command both refuse gets the policy's reason.
func TestValidateRunCommandChainsBothGates(t *testing.T) {
	for _, c := range []struct {
		name, cmd, want string // want: a part of the refusal, or "" for none
	}{
		{"a command both refuse", "rm -rf /workspace", "would wipe the whole project"},
		{"a /workspace path the policy allows", "cd /workspace && python app.py", "references /workspace"},
		{"a command both allow", "python app.py", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := validateRunCommand(c.cmd, "/home/isaac/snake")
			if (got == "") != (c.want == "") {
				t.Errorf("validateRunCommand(%q) = %q; refused: %v, want refused: %v", c.cmd, got, got != "", c.want != "")
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("validateRunCommand(%q) = %q, want %q in it", c.cmd, got, c.want)
			}
		})
	}
}

func TestSplitShellSegmentsRespectsQuotes(t *testing.T) {
	want := []string{"echo 'a;b'", " rm foo"}
	if got := splitShellSegments(`echo 'a;b'; rm foo`); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// An f-string that opens on a single quote and closes on a double: invalid
// Python as well as unparseable shell, so no re-quoting could save it; the
// shape has to change.
const brokenOneLiner = `python3 -c "from stats import mean, median; print(f'mean([1, 2, 3]): {mean([1, 2, 3])}"); print(f'median([1, 2, 3]): {median([1, 2, 3])}")"`

const bashParseErr = "bash: -c: line 1: syntax error near unexpected token `)'"

// The hint is for an inline program the shell could not parse: nothing ran,
// so sending it again fails the same way, and a file is the way out.
func TestTheQuotingHintIsOnlyForAnInlineProgramThatDidNotParse(t *testing.T) {
	hint := []string{"write_file", "nothing ran", "fails identically"}
	for _, c := range []struct {
		name, cmd, errText string
		wants              []string // nil: no hint
	}{
		{"a python one-liner", brokenOneLiner, bashParseErr, hint},
		{"a node one-liner", `node -e "console.log('x)"`, bashParseErr, hint},
		{"a perl one-liner", `perl -e 'print "x'`, bashParseErr, hint},
		{"a command that ran and failed", "pytest -q", "1 failed, 3 passed", nil},
		{"a script file", "python3 check.py", bashParseErr, nil},
		{"an empty command", "", bashParseErr, nil},
		{"an empty error", brokenOneLiner, "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := shellQuotingHint(c.cmd, c.errText)
			if (got != "") != (c.wants != nil) {
				t.Fatalf("hint %q, want a hint: %v", got, c.wants != nil)
			}
			for _, want := range c.wants {
				if !strings.Contains(got, want) {
					t.Errorf("hint lacks %q: %s", want, got)
				}
			}
		})
	}
}

// A server started with run_command holds it until the sandbox timeout and
// then reports a failure that says nothing about the code. The launchers that
// always serve are sent to run_background before they run. The pattern is
// narrow: a command that exits must not be refused.
func TestForegroundServerStartsAreRedirectedBeforeTheyRun(t *testing.T) {
	for _, c := range []struct {
		cmd        string
		redirected bool
	}{
		{"python3 -m http.server 8000", true},
		{"python -m http.server", true},
		{"cd site && python3 -m http.server 9000", true},
		{"npm run dev", true},
		{"npm start", true},
		{"flask run --port 5001", true},
		{"uvicorn main:app --reload", true},
		{"gunicorn app:app", true},
		{"php -S localhost:8000", true},
		{"npx vite", true},
		// Without the workspace reader, a script is not judged by its name.
		{"python app.py", false},
		{"pytest tests/", false},
		{"npm run build", false},
		{"npm test", false},
	} {
		t.Run(c.cmd, func(t *testing.T) {
			if got := foregroundServerRejectionWithSource(c.cmd, nil); (got != "") != c.redirected {
				t.Errorf("foregroundServerRejectionWithSource = %q, want a redirect: %v", got, c.redirected)
			}
		})
	}
	msg := foregroundServerRejectionWithSource("python3 -m http.server 8000", nil)
	for _, want := range []string{"run_background", "python3 -m http.server 8000", "curl", "stop_background"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the redirect lacks %q:\n%s", want, msg)
		}
	}
}

// A script is a server when the file it runs serves. That is read from the
// file, not guessed from its name: `python app.py` and `python solve.py` look
// the same from the command alone.
func TestForegroundServerStartIsRedirectedBeforeItRuns(t *testing.T) {
	files := map[string]string{
		"app.py":   "from flask import Flask\napp = Flask(__name__)\napp.run(port=5000)\n",
		"solve.py": "import sys\nprint(sum(int(x) for x in sys.stdin))\n",
	}
	read := func(rel string) (string, bool) { src, ok := files[rel]; return src, ok }
	for _, c := range []struct {
		name, cmd  string
		redirected bool
	}{
		{"a file that serves", "python app.py", true},
		{"a file that serves, run with a flag", "python -u app.py", true},
		{"a server start after an install that runs a module", "python3 -m pip install -r requirements.txt && python3 app.py", true},
		{"a server start after an install", "pip install flask; python3 app.py", true},
		{"a file that exits", "python3 solve.py", false},
		{"a file that exits, fed from stdin", "python3 solve.py < input.txt", false},
		{"a file that cannot be read", "python missing.py", false},
		{"a command that already detaches", "python app.py &", false},
		{"a module that compiles the file", "python3 -m py_compile app.py", false},
		{"a module, run by python", "python -m py_compile app.py", false},
		{"a module after a flag", "python3 -u -m py_compile app.py", false},
		{"inline code that parses the file", `python3 -c "import ast; ast.parse(open('app.py').read())"`, false},
		{"an install, then a compile", "pip install flask && python3 -m py_compile app.py", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := foregroundServerRejectionWithSource(c.cmd, read)
			if (got != "") != c.redirected {
				t.Fatalf("foregroundServerRejectionWithSource(%q) = %q, want a redirect: %v", c.cmd, got, c.redirected)
			}
			if c.redirected && !strings.Contains(got, "run_background") {
				t.Errorf("the redirect does not name run_background:\n%s", got)
			}
		})
	}
}

// buildToolDescriptionsExcluding is in tools.go. It is the text the model
// reads when it picks a tool: run_command must say that it waits and that a
// server belongs to run_background, and run_background must name a server
// command.
func TestToolDescriptionsSendServersToRunBackground(t *testing.T) {
	docs := buildToolDescriptionsExcluding(nil)
	for _, want := range []string{"run_background", "doesn't exit", "WAIT for it to exit", "python app.py"} {
		if !strings.Contains(docs, want) {
			t.Errorf("the tool descriptions lack %q", want)
		}
	}
}
