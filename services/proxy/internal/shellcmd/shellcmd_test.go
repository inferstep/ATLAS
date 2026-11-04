package shellcmd

import (
	"slices"
	"strings"
	"testing"
)

func TestClassifyCommandEvidence(t *testing.T) {
	cases := []struct {
		cmd    string
		kind   CommandEvidenceKind
		masked bool
	}{
		// The program or its tests ran, and the line reports how.
		{"python3 app.py", EvidenceExecution, false},
		{"timeout 10 python3 app.py", EvidenceExecution, false},
		{"FOO=1 python3 app.py", EvidenceExecution, false},
		{"cd app && python3 main.py", EvidenceExecution, false},
		{"(cd app && pytest)", EvidenceExecution, false},
		{"(cd app && pytest) > log.txt 2>&1", EvidenceExecution, false},
		{`bash -c "pytest -q"`, EvidenceExecution, false},
		{`bash -lc 'pytest -q'`, EvidenceExecution, false},
		{`sh -ec "python3 a.py; python3 b.py"`, EvidenceExecution, false},
		{`bash -euo pipefail -c "pytest | tail -5"`, EvidenceExecution, false},
		{"uv run pytest", EvidenceExecution, false},
		{"uv run --with requests app.py", EvidenceExecution, false},
		{"poetry run python app.py", EvidenceExecution, false},
		{"bundle exec rspec", EvidenceExecution, false},
		{"npx --yes jest", EvidenceExecution, false},
		{"node app.js", EvidenceExecution, false},
		{"java -jar app.jar", EvidenceExecution, false},
		{"java App.java", EvidenceExecution, false},
		{"javac App.java && java App", EvidenceExecution, false},
		{"php index.php", EvidenceExecution, false},
		{"bash run.sh", EvidenceExecution, false},
		{"go run main.go", EvidenceExecution, false},
		{"./solve.py", EvidenceExecution, false},
		{"echo '1 2' | python3 solve.py", EvidenceExecution, false},
		{"python3 solve.py < input.txt", EvidenceExecution, false},
		{"pytest || exit 1", EvidenceExecution, false},
		{"set -o pipefail; pytest | tail -5", EvidenceExecution, false},
		{"set -e; pytest; echo done", EvidenceExecution, false},
		{"make test", EvidenceExecution, false},
		{"make check", EvidenceExecution, false},
		{"pytest && ruff check .", EvidenceExecution, false},
		{"python3 -m unittest discover", EvidenceExecution, false},
		{"python3 - <<'EOF'\nimport app\nprint(app.solve())\nEOF", EvidenceExecution, false},
		{"lua main.lua", EvidenceExecution, false},
		{"bun app.ts", EvidenceExecution, false},
		{"dotnet test", EvidenceExecution, false},
		{"swift main.swift", EvidenceExecution, false},
		{"mvn test", EvidenceExecution, false},

		// Well formed is not working.
		{"python3 -m py_compile app.py", EvidenceStatic, false},
		{`python3 -c "import ast; ast.parse(open('app.py').read())"`, EvidenceStatic, false},
		{`python3 -c "import ast, sys; ast.parse(open(sys.argv[1]).read())" app.py`, EvidenceStatic, false},
		{"node --check app.js", EvidenceStatic, false},
		{"bash -n run.sh", EvidenceStatic, false},
		{"php -l index.php", EvidenceStatic, false},
		{"ruby -c app.rb", EvidenceStatic, false},
		{"perl -c app.pl", EvidenceStatic, false},
		{"deno check main.ts", EvidenceStatic, false},
		{"bun build", EvidenceStatic, false},
		{"swift build", EvidenceStatic, false},
		{"java -version", EvidenceStatic, false},
		{`cd app && bash -c "ruff check ."`, EvidenceStatic, false},
		{"ruff check .", EvidenceStatic, false},
		{"mypy app.py", EvidenceStatic, false},
		{"npx tsc --noEmit", EvidenceStatic, false},
		{"go build ./...", EvidenceStatic, false},
		{"go vet ./...", EvidenceStatic, false},
		{"cargo build", EvidenceStatic, false},
		{"npm run build", EvidenceStatic, false},
		{"npm run lint:fix", EvidenceStatic, false},
		{"yarn build", EvidenceStatic, false},
		{"make", EvidenceStatic, false},
		{"make lint", EvidenceStatic, false},
		{"make typecheck", EvidenceStatic, false},
		{"pytest --collect-only -q", EvidenceStatic, false},
		{"python3 -m pytest --co", EvidenceStatic, false},
		{"python3 --version", EvidenceStatic, false},
		{"node -v", EvidenceStatic, false},
		{"python3 app.py --help", EvidenceStatic, false},
		{"node app.js --version", EvidenceStatic, false},
		{"./app --version", EvidenceStatic, false},
		{"python3 -m py_compile app.py && echo OK", EvidenceStatic, false},

		// Recon and setup.
		{"ls -la", EvidenceNone, false},
		{"cat app.py", EvidenceNone, false},
		{"echo hi", EvidenceNone, false},
		{"pip install flask", EvidenceNone, false},
		{"python3 -m pip install flask", EvidenceNone, false},
		{"python3 -m venv .venv", EvidenceNone, false},
		{"npm install", EvidenceNone, false},
		{"make install", EvidenceNone, false},
		{"curl -I http://localhost:5000/", EvidenceNone, false},
		{"java", EvidenceNone, false},
		{"FOO=1", EvidenceNone, false},

		// The verifying part ran, and the line's status is something else's.
		{"pytest | tail -5", EvidenceNone, true},
		{"pytest -q 2>&1 | tail -20", EvidenceNone, true},
		{"pytest || true", EvidenceNone, true},
		{"python3 app.py; echo done", EvidenceNone, true},
		{"python3 app.py &", EvidenceNone, true},
		{"python3 app.py 2>&1 | tee out.log", EvidenceNone, true},
		{"pytest && echo PASS || echo FAIL", EvidenceNone, true},
		{"python3 app.py | grep -q 42", EvidenceNone, true},
		{`bash -c "pytest | tail -5"`, EvidenceNone, true},
		// curl without -f exits 0 on an error page.
		{"curl http://localhost:5000/", EvidenceNone, true},
		{"curl -s http://localhost:5000/ | head -5", EvidenceNone, true},

		// A page fetched in a way an HTTP error would fail.
		{"curl -sf http://localhost:5000/", evidenceProbe, false},
		{"curl -s --fail-with-body http://localhost:5000/", evidenceProbe, false},
		{"curl -s http://localhost:5000/ | grep -q Welcome", evidenceProbe, false},
		{"curl -s http://localhost:5000/api | jq -e .ok", evidenceProbe, false},
		{"wget -qO- http://localhost:5000/", evidenceProbe, false},
		// The background start is hidden, the probe after it is not.
		{"python3 app.py & sleep 1 && curl -sf localhost:5000", evidenceProbe, true},
	}
	for _, c := range cases {
		ev := ClassifyCommandEvidence(c.cmd)
		if ev.Kind != c.kind || ev.Masked != c.masked {
			t.Errorf("%q: kind=%s masked=%v, want kind=%s masked=%v",
				c.cmd, ev.Kind, ev.Masked, c.kind, c.masked)
		}
		if ev.Masked && ev.MaskNote == "" {
			t.Errorf("%q: masked with no note for the model", c.cmd)
		}
	}
}

