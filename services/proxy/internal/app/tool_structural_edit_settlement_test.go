package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itigges22/atlas-proxy/internal/wire"
)

// A clean structural_edit must settle its own mutation debt.
//
// structural_edit runs no check of its own, and reading v3-service's ok
// boolean as a syntax pass would be an inference. But not_run is not neutral
// at the exit: a path retires mutation debt only on a verdict about its
// CURRENT bytes, so a file edited this way would carry debt nothing could
// discharge. So the tool asks the syntax checker about the bytes it landed,
// and records a broken result as broken, not as unknown.

const chunkBefore = "def chunks(v, n):\n    out = []\n    for i in range(0, len(v) - n, n):\n        out.append(v[i:i + n])\n    return out\n"
const chunkAfter = "def chunks(v, n):\n    out = []\n    for i in range(0, len(v), n):\n        out.append(v[i:i + n])\n    return out\n"

// structuralEditServices stands in for v3-service and the sandbox in one
// structural_edit of chunk.py: the splice gives chunkAfter, and the syntax
// checker calls those bytes valid or not.
func structuralEditServices(checkerSaysValid bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/internal/structural_edit":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "language": "python", "new_content": chunkAfter,
				"old_size": len(chunkBefore), "new_size": len(chunkAfter),
			})
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			out := map[string]interface{}{"valid": checkerSaysValid}
			if !checkerSaysValid {
				out["errors"] = []string{"SyntaxError: invalid syntax (line 2)"}
			}
			json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/internal/cyclomatic_complexity":
			json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
		default:
			http.Error(w, "unexpected endpoint "+r.URL.Path, http.StatusTeapot)
		}
	}
}

func TestACleanStructuralEditSettlesItsOwnDebt(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(structuralEditServices(true))
	defer srv.Close()
	path := filepath.Join(dir, "chunk.py")
	if err := os.WriteFile(path, []byte(chunkBefore), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	ctx.V3URL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.RecordFileRead(path, chunkBefore)
	ctx.RecordBodySeen(path)

	args, _ := json.Marshal(map[string]string{
		"path": "chunk.py", "selector": "function:chunks", "content": chunkAfter})
	res := executeToolCall("structural_edit", args, ctx)

	if res == nil {
		t.Fatal("the tool gave no result")
	}
	if !res.Success {
		t.Fatalf("the splice must land: %+v", res)
	}
	if res.ValidationStatus != wire.ValidationPassed {
		t.Errorf("validation = %q, want %q: the checker was asked", res.ValidationStatus, wire.ValidationPassed)
	}
	var out wire.StructuralEditOutput
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatalf("output is not StructuralEditOutput: %v", err)
	}
	if out.Warning != "" {
		t.Errorf("a clean edit carries no warning, got %q", out.Warning)
	}
	// The completion path's own predicate, not a stand-in for it.
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[ledgerKey(ctx, path)]
	ctx.LedgerMu.Unlock()
	if !validationSettles(d) {
		t.Error("a clean edit must settle its own debt")
	}
}

func TestABrokenStructuralEditIsRecordedAsBroken(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(structuralEditServices(false))
	defer srv.Close()
	path := filepath.Join(dir, "chunk.py")
	if err := os.WriteFile(path, []byte(chunkBefore), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	ctx.V3URL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.RecordFileRead(path, chunkBefore)
	ctx.RecordBodySeen(path)

	args, _ := json.Marshal(map[string]string{
		"path": "chunk.py", "selector": "function:chunks", "content": chunkAfter})
	res := executeToolCall("structural_edit", args, ctx)

	// The tool does not refuse after the rename, whatever the checker says.
	if res == nil {
		t.Fatal("the tool gave no result")
	}
	if !res.Success {
		t.Fatalf("the splice must land: %+v", res)
	}
	if res.ValidationStatus != wire.ValidationFailed {
		t.Errorf("validation = %q, want %q: the checker was asked", res.ValidationStatus, wire.ValidationFailed)
	}
	var out wire.StructuralEditOutput
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatalf("output is not StructuralEditOutput: %v", err)
	}
	if out.Warning == "" {
		t.Error("a broken edit must carry a warning")
	}
	// The completion path's own predicate, not a stand-in for it.
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[ledgerKey(ctx, path)]
	ctx.LedgerMu.Unlock()
	if validationSettles(d) {
		t.Error("a broken edit must not settle its debt")
	}
}
