package fencing

import (
	"strings"
	"testing"
)

// Only a closed fence proves a whole file. Properly framed content that looks
// unfinished is a file; unframed content that looks finished is not evidence.
func TestFramingIsDecidedByFencesAlone(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    Framing
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
			got, body := ClassifyPayload(c.payload)
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
	if framing, got := ClassifyPayload("```markdown\n" + body + "```"); framing != fenceFramingAmbiguous || got != "" {
		t.Fatalf("``` wrapper: framing %v body %q, want ambiguous and no bytes", framing, got)
	}
	framing, got := ClassifyPayload("````markdown\n" + body + "````")
	if framing != fenceFramingComplete || got != body {
		t.Fatalf("```` wrapper: framing %v body %q, want the whole body", framing, got)
	}
	if !strings.Contains(RetryNote(fenceFramingAmbiguous, "markdown"), "four backticks") {
		t.Fatalf("the ambiguous retry note does not give the four-backtick path")
	}
}

func TestFramingClassificationsAreTruthful(t *testing.T) {
	for _, c := range []struct {
		f    Framing
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
		framing, body := ClassifyPayload(p)
		fetched := ExtractContent(p)
		if (framing == fenceFramingComplete) != (fetched != "") {
			t.Errorf("%q: inline says %v, fetched-path extraction says %q",
				p, framing, fetched)
		}
		if framing == fenceFramingComplete && body != fetched {
			t.Errorf("%q: inline body %q != fetched body %q", p, body, fetched)
		}
	}
}

// The grammar is built from the path's tag and cannot be steered by it.
func TestTheGrammarIsBuiltFromASafeTag(t *testing.T) {
	g := BlockGrammar("py\"; drop\n")
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
	g := BlockGrammar("markdown")
	if !strings.HasPrefix(g, "root ::= \"````markdown\\n\" line* \"````\"") {
		t.Fatalf("the outer fence is not four backticks: %q", g)
	}
	// The line rule: up to three leading backticks, never four.
	if !strings.Contains(g, "\"```\" ( [^`\\n] [^\\n]* )?") {
		t.Fatalf("the line rule does not admit a line starting with ```: %q", g)
	}
	framing, got := ClassifyPayload("````markdown\n" + body + "````")
	if framing != fenceFramingComplete || got != body {
		t.Fatalf("round trip: framing=%v body=%q, want the whole file", framing, got)
	}
	if !GrammarFits(body) || GrammarFits("intro\n````\nnested\n````\n") {
		t.Error("GrammarFits misjudges which files the fence can carry")
	}
}

