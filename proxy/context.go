package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// GH #39 point 4: auto-inject reachability slice. When a user message names
// project symbols (a function, class, method) directly, we want the model
// to start its first turn already holding those definitions instead of
// burning agent turns on read_file/list_directory recon. Concrete failure
// mode this fixes: "fix the dashboard route" → model spends 4 turns finding
// app.py before it starts editing.
//
// Pipeline:
//   1. extractCandidateSymbols  — regex over the user message
//   2. walkPythonFiles          — list .py under working_dir (caps applied)
//   3. POST /internal/symbol_index → v3-service tree-sitter walks each file,
//      returns snippets for symbols defined in the project
//   4. formatProjectContextMessage — render as a system-role message that
//      prepends the user message in ctx.Messages
//
// All caps are enforced upstream (max 50 files, 500 KB total) so a giant
// monorepo can't OOM v3-service. Per-snippet trim happens server-side.

const (
	// projectScanMaxFiles caps the number of .py files we read from disk.
	// Keeps a 1000-file repo from sending 100 MB to v3-service. 50 is
	// enough for typical L6-tier projects (a flask app, a small library)
	// and forces graceful degradation on bigger ones — the symbol may
	// still get matched if it's in one of the first 50 files visited.
	projectScanMaxFiles = 50
	// projectScanMaxBytes is the total source-byte budget across all
	// scanned files. 500 KB ≈ ~12k LOC of Python — comfortable for a
	// small project, hard limit for runaways.
	projectScanMaxBytes = 500 * 1024
	// projectScanTimeout is the v3-service round-trip cap. Symbol index
	// runs once per session at startup, so a 5s budget is fine — beyond
	// that, fall back to no injection rather than blocking the loop.
	projectScanTimeout = 5 * time.Second
	// symbolMaxCandidates limits how many symbols we extract from the
	// user message. Above this, we keep the first N (regex order) and
	// drop the rest — defends against a paste-bomb message inflating
	// the index lookup.
	symbolMaxCandidates = 10
)

// symbolStopwords filters out common English words that happen to look
// like identifiers. Without this, a message like "fix the route" would
// extract "fix" and "route" as candidates and try to resolve them.
// Keep small — false negatives (real symbols filtered) are worse than
// false positives (junk symbols that just won't match anything in the
// index).
var symbolStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "it": true, "to": true,
	"of": true, "in": true, "on": true, "at": true, "by": true, "for": true,
	"and": true, "or": true, "not": true, "but": true, "if": true, "as": true,
	"this": true, "that": true, "these": true, "those": true,
	"fix": true, "add": true, "make": true, "code": true, "file": true,
	"function": true, "class": true, "method": true, "route": true,
	"app": true, "test": true, "tests": true, "run": true, "running": true,
	"please": true, "thanks": true, "okay": true, "ok": true,
	"i": true, "you": true, "we": true, "they": true,
	"my": true, "your": true, "our": true, "their": true,
	"can": true, "should": true, "would": true, "will": true, "do": true,
	"does": true, "did": true, "have": true, "has": true, "had": true,
	"true": true, "false": true, "none": true, "null": true,
	"http": true, "https": true, "url": true, "json": true, "html": true, "css": true,
	"py": true, "js": true, "ts": true, "go": true, "rs": true,
}

// reBacktickIdent matches `name` — a single identifier in backticks.
// Case-insensitive identifier rule. Backticks are an unambiguous
// "this is a code reference" signal in user prose.
var reBacktickIdent = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_]{1,63})`")

// reDottedPath matches a dotted identifier path (foo.bar.baz). Each
// segment becomes a candidate so we'll try resolving any of them.
var reDottedPath = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]{1,63}(?:\.[A-Za-z_][A-Za-z0-9_]{1,63})+)\b`)

// reTheNamedThing matches "the dashboard function", "the UserModel
// class", "the validate method" — a common natural-language reference
// to a symbol. We grab the noun in the middle.
var reTheNamedThing = regexp.MustCompile(`(?i)\bthe\s+([A-Za-z_][A-Za-z0-9_]{1,63})\s+(function|class|method|route|handler)\b`)

