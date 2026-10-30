package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

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
					serveReply(w, c.grammarReply)
					return
				}
				serveReply(w, c.retryReply)
			}))
			defer srv.Close()
			ctx := NewAgentContext(t.TempDir(), Tier2Medium)
			ctx.InferenceURL = srv.URL
			ctx.Ctx = context.Background()
			call, _ := json.Marshal(map[string]interface{}{"type": "tool_call", "name": "write_file",
				"args": map[string]string{"path": c.path, "content": "@fenced"}})

			content, err := fetchFencedContent(ctx, string(call), c.path)
			if err != nil {
				t.Fatalf("fetch %s: %v", c.path, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if fmt.Sprint(used) != fmt.Sprint(c.wantGrammar) {
				t.Errorf("requests carried a grammar as %v, want %v", used, c.wantGrammar)
			}
			if content != c.want {
				t.Errorf("content %q, want %q", content, c.want)
			}
		})
	}
}
