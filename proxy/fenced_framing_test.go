package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// writeStubSSE emits one content delta plus a usage frame, the shape the
// real inference stream uses.
func writeStubSSE(w http.ResponseWriter, content string, tokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	delta, _ := json.Marshal(map[string]interface{}{
		"choices": []map[string]interface{}{
			{"delta": map[string]string{"content": content}}},
	})
	usage, _ := json.Marshal(map[string]interface{}{
		"choices": []interface{}{},
		"usage":   map[string]int{"total_tokens": tokens},
	})
	fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", delta, usage)
}

// Framing, not language.
//
// "@fenced" routes a file body around the JSON channel, and the caller must
// then decide whether what came back is a whole file. Only a closed fence says
// so. An opening fence with no closer is cut off, and a bare body with no fence
// at all carries no framing evidence in either direction: an inlined body is
// usually bare, and one that stopped mid-emission can still parse cleanly.
//
// Nothing below reads the payload as source. No test here depends on a
// language, a comment, a final line, indentation, a token count, or any
// benchmark task.

// --- the classifier both paths share --------------------------------------

// Only a closed fence proves a whole file. Properly framed content that looks
// unfinished is a file; unframed content that looks finished is not evidence.
func TestFramingIsDecidedByFencesAlone(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    fenceFraming
		body    string
	}{
		{"complete block", "```lang\nBODY\n```", fenceFramingComplete, "BODY\n"},
		{"complete bare-tag block", "```\nBODY\n```", fenceFramingComplete, "BODY\n"},
		{"opener then body, never closed", "```lang\nBODY", fenceFramingUnterminated, ""},
		{"opener only", "```lang", fenceFramingUnterminated, ""},
		{"closing fence only", "BODY\n```", fenceFramingUnterminated, ""},
		{"no fence at all", "BODY\n", fenceFramingAbsent, ""},
		{"empty", "", fenceFramingAbsent, ""},
		{"whitespace only", "   \n\t\n", fenceFramingAbsent, ""},
		{"fenced but empty body", "```lang\n\n```", fenceFramingUnterminated, ""},
		// Framed, and the last line is a comment, a dangling clause or an
		// open bracket: complete, because the fence closed.
		{"framed, ends on a comment", "```lang\nx = 1\n# trailing note about the rule\n```",
			fenceFramingComplete, "x = 1\n# trailing note about the rule\n"},
		{"framed, stops mid-word", "```lang\nprose that simply stops mid senten\n```",
			fenceFramingComplete, "prose that simply stops mid senten\n"},
		{"framed, open bracket", "```lang\ndata = [\n  1,\n  2,\n```", fenceFramingComplete, "data = [\n  1,\n  2,\n"},
		{"framed, not ASCII", "```lang\nこんにちは\n```", fenceFramingComplete, "こんにちは\n"},
		{"framed, inert bytes", "```lang\nAAAA\nBBBB\nCCC\n```", fenceFramingComplete, "AAAA\nBBBB\nCCC\n"},
		// Unframed: a finished-looking program, or one cut mid-emission, is
		// never handed on as a file.
		{"unframed program", "x = 1\nprint(x)\n", fenceFramingAbsent, ""},
		{"unframed word", "COMPLETE\n", fenceFramingAbsent, ""},
		{"unframed, cut mid-emission", "AAAA\nBBBB\nCCC", fenceFramingAbsent, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, body := classifyFencedPayload(c.payload)
			if got != c.want {
				t.Errorf("framing %v, want %v", got, c.want)
			}
			if body != c.body {
				t.Errorf("body %q, want %q", body, c.body)
			}
		})
	}
}

// A file that legitimately contains fences must never be cut at an interior
// one. Under a ``` wrapper an interior closing fence makes the reply ambiguous
// (it is also the shape of a file plus a second block), so nothing is returned;
// under a ```` wrapper the whole body is the file.
func TestFramingKeepsInteriorFences(t *testing.T) {
	body := "# doc\n```lang\ninner\n```\nmore\n"
	if framing, got := classifyFencedPayload("```markdown\n" + body + "```"); framing != fenceFramingAmbiguous || got != "" {
		t.Fatalf("``` wrapper: framing %v body %q, want ambiguous and no bytes", framing, got)
	}
	framing, got := classifyFencedPayload("````markdown\n" + body + "````")
	if framing != fenceFramingComplete || got != body {
		t.Fatalf("```` wrapper: framing %v body %q, want the whole body", framing, got)
	}
	if !strings.Contains(fencedRetryNote(fenceFramingAmbiguous, "markdown"), "four backticks") {
		t.Fatalf("the ambiguous retry note does not give the four-backtick path")
	}
}

