package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"strings"
	"testing"
)

// Operator logs carry shape, never bodies.
//
// The sealed Stage-A acquisition wrote 353 agent trace lines, 291 of which
// carried a content or command argument, each up to 200 bytes of
// model-authored source. Those lines land in container stdout, which an
// acquisition captures into its evidence archive -- so source fragments
// become evidence-resident and get sealed and shared. The bound was a
// character count, which is not a policy: it decides how MUCH source leaks,
// not whether any does.
//
// Every assertion below drives the real logger and searches its real output
// for a sentinel that went in through the real argument shapes.

const (
	sentinelBody   = "ZZQBODYZZ_5f1a7c"
	sentinelSecret = "ZZQSECRETZZ_a31b90"
)

// captureLog installs a buffer as the standard logger's sink, through the
// same filtering writer production installs, and returns everything written.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(filteringWriter{w: &buf})
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	fn()
	return buf.String()
}

// bodyBearingCalls is one call per registered tool that can carry a body,
// command, or model-authored text, with the sentinel in every such field.
func bodyBearingCalls() map[string]string {
	return map[string]string{
		"write_file":      `{"path":"a.py","content":"` + sentinelBody + `"}`,
		"edit_file":       `{"path":"a.py","old_str":"` + sentinelBody + `","new_str":"` + sentinelBody + `x"}`,
		"insert_after":    `{"path":"a.py","line":3,"content":"` + sentinelBody + `"}`,
		"replace_lines":   `{"path":"a.py","start_line":1,"end_line":2,"expected_first_line":"` + sentinelBody + `","expected_last_line":"` + sentinelBody + `","content":"` + sentinelBody + `"}`,
		"structural_edit": `{"path":"a.py","selector":"function:f","content":"` + sentinelBody + `"}`,
		"run_command":     `{"command":"echo ` + sentinelSecret + `","cwd":"/workspace"}`,
		"run_background":  `{"command":"echo ` + sentinelSecret + `","cwd":"/workspace"}`,
		"search_files":    `{"pattern":"` + sentinelBody + `","path":"."}`,
		"find_file":       `{"pattern":"` + sentinelBody + `","path":"."}`,
	}
}

func TestNoBodyBearingToolLeaksItsBodyThroughTheLogger(t *testing.T) {
	for tool, args := range bodyBearingCalls() {
		out := captureLog(t, func() {
			log.Printf("[agent] turn=%d type=tool_call name=%s args=%s",
				1, tool, safeArgsSummary(tool, json.RawMessage(args)))
		})
		if strings.Contains(out, sentinelBody) || strings.Contains(out, sentinelSecret) {
			t.Errorf("%s leaked its body into the log: %s", tool, out)
		}
	}
}

// A tool nobody classified yet must not be a hole.
func TestAnUnregisteredToolIsRedactedWholesale(t *testing.T) {
	args := `{"path":"a.py","brand_new_body_field":"` + sentinelBody + `"}`
	got := safeArgsSummary("tool_invented_tomorrow", json.RawMessage(args))
	if strings.Contains(got, sentinelBody) {
		t.Fatalf("an unregistered tool leaked: %s", got)
	}
}

// A new field on a KNOWN tool is redacted until it is declared safe, so a
// body-bearing field cannot be added and silently logged.
func TestANewFieldOnAKnownToolIsRedactedUntilDeclaredSafe(t *testing.T) {
	args := `{"path":"a.py","content":"x","future_body":"` + sentinelBody + `"}`
	got := safeArgsSummary("write_file", json.RawMessage(args))
	if strings.Contains(got, sentinelBody) {
		t.Fatalf("an undeclared field leaked: %s", got)
	}
}

// Every registered tool must be structurally classified.
func TestEveryRegisteredToolIsClassified(t *testing.T) {
	for _, def := range allTools() {
		if _, ok := toolSafeArgFields[def.Name]; !ok {
			t.Errorf("tool %q has no logging classification; declare its safe "+
				"argument fields in toolSafeArgFields", def.Name)
		}
	}
}

// --- what must survive ----------------------------------------------------

func TestSafeSummaryKeepsShapeAndProvenance(t *testing.T) {
	body := strings.Repeat("q", 1106)
	got := safeArgsSummary("write_file",
		json.RawMessage(`{"path":"pkg/a.py","content":"`+body+`"}`))
	for _, want := range []string{
		`"path":"pkg/a.py"`,          // canonical path, already permitted
		"1106B",                      // body length
		hashBytes([]byte(body))[:12], // stable content hash
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary dropped %q: %s", want, got)
		}
	}
	if strings.Contains(got, body) {
		t.Fatalf("summary kept the body")
	}
	// The hash is stable across calls.
	if again := safeArgsSummary("write_file",
		json.RawMessage(`{"path":"pkg/a.py","content":"`+body+`"}`)); again != got {
		t.Fatalf("summary is not stable: %q vs %q", got, again)
	}
}

