package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Under the four-backtick fence grammar the model's three-backtick closer was
// a body line, so the grammar refused to end, and the attempt ran on until the
// idle watchdog cut it. Smoke run on 4403ae8 (2026-09-28): 14 of 29 fenced
// writes waited that way, median 53 s; one stream went on to write the model's
// next tool calls into the file. A code file may now end on three backticks.

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
		if got := closeShortFence(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// fetchWithGrammarReply runs fetchFencedContent for path against a server whose
// grammar-constrained attempt returns grammarReply and whose free-text retry
// returns retryReply. It returns the content and, per request, whether the
// request carried a grammar.
func fetchWithGrammarReply(t *testing.T, path, grammarReply, retryReply string) (string, []bool) {
	t.Helper()
	var mu sync.Mutex
	var used []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
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
			serveFencedBlock(w, grammarReply)
			return
		}
		serveFencedBlock(w, retryReply)
	}))
	defer srv.Close()
	ctx := NewAgentContext(t.TempDir(), Tier2Medium)
	ctx.InferenceURL = srv.URL
	ctx.Ctx = context.Background()
	call, _ := json.Marshal(map[string]interface{}{"type": "tool_call", "name": "write_file",
		"args": map[string]string{"path": path, "content": "@fenced"}})
	content, err := fetchFencedContent(ctx, string(call), path)
	if err != nil {
		t.Fatalf("fetch %s: %v", path, err)
	}
	mu.Lock()
	defer mu.Unlock()
	return content, append([]bool(nil), used...)
}

func TestAThreeBacktickCloserEndsTheGrammarAttempt(t *testing.T) {
	content, used := fetchWithGrammarReply(t, "app.py",
		"````python\nprint(1)\n```", "````python\nprint(2)\n````")
	if len(used) != 1 || !used[0] {
		t.Fatalf("want one grammar-constrained request, got %v", used)
	}
	if content != "print(1)\n" {
		t.Errorf("content %q, want the grammar attempt's file", content)
	}
}

// A Markdown file keeps the four-backtick closer: its own ``` lines are
// content, so a reply that ends on one is not a finished block.
func TestAMarkdownFileKeepsTheFourBacktickCloser(t *testing.T) {
	content, used := fetchWithGrammarReply(t, "notes.md",
		"````markdown\n# Title\n```", "````markdown\n# Title\n```sh\nls\n```\n````")
	if len(used) != 2 || !used[0] || used[1] {
		t.Fatalf("want the grammar attempt refused and a free-text retry, got %v", used)
	}
	if content != "# Title\n```sh\nls\n```\n" {
		t.Errorf("content %q, want the retry's whole file", content)
	}
}