func TestFramingClassificationsAreTruthful(t *testing.T) {
	for _, c := range []struct {
		f    fenceFraming
		want string
	}{
		{fenceFramingComplete, "complete"},
		{fenceFramingUnterminated, "fence opened, never closed"},
		{fenceFramingAbsent, "no fence at all"},
		{fenceFramingAmbiguous, "more than one fence closes the block"},
	} {
		if got := c.f.String(); got != c.want {
			t.Errorf("%v: %q, want %q", int(c.f), got, c.want)
		}
	}
}

// --- inline and fetched must agree ----------------------------------------

// The sub-call already refused everything the classifier calls incomplete.
// This pins that the two paths ask the SAME question, so a later change to
// one cannot drift from the other.
func TestInlineAndFetchedShareFramingSemantics(t *testing.T) {
	payloads := []string{
		"```lang\nBODY\n```",
		"```lang\nBODY",
		"BODY\n",
		"",
	}
	for _, p := range payloads {
		framing, body := classifyFencedPayload(p)
		fetched := extractFencedContent(p)
		if (framing == fenceFramingComplete) != (fetched != "") {
			t.Errorf("%q: inline says %v, fetched-path extraction says %q",
				p, framing, fetched)
		}
		if framing == fenceFramingComplete && body != fetched {
			t.Errorf("%q: inline body %q != fetched body %q", p, body, fetched)
		}
	}
}

// --- the failure class, end to end ----------------------------------------

// stubMarkerModel serves replies chosen by what the request contains, so the
// test can prove the model only corrects itself AFTER it receives the
// bounded recovery -- not merely that a retry happened.
func stubMarkerModel(t *testing.T, marker, before, after string, seen *int, sawMarker *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		*seen++
		reply := before
		if strings.Contains(string(body), marker) {
			*sawMarker = true
			reply = after
		}
		writeStubSSE(w, reply, 10)
	}))
}

// --- the sub-call refuses the same shapes, and is bounded -----------------

// An unframed reply is not a block: the fetch charges the attempt, retries
// within the session allowance, and errors rather than returning bytes.
func TestFetchRefusesAnUnframedReply(t *testing.T) {
	calls := 0
	srv := stubInference(t, []string{"AAAA\nBBBB\nCCC"}, 7, &calls)
	defer srv.Close()
	t.Setenv("ATLAS_LLAMA_URL", srv.URL)

	ctx := &AgentContext{}
	got, err := fetchFencedContent(ctx, `{"type":"tool_call"}`, "a.txt")
	if err == nil {
		t.Fatalf("an unframed reply was accepted as a file: %q", got)
	}
	if got != "" {
		t.Fatalf("bytes returned despite the error: %q", got)
	}
	if calls != maxFencedFailuresPerPath {
		t.Fatalf("attempts %d, want the session allowance %d", calls, maxFencedFailuresPerPath)
	}
	if ctx.FencedCalls != calls {
		t.Fatalf("accounting: %d generations charged for %d calls", ctx.FencedCalls, calls)
	}
}

// The correction must be caused by the bounded recovery, not by any retry.
// The stub only emits a framed file once it has SEEN the sub-call's note, so
// a pass here cannot come from retrying the same request.
func TestCorrectionFollowsTheBoundedRecoveryAndIsCountedOnce(t *testing.T) {
	seen := 0
	sawMarker := false
	// The marker is the sub-call's own instruction, not a task word.
	const marker = "No JSON, no commentary, no partial file."
	srv := stubMarkerModel(t, marker, "AAAA\nBBBB\nCCC", "```lang\nWHOLE\n```", &seen, &sawMarker)
	defer srv.Close()
	t.Setenv("ATLAS_LLAMA_URL", srv.URL)

	ctx := &AgentContext{}
	got, err := fetchFencedContent(ctx, `{"type":"tool_call"}`, "a.txt")
	if err != nil {
		t.Fatalf("the framed reply after the recovery was refused: %v", err)
	}
	if !sawMarker {
		t.Fatal("the model corrected itself without ever receiving the recovery")
	}
	if got != "WHOLE\n" {
		t.Fatalf("body %q, want the framed file", got)
	}
	if seen != 1 {
		t.Fatalf("the recovery was spent %d times, want exactly once", seen)
	}
	if ctx.FencedCalls != seen {
		t.Fatalf("accounting: %d generations charged for %d calls", ctx.FencedCalls, seen)
	}
}

