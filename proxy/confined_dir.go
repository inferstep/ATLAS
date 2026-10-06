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

// WriteNewFile creates rel, a path relative to the folder, and writes data to
// it. A name that is already taken is an error, whatever holds it, so nothing
// that exists is written over and no link is written through.
func (c confinedDir) WriteNewFile(rel string, data []byte, perm os.FileMode) error {
	if c.root == nil {
		return errConfinedDirClosed
	}
	file, err := c.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	return writeErr
}

// Remove deletes the name rel itself. For a link that is the link, never the
// file it points at.
func (c confinedDir) Remove(rel string) error {
	if c.root == nil {
		return errConfinedDirClosed
	}
	return c.root.Remove(rel)
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

// mountProbeName is the file the alignment check writes in the workspace and
// asks the sandbox to read back.
const mountProbeName = ".atlas-mount-probe"

// writeMountProbe puts the probe in the folder dir and returns the call that
// removes it again. The name is cleared first and then created new, so a link
// with that name is removed and never written through.
func writeMountProbe(dir, token string) (func(), error) {
	folder := openConfinedDir(dir)
	_ = folder.Remove(mountProbeName)
	if err := folder.WriteNewFile(mountProbeName, []byte(token), 0o644); err != nil {
		folder.Close()
		return nil, err
	}
	return func() {
		_ = folder.Remove(mountProbeName)
		folder.Close()
	}, nil
}
