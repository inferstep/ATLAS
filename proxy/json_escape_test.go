package main

import (
	"strings"
	"testing"
)

// The salvaged text of a cut answer decodes a \uXXXX escape as encoding/json
// does, a surrogate pair included, and does not keep the letters "uXXXX".
func TestTheSalvagedTextDecodesUnicodeEscapes(t *testing.T) {
	padding := strings.Repeat(" padding to clear the length floor.", 8)
	for _, c := range []struct {
		name, escaped, want string
	}{
		{"a letter", `caf\u00e9`, "caf\u00e9"},
		{"a surrogate pair", `smile \ud83d\ude00`, "smile \U0001F600"},
		{"a surrogate alone", `\ud83d!`, "\uFFFD!"},
		{"an escape the cut leaves short", `cut \u00`, "cut u00"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := recoverTruncatedText(`{"type":"text","content":"` + c.escaped + padding)
			if !ok || !strings.HasPrefix(got, c.want) {
				t.Errorf("salvaged %q; want it to start with %q", got, c.want)
			}
		})
	}
}
