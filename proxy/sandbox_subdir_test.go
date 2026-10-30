package main

import (
	"strings"
	"testing"
)

func TestValidSandboxSubdir(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"a/b", false},
		{`a\b`, false},
		{".", false},
		{"..", false},
		{"pane/", false}, // filepath.Clean("pane/") is "pane"
		{"a b", false},
		{"a\nb", false},
		{"a\x01b", false},
		{"café", false}, // a letter outside ASCII
		{"", false},
		{strings.Repeat("a", 65), false},
		{strings.Repeat("a", 64), true},
		{"_reliability", true},
		{".demo-v3-1", true},
		{"sub1", true},
	}
	for _, tc := range cases {
		if got := validSandboxSubdir(tc.name); got != tc.ok {
			t.Errorf("validSandboxSubdir(%q) = %v, want %v", tc.name, got, tc.ok)
		}
	}
}
