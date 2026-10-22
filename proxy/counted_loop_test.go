package main

import (
	"fmt"
	"strings"
	"testing"
)

// A prose reply that loops while it counts ("29. I'll check planning.py's
// end. 30. I'll check ...") is cut. A file body that counts is not: the
// masked count runs only on a text or done reply.

// loopCutAt feeds text through streamLooping at the stream's own cadence (a
// check every 200 characters after the first 600) and returns the length at
// which it cut, or -1.
func loopCutAt(text string) int {
	var buf strings.Builder
	last := 0
	for i := 0; i < len(text); i += 7 {
		end := i + 7
		if end > len(text) {
			end = len(text)
		}
		buf.WriteString(text[i:end])
		if buf.Len() > 600 && buf.Len()-last > 200 {
			last = buf.Len()
			if streamLooping(buf.String()) {
				return buf.Len()
			}
		}
	}
	return -1
}

func proseReply(body string) string {
	return `{"type":"text","content":"` + jsonStringBody(body) + `"}`
}

func writeFileReply(path, body string) string {
	return `{"type":"tool_call","name":"write_file","args":{"path":"` + path +
		`","content":"` + jsonStringBody(body) + `"}}`
}

func counted(format string, from, to int) string {
	var sb strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&sb, format, i)
	}
	return sb.String()
}

func TestAProseReplyThatLoopsWhileItCountsIsCut(t *testing.T) {
	loop := proseReply("I have analyzed the code. The tie-breaking happens when the best plan is selected. " +
		counted("%d. I'll check `planning.py`'s end. ", 1, 440))
	at := loopCutAt(loop)
	if at < 0 || at > 3000 {
		t.Fatalf("the counted loop was cut at %d of %d chars, want a cut within 3000", at, len(loop))
	}
	done := `{"type":"done","summary":"` + jsonStringBody(counted("Step %d: I will check the output again. ", 1, 200)) + `"}`
	if loopCutAt(done) < 0 {
		t.Error("a done summary that loops while it counts was not cut")
	}
}

func TestAFileBodyThatCountsIsNotCut(t *testing.T) {
	csv := "id,name\n" + counted("row %d,foo\n", 1, 200)
	for name, stream := range map[string]string{
		"csv rows":         writeFileReply("data.csv", csv),
		"numbered asserts": writeFileReply("test_f.py", "from f import f\n\n"+counted("assert f(%d) == %[1]d\n", 1, 200)),
		"migration list":   writeFileReply("MIGRATIONS.txt", counted("%04d_add_column.sql applied\n", 1, 150)),
		"fenced csv body":  "````csv\n" + csv + "````\n",
		// The tool named in "type": models do send this shape.
		"tool named in type": `{"type":"write_file","path":"data.csv","content":"` + jsonStringBody(csv) + `"}`,
		// Keys in another order: the body streams before "tool_call" does.
		"args before type": `{"name":"write_file","args":{"path":"data.csv","content":"` + jsonStringBody(csv) + `"},"type":"tool_call"}`,
	} {
		if at := loopCutAt(stream); at >= 0 {
			t.Errorf("%s: a file body that counts was cut at %d chars", name, at)
		}
	}
}

// variedIntro is a paragraph of more than 600 characters with no repeated
// sentence, so only what follows it can meet a loop rule.
var variedIntro = "I read the three files in the project and traced how the plan scorer picks a winner. " +
	"The scorer adds points for each property it checks, and the selector sorts by that score. " +
	"When two plans tie, the selector keeps whichever one came first in the candidate list. " +
	"That order comes from the sampler, which returns plans in the order the model produced them. " +
	"So a tie is broken by generation order, not by any property of the plans themselves. " +
	"I ran the tests before and after each change and compared the outputs line by line. " +
	"Here is what each check showed, in the order I ran them: "

func TestProseThatCountsWithoutLoopingIsNotCut(t *testing.T) {
	for name, reply := range map[string]string{
		"numeric table":      proseReply("Results:\n" + counted("| %d | 7 | 49 |\n", 1, 80)),
		"four counted steps": proseReply(variedIntro + counted("Step %d: run the test again and read the output. ", 1, 4) + "Done."),
		"distinct numbered steps": proseReply(variedIntro + "1. Install the dependencies. 2. Configure the settings. " +
			"3. Build the binary. 4. Run the tests. 5. Check the logs. 6. Deploy the service. " +
			"7. Watch the metrics. 8. Tag the release. 9. Notify the team. 10. Close the ticket."),
		"code block in the reply": proseReply("Here is the table as code:\n```\n" + counted("row %d,foo\n", 1, 200) + "```\n"),
	} {
		if at := loopCutAt(reply); at >= 0 {
			t.Errorf("%s: cut at %d chars", name, at)
		}
	}
}

// The same text with and without counters meets the same rule: identical
// counted rows in a prose reply are cut, as identical uncounted rows already
// were.
func TestCountedRowsInProseMeetTheVerbatimRule(t *testing.T) {
	plain := proseReply("The file holds:\n" + strings.Repeat("Row: foo\n", 100))
	numbered := proseReply("The file holds:\n" + counted("Row %d: foo\n", 1, 100))
	if loopCutAt(plain) < 0 || loopCutAt(numbered) < 0 {
		t.Errorf("plain cut=%d numbered cut=%d, want both cut", loopCutAt(plain), loopCutAt(numbered))
	}
}

// Four counted repeats in an answer are a short list, not a loop, so the
// masked threshold is 5. Checked on the stream as it stands right after the
// fourth repeat, where a check sees the most of them.
func TestFourCountedStepsInProseAreNotALoop(t *testing.T) {
	step := "Step %d: run the test again and read the output. "
	four := `{"type":"text","content":"` + jsonStringBody(variedIntro+counted(step, 1, 4))
	if streamLooping(four) {
		t.Error("four counted steps were cut as a loop")
	}
	six := `{"type":"text","content":"` + jsonStringBody(variedIntro+counted(step, 1, 6))
	if !streamLooping(six) {
		t.Error("six counted steps were not cut")
	}
}
