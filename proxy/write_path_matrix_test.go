package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Write-path matrix, driven through the real agent loop: which rewrites of a
// file land and which are refused. An invalid rewrite must not replace valid
// bytes on disk. writeNewFileWithWarning admits invalid content only when
// there is nothing on disk to protect.

// Small: under the 10-line floor in classifyFileTier, so it stays T1
// regardless of extension.
const smallValidPy = "A = 1\nB = 2\nC = 3\n"
const smallInvalidPy = "A = 1\nB = 2\ndef broken(:\n"

// matrixStub serves a scripted model that writes `first` to path on its first
// turn and `rest` on every later turn. V3 is unavailable, and the syntax check
// rejects the broken bodies the tests use.
func matrixStub(t *testing.T, path, first, rest string, calls *int) *httptest.Server {
	t.Helper()
	write := func(content string) string {
		b, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": path, "content": content},
		})
		return string(b)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v3/") || strings.HasPrefix(r.URL.Path, "/internal/") {
			http.Error(w, "v3 unavailable in this test", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "def broken(:") &&
				!strings.Contains(in.Code, "def alpha(n:")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: invalid syntax"}
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		i := *calls
		*calls++
		reply := write(rest)
		if i == 0 {
			reply = write(first)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		delta, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": reply}}},
		})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
	}))
}

// The model creates a valid file, then keeps pushing an invalid rewrite of it.
// The valid bytes stay on disk. A small file never reaches the gated branch,
// which needs both a T2 file and V3, so it is protected on the direct path
// with or without V3. A T2 file with V3 reaches the syntax regression gate.
func TestAnInvalidRewriteKeepsTheValidBytes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		file      string
		valid     string
		invalid   string
		withV3    bool
		wantTier2 bool
	}{
		{"T1 without V3", "small.py", smallValidPy, smallInvalidPy, false, false},
		{"T1 with V3", "small.py", smallValidPy, smallInvalidPy, true, false},
		{"T2 with V3", "mod.py", banLoopValidBody, banLoopInvalidBody, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tier := classifyFileTier(tc.file, tc.valid); (tier >= Tier2Medium) != tc.wantTier2 {
				t.Fatalf("fixture drifted to tier %v", tier)
			}
			dir := t.TempDir()
			calls := 0
			srv := matrixStub(t, tc.file, tc.valid, tc.invalid, &calls)
			defer srv.Close()
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.InferenceURL, ctx.SandboxURL = srv.URL, srv.URL
			if tc.withV3 {
				ctx.V3URL = srv.URL
			}
			ctx.PermissionMode = PermissionYolo
			ctx.MaxTurns = 8
			ctx.StreamFn = func(string, interface{}) {}

			if err := runAgentLoop(ctx, "Create and then modify "+tc.file); err != nil {
				t.Fatalf("agent loop error: %v", err)
			}

			got, err := os.ReadFile(filepath.Join(dir, tc.file))
			if err != nil {
				t.Fatalf("the file is missing: %v", err)
			}
			if string(got) != tc.valid {
				t.Fatalf("an invalid rewrite destroyed valid bytes.\n on disk: %q\n expected: %q", got, tc.valid)
			}
		})
	}
}

// --- policies the overwrite gate must not break ------------------------------

// A NEW file that does not parse still lands, with a warning, so the model can
// run it and read the real traceback (writeNewFileWithWarning). There are no
// prior bytes to protect, which is what separates this from an overwrite.
func TestInvalidNewCodeFileStillLands(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	srv := matrixStub(t, "fresh.py", banLoopInvalidBody, banLoopInvalidBody, &calls)
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL, ctx.V3URL = srv.URL, srv.URL, srv.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 6
	ctx.StreamFn = func(string, interface{}) {}
	if err := runAgentLoop(ctx, "Create fresh.py"); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "fresh.py"))
	if err != nil {
		t.Fatalf("invalid NEW file did not land; the debugging policy regressed: %v", err)
	}
	if string(b) != banLoopInvalidBody {
		t.Fatalf("invalid new file landed with different bytes: %q", string(b))
	}
}

