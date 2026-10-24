package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// shellCommandSeeds are the seeds of FuzzValidateShellCommand. The expected
// verdicts come from the existing command-policy unit tests.
var shellCommandSeeds = []struct {
	command string
	blocked bool
}{
	{"rm -rf /", true},
	{"rm -rf /workspace", true},
	{"rm -rf .", true},
	{"find . -name '*.tmp' -delete", true},
	{"mkfs.ext4 /dev/sda1", true},
	{"dd if=/dev/zero of=/dev/sda bs=1M", true},
	{`bash -c "rm -rf /"`, true},
	{"env rm -rf /", true},
	{"nohup rm -rf /", true},
	{"(rm -rf /)", true},
	{"echo $(rm -rf /)", true},
	{"mv index.html templates/", false},
	{"rm -f stale.pyc", false},
	{"rm -rf __pycache__", false},
	{"mkdir -p static/js", false},
	{"go test ./...", false},
	{"env ATLAS_FLAG=x bash -c 'go test ./...'", false},
	{`env -S 'bash -c "go test ./..."'`, false},
	{`bash -c "python app.py"`, false},
	{"env FLASK_APP=app.py flask run", false},
	{"nohup python app.py > app.log 2>&1 &", false},
}

func TestTheShellCommandSeedsHaveTheirKnownVerdicts(t *testing.T) {
	for _, seed := range shellCommandSeeds {
		if blocked := validateShellCommand(seed.command) != ""; blocked != seed.blocked {
			t.Errorf("seed %q: blocked = %t, want %t", seed.command, blocked, seed.blocked)
		}
	}
}

func FuzzValidateShellCommand(f *testing.F) {
	for _, seed := range shellCommandSeeds {
		f.Add(seed.command)
	}

	f.Fuzz(func(t *testing.T, command string) {
		// Arbitrary bytes, including incomplete shell input, must not panic.
		first := validateShellCommand(command)
		if again := validateShellCommand(command); again != first {
			t.Fatalf("command %q: result changed from %q to %q", command, first, again)
		}
		if len(command) > 128 {
			return
		}
		for _, tc := range fuzzShellCommandCases(command) {
			for _, wrapped := range fuzzShellWrappers(tc.command, command) {
				if got := validateShellCommand(wrapped); (got != "") != tc.blocked {
					t.Fatalf("command %q: blocked = %t, want %t", wrapped, got != "", tc.blocked)
				}
			}
		}
	})
}

type fuzzShellCommandCase struct {
	command string
	blocked bool
}

func fuzzShellCommandCases(input string) []fuzzShellCommandCase {
	pick := func(index int) byte {
		if input == "" {
			return 0
		}
		return input[index%len(input)]
	}
	spaces := []string{" ", "\t", "  "}
	sep1 := spaces[int(pick(0))%len(spaces)]
	sep2 := spaces[int(pick(1))%len(spaces)]
	targets := []string{"/", ".", "..", "/workspace"}
	target := targets[int(pick(2))%len(targets)]
	var flags strings.Builder
	flags.WriteString("-rf")
	for i := range len(input) {
		if input[i]&1 == 0 {
			flags.WriteByte('r')
		} else {
			flags.WriteByte('f')
		}
	}
	// Fuzz bytes vary a real rm command, rather than becoming quoted prose.
	return []fuzzShellCommandCase{
		{"rm" + sep1 + flags.String() + sep2 + target, true},
		{"rm" + sep1 + "-rf" + sep2 + "atlas-" + hex.EncodeToString([]byte(input)), false},
	}
}

func fuzzShellQuote(command string) string {
	return "'" + strings.ReplaceAll(command, "'", "'\\''") + "'"
}

func fuzzShellWrappers(command, input string) []string {
	// Hex keeps fuzz bytes in one safe environment value. No input is executed.
	assignment := "ATLAS_FLAG=" + hex.EncodeToString([]byte(input))
	quoted := fuzzShellQuote(command)
	shell := "bash -c " + quoted
	return []string{
		command,
		"env " + assignment + " " + command,
		"nohup env " + assignment + " " + command,
		"command -- " + command,
		"nice -n 5 " + command,
		"timeout 5s " + command,
		"stdbuf -oL " + command,
		shell,
		"sh -c " + quoted,
		"bash -lc " + quoted,
		"eval " + quoted,
		"( " + command + " )",
		"echo $(" + command + ")",
		"env " + assignment + " " + shell,
		"nohup env " + assignment + " " + shell,
		"sudo -u root " + shell,
		"env -u HOME " + shell,
		"env -S " + fuzzShellQuote(shell),
		"env --split-string=" + fuzzShellQuote(shell),
		"bash -c " + fuzzShellQuote(shell),
	}
}

