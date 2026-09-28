package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A truncated read shows part of a file. The unread-citation check works per
// file, so it let a reply describe code past the cut. Smoke run 2026-09-27,
// bugfind_tiebreak: both reads stopped near line 190, the reply named
// `_score_plan` and said it lay "past the provided snippet", and the run ended
// completed.

// planningSource is a 600-line file with _score_plan defined at line 412.
func planningSource() string {
	var b strings.Builder
	for i := 1; i <= 600; i++ {
		switch i {
		case 412:
			b.WriteString("def _score_plan(plans):\n")
		case 413:
			b.WriteString("    return sorted(plans, key=lambda p: p.score)\n")
		default:
			fmt.Fprintf(&b, "# planning line %03d: padding that makes the file too long for one read\n", i)
		}
	}
	return b.String()
}

const unshownAnswer = "The issue is in `planning.py` within the `_score_plan` function " +
	"(which appears later in the file, past the provided snippet)."

func TestATruncatedReadDoesNotShowTheWholeFile(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"planning.py": planningSource()})
	path := resolveAgentPath(ctx, "planning.py")
	ctx.RecordBodyRead(path, 1, 189, 600)
	if got := unreadFileCitations(ctx, unshownAnswer); len(got) != 0 {
		t.Fatalf("the file was opened, so the file-level check passes it: %v", got)
	}
	got := unshownSymbolCitations(ctx, unshownAnswer)
	if len(got) != 1 || got[0].Name != "_score_plan" || got[0].Line != 412 || got[0].File != "planning.py" {
		t.Fatalf("want _score_plan at line 412 of planning.py, got %+v", got)
	}
}

func TestASymbolInsideTheReadIsNotFlagged(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"planning.py": planningSource()})
	path := resolveAgentPath(ctx, "planning.py")
	ctx.RecordBodyRead(path, 380, 440, 600)
	if got := unshownSymbolCitations(ctx, unshownAnswer); len(got) != 0 {
		t.Fatalf("line 412 was shown: %+v", got)
	}
}

func TestReadingTheRestClearsTheGap(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"planning.py": planningSource()})
	path := resolveAgentPath(ctx, "planning.py")
	ctx.RecordBodyRead(path, 1, 189, 600)
	ctx.RecordBodyRead(path, 401, 440, 600)
	if got := unshownSymbolCitations(ctx, unshownAnswer); len(got) != 0 {
		t.Fatalf("a second read showed line 412: %+v", got)
	}
}

func TestAWholeReadOrAWriteShowsEverything(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"planning.py": planningSource()})
	path := resolveAgentPath(ctx, "planning.py")
	ctx.RecordBodyRead(path, 1, 600, 600)
	ctx.RecordBodyRead(path, 1, 50, 600) // a later partial read takes nothing back
	if got := unshownSymbolCitations(ctx, unshownAnswer); len(got) != 0 {
		t.Fatalf("the whole file was read: %+v", got)
	}

	ctx = evidenceCtx(t, map[string]string{"planning.py": planningSource()})
	path = resolveAgentPath(ctx, "planning.py")
	ctx.RecordBodyRead(path, 1, 189, 600)
	ctx.RecordBodySeen(path) // what every write and edit path records
	if got := unshownSymbolCitations(ctx, unshownAnswer); len(got) != 0 {
		t.Fatalf("after a write the old line numbers no longer hold: %+v", got)
	}
}

func TestANameShownInAnotherFileIsNotFlagged(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{
		"planning.py": planningSource(),
		"helpers.py":  "def _score_plan(plans):\n    return plans\n",
	})
	ctx.RecordBodyRead(resolveAgentPath(ctx, "planning.py"), 1, 189, 600)
	ctx.RecordBodyRead(resolveAgentPath(ctx, "helpers.py"), 1, 3, 3)
	if got := unshownSymbolCitations(ctx, unshownAnswer); len(got) != 0 {
		t.Fatalf("a definition of the name was shown in helpers.py: %+v", got)
	}
}

func TestFileNamesAndUnknownNamesAreIgnored(t *testing.T) {
	ctx := evidenceCtx(t, map[string]string{"planning.py": planningSource()})
	ctx.RecordBodyRead(resolveAgentPath(ctx, "planning.py"), 1, 189, 600)
	for _, answer := range []string{
		"The bug is in `planning.py`.",
		"It reads `config.json` and calls `sorted()` and `os.path.join`.",
		"No code named at all.",
		"",
	} {
		if got := unshownSymbolCitations(ctx, answer); len(got) != 0 {
			t.Errorf("%q: nothing here is unshown code, got %+v", answer, got)
		}
	}
}

