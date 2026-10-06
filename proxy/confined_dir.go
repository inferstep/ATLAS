package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// confinedDir reads one folder through os.Root. A name that resolves to
// something outside the folder is an error and never a read, whatever the
// name points at and whenever it was made to point there. A folder that
// could not be opened gives a confinedDir whose every read fails, so a
// caller treats it like a folder with nothing readable in it.
type confinedDir struct{ root *os.Root }

var errConfinedDirClosed = errors.New("folder is not open")

func openConfinedDir(dir string) confinedDir {
	if dir == "" {
		return confinedDir{}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return confinedDir{}
	}
	return confinedDir{root: root}
}

func (c confinedDir) Close() {
	if c.root != nil {
		_ = c.root.Close()
	}
}

// Stat describes rel, a path relative to the folder.
func (c confinedDir) Stat(rel string) (os.FileInfo, error) {
	if c.root == nil {
		return nil, errConfinedDirClosed
	}
	return c.root.Stat(rel)
}

// ReadFile returns the content of rel, a path relative to the folder.
func (c confinedDir) ReadFile(rel string) ([]byte, error) {
	if c.root == nil {
		return nil, errConfinedDirClosed
	}
	file, err := c.root.Open(rel)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(file)
	if closeErr := file.Close(); readErr == nil {
		readErr = closeErr
	}
	return data, readErr
}

// ReadDir lists the folder itself.
func (c confinedDir) ReadDir() ([]fs.DirEntry, error) {
	if c.root == nil {
		return nil, errConfinedDirClosed
	}
	return fs.ReadDir(c.root.FS(), ".")
}

// Walk calls fn for every entry below the folder, in the order and with the
// SkipDir and SkipAll rules of fs.WalkDir. rel is relative to the folder
// and uses the separator of the operating system. The folder itself is not
// reported, so its own name never decides what is walked.
func (c confinedDir) Walk(fn func(rel string, entry fs.DirEntry, err error) error) error {
	if c.root == nil {
		return errConfinedDirClosed
	}
	return fs.WalkDir(c.root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if path == "." {
			return err
		}
		return fn(filepath.FromSlash(path), entry, err)
	})
}

// readCapped returns the text of rel, cut to maxBytes. It reports false for
// a folder, for a file over four times maxBytes, and for anything that
// cannot be read.
func (c confinedDir) readCapped(rel string, maxBytes int) (string, bool) {
	info, err := c.Stat(rel)
	if err != nil || info.IsDir() || info.Size() > int64(maxBytes)*4 {
		return "", false
	}
	data, err := c.ReadFile(rel)
	if err != nil {
		return "", false
	}
	text := string(data)
	if len(text) > maxBytes {
		text = text[:maxBytes] + "\n... (truncated)"
	}
	return text, true
}

// readConfined returns the content of rel inside dir, for a caller that
// reads one file.
func readConfined(dir, rel string) ([]byte, error) {
	folder := openConfinedDir(dir)
	defer folder.Close()
	return folder.ReadFile(rel)
}
