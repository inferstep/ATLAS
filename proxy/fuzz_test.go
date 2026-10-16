package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func FuzzValidateShellCommand(f *testing.F) {
	// These expectations come from the existing command-policy unit tests.
	seeds := []struct {
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
	for _, seed := range seeds {
		f.Add(seed.command)
		if blocked := validateShellCommand(seed.command) != ""; blocked != seed.blocked {
			f.Errorf("seed %q: blocked = %t, want %t", seed.command, blocked, seed.blocked)
		}
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
		extracted := extractFencedContent(reply)
		if framing == fenceFramingComplete {
			if body == "" || extracted != body {
				t.Fatalf("reply %q: complete body %q, extracted %q", reply, body, extracted)
			}
		} else if body != "" || extracted != "" {
			t.Fatalf("reply %q: framing %v returned body %q or extracted %q", reply, framing, body, extracted)
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
