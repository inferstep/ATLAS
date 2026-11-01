package workspacefs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ConfinedDir reads one folder through os.Root. A name that resolves to
// something outside the folder is an error and never a read, whatever the
// name points at and whenever it was made to point there. A folder that
// could not be opened gives a ConfinedDir whose every read fails, so a
// caller treats it like a folder with nothing readable in it.
type ConfinedDir struct{ root *os.Root }

var errConfinedDirClosed = errors.New("folder is not open")

func OpenConfinedDir(dir string) ConfinedDir {
	if dir == "" {
		return ConfinedDir{}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ConfinedDir{}
	}
	return ConfinedDir{root: root}
}

func (c ConfinedDir) Close() {
	if c.root != nil {
		_ = c.root.Close()
	}
}

// Stat describes rel, a path relative to the folder.
func (c ConfinedDir) Stat(rel string) (os.FileInfo, error) {
	if c.root == nil {
		return nil, errConfinedDirClosed
	}
	return c.root.Stat(rel)
}

// ReadFile returns the content of rel, a path relative to the folder. Only a
// regular file is read, for the reason file_kind.go gives: a name that holds a
// named pipe would hold the open until a writer appeared, and this folder's
// own rule is that a name it cannot read is skipped like one that is absent.
func (c ConfinedDir) ReadFile(rel string) ([]byte, error) {
	if c.root == nil {
		return nil, errConfinedDirClosed
	}
	file, err := regularFileHandle(c.root.OpenFile(rel, os.O_RDONLY|openNonBlock, 0))
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
func (c ConfinedDir) ReadDir() ([]fs.DirEntry, error) {
	if c.root == nil {
		return nil, errConfinedDirClosed
	}
	return fs.ReadDir(c.root.FS(), ".")
}

// Walk calls fn for every entry below the folder, in the order and with the
// SkipDir and SkipAll rules of fs.WalkDir. rel is relative to the folder
// and uses the separator of the operating system. The folder itself is not
// reported, so its own name never decides what is walked.
func (c ConfinedDir) Walk(fn func(rel string, entry fs.DirEntry, err error) error) error {
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
func (c ConfinedDir) WriteNewFile(rel string, data []byte, perm os.FileMode) error {
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
func (c ConfinedDir) Remove(rel string) error {
	if c.root == nil {
		return errConfinedDirClosed
	}
	return c.root.Remove(rel)
}

// ReadCapped returns the text of rel, for the caller to cut to maxBytes. It
// reports false for a folder, for anything else that is not a regular file,
// for a file over four times maxBytes, and for anything that cannot be read.
func (c ConfinedDir) ReadCapped(rel string, maxBytes int) (string, bool) {
	info, err := c.Stat(rel)
	if err != nil || !info.Mode().IsRegular() || info.Size() > int64(maxBytes)*4 {
		return "", false
	}
	data, err := c.ReadFile(rel)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// ReadConfined returns the content of rel inside dir, for a caller that
// reads one file.
func ReadConfined(dir, rel string) ([]byte, error) {
	folder := OpenConfinedDir(dir)
	defer folder.Close()
	return folder.ReadFile(rel)
}

// MountProbeName is the file the alignment check writes in the workspace and
// asks the sandbox to read back.
const MountProbeName = ".atlas-mount-probe"

// WriteMountProbe puts the probe in the folder dir and returns the call that
// removes it again. The name is cleared first and then created new, so a link
// with that name is removed and never written through.
func WriteMountProbe(dir, token string) (func(), error) {
	folder := OpenConfinedDir(dir)
	_ = folder.Remove(MountProbeName)
	if err := folder.WriteNewFile(MountProbeName, []byte(token), 0o644); err != nil {
		folder.Close()
		return nil, err
	}
	return func() {
		_ = folder.Remove(MountProbeName)
		folder.Close()
	}, nil
}
