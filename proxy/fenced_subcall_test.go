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

// --- inline and fetched must agree ----------------------------------------

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

// If the fence grammar takes the model's three-backtick closer as a body line,
// the grammar cannot end, and the attempt runs on until the idle watchdog cuts
// it; a stream can even write the model's next tool calls into the file. So a
// code file may end on three backticks.

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
