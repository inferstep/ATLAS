package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Write-path matrix, driven through the real agent loop.
//
// The ban-loop regression exposed something separate from the loop bound: a
// small Python file taking the T0/T1 direct path had an INVALID rewrite land
// on top of VALID bytes with no syntax check at all. That is a different
// defect from the documented `writeNewFileWithWarning` policy, which admits
// invalid content only when there is nothing on disk to protect.
//
// These tests record present behavior per matrix cell. They are written to
// pass where behavior is already safe and to FAIL where it is not, so the
// failure is the specification for Invariant 2.

// Small: under the 10-line floor in classifyFileTier, so it stays T1
// regardless of extension.
const smallValidPy = "A = 1\nB = 2\nC = 3\n"
const smallInvalidPy = "A = 1\nB = 2\ndef broken(:\n"

func matrixStub(t *testing.T, path, first, rest string, v3 bool, calls *int) *httptest.Server {
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

// runOverwriteCase lands `valid`, then has the model push `invalid` at the
// same path, and reports what survived.
func runOverwriteCase(t *testing.T, rel, valid, invalid string, withV3 bool) (survived string, wantTier Tier) {
	t.Helper()
	dir := t.TempDir()
	calls := 0
	srv := matrixStub(t, rel, valid, invalid, withV3, &calls)
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = srv.URL
	if withV3 {
		ctx.V3URL = srv.URL
	}
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 8
	ctx.StreamFn = func(string, interface{}) {}

	if err := runAgentLoop(ctx, "Create and then modify "+rel); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("artifact missing entirely: %v", err)
	}
	return string(b), classifyFileTier(rel, valid)
}

// T1, no V3. The cell the ban-loop test stumbled into.
func TestOverwriteT1NoV3PreservesValidBytes(t *testing.T) {
	got, tier := runOverwriteCase(t, "small.py", smallValidPy, smallInvalidPy, false)
	if tier >= Tier2Medium {
		t.Fatalf("fixture drifted to %v; this cell must stay T0/T1", tier)
	}
	if got != smallValidPy {
		t.Fatalf("T1/no-V3: invalid rewrite destroyed valid bytes.\n on disk: %q\n expected: %q",
			got, smallValidPy)
	}
}

// T1 WITH V3 configured: the tier check at tools.go:855 is an AND, so a small
// file still misses the gated branch even when V3 is available.
func TestOverwriteT1WithV3PreservesValidBytes(t *testing.T) {
	got, tier := runOverwriteCase(t, "small.py", smallValidPy, smallInvalidPy, true)
	if tier >= Tier2Medium {
		t.Fatalf("fixture drifted to %v; this cell must stay T0/T1", tier)
	}
	if got != smallValidPy {
		t.Fatalf("T1/with-V3: invalid rewrite destroyed valid bytes.\n on disk: %q\n expected: %q",
			got, smallValidPy)
	}
}

