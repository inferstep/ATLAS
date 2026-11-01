package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Agent loop — iterative tool-calling loop between model and executors
// ---------------------------------------------------------------------------

// runAgentLoop runs the agent loop for a single user request.
// The model emits tool calls (constrained by grammar), the proxy executes them,
// and returns results. Continues until the model emits "done" or max turns hit.
func runAgentLoop(ctx *AgentContext, userMessage string) error {
	loopStart := beginAgentRun(ctx, userMessage)
	defer endAgentStage(ctx, loopStart)
	if !openConversation(ctx, userMessage) {
		return nil
	}

	// edit_file old_str-mismatch failures per path. A successful read_file
	// between attempts resets consecutiveErrors/RecentFailurePaths, which
	// masks the classic read→edit-miss→read loop (smaller models can't
	// reproduce old_str byte-for-byte). This counter survives interleaved
	// reads so we can force the structural_edit steer after the second miss.
	editMissByPath := map[string]int{}
	// THE output-obligation decision, made once, from the contract the
	// request boundary already validated. Nothing the model emits later can
	// change which source spoke.
	outputObligation := resolveOutputObligation(ctx, userMessage)
	loop := newAgentLoop(ctx, userMessage, outputObligation)
	st := loop.st

	for turn := 0; ctx.MaxTurns <= 0 || turn < ctx.MaxTurns; turn++ {
		response, parsed, outcome, err := loop.takeModelTurn(turn)
		switch outcome {
		case turnEnds:
			continue
		case runEnds:
			return err
		}

		switch parsed.Type {
		case "done":
			if loop.endOnDone(turn, parsed) {
				return nil
			}

		case "text":
			if loop.endOnText(turn, parsed) {
				return nil
			}

		case "tool_call":
			st.toolsRun++
			st.pendingToolCall = parsed.Name
			intentArgs := announceToolCall(ctx, turn, parsed)
			switch loop.screenBeforeWorkspaceCheck(turn, response, parsed, intentArgs) {
			case turnEnds:
				continue
			case runEnds:
				return nil
			}

			// Enforce the workspace boundary before any pre-execution gate reads
			// a path. executeToolCall repeats this check for parallel dispatch.
			if rejection := enforceWorkspacePaths(&parsed, ctx); rejection != "" {
				if loop.refuseOutsideWorkspace(parsed, intentArgs, rejection) {
					return nil
				}
				continue
			}

			switch loop.screenAfterWorkspaceCheck(turn, response, &parsed, intentArgs) {
			case turnEnds:
				continue
			case runEnds:
				return nil
			}
			if parsed.Name == "write_file" {
				var wfInput WriteFileInput
				if json.Unmarshal(parsed.Args, &wfInput) == nil {
					existingPath := resolveAgentPath(ctx, wfInput.Path)
					existing, readErr := os.ReadFile(existingPath)
					switch loop.guardExistingFileWrite(parsed, intentArgs, wfInput, existingPath, existing, readErr) {
					case turnEnds:
						continue
					case runEnds:
						return nil
					}
				}
			}

			result, steers, callOutcome, callErr := loop.runToolCall(turn, parsed, intentArgs)
			switch callOutcome {
			case turnEnds:
				continue
			case runEnds:
				return callErr
			}
			if result.Success && len(ctx.LiteralBlocks) > 0 &&
				(parsed.Name == "write_file" || parsed.Name == "edit_file" ||
					parsed.Name == "structural_edit" ||
					parsed.Name == "insert_after" || parsed.Name == "replace_lines") {
				// Literal-contract enforcement: the user's own bytes are the
				// authoritative rendering of any content they spelled out, and
				// the model measurably cannot transcribe bytes (space-prefixed
				// BPE tokens win after quotes — `BANNER = "ready"` arrives as
				// `BANNER = " ready"` deterministically). Spacing drift within
				// lines of a stated literal is repaired in place; indentation
				// is never changed (repairLiteralDrift).
				var wp struct {
					Path string `json:"path"`
				}
				if json.Unmarshal(parsed.Args, &wp) == nil && wp.Path != "" {
					lp := resolveAgentPath(ctx, wp.Path)
					if body, rerr := os.ReadFile(lp); rerr == nil {
						if fixed, repairedLits, changed := repairLiteralDrift(string(body), ctx.LiteralBlocks); changed {
							if werr := os.WriteFile(lp, []byte(fixed), 0o644); werr == nil {
								reportLiteralRepair(ctx, result, wp.Path, lp, fixed, repairedLits)
							}
						}
					}
				}
			}
			loop.recordCallEffects(turn, parsed, result)

			// edit_file old_str miss: count per path independently of the
			// consecutiveErrors reset an interleaved read causes. On the
			// second miss for the same structured file, force the structural_edit
			// steer as a [system note] (the inline tool-error hint alone
			// doesn't reliably move a small model off edit_file).
			if !result.Success && parsed.Name == "edit_file" &&
				strings.Contains(result.Error, "string to replace not found") {
				mp := extractFailurePath(parsed.Name, parsed.Args)
				editMissByPath[mp]++
				ext := strings.ToLower(filepath.Ext(mp))
				// Force the structural_edit steer on the FIRST miss for structured
				// files — small models bail to run_command after a single
				// edit_file miss rather than retrying, so waiting for a
				// second miss never fires (observed: 1 edit_file all session,
				// then 9 run_command re-runs).
				if editMissByPath[mp] >= 1 && (ext == ".py" || ext == ".html" || ext == ".htm") {
					var source string
					if b, rerr := os.ReadFile(resolveAgentPath(ctx, mp)); rerr == nil {
						source = string(b)
					}
					steers.repeat = editMissSteer(mp, source)
					log.Printf("[agent] edit_file miss on %q — forcing structural_edit steer", mp)
				}
			}

			if loop.breakErrorLoop(turn, parsed, result) {
				return nil
			}
			loop.answerToolCall(turn, response, parsed, result, steers)

		default:
			askForAKnownResponseType(ctx, parsed)
		}
	}

	loop.endOutOfTurns()
	return nil
}