// extractCandidateSymbols pulls plausible symbol names from a user
// message. Conservative — false negatives are fine (the symbol_index
// will turn up nothing and we just skip injection); false positives
// just waste a tree-sitter walk on the server side.
func extractCandidateSymbols(message string) []string {
	if message == "" {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	add := func(name string) {
		if len(out) >= symbolMaxCandidates {
			return
		}
		lower := strings.ToLower(name)
		if symbolStopwords[lower] || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}

	// Backticked first — strongest signal.
	for _, m := range reBacktickIdent.FindAllStringSubmatch(message, -1) {
		add(m[1])
	}
	// "the X function/class/..." next.
	for _, m := range reTheNamedThing.FindAllStringSubmatch(message, -1) {
		add(m[1])
	}
	// Dotted paths — split into segments. Skip the dotted form itself
	// since v3-service indexes top-level definitions (no qualified
	// lookup yet); the leaf identifier is the most likely match.
	for _, m := range reDottedPath.FindAllStringSubmatch(message, -1) {
		segs := strings.Split(m[1], ".")
		for _, seg := range segs {
			add(seg)
		}
	}
	return out
}

// walkPythonFiles collects .py files under root, capped by file count
// and total bytes. Skips hidden directories (.git, .venv, __pycache__,
// node_modules) so a typical project doesn't blow the budget on
// vendored dependencies. Returns map[relativePath]source.
func walkPythonFiles(root string) map[string]string {
	result := map[string]string{}
	if root == "" {
		return result
	}
	totalBytes := 0
	skipDirs := map[string]bool{
		".git": true, ".venv": true, "venv": true, "env": true,
		"__pycache__": true, "node_modules": true, ".tox": true,
		"dist": true, "build": true, ".mypy_cache": true,
		".pytest_cache": true, ".idea": true, ".vscode": true,
	}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(info.Name()), ".py") {
			return nil
		}
		if len(result) >= projectScanMaxFiles || totalBytes >= projectScanMaxBytes {
			return filepath.SkipAll
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if totalBytes+len(data) > projectScanMaxBytes {
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		result[rel] = string(data)
		totalBytes += len(data)
		return nil
	})
	return result
}

// symbolGraphNode mirrors one entry of the v3-service symbol_index "graph"
// field (issue #39 Phase 3): a matched symbol's call-graph neighborhood.
// Omitted when the service found no neighborhood.
type symbolGraphNode struct {
	Symbol  string   `json:"symbol"`
	Callers []string `json:"callers"`
	Callees []string `json:"callees"`
}

// symbolIndexResult mirrors the v3-service /internal/symbol_index response.
type symbolIndexResult struct {
	Matched []struct {
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		File      string `json:"file"`
		Snippet   string `json:"snippet"`
		NLines    int    `json:"n_lines"`
		Truncated bool   `json:"truncated"`
	} `json:"matched"`
	Skipped []struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	} `json:"skipped"`
	Graph []symbolGraphNode `json:"graph"`
}

// resolveProjectSymbols POSTs to v3-service and returns the matched
// snippets. Fail-soft: any error returns (empty, false) so the caller
// just skips injection and the loop runs as it did before.
func resolveProjectSymbols(ctx *AgentContext, fileMap map[string]string, symbols []string) (symbolIndexResult, bool) {
	var zero symbolIndexResult
	if ctx == nil || ctx.V3URL == "" || len(fileMap) == 0 || len(symbols) == 0 {
		return zero, false
	}
	body, err := json.Marshal(map[string]interface{}{
		"file_map":              fileMap,
		"symbols":               symbols,
		"max_snippets":          3,
		"max_lines_per_snippet": 200,
	})
	if err != nil {
		return zero, false
	}
	reqCtx, cancel := context.WithTimeout(ctx.Ctx, projectScanTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST",
		ctx.V3URL+"/internal/symbol_index", bytes.NewReader(body))
	if err != nil {
		return zero, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, false
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, false
	}
	var r symbolIndexResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return zero, false
	}
	return r, true
}

