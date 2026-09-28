package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The active edit-test-fix loop: after a successful write of foo.py and a
// FAILED run referencing it, the next write must fast-path (skip V3).
func TestIsActiveDebugIteration(t *testing.T) {
	ctx := &AgentContext{
		SessionWrites: map[string]bool{"foo.py": true},
		Messages: []AgentMessage{
			{Role: "assistant", Content: "..."},
			{Role: "tool", ToolName: "run_command",
				Content: `{"success":false,"error":"File \"foo.py\", line 3\nSyntaxError"}`},
		},
	}
	if !isActiveDebugIteration(ctx, "foo.py") {
		t.Error("failed run of an already-written file should be active iteration")
	}
	// First write of a file (not in SessionWrites) → V3, not fast-path.
	if isActiveDebugIteration(ctx, "bar.py") {
		t.Error("first write of a file must not fast-path")
	}
	// Last tool action was a read, not a failing run → not iterating.
	ctx.Messages[1] = AgentMessage{Role: "tool", ToolName: "read_file", Content: "..."}
	if isActiveDebugIteration(ctx, "foo.py") {
		t.Error("a read as the last action is not an edit-test-fix loop")
	}
	// Run succeeded → task likely done, not iterating.
	ctx.Messages[1] = AgentMessage{Role: "tool", ToolName: "run_command",
		Content: `{"success":true,"data":{"stdout":"ok"}}`}
	if isActiveDebugIteration(ctx, "foo.py") {
		t.Error("a passing run is not an active fix loop")
	}
}

// isBinaryContent: NUL-bearing data is binary; clean text is not.
func TestIsBinaryContent(t *testing.T) {
	if !isBinaryContent([]byte("\x7fELF\x02\x01\x00\x00garbage")) {
		t.Error("ELF header with NULs should be binary")
	}
	if isBinaryContent([]byte("def foo():\n    return 1\n")) {
		t.Error("plain source text must not be flagged binary")
	}
	if isBinaryContent([]byte("")) {
		t.Error("empty file is not binary")
	}
}

// An unbounded read of a huge file is capped so it can't blow
// the context window; an explicit limit is honored as-is.
func TestReadFileSizeCap(t *testing.T) {
	// Build content well over the cap.
	big := strings.Repeat("some line of text here\n", (maxReadFileBytes/23)+5000)
	if len(big) <= maxReadFileBytes {
		t.Fatal("test setup: content not over cap")
	}
	// Simulate the cap logic (mirrors the read_file body).
	content := big
	if len(content) > maxReadFileBytes {
		cut := maxReadFileBytes
		if nl := strings.LastIndexByte(content[:cut], '\n'); nl > 0 {
			cut = nl + 1
		}
		content = content[:cut] + "\n... [read_file truncated"
	}
	if len(content) > maxReadFileBytes+200 {
		t.Errorf("capped content still too large: %d", len(content))
	}
	if !strings.Contains(content, "truncated") {
		t.Error("cap note missing")
	}
}

// #147 review #7: a UTF-16 BOM identifies text even though it has NULs.
func TestIsBinaryContentUTF16BOM(t *testing.T) {
	if isBinaryContent([]byte{0xFF, 0xFE, 0x68, 0x00, 0x69, 0x00}) {
		t.Error("UTF-16 LE (BOM) must be treated as text")
	}
	if isBinaryContent([]byte{0xFE, 0xFF, 0x00, 0x68}) {
		t.Error("UTF-16 BE (BOM) must be treated as text")
	}
	if !isBinaryContent([]byte("\x7fELF\x00\x00garbage")) {
		t.Error("ELF must still be binary")
	}
}

// #147 review #12: filename match must be a whole token, not a substring.
func TestMentionsFilename(t *testing.T) {
	if mentionsFilename(`{"error":"data.py line 3"}`, "a.py") {
		t.Error("a.py must not match inside data.py")
	}
	if mentionsFilename("domain.py failed", "main.py") {
		t.Error("main.py must not match inside domain.py")
	}
	if !mentionsFilename(`{"error":"File \"app.py\", line 3"}`, "app.py") {
		t.Error("app.py must match as a whole token")
	}
	if !mentionsFilename("python3 app.py:12: error", "app.py") {
		t.Error("app.py must match when bounded by a colon")
	}
}

// closestLineHint maps a from-memory paraphrase onto the real file line
// so the edit_file mismatch error can anchor the model's retry in actual
// content. The motivating case (Jun 8 2026 flask test): the model
// searched for `item = items[id + 1]` when the file's real line was
// `return jsonify(items[item_id + 1])`, then gave up on the surgical
// edit and rewrote the whole function from the same faulty memory.
func TestClosestLineHintParaphrase(t *testing.T) {
	content := `from flask import Flask, jsonify

app = Flask(__name__)

@app.route("/items/<int:item_id>", methods=["GET"])
def get_item(item_id):
    return jsonify(items[item_id + 1])
`
	hint := closestLineHint(content, "item = items[id + 1]")
	if hint == "" {
		t.Fatal("expected a hint for a near-miss paraphrase, got none")
	}
	if !strings.Contains(hint, "jsonify(items[item_id + 1])") {
		t.Errorf("hint should quote the real line, got: %s", hint)
	}
	if !strings.Contains(hint, "line 7") {
		t.Errorf("hint should carry the real line number, got: %s", hint)
	}
}

func TestClosestLineHintUnrelatedReturnsNothing(t *testing.T) {
	content := "def alpha():\n    return 1\n"
	if hint := closestLineHint(content, "zebra elephant giraffe"); hint != "" {
		t.Errorf("unrelated old_str must not get an anchor, got: %s", hint)
	}
}

func TestClosestLineHintEmptyOldStr(t *testing.T) {
	if hint := closestLineHint("x = 1\n", "   \n  "); hint != "" {
		t.Errorf("blank old_str must not get an anchor, got: %s", hint)
	}
}

// File-tier classifier tests. Covers the post-inversion behaviour where
// V3 should fire much more often: small-but-routed code files (flask,
// express) used to slip through to T1 because hasLogicIndicators needed
// 3+ patterns and the <50-line short-circuit fired first.

func TestClassifyFileTierFlaskAppPyIsT2(t *testing.T) {
	// 33-line flask routing module — exactly the file the user was
	// debugging when V3 never fired. Must be T2 now.
	content := `from flask import Flask, render_template, redirect, url_for

app = Flask(__name__)

@app.route('/')
def index():
    return render_template('index.html')

@app.route('/product')
def product():
    return render_template('product.html')

@app.route('/contact')
def contact():
    return render_template('contact.html')

if __name__ == '__main__':
    app.run(debug=True, host='0.0.0.0', port=5000)
`
	if got := classifyFileTier("app.py", content); got < Tier2Medium {
		t.Errorf("flask app.py = %v, want >= T2 (was T1 under old <50-line rule)", got)
	}
}

func TestClassifyFileTierTinyConfigStaysT1(t *testing.T) {
	// Single-line .gitignore / 5-line shell wrapper — V3 has nothing to
	// diversify on, must stay T1 to avoid wasted pipeline cost.
	if got := classifyFileTier(".gitignore", "node_modules\n.env\n"); got != Tier1Simple {
		t.Errorf(".gitignore = %v, want T1", got)
	}
	if got := classifyFileTier("foo.sh", "#!/bin/sh\nexec node app.js\n"); got != Tier1Simple {
		t.Errorf("tiny shell script = %v, want T1", got)
	}
}

func TestClassifyFileTierConfigByName(t *testing.T) {
	// Config files by name are always T1 regardless of line count.
	configFiles := []string{"package.json", "go.mod", "Dockerfile", "requirements.txt"}
	for _, f := range configFiles {
		if got := classifyFileTier(f, "x\n\n\n\n\n\n\n\n\n\n\n\n\n"); got != Tier1Simple {
			t.Errorf("classifyFileTier(%q) = %v, want T1", f, got)
		}
	}
}

func TestClassifyFileTierCodeExtBenefitOfDoubt(t *testing.T) {
	// 15-line python file with no recognisable logic patterns — naming
	// it .py is enough to get T2 now, because V3 helps with code shape
	// even when the model didn't tag any specific framework idiom.
	content := `# generated module
NAME = "atlas"
VERSION = "0.1.0"
AUTHOR = "team"
LICENSE = "MIT"
DESCRIPTION = "demo"
KEYWORDS = ["a", "b"]
EXTRAS = {}
DEPS = []
DEV_DEPS = []
URL = "https://example.com"
HOMEPAGE = URL
DOWNLOADS = URL + "/dl"
`
	if got := classifyFileTier("constants.py", content); got != Tier2Medium {
		t.Errorf("constants.py = %v, want T2 (code-ext fallback)", got)
	}
}

func TestClassifyFileTierTinyCodeFileStaysT1(t *testing.T) {
	// 4 lines — below the new 10-line floor, even .py is T1.
	if got := classifyFileTier("hello.py", "print('hi')\n"); got != Tier1Simple {
		t.Errorf("1-line script = %v, want T1", got)
	}
}

func TestClassifyFileTierMidSizedHtmlIsT2(t *testing.T) {
	// 90-line flask template — used to fall through to T1 because the
	// markup branch only fired at >=150 lines. Now .html is in codeExts
	// so V3 fires on real templates instead of only on huge mockups.
	content := ""
	for i := 0; i < 90; i++ {
		content += "<p>row " + string(rune('a'+i%26)) + "</p>\n"
	}
	if got := classifyFileTier("templates/index.html", content); got != Tier2Medium {
		t.Errorf("90-line index.html = %v, want T2 (was T1 under <150-line markup veto)", got)
	}
}

func TestHasLogicIndicatorsFlaskAppHits(t *testing.T) {
	// Verifies the threshold drop + flask-pattern additions. A flask
	// app.py used to register only "def " (1 indicator); now the route
	// decorators count, putting it well over the new threshold of 2.
	content := `from flask import Flask, render_template

app = Flask(__name__)

@app.route('/')
def index():
    return render_template('index.html')
`
	if !hasLogicIndicators(content) {
		t.Errorf("hasLogicIndicators(flask snippet) = false, want true")
	}
}

// Tests for the edit_file fallback matchers (findFuzzyLineMatch,
// findActualString) and recoverTruncatedWriteFile. These decide where an
// edit lands when the model's old_str doesn't byte-match the file — the
// silent-wrong-edit failure class, so the fail-safe paths matter as much
// as the matches.

func TestFindFuzzyLineMatch(t *testing.T) {
	file := "func main() {\n" +
		"\tx := 1\n" +
		"\tfmt.Println(x)\n" +
		"}\n" +
		"\n" +
		"func helper() {\n" +
		"\tx := 1\n" +
		"}\n"

	t.Run("whitespace drift resolves to the file's own lines", func(t *testing.T) {
		// Model remembered the lines with spaces instead of tabs.
		got, ok := findFuzzyLineMatch(file, "    x := 1\n    fmt.Println(x)")
		if !ok {
			t.Fatal("no match for whitespace-drifted old_str")
		}
		// The returned text must be the FILE's bytes (tabs), so the
		// subsequent strings.Replace hits.
		if got != "\tx := 1\n\tfmt.Println(x)" {
			t.Errorf("matched %q, want the file's original tab-indented lines", got)
		}
		if !strings.Contains(file, got) {
			t.Error("returned match is not a substring of the file")
		}
	})

	t.Run("ambiguous match fails safe", func(t *testing.T) {
		// `x := 1` appears in both functions — editing either would be a
		// guess, and a wrong guess is a silent wrong edit.
		if got, ok := findFuzzyLineMatch(file, "x := 1"); ok {
			t.Errorf("ambiguous single line matched %q, want failure", got)
		}
	})

	t.Run("no match returns false", func(t *testing.T) {
		if _, ok := findFuzzyLineMatch(file, "y := 2"); ok {
			t.Error("nonexistent line reported a match")
		}
	})

	t.Run("all-whitespace target is refused", func(t *testing.T) {
		if _, ok := findFuzzyLineMatch(file, "   \n\t"); ok {
			t.Error("all-whitespace old_str matched — would edit an arbitrary blank region")
		}
	})

	t.Run("trailing newline in old_str is tolerated", func(t *testing.T) {
		got, ok := findFuzzyLineMatch(file, "\tfmt.Println(x)\n}\n")
		if !ok {
			t.Fatal("trailing-newline old_str did not match")
		}
		if got != "\tfmt.Println(x)\n}" {
			t.Errorf("matched %q", got)
		}
	})

	t.Run("empty old_str is refused", func(t *testing.T) {
		if _, ok := findFuzzyLineMatch(file, ""); ok {
			t.Error("empty old_str matched")
		}
	})
}