// fetchFencedContent asks the model for a file's contents in its native
// channel: a single fenced block (attempt 0 constrained by the fence grammar, a
// retry as free text).
//
// Code emitted INSIDE a JSON string pays escaping pressure on every dense line,
// and slips such as a missing close-paren, a literal \n fusing a statement into
// a comment, or list joins losing their spaces are this one channel problem.
// The envelope stays under the JSON constraint; only the file body moves to
// plain text.
//
// The sub-call is ephemeral: nothing is appended to ctx.Messages, so from the
// main conversation's view the model wrote "@fenced" and the write simply
// happened.
func fetchFencedContent(ctx *AgentContext, rawCall, path string) (string, error) {
	if err := fencedFetchRefusal(ctx, path); err != nil {
		return "", err
	}
	tag := fenceTagForPath(path)
	note := AgentMessage{Role: "user", Content: fmt.Sprintf(
		"[system note]: Now provide ONLY the complete contents of %s, as plain "+
			"code in a single fenced block, fenced with FOUR backticks (````%s ... ````) "+
			"so a ``` line inside the file stays inside it. No JSON, no "+
			"commentary, no partial file.", path, tag)}
	msgs := append(append([]AgentMessage{}, ctx.Messages...),
		AgentMessage{Role: "assistant", Content: rawCall}, note)
	fetch := &fencedFetch{ctx: ctx, path: path, tag: tag, msgs: msgs}
	// Attempts remaining are what the SESSION still allows for this path, not
	// a fresh two. Requirement: a new write_file call must not restore the
	// allowance a previous turn spent.
	remaining := maxFencedFailuresPerPath - ctx.FencedFailures[fencedKey(ctx, path)]
	for attempt := 0; attempt < remaining; attempt++ {
		if attempt > 0 && !fencedFitsRemainingBudget(ctx) {
			break // a retry that cannot finish and still be validated
		}
		// A stall on attempt 0 already raised FencedStalls to the disable
		// limit; a retry would only pay another watchdog cut for a channel
		// the session has given up on. Stop here and let the caller steer the
		// model inline, reclaiming that ~25s on the file that first stalls.
		if attempt > 0 && fencedChannelDisabledForSession(ctx) {
			break
		}
		attemptStart := time.Now()
		// Attempt 0 constrains decoding to the contract the note describes:
		// one fenced block with the requested tag. Probe M (stabilization
		// cycle 4): 44 of 49 captured sub-calls returned clean usable content
		// that way, against 19 of 49 as free text, with no empty replies at
		// all (27 of 49 as free text) and a median of 12 s against 60 s.
		// A later attempt drops the grammar, so a server that refuses it
		// still gets the free-text request this channel has always sent.
		grammar := fenceBlockGrammar(tag)
		current, _ := os.ReadFile(resolveAgentPath(ctx, path)) // absent: a new file
		if attempt > 0 || !fencedGrammarFits(string(current)) {
			grammar = rawEmissionSentinel
		}
		answer := fetch.ask(grammar, attempt, attemptStart)
		if content, done, err := fetch.settle(answer, attempt); done {
			return content, err
		}
	}
	if fetch.lastErr != nil {
		return "", fmt.Errorf("fenced resolution for %s was cut after %d attempt(s) "+
			"this session (%v); send the file inline or edit it instead",
			path, ctx.FencedFailures[fencedKey(ctx, path)], fetch.lastErr)
	}
	return "", fmt.Errorf("no fenced block after %d attempt(s) for %s this session; "+
		"send the file inline or edit it instead",
		ctx.FencedFailures[fencedKey(ctx, path)], path)
}

