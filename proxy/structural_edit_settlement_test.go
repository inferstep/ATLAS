package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A clean structural_edit must settle its own mutation debt.
//
// structural_edit wrote bytes and recorded ValidationNotRun for them, on sound
// reasoning: it ran no check of its own, and reading v3-service's ok boolean
// as a syntax pass would be an inference. But not_run is not neutral at the
// exit -- a path retires mutation debt only on a verdict about its CURRENT
// bytes -- so every file edited this way carried debt nothing could discharge.
//
// Measured on offbyone in the a1e2a00 matrix: the run wrote a reproduction,
// showed the bug (`[[1, 2], [3, 4]]`), fixed chunks() with structural_edit,
// re-ran the reproduction and printed `[[1, 2], [3, 4], [5]]` -- then ended
// `unresolved_mutation_debt`, told that chunk.py "was never written in a state
// this run could check". It had been. The fix is to run the check, not to
// assume the answer.
func structuralEditWorld(t *testing.T, before, after string, checkerSaysValid bool) (*ToolResult, *AgentContext, string) {
	t.Helper()
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/internal/structural_edit":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "language": "python", "new_content": after,
				"old_size": len(before), "new_size": len(after),
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
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(dir, "chunk.py")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	ctx.V3URL = srv.URL
	ctx.SandboxURL = srv.URL
	ctx.RecordFileRead(path, before)
	ctx.RecordBodySeen(path)

	args, _ := json.Marshal(map[string]string{
		"path": "chunk.py", "selector": "function:chunks", "content": after})
	res := executeToolCall("structural_edit", args, ctx)
	return res, ctx, path
}

const chunkBefore = "def chunks(v, n):\n    out = []\n    for i in range(0, len(v) - n, n):\n        out.append(v[i:i + n])\n    return out\n"
const chunkAfter = "def chunks(v, n):\n    out = []\n    for i in range(0, len(v), n):\n        out.append(v[i:i + n])\n    return out\n"

func TestACleanStructuralEditSettlesItsOwnDebt(t *testing.T) {
	res, ctx, path := structuralEditWorld(t, chunkBefore, chunkAfter, true)
	if res == nil || !res.Success {
		t.Fatalf("the splice must land: %+v", res)
	}
	if res.ValidationStatus != ValidationPassed {
		t.Errorf("validation = %q, want passed -- the checker was asked and said valid; "+
			"not_run leaves debt nothing can discharge", res.ValidationStatus)
	}
	// The completion path's own predicate, not a proxy for it.
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[ledgerKey(ctx, path)]
	ctx.LedgerMu.Unlock()
	if !validationSettles(d) {
		t.Error("a clean structural_edit did not settle: the session would end " +
			"unresolved_mutation_debt on work it had demonstrably done")
	}
}

func TestABrokenStructuralEditIsRecordedAsBrokenNotUnknown(t *testing.T) {
	res, ctx, path := structuralEditWorld(t, chunkBefore, chunkAfter, false)
	if res == nil || !res.Success {
		t.Fatalf("this tool does not refuse after the rename: %+v", res)
	}
	if res.ValidationStatus != ValidationFailed {
		t.Errorf("validation = %q, want failed", res.ValidationStatus)
	}
	var out StructuralEditOutput
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatalf("output is not StructuralEditOutput: %v", err)
	}
	if out.Warning == "" {
		t.Error("the model was told the edit landed and nothing else, over bytes " +
			"the checker had just reported as invalid")
	}
	ctx.LedgerMu.Lock()
	d := ctx.Ledger[ledgerKey(ctx, path)]
	ctx.LedgerMu.Unlock()
	if validationSettles(d) {
		t.Error("a file that does not parse settled its debt")
	}
}