// The file-content sub-call asks for the contract it describes, and asks about
// one file. Decoding is constrained to one fenced block, which makes most
// replies usable and none empty. Only the target's own contents are restated:
// with another file's contents at the end of the context, a reply can be a
// near copy of that file instead of the one asked for.

// tempting is a complete, valid module for a DIFFERENT file than the one being
// written: the failure mode is copying it, and it parses perfectly.
const tempting = "import json\n\n\ndef load(path):\n    with open(path) as f:\n        return json.load(f)\n\n\n" +
	"def save(path, data):\n    with open(path, 'w') as f:\n        json.dump(data, f)\n"

func fencedRun(t *testing.T, turns []string, reply func(prompt string) string) (*integrityRun, *[]string) {
	t.Helper()
	var subCalls []string
	run := integrityLoop(t, "Write report.py, which prints a summary.",
		map[string]string{"store.py": tempting}, turns, func(prompt string) string {
			subCalls = append(subCalls, prompt)
			return reply(prompt)
		})
	return run, &subCalls
}

func TestTheSubCallConstrainsDecodingAndRestatesOnlyItsTarget(t *testing.T) {
	body := "def main():\n    print('summary')\n\n\nif __name__ == '__main__':\n    main()\n"
	run, subCalls := fencedRun(t, []string{
		toolCall("read_file", map[string]interface{}{"path": "store.py"}),
		toolCall("write_file", map[string]interface{}{"path": "report.py", "content": "@fenced"}),
		`{"type":"done","summary":"wrote report.py"}`,
	}, func(string) string { return "```python\n" + body + "```" })

	if len(*subCalls) != 1 {
		t.Fatalf("fenced sub-calls: %d, want 1", len(*subCalls))
	}
	var req struct {
		Grammar  string `json:"grammar"`
		Messages []struct{ Role, Content string }
	}
	if err := json.Unmarshal([]byte((*subCalls)[0]), &req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.Grammar, "```python\\n") || !strings.Contains(req.Grammar, "line*") {
		t.Errorf("the sub-call did not ask for one fenced python block: %q", req.Grammar)
	}
	for _, m := range req.Messages {
		if strings.HasPrefix(m.Content, "Current contents of store.py") {
			t.Error("the sub-call restated store.py, a file it was not asked to write")
		}
	}
	// The conversation itself is untouched: the read_file result that showed
	// store.py is still there. Only the appended restatement is dropped.
	if !strings.Contains((*subCalls)[0], "def save(path, data)") {
		t.Error("the sub-call lost the conversation's own tool results")
	}
	if got, _ := run.disk(t, "report.py"); got != body {
		t.Errorf("report.py bytes:\n got %q\nwant %q", got, body)
	}
	if got, _ := run.disk(t, "store.py"); got != tempting {
		t.Error("store.py changed")
	}
}

// The file being rewritten is still restated: that one is the subject of the
// request, and dropping it would remove the context the rewrite needs.
func TestTheTargetsOwnContentsAreStillRestated(t *testing.T) {
	first := "def main():\n    print('summary')\n"
	updated := "def main():\n    print('summary')\n    print('done')\n"
	var subCalls []string
	run := integrityLoop(t, "Write report.py, which prints a summary, then add a done line.",
		map[string]string{"store.py": tempting}, []string{
			toolCall("write_file", map[string]interface{}{"path": "report.py", "content": first}),
			toolCall("read_file", map[string]interface{}{"path": "report.py"}),
			toolCall("write_file", map[string]interface{}{"path": "report.py", "content": "@fenced"}),
			`{"type":"done","summary":"report.py prints a summary and a done line"}`,
		}, func(prompt string) string {
			subCalls = append(subCalls, prompt)
			return "```python\n" + updated + "```"
		})
	if len(subCalls) != 1 {
		t.Fatalf("fenced sub-calls: %d, want 1", len(subCalls))
	}
	if !strings.Contains(subCalls[0], "Current contents of report.py") {
		t.Error("the target's own restatement was dropped")
	}
	if got, _ := run.disk(t, "report.py"); got != updated {
		t.Errorf("report.py bytes: %q", got)
	}
}

