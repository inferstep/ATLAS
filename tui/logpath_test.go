package main

import (
	"os"
	"path/filepath"
	"testing"
)

// "off" is the documented opt-out and the Python wrapper strips it before
// exec. Anyone running the binary directly bypasses that, and initDebugLog
// used to take the word as a filename — `ATLAS_TUI_LOG=off atlas-tui` left a
// file called "off" in the working directory (one got committed that way).
func TestInitDebugLogTreatsOffAsDisabled(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	for _, val := range []string{"off", "OFF", " off "} {
		closer, err := initDebugLog(val)
		if err != nil {
			t.Fatalf("initDebugLog(%q) errored: %v", val, err)
		}
		if closer != nil {
			closer()
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("initDebugLog(%q) created %v, want no file", val, names)
		}
	}
}

func TestInitDebugLogStillWritesToARealPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.log")
	closer, err := initDebugLog(path)
	if err != nil {
		t.Fatalf("initDebugLog: %v", err)
	}
	if closer != nil {
		closer()
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected a log at %s: %v", path, err)
	}
}
