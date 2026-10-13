package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestDetectNodeJSUsesDeclaredScripts(t *testing.T) {
	dir := t.TempDir()
	packageJSON := `{
		"scripts": {
			"build": "vite build",
			"dev": "vite --host 0.0.0.0",
			"test": "vitest run"
		},
		"dependencies": {
			"react": "latest"
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(packageJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	info := detectProjectInfo(dir)
	if info == nil {
		t.Fatal("detectProjectInfo returned nil")
	}
	if info.BuildCommand != "npm run build" {
		t.Fatalf("BuildCommand = %q, want npm run build", info.BuildCommand)
	}
	if info.DevCommand != "npm run dev" {
		t.Fatalf("DevCommand = %q, want npm run dev", info.DevCommand)
	}
	if info.TestCommand != "npm test" {
		t.Fatalf("TestCommand = %q, want npm test", info.TestCommand)
	}
}

func TestDetectNodeJSDoesNotInventMissingBuildScript(t *testing.T) {
	dir := t.TempDir()
	packageJSON := `{
		"scripts": {
			"start": "node server.js"
		},
		"dependencies": {
			"express": "latest"
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(packageJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	info := detectProjectInfo(dir)
	if info == nil {
		t.Fatal("detectProjectInfo returned nil")
	}
	if info.BuildCommand != "" {
		t.Fatalf("BuildCommand = %q, want empty when package has no build script", info.BuildCommand)
	}
	if info.TestCommand != "" {
		t.Fatalf("TestCommand = %q, want empty when package has no test script", info.TestCommand)
	}
}

func TestDetectNodeJSUsesLockfilePackageManagerForScripts(t *testing.T) {
	dir := t.TempDir()
	packageJSON := `{
		"scripts": {
			"build": "vite build",
			"test": "vitest run"
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(packageJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	info := detectProjectInfo(dir)
	if info == nil {
		t.Fatal("detectProjectInfo returned nil")
	}
	if info.BuildCommand != "pnpm run build" {
		t.Fatalf("BuildCommand = %q, want pnpm run build", info.BuildCommand)
	}
	if info.TestCommand != "pnpm run test" {
		t.Fatalf("TestCommand = %q, want pnpm run test", info.TestCommand)
	}
}

func TestDetectNodeJSUsesCurrentBunLockfile(t *testing.T) {
	dir := t.TempDir()
	packageJSON := `{
		"scripts": {
			"build": "vite build",
			"test": "bun test"
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(packageJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bun.lock"), []byte("# bun lockfile\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	info := detectProjectInfo(dir)
	if info == nil {
		t.Fatal("detectProjectInfo returned nil")
	}
	if info.BuildCommand != "bun run build" {
		t.Fatalf("BuildCommand = %q, want bun run build", info.BuildCommand)
	}
	if info.TestCommand != "bun run test" {
		t.Fatalf("TestCommand = %q, want bun run test", info.TestCommand)
	}
}

func TestDetectNodeJSUsesPackageManagerFieldWithoutLockfile(t *testing.T) {
	dir := t.TempDir()
	packageJSON := `{
		"packageManager": "yarn@4.10.3",
		"scripts": {
			"build": "vite build"
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(packageJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	info := detectProjectInfo(dir)
	if info == nil {
		t.Fatal("detectProjectInfo returned nil")
	}
	if info.BuildCommand != "yarn build" {
		t.Fatalf("BuildCommand = %q, want yarn build", info.BuildCommand)
	}
}

func TestDetectNextJSFallsBackToNextBuild(t *testing.T) {
	dir := t.TempDir()
	packageJSON := `{
		"dependencies": {
			"next": "latest",
			"react": "latest",
			"react-dom": "latest"
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(packageJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	info := detectProjectInfo(dir)
	if info == nil {
		t.Fatal("detectProjectInfo returned nil")
	}
	if info.Framework != "nextjs" {
		t.Fatalf("Framework = %q, want nextjs", info.Framework)
	}
	if info.BuildCommand != "npx next build" {
		t.Fatalf("BuildCommand = %q, want npx next build", info.BuildCommand)
	}
}

func TestExtractCandidateSymbols(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want []string
	}{
		{
			"backticked single ident",
			"please fix `dashboard`",
			[]string{"dashboard"},
		},
		{
			"the X function pattern",
			"the dashboard function is broken, fix it",
			[]string{"dashboard"},
		},
		{
			"the X class pattern",
			"make the UserModel class handle empty strings",
			[]string{"UserModel"},
		},
		{
			"dotted path expands to leaves",
			"add validation to UserModel.profile.email",
			[]string{"UserModel", "profile", "email"},
		},
		{
			"stopwords filtered",
			"fix the route the function the file",
			nil, // route, function, file all stopworded
		},
		{
			"mixed signals deduped",
			"fix `dashboard` — the dashboard function is broken",
			[]string{"dashboard"},
		},
		{
			"empty message",
			"",
			nil,
		},
		{
			"message with no symbols",
			"please clean up the formatting and add some comments",
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractCandidateSymbols(tc.msg)
			// Sort both for deterministic compare. Order doesn't matter
			// semantically — v3-service iterates regardless of order.
			gotSorted := append([]string{}, got...)
			wantSorted := append([]string{}, tc.want...)
			sort.Strings(gotSorted)
			sort.Strings(wantSorted)
			if len(gotSorted) == 0 && len(wantSorted) == 0 {
				return
			}
			if !reflect.DeepEqual(gotSorted, wantSorted) {
				t.Errorf("extractCandidateSymbols(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

func TestExtractCandidateSymbolsCap(t *testing.T) {
	// 12 backticked symbols — only the first symbolMaxCandidates (=10)
	// should be returned. Defends against a paste-bomb message that
	// would otherwise inflate the index lookup.
	msg := "look at `a1` `a2` `a3` `a4` `a5` `a6` `a7` `a8` `a9` `a10` `a11` `a12`"
	got := extractCandidateSymbols(msg)
	if len(got) != symbolMaxCandidates {
		t.Errorf("got %d symbols, want %d (cap)", len(got), symbolMaxCandidates)
	}
}

// #39 Phase 3: the proxy must parse and consume the v3-service "graph" field.
func TestSymbolIndexResultParsesGraph(t *testing.T) {
	raw := `{"matched":[{"name":"load","kind":"function","file":"svc.py","snippet":"def load(): ...","n_lines":1,"truncated":false}],` +
		`"skipped":[],` +
		`"graph":[{"symbol":"load","defined_in":["svc.py"],"callers":["main"],"callees":["clean"],"impact":["main"]}]}`
	var r symbolIndexResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(r.Graph) != 1 || r.Graph[0].Symbol != "load" {
		t.Fatalf("graph not parsed: %+v", r.Graph)
	}
	if r.Graph[0].Callers[0] != "main" || r.Graph[0].Callees[0] != "clean" {
		t.Errorf("graph neighborhood wrong: %+v", r.Graph[0])
	}
}

func TestFormatGraphNeighborhood(t *testing.T) {
	if formatGraphNeighborhood(nil) != "" {
		t.Error("empty graph should format to empty string")
	}
	out := formatGraphNeighborhood([]symbolGraphNode{
		{Symbol: "load", Callers: []string{"main"}, Callees: []string{"clean"}},
	})
	if !strings.Contains(out, "load") || !strings.Contains(out, "called by: main") ||
		!strings.Contains(out, "calls: clean") {
		t.Errorf("unexpected format: %q", out)
	}
}

func TestResolveWorkspacePathRejectsTraversalAndAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	ctx := &AgentContext{WorkingDir: root}
	for _, input := range []string{"../outside.txt", "/etc/passwd"} {
		if _, err := resolveWorkspacePath(ctx, input); err == nil {
			t.Errorf("resolveWorkspacePath(%q) succeeded, want rejection", input)
		}
	}
}

func TestResolveWorkspacePathAllowsHostPathTranslation(t *testing.T) {
	root := t.TempDir()
	ctx := &AgentContext{WorkingDir: root, HostWorkingDir: "/Users/test/project"}
	got, err := resolveWorkspacePath(ctx, "/Users/test/project/src/main.go")
	if err != nil {
		t.Fatalf("resolveWorkspacePath: %v", err)
	}
	want := filepath.Join(root, "src", "main.go")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveWorkspacePathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	ctx := &AgentContext{WorkingDir: root}
	if _, err := resolveWorkspacePath(ctx, "escape/file.txt"); err == nil {
		t.Fatal("symlink escape succeeded, want rejection")
	}
}

func TestReadWorkspaceFileRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	ctx := &AgentContext{WorkingDir: root}
	if _, _, err := readWorkspaceFile(ctx, "escape/secret.txt"); err == nil {
		t.Fatal("readWorkspaceFile followed a symlink outside the workspace")
	}
}

func TestReadWorkspaceFileReadsRegularWorkspaceFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, _, err := readWorkspaceFile(&AgentContext{WorkingDir: root}, "inside.txt")
	if err != nil {
		t.Fatalf("readWorkspaceFile: %v", err)
	}
	if string(got) != "inside" {
		t.Fatalf("readWorkspaceFile = %q, want inside", got)
	}
}

func TestExecuteToolCallRejectsWorkspaceEscape(t *testing.T) {
	root := t.TempDir()
	ctx := &AgentContext{WorkingDir: root}
	res := executeToolCall("read_file", json.RawMessage(`{"path":"../secret"}`), ctx)
	if res.Success || !strings.Contains(res.Error, "outside the workspace") {
		t.Fatalf("result = %+v, want workspace rejection", res)
	}
}

// encoding/json decodes "PATH" or "ſource" into the handler's field, so a
// value under such a key never passed the workspace check or the deny-list.
// Each is refused, and nothing outside the workspace is read or written.
func TestAnArgumentNameThatIsNotLowercaseIsRefused(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(root, "inside.txt"), []byte("inside\n"), 0o644)
	secret := filepath.Join(outside, "secret.txt")
	os.WriteFile(secret, []byte("OUTSIDE\n"), 0o644)
	escaped := filepath.Join(outside, "escaped.txt")
	calls := []struct{ tool, args string }{
		{"read_file", `{"PATH":` + jsonQuote(secret) + `}`},
		{"read_file", `{"path":"inside.txt","Path":` + jsonQuote(secret) + `}`},
		{"write_file", `{"Path":` + jsonQuote(escaped) + `,"content":"x\n"}`},
		{"move_file", `{"source":"inside.txt","ſource":` + jsonQuote(secret) + `,"destination":"moved.txt"}`},
		{"run_command", `{"Command":"rm -rf /"}`}, // the deny-list reads "command"
	}
	for _, c := range calls {
		ctx := NewAgentContext(root, Tier2Medium)
		ctx.PermissionMode = PermissionYolo
		res := executeToolCall(c.tool, json.RawMessage(c.args), ctx)
		if res.Success || !strings.Contains(res.Error, "argument names are lowercase") {
			t.Errorf("%s %s = %+v, want a refusal", c.tool, c.args, res)
		}
		if strings.Contains(string(res.Data), "OUTSIDE") {
			t.Errorf("%s %s returned the outside file", c.tool, c.args)
		}
	}
	if _, err := os.Stat(escaped); err == nil {
		t.Fatal("write_file wrote outside the workspace")
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatal("move_file moved a file from outside the workspace")
	}
}

// The ledger trims a path and the handler does not, so " /x" would be
// checked as a file inside the workspace and recorded as /x outside it.
func TestAPathPaddedWithWhitespaceIsRefused(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	for _, args := range []string{
		`{"path":` + jsonQuote(" "+filepath.Join(outside, "padded.txt")) + `,"content":"x\n"}`,
		`{"path":"inside.txt\n","content":"x\n"}`,
	} {
		ctx := NewAgentContext(root, Tier2Medium)
		ctx.PermissionMode = PermissionYolo
		res := executeToolCall("write_file", json.RawMessage(args), ctx)
		if res.Success || !strings.Contains(res.Error, "whitespace") {
			t.Errorf("write_file %s = %+v, want a refusal", args, res)
		}
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("a refused write left %v in the workspace", entries)
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A build command is the project's own, never an invented syntax sweep. V3
// runs a detected build command against a candidate and records the result
// as the project's build, so "python -m py_compile *.py" passed candidates in
// src/ it never compiled and failed every project with no top-level .py, and
// "bash -n *.sh" checked only the first script.
func TestDetectInventsNoSyntaxSweepAsABuild(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		wantBuild string
		wantTest  string
	}{
		{"python src layout", map[string]string{"pyproject.toml": "[project]\nname='x'\n", "src/pkg/mod.py": "x = 1\n"}, "", "python -m pytest"},
		{"python flat", map[string]string{"requirements.txt": "flask\n", "app.py": "x = 1\n"}, "", "python -m pytest"},
		{"shell scripts", map[string]string{"a.sh": "echo a\n", "b.sh": "echo b\n"}, "", ""},
		{"go subpackages", map[string]string{"go.mod": "module x\n\ngo 1.22\n", "pkg/a/a.go": "package a\n"}, "go build ./...", "go test ./..."},
	}
	for _, c := range cases {
		dir := t.TempDir()
		for name, body := range c.files {
			p := filepath.Join(dir, name)
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		info := detectProjectInfo(dir)
		if info == nil {
			t.Fatalf("%s: no project detected", c.name)
		}
		if info.BuildCommand != c.wantBuild || info.TestCommand != c.wantTest {
			t.Errorf("%s: build %q test %q, want %q / %q", c.name,
				info.BuildCommand, info.TestCommand, c.wantBuild, c.wantTest)
		}
	}
}
