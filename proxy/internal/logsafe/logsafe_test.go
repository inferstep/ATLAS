package logsafe

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Operator logs carry shape, never bodies. Log lines land in container
// stdout, which is captured and shared, so a line that quotes a content or
// command argument puts model-authored source, or a secret, into that
// record. A length bound is not a policy: it decides how much source leaks,
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
	log.SetOutput(Writer{W: &buf})
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
				1, tool, ArgsSummary(tool, json.RawMessage(args)))
		})
		if strings.Contains(out, sentinelBody) || strings.Contains(out, sentinelSecret) {
			t.Errorf("%s leaked its body into the log: %s", tool, out)
		}
	}
}

// A tool nobody classified yet must not be a hole.
func TestAnUnregisteredToolIsRedactedWholesale(t *testing.T) {
	args := `{"path":"a.py","brand_new_body_field":"` + sentinelBody + `"}`
	got := ArgsSummary("tool_invented_tomorrow", json.RawMessage(args))
	if strings.Contains(got, sentinelBody) {
		t.Fatalf("an unregistered tool leaked: %s", got)
	}
}

// A new field on a KNOWN tool is redacted until it is declared safe, so a
// body-bearing field cannot be added and silently logged.
func TestANewFieldOnAKnownToolIsRedactedUntilDeclaredSafe(t *testing.T) {
	args := `{"path":"a.py","content":"x","future_body":"` + sentinelBody + `"}`
	got := ArgsSummary("write_file", json.RawMessage(args))
	if strings.Contains(got, sentinelBody) {
		t.Fatalf("an undeclared field leaked: %s", got)
	}
}

func TestSafeSummaryKeepsShapeAndProvenance(t *testing.T) {
	body := strings.Repeat("q", 1106)
	got := ArgsSummary("write_file",
		json.RawMessage(`{"path":"pkg/a.py","content":"`+body+`"}`))
	for _, want := range []string{
		`"path":"pkg/a.py"`,          // canonical path, already permitted
		"1106B",                      // body length
		sha256Hex([]byte(body))[:12], // stable content hash
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary dropped %q: %s", want, got)
		}
	}
	if strings.Contains(got, body) {
		t.Fatalf("summary kept the body")
	}
	// The hash is stable across calls.
	if again := ArgsSummary("write_file",
		json.RawMessage(`{"path":"pkg/a.py","content":"`+body+`"}`)); again != got {
		t.Fatalf("summary is not stable: %q vs %q", got, again)
	}
}

func TestSafeSummaryKeepsSelectorAndRangeMetadata(t *testing.T) {
	got := ArgsSummary("structural_edit",
		json.RawMessage(`{"path":"a.py","selector":"function:solve","content":"`+sentinelBody+`"}`))
	if !strings.Contains(got, "function:solve") {
		t.Errorf("selector metadata lost: %s", got)
	}
	got = ArgsSummary("replace_lines",
		json.RawMessage(`{"path":"a.py","start_line":4,"end_line":9,"content":"`+sentinelBody+`"}`))
	if !strings.Contains(got, `"start_line":4`) || !strings.Contains(got, `"end_line":9`) {
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
		got := ArgsSummary("write_file", json.RawMessage(c.args))
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
		log.Printf("[agent] turn=%d EMPTY ARGS — model output: %s", 2, TextSummary(raw))
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
			"a.py", DiagnosticSummary(detail))
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

// The filter reads the record before the escape does: an escaped line break is
// no longer white space, so a credential after it would pass the filter.
func TestCredentialFilteringSeesTheRecordBeforeItsEscape(t *testing.T) {
	out := captureLog(t, func() {
		log.Printf("auth=%s", "bearer\nSUPERSECRETVALUE1234567890")
	})
	if strings.Contains(out, "SUPERSECRETVALUE1234567890") {
		t.Fatalf("the escape ran before the filter: %s", out)
	}
}

// Corpus-driven tests for private-value filtering. Every fixture value
// is synthetic (see tests/fixtures/private_value_fixtures.json).

type pvCase struct {
	Name           string   `json:"name"`
	Input          string   `json:"input"`
	MustNotContain []string `json:"must_not_contain"`
	MustContain    []string `json:"must_contain"`
	MustEqualInput bool     `json:"must_equal_input"`
}

type pvCorpus struct {
	Placeholder   string   `json:"placeholder"`
	Cases         []pvCase `json:"cases"`
	NegativeCases []pvCase `json:"negative_cases"`
}

func loadCorpus(t *testing.T) pvCorpus {
	t.Helper()
	path := filepath.Join("..", "..", "..", "tests", "fixtures",
		"private_value_fixtures.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture corpus missing: %v", err)
	}
	var c pvCorpus
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("fixture corpus unparsable: %v", err)
	}
	return c
}

func TestFilterPrivateValuesCorpus(t *testing.T) {
	corpus := loadCorpus(t)
	if corpus.Placeholder != privateValuePlaceholder {
		t.Fatalf("placeholder drift: corpus %q vs code %q",
			corpus.Placeholder, privateValuePlaceholder)
	}
	for _, c := range corpus.Cases {
		got := filterPrivateValues(c.Input)
		for _, bad := range c.MustNotContain {
			if strings.Contains(got, bad) {
				t.Errorf("%s: %q survived filtering: %q", c.Name, bad, got)
			}
		}
		for _, keep := range c.MustContain {
			if !strings.Contains(got, keep) {
				t.Errorf("%s: context %q lost: %q", c.Name, keep, got)
			}
		}
		if len(c.MustNotContain) > 0 &&
			!strings.Contains(got, privateValuePlaceholder) {
			t.Errorf("%s: no placeholder in output: %q", c.Name, got)
		}
	}
}

func TestFilterPrivateValuesLeavesTheNegativeCorpusUnchanged(t *testing.T) {
	for _, c := range loadCorpus(t).NegativeCases {
		if got := filterPrivateValues(c.Input); got != c.Input {
			t.Errorf("%s: benign input modified: %q -> %q",
				c.Name, c.Input, got)
		}
	}
}

func TestFilteringWriterOnLogger(t *testing.T) {
	var buf bytes.Buffer
	lg := log.New(Writer{W: &buf}, "", 0)
	lg.Printf("turn failed: EXAMPLE_API_TOKEN=not-a-real-token status=500")
	out := buf.String()
	if strings.Contains(out, "not-a-real-token") {
		t.Fatalf("fixture value reached the log sink: %q", out)
	}
	if !strings.Contains(out, "status=500") {
		t.Fatalf("benign context lost: %q", out)
	}
	if !strings.Contains(out, privateValuePlaceholder) {
		t.Fatalf("placeholder missing: %q", out)
	}
}