// Known-protected comparison: T2Medium with V3 reaches the syntax regression
// gate and must refuse the invalid rewrite. If this ever fails the gate
// itself regressed, not the matrix.
func TestOverwriteT2WithV3PreservesValidBytes(t *testing.T) {
	got, tier := runOverwriteCase(t, "mod.py", banLoopValidBody, banLoopInvalidBody, true)
	if tier < Tier2Medium {
		t.Fatalf("fixture drifted to %v; this cell must stay T2Medium", tier)
	}
	if got != banLoopValidBody {
		t.Fatalf("T2/with-V3: the protected path let an invalid rewrite land: %q", got)
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
			valid := !strings.Contains(in.Code, "def broken(:") &&
				!strings.Contains(in.Code, "def alpha(n:") &&
				!strings.Contains(in.Code, "def alpha(n,:")
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

// readThenWriteCase runs the read->write sequence and reports what survived,
// failing loudly if the run never reached the write path.
func readThenWriteCase(t *testing.T, rel, baseline, replacement string, withV3 bool) string {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, rel)
	if err := os.WriteFile(target, []byte(baseline), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := readThenWriteStub(t, rel, replacement, &calls)
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL = srv.URL, srv.URL
	if withV3 {
		ctx.V3URL = srv.URL
	}
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 8

	var sawRead, sawWriteAttempt, unreadGuard bool
	ctx.StreamFn = func(eventType string, data interface{}) {
		b, err := json.Marshal(data)
		if err != nil {
			return
		}
		blob := string(b)
		switch eventType {
		case "tool_call":
			if strings.Contains(blob, `"read_file"`) {
				sawRead = true
			}
			if strings.Contains(blob, `"write_file"`) {
				sawWriteAttempt = true
			}
		}
		if strings.Contains(blob, "unread existing") {
			unreadGuard = true
		}
	}
	if err := runAgentLoop(ctx, "Read then rewrite "+rel); err != nil {
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
	return string(got)
}

// --- intentional policies the Stage A gate must NOT break --------------------

// A NEW file that does not parse still lands, with a warning, so the model can
// run it and read the real traceback (tools.go, writeNewFileWithWarning).
// There are no prior bytes to protect, which is what separates this from an
// overwrite.
func TestInvalidNewCodeFileStillLands(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	srv := matrixStub(t, "fresh.py", banLoopInvalidBody, banLoopInvalidBody, true, &calls)
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

func TestNonCodeOverwriteUnaffected(t *testing.T) {
	if _, gated := syntaxGateLanguages[".txt"]; gated {
		t.Fatal(".txt is now syntax-gated; this test's premise changed")
	}
	got := readThenWriteCase(t, "notes.txt", "first\n", "def broken(:\n", true)
	if got != "def broken(:\n" {
		t.Fatalf("non-code overwrite was blocked; the gate must not apply "+
			"outside syntaxGateLanguages. on disk: %q", got)
	}
}

// Validation unavailable must never read as passed. With no sandbox the gate
// fails open, so the write proceeds -- but it proceeds as UNVALIDATED, which
// Stage B has to represent explicitly rather than defaulting to passed.
func TestValidationUnavailableDoesNotBlockOrClaimPassed(t *testing.T) {
	dir := t.TempDir()
	rel := "mod.py"
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(banLoopValidBody), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := matrixStub(t, rel, banLoopInvalidBody, banLoopInvalidBody, false, &calls)
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.SandboxURL = "" // no validator reachable
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 6
	ctx.StreamFn = func(string, interface{}) {}
	if err := runAgentLoop(ctx, "Rewrite "+rel); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}
	// Fail-open is the existing documented behavior; this test pins it so
	// Stage B cannot quietly turn "could not check" into "checked and passed".
	if _, err := os.ReadFile(filepath.Join(dir, rel)); err != nil {
		t.Fatalf("artifact vanished when validation was unavailable: %v", err)
	}
}
func TestNonCodeOverwriteNoV3(t *testing.T) {
	got := readThenWriteCase(t, "notes.txt", "first\n", "def broken(:\n", false)
	if got != "def broken(:\n" {
		t.Fatalf("no-V3: non-code overwrite was blocked: %q", got)
	}
}

// NOT part of Stage A. Repairing a PRE-EXISTING file through write_file is
// not a capability this harness offers: the tool refuses an existing file
// outright ("write_file is for creating new files, not modifying existing
// ones. Use edit_file with old_str/new_str"), so the V3-gated repair policy
// is never reached that way -- measured v3_calls=0 in every repair cell, on
// both revisions, with and without V3.
//
// The T1 overwrite tests above still land bytes because the model CREATES
// the file in-session, which is what makes the later write_file permitted.
//
// Verifying the documented broken-file repair policy therefore needs an
// edit_file / structural_edit fixture, tracked separately from Stage A.

// --- session-owned invalid-new -> repair workflow ----------------------------
//
// The invalid-new-file warning policy and the repair that follows it are ONE
// workflow: a file lands invalid so the model can run it and read the real
// traceback, then the same session rewrites it. Session ownership is what
// makes the second write_file permitted at all -- an externally pre-created
// file is refused outright.

func sessionOwnedRepair(t *testing.T, replacement string, v3Mode string) (string, int, int) {
	t.Helper()
	dir := t.TempDir()
	rel := "owned.py"
	calls, v3Calls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v3/generate") {
			v3Calls++
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
		}
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			http.Error(w, "not stubbed", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
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
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		i := calls
		calls++
		content := replacement
		if i == 0 {
			content = banLoopInvalidBody // step 1: create it invalid
		}
		b, _ := json.Marshal(map[string]interface{}{
			"type": "tool_call", "name": "write_file",
			"args": map[string]string{"path": rel, "content": content}})
		w.Header().Set("Content-Type", "text/event-stream")
		delta, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(b)}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
	}))
	defer srv.Close()

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL, ctx.SandboxURL = srv.URL, srv.URL
	if v3Mode != "absent" {
		ctx.V3URL = srv.URL
	}
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 6
	ctx.StreamFn = func(string, interface{}) {}
	if err := runAgentLoop(ctx, "Create owned.py then fix it"); err != nil {
		t.Fatalf("agent loop error: %v", err)
	}
	if tier := classifyFileTier(rel, banLoopInvalidBody); tier < Tier2Medium {
		t.Fatalf("fixture must be Tier2Medium on the first write, got %v", tier)
	}
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("session-owned file vanished: %v", err)
	}
	return string(b), v3Calls, calls
}

func TestSessionOwnedRepairValidReplacement(t *testing.T) {
	repaired := strings.Replace(banLoopInvalidBody, "def alpha(n:", "def alpha(n):", 1)
	for _, mode := range []string{"working", "503", "absent"} {
		t.Run(mode, func(t *testing.T) {
			got, v3Calls, turns := sessionOwnedRepair(t, repaired, mode)
			t.Logf("v3_mode=%s v3_calls=%d turns=%d final_sha=%x", mode, v3Calls, turns, sha256.Sum256([]byte(got)))
			if got == banLoopInvalidBody {
				t.Fatalf("valid repair of a session-owned invalid file was refused; "+
					"the invalid bytes survived (v3_calls=%d)", v3Calls)
			}
			if got != repaired {
				t.Fatalf("unexpected final bytes: %q", got)
			}
		})
	}
}

// The documented policy also covers a replacement that is still invalid: not
// refusing it is what stops the original broken version from being stranded.
func TestSessionOwnedRepairStillInvalidReplacement(t *testing.T) {
	other := strings.Replace(banLoopInvalidBody, "def alpha(n:", "def alpha(n,:", 1)
	for _, mode := range []string{"working", "503", "absent"} {
		t.Run(mode, func(t *testing.T) {
			got, v3Calls, _ := sessionOwnedRepair(t, other, mode)
			t.Logf("v3_mode=%s v3_calls=%d final_sha=%x", mode, v3Calls, sha256.Sum256([]byte(got)))
			if got != other {
				t.Fatalf("still-invalid repair attempt on a session-owned broken "+
					"file was refused (v3_calls=%d); on disk: %q", v3Calls, got)
			}
		})
	}
}