// formatProjectContextMessage renders matched snippets as a system-role
// message body. Empty string → caller skips injection. The wording
// mirrors the existing system-prompt voice so the model treats it as
// authoritative project information rather than random user content.
func formatProjectContextMessage(matched []struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	File      string `json:"file"`
	Snippet   string `json:"snippet"`
	NLines    int    `json:"n_lines"`
	Truncated bool   `json:"truncated"`
}) string {
	if len(matched) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Project context (auto-resolved from your request)\n\n")
	sb.WriteString("These are the actual definitions of symbols you mentioned. You don't need to read_file these — the content below is current and authoritative:\n\n")
	for _, m := range matched {
		sb.WriteString("### `")
		sb.WriteString(m.Name)
		sb.WriteString("` — ")
		sb.WriteString(m.Kind)
		sb.WriteString(" in ")
		sb.WriteString(m.File)
		if m.Truncated {
			sb.WriteString(" (truncated)")
		}
		sb.WriteString("\n\n```python\n")
		sb.WriteString(m.Snippet)
		if !strings.HasSuffix(m.Snippet, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n\n")
	}
	return sb.String()
}

// formatGraphNeighborhood renders the call-graph neighborhood (callers /
// callees) of the matched symbols (issue #39 Phase 3). Empty when no graph
// data was returned (flag off, or no edges). Gives the model the structurally
// related code without a read_file loop.
func formatGraphNeighborhood(graph []symbolGraphNode) string {
	if len(graph) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Call-graph neighborhood (structurally related symbols)\n\n")
	for _, g := range graph {
		sb.WriteString("- `")
		sb.WriteString(g.Symbol)
		sb.WriteString("`")
		if len(g.Callers) > 0 {
			sb.WriteString(" — called by: " + strings.Join(g.Callers, ", "))
		}
		if len(g.Callees) > 0 {
			sb.WriteString("; calls: " + strings.Join(g.Callees, ", "))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

// ---------------------------------------------------------------------------
// Project detection — identifies language, framework, and build commands
// ---------------------------------------------------------------------------

// detectProjectInfo scans the project root for config files and identifies
// the language, framework, and build commands.
func detectProjectInfo(projectDir string) *ProjectInfo {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return nil
	}

	fileNames := make(map[string]bool)
	for _, e := range entries {
		if !e.IsDir() {
			fileNames[e.Name()] = true
		}
	}

	// Check each language/framework in priority order
	if info := detectNodeJS(projectDir, fileNames); info != nil {
		return info
	}
	if info := detectPython(projectDir, fileNames); info != nil {
		return info
	}
	if info := detectRust(fileNames); info != nil {
		return info
	}
	if info := detectGo(fileNames); info != nil {
		return info
	}
	if info := detectC(fileNames); info != nil {
		return info
	}
	if info := detectShell(projectDir, fileNames); info != nil {
		return info
	}

	return nil
}

// ---------------------------------------------------------------------------
// Language-specific detectors
// ---------------------------------------------------------------------------

func detectNodeJS(projectDir string, files map[string]bool) *ProjectInfo {
	if !files["package.json"] {
		return nil
	}

	info := &ProjectInfo{
		Language:    "nodejs",
		ConfigFiles: []string{"package.json"},
	}

	// Parse package.json for framework detection
	data, err := os.ReadFile(filepath.Join(projectDir, "package.json"))
	if err != nil {
		return info
	}

	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
		PackageManager  string            `json:"packageManager"`
		Scripts         map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return info
	}
	pm := detectNodePackageManager(files, pkg.PackageManager)

	if hasScript(pkg.Scripts, "build") {
		info.BuildCommand = nodeScriptCommand(pm, "build")
	}
	if hasScript(pkg.Scripts, "dev") {
		info.DevCommand = nodeScriptCommand(pm, "dev")
	}
	if hasScript(pkg.Scripts, "test") {
		info.TestCommand = nodeScriptCommand(pm, "test")
	}

	allDeps := make(map[string]string)
	for k, v := range pkg.Dependencies {
		allDeps[k] = v
	}
	for k, v := range pkg.DevDependencies {
		allDeps[k] = v
	}

	// Detect framework
	if _, ok := allDeps["next"]; ok {
		info.Framework = "nextjs"
		if info.BuildCommand == "" {
			info.BuildCommand = "npx next build"
		}
	} else if _, ok := allDeps["react"]; ok {
		info.Framework = "react"
	} else if _, ok := allDeps["vue"]; ok {
		info.Framework = "vue"
	} else if _, ok := allDeps["express"]; ok {
		info.Framework = "express"
	}

	// Add detected config files
	for _, f := range []string{"tsconfig.json", "tailwind.config.ts", "tailwind.config.js",
		"postcss.config.js", "next.config.js", "next.config.ts", "vite.config.ts"} {
		if files[f] {
			info.ConfigFiles = append(info.ConfigFiles, f)
		}
	}

	if files["tsconfig.json"] {
		info.Language = "typescript"
	}

	return info
}

func detectNodePackageManager(files map[string]bool, packageManager string) string {
	switch {
	case files["pnpm-lock.yaml"]:
		return "pnpm"
	case files["yarn.lock"]:
		return "yarn"
	case files["bun.lock"] || files["bun.lockb"]:
		return "bun"
	default:
		if packageManager != "" {
			pm := strings.ToLower(strings.TrimSpace(packageManager))
			if idx := strings.IndexAny(pm, "@+"); idx > 0 {
				pm = pm[:idx]
			}
			switch pm {
			case "pnpm", "yarn", "bun", "npm":
				return pm
			}
		}
		return "npm"
	}
}

func hasScript(scripts map[string]string, name string) bool {
	if scripts == nil {
		return false
	}
	_, ok := scripts[name]
	return ok
}

func nodeScriptCommand(pm, script string) string {
	switch pm {
	case "yarn":
		return "yarn " + script
	case "bun":
		return "bun run " + script
	case "pnpm":
		return "pnpm run " + script
	default:
		if script == "test" {
			return "npm test"
		}
		return "npm run " + script
	}
}

func detectPython(projectDir string, files map[string]bool) *ProjectInfo {
	hasPython := files["pyproject.toml"] || files["setup.py"] || files["requirements.txt"] || files["Pipfile"]
	if !hasPython {
		// Check for .py files
		entries, _ := filepath.Glob(filepath.Join(projectDir, "*.py"))
		if len(entries) == 0 {
			return nil
		}
	}

	info := &ProjectInfo{
		Language:    "python",
		ConfigFiles: []string{},
	}

	for _, f := range []string{"pyproject.toml", "setup.py", "requirements.txt", "Pipfile"} {
		if files[f] {
			info.ConfigFiles = append(info.ConfigFiles, f)
		}
	}

	// Detect framework from requirements or pyproject
	if data, err := os.ReadFile(filepath.Join(projectDir, "requirements.txt")); err == nil {
		content := strings.ToLower(string(data))
		if strings.Contains(content, "flask") {
			info.Framework = "flask"
			info.DevCommand = "flask run"
		} else if strings.Contains(content, "django") {
			info.Framework = "django"
			info.DevCommand = "python manage.py runserver"
		} else if strings.Contains(content, "fastapi") {
			info.Framework = "fastapi"
			info.DevCommand = "uvicorn main:app --reload"
		}
	}

	info.TestCommand = "python -m pytest"
	// No build command. "python -m py_compile *.py" was invented here and
	// forwarded to V3 as the project's build: it compiles only top-level
	// files (passing a candidate in src/ that it never compiled), fails on
	// every project with none, and V3 recorded it as a passed build. The
	// syntax check V3 already runs on the candidate itself covers the file.

	return info
}

func detectRust(files map[string]bool) *ProjectInfo {
	if !files["Cargo.toml"] {
		return nil
	}
	return &ProjectInfo{
		Language:     "rust",
		ConfigFiles:  []string{"Cargo.toml"},
		BuildCommand: "cargo build",
		DevCommand:   "cargo run",
		TestCommand:  "cargo test",
	}
}

func detectGo(files map[string]bool) *ProjectInfo {
	if !files["go.mod"] {
		return nil
	}
	return &ProjectInfo{
		Language:    "go",
		ConfigFiles: []string{"go.mod"},
		// ./... builds every package; "." failed with no Go files at the
		// root and skipped the subpackages.
		BuildCommand: "go build ./...",
		DevCommand:   "go run .",
		TestCommand:  "go test ./...",
	}
}

func detectC(files map[string]bool) *ProjectInfo {
	hasMakefile := files["Makefile"] || files["makefile"]
	hasCMake := files["CMakeLists.txt"]

	if !hasMakefile && !hasCMake {
		return nil
	}

	info := &ProjectInfo{
		Language:    "c",
		ConfigFiles: []string{},
	}

	if hasMakefile {
		info.ConfigFiles = append(info.ConfigFiles, "Makefile")
		info.BuildCommand = "make"
		info.TestCommand = "make test"
	}
	if hasCMake {
		info.ConfigFiles = append(info.ConfigFiles, "CMakeLists.txt")
		info.BuildCommand = "cmake --build ."
	}

	return info
}

func detectShell(projectDir string, files map[string]bool) *ProjectInfo {
	// Check for .sh files
	entries, _ := filepath.Glob(filepath.Join(projectDir, "*.sh"))
	if len(entries) == 0 {
		return nil
	}

	// No build or test command: "bash -n *.sh" checks only the first file
	// (the rest become its arguments), and a syntax check is not a test.
	return &ProjectInfo{
		Language:    "shell",
		ConfigFiles: []string{},
	}
}

// resolveWorkspacePath translates an approved host path and verifies that the
// result stays inside the configured workspace. It checks both the lexical path
// and the nearest existing ancestor so symlinks cannot escape the workspace.
func resolveWorkspacePath(ctx *AgentContext, path string) (string, error) {
	resolved := resolveAgentPath(ctx, path)
	root, err := filepath.Abs(ctx.WorkingDir)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	candidate, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace", path)
	}

	workspace, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("open workspace root: %w", err)
	}
	defer workspace.Close()
	existing := rel
	for {
		if _, statErr := workspace.Stat(existing); statErr == nil {
			break
		} else if !os.IsNotExist(statErr) {
			return "", fmt.Errorf("inspect path %q: %w", path, statErr)
		}
		parent := filepath.Dir(existing)
		if parent == existing || existing == "." {
			return "", fmt.Errorf("path %q has no existing workspace ancestor", path)
		}
		existing = parent
	}
	return candidate, nil
}

