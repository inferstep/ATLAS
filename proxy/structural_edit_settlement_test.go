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
// structural_edit runs no check of its own, and reading v3-service's ok
// boolean as a syntax pass would be an inference. But not_run is not neutral
// at the exit: a path retires mutation debt only on a verdict about its
// CURRENT bytes, so a file edited this way would carry debt nothing could
// discharge. So the tool asks the syntax checker about the bytes it landed,
// and records a broken result as broken, not as unknown.

const chunkBefore = "def chunks(v, n):\n    out = []\n    for i in range(0, len(v) - n, n):\n        out.append(v[i:i + n])\n    return out\n"
const chunkAfter = "def chunks(v, n):\n    out = []\n    for i in range(0, len(v), n):\n        out.append(v[i:i + n])\n    return out\n"

func TestAStructuralEditRecordsTheCheckersVerdictOnItsBytes(t *testing.T) {
	for _, tc := range []struct {
		name             string
		checkerSaysValid bool
		wantStatus       ValidationStatus
		wantSettles      bool
		wantWarning      bool
	}{
		{"clean edit settles its own debt", true, ValidationPassed, true, false},
		{"broken edit is recorded as broken", false, ValidationFailed, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/internal/structural_edit":
					json.NewEncoder(w).Encode(map[string]interface{}{
						"success": true, "language": "python", "new_content": chunkAfter,
						"old_size": len(chunkBefore), "new_size": len(chunkAfter),
					})
				case strings.HasSuffix(r.URL.Path, "/syntax-check"):
					out := map[string]interface{}{"valid": tc.checkerSaysValid}
					if !tc.checkerSaysValid {
						out["errors"] = []string{"SyntaxError: invalid syntax (line 2)"}
					}
					json.NewEncoder(w).Encode(out)
				case r.URL.Path == "/internal/cyclomatic_complexity":
					json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
				default:
					http.Error(w, "unexpected endpoint "+r.URL.Path, http.StatusTeapot)
				}
			}))
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
			if res == nil || !res.Success {
				t.Fatalf("the splice must land: %+v", res)
			}
			if res.ValidationStatus != tc.wantStatus {
				t.Errorf("validation = %q, want %q: the checker was asked", res.ValidationStatus, tc.wantStatus)
			}
			var out StructuralEditOutput
			if err := json.Unmarshal(res.Data, &out); err != nil {
				t.Fatalf("output is not StructuralEditOutput: %v", err)
			}
			if (out.Warning != "") != tc.wantWarning {
				t.Errorf("warning %q; want a warning: %v", out.Warning, tc.wantWarning)
			}
			// The completion path's own predicate, not a stand-in for it.
			ctx.LedgerMu.Lock()
			d := ctx.Ledger[ledgerKey(ctx, path)]
			ctx.LedgerMu.Unlock()
			if validationSettles(d) != tc.wantSettles {
				t.Errorf("the edit settles its debt: %v, want %v", validationSettles(d), tc.wantSettles)
			}
		})
	}
}