func TestFindActualString(t *testing.T) {
	t.Run("direct match wins untouched", func(t *testing.T) {
		if got := findActualString(`say "hi"`, `say "hi"`); got != `say "hi"` {
			t.Errorf("got %q", got)
		}
	})

	t.Run("curly quotes in old_str match straight quotes in file", func(t *testing.T) {
		file := `msg := "hello"`
		oldStr := "msg := “hello”" // model emitted curly quotes
		got := findActualString(file, oldStr)
		if got != `msg := "hello"` {
			t.Errorf("got %q, want the straight-quoted form present in the file", got)
		}
	})

	t.Run("straight apostrophe in old_str matches curly apostrophe in file", func(t *testing.T) {
		// The reverse (denormalize) direction is best-effort: straight
		// singles map to the right-single curly — the apostrophe case
		// (prose in docs/comments) is what it exists for.
		file := "// don’t retry on 4xx"
		got := findActualString(file, "// don't retry on 4xx")
		if got != "// don’t retry on 4xx" {
			t.Errorf("got %q, want the file's curly-apostrophe form", got)
		}
	})

	t.Run("no variant matches returns empty", func(t *testing.T) {
		if got := findActualString("abc", "xyz"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

// Tests for buildResponseFormat — the schema-constrained sampling path
// (#33). These tests pin the response_format payload shape that goes
// over the wire to llama-server, so a regression that silently flips
// the default back to loose JSON gets caught.

func TestBuildResponseFormat_DefaultIsStrictSchema(t *testing.T) {
	// Default mode (no env var set) must produce the schema-constrained
	// payload. This is the #33 perf optimization — losing the schema
	// means we silently regress to the wasted-token retry pattern.
	t.Setenv("ATLAS_GRAMMAR_MODE", "")
	rf := buildResponseFormat()

	m, ok := rf.(map[string]interface{})
	if !ok {
		t.Fatalf("strict mode should return map[string]interface{}, got %T", rf)
	}
	if m["type"] != "json_object" {
		t.Errorf("expected type=json_object, got %v", m["type"])
	}
	if _, has := m["schema"]; !has {
		t.Error("strict mode must include a 'schema' key — without it " +
			"llama-server falls back to plain valid-JSON-only enforcement " +
			"and the #33 optimization no-ops")
	}
}

func TestBuildResponseFormat_LooseDropsSchema(t *testing.T) {
	// Escape hatch: ATLAS_GRAMMAR_MODE=loose reverts to the pre-#33
	// "any valid JSON" behavior. Used when a model handles the schema
	// poorly or for debugging. The payload must NOT include 'schema'.
	t.Setenv("ATLAS_GRAMMAR_MODE", "loose")
	rf := buildResponseFormat()

	m, ok := rf.(map[string]string)
	if !ok {
		t.Fatalf("loose mode should return map[string]string, got %T", rf)
	}
	if m["type"] != "json_object" {
		t.Errorf("expected type=json_object, got %v", m["type"])
	}
}

func TestBuildResponseFormat_UnknownModeDefaultsToStrict(t *testing.T) {
	// Anything other than "loose" should fall through to strict.
	// Future modes (e.g. "json_schema" for the OpenAI-style payload)
	// would need explicit branches; until then unknown = strict, never
	// silently regress to loose.
	t.Setenv("ATLAS_GRAMMAR_MODE", "experimental-future-thing")
	rf := buildResponseFormat()
	m, ok := rf.(map[string]interface{})
	if !ok {
		t.Fatalf("unknown mode should still produce strict payload, "+
			"got %T (would silently lose the schema)", rf)
	}
	if _, has := m["schema"]; !has {
		t.Error("unknown mode must default to strict (schema included)")
	}
}

func TestNormalAgentPromptOmitsRemovedTools(t *testing.T) {
	ctx := &AgentContext{Messages: []AgentMessage{{Role: "user", Content: "build the project"}}}
	if prompt := buildSystemPrompt(ctx); strings.Contains(prompt, "plan_tasks") {
		t.Fatal("prompt still mentions the removed plan_tasks tool")
	}
	_, grammar := buildStepRequest(ctx)
	if grammar != "" {
		t.Fatalf("normal agent unexpectedly received override grammar: %q", grammar)
	}
}

// TestSchemaConstrained_ReachesLlamaServerOverTheWire is the
// integration-shaped end of #33: it spins up a fake llama-server with
// httptest, captures the actual JSON the proxy POSTs, and verifies the
// schema field made it through. The unit tests above pin the helper's
// output; this one proves the helper's output actually flows into the
// callLLMOnceWithGrammar request body without getting dropped, renamed,
// or shadowed by a later assignment.
//
// Without this test a future refactor could accidentally route
// callLLMOnceWithGrammar through a different request-construction
// path that ignores buildResponseFormat() and silently regress to
// loose JSON. The user-visible symptom would be "Lens / ASA stay
// happy but token throughput slowly tanks" — exactly the class of
// regression that's hardest to spot without an explicit guard.
func TestSchemaConstrained_ReachesLlamaServerOverTheWire(t *testing.T) {
	t.Setenv("ATLAS_GRAMMAR_MODE", "strict")

	var (
		mu          sync.Mutex
		capturedReq map[string]interface{}
	)

	// Fake llama-server: capture the inbound request body, then return
	// the minimal SSE stream the proxy's streaming reader needs to
	// complete without error. We only need to reach the request-write
	// step; the response can be a no-op DONE.
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]interface{}
			_ = json.Unmarshal(body, &parsed)
			mu.Lock()
			capturedReq = parsed
			mu.Unlock()
			// Stream a minimal valid response so callLLMOnceWithGrammar
			// returns without an error path we'd need to handle.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			io.WriteString(w, `data: {"choices":[{"delta":{"content":"{\"type\":\"done\",\"summary\":\"ok\"}"},"finish_reason":"stop"}],"usage":{"total_tokens":1}}`+"\n\n")
			if fl != nil {
				fl.Flush()
			}
			io.WriteString(w, "data: [DONE]\n\n")
			if fl != nil {
				fl.Flush()
			}
		}))
	defer srv.Close()

	ctx := &AgentContext{
		InferenceURL: srv.URL,
		Ctx:          context.Background(),
		Messages: []AgentMessage{
			{Role: "user", Content: "hi"},
		},
	}

	// Fire the LLM call. We ignore the returned content — we only care
	// that the request body the proxy POSTed to our fake llama-server
	// includes the schema field.
	_, _, err := callLLMOnceWithGrammar(ctx, ctx.Messages, 0.3, "")
	if err != nil {
		// Streaming reader might still error on the minimal payload;
		// that's fine as long as the request was actually sent.
		t.Logf("callLLMOnceWithGrammar returned err (expected on minimal fake): %v", err)
	}

	mu.Lock()
	got := capturedReq
	mu.Unlock()

	if got == nil {
		t.Fatal("fake llama-server never received a request — proxy did " +
			"not POST anything (test infrastructure broken)")
	}

	rf, ok := got["response_format"].(map[string]interface{})
	if !ok {
		t.Fatalf("response_format missing or wrong type in request body, "+
			"got: %v", got["response_format"])
	}
	if rf["type"] != "json_object" {
		t.Errorf("response_format.type = %v, want json_object", rf["type"])
	}
	if _, hasSchema := rf["schema"]; !hasSchema {
		t.Errorf("response_format on the wire MISSING schema field — "+
			"the #33 optimization regressed to loose JSON. "+
			"request body: %v", got)
	}
	kwargs, ok := got["chat_template_kwargs"].(map[string]interface{})
	if !ok || kwargs["enable_thinking"] != false {
		t.Fatalf("chat_template_kwargs.enable_thinking = %v, want false",
			got["chat_template_kwargs"])
	}
	if _, legacy := got["enable_thinking"]; legacy {
		t.Fatal("enable_thinking must be nested under chat_template_kwargs")
	}

	// Strict mode should NOT also send a `grammar` field (mixing the
	// two confuses llama-server).
	if g, hasGrammar := got["grammar"]; hasGrammar {
		t.Errorf("strict mode should not send 'grammar' field alongside "+
			"schema-constrained response_format — llama-server rejects "+
			"requests with both. got grammar=%s", asString(g))
	}
}

// asString is a tiny stringification helper for test error messages.
// Defined inside _test so it doesn't leak into production binaries.
func asString(v interface{}) string {
	b, _ := json.Marshal(v)
	return strings.TrimSpace(string(b))
}

func TestBuildResponseFormat_SchemaMatchesToolRegistry(t *testing.T) {
	// The schema embedded in the response_format must match what
	// buildToolCallSchema() produces. If the two diverge, llama-server's
	// token sampler would constrain output to a stale set of tools and
	// the agent loop would reject responses from the model.
	t.Setenv("ATLAS_GRAMMAR_MODE", "strict")
	rf := buildResponseFormat()
	m := rf.(map[string]interface{})
	embedded, ok := m["schema"].(map[string]interface{})
	if !ok {
		t.Fatalf("schema field should be map[string]interface{}, got %T",
			m["schema"])
	}
	canonical := buildToolCallSchema()
	if len(embedded) != len(canonical) {
		t.Errorf("schema field has %d top-level keys, canonical has %d "+
			"— drift between buildResponseFormat and buildToolCallSchema",
			len(embedded), len(canonical))
	}
}

func newMoveCtx(dir string) *AgentContext {
	return &AgentContext{
		WorkingDir:    dir,
		FilesRead:     map[string]string{},
		FileReadTimes: map[string]time.Time{},
		SessionWrites: map[string]bool{},
	}
}

// The reported failure: reorganizing a flask app by moving index.html into
// templates/. move_file should relocate it into the existing directory keeping
// the basename, content intact.
func TestMoveFileIntoDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>hi</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0755); err != nil {
		t.Fatal(err)
	}
	ctx := newMoveCtx(dir)
	res, err := moveFileTool().Execute(json.RawMessage(`{"source":"index.html","destination":"templates/"}`), ctx)
	if err != nil || !res.Success {
		t.Fatalf("move failed: err=%v res=%+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); !os.IsNotExist(err) {
		t.Errorf("source still exists after move")
	}
	data, err := os.ReadFile(filepath.Join(dir, "templates", "index.html"))
	if err != nil || string(data) != "<h1>hi</h1>" {
		t.Errorf("destination missing or content changed: %q err=%v", string(data), err)
	}
}

// Plain rename within the same directory.
func TestMoveFileRename(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.py"), []byte("print(1)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := newMoveCtx(dir)
	res, err := moveFileTool().Execute(json.RawMessage(`{"source":"old.py","destination":"new.py"}`), ctx)
	if err != nil || !res.Success {
		t.Fatalf("rename failed: err=%v res=%+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.py")); err != nil {
		t.Errorf("renamed file missing: %v", err)
	}
}

// A move must never silently clobber an existing destination file.
func TestMoveFileRefusesClobber(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("B"), 0644)
	ctx := newMoveCtx(dir)
	res, err := moveFileTool().Execute(json.RawMessage(`{"source":"a.txt","destination":"b.txt"}`), ctx)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if res.Success {
		t.Errorf("expected refusal to clobber existing destination")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(data) != "B" {
		t.Errorf("destination was overwritten: %q", string(data))
	}
}