// With no syntax checker reachable the overwrite gate fails open: the rewrite
// lands, and the ledger never records "could not check" as "checked and
// passed". The file is one the session creates, so the rewrite reaches the
// write path.
func TestValidationUnavailableDoesNotBlockOrClaimPassed(t *testing.T) {
	dir := t.TempDir()
	// The workspace already holds another Python file, so the loop also looks
	// up the symbols the request names. With no V3 service that lookup is
	// skipped, and the run goes on with the request alone.
	if err := os.WriteFile(filepath.Join(dir, "helper.py"), []byte("def helper():\n    return 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := matrixStub(t, "mod.py", banLoopValidBody, banLoopInvalidBody, &calls)
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = "" // no validator reachable
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 6
	ctx.StreamFn = func(string, interface{}) {}
	if err := runAgentLoop(ctx, "Create and then modify mod.py"); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "mod.py"))
	if err != nil {
		t.Fatalf("the file is missing: %v", err)
	}
	if string(got) != banLoopInvalidBody {
		t.Fatalf("the rewrite was blocked although validation was unavailable; on disk: %q", got)
	}
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[ledgerKey(ctx, "mod.py")]
	ctx.LedgerMu.Unlock()
	if d == nil {
		t.Fatal("the rewrite is not in the ledger")
	}
	if d.ValidationStatus != ValidationNotRun {
		t.Fatalf("validation status %q for an unchecked rewrite, want %q", d.ValidationStatus, ValidationNotRun)
	}
}

// readThenWriteStub emits the real sequence a model must use against an
// existing file: read_file first, then write_file. ATLAS refuses a write over
// an unread existing file, so a fixture that pre-creates the target on disk
// never reaches the write path without this.
func readThenWriteStub(t *testing.T, path, replacement string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v3/") || strings.HasPrefix(r.URL.Path, "/internal/") {
			http.Error(w, "v3 unavailable in this test", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "def broken(:")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: invalid syntax"}
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		i := *calls
		*calls++
		var call map[string]interface{}
		if i == 0 {
			call = map[string]interface{}{"type": "tool_call", "name": "read_file",
				"args": map[string]string{"path": path}}
		} else {
			call = map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": path, "content": replacement}}
		}
		b, _ := json.Marshal(call)
		w.Header().Set("Content-Type", "text/event-stream")
		delta, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": string(b)}}},
		})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
	}))
}