func TestDefinitionLines(t *testing.T) {
	src := strings.Join([]string{
		"def target(x):",                      // 1: python function
		"    async def target(self):",         // 2: indented async method
		"class target:",                       // 3: class
		"func target() {}",                    // 4: go function
		"func (s *T) target(n int) {}",        // 5: go method
		"export async function target() {}",   // 6: js
		"pub fn target() {}",                  // 7: rust
		"type target struct{}",                // 8: go type
		"x = target(y)",                       // call site
		"def target_other():",                 // longer name
		"# def target(): in a comment is not", // comment
	}, "\n")
	got := definitionLines(src, "target")
	want := []int{1, 2, 3, 4, 5, 6, 7, 8}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("definitionLines = %v, want %v", got, want)
	}
}

func TestTheMessageNamesTheCodeAndTheLines(t *testing.T) {
	msg := unshownSymbolMessage([]unshownSymbol{{
		Name: "_score_plan", File: "planning.py", Line: 412, Spans: [][2]int{{1, 189}, {300, 320}},
	}})
	for _, want := range []string{"`_score_plan`", "line 412 of planning.py",
		"lines 1-189 and 300-320", "read_file"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}
}

// read_file must record the lines it showed, not the whole file.
func TestReadFileRecordsWhatItShowed(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	n := 0
	for b.Len() < 3*maxReadFileBytes {
		n++
		fmt.Fprintf(&b, "# line %05d of a file too long for one read\n", n)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.py"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := NewAgentContext(dir, Tier2Medium)
	path := resolveAgentPath(ctx, "big.py")
	read := func(args map[string]interface{}) {
		t.Helper()
		raw, _ := json.Marshal(args)
		res, err := readFileTool().Execute(raw, ctx)
		if err != nil || res == nil || !res.Success {
			t.Fatalf("read_file %v: %v %+v", args, err, res)
		}
	}
	read(map[string]interface{}{"path": "big.py"})
	spans := ctx.ShownSpans(path)
	if len(spans) != 1 || spans[0][0] != 1 || spans[0][1] >= n {
		t.Fatalf("a truncated read must record only its head, got %v of %d lines", spans, n)
	}
	last := n - 5
	if ctx.LineWasShown(path, last) {
		t.Fatalf("line %d was past the cut", last)
	}
	read(map[string]interface{}{"path": "big.py", "offset": n - 10, "limit": 10})
	if !ctx.LineWasShown(path, last) {
		t.Fatalf("the ranged read showed line %d; spans %v", last, ctx.ShownSpans(path))
	}
}

// End to end, as the smoke run went: a truncated read, an answer about code
// past the cut, and one bounce that sends the model back to read it.
func TestAnAnswerAboutUnshownCodeIsSentBackToRead(t *testing.T) {
	dir := t.TempDir()
	src := planningSource()
	for len(src) < 2*maxReadFileBytes {
		src += "# more padding past the end\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "planning.py"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	turns := 0
	plan := func(i int) map[string]interface{} {
		switch i {
		case 0:
			return map[string]interface{}{"type": "tool_call", "name": "read_file",
				"args": map[string]string{"path": "planning.py"}}
		case 2:
			return map[string]interface{}{"type": "tool_call", "name": "read_file",
				"args": map[string]interface{}{"path": "planning.py", "offset": 400, "limit": 40}}
		}
		return map[string]interface{}{"type": "text", "content": unshownAnswer}
	}
	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		mu.Lock()
		i := turns
		turns++
		mu.Unlock()
		if i >= 8 {
			http.Error(w, "turn ceiling exceeded", http.StatusInsufficientStorage)
			return
		}
		call, _ := json.Marshal(plan(i))
		d, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{"content": string(call)}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", d)
	}))
	t.Cleanup(inference.Close)

	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.InferenceURL = inference.URL
	ctx.PermissionMode = PermissionYolo
	ctx.MaxTurns = 0
	var gates []string
	terminal := map[string]string{}
	ctx.StreamFn = func(et string, data interface{}) {
		b, _ := json.Marshal(data)
		mu.Lock()
		defer mu.Unlock()
		if et == "gate" {
			var g struct{ Gate, Reason string }
			if json.Unmarshal(b, &g) == nil && g.Gate == "evidence_gate" {
				gates = append(gates, g.Reason)
			}
		}
		if et == "done" {
			var m map[string]string
			json.Unmarshal(b, &m)
			for k, v := range m {
				terminal[k] = v
			}
		}
	}
	runAgentLoop(ctx, "Why does the planner pick a longer plan when two plans tie? Do not change the code.")

	t.Logf("gates=%v status=%q reason=%q", gates, terminal["status"], terminal["reason"])
	if len(gates) != 1 || !strings.Contains(gates[0], "_score_plan") {
		t.Fatalf("want one evidence_gate bounce naming _score_plan, got %v", gates)
	}
	if terminal["status"] != "completed" {
		t.Errorf("after the read the answer stands: status %q, reason %q", terminal["status"], terminal["reason"])
	}
}