// Missing source is a clean tool error, not a crash.
func TestMoveFileSourceMissing(t *testing.T) {
	dir := t.TempDir()
	ctx := newMoveCtx(dir)
	res, err := moveFileTool().Execute(json.RawMessage(`{"source":"nope.py","destination":"x.py"}`), ctx)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if res.Success {
		t.Errorf("expected failure for missing source")
	}
}

// storedReadMessage mimics how the agent loop records a read_file result in the
// conversation: ToolResult.Data = marshaled ReadFileOutput, then the message
// content = ToolResult.MarshalText() (json.Marshal of the whole result). This
// is where the file content gets JSON-escaped.
func storedReadMessage(t *testing.T, fileContent string) AgentMessage {
	t.Helper()
	// read_file numbers lines; the numbering doesn't affect the probe, but
	// model the real shape anyway.
	var numbered strings.Builder
	for i, l := range strings.Split(fileContent, "\n") {
		numbered.WriteString(strings.TrimRight(
			strings.Join([]string{itoaTest(i + 1), l}, "\t"), "\n"))
		numbered.WriteString("\n")
	}
	out := ReadFileOutput{Content: numbered.String()}
	data, _ := json.Marshal(out)
	res := &ToolResult{Success: true, Data: data}
	return AgentMessage{Role: "tool", ToolName: "read_file", Content: res.MarshalText()}
}

func itoaTest(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Regression: a flask app whose longest line is embedded HTML/JS full of double
// quotes must still be detected as present in context. The old longest-raw-line
// probe failed here (the `"` in the line became `\"` in the stored JSON), so
// the dedup re-served the file every read and the model looped on read_file.
func TestFileContentInContextSurvivesJSONEscaping(t *testing.T) {
	fileContent := `from flask import Flask, render_template_string
app = Flask(__name__)
HTML = "<div class=\"board\" style=\"width:400px;height:400px\" id=\"game-board-container\"></div>"

@app.route("/")
def index():
    return render_template_string(HTML)
`
	ctx := &AgentContext{Messages: []AgentMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "make a snake game"},
		storedReadMessage(t, fileContent),
	}}
	if !fileContentInContext(ctx, fileContent) {
		t.Errorf("content with quoted HTML line reported as NOT in context — false-negative would make dedup re-serve and loop")
	}
}

// When the content really is gone (trimmed), it must report absent so the
// dedup re-serves rather than lying that "it's above."
func TestFileContentInContextDetectsAbsence(t *testing.T) {
	fileContent := "def compute_subtotal(rows):\n    return sum(r.price for r in rows)\n"
	ctx := &AgentContext{Messages: []AgentMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "unrelated chatter with no file content"},
	}}
	if fileContentInContext(ctx, fileContent) {
		t.Errorf("expected absent verdict when content is not in any message")
	}
}

// fakeSandboxShell is a tiny stand-in for sandbox /shell.
// Echoes back canned output so we can assert routing, request shape,
// and response decoding without bringing up the real sandbox.
func fakeSandboxShell(t *testing.T, status int, resp interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/shell" {
			http.NotFound(w, r)
			return
		}
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestRunViaSandboxDecodesSuccessfulResponse(t *testing.T) {
	srv := fakeSandboxShell(t, 200, map[string]interface{}{
		"success":    true,
		"stdout":     "hello world\n",
		"stderr":     "",
		"exit_code":  0,
		"elapsed_ms": 42,
	})
	defer srv.Close()

	ctx := &AgentContext{SandboxURL: srv.URL}
	out, err := runViaSandbox(ctx, `echo "hello world"`, "/workspace", 5)
	if err != nil {
		t.Fatalf("runViaSandbox: %v", err)
	}
	if out.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", out.ExitCode)
	}
	if !strings.Contains(out.Stdout, "hello world") {
		t.Errorf("stdout = %q, want to contain 'hello world'", out.Stdout)
	}
}

func TestRunViaSandboxSurfacesNonZeroExit(t *testing.T) {
	srv := fakeSandboxShell(t, 200, map[string]interface{}{
		"success":   false,
		"stdout":    "",
		"stderr":    "ImportError: No module named flask",
		"exit_code": 1,
	})
	defer srv.Close()

	ctx := &AgentContext{SandboxURL: srv.URL}
	out, err := runViaSandbox(ctx, "python -c 'import flask'", "/workspace", 5)
	if err != nil {
		t.Fatalf("runViaSandbox: %v", err)
	}
	if out.ExitCode != 1 {
		t.Errorf("exit_code = %d, want 1", out.ExitCode)
	}
	if !strings.Contains(out.Stderr, "ImportError") {
		t.Errorf("stderr lost: %q", out.Stderr)
	}
}

func TestRunViaSandbox4xxIsValidationFailure(t *testing.T) {
	// A 4xx from the sandbox means the request was bad (e.g. cwd
	// outside /workspace). Should NOT propagate as "sandbox
	// unreachable" — that would trigger the local-exec fallback
	// and let the model bypass the cwd guard. Instead we surface
	// the FastAPI detail on stderr with exit_code=1.
	srv := fakeSandboxShell(t, 400, map[string]string{
		"detail": "cwd must be under /workspace, got /etc",
	})
	defer srv.Close()

	ctx := &AgentContext{SandboxURL: srv.URL}
	out, err := runViaSandbox(ctx, "ls", "/etc", 5)
	if err != nil {
		t.Fatalf("4xx should NOT return Go error (fallback would trip): %v", err)
	}
	if out.ExitCode != 1 {
		t.Errorf("exit_code = %d, want 1 for 4xx", out.ExitCode)
	}
	if !strings.Contains(out.Stderr, "must be under /workspace") {
		t.Errorf("stderr should carry the FastAPI detail, got %q", out.Stderr)
	}
}

func TestRunViaSandboxUnreachableReturnsError(t *testing.T) {
	// Sandbox URL that won't accept connections. The caller must surface
	// this error and must not change the execution target implicitly.
	ctx := &AgentContext{SandboxURL: "http://127.0.0.1:1"}
	_, err := runViaSandbox(ctx, "echo hi", "/workspace", 5)
	if err == nil {
		t.Error("expected network error for unreachable sandbox")
	}
}

func TestRunCommandDoesNotFallbackWhenSandboxIsUnavailable(t *testing.T) {
	ctx := &AgentContext{
		WorkingDir: "/workspace",
		SandboxURL: "http://127.0.0.1:1",
	}
	res, err := runCommandTool().Execute(
		json.RawMessage(`{"command":"printf fallback-ran"}`), ctx,
	)
	if err != nil {
		t.Fatalf("run_command: %v", err)
	}
	if res.Success {
		t.Fatal("run_command succeeded even though the sandbox was unavailable")
	}
	if strings.Contains(string(res.Data), "fallback-ran") {
		t.Fatalf("command appears to have run locally: %s", res.Data)
	}
	if !strings.Contains(res.Error, "sandbox unavailable") {
		t.Fatalf("error = %q, want sandbox unavailable", res.Error)
	}
}

func TestRunLocallyEcho(t *testing.T) {
	// runLocally is the explicit ATLAS_VERIFY_IN=host execution primitive.
	// Quick sanity: a trivial echo command returns a populated result.
	out := runLocally("echo hello", ".", 0)
	// timeout=0 still has an internal default — go's select with
	// time.After(0) fires immediately, so we accept either the
	// successful path or the timeout path. The important property
	// is "doesn't panic and returns a populated struct."
	if out.ExitCode != 0 && out.ExitCode != 124 {
		t.Errorf("unexpected exit_code %d for runLocally(echo)", out.ExitCode)
	}
}

// Credential-file read exclusion (P0: sensitive values must not flow
// into model context by default). All paths are synthetic fixtures.

func TestDenyReadPathReason(t *testing.T) {
	os.Unsetenv("ATLAS_ALLOW_CREDENTIAL_READS")

	blocked := []string{
		".env",
		"subdir/.env",
		".env.production",
		".netrc",
		".npmrc",
		".pypirc",
		"certs/server.pem",
		"secrets/signing.key",
		".ssh/id_rsa",
		"id_ed25519",
		"/home/user/.ssh/id_ecdsa",
		".aws/credentials",
		".aws/config",
		".kube/config",
		".docker/config.json",
		"secrets/service-token",
		"secrets/api-keys.json",
		"gcp-credentials.json",
	}
	for _, p := range blocked {
		if reason := denyReadPathReason(p); reason == "" {
			t.Errorf("read of %q should be blocked", p)
		}
	}

	allowed := []string{
		".env.example", // template, documented
		"main.go",
		"config.yaml",
		"src/environment.ts", // unrelated name
		"staging.envrc.sample",
		".ssh/id_rsa.pub",     // public half
		"docs/kube/config.md", // .kube parent match is exact-dir only
		"README.md",
	}
	for _, p := range allowed {
		if reason := denyReadPathReason(p); reason != "" {
			t.Errorf("read of %q wrongly blocked: %s", p, reason)
		}
	}
}

func TestDenyReadOverride(t *testing.T) {
	os.Setenv("ATLAS_ALLOW_CREDENTIAL_READS", "1")
	defer os.Unsetenv("ATLAS_ALLOW_CREDENTIAL_READS")
	if reason := denyReadPathReason(".env"); reason != "" {
		t.Fatalf("override not honored: %s", reason)
	}
}