// readWorkspaceFile opens a file through os.Root, so the read remains confined
// even if a workspace symlink is swapped between validation and use.
func readWorkspaceFile(ctx *AgentContext, path string) ([]byte, string, error) {
	resolved, err := resolveWorkspacePath(ctx, path)
	if err != nil {
		return nil, "", err
	}
	rootPath, err := filepath.Abs(ctx.WorkingDir)
	if err != nil {
		return nil, "", fmt.Errorf("resolve workspace root: %w", err)
	}
	rel, err := filepath.Rel(rootPath, resolved)
	if err != nil {
		return nil, "", fmt.Errorf("resolve workspace-relative path: %w", err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, "", fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close()
	file, err := root.Open(rel)
	if err != nil {
		return nil, "", err
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, "", readErr
	}
	if closeErr != nil {
		return nil, "", closeErr
	}
	return data, resolved, nil
}

// readWorkspaceDir is the directory equivalent of readWorkspaceFile.
func readWorkspaceDir(ctx *AgentContext, path string) ([]os.DirEntry, error) {
	resolved, err := resolveWorkspacePath(ctx, path)
	if err != nil {
		return nil, err
	}
	rootPath, err := filepath.Abs(ctx.WorkingDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	rel, err := filepath.Rel(rootPath, resolved)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace-relative path: %w", err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close()
	dir, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.ReadDir(-1)
}

// validateToolWorkspacePaths applies workspace containment before any tool
// handler can touch the filesystem. It is used by both the agent loop and the
// shared dispatcher so parallel and direct dispatch paths follow one policy.
// workspacePathFields is the single registry of which arguments each tool
// treats as a workspace path. Package-scoped so a test can enumerate the real
// owner instead of maintaining a second copy that drifts.
var workspacePathFields = map[string][]string{
	"read_file":       {"path"},
	"outline_file":    {"path"},
	"write_file":      {"path"},
	"edit_file":       {"path"},
	"structural_edit": {"path"},
	// Both were absent, so the workspace-containment pre-check never ran on
	// them even though their doc comments claim "same gates as every other
	// write". Mitigated transitively by the read-first requirement, but the
	// explicit gate belongs here.
	"insert_after":   {"path"},
	"replace_lines":  {"path"},
	"delete_file":    {"path"},
	"move_file":      {"source", "destination"},
	"search_files":   {"path"},
	"find_file":      {"path"},
	"list_directory": {"path"},
	"run_command":    {"cwd"},
	"run_background": {"cwd"},
}

func validateToolWorkspacePaths(name string, args json.RawMessage, ctx *AgentContext) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return ""
	}
	keys := workspacePathFields
	for _, key := range keys[name] {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" {
			continue
		}
		if _, err := resolveWorkspacePath(ctx, value); err != nil {
			return fmt.Sprintf("%s: %v", name, err)
		}
	}
	return ""
}