func TestSafeSummaryKeepsSelectorAndRangeMetadata(t *testing.T) {
	got := safeArgsSummary("structural_edit",
		json.RawMessage(`{"path":"a.py","selector":"function:solve","content":"`+sentinelBody+`"}`))
	if !strings.Contains(got, "function:solve") {
		t.Errorf("selector metadata lost: %s", got)
	}
	got = safeArgsSummary("replace_lines",
		json.RawMessage(`{"path":"a.py","start_line":4,"end_line":9,"content":"`+sentinelBody+`"}`))
	if !strings.Contains(got, "4") || !strings.Contains(got, "9") {
		t.Errorf("range metadata lost: %s", got)
	}
	if strings.Contains(got, sentinelBody) {
		t.Fatalf("body survived: %s", got)
	}
}

func TestSafeSummaryHandlesDegenerateArgs(t *testing.T) {
	for _, c := range []struct{ name, args string }{
		{"empty", ``},
		{"null", `null`},
		{"not an object", `["` + sentinelBody + `"]`},
		{"malformed", `{"path":"a.py","content":`},
	} {
		got := safeArgsSummary("write_file", json.RawMessage(c.args))
		if strings.Contains(got, sentinelBody) {
			t.Errorf("%s leaked: %s", c.name, got)
		}
		if got == "" {
			t.Errorf("%s produced no diagnostic at all", c.name)
		}
	}
}

// --- raw model output, syntax diagnostics -------------------------------

func TestRawModelOutputIsSummarisedNotQuoted(t *testing.T) {
	raw := "some reasoning then " + sentinelBody + " and more"
	out := captureLog(t, func() {
		log.Printf("[agent] turn=%d EMPTY ARGS — model output: %s", 2, safeTextSummary(raw))
	})
	if strings.Contains(out, sentinelBody) {
		t.Fatalf("raw model output leaked: %s", out)
	}
	if !strings.Contains(out, "chars") {
		t.Fatalf("summary lost the size: %s", out)
	}
}

func TestSyntaxDiagnosticsCannotLeakTheOffendingSourceLine(t *testing.T) {
	// Real checker detail quotes the source line back.
	detail := "SyntaxError: invalid syntax (line 16)\n    " + sentinelBody + "\n    ^"
	out := captureLog(t, func() {
		log.Printf("[write_file] fallback content for %s failed syntax gate: %s",
			"a.py", safeDiagnosticSummary(detail))
	})
	if strings.Contains(out, sentinelBody) {
		t.Fatalf("a syntax diagnostic leaked the source line: %s", out)
	}
	if !strings.Contains(out, "SyntaxError") {
		t.Fatalf("the useful classification was dropped: %s", out)
	}
}

// --- defence in depth -----------------------------------------------------

func TestCredentialFilteringStillApplies(t *testing.T) {
	out := captureLog(t, func() {
		log.Printf("api_key=%s", "SUPERSECRETVALUE1234567890")
	})
	if strings.Contains(out, "SUPERSECRETVALUE1234567890") {
		t.Fatalf("credential filtering regressed: %s", out)
	}
}

// Hashes are an operator affordance. They must not be handed to the model or
// to an external client.
func TestHashesAreNotOfferedToTheModel(t *testing.T) {
	body := strings.Repeat("z", 40)
	summary := safeArgsSummary("write_file",
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
// the logger. A new one must go through safeArgsSummary, safeTextSummary or
// safeDiagnosticSummary, or be added to the model-visible carve-out below
// with a reason.
//
// modelVisibleCarveOut names the three places a body-derived string is
// deliberately kept: they are not logs. Two build text the MODEL reads back
// (it authored the source, so quoting its own syntax error tells it nothing
// new), and one is the /events envelope an external client subscribes to,
// whose schema is out of scope for this change.
var modelVisibleCarveOut = map[string]string{
	"gates.go:v3CandidateRegression": "model-visible rejection text",
	"tools.go:preflightWarnedWrite":  "model-visible ToolResult warning",
	"agent.go:EvtToolCall":           "/events SSE envelope, schema unchanged",
}

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
	for _, file := range []string{"agent.go", "tools.go", "gates.go", "guardrails.go", "main.go"} {
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

func TestTheCarveOutIsSmallAndNamed(t *testing.T) {
	if len(modelVisibleCarveOut) != 3 {
		t.Fatalf("the carve-out changed size (%d); every entry needs a reason",
			len(modelVisibleCarveOut))
	}
	for k, v := range modelVisibleCarveOut {
		if v == "" {
			t.Errorf("%s has no reason recorded", k)
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