func TestShouldDenyToolCallReadFile(t *testing.T) {
	os.Unsetenv("ATLAS_ALLOW_CREDENTIAL_READS")
	args, _ := json.Marshal(map[string]string{"path": ".netrc"})
	denied, reason := shouldDenyToolCall("read_file", args)
	if !denied {
		t.Fatal("read_file .netrc not denied")
	}
	if reason == "" || !containsStr(reason, "ATLAS_ALLOW_CREDENTIAL_READS") {
		t.Fatalf("refusal must name the documented override: %q", reason)
	}

	denied, _ = shouldDenyToolCall("outline_file", args)
	if !denied {
		t.Fatal("outline_file .netrc not denied")
	}

	ok, _ := json.Marshal(map[string]string{"path": "main.py"})
	denied, reason = shouldDenyToolCall("read_file", ok)
	if denied {
		t.Fatalf("normal read denied: %s", reason)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The call-graph footer is concatenated onto the file content read_file
// returns, so without an explicit boundary it reads as the file's last lines.
// A live session anchored edit_file's old_str on "## Call graph (within this
// file)\n- mean calls: ..." and the edit could never match — that text is not
// on disk. The footer has to say where the file ends.
func TestCallGraphFooterMarksItselfAsNotFileContent(t *testing.T) {
	v3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"supported":true,"symbols":[
			{"name":"mean","kind":"function","start_line":1,"end_line":3,"calls":["sum","len"]}]}`))
	}))
	defer v3.Close()
	ctx := &AgentContext{V3URL: v3.URL, Ctx: context.Background()}

	footer := callGraphFooter(ctx, "stats.py", "def mean(v):\n    return sum(v)/len(v)\n")
	if footer == "" {
		t.Skip("outline unavailable in this environment")
	}
	if !strings.Contains(footer, "--- end of stats.py ---") {
		t.Errorf("footer must mark where the file ends, got %q", footer)
	}
	if !strings.Contains(footer, "NOT part of the file") {
		t.Errorf("footer must disclaim being file content, got %q", footer)
	}
	if !strings.Contains(footer, "old_str") {
		t.Errorf("footer must warn against anchoring on it, got %q", footer)
	}
	// The boundary has to come before the analysis, or it marks nothing.
	if strings.Index(footer, "--- end of") > strings.Index(footer, "## Call graph") {
		t.Error("the end-of-file marker must precede the analysis section")
	}
}

// The call graph is not a switch: a whole-file read of a Python file carries
// its call edges with no flag set, and a file of another language never does.
func TestReadFileAttachesTheCallGraphWithoutAFlag(t *testing.T) {
	t.Setenv("ATLAS_CALL_GRAPH", "")
	var outlines int
	v3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outlines++
		_, _ = w.Write([]byte(`{"supported":true,"symbols":[
			{"name":"mean","kind":"function","start_line":1,"end_line":2,"calls":["total"]}]}`))
	}))
	defer v3.Close()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"stats.py": "def mean(v):\n    return total(v)/len(v)\n",
		"stats.js": "function mean(v) { return total(v) / v.length; }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.Ctx = context.Background()
	ctx.V3URL = v3.URL
	read := func(path string) string {
		t.Helper()
		res, err := readFileTool().Execute(json.RawMessage(`{"path":"`+path+`"}`), ctx)
		if err != nil || res == nil || !res.Success {
			t.Fatalf("read_file %s failed: %v %+v", path, err, res)
		}
		var out ReadFileOutput
		if err := json.Unmarshal(res.Data, &out); err != nil {
			t.Fatal(err)
		}
		return out.Content
	}
	if got := read("stats.py"); !strings.Contains(got, "## Call graph") {
		t.Errorf("a Python read carries no call graph: %q", got)
	}
	before := outlines
	if got := read("stats.js"); strings.Contains(got, "## Call graph") {
		t.Errorf("a JavaScript read carries a call graph: %q", got)
	}
	if outlines != before {
		t.Error("a JavaScript read asked for a call graph")
	}
}

// read_file numbers lines "N<tab>content" for reference, and nothing said so.
// A model reasonably concluded the file itself was tab-delimited: an
// otherwise correct grid-puzzle solution parsed every line as
// line.split('\t')[1], found no tabs in the real file, built an empty grid
// and printed 0. The payload has to disclaim its own formatting.
func TestReadFileDisclaimsItsLineNumbering(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "grid.txt"),
		[]byte("..#\n#..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.Ctx = context.Background()

	res, err := readFileTool().Execute(json.RawMessage(`{"path":"grid.txt"}`), ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("read_file failed: %v %+v", err, res)
	}
	var out ReadFileOutput
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(out.Content, "NOT in the file") {
		t.Errorf("read_file must disclaim its line numbering, got %q", out.Content)
	}
	// The disclaimer must precede the numbered body, or it explains nothing.
	if strings.Index(out.Content, "NOT in the file") > strings.Index(out.Content, "1\t") {
		t.Error("the disclaimer must come before the numbered lines")
	}
	// And the actual content still has to be there, numbered as before.
	if !strings.Contains(out.Content, "1\t..#") {
		t.Errorf("numbered content missing: %q", out.Content)
	}
}

// insert_after exists because both other edit primitives make the model
// reproduce text verbatim — edit_file an anchor, structural_edit a whole node
// — and that is the step that measurably fails ("safe_load_aller" for
// "safe_load_all"). A line number is something read_file already showed it.
func TestInsertAfterPlacesLinesAtTheNamedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.py")
	orig := "def a():\n    return 1\n\ndef c():\n    return 3\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.Ctx = context.Background()
	ctx.RecordFileRead(path, orig)

	args, _ := json.Marshal(InsertAfterInput{
		Path: "m.py", Line: 2, Content: "\ndef b():\n    return 2\n"})
	res, err := insertAfterTool().Execute(json.RawMessage(args), ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("insert_after failed: %v %+v", err, res)
	}
	got, _ := os.ReadFile(path)
	want := "def a():\n    return 1\n\ndef b():\n    return 2\n\ndef c():\n    return 3\n"
	if string(got) != want {
		t.Errorf("wrong result:\n got %q\nwant %q", got, want)
	}
}

func TestInsertAfterZeroInsertsAtTheTop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.py")
	if err := os.WriteFile(path, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.Ctx = context.Background()
	ctx.RecordFileRead(path, "x = 1\n")

	args, _ := json.Marshal(InsertAfterInput{Path: "m.py", Line: 0, Content: "import os\n"})
	if _, err := insertAfterTool().Execute(json.RawMessage(args), ctx); err != nil {
		t.Fatalf("insert_after: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "import os\nx = 1\n" {
		t.Errorf("line 0 must insert at the top, got %q", got)
	}
}

// The same gates as every other write. An insert that breaks a healthy file
// must be refused, and nothing may land on disk.
func TestInsertAfterIsSyntaxGated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.py")
	orig := "x = 1\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	sb := fakeSyntaxSandbox(t, "BROKEN")
	defer sb.Close()
	ctx := writeGateCtx(t, "", sb.URL, dir)
	ctx.RecordFileRead(path, orig)

	args, _ := json.Marshal(InsertAfterInput{Path: "m.py", Line: 1, Content: "BROKEN(\n"})
	res, _ := insertAfterTool().Execute(json.RawMessage(args), ctx)
	if res == nil || res.Success {
		t.Fatalf("an insert that breaks the file must be refused, got %+v", res)
	}
	after, _ := os.ReadFile(path)
	if string(after) != orig {
		t.Errorf("file must be untouched after a refusal, got %q", after)
	}
}

func TestInsertAfterRejectsOutOfRangeAndUnreadFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.py")
	if err := os.WriteFile(path, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.Ctx = context.Background()

	// Not read yet — same rule edit_file and structural_edit enforce.
	args, _ := json.Marshal(InsertAfterInput{Path: "m.py", Line: 1, Content: "y = 2\n"})
	if _, err := insertAfterTool().Execute(json.RawMessage(args), ctx); err == nil {
		t.Error("must require read_file first")
	}

	ctx.RecordFileRead(path, "x = 1\n")
	args2, _ := json.Marshal(InsertAfterInput{Path: "m.py", Line: 99, Content: "y = 2\n"})
	res, _ := insertAfterTool().Execute(json.RawMessage(args2), ctx)
	if res == nil || res.Success {
		t.Error("an out-of-range line must be refused")
	} else if !strings.Contains(res.Error, "out of range") {
		t.Errorf("error should name the problem, got %q", res.Error)
	}
}

// The live failure: the same edit_file ran twice — old_str
// "msgElement.innerText = 'PAUSED';", new_str " msgElement.innerText =
// 'PAUSED';" — at 1m25s and 1m15s, each adding another leading space. The
// existing no-op guard only catches an edit that leaves the file identical,
// and the repeat detector needs three occurrences, so two runs of a
// 75-second edit slipped through as four minutes of wasted turn.
func TestAppliedEditsAreTrackedPerSession(t *testing.T) {
	ctx := NewAgentContext(t.TempDir(), Tier1Simple)
	if ctx.AppliedEdits == nil {
		t.Fatal("AppliedEdits must be initialised, or the guard silently never fires")
	}

	key := func(path, oldStr, newStr string) string {
		return path + "\x00" + oldStr + "\x00" + newStr
	}
	k := key("app.py", "x = 1", "x = 2")
	if ctx.AppliedEdits[k] {
		t.Error("a fresh context must not report an edit as already applied")
	}
	ctx.AppliedEdits[k] = true
	if !ctx.AppliedEdits[k] {
		t.Error("recording an applied edit must be observable")
	}

	// A different new_str for the same anchor is a DIFFERENT edit — the guard
	// must not block a model that is correcting its previous change.
	if ctx.AppliedEdits[key("app.py", "x = 1", "x = 3")] {
		t.Error("a different new_str must not be treated as already applied")
	}
	// Same edit text, different file is also different.
	if ctx.AppliedEdits[key("other.py", "x = 1", "x = 2")] {
		t.Error("the same edit in another file must not be blocked")
	}
	// The whitespace case that motivated this: the shapes must not collide.
	if key("a.py", "y", " y") == key("a.py", " y", "y") {
		t.Error("old_str/new_str must not be able to swap without changing the key")
	}
}

// The live failure: the model sent
//
//	{"path":"app.py","old_str":"","":"new_str","content":"# hello world\n"}
//
// and got "file not read yet" — true, and silent about the empty old_str, the
// key literally named "", and `content` being write_file's field. Its next
// attempt fumbled a different field (insert_after with no path).
func TestMalformedEditArgsNameTheShape(t *testing.T) {
	tool := findTool(t, "edit_file")
	ctx := NewAgentContext(t.TempDir(), Tier1Simple)

	res, err := tool.Execute(json.RawMessage(
		`{"path":"app.py","old_str":"","":"new_str","content":"# hello world\n"}`), ctx)
	if err != nil {
		t.Fatalf("expected a result, got error: %v", err)
	}
	if res.Success {
		t.Fatal("an empty old_str must not be accepted")
	}
	for _, want := range []string{"old_str is required", "write_file's field", "insert_after"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("error missing %q:\n%s", want, res.Error)
		}
	}
}

func TestEmptyOldStrWithoutContentSkipsTheContentHint(t *testing.T) {
	// Don't accuse the model of a mistake it didn't make.
	tool := findTool(t, "edit_file")
	ctx := NewAgentContext(t.TempDir(), Tier1Simple)
	res, _ := tool.Execute(json.RawMessage(`{"path":"a.py","old_str":"","new_str":"x"}`), ctx)
	if res.Success {
		t.Fatal("empty old_str must be rejected")
	}
	if strings.Contains(res.Error, "write_file's field") {
		t.Errorf("content hint fired without a content field:\n%s", res.Error)
	}
}

func TestInsertAfterMissingPathNamesTheWholeCall(t *testing.T) {
	tool := findTool(t, "insert_after")
	ctx := NewAgentContext(t.TempDir(), Tier1Simple)
	res, _ := tool.Execute(json.RawMessage(`{"line":0,"content":"# hello world\n"}`), ctx)
	if res.Success {
		t.Fatal("missing path must be rejected")
	}
	for _, want := range []string{"path", "line", "content", "0 for top of file"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("error missing %q:\n%s", want, res.Error)
		}
	}
}

// findTool returns a registered tool by name, failing the test if absent.
func findTool(t *testing.T, name string) *ToolDef {
	t.Helper()
	for _, tool := range allTools() {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q is not registered", name)
	return nil
}

// replace_lines exists because this model cannot reproduce a 9-13 line anchor
// verbatim — observed corruptions food.y -> hood.y, scoreElement ->
// scorerElement, unshift(( . Citing two line numbers removes that burden. The
// content assertion is what keeps the address honest: a wrong anchor simply
// fails to match, but a wrong line number splices cleanly and corrupts.
func TestReplaceLinesReplacesByNumber(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.py")
	src := "one\ntwo\nthree\nfour\nfive\n"
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.RecordFileRead(file, src)
	tool := findTool(t, "replace_lines")

	res, err := tool.Execute(json.RawMessage(`{"path":"a.py","start_line":2,"end_line":4,
		"expected_first_line":"two","expected_last_line":"four","content":"TWO\nTHREE"}`), ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Error)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "one\nTWO\nTHREE\nfive\n" {
		t.Errorf("wrong result:\n%q", string(got))
	}
}

func TestReplaceLinesRefusesAWrongRange(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.py")
	src := "one\ntwo\nthree\nfour\nfive\n"
	os.WriteFile(file, []byte(src), 0644)
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.RecordFileRead(file, src)
	tool := findTool(t, "replace_lines")

	// Off by one: claims line 2 is "one". Without the assertion this applies
	// cleanly and silently corrupts.
	res, _ := tool.Execute(json.RawMessage(`{"path":"a.py","start_line":2,"end_line":3,
		"expected_first_line":"one","expected_last_line":"three","content":"X"}`), ctx)
	if res.Success {
		t.Fatal("an off-by-one range must be refused, not applied")
	}
	for _, want := range []string{"line 2", "you said", "actually"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("error missing %q:\n%s", want, res.Error)
		}
	}
	if got, _ := os.ReadFile(file); string(got) != src {
		t.Error("file was modified despite the refusal")
	}
}

func TestReplaceLinesIgnoresIndentationInTheAssertion(t *testing.T) {
	// The model reproduces the TEXT of one line reliably and its indentation
	// unreliably. The assertion exists to catch a wrong NUMBER, not whitespace.
	dir := t.TempDir()
	file := filepath.Join(dir, "a.py")
	src := "def f():\n    return 1\n"
	os.WriteFile(file, []byte(src), 0644)
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.RecordFileRead(file, src)

	res, _ := findTool(t, "replace_lines").Execute(json.RawMessage(
		`{"path":"a.py","start_line":2,"end_line":2,"expected_first_line":"return 1",
		  "expected_last_line":"return 1","content":"    return 2"}`), ctx)
	if !res.Success {
		t.Fatalf("indentation-only difference must not fail the assertion: %s", res.Error)
	}
}

func TestReplaceLinesGuardsRangeAndSize(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.py")
	src := strings.Repeat("x\n", 100)
	os.WriteFile(file, []byte(src), 0644)
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.RecordFileRead(file, src)
	tool := findTool(t, "replace_lines")

	for _, tc := range []struct{ name, args, want string }{
		{"past end of file", `{"path":"a.py","start_line":99,"end_line":140,"expected_first_line":"x","expected_last_line":"x","content":"y"}`, "invalid"},
		{"inverted range", `{"path":"a.py","start_line":9,"end_line":2,"expected_first_line":"x","expected_last_line":"x","content":"y"}`, "invalid"},
		{"zero start", `{"path":"a.py","start_line":0,"end_line":2,"expected_first_line":"x","expected_last_line":"x","content":"y"}`, "invalid"},
		// A whole JavaScript function inside a template runs 40-50 lines and
		// has to fit; the cap is there for a rewrite pretending to be an edit.
		{"span too large", `{"path":"a.py","start_line":1,"end_line":80,"expected_first_line":"x","expected_last_line":"x","content":"y"}`, "too large"},
		{"missing path", `{"start_line":1,"end_line":2,"content":"y"}`, "path is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tool.Execute(json.RawMessage(tc.args), ctx)
			if err != nil {
				return // read-staleness path returns an error, also a refusal
			}
			if res.Success {
				t.Fatalf("expected refusal, got success")
			}
			if !strings.Contains(res.Error, tc.want) {
				t.Errorf("error missing %q:\n%s", tc.want, res.Error)
			}
		})
	}
}

// The tool the model most needs to reach for got the only empty example.
// Commit 7f931a4's own message records it reaching for edit_file on a pure-ADD
// task because nothing pointed at insert_after.
func TestLineAddressedToolsHaveWorkedExamples(t *testing.T) {
	for _, name := range []string{"insert_after", "replace_lines"} {
		ex := generateInputExample(name)
		if ex == "{}" || ex == "" {
			t.Errorf("%s renders an empty example in the system prompt", name)
			continue
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(ex), &parsed); err != nil {
			t.Errorf("%s example is not valid JSON: %v\n%s", name, err, ex)
			continue
		}
		if _, ok := parsed["path"]; !ok {
			t.Errorf("%s example omits path, which is the field it was seen forgetting", name)
		}
	}
	// replace_lines' example must demonstrate the assertion, or the model
	// learns the address and skips the safety belt.
	var rl map[string]interface{}
	json.Unmarshal([]byte(generateInputExample("replace_lines")), &rl)
	for _, k := range []string{"start_line", "end_line", "expected_first_line", "expected_last_line"} {
		if _, ok := rl[k]; !ok {
			t.Errorf("replace_lines example omits %q", k)
		}
	}
}

// A failing insert_after/replace_lines loop must be able to trip the 3-strike
// path-aware breaker. It could not: extractFailurePath returned "" for both, so
// three identical failures read as three different paths.
func TestFailurePathIsExtractedForLineAddressedTools(t *testing.T) {
	for _, name := range []string{"insert_after", "replace_lines"} {
		got := extractFailurePath(name, json.RawMessage(`{"path":"app.py","line":3}`))
		if got != "app.py" {
			t.Errorf("extractFailurePath(%s) = %q, want \"app.py\" — the breaker needs a non-empty path", name, got)
		}
	}
}

// The steer a model actually reads when a long anchor misses. It used to
// name only insert_after, which handles ADDING — so a model changing a
// multi-line region was told "anchor on ONE short line" with no pointer to
// the tool built for exactly that case. Observed live: two failed 15-line
// old_str attempts in a row, then the model abandoned the edit.
func TestLongAnchorFailureNamesTheLineAddressedTool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.py")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\ne\nf\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.RecordFileRead(path, "a\nb\nc\nd\ne\nf\n")

	tool := findTool(t, "edit_file")
	_, err := tool.Execute(json.RawMessage(
		`{"path":"app.py","old_str":"nope1\nnope2\nnope3\nnope4\nnope5\nnope6","new_str":"x"}`), ctx)
	if err == nil {
		t.Fatal("a six-line anchor that matches nothing must fail")
	}
	if !strings.Contains(err.Error(), "replace_lines") {
		t.Errorf("steer never names replace_lines:\n%s", err)
	}
}

// Guidance the model reads before it picks anything. Every tool that edits
// without an old_str has to be reachable from it, or the model falls back to
// the two tools the prose repeats.
func TestToolGuidanceNamesEveryOldStrFreeEditTool(t *testing.T) {
	guidance := buildSystemPrompt(NewAgentContext(t.TempDir(), Tier2Medium))
	for _, name := range []string{"replace_lines", "insert_after", "structural_edit"} {
		if !strings.Contains(guidance, name) {
			t.Errorf("system prompt never mentions %s — the model cannot choose it", name)
		}
	}
}

// A question about the code is answered from the file, not from a guess. The
// bullet saying so was once lumped in with greetings, and questions about code
// were answered without opening anything; when it was deleted, only the
// whole-prompt hash noticed.
func TestSystemPromptSaysToReadBeforeAnsweringCodeQuestions(t *testing.T) {
	prompt := buildSystemPrompt(NewAgentContext(t.TempDir(), Tier2Medium))
	for _, want := range []string{
		"Questions about the CODE",
		"read the file first, then answer",
		"never answer from a guess about code you have not opened",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt lost the code-question guidance: missing %q", want)
		}
	}
}

// The over-limit refusal used to say "use structural_edit with function:NAME".
// For a JavaScript function inside a Flask template that is a dead end — the
// markup is one string literal to the Python grammar, so no selector reaches
// it, and structural_edit's own refusal points back here. An observed session
// burned all three strikes bouncing between the two with the file untouched.
func TestOverLimitAdviceDoesNotDeadEndOnEmbeddedCode(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.py")
	src := strings.Repeat("x\n", 200)
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.RecordFileRead(file, src)

	res, err := findTool(t, "replace_lines").Execute(json.RawMessage(
		`{"path":"a.py","start_line":1,"end_line":150,"expected_first_line":"x","expected_last_line":"x","content":"y"}`), ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success {
		t.Fatal("a 150-line span must still be refused")
	}
	for _, want := range []string{
		"consecutive replace_lines", // the way out that always works
		"BOTTOM of the file upward", // why the split doesn't invalidate itself
		"no selector reaches into it",
	} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("advice missing %q:\n%s", want, res.Error)
		}
	}
}

// A whole JS function inside a template is the unit of work that kept hitting
// the old cap of 20.
func TestAWholeEmbeddedFunctionFitsInOneCall(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.py")
	src := strings.Repeat("x\n", 100)
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier1Simple)
	ctx.RecordFileRead(file, src)

	res, err := findTool(t, "replace_lines").Execute(json.RawMessage(
		`{"path":"a.py","start_line":25,"end_line":70,"expected_first_line":"x","expected_last_line":"x","content":"y"}`), ctx)
	if err != nil {
		t.Fatalf("a 46-line replacement must be allowed: %v", err)
	}
	if !res.Success {
		t.Fatalf("a 46-line replacement must be allowed: %s", res.Error)
	}
}

// read_file prints "12<tab>" before each line and the model pastes back what
// it was shown. The rejection for this names the mistake and the alternative
// tool exactly, and was still ignored: watched on executor_server.py
// (2026-08-03) the model got it on turn 2 and re-sent the same prefixed block
// on turn 6. The prefix is this harness's own addition, so it comes off here.
func TestStripLineNumberPrefixes(t *testing.T) {
	t.Run("strips read_file's prefix", func(t *testing.T) {
		got := stripLineNumberPrefixes("12\tconst ctx = canvas.getContext('2d');\n13\treturn ctx;")
		want := "const ctx = canvas.getContext('2d');\nreturn ctx;"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("leaves unprefixed text alone", func(t *testing.T) {
		src := "const ctx = canvas.getContext('2d');"
		if got := stripLineNumberPrefixes(src); got != src {
			t.Errorf("got %q, want it unchanged", got)
		}
	})

	t.Run("a number that is not a prefix survives", func(t *testing.T) {
		// Tab-separated data, not read_file output: the digits are content.
		src := "value\t42\n"
		if got := stripLineNumberPrefixes(src); got != src {
			t.Errorf("got %q, want it unchanged", got)
		}
	})
}

func TestAllLinesLineNumbered(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"wholesale paste of read_file output", "1\tdef f():\n2\t    return 1\n", true},
		{"blank lines do not disqualify", "1\tdef f():\n\n3\t    return 1\n", true},
		{"a block that merely contains one numbered line", "def f():\n2\t    return 1\n", false},
		{"ordinary replacement text", "def f():\n    return 1\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allLinesLineNumbered(tc.src); got != tc.want {
				t.Errorf("allLinesLineNumbered(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

// The strip is only safe because it is self-validating: the stripped form has
// to match the file. A stripped old_str that matches nothing must fall through
// to the existing rejection rather than edit somewhere else.
func TestStrippedOldStrOnlyCountsWhenItMatchesTheFile(t *testing.T) {
	file := "func main() {\n\tx := 1\n}\n"

	if got := findActualString(file, stripLineNumberPrefixes("2\t\tx := 1")); got == "" {
		t.Error("a prefixed copy of a real line should match once stripped")
	}
	if got := findActualString(file, stripLineNumberPrefixes("2\t\ty := 2")); got != "" {
		t.Errorf("a stripped line absent from the file matched %q", got)
	}
}

// A replace_lines range goes stale the moment an earlier edit changes the
// file's length, and the model re-sends the same numbers because nothing
// looks wrong from its side. Measured across 168 sessions: 23 hit an anchor
// refusal and 11 of those lost the task, usually to a re-send loop after
// "line N is not what you expected".
//
// The assertions the call already carries say where the block went. This is
// grok-build's "shifted" anchor case using text ATLAS already receives,
// rather than hashing every line of read_file output.
func TestRelocateStaleRange(t *testing.T) {
	file := []string{
		"import sys",       // 1
		"",                 // 2
		"def helper():",    // 3
		"    return 1",     // 4
		"",                 // 5
		"def main():",      // 6
		"    x = helper()", // 7
		"    return x",     // 8
	}
	limit := len(file)

	t.Run("a block that moved is found", func(t *testing.T) {
		// Model thinks main() is at 3-5; two lines were inserted above it.
		got := relocateStaleRange(file, limit, "def main():", "    return x", 3)
		if got != 6 {
			t.Errorf("relocated to %d, want 6", got)
		}
	})

	t.Run("an ambiguous anchor is refused", func(t *testing.T) {
		dup := append([]string{}, file...)
		dup = append(dup, "def main():", "    x = helper()", "    return x")
		if got := relocateStaleRange(dup, len(dup), "def main():", "    return x", 3); got != 0 {
			t.Errorf("relocated an ambiguous block to %d — a wrong-place edit is "+
				"worse than a refusal", got)
		}
	})

	t.Run("a block that changed shape is refused", func(t *testing.T) {
		// Both anchors present but 3 lines apart, not the 5 claimed.
		if got := relocateStaleRange(file, limit, "def main():", "    return x", 5); got != 0 {
			t.Errorf("relocated a reshaped block to %d", got)
		}
	})

	t.Run("a missing anchor is refused", func(t *testing.T) {
		if got := relocateStaleRange(file, limit, "def gone():", "    return x", 3); got != 0 {
			t.Errorf("relocated on a missing anchor to %d", got)
		}
	})

	t.Run("empty assertions do nothing", func(t *testing.T) {
		if got := relocateStaleRange(file, limit, "", "    return x", 3); got != 0 {
			t.Errorf("relocated without a first-line assertion to %d", got)
		}
	})

	t.Run("indentation drift does not block the match", func(t *testing.T) {
		// The assertion is whitespace-insensitive elsewhere for the same
		// reason: the model reproduces text reliably and indentation badly.
		got := relocateStaleRange(file, limit, "  def main():", "return x", 3)
		if got != 6 {
			t.Errorf("relocated to %d, want 6", got)
		}
	})
}

// A background job's outcome is invisible unless the model calls
// tail_background, so a server that died on startup reads the same as one
// serving happily: the run continues, the next probe fails for a reason
// nothing explains, and a session can finish claiming work it never
// verified. The foreground-server redirect pushes more work down this path.
func TestFinishedBackgroundNote(t *testing.T) {
	newCtx := func(jobs map[string]string, body string) (*AgentContext, *int) {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return &AgentContext{SandboxURL: srv.URL, BackgroundJobs: jobs}, &hits
	}

	t.Run("an exited job is reported once and then forgotten", func(t *testing.T) {
		ctx, _ := newCtx(map[string]string{"job-1": "python app.py"},
			`{"jobs":[{"job_id":"job-1","command":"python app.py","running":false}]}`)
		got := finishedBackgroundNote(ctx)
		if !strings.Contains(got, "job-1") || !strings.Contains(got, "tail_background") {
			t.Fatalf("expected a note naming the job and how to read it:\n%s", got)
		}
		if _, still := ctx.BackgroundJobs["job-1"]; still {
			t.Error("a reported job must stop being tracked")
		}
		if second := finishedBackgroundNote(ctx); second != "" {
			t.Errorf("reported twice:\n%s", second)
		}
	})

	t.Run("a running job is not reported", func(t *testing.T) {
		ctx, _ := newCtx(map[string]string{"job-1": "python app.py"},
			`{"jobs":[{"job_id":"job-1","command":"python app.py","running":true}]}`)
		if got := finishedBackgroundNote(ctx); got != "" {
			t.Errorf("a live job was reported as finished:\n%s", got)
		}
		if _, still := ctx.BackgroundJobs["job-1"]; !still {
			t.Error("a live job must stay tracked")
		}
	})

	t.Run("another session's jobs are not this run's news", func(t *testing.T) {
		// The sandbox registry is process-wide and outlives sessions.
		ctx, _ := newCtx(map[string]string{},
			`{"jobs":[{"job_id":"other","command":"npm start","running":false}]}`)
		if got := finishedBackgroundNote(ctx); got != "" {
			t.Errorf("reported a job this run never started:\n%s", got)
		}
	})

	t.Run("no tracked jobs means no request at all", func(t *testing.T) {
		ctx, hits := newCtx(map[string]string{}, `{"jobs":[]}`)
		finishedBackgroundNote(ctx)
		if *hits != 0 {
			t.Errorf("polled the sandbox with nothing to ask about (%d calls)", *hits)
		}
	})

	t.Run("an unreachable sandbox is silent", func(t *testing.T) {
		ctx := &AgentContext{SandboxURL: "http://127.0.0.1:1",
			BackgroundJobs: map[string]string{"job-1": "x"}}
		if got := finishedBackgroundNote(ctx); got != "" {
			t.Errorf("a failed listing must not interrupt the loop:\n%s", got)
		}
	})
}

// A refusal the model cannot act on is a loop. It copies old_str into
// new_str precisely because it cannot reproduce a span AND change it, so
// "they are identical" tells it nothing it can use and it re-sends the same
// call until the breaker kills the session. Measured on a "build me a snake
// game" run: refused at turn 11, re-sent at 12 and 13, dead at 757s.
func TestIdenticalEditRefusalNamesAWayOut(t *testing.T) {
	dir := t.TempDir()
	ctx := &AgentContext{WorkingDir: dir,
		FilesRead:     map[string]string{},
		BodySeen:      map[string]bool{},
		FileReadTimes: map[string]time.Time{}}
	for _, tc := range []struct{ name, want string }{
		{"game.js", "replace_lines"},
		{"app.py", "structural_edit"},
		{"page.html", "structural_edit"},
	} {
		p := filepath.Join(dir, tc.name)
		if err := os.WriteFile(p, []byte("const x = 1;\nconst y = 2;\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		ctx.FilesRead[p] = "const x = 1;\nconst y = 2;\n"
		ctx.BodySeen[tc.name] = true
		ctx.FileReadTimes[p] = time.Now().Add(time.Minute)
		in, _ := json.Marshal(EditFileInput{
			Path: tc.name, OldStr: "const x = 1;", NewStr: "const x = 1;"})
		_, err := getTool("edit_file").Execute(in, ctx)
		if err == nil {
			t.Fatalf("%s: a no-op edit must be refused", tc.name)
		}
		msg := err.Error()
		if !strings.Contains(msg, tc.want) {
			t.Errorf("%s: refusal should name %s, got: %s", tc.name, tc.want, msg)
		}
		if !strings.Contains(msg, "not help") {
			t.Errorf("%s: refusal should say re-sending will not help: %s", tc.name, msg)
		}
	}
}

// ---------------------------------------------------------------------------
// The content-edit tools' syntax gate, as structured evidence
// ---------------------------------------------------------------------------
//
// edit_file, insert_after and replace_lines share one healthy->broken policy:
// evaluate the bytes about to be written once, and consult the original only
// when those bytes demonstrably fail. What each tool reports about that
// observation is what these pin -- through the real handlers, with the request
// counts that prove the evaluation happened once per side.

// editGateStub answers the checker for the three edit tools and counts what it
// was asked. `badFor` names the content it calls unparseable.
type editGateStub struct {
	mu         sync.Mutex
	syntax     []string
	structural int
	embedded   int
	unexpected []string
}

func (s *editGateStub) syntaxCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.syntax)
}

type editGateCase struct {
	name string
	// what is on disk before the edit
	original string
	// the edit each tool performs, expressed per tool below
	badFor func(code string) bool
	// non-200 for content carrying this marker: applicable, but unrunnable
	unavailableFor string
	// non-200 for the Nth syntax request (1-based). The baseline check is the
	// second one, and for an insertion the original's text is a subset of the
	// edited text, so only the ordinal can separate the two sides.
	unavailableCall int
	// no sandbox at all
	noSandbox bool
	// structural_check reports this symbol as newly unresolved
	introduces string
	// the destination directory is read-only, so the write fails
	readOnlyDir bool
	// non-default artifact: relative path plus the per-tool arguments that
	// edit it. Used for the content types no checker applies to.
	relPath string
	argsFor func(tool string) []byte

	wantSuccess    bool
	wantMutation   MutationStatus
	wantKind       ValidationKind
	wantStatus     ValidationStatus
	wantSyntaxReqs int
	// bytes expected on disk afterwards: "edited" or "original"
	wantDisk string
}

// The three tools' fixtures: the same file, edited the same way, so one table
// drives all of them.
const egOriginal = "import math\n\n\ndef area(r):\n    if r < 0:\n        raise ValueError('neg')\n    return math.pi * r * r\n\n\ndef edge(r):\n    for _ in range(1):\n        pass\n    return 2 * math.pi * r\n"

func egEdited(tool string) string {
	switch tool {
	case "edit_file":
		return strings.Replace(egOriginal, "raise ValueError('neg')",
			"raise ValueError('negative')", 1)
	case "insert_after":
		return strings.Replace(egOriginal, "import math\n", "import math\nimport sys\n", 1)
	default: // replace_lines
		return strings.Replace(egOriginal, "    return math.pi * r * r",
			"    return math.pi * r ** 2", 1)
	}
}

func egArgs(tool string) []byte {
	var m map[string]interface{}
	switch tool {
	case "edit_file":
		m = map[string]interface{}{"path": "app.py",
			"old_str": "raise ValueError('neg')", "new_str": "raise ValueError('negative')"}
	case "insert_after":
		m = map[string]interface{}{"path": "app.py", "line": 1, "content": "import sys"}
	default:
		m = map[string]interface{}{"path": "app.py", "start_line": 7, "end_line": 7,
			"expected_first_line": "    return math.pi * r * r",
			"expected_last_line":  "    return math.pi * r * r",
			"content":             "    return math.pi * r ** 2"}
	}
	b, _ := json.Marshal(m)
	return b
}

func runEditGate(t *testing.T, tool string, c editGateCase) (*ToolResult, string, *editGateStub, []string) {
	t.Helper()
	dir := t.TempDir()
	st := &editGateStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/"):
			st.mu.Lock()
			st.unexpected = append(st.unexpected, r.URL.Path)
			st.mu.Unlock()
			http.Error(w, "V3 generation must not run on this route", http.StatusTeapot)
		case r.URL.Path == "/internal/cyclomatic_complexity":
			// Below the bar that would route this edit through V3: this table
			// is about the local gate, not the pipeline.
			_, _ = w.Write([]byte(`{"ok":true,"cyclomatic_complexity":1}`))
		case r.URL.Path == "/internal/structural_check":
			st.mu.Lock()
			st.structural++
			st.mu.Unlock()
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			src, _ := body["source"].(string)
			out := map[string]interface{}{"ok": true, "unresolved": []string{}}
			if c.introduces != "" && strings.Contains(src, c.introduces+"(") {
				out["unresolved"] = []string{c.introduces}
			}
			json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/internal/embedded_script_check":
			st.mu.Lock()
			st.embedded++
			st.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"findings":[]}`))
		case strings.HasSuffix(r.URL.Path, "/syntax-check"):
			var in struct{ Code string }
			json.NewDecoder(r.Body).Decode(&in)
			st.mu.Lock()
			st.syntax = append(st.syntax, in.Code)
			st.mu.Unlock()
			st.mu.Lock()
			nth := len(st.syntax)
			st.mu.Unlock()
			if (c.unavailableFor != "" && strings.Contains(in.Code, c.unavailableFor)) ||
				(c.unavailableCall != 0 && nth == c.unavailableCall) {
				http.Error(w, "checker unavailable", http.StatusInternalServerError)
				return
			}
			bad := c.badFor != nil && c.badFor(in.Code)
			out := map[string]interface{}{"valid": !bad}
			if bad {
				out["errors"] = []string{"SyntaxError: invalid syntax (line 6)"}
			}
			json.NewEncoder(w).Encode(out)
		default:
			st.mu.Lock()
			st.unexpected = append(st.unexpected, r.URL.Path)
			st.mu.Unlock()
			http.Error(w, "unexpected endpoint", http.StatusTeapot)
		}
	}))
	defer srv.Close()

	rel := c.relPath
	if rel == "" {
		rel = "app.py"
	}
	path := filepath.Join(dir, rel)
	if err := os.WriteFile(path, []byte(c.original), 0o644); err != nil {
		t.Fatal(err)
	}
	var events []string
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	ctx.StreamFn = func(e string, _ interface{}) { events = append(events, e) }
	ctx.V3URL = srv.URL
	if !c.noSandbox {
		ctx.SandboxURL = srv.URL
	}
	ctx.SessionWrites[rel] = true
	ctx.RecordFileRead(path, c.original)

	if c.readOnlyDir {
		// Both: edit_file creates a temp file in the directory, the two
		// line-addressed tools overwrite the file in place.
		if err := os.Chmod(path, 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755); os.Chmod(path, 0o644) })
	}

	args := egArgs(tool)
	if c.argsFor != nil {
		args = c.argsFor(tool)
	}
	res := executeToolCall(tool, args, ctx)
	if len(st.unexpected) > 0 {
		t.Fatalf("unexpected endpoints: %v", st.unexpected)
	}
	after, _ := os.ReadFile(path)
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".atlas.tmp") {
			t.Errorf("temporary artifact survived: %s", e.Name())
		}
	}
	return res, string(after), st, events
}