// A tempting reply is the other file, complete and valid. If it arrives, it
// lands as the model's own answer for the target, and the run's checks judge
// it; nothing here silently substitutes a different file's bytes.
func TestACopiedBodyIsNotPassedOffAsTheTarget(t *testing.T) {
	run, _ := fencedRun(t, []string{
		toolCall("read_file", map[string]interface{}{"path": "store.py"}),
		toolCall("write_file", map[string]interface{}{"path": "report.py", "content": "@fenced"}),
		`{"type":"done","summary":"wrote report.py"}`,
	}, func(string) string { return "```python\n" + tempting + "```" })
	got, _ := run.disk(t, "report.py")
	if got != tempting {
		t.Errorf("the delivered bytes are not what the sub-call returned: %q", got)
	}
	if store, _ := run.disk(t, "store.py"); store != tempting {
		t.Error("store.py was modified")
	}
}

// A server that refuses the grammar still gets the free-text request this
// channel has always sent, and the file still lands.
func TestTheSubCallFallsBackWhenTheGrammarIsRefused(t *testing.T) {
	body := "print('summary')\n"
	var sawGrammar, sawPlain int
	run, subCalls := fencedRun(t, []string{
		toolCall("write_file", map[string]interface{}{"path": "report.py", "content": "@fenced"}),
		`{"type":"done","summary":"wrote report.py"}`,
	}, func(prompt string) string {
		if strings.Contains(prompt, `"grammar"`) {
			sawGrammar++
			return "" // as a refusal looks from here: no content
		}
		sawPlain++
		return "```python\n" + body + "```"
	})
	if sawGrammar == 0 || sawPlain == 0 {
		t.Fatalf("grammar attempts %d, plain attempts %d, calls %d", sawGrammar, sawPlain, len(*subCalls))
	}
	if got, _ := run.disk(t, "report.py"); got != body {
		t.Errorf("the fallback attempt did not deliver: %q", got)
	}
}

// The grammar is built from the path's tag and cannot be steered by it.
func TestTheGrammarIsBuiltFromASafeTag(t *testing.T) {
	g := fenceBlockGrammar("py\"; drop\n")
	if strings.Contains(g, "drop\n") || strings.Contains(g, "\"; ") {
		t.Errorf("tag characters reached the grammar unescaped: %q", g)
	}
	if !strings.HasPrefix(g, "root ::= \"````pydrop\\n\"") {
		t.Errorf("unexpected grammar: %q", g)
	}
}

// A Markdown file's own code blocks survive the fence. With a three-backtick
// fence no body line could start with ```, so the file's first code block
// would end it, and the truncated body would parse as complete.
func TestAFileWithACodeBlockSurvivesTheFence(t *testing.T) {
	body := "# Tool\n\nInstall:\n\n```bash\npip install tool\n```\n\nMore docs here.\n"
	g := fenceBlockGrammar("markdown")
	if !strings.HasPrefix(g, "root ::= \"````markdown\\n\" line* \"````\"") {
		t.Fatalf("the outer fence is not four backticks: %q", g)
	}
	// The line rule: up to three leading backticks, never four.
	if !strings.Contains(g, "\"```\" ( [^`\\n] [^\\n]* )?") {
		t.Fatalf("the line rule does not admit a line starting with ```: %q", g)
	}
	framing, got := classifyFencedPayload("````markdown\n" + body + "````")
	if framing != fenceFramingComplete || got != body {
		t.Fatalf("round trip: framing=%v body=%q, want the whole file", framing, got)
	}
	if !fencedGrammarFits(body) || fencedGrammarFits("intro\n````\nnested\n````\n") {
		t.Error("fencedGrammarFits misjudges which files the fence can carry")
	}
}