// Toolchain describes one language ecosystem detected in the project.
// The fields are surfaced into the system prompt so the model knows
// which runner to invoke and how to install deps if needed.
//
// Detection is manifest-driven: presence of pyproject.toml means
// Python, package.json means Node, Cargo.toml means Rust, etc. A
// polyglot project (React frontend + Django backend + deploy scripts)
// returns multiple Toolchains so the model can pick the right one
// per file edit.
type Toolchain struct {
	Name           string   // canonical key: "python", "node", "rust", "go", "ruby", "java-maven", "java-gradle", "php", "dotnet", "dart"
	Manifests      []string // manifest files found relative to workingDir (e.g. ["pyproject.toml", "requirements.txt"])
	Runner         string   // command to run the project's main entry (e.g. "/workspace/venv/bin/python", "node", "cargo run", "go run .")
	PackageManager string   // detected pkg manager when ambiguous (npm vs pnpm vs yarn vs bun for Node)
	InstallCommand string   // command to install deps from lockfile (e.g. "npm ci", "pip install -r requirements.txt")
	TestCommand    string   // best-guess test runner ("pytest", "npm test", "cargo test", ...)
}

// detectProjectToolchains scans workingDir for language manifests and
// returns one Toolchain per detected ecosystem. Polyglot projects
// (e.g. React + Django) produce multiple entries. Empty slice means
// no recognized manifest was found at the root.
//
// We deliberately only look ONE level deep at the root — most
// monorepos have manifests in subdirs (apps/web/package.json,
// services/api/pyproject.toml) but probing deeper here would be
// expensive and noisy. The model can still discover deep manifests
// via list_directory / read_file when it needs to.
func detectProjectToolchains(workingDir string) []Toolchain {
	if workingDir == "" {
		return nil
	}
	var out []Toolchain

	// Python — venv-aware so the runner points at the project's
	// pinned interpreter when one exists.
	pyManifests := pickExisting(workingDir, "pyproject.toml", "requirements.txt", "setup.py", "Pipfile", "poetry.lock")
	if len(pyManifests) > 0 || detectProjectVenvPython(workingDir) != "" {
		out = append(out, pythonToolchain(workingDir, pyManifests))
	}

	// Node / TypeScript — pkg manager picked from lockfile.
	if hasFile(workingDir, "package.json") {
		pm, install := nodePackageManager(workingDir)
		runner := "node"
		if hasFile(workingDir, "tsconfig.json") {
			runner = "tsx" // ts/jsx-aware launcher; falls back to node for plain .js
		}
		out = append(out, Toolchain{
			Name: "node", Manifests: pickExisting(workingDir, "package.json", "tsconfig.json"),
			Runner: runner, PackageManager: pm, InstallCommand: install,
			TestCommand: pm + " test",
		})
	}

	// Rust
	if hasFile(workingDir, "Cargo.toml") {
		out = append(out, Toolchain{
			Name: "rust", Manifests: pickExisting(workingDir, "Cargo.toml", "Cargo.lock"),
			Runner: "cargo run", InstallCommand: "cargo fetch",
			TestCommand: "cargo test",
		})
	}

	// Go
	if hasFile(workingDir, "go.mod") {
		out = append(out, Toolchain{
			Name: "go", Manifests: pickExisting(workingDir, "go.mod", "go.sum"),
			Runner: "go run .", InstallCommand: "go mod download",
			TestCommand: "go test ./...",
		})
	}

	// Ruby
	if hasFile(workingDir, "Gemfile") {
		out = append(out, Toolchain{
			Name: "ruby", Manifests: pickExisting(workingDir, "Gemfile", "Gemfile.lock"),
			Runner: "bundle exec ruby", InstallCommand: "bundle install",
			TestCommand: "bundle exec rspec",
		})
	}

	// Java — Maven
	if hasFile(workingDir, "pom.xml") {
		out = append(out, Toolchain{
			Name: "java-maven", Manifests: []string{"pom.xml"},
			Runner: "mvn exec:java", InstallCommand: "mvn install -DskipTests",
			TestCommand: "mvn test",
		})
	}

	// Java/Kotlin — Gradle (prefer wrapper if present)
	if hasFile(workingDir, "build.gradle") || hasFile(workingDir, "build.gradle.kts") {
		runner, install, test := gradleCommands(workingDir)
		out = append(out, Toolchain{
			Name: "java-gradle", Manifests: pickExisting(workingDir, "build.gradle", "build.gradle.kts", "settings.gradle", "gradlew"),
			Runner: runner, InstallCommand: install, TestCommand: test,
		})
	}

	// PHP / Composer
	if hasFile(workingDir, "composer.json") {
		out = append(out, Toolchain{
			Name: "php", Manifests: pickExisting(workingDir, "composer.json", "composer.lock"),
			Runner: "php", InstallCommand: "composer install",
			TestCommand: "vendor/bin/phpunit",
		})
	}

	// .NET — pick the first project file we find
	if csproj := firstMatchingGlob(workingDir, "*.csproj", "*.fsproj", "*.sln"); csproj != "" {
		out = append(out, Toolchain{
			Name: "dotnet", Manifests: []string{csproj},
			Runner: "dotnet run", InstallCommand: "dotnet restore",
			TestCommand: "dotnet test",
		})
	}

	// Dart / Flutter
	if hasFile(workingDir, "pubspec.yaml") {
		out = append(out, dartToolchain(workingDir))
	}

	return out
}