func TestEditToolsClassifyTheirSyntaxGate(t *testing.T) {
	broken := func(marker string) func(string) bool {
		return func(code string) bool { return strings.Contains(code, marker) }
	}
	for _, tool := range []string{"edit_file", "insert_after", "replace_lines"} {
		tool := tool
		edited := egEdited(tool)
		// What the edit introduced, used to make the checker condemn exactly
		// the proposed bytes and not the original.
		var editedMarker string
		switch tool {
		case "edit_file":
			editedMarker = "negative"
		case "insert_after":
			editedMarker = "import sys"
		default:
			editedMarker = "r ** 2"
		}

		for _, c := range []editGateCase{
			{name: "proposal passes", original: egOriginal,
				wantSuccess: true, wantMutation: MutationApplied,
				wantKind: ValidationKindSyntax, wantStatus: ValidationPassed,
				wantSyntaxReqs: 1, wantDisk: "edited"},
			{name: "checker applicable but unavailable", original: egOriginal,
				unavailableFor: "import math",
				wantSuccess:    true, wantMutation: MutationApplied,
				wantKind: ValidationKindSyntax, wantStatus: ValidationNotRun,
				wantSyntaxReqs: 1, wantDisk: "edited"},
			{name: "no sandbox configured", original: egOriginal, noSandbox: true,
				wantSuccess: true, wantMutation: MutationApplied,
				wantKind: ValidationKindSyntax, wantStatus: ValidationNotRun,
				wantSyntaxReqs: 0, wantDisk: "edited"},
			{name: "failed over a failed baseline lands", original: egOriginal,
				badFor:      broken("import math"), // both sides are broken
				wantSuccess: true, wantMutation: MutationApplied,
				wantKind: ValidationKindSyntax, wantStatus: ValidationFailed,
				wantSyntaxReqs: 2, wantDisk: "edited"},
			{name: "failed over a passing baseline is refused", original: egOriginal,
				badFor:      broken(editedMarker),
				wantSuccess: false, wantMutation: MutationRefused,
				wantKind: ValidationKindSyntax, wantStatus: ValidationFailed,
				wantSyntaxReqs: 2, wantDisk: "original"},
			{name: "failed over an unavailable baseline is refused", original: egOriginal,
				badFor: broken(editedMarker), unavailableCall: 2,
				wantSuccess: false, wantMutation: MutationRefused,
				wantKind: ValidationKindSyntax, wantStatus: ValidationFailed,
				wantSyntaxReqs: 2, wantDisk: "original"},
			{name: "passed then the write fails", original: egOriginal,
				readOnlyDir: true,
				wantSuccess: false, wantMutation: MutationFailed,
				wantKind: ValidationKindSyntax, wantStatus: ValidationPassed,
				wantSyntaxReqs: 1, wantDisk: "original"},
			{name: "not_run then the write fails", original: egOriginal,
				noSandbox: true, readOnlyDir: true,
				wantSuccess: false, wantMutation: MutationFailed,
				wantKind: ValidationKindSyntax, wantStatus: ValidationNotRun,
				wantSyntaxReqs: 0, wantDisk: "original"},
		} {
			c := c
			t.Run(tool+"/"+c.name, func(t *testing.T) {
				res, disk, st, events := runEditGate(t, tool, c)

				if res.Success != c.wantSuccess {
					t.Fatalf("Success = %v, want %v (%s)", res.Success, c.wantSuccess, res.Error)
				}
				want := edited
				if c.wantDisk == "original" {
					want = c.original
				}
				if disk != want {
					t.Fatalf("bytes on disk:\n got %q\nwant %q", disk, want)
				}
				if res.MutationStatus != c.wantMutation ||
					res.ValidationKind != c.wantKind ||
					res.ValidationStatus != c.wantStatus {
					t.Errorf("got %q/%q/%q, want %q/%q/%q",
						res.MutationStatus, res.ValidationKind, res.ValidationStatus,
						c.wantMutation, c.wantKind, c.wantStatus)
				}
				if !res.Classified() {
					t.Error("result not fully classified")
				}
				if got := st.syntaxCount(); got != c.wantSyntaxReqs {
					t.Errorf("syntax-check requests = %d, want %d", got, c.wantSyntaxReqs)
				}
				// No V3 ran on this route, so no provenance may appear.
				if res.V3Used || res.CandidatesTested != 0 || res.PhaseSolved != "" {
					t.Errorf("V3 provenance on a non-pipeline edit: %+v", res)
				}
				for _, e := range events {
					if e != "v3_progress" && e != "text" {
						t.Errorf("unexpected SSE projection %q", e)
					}
				}
			})
		}
	}
}