func FuzzExtractModelResponse(f *testing.F) {
	for _, raw := range []string{
		`{"type":"tool_call","name":"read_file","args":{"path":"app.py"}}`,
		`{"type":"tool_call","name":"write_file","args":{"path":"a.py","content":"x = 1\n"}}`,
		`{"type":"tool_call","name":"read_file","arguments":{"path":"app.py"}}`,
		`{"type":"tool_call","name":"read_file","parameters":{"path":"app.py"}}`,
		`{"type":"read_file","path":"app.py"}`,
		`{"type":"tool_call","name":"read_file","path":"app.py"}`,
		`Before {"type":"tool_call","name":"read_file","args":{"path":"app.py"}} after`,
		`{"type":"tool_call","name":"write_file","args":{"path":"cut.py","content":"def f():\n    return [`,
		`{"type":"tool_call","name":"write_file","args":{"path":"bad.py","content":"P = re.compile(\"\d+\")\n"}}`,
		"{\"type\":\"tool_call\",\"name\":\"read_file\",\"args\":{\"path\":\"a\nb\"}}",
		"",
		"   \n\t ",
		string([]byte{0xff, 0x00, '{', '}'}),
	} {
		f.Add(raw)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		first, firstErr := extractModelResponse(raw)
		again, againErr := extractModelResponse(raw)
		if !reflect.DeepEqual(first, again) || (firstErr == nil) != (againErr == nil) ||
			(firstErr != nil && firstErr.Error() != againErr.Error()) {
			t.Fatalf("response %q: parse changed from (%+v, %v) to (%+v, %v)", raw, first, firstErr, again, againErr)
		}

		if len(raw) > 4096 {
			return
		}
		args, err := json.Marshal(map[string]string{"path": "file.txt", "content": raw})
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := json.Marshal(ModelResponse{Type: "tool_call", Name: "write_file", Args: args})
		if err != nil {
			t.Fatal(err)
		}
		var want ModelResponse
		if err := json.Unmarshal(envelope, &want); err != nil {
			t.Fatal(err)
		}
		got, err := extractModelResponse(string(envelope))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("canonical response for %q: got %+v, err %v; want %+v", raw, got, err, want)
		}
		// Truncated tool-call JSON must be rejected rather than reconstructed
		// into a call whose missing arguments would have to be guessed.
		if got, err := extractModelResponse(string(envelope[:len(envelope)-1])); err == nil {
			t.Fatalf("truncated canonical response for %q parsed as %+v", raw, got)
		}
	})
}

func FuzzParseFencedReply(f *testing.F) {
	for _, reply := range []string{
		"```lang\nBODY\n```",
		"```\nBODY\n```",
		"```lang\nBODY",
		"BODY\n```",
		"BODY\n",
		"",
		"```lang\n\n```",
		"```markdown\n# doc\n```lang\ninner\n```\nmore\n```",
		"````markdown\n# doc\n```lang\ninner\n```\nmore\n````",
		"Before\n```lang\nBODY\n```\nAfter",
		"```lang\r\nBODY\r\n```\r\n",
		string([]byte{0xff, 0x00, '`', '`', '`'}),
	} {
		f.Add(reply)
	}

	f.Fuzz(func(t *testing.T, reply string) {
		framing, body := parseFencedReply(reply)
		if againFraming, againBody := parseFencedReply(reply); againFraming != framing || againBody != body {
			t.Fatalf("reply %q: parse changed from (%v, %q) to (%v, %q)", reply, framing, body, againFraming, againBody)
		}
		// A complete fence has a body, and nothing else has one.
		if (body != "") != (framing == fenceFramingComplete) {
			t.Fatalf("reply %q: framing %v returned body %q", reply, framing, body)
		}
		// So the extracted content is that body, or empty with no complete fence.
		if extracted := extractFencedContent(reply); extracted != body {
			t.Fatalf("reply %q: framing %v, body %q, extracted %q", reply, framing, body, extracted)
		}
		if len(reply) <= 4096 {
			// Prefix each body line so arbitrary bytes cannot form a closer.
			wantBody := "x" + strings.ReplaceAll(reply, "\n", "\nx") + "\n"
			wrapped := "````lang\n" + wantBody + "````"
			gotFraming, gotBody := parseFencedReply(wrapped)
			if gotFraming != fenceFramingComplete || gotBody != wantBody {
				t.Fatalf("wrapped reply %q: framing %v, body %q; want complete, %q", reply, gotFraming, gotBody, wantBody)
			}
		}
	})
}

