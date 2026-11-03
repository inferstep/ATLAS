package projectcontext

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestSamplePlanContextPicksUpPriorityFiles(t *testing.T) {
	dir := t.TempDir()
	// Lay down a typical flask app shape.
	if err := os.WriteFile(filepath.Join(dir, "app.py"),
		[]byte("from flask import Flask\napp = Flask(__name__)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "index.html"),
		[]byte("<html><body>hi</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A noisy unrelated file that shouldn't appear.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"),
		[]byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := Sample(dir, 6, 2000)
	if _, ok := got["app.py"]; !ok {
		t.Errorf("expected app.py in context, got keys %v", slices.Sorted(maps.Keys(got)))
	}
	if _, ok := got["templates/index.html"]; !ok {
		t.Errorf("expected templates/index.html in context, got keys %v", slices.Sorted(maps.Keys(got)))
	}
	if _, ok := got["notes.txt"]; ok {
		t.Errorf("notes.txt leaked into priority context")
	}
}

func TestSamplePlanContextTruncatesLargeFiles(t *testing.T) {
	// File between maxBytes (1000) and the hard-skip ceiling (4×maxBytes
	// = 4000) should pass the size gate and get truncated. Files above
	// 4000 are skipped wholesale to avoid yanking a 50KB README into
	// the planner.
	dir := t.TempDir()
	big := make([]byte, 3000)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	got := Sample(dir, 6, 1000)
	content, ok := got["main.py"]
	if !ok {
		t.Fatal("main.py missing from sampled context")
	}
	// 1000 bytes of body + "\n... (truncated)" marker.
	if len(content) > 1100 {
		t.Errorf("content %d bytes — sampler should truncate to ~1000", len(content))
	}
	if len(content) < 1000 {
		t.Errorf("content %d bytes — sampler shouldn't truncate below maxBytes", len(content))
	}
}

func TestSamplePlanContextSkipsHugeFiles(t *testing.T) {
	// Files >4×maxBytes are skipped wholesale to keep the planner
	// budget small. Verifies the hard-skip ceiling.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"),
		[]byte("from flask import Flask\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	huge := make([]byte, 10_000)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), huge, 0o644); err != nil {
		t.Fatal(err)
	}

	got := Sample(dir, 6, 1000)
	if _, ok := got["README.md"]; ok {
		t.Errorf("10KB README should be skipped at maxBytes=1000")
	}
	if _, ok := got["app.py"]; !ok {
		t.Errorf("small app.py should still be picked up")
	}
}

func TestSamplePlanContextFallsBackToShallowWalk(t *testing.T) {
	// No priority files — sampler should still pick up source files
	// from a shallow read of the working dir.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "weird_entry.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.dat"),
		[]byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := Sample(dir, 6, 2000)
	if _, ok := got["weird_entry.go"]; !ok {
		t.Errorf("expected weird_entry.go in fallback walk, got %v", slices.Sorted(maps.Keys(got)))
	}
	if _, ok := got["ignored.dat"]; ok {
		t.Errorf(".dat file leaked through extension filter")
	}
}

func TestSamplePlanContextEmptyOnMissingDir(t *testing.T) {
	got := Sample("", 5, 1000)
	if got != nil {
		t.Errorf("expected nil for empty workingDir, got %v", got)
	}
}

func TestSamplePlanContextWalksSubdirsForPriorityFiles(t *testing.T) {
	// May 2026 user case: workspace root has no app.py, but a
	// snake/ subdir does. Sampler should pick up snake/app.py with
	// the path keyed as "snake/app.py" so the planner emits tool
	// calls using that exact path.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "snake", "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snake", "app.py"),
		[]byte("from flask import Flask\napp=Flask(__name__)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snake", "templates", "index.html"),
		[]byte("<html>hi</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := Sample(dir, 6, 2000)
	if _, ok := got["snake/app.py"]; !ok {
		t.Errorf("expected snake/app.py via subdir walk, got keys %v", slices.Sorted(maps.Keys(got)))
	}
	if _, ok := got["snake/templates/index.html"]; !ok {
		t.Errorf("expected snake/templates/index.html via subdir walk, got keys %v", slices.Sorted(maps.Keys(got)))
	}
}

func TestSamplePlanContextSkipsNoiseDirs(t *testing.T) {
	// venv/ and node_modules/ shouldn't be walked even if they
	// contain a priority filename — these are cache/vendor dirs.
	dir := t.TempDir()
	for _, junk := range []string{"venv", "node_modules", ".git", "__pycache__"} {
		if err := os.MkdirAll(filepath.Join(dir, junk), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, junk, "app.py"),
			[]byte("# noise"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := Sample(dir, 6, 2000)
	for _, junk := range []string{"venv/app.py", "node_modules/app.py", ".git/app.py", "__pycache__/app.py"} {
		if _, ok := got[junk]; ok {
			t.Errorf("noise path %q leaked into context", junk)
		}
	}
}

// The sample stops at its limit wherever it is reading, and a hit at one level
// leaves the next level unread.
func TestThePlanSampleStopsWhereItShould(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  []string
	}{
		{"the root stops at the limit",
			[]string{"app.py", "main.py", "manage.py"},
			[]string{"app.py", "main.py"}},
		{"the subfolders stop at the limit",
			[]string{"a/app.py", "a/main.py", "b/app.py"},
			[]string{"a/app.py", "a/main.py"}},
		{"the source files stop at the limit",
			[]string{"a.py", "b.py", "c.py"},
			[]string{"a.py", "b.py"}},
		{"a root hit ends the walk",
			[]string{"app.py", "sub/main.py"},
			[]string{"app.py"}},
		{"a subfolder hit ends the walk",
			[]string{"sub/app.py", "a.py"},
			[]string{"sub/app.py"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range c.files {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := slices.Sorted(maps.Keys(Sample(dir, 2, 2000)))
			if !slices.Equal(got, c.want) {
				t.Errorf("sampled %v, want %v", got, c.want)
			}
		})
	}
}

// More files were read than fit into the context of a revised plan. The
// planner sees the first of them in sorted path order, every time, not
// whatever the map's order gives.
func TestTheFilesReadThatFillAPlansContextAreTheSameEachTime(t *testing.T) {
	dir := t.TempDir()
	filesRead := map[string]string{}
	for n := 11; n >= 0; n-- {
		filesRead[filepath.Join(dir, fmt.Sprintf("f%02d.py", n))] = "x = 1\n"
	}
	want := []string{"f00.py", "f01.py", "f02.py", "f03.py", "f04.py", "f05.py", "f06.py", "f07.py"}
	for try := 1; try <= 100; try++ {
		pctx := map[string]string{}
		AddFilesRead(pctx, filesRead, dir, 8)
		if got := slices.Sorted(maps.Keys(pctx)); !reflect.DeepEqual(got, want) {
			t.Fatalf("try %d: %v, want %v", try, got, want)
		}
	}
}