// Syntax passes on the edited bytes and a later gate refuses them: the
// structural failure is decisive, and the passing syntax verdict must not be
// what the result reports.
func TestEditToolsReportTheDecisiveStructuralRefusal(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]interface{}
	}{
		{"edit_file", map[string]interface{}{"path": "app.py",
			"old_str": "    return math.pi * r * r",
			"new_str": "    return missing_helper(r)"}},
		{"insert_after", map[string]interface{}{"path": "app.py", "line": 7,
			"content": "AREA = missing_helper(2)"}},
		{"replace_lines", map[string]interface{}{"path": "app.py",
			"start_line": 7, "end_line": 7,
			"expected_first_line": "    return math.pi * r * r",
			"expected_last_line":  "    return math.pi * r * r",
			"content":             "    return missing_helper(r)"}},
	} {
		tc := tc
		t.Run(tc.tool, func(t *testing.T) {
			dir := t.TempDir()
			st := &editGateStub{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/internal/cyclomatic_complexity":
					_, _ = w.Write([]byte(`{"ok":true,"cyclomatic_complexity":1}`))
				case r.URL.Path == "/internal/structural_check":
					var body map[string]interface{}
					json.NewDecoder(r.Body).Decode(&body)
					src, _ := body["source"].(string)
					out := map[string]interface{}{"ok": true, "unresolved": []string{}}
					if strings.Contains(src, "missing_helper(") {
						out["unresolved"] = []string{"missing_helper"}
					}
					json.NewEncoder(w).Encode(out)
				case strings.HasSuffix(r.URL.Path, "/syntax-check"):
					var in struct{ Code string }
					json.NewDecoder(r.Body).Decode(&in)
					st.mu.Lock()
					st.syntax = append(st.syntax, in.Code)
					st.mu.Unlock()
					_, _ = w.Write([]byte(`{"valid":true}`))
				default:
					_, _ = w.Write([]byte(`{"ok":true}`))
				}
			}))
			defer srv.Close()

			path := filepath.Join(dir, "app.py")
			if err := os.WriteFile(path, []byte(egOriginal), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx := NewAgentContext(dir, Tier2Medium)
			ctx.PermissionMode = PermissionYolo
			ctx.StreamFn = func(string, interface{}) {}
			ctx.V3URL = srv.URL
			ctx.SandboxURL = srv.URL
			ctx.SessionWrites["app.py"] = true
			ctx.RecordFileRead(path, egOriginal)

			args, _ := json.Marshal(tc.args)
			res := executeToolCall(tc.tool, args, ctx)

			if res.Success {
				t.Fatalf("the structural gate did not refuse: %+v", res)
			}
			if !strings.Contains(res.Error, "missing_helper") {
				t.Fatalf("refusal came from a different gate: %q", res.Error)
			}
			if res.MutationStatus != MutationRefused ||
				res.ValidationKind != ValidationKindStructural ||
				res.ValidationStatus != ValidationFailed {
				t.Errorf("got %q/%q/%q, want refused/structural/failed",
					res.MutationStatus, res.ValidationKind, res.ValidationStatus)
			}
			if !strings.Contains(res.ValidationDetail, "missing_helper") {
				t.Errorf("ValidationDetail must name the symbol, got %q", res.ValidationDetail)
			}
			if !res.Classified() {
				t.Error("result not fully classified")
			}
			if disk, _ := os.ReadFile(path); string(disk) != egOriginal {
				t.Errorf("a refused edit changed the file: %q", disk)
			}
			// Syntax ran first and passed on those exact bytes; exactly one
			// request, and the baseline was never consulted.
			if got := st.syntaxCount(); got != 1 {
				t.Errorf("syntax-check requests = %d, want 1", got)
			}
		})
	}
}