// If the fence grammar takes the model's three-backtick closer as a body line,
// the grammar cannot end, and the attempt runs on until the idle watchdog cuts
// it; a stream can even write the model's next tool calls into the file. So a
// code file may end on three backticks.

func TestTheGrammarLetsACodeFileEndOnThreeBackticks(t *testing.T) {
	py := fenceBlockGrammar("python")
	if !strings.Contains(py, "line* ( \"````\" | \"```\" \"\\n\"? )") {
		t.Errorf("a python block cannot end on three backticks: %q", py)
	}
	for _, tag := range []string{"markdown", ""} {
		g := fenceBlockGrammar(tag)
		if !strings.Contains(g, "line* \"````\"\n") || strings.Contains(g, "| \"```\" \"\\n\"?") {
			t.Errorf("tag %q must keep only the four-backtick closer: %q", tag, g)
		}
	}
}

func TestCloseShortFence(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"a closer of three becomes four", "````python\nprint(1)\n```", "````python\nprint(1)\n````"},
		{"with the newline after it", "````python\nprint(1)\n```\n", "````python\nprint(1)\n````\n"},
		{"interior ``` lines stay", "````python\ns = '''\n```\nx\n```\n'''\n```", "````python\ns = '''\n```\nx\n```\n'''\n````"},
		{"a four-backtick closer is left alone", "````python\nx\n````", "````python\nx\n````"},
		{"a three-backtick opener is left alone", "```python\nx\n```", "```python\nx\n```"},
		{"no closing line at all", "````python\nx = 1\n", "````python\nx = 1\n"},
		{"text after the closer", "````python\nx\n```\n{\"type\":\"done\"}", "````python\nx\n```\n{\"type\":\"done\"}"},
		{"a closed block with a stray ``` after it", "````python\nx\n````\n```", "````python\nx\n````\n```"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := closeShortFence(c.in); got != c.want {
				t.Errorf("got %q\nwant %q", got, c.want)
			}
		})
	}
}

// grammarAwareServer answers a request that carries a grammar with one reply and
// any other with another. It returns its address and what it has seen so far.
func grammarAwareServer(t *testing.T, grammarReply, retryReply string) (string, func() []bool) {
	t.Helper()
	var mu sync.Mutex
	var used []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isChatCompletion(r) {
			http.NotFound(w, r)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, withGrammar := body["grammar"]
		mu.Lock()
		used = append(used, withGrammar)
		mu.Unlock()
		if withGrammar {
			serveReply(w, grammarReply)
			return
		}
		serveReply(w, retryReply)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []bool {
		mu.Lock()
		defer mu.Unlock()
		return append([]bool(nil), used...)
	}
}

// A code file's block ends on a three-backtick closer, so the grammar attempt
// is used. A Markdown file keeps the four-backtick closer: its own ``` lines
// are content, so a reply that ends on one is not a finished block, and a
// free-text retry fetches the file.
func TestWhichCloserEndsTheGrammarAttempt(t *testing.T) {
	for _, c := range []struct {
		name, path               string
		grammarReply, retryReply string
		wantGrammar              []bool // per request, whether it carried a grammar
		want                     string
	}{
		{"code file", "app.py", "````python\nprint(1)\n```", "````python\nprint(2)\n````",
			[]bool{true}, "print(1)\n"},
		{"markdown file", "notes.md", "````markdown\n# Title\n```", "````markdown\n# Title\n```sh\nls\n```\n````",
			[]bool{true, false}, "# Title\n```sh\nls\n```\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			url, used := grammarAwareServer(t, c.grammarReply, c.retryReply)
			ctx := NewAgentContext(t.TempDir(), Tier2Medium)
			ctx.InferenceURL = url
			ctx.Ctx = context.Background()
			call, _ := json.Marshal(map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": c.path, "content": "@fenced"}})

			content, err := fetchFencedContent(ctx, string(call), c.path)
			if err != nil {
				t.Fatalf("fetch %s: %v", c.path, err)
			}
			if got := used(); fmt.Sprint(got) != fmt.Sprint(c.wantGrammar) {
				t.Errorf("requests carried a grammar as %v, want %v", got, c.wantGrammar)
			}
			if content != c.want {
				t.Errorf("content %q, want %q", content, c.want)
			}
		})
	}
}