func TestTheGrammarLetsACodeFileEndOnThreeBackticks(t *testing.T) {
	py := BlockGrammar("python")
	if !strings.Contains(py, "line* ( \"````\" | \"```\" \"\\n\"? )") {
		t.Errorf("a python block cannot end on three backticks: %q", py)
	}
	for _, tag := range []string{"markdown", ""} {
		g := BlockGrammar(tag)
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
			if got := CloseShort(c.in); got != c.want {
				t.Errorf("got %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestFencedContentExtraction(t *testing.T) {
	markdown := "# readme\n\n```\nexample\n```\n\ntail line"
	for _, tc := range []struct {
		name, reply, want string
	}{
		{"tagged block in prose", "Here is the file:\n```python\nx = 1\nprint(x)\n```\nDone.", "x = 1\nprint(x)\n"},
		{"bare fence", "```\ny = 2\n```", "y = 2\n"},
		{"c++ tag", "```c++\nint x;\n```", "int x;\n"},
		{"c# tag", "```c#\nint x;\n```", "int x;\n"},
		{"objective-c tag", "```objective-c\nint x;\n```", "int x;\n"},
		// A file's own leading blank lines are its content, not part of the
		// fence line. (Whitespace-only content is not a case: an empty block
		// is what makes the sub-call ask again.)
		{"one leading blank line", "```python\n\nx = 1\n```", "\nx = 1\n"},
		{"three leading blank lines", "```python\n\n\n\nx = 1\n```", "\n\n\nx = 1\n"},
		{"blank lines before a def", "```python\n\n\ndef f():\n    pass\n```", "\n\ndef f():\n    pass\n"},
		{"CRLF fence line", "```python\r\nx = 1\n```", "x = 1\n"},
		// Inside a ``` wrapper, the file's own ``` lines look the same as a
		// second block, so nothing is returned and the sub-call asks again
		// with a four-backtick note. Inside a ```` wrapper the whole file
		// comes back.
		{"file with ``` lines in a ``` wrapper", "```markdown\n" + markdown + "\n```", ""},
		{"file with ``` lines in a ```` wrapper", "````markdown\n" + markdown + "\n````", markdown + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractContent(tc.reply); got != tc.want {
				t.Errorf("ExtractContent(%q) = %q, want %q", tc.reply, got, tc.want)
			}
		})
	}
}

// An inline body reaches disk only when its framing proves it complete: a
// closed fence. A bare body carries no framing either way, so it may have
// stopped mid-emission, and it is resolved through the sub-call instead.
func TestInlineFencedBodyRequiresFramingToProveCompletion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inline   string
		wantBody string // "": falls back to the sub-call
	}{
		{"truncated fence", "```python\nprint(", ""},
		{"opener only", "```python", ""},
		{"complete fence", "```python\nprint(\"hi\")\n```", "print(\"hi\")\n"},
		{"bare body, however finished it looks", "print(\"hi\")\n", ""},
		{"bare body cut mid-emission", "print(", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, _ := ResolveInlineBody(tc.inline)
			if !ok {
				got = ""
			}
			if got != tc.wantBody {
				t.Errorf("resolved %q, want %q", got, tc.wantBody)
			}
		})
	}
}

func FuzzParseFencedReply(f *testing.F) {
	for _, reply := range []string{
		"```lang\nBODY\n```",
		"```\nBODY\n```",
		"```lang\nBODY",
		"BODY\n```",
		"BODY\n",
		"",
		"```lang\n\n```",
		"```markdown\n# doc\n```lang\ninner\n```\nmore\n```",
		"````markdown\n# doc\n```lang\ninner\n```\nmore\n````",
		"Before\n```lang\nBODY\n```\nAfter",
		"```lang\r\nBODY\r\n```\r\n",
		string([]byte{0xff, 0x00, '`', '`', '`'}),
	} {
		f.Add(reply)
	}

	f.Fuzz(func(t *testing.T, reply string) {
		framing, body := parseFencedReply(reply)
		if againFraming, againBody := parseFencedReply(reply); againFraming != framing || againBody != body {
			t.Fatalf("reply %q: parse changed from (%v, %q) to (%v, %q)", reply, framing, body, againFraming, againBody)
		}
		// A complete fence has a body, and nothing else has one.
		if (body != "") != (framing == fenceFramingComplete) {
			t.Fatalf("reply %q: framing %v returned body %q", reply, framing, body)
		}
		// So the extracted content is that body, or empty with no complete fence.
		if extracted := ExtractContent(reply); extracted != body {
			t.Fatalf("reply %q: framing %v, body %q, extracted %q", reply, framing, body, extracted)
		}
		if len(reply) <= 4096 {
			// Prefix each body line so arbitrary bytes cannot form a closer.
			wantBody := "x" + strings.ReplaceAll(reply, "\n", "\nx") + "\n"
			wrapped := "````lang\n" + wantBody + "````"
			gotFraming, gotBody := parseFencedReply(wrapped)
			if gotFraming != fenceFramingComplete || gotBody != wantBody {
				t.Fatalf("wrapped reply %q: framing %v, body %q; want complete, %q", reply, gotFraming, gotBody, wantBody)
			}
		}
	})
}