// Source-level inventory. The three edit handlers now use the structured
// producer, and the remaining legacy wrapper calls are named rather than
// counted, so a reintroduction has to edit this list.
func TestLegacyCheckerWrapperInventory(t *testing.T) {
	src, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	span := func(from, to string) string {
		i := strings.Index(body, from)
		if i < 0 {
			t.Fatalf("cannot find %q", from)
		}
		rest := body[i:]
		j := strings.Index(rest, to)
		if j < 0 {
			return rest
		}
		return rest[:j]
	}
	for name, fn := range map[string]string{
		"edit_file":     span("Name:   \"edit_file\"", "\nfunc structuralEditTool("),
		"insert_after":  span("Name:   \"insert_after\"", "\n// replaceLinesMaxSpan"),
		"replace_lines": span("Name:   \"replace_lines\"", "\nfunc deleteFileTool("),
	} {
		if n := strings.Count(fn, "checkFallbackSyntax("); n != 0 {
			t.Errorf("%s still calls the legacy wrapper %d time(s)", name, n)
		}
		if !strings.Contains(fn, "editSyntaxObservation(") {
			t.Errorf("%s does not use the shared structured gate", name)
		}
	}

	// What is left, named. Two sites, both inside writeFileWithV3: the V3
	// preflight's own pair is gone, and these belong to candidate revocation.
	remaining := strings.Count(body, "checkFallbackSyntax(ctx")
	if remaining != 0 {
		t.Errorf("unexpected legacy wrapper calls remain in tools.go: %d", remaining)
	}
	// One producer, one wrapper definition -- no second checker implementation.
	gates, err := os.ReadFile("gates.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(gates), "func checkFallbackSyntax("); n != 1 {
		t.Errorf("checkFallbackSyntax definitions = %d, want 1", n)
	}
	if n := strings.Count(string(gates), "func fallbackSyntaxOutcomeFor("); n != 1 {
		t.Errorf("fallbackSyntaxOutcomeFor definitions = %d, want 1", n)
	}
	// And the shared edit gate exists exactly once.
	if n := strings.Count(body, "func editSyntaxObservation("); n != 1 {
		t.Errorf("editSyntaxObservation definitions = %d, want 1", n)
	}
}

// Nothing applicable: an artifact type neither checker can speak about. The
// observation is a real one -- "there was nothing here to check" -- and it is
// distinct from the unavailable-service case, which reports not_run.
func TestEditToolsClassifyANotApplicableArtifact(t *testing.T) {
	// Routing premises, asserted rather than assumed: a future entry in either
	// map turns this into a different case, and the test must fail then.
	if _, gated := syntaxGateLanguages[".rs"]; gated {
		t.Fatal(".rs gained a sandbox checker; pick a type neither check applies to")
	}
	if embeddedScriptExts[".rs"] {
		t.Fatal(".rs gained an embedded-script check; pick a type neither check applies to")
	}

	const rustOriginal = "pub fn area(radius: f64) -> f64 {\n    if radius < 0.0 {\n        panic!(\"negative radius\");\n    }\n    std::f64::consts::PI * radius * radius\n}\n\npub fn total(radii: &[f64]) -> f64 {\n    let mut sum = 0.0;\n    for r in radii {\n        sum += area(*r);\n    }\n    sum\n}\n"

	edited := map[string]string{
		"edit_file": strings.Replace(rustOriginal, "let mut sum = 0.0;",
			"let mut sum: f64 = 0.0;", 1),
		"insert_after": strings.Replace(rustOriginal, "pub fn area(radius: f64) -> f64 {\n",
			"pub fn area(radius: f64) -> f64 {\n    // checked\n", 1),
		"replace_lines": strings.Replace(rustOriginal, "    sum\n}", "    sum + 0.0\n}", 1),
	}
	args := func(tool string) []byte {
		var m map[string]interface{}
		switch tool {
		case "edit_file":
			m = map[string]interface{}{"path": "engine.rs",
				"old_str": "let mut sum = 0.0;", "new_str": "let mut sum: f64 = 0.0;"}
		case "insert_after":
			m = map[string]interface{}{"path": "engine.rs", "line": 1,
				"content": "    // checked"}
		default:
			m = map[string]interface{}{"path": "engine.rs",
				"start_line": 13, "end_line": 13,
				"expected_first_line": "    sum",
				"expected_last_line":  "    sum",
				"content":             "    sum + 0.0"}
		}
		b, _ := json.Marshal(m)
		return b
	}

	for _, tool := range []string{"edit_file", "insert_after", "replace_lines"} {
		tool := tool
		t.Run(tool, func(t *testing.T) {
			res, disk, st, events := runEditGate(t, tool, editGateCase{
				original: rustOriginal, relPath: "engine.rs", argsFor: args})

			if !res.Success {
				t.Fatalf("the edit did not land: %s", res.Error)
			}
			if disk != edited[tool] {
				t.Fatalf("bytes on disk:\n got %q\nwant %q", disk, edited[tool])
			}
			if res.MutationStatus != MutationApplied ||
				res.ValidationKind != ValidationKindNone ||
				res.ValidationStatus != ValidationNotApplicable {
				t.Errorf("got %q/%q/%q, want applied/none/not_applicable",
					res.MutationStatus, res.ValidationKind, res.ValidationStatus)
			}
			if res.ValidationStatus.Passed() {
				t.Error("nothing was checked, so nothing may read as passed")
			}
			if !res.Classified() {
				t.Error("result not fully classified")
			}
			if st.syntaxCount() != 0 {
				t.Errorf("syntax-check requests = %d, want 0", st.syntaxCount())
			}
			st.mu.Lock()
			embedded, structural := st.embedded, st.structural
			st.mu.Unlock()
			if embedded != 0 {
				t.Errorf("embedded-script requests = %d, want 0", embedded)
			}
			if structural != 0 {
				t.Errorf("structural requests = %d, want 0 (the gate is .py-scoped)", structural)
			}
			if res.V3Used || res.CandidatesTested != 0 || res.PhaseSolved != "" {
				t.Errorf("V3 provenance on a non-pipeline edit: %+v", res)
			}
			for _, e := range events {
				if e != "v3_progress" && e != "text" {
					t.Errorf("unexpected SSE projection %q", e)
				}
			}
		})
	}
}

// --- Deterministic model-facing bytes ----------------------------------------
//
// Three parts of every upstream request are built by ranging toolRegistry, a Go
// map: the response_format tool-name enum, the GBNF tool-name alternation, and
// the "### <tool>" documentation blocks in the system prompt. Go randomises map
// iteration per range, so identical inputs produce different bytes on the wire.
//
// Within one process the difference is easy to miss, so this compares fresh
// processes: the test re-executes the test binary, and each child builds the
// real artefacts with the production builders and prints them.

const promptBytesChildEnv = "ATLAS_PROMPT_BYTES_CHILD"

// promptArtifacts is every model-facing byte sequence these builders produce,
// across every mode that uses them.
func promptArtifacts(t *testing.T) map[string]string {
	t.Helper()
	// A FIXED working directory, not t.TempDir(): the prompt embeds the path,
	// and a per-process temp dir is a difference in the INPUT. Holding it
	// constant keeps this a comparison of the builders, so nothing about the
	// emitted bytes has to be normalised afterwards.
	ctx := NewAgentContext("/atlas-prompt-fixture", Tier2Medium)
	out := map[string]string{}
	out["system_prompt"] = buildSystemPrompt(ctx)
	out["tool_docs_excluding"] = buildToolDescriptionsExcluding([]string{"edit_file"})
	out["grammar"] = buildGBNFGrammarForTools(nil)
	out["grammar_excluded"] = buildGBNFGrammarForTools([]string{"edit_file", "write_file"})
	for _, mode := range []string{"strict", "loose"} {
		t.Setenv("ATLAS_GRAMMAR_MODE", mode)
		b, err := json.Marshal(buildResponseFormat())
		if err != nil {
			t.Fatalf("response_format %s: %v", mode, err)
		}
		out["response_format_"+mode] = string(b)
	}
	b, err := json.Marshal(buildToolCallSchemaForTools([]string{"edit_file"}))
	if err != nil {
		t.Fatal(err)
	}
	out["schema_excluded"] = string(b)
	return out
}

// TestPromptBytesChild is the child half: it prints the artefacts and exits.
// It does nothing when run as part of an ordinary suite.
func TestPromptBytesChild(t *testing.T) {
	if os.Getenv(promptBytesChildEnv) != "1" {
		t.Skip("child-only: driven by TestModelFacingBytesAreProcessStable")
	}
	b, err := json.Marshal(promptArtifacts(t))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("ARTIFACTS " + string(b))
}

// Identical inputs must produce identical model-facing bytes in every process.
//
// Nothing is sorted or canonicalised before comparing: these are the bytes that
// would go to llama-server.
func TestModelFacingBytesAreProcessStable(t *testing.T) {
	const runs = 8
	var seen []map[string]string
	for i := 0; i < runs; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPromptBytesChild$", "-test.v=false")
		cmd.Env = append(os.Environ(), promptBytesChildEnv+"=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("child %d: %v\n%s", i, err, out)
		}
		var payload string
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "ARTIFACTS ") {
				payload = strings.TrimPrefix(line, "ARTIFACTS ")
			}
		}
		if payload == "" {
			t.Fatalf("child %d printed no artefacts:\n%s", i, out)
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("child %d payload: %v", i, err)
		}
		seen = append(seen, m)
	}

	keys := make([]string, 0, len(seen[0]))
	for k := range seen[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		variants := map[string]int{}
		for _, m := range seen {
			variants[m[k]]++
		}
		if len(variants) != 1 {
			// Report where they diverge, and prove the divergence is ordering
			// and nothing else: the same lines, in a different sequence.
			var forms []string
			for v := range variants {
				forms = append(forms, v)
			}
			sort.Strings(forms)
			a, b := forms[0], forms[1]
			i := 0
			for i < len(a) && i < len(b) && a[i] == b[i] {
				i++
			}
			lo := i - 60
			if lo < 0 {
				lo = 0
			}
			clip := func(s string) string {
				hi := i + 90
				if hi > len(s) {
					hi = len(s)
				}
				return s[lo:hi]
			}
			t.Errorf("%s differs across %d fresh processes (%d distinct forms), "+
				"first at byte %d:\n  A: %s\n  B: %s",
				k, runs, len(variants), i, clip(a), clip(b))
			continue
		}
		t.Logf("%s: identical across %d processes (%d bytes)", k, runs, len(seen[0][k]))
	}
}