func FuzzResolveWorkspacePath(f *testing.F) {
	base := f.TempDir()
	root := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(root, "dir", "nested"), 0o755); err != nil {
		f.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		f.Fatal(err)
	}
	if err := os.Symlink("dir", filepath.Join(root, "alias")); err != nil {
		f.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(root, "escape")); err != nil {
		f.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("file\n"), 0o644); err != nil {
		f.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "file.txt"), []byte("nested file\n"), 0o644); err != nil {
		f.Fatal(err)
	}
	for _, path := range []string{
		".", "file.txt", "dir/file.txt", "dir/nested/new.txt", "alias/file.txt", "alias/new.txt",
		"escape", "escape/new.txt",
		"../outside.txt", "/etc/passwd", "dir/../../outside.txt", "..\\outside.txt", "",
	} {
		f.Add(path)
	}
	if _, err := resolveWorkspacePath(&AgentContext{WorkingDir: root}, "escape/new.txt"); err == nil {
		f.Fatal("symlink escape seed was accepted")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		f.Fatal(err)
	}
	ctx := &AgentContext{WorkingDir: root}
	f.Fuzz(func(t *testing.T, path string) {
		resolved, err := resolveWorkspacePath(ctx, path)
		if err != nil {
			return
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("path %q escaped workspace %q: resolved %q, rel %q, err %v", path, root, resolved, rel, err)
		}
		// The target need not exist or even be an openable filename. Check
		// every existing prefix from the root, so an unusable suffix cannot
		// hide an earlier symlink that leaves the workspace.
		prefix := root
		for _, component := range strings.Split(rel, string(filepath.Separator)) {
			if component == "." {
				continue
			}
			prefix = filepath.Join(prefix, component)
			if _, err := os.Lstat(prefix); err != nil {
				break
			}
			realPrefix, err := filepath.EvalSymlinks(prefix)
			if err != nil {
				t.Fatalf("path %q: resolve existing prefix %q: %v", path, prefix, err)
			}
			realRel, err := filepath.Rel(realRoot, realPrefix)
			if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
				t.Fatalf("path %q escaped through symlink: prefix %q resolved to %q, rel %q, err %v", path, prefix, realPrefix, realRel, err)
			}
		}
	})
}

// FuzzToolCallPathsStayInTheWorkspace: when the workspace check accepts a tool
// call, every path that the tool's handler decodes from the same arguments
// resolves inside the workspace, also through links. The root is a temporary
// folder, so the strip of a leading "workspace/", which runs only for a root of
// exactly /workspace, is not part of this target.
func FuzzToolCallPathsStayInTheWorkspace(f *testing.F) {
	base := f.TempDir()
	root := filepath.Join(base, "workspace")
	for _, dir := range []string{filepath.Join(root, "dir", "nested"), filepath.Join(base, "outside")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			f.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("file\n"), 0o644); err != nil {
		f.Fatal(err)
	}
	if err := os.Symlink("dir", filepath.Join(root, "alias")); err != nil {
		f.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(root, "escape")); err != nil {
		f.Fatal(err)
	}
	tools := make([]string, 0, len(workspacePathFields))
	for name := range workspacePathFields {
		if _, known := handlerPaths(name, []byte(`{}`)); !known {
			f.Fatalf("handlerPaths does not know %s, which the workspace check lists", name)
		}
		tools = append(tools, name)
	}
	sort.Strings(tools)
	// Arguments that the tests of the workspace check and of path resolution use.
	for i := range tools {
		for _, args := range []string{
			`{"path":"file.txt"}`, `{"path":"dir/nested/new.txt"}`, `{"path":"alias/file.txt"}`,
			`{"path":"escape/new.txt"}`, `{"path":"../outside.txt"}`, `{"path":"/etc/passwd"}`,
			`{"path":"/Users/test/project/file.txt"}`, `{"path":"inside.txt","Path":"x"}`,
			`{"source":"inside.txt","destination":"moved.txt"}`, `{"cwd":"/workspace"}`,
		} {
			f.Add(uint8(i), []byte(args))
		}
	}
	f.Fuzz(func(t *testing.T, toolIndex uint8, args []byte) {
		tool := tools[int(toolIndex)%len(tools)]
		ctx := &AgentContext{WorkingDir: root, HostWorkingDir: "/Users/test/project"}
		if validateToolWorkspacePaths(tool, args, ctx) != "" {
			return
		}
		paths, ok := handlerPaths(tool, args)
		if !ok {
			return // the handler's own decoding fails, so it does not run
		}
		for _, p := range paths {
			if strings.TrimSpace(p) == "" {
				continue // each handler refuses or ignores a blank path
			}
			if resolved := resolveAgentPath(ctx, p); !insideWorkspace(root, resolved) {
				t.Fatalf("%s: the check accepted %s, and the handler resolves %q to %q, outside %s",
					tool, args, p, resolved, root)
			}
		}
	})
}

