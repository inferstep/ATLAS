// Package projectcontext picks the files of a project that a V3 request
// carries, and cuts each to the bytes the request allows.
package projectcontext

import (
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/itigges22/atlas-proxy/internal/workspacefs"
)

// Sample walks workingDir and reads a handful of files
// the planner is most likely to need: source files, templates,
// manifests. Limited to maxFiles per call, each truncated to maxBytes.
//
// The planner runs *before* any tool calls have happened in the loop,
// so no file has been read yet — without this, plans for "fix the flask
// app" would have no signal about what's in app.py and would generate
// generic 5-step recipes. We pay one fs walk + a few small reads up
// front; the budget is small (~5 files × 2KB) and the planning quality
// jump is large.
func Sample(workingDir string, maxFiles, maxBytes int) map[string]string {
	return SampleWithin(workingDir, maxFiles, maxBytes, time.Time{})
}

// SampleWithin is Sample with a deadline: the scan stops at it and returns the
// files it collected before it ran out, so a deep tree cannot hold a session
// start. A zero deadline means no limit.
func SampleWithin(workingDir string, maxFiles, maxBytes int, deadline time.Time) map[string]string {
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
	dir := workspacefs.OpenConfinedDir(workingDir)
	defer dir.Close()
	sample := planSample{dir: dir, out: out, maxFiles: maxFiles, maxBytes: maxBytes, deadline: deadline}
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

// planSample is what Sample has collected so far, and its limits.
type planSample struct {
	dir                workspacefs.ConfinedDir
	out                map[string]string
	maxFiles, maxBytes int
	deadline           time.Time // when the scan must stop; zero means no limit
}

// expired reports whether the scan has spent its budget.
func (s planSample) expired() bool {
	return !s.deadline.IsZero() && time.Now().After(s.deadline)
}

// readPriorityFiles adds the priority files under subdir, keyed by the path the
// agent will need to use in tool calls. Oversized files are skipped: the planner
// doesn't need a 50KB README.
func (s planSample) readPriorityFiles(subdir string, priority []string) {
	for _, rel := range priority {
		if len(s.out) >= s.maxFiles || s.expired() {
			return
		}
		s.add(filepath.Join(subdir, rel))
	}
}

// readSourceFiles adds the source-looking files among entries.
func (s planSample) readSourceFiles(entries []fs.DirEntry) {
	for _, e := range entries {
		if len(s.out) >= s.maxFiles || s.expired() {
			return
		}
		name := e.Name()
		if e.IsDir() || !isSourceExtension(strings.ToLower(filepath.Ext(name))) {
			continue
		}
		s.add(name)
	}
}

// add puts one file into the sample, cut to the bytes a request carries for it.
// A file over four times that is skipped.
func (s planSample) add(rel string) {
	if s.expired() {
		return
	}
	if body, ok := s.dir.ReadUpTo(rel, s.maxBytes*4); ok {
		s.out[rel] = Entry(body, s.maxBytes)
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

// AddFilesRead adds filesRead, the files this run has read (path to
// content), to the context of a revised plan, until the context holds `limit`
// files.
//
// When more files were read than fit, they are taken in sorted order of their
// paths. The run keeps no order of reading (filesRead is a map), and the map's
// own order would make which files the planner sees a matter of chance.
func AddFilesRead(pctx, filesRead map[string]string, workingDir string, limit int) {
	for _, path := range slices.Sorted(maps.Keys(filesRead)) {
		if len(pctx) >= limit {
			break
		}
		// Use relative path if possible so the planner key matches
		// what the agent will pass to read_file/edit_file later.
		rel := path
		if strings.HasPrefix(path, workingDir+"/") {
			rel = strings.TrimPrefix(path, workingDir+"/")
		}
		pctx[rel] = Entry(filesRead[path], 2000)
	}
}

// Entry cuts a file to the bytes a request carries for it, to save
// tokens.
func Entry(content string, limit int) string {
	if len(content) > limit {
		return content[:limit] + "\n... (truncated)"
	}
	return content
}