// The inventory behind those bytes is identical across processes: same tools,
// same descriptions, same schemas, same grammar alternatives, same
// documentation bodies. Only their sequence moves.
//
// This is green both before and after the ordering fix, which is what makes it
// the evidence that ordering is the ONLY thing that changed.
func TestToolInventoryIsIdenticalAcrossProcesses(t *testing.T) {
	const runs = 6
	var seen []map[string]string
	for i := 0; i < runs; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestToolInventoryChild$", "-test.v=false")
		cmd.Env = append(os.Environ(), promptBytesChildEnv+"=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("child %d: %v\n%s", i, err, out)
		}
		var payload string
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "INVENTORY ") {
				payload = strings.TrimPrefix(line, "INVENTORY ")
			}
		}
		if payload == "" {
			t.Fatalf("child %d printed no inventory:\n%s", i, out)
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, m)
	}
	for _, m := range seen[1:] {
		if len(m) != len(seen[0]) {
			t.Fatalf("inventory size differs: %d vs %d", len(m), len(seen[0]))
		}
		for k, v := range seen[0] {
			if m[k] != v {
				t.Errorf("%s differs between processes", k)
			}
		}
	}
	t.Logf("%d inventory facts identical across %d processes", len(seen[0]), runs)
}

// TestToolInventoryChild prints order-independent facts about the registry.
func TestToolInventoryChild(t *testing.T) {
	if os.Getenv(promptBytesChildEnv) != "1" {
		t.Skip("child-only: driven by TestToolInventoryIsIdenticalAcrossProcesses")
	}
	facts := map[string]string{}
	var names []string
	for _, tool := range allTools() {
		names = append(names, tool.Name)
		facts["desc:"+tool.Name] = tool.Description
		facts["schema:"+tool.Name] = fmt.Sprintf("%T|%s",
			tool.InputSchema, generateInputExample(tool.Name))
		facts["effect:"+tool.Name] = fmt.Sprintf("%v|ro=%v|destructive=%v",
			tool.Effect, tool.ReadOnly, tool.Destructive)
	}
	sort.Strings(names)
	facts["names"] = strings.Join(names, ",")
	facts["count"] = fmt.Sprint(len(names))

	// The grammar's alternatives, as a set.
	var alts []string
	for _, line := range strings.Split(buildGBNFGrammarForTools(nil), "\n") {
		head, body, ok := strings.Cut(line, "::=")
		if !ok || !strings.Contains(strings.TrimSpace(head), "tool-name") {
			continue
		}
		alts = strings.Split(strings.TrimSpace(body), " | ")
		sort.Strings(alts)
	}
	facts["grammar_alternatives"] = strings.Join(alts, ",")

	// The documentation blocks, as a set of bodies keyed by tool.
	docs := buildToolDescriptionsExcluding(nil)
	for i, block := range strings.Split(docs, "\n### ") {
		if i == 0 {
			facts["docs_preamble"] = block
			continue
		}
		name, body, _ := strings.Cut(block, "\n")
		// Trailing newlines are trimmed because the "\n### " separator eats
		// one from every block except the last, so whichever block happens to
		// sit last keeps an extra one. That is an artefact of splitting here,
		// not a difference in what production emits -- the emitted bytes are
		// compared untrimmed by TestModelFacingBytesAreProcessStable.
		facts["docblock:"+strings.TrimSpace(name)] = strings.TrimRight(body, "\n")
	}

	// The schema enum, as a set.
	schema := buildToolCallSchemaForTools(nil)
	b, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]interface{}
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	var enums []string
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch n := v.(type) {
		case map[string]interface{}:
			for k, e := range n {
				if k == "enum" {
					if arr, ok := e.([]interface{}); ok {
						for _, x := range arr {
							if s, ok := x.(string); ok {
								enums = append(enums, s)
							}
						}
					}
				}
				walk(e)
			}
		case []interface{}:
			for _, e := range n {
				walk(e)
			}
		}
	}
	walk(round)
	sort.Strings(enums)
	facts["schema_enums"] = strings.Join(enums, ",")

	out, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("INVENTORY " + string(out))
}

// One ordering authority, enforced structurally.
//
// The determinism above holds because allTools() is the only place that ranges
// the registry map; the enum and the grammar alternation read their order from
// it. A future reader that ranges toolRegistry directly would silently get a
// fresh Go map order and put nondeterministic bytes back on the wire, and the
// only symptom would be a cache-miss rate nobody attributes. So the direct
// range is what fails here, not the symptom.
func TestOnlyAllToolsRangesTheRegistry(t *testing.T) {
	const authority = "allTools"
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				rng, ok := n.(*ast.RangeStmt)
				if !ok {
					return true
				}
				id, ok := rng.X.(*ast.Ident)
				if !ok || id.Name != "toolRegistry" {
					return true
				}
				found++
				if fd.Name.Name != authority {
					t.Errorf("%s:%d: %s ranges toolRegistry directly. Map iteration "+
						"order is randomised per range, and anything derived from it "+
						"that reaches the model changes bytes between identical "+
						"requests. Read the order from %s() instead.",
						name, fset.Position(rng.Pos()).Line, fd.Name.Name, authority)
				}
				return true
			})
		}
	}
	if found == 0 {
		t.Fatal("no range over toolRegistry found at all — this guard is no longer wired to anything")
	}
	if found != 1 {
		t.Errorf("%d ranges over toolRegistry, want exactly the one in %s()", found, authority)
	}
}
