package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/itigges22/atlas-proxy/internal/logsafe"
)

// captureLog installs a buffer as the standard logger's sink, through the
// same filtering writer production installs, and returns everything written.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(logsafe.Writer{W: &buf})
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	fn()
	return buf.String()
}

// Every registered tool must be structurally classified.
func TestEveryRegisteredToolIsClassified(t *testing.T) {
	for _, def := range allTools() {
		if !logsafe.Classified(def.Name) {
			t.Errorf("tool %q has no logging classification; declare its safe "+
				"argument fields in internal/logsafe", def.Name)
		}
	}
}

// A logged value keeps no control character but tab and no Unicode line break,
// so it cannot pass for a record or act on a terminal. Sites that already call
// logPath escape the same text twice, which must change nothing.
func TestALoggedValueStaysInsideItsRecord(t *testing.T) {
	for _, c := range []struct{ name, value, want string }{
		{"line feed", "a\nb", `a\nb`},
		{"carriage return", "a\rb", `a\rb`},
		{"backslash", `a\b`, `a\b`},
		{"text that reads as an escape", `a\nb`, `a\nb`},
		{"tab", "a\tb", "a\tb"},
		{"escape character", "a\x1b[0mb", `a\u001b[0mb`},
		{"null", "a\x00b", `a\u0000b`},
		{"delete", "a\x7fb", `a\u007fb`},
		{"control character above ASCII", "a\xc2\x85b", `a\u0085b`},
		{"line and paragraph separators", "a\xe2\x80\xa8b\xe2\x80\xa9c", `a\u2028b\u2029c`},
		{"letter above ASCII", "a\xc3\xa9b", "a\xc3\xa9b"},
		{"byte that is not UTF-8", "a\xffb", "a\xef\xbf\xbdb"},
		{"stack trace", "main.f()\n\tmain.go:1\nmain.main()",
			`main.f()\n` + "\t" + `main.go:1\nmain.main()`},
	} {
		want := "name=" + c.want + " status=500\n"
		once := captureLog(t, func() { log.Printf("name=%s status=500", c.value) })
		if once != want {
			t.Errorf("%s: got %q, want %q", c.name, once, want)
		}
		twice := captureLog(t, func() { log.Printf("name=%s status=500", logPath(c.value)) })
		if twice != want {
			t.Errorf("%s, logPath first: got %q, want %q", c.name, twice, want)
		}
	}
}

// logEvent's finished JSON passes through the writer too. An escape that is
// not valid JSON would make jsonLineWriter read the record as plain text, and
// the record's own fields would be lost.
func TestAJSONRecordStaysWholeWithAControlCharacterInIt(t *testing.T) {
	var buf bytes.Buffer
	oldOut, oldFlags, oldJSON := log.Writer(), log.Flags(), logJSON
	log.SetOutput(logsafe.Writer{W: jsonLineWriter{w: &buf}})
	log.SetFlags(0)
	logJSON = true
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags); logJSON = oldJSON })

	logEvent("warn", "a\x7fb\xc2\x85c", "req-7", nil)

	var rec map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, buf.String())
	}
	if rec["level"] != "warn" || rec["request_id"] != "req-7" || rec["msg"] != "a\x7fb\xc2\x85c" {
		t.Fatalf("the record lost its fields: %v", rec)
	}
}

// filteringWriter is the one place every log line is filtered and escaped, so
// nothing in the proxy may reach stdout or stderr around it.
func TestNothingWritesBesideTheLogFilter(t *testing.T) {
	output := regexp.MustCompile(`.*(os\.Std(out|err)|fmt\.Print|log\.New\(|\.SetOutput\().*`)
	var found []string
	for _, path := range moduleGoFiles(t) {
		if !strings.HasSuffix(path, "_test.go") {
			for _, line := range output.FindAllString(readSourceForTest(t, path), -1) {
				found = append(found, path+": "+strings.TrimSpace(line))
			}
		}
	}
	want := []string{
		"main.go: out := io.Writer(os.Stderr)",
		"main.go: out = jsonLineWriter{w: os.Stderr}",
		"main.go: log.SetOutput(logsafe.Writer{W: out})",
	}
	if !slices.Equal(found, want) {
		t.Errorf("a log output beside filteringWriter:\n got %q\nwant %q", found, want)
	}
}

// Hashes are an operator affordance. They must not be handed to the model or
// to an external client.
func TestHashesAreNotOfferedToTheModel(t *testing.T) {
	body := strings.Repeat("z", 40)
	summary := logsafe.ArgsSummary("write_file",
		json.RawMessage(`{"path":"a.py","content":"`+body+`"}`))
	h := hashBytes([]byte(body))[:12]
	if !strings.Contains(summary, h) {
		t.Fatalf("operator summary lost the hash: %s", summary)
	}
	// The bounce text a model receives is built elsewhere and must not carry it.
	if strings.Contains(fallbackSyntaxRejection("a.py", body, "SyntaxError: x"), h) {
		t.Fatal("a model-visible message carried a content hash")
	}
}

// --- the surface stays closed --------------------------------------------

// Operator log call sites may not hand a body-bearing expression straight to
// the logger; a new one must go through logsafe.ArgsSummary, TextSummary or
// DiagnosticSummary. The test reads the files it lists and fails when one
// of the forbidden expressions is on a log.Printf or logEvent line, or up to
// five lines below one. Walking back stops at a return, a "Warning:" text or
// a NewEnvelope call: those build text the MODEL reads back (it authored the
// source, so quoting its own syntax error tells it nothing new), or the
// /events envelope an external client subscribes to, and may quote a body.
func TestOperatorLogSitesCannotQuoteABody(t *testing.T) {
	forbidden := []string{
		"truncateStr(response",
		"truncateStr(reply",
		"truncateStr(synErr",
		"truncateStr(deliveredCheck",
		"truncateStr(string(parsed.Args)",
		"truncateStr(input.Content",
	}
	// Lines that are logger calls, or continuations of one.
	for _, file := range append(append([]string{"gates.go",
		"gates_asset_lint.go", "gates_background_jobs.go", "shell_wrapper.go",
		"gates_completion.go", "gates_edit_requests.go", "gates_ledger.go", "gates_plan.go",
		"gates_syntax_rejection.go", "gates_workspace_alignment.go",
		"guardrails.go",
		"guardrails_write.go", "guardrails_tool_hints.go", "guardrails_shell.go",
		"guardrails_reply.go", "guardrails_verification_gate.go", "guardrails_summaries.go",
		"main.go"}, agentFiles...), toolFiles...) {
		src := readSourceForTest(t, file)
		lines := strings.Split(src, "\n")
		for i, line := range lines {
			var bad string
			for _, f := range forbidden {
				if strings.Contains(line, f) {
					bad = f
				}
			}
			if bad == "" {
				continue
			}
			// Walk back to the call this argument belongs to.
			call := ""
			for j := i; j >= 0 && j > i-6; j-- {
				if strings.Contains(lines[j], "log.Printf(") || strings.Contains(lines[j], "logEvent(") {
					call = lines[j]
					break
				}
				if strings.Contains(lines[j], "return ") || strings.Contains(lines[j], "Warning:") ||
					strings.Contains(lines[j], "NewEnvelope(") {
					break // not a logger call
				}
			}
			if call != "" {
				t.Errorf("%s:%d hands %s to the logger; route it through a safe summary",
					file, i+1, bad)
			}
		}
	}
}

func readSourceForTest(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