// pythonToolchain is the Python entry: the project's own venv interpreter when
// it has one, and the install command its lockfile or manifest calls for.
func pythonToolchain(workingDir string, pyManifests []string) Toolchain {
	runner := detectProjectVenvPython(workingDir)
	if runner == "" {
		runner = "python"
	}
	install := "pip install -r requirements.txt"
	if hasFile(workingDir, "poetry.lock") {
		install = "poetry install"
	} else if hasFile(workingDir, "Pipfile.lock") {
		install = "pipenv install"
	} else if hasFile(workingDir, "pyproject.toml") && !hasFile(workingDir, "requirements.txt") {
		install = "pip install -e ."
	}
	return Toolchain{
		Name: "python", Manifests: pyManifests,
		Runner: runner, InstallCommand: install,
		TestCommand: "pytest",
	}
}

// nodePackageManager picks Node's package manager, and the command that
// installs with it, from the lockfile the project has.
func nodePackageManager(workingDir string) (string, string) {
	pm, install := "npm", "npm install"
	switch {
	case hasFile(workingDir, "pnpm-lock.yaml"):
		pm, install = "pnpm", "pnpm install --frozen-lockfile"
	case hasFile(workingDir, "yarn.lock"):
		pm, install = "yarn", "yarn install --frozen-lockfile"
	case hasFile(workingDir, "bun.lockb"):
		pm, install = "bun", "bun install --frozen-lockfile"
	case hasFile(workingDir, "package-lock.json"):
		pm, install = "npm", "npm ci"
	}
	return pm, install
}