// An escaped operator, a backtick substitution and a here-string stay inside
// their segment; `|&` is a pipe.
func TestSplitTopLevelShell(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		segs []string
		ops  []string
	}{
		{`echo a\;b; pytest`, []string{`echo a\;b`, " pytest"}, []string{";"}},
		{"echo `a; b` && pytest", []string{"echo `a; b` ", " pytest"}, []string{"&&"}},
		{`python3 app.py <<< "1 2" && pytest`, []string{`python3 app.py <<< "1 2" `, " pytest"}, []string{"&&"}},
		{"pytest |& tail", []string{"pytest ", " tail"}, []string{"|"}},
	} {
		segs, ops, _ := SplitTopLevelShell(c.cmd)
		if !slices.Equal(segs, c.segs) || !slices.Equal(ops, c.ops) {
			t.Errorf("SplitTopLevelShell(%q) = %q, %q; want %q, %q", c.cmd, segs, ops, c.segs, c.ops)
		}
	}
}

// ProgramWords looks past each wrapper to the program it runs.
func TestProgramWords(t *testing.T) {
	for in, want := range map[string]string{
		"nohup python3 app.py":              "python3 app.py",
		"env -i FOO=1 python3 app.py":       "python3 app.py",
		"nice -n 5 python3 app.py":          "python3 app.py",
		"stdbuf -oL python3 app.py":         "python3 app.py",
		"timeout -s KILL 10 python3 app.py": "python3 app.py",
		"time":                              "",
	} {
		if got := strings.Join(ProgramWords(ShellFields(in)), " "); got != want {
			t.Errorf("ProgramWords(%q) = %q, want %q", in, got, want)
		}
	}
}

