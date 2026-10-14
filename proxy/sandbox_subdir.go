package main

import "strings"

// Characters a sandbox_subdir may contain. The name is this list, not a
// list of separators to refuse: ASCII letters, digits, dot, underscore, dash.
const sandboxSubdirChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-"

const sandboxSubdirDetail = `sandbox_subdir: a single bare directory name is required (ASCII letters, digits, dot, underscore, and dash; not "." or ".."; at most 64 characters)`

// validSandboxSubdir reports whether name is an allowed sandbox_subdir.
// Empty, ".", "..", and anything longer than 64 are refused.
func validSandboxSubdir(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !strings.ContainsRune(sandboxSubdirChars, c) {
			return false
		}
	}
	return true
}