// gradleCommands picks Gradle's run, install and test commands, through the
// project's own wrapper when it has one.
func gradleCommands(workingDir string) (string, string, string) {
	runner := "gradle run"
	install := "gradle build -x test"
	test := "gradle test"
	if hasFile(workingDir, "gradlew") {
		runner = "./gradlew run"
		install = "./gradlew build -x test"
		test = "./gradlew test"
	}
	return runner, install, test
}

// dartToolchain is the Dart entry, run through Flutter when the project is a
// Flutter app.
func dartToolchain(workingDir string) Toolchain {
	runner, install := "dart run", "dart pub get"
	if hasFile(workingDir, ".flutter-plugins") || hasFile(workingDir, "flutter.yaml") {
		runner, install = "flutter run", "flutter pub get"
	}
	return Toolchain{
		Name: "dart", Manifests: pickExisting(workingDir, "pubspec.yaml", "pubspec.lock"),
		Runner: runner, InstallCommand: install,
		TestCommand: "dart test",
	}
}

// probeToolchainReady returns a short status string for a Toolchain
// that's safe to run from buildSystemPrompt — meaning: it MUST be
// purely filesystem-based (no shelling out, no network). The model
// uses this to decide whether to install deps or skip straight to
// verification.
//
// We can't actually invoke `python -c "import flask"` here without
// running a subprocess in the sandbox, which is too expensive for
// every system-prompt build. Instead we look for filesystem evidence
// that deps are installed: venv with site-packages populated,
// node_modules present, target/debug/ for Rust, vendor/ for Ruby/Go,
// etc. False positives are fine ("looks installed but isn't" — the
// model will discover that on first verify and install). False
// negatives are bad — they push the model toward unnecessary
// reinstalls. Bias toward "ready" when the evidence is ambiguous.
func probeToolchainReady(workingDir string, tc Toolchain) string {
	switch tc.Name {
	case "python":
		for _, vd := range []string{"venv", ".venv", "env", ".env-py"} {
			sp := filepath.Join(workingDir, vd, "lib")
			if entries, err := os.ReadDir(sp); err == nil {
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), "python") && e.IsDir() {
						if hasUserPackages(filepath.Join(sp, e.Name(), "site-packages")) {
							return "ready"
						}
					}
				}
			}
		}
		if hasFile(workingDir, "requirements.txt") || hasFile(workingDir, "pyproject.toml") {
			return "needs install"
		}
		return "no manifest"

	case "node":
		if entries, err := os.ReadDir(filepath.Join(workingDir, "node_modules")); err == nil && len(entries) > 0 {
			return "ready"
		}
		return "needs install"

	case "rust":
		if info, err := os.Stat(filepath.Join(workingDir, "target")); err == nil && info.IsDir() {
			return "warm"
		}
		return "cold"

	case "go":
		if info, err := os.Stat(filepath.Join(workingDir, "vendor")); err == nil && info.IsDir() {
			return "vendored"
		}
		if hasFile(workingDir, "go.sum") {
			return "ready"
		}
		return "needs `go mod tidy`"

	case "ruby":
		if info, err := os.Stat(filepath.Join(workingDir, "vendor", "bundle")); err == nil && info.IsDir() {
			return "ready"
		}
		return "needs install"

	case "java-maven", "java-gradle":
		dir := "target"
		if tc.Name == "java-gradle" {
			dir = "build"
		}
		if info, err := os.Stat(filepath.Join(workingDir, dir)); err == nil && info.IsDir() {
			return "warm"
		}
		return "cold"

	case "php":
		if info, err := os.Stat(filepath.Join(workingDir, "vendor")); err == nil && info.IsDir() {
			return "ready"
		}
		return "needs install"

	case "dotnet":
		if info, err := os.Stat(filepath.Join(workingDir, "bin")); err == nil && info.IsDir() {
			return "warm"
		}
		return "cold"

	case "dart":
		if info, err := os.Stat(filepath.Join(workingDir, ".dart_tool")); err == nil && info.IsDir() {
			return "ready"
		}
		return "needs install"
	}
	return ""
}

