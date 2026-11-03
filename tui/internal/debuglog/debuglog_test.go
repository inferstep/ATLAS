package debuglog

import (
	"os"
	"path/filepath"
	"testing"
)

// "off" is the documented opt-out and the Python wrapper strips it before
// exec. Anyone running the binary directly bypasses that, and Open
// used to take the word as a filename — `ATLAS_TUI_LOG=off atlas-tui` left a
// file called "off" in the working directory (one got committed that way).
func TestOpenTreatsOffAsDisabled(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	for _, val := range []string{"off", "OFF", " off "} {
		closer, err := Open(val)
		if err != nil {
			t.Fatalf("Open(%q) errored: %v", val, err)
		}
		if closer != nil {
			closer()
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("Open(%q) created %v, want no file", val, names)
		}
	}
}

func TestOpenStillWritesToARealPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.log")
	closer, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if closer != nil {
		closer()
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected a log at %s: %v", path, err)
	}
}
