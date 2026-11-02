package logsafe

import (
	"io"
	"regexp"
	"strings"
)

// Private-value filtering: masks values that look like credentials
// before they reach a serialized sink. The proxy installs this on the
// standard logger's output (one choke point covers every log.Printf),
// so an error that happens to embed an env assignment or a header
// never lands in the log verbatim.
//
// The pattern spec is shared with the Python services via the fixture
// corpus at tests/fixtures/private_value_fixtures.json — change the
// patterns here and there together; the contract test runs the corpus
// against every implementation.
//
// Patterns are deliberately conservative (assignment/header/key-block
// shapes with secret-ish key names) so ordinary log content —
// "timeout=30", token counts, health URLs — passes through untouched.

const privateValuePlaceholder = "[FILTERED]"

var privateValuePatterns = []*regexp.Regexp{
	// KEY=value / key: value / "key": "value" assignments where the key
	// smells like a credential. Value part is masked, key kept.
	regexp.MustCompile(`(?i)([A-Z0-9_.-]{0,64}(?:api[_-]?key|apikey|token|secret|password|passwd|credential|access[_-]?key)[A-Z0-9_.-]{0,64}["']?\s*[=:]\s*["']?)([^\s"',;&]+)`),
	// Authorization / bearer values.
	regexp.MustCompile(`(?i)(bearer\s+)([A-Za-z0-9._~+/=-]+)`),
	// URL userinfo passwords: scheme://user:pass@host
	regexp.MustCompile(`(://[^/:@\s]{0,64}:)([^@\s]{1,256})(@)`),
	// Private-key blocks (any BEGIN ... PRIVATE KEY variant), body inclusive.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]{0,40}PRIVATE KEY-----.*?-----END [A-Z ]{0,40}PRIVATE KEY-----`),
}

// filterPrivateValues masks credential-shaped substrings in s.
func filterPrivateValues(s string) string {
	// Key-block pattern replaces the whole match; assignment patterns
	// keep the key and mask the value.
	s = privateValuePatterns[3].ReplaceAllString(s, privateValuePlaceholder)
	s = privateValuePatterns[0].ReplaceAllString(s, "${1}"+privateValuePlaceholder)
	s = privateValuePatterns[1].ReplaceAllString(s, "${1}"+privateValuePlaceholder)
	s = privateValuePatterns[2].ReplaceAllString(s, "${1}"+privateValuePlaceholder+"${3}")
	return s
}

// Writer applies the filter to every write — installed as the
// standard logger's output in main(), so all proxy log lines pass
// through it. Line-buffered writes from log.Printf arrive whole, so
// per-write filtering is sound for the standard logger. Each write is
// one record, so all of it before its final newline goes through
// escapeControls.
type Writer struct {
	W io.Writer
}

func (f Writer) Write(p []byte) (int, error) {
	filtered := filterPrivateValues(string(p))
	record := strings.TrimSuffix(filtered, "\n")
	if _, err := f.W.Write([]byte(escapeControls(record) + filtered[len(record):])); err != nil {
		return 0, err
	}
	// Report the original length so log.Printf never sees a short write.
	return len(p), nil
}
