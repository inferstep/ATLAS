package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The direct new-file gate: reached when the V3 activation condition is false.
// A sub-Tier2 new file does NOT skip validation -- it falls through to here.
func newFileCase(t *testing.T, rel, content string, withSandbox bool) (*ToolResult, string, int, string) {
	t.Helper()
	dir := t.TempDir()
	calls := 0
	var v3Calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v3/") {
			v3Calls++
			http.Error(w, "V3 must not be reached on this route", http.StatusTeapot)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			calls++
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			valid := !strings.Contains(in.Code, "def broken(:")
			out := map[string]interface{}{"valid": valid}
			if !valid {
				out["errors"] = []string{"SyntaxError: invalid syntax (line 1)"}
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// Route preconditions.
	if tier := classifyFileTier(rel, content); tier >= Tier2Medium {
		t.Fatalf("fixture must be sub-Tier2 to reach the direct gate, got %v", tier)
	}
	if _, err := os.Stat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
		t.Fatal("destination must be absent before dispatch")
	}

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	if withSandbox {
		ctx.SandboxURL = srv.URL
	}
	args, _ := json.Marshal(map[string]string{"path": rel, "content": content})
	res := executeToolCall("write_file", args, ctx)
	if v3Calls != 0 {
		t.Fatalf("V3 generation was reached (%d calls); wrong route", v3Calls)
	}
	b, _ := os.ReadFile(filepath.Join(dir, rel))
	return res, string(b), calls, dir
}

const smallOK = "A = 1\nB = 2\n"
const smallBad = "def broken(:\n"

func TestNewFileRouteClassification(t *testing.T) {
	t.Run("demonstrated pass", func(t *testing.T) {
		res, bytes, calls, _ := newFileCase(t, "m.py", smallOK, true)
		if !res.Success || bytes != smallOK {
			t.Fatalf("write did not land: success=%v bytes=%q", res.Success, bytes)
		}
		if res.MutationStatus != MutationApplied ||
			res.ValidationKind != ValidationKindSyntax ||
			res.ValidationStatus != ValidationPassed {
			t.Errorf("got %q/%q/%q, want applied/syntax/passed",
				res.MutationStatus, res.ValidationKind, res.ValidationStatus)
		}
		if calls != 1 {
			t.Errorf("syntax-check calls = %d, want exactly 1 (single evaluation)", calls)
		}
		if !res.Classified() {
			t.Error("result not fully classified")
		}
	})

	t.Run("applicable but sandbox unavailable", func(t *testing.T) {
		res, bytes, _, _ := newFileCase(t, "m.py", smallOK, false)
		if !res.Success || bytes != smallOK {
			t.Fatal("fail-open write did not land")
		}
		if res.ValidationStatus != ValidationNotRun || res.ValidationKind != ValidationKindSyntax {
			t.Errorf("got %q/%q, want syntax/not_run", res.ValidationKind, res.ValidationStatus)
		}
		if res.ValidationStatus.Passed() {
			t.Error("unavailable validation must never read as passed")
		}
	})

	t.Run("no checks applicable", func(t *testing.T) {
		res, bytes, calls, _ := newFileCase(t, "notes.txt", "hello\n", true)
		if !res.Success || bytes != "hello\n" {
			t.Fatal("non-code write did not land")
		}
		if res.ValidationKind != ValidationKindNone ||
			res.ValidationStatus != ValidationNotApplicable {
			t.Errorf("got %q/%q, want none/not_applicable",
				res.ValidationKind, res.ValidationStatus)
		}
		if calls != 0 {
			t.Errorf("syntax-check calls = %d, want 0 for a non-code file", calls)
		}
	})

	t.Run("demonstrated failure lands with warning", func(t *testing.T) {
		res, bytes, calls, dir := newFileCase(t, "m.py", smallBad, true)
		if !res.Success {
			t.Fatal("the warned-new-file policy must still land the bytes")
		}
		if bytes != smallBad {
			t.Fatalf("warned write changed the bytes: %q", bytes)
		}
		if res.MutationStatus != MutationApplied ||
			res.ValidationKind != ValidationKindSyntax ||
			res.ValidationStatus != ValidationFailed {
			t.Errorf("got %q/%q/%q, want applied/syntax/failed",
				res.MutationStatus, res.ValidationKind, res.ValidationStatus)
		}
		if !strings.Contains(string(res.Data), "does not parse") {
			t.Errorf("warning lost from the result payload: %s", res.Data)
		}
		if calls != 1 {
			t.Errorf("syntax-check calls = %d, want exactly 1 -- the failure "+
				"path must not re-evaluate the structured checker", calls)
		}
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if strings.Contains(e.Name(), ".atlas.tmp") {
				t.Errorf("temporary artifact survived: %s", e.Name())
			}
		}
	})
}

// Validation passed and mutation failed are orthogonal: uses the already
// proven temp-write failure mechanism, not a new injection path.
func TestNewFilePassedThenWriteFailure(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "ro")
	os.Mkdir(sub, 0o755)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/syntax-check") {
			json.NewEncoder(w).Encode(map[string]interface{}{"valid": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	os.Chmod(sub, 0o555) // temp write must fail
	defer os.Chmod(sub, 0o755)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	ctx.SandboxURL = srv.URL
	args, _ := json.Marshal(map[string]string{"path": "ro/new.py", "content": smallOK})
	res := executeToolCall("write_file", args, ctx)

	if res.Success {
		t.Fatal("a failed temp write must not report success")
	}
	if res.MutationStatus != MutationFailed {
		t.Errorf("MutationStatus = %q, want failed", res.MutationStatus)
	}
	if res.ValidationStatus != ValidationPassed || res.ValidationKind != ValidationKindSyntax {
		t.Errorf("validation = %q/%q, want syntax/passed -- the observation on "+
			"those exact bytes must survive the mutation failure",
			res.ValidationKind, res.ValidationStatus)
	}
	if _, err := os.Stat(filepath.Join(sub, "new.py")); err == nil {
		t.Error("target exists despite the failed write")
	}
}

// Structural proof that the direct gate migrated and the V3 preflight did not.
func TestDirectNewFileGateUsesStructuredChecker(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "tools.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var structured, legacy int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch id.Name {
		case "fallbackSyntaxOutcomeFor":
			structured++
		case "checkFallbackSyntax":
			legacy++
		}
		return true
	})
	if structured < 1 {
		t.Fatal("the direct new-file gate does not call fallbackSyntaxOutcomeFor")
	}
	// When this was written the direct new-file gate was the only migrated
	// route and the wrapper still had 14 callers. They are all gone now, which
	// is the end state this test was steering toward rather than a regression.
	if legacy != 0 {
		t.Errorf("legacy call sites remaining in tools.go = %d, want 0", legacy)
	}
	t.Logf("structured call sites=%d legacy call sites remaining=%d", structured, legacy)
}
