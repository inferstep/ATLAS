package main

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Reachability proof for the T0/T1 direct-path structural gate.
//
// Three earlier attempts to exercise this branch failed on unverified
// premises: the wrong guard location, a routing premise that disabled the
// gate, and the wrong endpoint. This fixture pins the real contract instead:
//
//	POST {V3URL}/internal/structural_check
//	  request  {"path": <abs>, "source": <content>, "project_context": {...}}
//	  response {"unresolved": []string}
//
// editIntroducesUnresolved calls it TWICE -- edited side first, then original
// side -- and refuses only when both succeed and the edited side names
// something the original did not. The gate FAILS OPEN otherwise, which is
// exactly how a previous fixture let the write land while appearing to test
// the gate.
//
// This task proves reachability ONLY. Classification is expected to remain
// Unknown here; restoring it is a separate change with its own RED evidence.

type structuralStub struct {
	mu       sync.Mutex
	bodies   []map[string]interface{}
	rejected []string
}

func (s *structuralStub) record(b map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies = append(s.bodies, b)
}

func newStructuralStub(t *testing.T, introduced string) (*httptest.Server, *structuralStub) {
	t.Helper()
	st := &structuralStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The write path also consults /internal/cyclomatic_complexity. That
		// is a legitimate call here, so it is answered benignly rather than
		// treated as a violation -- the strict stub caught it on the first
		// run, which is what a strict stub is for.
		if r.URL.Path == "/internal/cyclomatic_complexity" {
			json.NewEncoder(w).Encode(map[string]interface{}{"functions": []interface{}{}})
			return
		}
		// These must never be reached on the T0/T1 direct path: V3 candidate
		// generation belongs to the Tier2+ branch, and symbol_index is a
		// different service (project-context assembly, context.go).
		if r.URL.Path != "/internal/structural_check" || r.Method != http.MethodPost {
			st.mu.Lock()
			st.rejected = append(st.rejected, r.Method+" "+r.URL.Path)
			st.mu.Unlock()
			http.Error(w, "unexpected endpoint", http.StatusTeapot)
			return
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "undecodable body", http.StatusBadRequest)
			return
		}
		st.record(body)
		src, _ := body["source"].(string)
		// The response schema is {"ok": bool, "unresolved": []string}. The
		// client fails open on !ok, so omitting it silently disables the gate
		// -- diagnosed by calling checkStructuralUnresolved directly and
		// seeing ok=false against a correct-looking unresolved list.
		out := map[string]interface{}{"ok": true, "unresolved": []string{}}
		if strings.Contains(src, introduced+"(") {
			out["unresolved"] = []string{introduced}
		}
		json.NewEncoder(w).Encode(out)
	}))
	return srv, st
}

func TestT0T1StructuralBranchIsReachable(t *testing.T) {
	dir := t.TempDir()
	rel := "small.py"
	original := "def alpha():\n    return 1\n"
	proposed := "def alpha():\n    return missing_helper()\n"

	if tier := classifyFileTier(rel, proposed); tier >= Tier2Medium {
		t.Fatalf("fixture must stay below Tier2Medium, got %v", tier)
	}

	srv, stub := newStructuralStub(t, "missing_helper")
	defer srv.Close()

	target := filepath.Join(dir, rel)
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	priorHash := sha256.Sum256([]byte(original))

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(string, interface{}) {}
	ctx.V3URL = srv.URL // structural gate needs this; without it the gate no-ops
	ctx.SandboxURL = "" // no syntax gate: this branch is structural-only
	// Session ownership: the direct path requires the session to own the file.
	ctx.SessionWrites[rel] = true

	args, _ := json.Marshal(map[string]string{"path": rel, "content": proposed})
	res := executeToolCall("write_file", args, ctx)

	// --- routing evidence -------------------------------------------------
	if len(stub.rejected) > 0 {
		t.Fatalf("unexpected endpoints were called (V3 generation or symbol_index "+
			"must not be used on this path): %v", stub.rejected)
	}
	if len(stub.bodies) < 2 {
		t.Fatalf("expected both the edited-side and original-side structural "+
			"checks, got %d request(s); the branch was not reached",
			len(stub.bodies))
	}
	first, _ := stub.bodies[0]["source"].(string)
	second, _ := stub.bodies[1]["source"].(string)
	if !strings.Contains(first, "missing_helper") {
		t.Errorf("first structural_check must carry the PROPOSED content, got %q", first)
	}
	if strings.Contains(second, "missing_helper") {
		t.Errorf("second structural_check must carry the ORIGINAL content, got %q", second)
	}
	if p, _ := stub.bodies[0]["path"].(string); p == "" {
		t.Error("structural_check request omitted the path field")
	}

	// --- refusal evidence -------------------------------------------------
	if res.Success {
		t.Fatalf("the structural gate did not refuse: %+v", res)
	}
	if !strings.Contains(res.Error, "missing_helper") {
		t.Fatalf("refusal came from a different gate; error = %q", res.Error)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("target vanished: %v", err)
	}
	if sha256.Sum256(after) != priorHash {
		t.Fatalf("target bytes changed on a refusal: %q", string(after))
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".atlas.tmp") {
			t.Errorf("temporary artifact left behind: %s", e.Name())
		}
	}

	// --- classification ---------------------------------------------------
	// Structural validation failed before any mutation. Syntax does NOT run
	// on this route, so no syntax verdict is implied in either direction.
	if res.MutationStatus != MutationRefused {
		t.Errorf("MutationStatus = %q, want refused", res.MutationStatus)
	}
	if res.MutationStatus.Applied() {
		t.Error("a structural refusal must never read as applied")
	}
	if res.ValidationKind != ValidationKindStructural {
		t.Errorf("ValidationKind = %q, want structural", res.ValidationKind)
	}
	if res.ValidationStatus != ValidationFailed {
		t.Errorf("ValidationStatus = %q, want failed", res.ValidationStatus)
	}
	if !strings.Contains(res.ValidationDetail, "missing_helper") {
		t.Errorf("ValidationDetail must name the unresolved symbol, got %q",
			res.ValidationDetail)
	}
	if !res.Classified() {
		t.Errorf("result not fully classified: %+v", res)
	}
}