// displayRelativeRunner converts an absolute runner path to its
// project-relative form when it lives under workingDir. Compresses
// `/workspace/venv/bin/python` to `venv/bin/python` in prompt output —
// matches the existing "use relative paths" rule and stops the model
// confusing itself into emitting `workspace/app.py`.
func displayRelativeRunner(runner, workingDir string) string {
	if !filepath.IsAbs(runner) {
		return runner
	}
	if rel, err := filepath.Rel(workingDir, runner); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return runner
}

// hasUserPackages returns true when site-packages contains anything
// beyond pip/setuptools/wheel — i.e. the user has installed real
// project deps. Empty / pip-only venvs return false.
func hasUserPackages(sitePackages string) bool {
	entries, err := os.ReadDir(sitePackages)
	if err != nil {
		return false
	}
	skip := map[string]bool{
		"pip": true, "setuptools": true, "wheel": true,
		"pkg_resources": true, "_distutils_hack": true,
		"__pycache__": true,
	}
	for _, e := range entries {
		name := e.Name()
		// Strip dist-info / egg-info suffixes for the skip check.
		if i := strings.Index(name, "-"); i > 0 {
			name = name[:i]
		}
		if strings.HasSuffix(e.Name(), ".dist-info") || strings.HasSuffix(e.Name(), ".egg-info") {
			continue
		}
		if !skip[name] && !strings.HasPrefix(name, "_") {
			return true
		}
	}
	return false
}

// hasFile returns true when workingDir/name exists as a file.
func hasFile(workingDir, name string) bool {
	info, err := os.Stat(filepath.Join(workingDir, name))
	return err == nil && !info.IsDir()
}

// pickExisting returns the subset of names that exist as files in workingDir.
func pickExisting(workingDir string, names ...string) []string {
	var out []string
	for _, n := range names {
		if hasFile(workingDir, n) {
			out = append(out, n)
		}
	}
	return out
}

// firstMatchingGlob returns the first filename matching any of the
// glob patterns at the workingDir root, or "" if none match.
func firstMatchingGlob(workingDir string, patterns ...string) string {
	for _, p := range patterns {
		matches, _ := filepath.Glob(filepath.Join(workingDir, p))
		if len(matches) > 0 {
			return filepath.Base(matches[0])
		}
	}
	return ""
}

// detectProjectVenvPython returns the container-side path to the
// project's venv python (e.g. "/workspace/venv/bin/python") if the
// working directory has a recognisable Python virtual environment.
// Returns "" when no venv is found.
//
// The agent's working_dir is the container-internal /workspace, so
// we resolve against that. Common venv directory names: venv, .venv,
// env, .env-py — we probe in priority order and stop at the first hit.
// Inside each, look for bin/python, bin/python3, or Scripts/python.exe
// (Windows-emitted venvs occasionally end up bind-mounted on Linux).
//
// Caller passes workingDir from ctx.WorkingDir; the returned path is
// what the model should literally invoke via run_command — e.g.
// "/workspace/venv/bin/python app.py" — and what gets surfaced in the
// system prompt's venv hint.
func detectProjectVenvPython(workingDir string) string {
	if workingDir == "" {
		return ""
	}
	venvDirs := []string{"venv", ".venv", "env", ".env-py"}
	pythonRels := []string{"bin/python", "bin/python3", "Scripts/python.exe"}
	for _, vd := range venvDirs {
		for _, py := range pythonRels {
			abs := filepath.Join(workingDir, vd, py)
			if info, err := os.Stat(abs); err == nil && !info.IsDir() {
				// Return container-relative path (workingDir is already
				// the container-side /workspace), so caller can paste
				// it into a run_command argument unchanged.
				return abs
			}
		}
	}
	return ""
}

func listWorkspaceFiles(workingDir string, max int) []string {
	if workingDir == "" {
		return nil
	}
	var out []string
	skip := map[string]bool{".git": true, "node_modules": true,
		"__pycache__": true, ".venv": true, "venv": true, "dist": true}
	_ = filepath.WalkDir(workingDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if len(out) >= max {
			return fs.SkipAll
		}
		if rel, err := filepath.Rel(workingDir, path); err == nil {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}