// A prefix option that takes a separate value has that value skipped too, so
// the word found is the program the prefix runs, not the option's value. (The
// guard tests run sudo -u, doas -u, env -u, timeout --signal and stdbuf -o.)
func TestCommandPositionSkipsTheValueOfEachPrefixOption(t *testing.T) {
	for _, cmd := range []string{
		"sudo --user nobody python3 app.py",
		"sudo -g staff python3 app.py",
		"sudo --group staff python3 app.py",
		"sudo -h buildhost python3 app.py",
		"sudo --host buildhost python3 app.py",
		"sudo -p prompt python3 app.py",
		"sudo --prompt prompt python3 app.py",
		"sudo -r role python3 app.py",
		"sudo --role role python3 app.py",
		"sudo -t type python3 app.py",
		"sudo --type type python3 app.py",
		"env --unset HOME python3 app.py",
		"env -C /tmp python3 app.py",
		"env --chdir /tmp python3 app.py",
		"timeout -s KILL 5 python3 app.py",
		"stdbuf -i L python3 app.py",
		"stdbuf -e L python3 app.py",
		"time -f %e python3 app.py",
		"time --format %e python3 app.py",
		"time -o out.txt python3 app.py",
		"time --output out.txt python3 app.py",
	} {
		fields := ShellFields(cmd)
		if got := strings.Join(fields[CommandPosition(fields):], " "); got != "python3 app.py" {
			t.Errorf("CommandPosition(%q) runs %q, want python3 app.py", cmd, got)
		}
	}
}

// --- what the loop records -----------------------------------------------------

// A segment that runs no program, or a program that is not a runner, adds
// nothing; `python -m unittest` takes test*.py; a Go package run takes only
// the package's .go files; a file two roots discover is listed once.
func TestRunnerEntries(t *testing.T) {
	for _, c := range []struct {
		command    string
		candidates []string
		want       []string
	}{
		{"FOO=1 && python3 -m unittest", []string{"test_calc.py", "calc.py"}, []string{"test_calc.py"}},
		{"python3 app.py && pytest", []string{"app.py", "test_app.py"}, []string{"test_app.py"}},
		{"go run .", []string{"main.go", "notes.py"}, []string{"main.go"}},
		{"pytest tests tests/unit", []string{"tests/unit/test_u.py"}, []string{"tests/unit/test_u.py"}},
	} {
		if got := RunnerEntries(c.command, "/w", c.candidates); !slices.Equal(got, c.want) {
			t.Errorf("RunnerEntries(%q) = %q, want %q", c.command, got, c.want)
		}
	}
}

// shellScript gives the script that a shell runs inline, with the options the
// shell was started with put in front of it. A shell without a -c script gives
// none.
func TestShellScript(t *testing.T) {
	for _, c := range []struct {
		seg    string
		script string
		ok     bool
	}{
		{`bash -c "x"`, "x", true},
		{`bash -lc 'x'`, "x", true},
		{`sh -ec "x"`, "set -e\nx", true},
		{`bash -o pipefail -c "x"`, "set -o pipefail\nx", true},
		{`bash -euo pipefail -c "x"`, "set -e\nset -o pipefail\nx", true},
		{"bash run.sh", "", false},
		{"sh -e", "", false},
		{"bash", "", false},
		{"bash -c", "", false},
		{`bash -n -c "x"`, "", false},
		{`python3 -c "x"`, "", false},
	} {
		script, ok := shellScript(c.seg)
		if script != c.script || ok != c.ok {
			t.Errorf("shellScript(%q) = %q, %v; want %q, %v", c.seg, script, ok, c.script, c.ok)
		}
	}
}

// compileStep judges each build tool by its own rule: a compiler always
// compiles; go, cargo, dotnet and swift compile with `build`; Maven and Gradle
// with compile, build, package or assemble; npm, pnpm and yarn with a build
// script; make with any target that is not a test or a run.
func TestCompileStep(t *testing.T) {
	for seg, want := range map[string]bool{
		"javac App.java":     true,
		"gcc main.c -o main": true,
		"python3 app.py":     false,
		"go build ./...":     true,
		"go test ./...":      false,
		"cargo build":        true,
		"cargo package":      false,
		"dotnet build":       true,
		"dotnet test":        false,
		"swift build":        true,
		"swift package":      false,
		"mvn compile":        true,
		"mvn test":           false,
		"./mvnw package":     true,
		"./mvnw verify":      false,
		"gradle assemble":    true,
		"gradle test":        false,
		"./gradlew build":    true,
		"./gradlew run":      false,
		"npm run build":      true,
		"npm test":           false,
		"pnpm build":         true,
		"pnpm test":          false,
		"yarn run compile":   true,
		"yarn start":         false,
		"make":               true,
		"make test":          false,
	} {
		if got := compileStep(seg); got != want {
			t.Errorf("compileStep(%q) = %v, want %v", seg, got, want)
		}
	}
}

// Inline code that touches no file the session changed ran nothing the work is
// made of. `python -c "print(1)"` must not pass the fix-intent gate or clear
// an earlier red run of the real program.
