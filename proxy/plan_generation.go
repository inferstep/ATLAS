// What the planner is shown: a small sample of the project, read before any tool has run.

package main

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// samplePlanContext walks ctx.WorkingDir and reads a handful of files
// the planner is most likely to need: source files, templates,
// manifests. Limited to maxFiles per call, each truncated to maxBytes.
//
// The planner runs *before* any tool calls have happened in the loop,
// so ctx.FilesRead is empty — without this, plans for "fix the flask
// app" would have no signal about what's in app.py and would generate
// generic 5-step recipes. We pay one fs walk + a few small reads up
// front; the budget is small (~5 files × 2KB) and the planning quality
// jump is large.
func samplePlanContext(workingDir string, maxFiles, maxBytes int) map[string]string {
	if workingDir == "" {
		return nil
	}
	out := map[string]string{}
	// Files we always inline if present — most projects have at least
	// one of these and they describe shape (deps, entry point).
	priority := []string{
		"app.py", "main.py", "manage.py", "wsgi.py",
		"index.html", "templates/index.html", "templates/base.html",
		"package.json", "tsconfig.json", "vite.config.ts", "vite.config.js",
		"go.mod", "main.go",
		"Cargo.toml", "src/main.rs", "src/lib.rs",
		"requirements.txt", "pyproject.toml", "setup.py",
		"README.md",
	}
	// Every read goes through the workspace folder, so a name in it that
	// points somewhere else is skipped like a file that cannot be read.
	dir := openConfinedDir(workingDir)
	defer dir.Close()
	sample := planSample{dir: dir, out: out, maxFiles: maxFiles, maxBytes: maxBytes}
	sample.readPriorityFiles("", priority)
	// If priority files yielded nothing at the workspace root, the
	// project may live one level down -- common when the user's
	// `atlas tui` cwd was the parent dir (e.g. /workspace) and the app
	// is in a subdirectory. Walk one level looking for the SAME
	// priority filenames inside subdirectories. Without this the
	// planner sees no project context and the agent spends turns
	// finding the files.
	if len(out) == 0 {
		entries, err := dir.ReadDir()
		if err != nil {
			return nil
		}
		// First pass: peek into subdirectories for priority files.
		for _, e := range entries {
			if e.IsDir() && !notAProjectDir(e.Name()) {
				sample.readPriorityFiles(e.Name(), priority)
			}
		}
		// Second pass: shallow walk of the workspace root for any
		// source-looking files (uncommon repo layout, no priority
		// hits anywhere).
		if len(out) == 0 {
			sample.readSourceFiles(entries)
		}
	}
	return out
}

// planStage names the planner on the event stream.
const planStage = "v3:plan"

// planSample is what samplePlanContext has collected so far, and its limits.
type planSample struct {
	dir                confinedDir
	out                map[string]string
	maxFiles, maxBytes int
}

// readPriorityFiles adds the priority files under subdir, keyed by the path the
// agent will need to use in tool calls. Oversized files are skipped: the planner
// doesn't need a 50KB README.
func (s planSample) readPriorityFiles(subdir string, priority []string) {
	for _, rel := range priority {
		if len(s.out) >= s.maxFiles {
			return
		}
		path := filepath.Join(subdir, rel)
		if body, ok := s.dir.readCapped(path, s.maxBytes); ok {
			s.out[path] = body
		}
	}
}

// readSourceFiles adds the source-looking files among entries.
func (s planSample) readSourceFiles(entries []fs.DirEntry) {
	for _, e := range entries {
		if len(s.out) >= s.maxFiles {
			return
		}
		name := e.Name()
		if e.IsDir() || !isSourceExtension(strings.ToLower(filepath.Ext(name))) {
			continue
		}
		if body, ok := s.dir.readCapped(name, s.maxBytes); ok {
			s.out[name] = body
		}
	}
}

// notAProjectDir names caches, vendors and dot-dirs.
func notAProjectDir(name string) bool {
	switch name {
	case "node_modules", "venv", "__pycache__", "dist", "build", "target", "vendor":
		return true
	}
	return strings.HasPrefix(name, ".")
}

func isSourceExtension(ext string) bool {
	switch ext {
	case ".py", ".go", ".js", ".ts", ".tsx", ".jsx", ".html", ".rs", ".rb", ".java", ".kt", ".swift":
		return true
	}
	return false
}
