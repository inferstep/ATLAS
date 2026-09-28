package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The file-content sub-call asks for the contract it describes, and asks about
// one file.
//
// Measured (stabilization cycle 4, probe M, 98 generations over the 49 captured
// sub-call requests): constraining decoding to one fenced block took clean
// usable replies from 19/49 to 44/49 and empty replies from 27/49 to 0, with
// the median call falling from 60 s to 12 s. With the other file's contents
// still restated at the end of that context, 2 of 49 replies were a near copy
// of THAT file instead of the one asked for; with it dropped, 0 of 49, and the
// usable count was unchanged.

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

// A tempting reply — the other file, complete and valid — is what the removed
// restatement used to produce. If it arrives anyway, it lands as the model's
// own answer for the target, and the run's checks judge it; nothing here
// silently substitutes a different file's bytes.
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

// P-safety/INTEGRITY#5: with a three-backtick fence no body line could start
// with ```, so a Markdown file's first code block ended the file: the grammar
// allowed only the closer there, and the truncated body parsed as complete.
func TestAFileWithACodeBlockSurvivesTheFence(t *testing.T) {
	body := "# Tool\n\nInstall:\n\n```bash\npip install tool\n```\n\nMore docs here.\n"
	g := fenceBlockGrammar("markdown")
	if !strings.HasPrefix(g, "root ::= \"````markdown\\n\" line* \"````\"") {
		t.Fatalf("the outer fence is not four backticks: %q", g)
	}
	// The line rule, mirrored: up to three leading backticks, never four.
	if !strings.Contains(g, "\"```\" ( [^`\\n] [^\\n]* )?") {
		t.Fatalf("the line rule does not admit a line starting with ```: %q", g)
	}
	line := regexp.MustCompile("^(?:[^`][^\n]*|`(?:[^`][^\n]*)?|``(?:[^`][^\n]*)?|```(?:[^`][^\n]*)?)?$")
	for _, l := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if !line.MatchString(l) {
			t.Errorf("the grammar's line rule refuses %q", l)
		}
	}
	framing, got := classifyFencedPayload("````markdown\n" + body + "````")
	if framing != fenceFramingComplete || got != body {
		t.Fatalf("round trip: framing=%v body=%q, want the whole file", framing, got)
	}
	if !fencedGrammarFits(body) || fencedGrammarFits("intro\n````\nnested\n````\n") {
		t.Error("fencedGrammarFits misjudges which files the fence can carry")
	}
}