// The syntax gate applies only to syntaxGateLanguages: a read and then a
// rewrite of a text file lands even when the new text would not parse as
// code, with or without V3.
func TestANonCodeOverwriteIsNotSyntaxGated(t *testing.T) {
	if _, gated := syntaxGateLanguages[".txt"]; gated {
		t.Fatal(".txt is syntax-gated; this test needs a file type that is not")
	}
	for _, tc := range []struct {
		name   string
		withV3 bool
	}{
		{"with V3", true},
		{"without V3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "notes.txt")
			if err := os.WriteFile(target, []byte("first\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			calls := 0
			srv := readThenWriteStub(t, "notes.txt", "def broken(:\n", &calls)
			defer srv.Close()
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.InferenceURL, ctx.SandboxURL = srv.URL, srv.URL
			if tc.withV3 {
				ctx.V3URL = srv.URL
			}
			ctx.PermissionMode = PermissionYolo
			ctx.MaxTurns = 8
			var sawRead, sawWriteAttempt, unreadGuard bool
			ctx.StreamFn = func(eventType string, data interface{}) {
				b, _ := json.Marshal(data)
				blob := string(b)
				if eventType == "tool_call" && strings.Contains(blob, `"read_file"`) {
					sawRead = true
				}
				if eventType == "tool_call" && strings.Contains(blob, `"write_file"`) {
					sawWriteAttempt = true
				}
				// The unread-file guard's refusal reaches the stream as a
				// failed tool_result.
				if strings.Contains(blob, "this session has not read it") {
					unreadGuard = true
				}
			}

			if err := runAgentLoop(ctx, "Read then rewrite notes.txt"); err != nil {
				t.Fatalf("agent loop error: %v", err)
			}

			if !sawRead {
				t.Fatal("fixture never issued read_file; the baseline was never read")
			}
			if !sawWriteAttempt {
				t.Fatal("fixture never issued write_file; it did not reach the write path")
			}
			if unreadGuard {
				t.Fatal("run stopped at the unread-existing-file guard instead of " +
					"reaching the write path under test")
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("target vanished: %v", err)
			}
			if string(got) != "def broken(:\n" {
				t.Fatalf("non-code overwrite was blocked; on disk: %q", got)
			}
		})
	}
}

// --- session-owned invalid-new -> repair workflow ----------------------------
//
// The invalid-new-file warning policy and the repair that follows it are one
// workflow: a file lands invalid so the model can run it and read the real
// traceback, then the same session rewrites it. Session ownership is what
// makes the second write_file permitted at all: write_file refuses a file the
// session did not create.

// sessionOwnedRepairStub serves a scripted model that creates owned.py with
// banLoopInvalidBody and then rewrites it with replacement on every later
// turn. v3Mode "working" makes /v3/generate answer with replacement as a
// passing winner; any other mode answers 503. v3Calls counts /v3/generate
// requests.
func sessionOwnedRepairStub(t *testing.T, replacement, v3Mode string, v3Calls *int) *httptest.Server {
	t.Helper()
	calls := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/generate"):
			*v3Calls++
			if v3Mode == "working" {
				resp, _ := json.Marshal(V3GenerateResponse{
					Code: replacement, Passed: true, PhaseSolved: "direct",
					CandidatesTested: 1, WinningScore: 1.0,
				})
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: result\ndata: %s\n\ndata: [DONE]\n\n", resp)
				return
			}
			http.Error(w, "v3 unavailable", http.StatusServiceUnavailable)
			return
		case strings.HasPrefix(r.URL.Path, "/internal/"):
			http.Error(w, "not stubbed", http.StatusServiceUnavailable)
			return
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "def alpha(n:") &&
				!strings.Contains(in.Code, "def alpha(n,:")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: invalid syntax"}
			}
			json.NewEncoder(w).Encode(out)
			return
		case !strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			http.NotFound(w, r)
			return
		}
		content := replacement
		if calls == 0 {
			content = banLoopInvalidBody
		}
		calls++
		b, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": "owned.py", "content": content}})
		w.Header().Set("Content-Type", "text/event-stream")
		delta, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(b)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
	}))
}

// A session-owned file that landed invalid can be rewritten, whether V3
// works, answers 503 or is not configured. A replacement that is still invalid
// lands too: refusing it would strand the original broken version.
func TestASessionOwnedInvalidFileCanBeRewritten(t *testing.T) {
	if tier := classifyFileTier("owned.py", banLoopInvalidBody); tier < Tier2Medium {
		t.Fatalf("fixture must be Tier2Medium on the first write, got %v", tier)
	}
	repaired := strings.Replace(banLoopInvalidBody, "def alpha(n:", "def alpha(n):", 1)
	stillInvalid := strings.Replace(banLoopInvalidBody, "def alpha(n:", "def alpha(n,:", 1)
	for _, tc := range []struct {
		name        string
		replacement string
		v3Mode      string
	}{
		{"valid, V3 working", repaired, "working"},
		{"valid, V3 down", repaired, "503"},
		{"valid, no V3", repaired, "absent"},
		{"still invalid, V3 working", stillInvalid, "working"},
		{"still invalid, V3 down", stillInvalid, "503"},
		{"still invalid, no V3", stillInvalid, "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			v3Calls := 0
			srv := sessionOwnedRepairStub(t, tc.replacement, tc.v3Mode, &v3Calls)
			defer srv.Close()
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.InferenceURL, ctx.SandboxURL = srv.URL, srv.URL
			if tc.v3Mode != "absent" {
				ctx.V3URL = srv.URL
			}
			ctx.PermissionMode = PermissionYolo
			ctx.MaxTurns = 6
			ctx.StreamFn = func(string, interface{}) {}

			if err := runAgentLoop(ctx, "Create owned.py then fix it"); err != nil {
				t.Fatalf("agent loop error: %v", err)
			}

			got, err := os.ReadFile(filepath.Join(dir, "owned.py"))
			if err != nil {
				t.Fatalf("session-owned file vanished: %v", err)
			}
			if string(got) != tc.replacement {
				t.Fatalf("the rewrite of a session-owned invalid file was refused (v3_calls=%d); on disk: %q",
					v3Calls, got)
			}
		})
	}
}