// handlerPaths decodes args into the tool handler's own input type and returns
// the paths that the handler resolves. ok is false when that decoding fails, or
// when the tool is not one of these.
func handlerPaths(tool string, args []byte) (paths []string, ok bool) {
	decode := func(v any) bool { return json.Unmarshal(args, v) == nil }
	switch tool {
	case "read_file":
		var in ReadFileInput
		ok = decode(&in)
		return []string{in.Path}, ok
	case "outline_file":
		var in OutlineInput
		ok = decode(&in)
		return []string{in.Path}, ok
	case "write_file":
		var in WriteFileInput
		ok = decode(&in)
		return []string{in.Path}, ok
	case "edit_file":
		var in EditFileInput
		ok = decode(&in)
		return []string{in.Path}, ok
	case "structural_edit":
		var in StructuralEditInput
		ok = decode(&in)
		return []string{in.Path}, ok
	case "insert_after":
		var in InsertAfterInput
		ok = decode(&in)
		return []string{in.Path}, ok
	case "replace_lines":
		var in ReplaceLinesInput
		ok = decode(&in)
		return []string{in.Path}, ok
	case "delete_file":
		var in DeleteFileInput
		ok = decode(&in)
		return []string{in.Path}, ok
	case "move_file":
		var in MoveFileInput
		ok = decode(&in)
		return []string{in.Source, in.Destination}, ok
	case "search_files":
		var in SearchFilesInput
		ok = decode(&in)
		return []string{in.Path}, ok
	case "find_file":
		var in FindFileInput
		ok = decode(&in)
		return []string{in.Path}, ok
	case "list_directory":
		var in ListDirectoryInput
		ok = decode(&in)
		return []string{in.Path}, ok
	case "run_command":
		var in RunCommandInput
		ok = decode(&in)
		return []string{in.Cwd}, ok
	case "run_background":
		var in RunBackgroundInput
		ok = decode(&in)
		return []string{in.Cwd}, ok
	}
	return nil, false
}

// insideWorkspace reports whether resolved stays in root: by its name, and
// through every existing prefix with its links followed.
func insideWorkspace(root, resolved string) bool {
	outside := func(rel string) bool { return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) }
	rel, err := filepath.Rel(root, resolved)
	if err != nil || outside(rel) {
		return false
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	prefix := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		prefix = filepath.Join(prefix, part)
		if _, err := os.Lstat(prefix); err != nil {
			return true // nothing exists below here, so no link can lead out
		}
		realPrefix, err := filepath.EvalSymlinks(prefix)
		if err != nil {
			return false
		}
		if realRel, err := filepath.Rel(realRoot, realPrefix); err != nil || outside(realRel) {
			return false
		}
	}
	return true
}

// FuzzPermissionAnswerSettlesOnlyWhenDelivered: an answer to /v1/permission
// settles the waiting call only when it is delivered (200), and allows only for
// the exact word "allow". Every other answer leaves the call waiting.
func FuzzPermissionAnswerSettlesOnlyWhenDelivered(f *testing.F) {
	// Bodies of the tests of /v1/permission, and a word that other tests use.
	for _, body := range []string{
		`{"session_id":"sess-allow","tool_call_id":"call_1","decision":"allow","scope":"once"}`,
		`{"session_id":"sess-allow","tool_call_id":"call_1","decision":"deny"}`,
		`{"session_id":"sess-allow","tool_call_id":"call_1","decision":"yes"}`,
		`{"session_id":"nope","tool_call_id":"call_9","decision":"allow"}`,
		`{}`, `not json`,
	} {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		key := permKey("sess-allow", "call_1")
		entry := &pendingPermission{decision: make(chan permDecision, 1)}
		pendingPermissions.Store(key, entry)
		defer pendingPermissions.Delete(key)

		rec := httptest.NewRecorder()
		handlePermission(rec, httptest.NewRequest(http.MethodPost, "/v1/permission", bytes.NewReader(body)))
		_, stillWaiting := pendingPermissions.Load(key)
		select {
		case got := <-entry.decision:
			if rec.Code != http.StatusOK || stillWaiting {
				t.Fatalf("a decision was sent with answer %d (call still waiting: %v): %s", rec.Code, stillWaiting, body)
			}
			var req struct {
				Decision string `json:"decision"`
			}
			_ = json.NewDecoder(bytes.NewReader(body)).Decode(&req)
			if got.allow != (req.Decision == "allow") {
				t.Fatalf("allow is %v for the decision %q: %s", got.allow, req.Decision, body)
			}
		default:
			if rec.Code == http.StatusOK || !stillWaiting {
				t.Fatalf("answer %d sent no decision (call still waiting: %v): %s", rec.Code, stillWaiting, body)
			}
		}
	})
}