// Session file manifest. The agent creates multi-file projects one
// write at a time, and each write is generated as if it were the first
// file in an empty project — SessionWrites knows what exists, but that
// knowledge never reached the model or V3's candidate generation.
// Observed 2026-07-18: the model wrote templates/index.html and
// static/game.js, then generated an app.py that inlined its own page
// with render_template_string, orphaning both of its earlier files.
//
// Two consumers:
//   - sessionManifestNote: a [system note] appended to the conversation
//     when the session's file set grows past one file, listing what
//     exists so later files reference earlier ones.
//   - mergeSessionWritesIntoContext (tools.go): puts session-written
//     files into V3GenerateRequest.ProjectContext, which previously
//     held only files the model had READ — written-but-never-read
//     files were invisible to candidate generation.

// sessionManifestNote returns a one-shot context note listing the files
// this session has created, or "" when there is nothing new to say.
// Fires only when the set grew AND holds at least two files — a single
// file has no cross-file relationships worth announcing. Announced
// state lives in ctx.ManifestAnnounced so each file is announced once.
func sessionManifestNote(ctx *AgentContext) string {
	if len(ctx.SessionWrites) < 2 {
		return ""
	}
	if ctx.ManifestAnnounced == nil {
		ctx.ManifestAnnounced = make(map[string]bool)
	}
	fresh := false
	paths := make([]string, 0, len(ctx.SessionWrites))
	for p := range ctx.SessionWrites {
		paths = append(paths, p)
		if !ctx.ManifestAnnounced[p] {
			ctx.ManifestAnnounced[p] = true
			fresh = true
		}
	}
	if !fresh {
		return ""
	}
	sort.Strings(paths)
	return fmt.Sprintf(
		"Project files created this session: %s. When these should work "+
			"together, reference them (e.g. render_template(\"page.html\"), "+
			"<script src=\"/static/app.js\">) instead of re-creating or "+
			"inlining their content. Files you have not re-read may have "+
			"been improved by build verification since you wrote them.",
		strings.Join(paths, ", "))
}