// FuzzAgentRequestRefusalLeavesNothing: a /v1/agent request that the request's
// rules refuse gets 400 and leaves the workspace as it was.
func FuzzAgentRequestRefusalLeavesNothing(f *testing.F) {
	workspace := f.TempDir()
	f.Setenv("ATLAS_WORKSPACE_DIR", workspace)
	f.Setenv("ATLAS_INFERENCE_URL", "http://127.0.0.1:1") // a turn started by mistake fails at once
	// Bodies and names of the tests of /v1/agent and of sandbox_subdir.
	for _, body := range []string{
		`{}`, `not json`, `{"message":"hi","sandbox_subdir":"a/b"}`, `{"message":"hi","sandbox_subdir":".."}`,
		`{"message":"hi","sandbox_subdir":"pane/"}`, `{"message":"hi","sandbox_subdir":"sub1"}`,
	} {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if !agentRequestIsRefused(body) {
			return // an accepted request starts a turn, which this target does not run
		}
		before, _ := os.ReadDir(workspace)
		rec := httptest.NewRecorder()
		handleAgent(rec, httptest.NewRequest(http.MethodPost, "/v1/agent", bytes.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("a request that the rules refuse got %d, not 400: %s", rec.Code, body)
		}
		if after, _ := os.ReadDir(workspace); len(after) != len(before) {
			t.Fatalf("a refused request changed the workspace (%d entries, now %d): %s", len(before), len(after), body)
		}
	})
}

// agentRequestIsRefused says whether handleAgent must answer 400 before a turn
// starts: the body does not decode, the message is empty, or sandbox_subdir is
// not a bare directory name. handleAgent decodes these two fields as the same
// plain strings, so a body that fails here fails there too.
func agentRequestIsRefused(body []byte) bool {
	var req struct {
		Message       string `json:"message"`
		SandboxSubdir string `json:"sandbox_subdir"`
	}
	if json.NewDecoder(bytes.NewReader(body)).Decode(&req) != nil {
		return true
	}
	return req.Message == "" || (req.SandboxSubdir != "" && !validSandboxSubdir(req.SandboxSubdir))
}

// FuzzPassthroughCarriesTheBound: a /v1/chat/completions body that is a valid
// JSON object leaves the proxy with a max_tokens in (0, ceiling], read by a
// strict reader that keeps numbers as written.
func FuzzPassthroughCarriesTheBound(f *testing.F) {
	f.Setenv("ATLAS_MAX_COMPLETION_TOKENS", "8192")
	// Bodies of the tests of the clamp.
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"hi"}]}`, `{"max_tokens":512}`, `{"max_tokens":-1}`,
		`{"max_tokens":900000}`, `{}`, `not json`, `{"seed":1` + strings.Repeat("0", 400) + `}`,
	} {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if !json.Valid(body) || !bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
			return // the bound is promised for a JSON object only
		}
		out, ok := clampGenerationBody("/v1/chat/completions", body)
		if !ok {
			t.Fatalf("a valid JSON object was refused: %s", body)
		}
		decoder := json.NewDecoder(bytes.NewReader(out))
		decoder.UseNumber()
		var fields map[string]any
		if err := decoder.Decode(&fields); err != nil {
			t.Fatalf("a valid JSON object left the proxy unreadable: %s -> %s (%v)", body, out, err)
		}
		n, ok := fields["max_tokens"].(json.Number)
		if !ok {
			t.Fatalf("no numeric max_tokens after the clamp: %s -> %s", body, out)
		}
		if v, err := n.Float64(); err != nil || v <= 0 || v > 8192 {
			t.Fatalf("max_tokens %s is not in (0, 8192] after the clamp: %s -> %s", n, body, out)
		}
	})
}
