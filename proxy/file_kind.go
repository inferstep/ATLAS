package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// A read of a file that is not a regular file is refused before anything is
// read. Confinement answers where a name resolves; this answers what it holds,
// and the two are separate questions — a name inside the workspace can hold a
// named pipe just as easily as a file.
//
// The kind matters because the outcomes differ and one of them is not an
// outcome at all. Opening a named pipe for reading waits until some program
// opens it for writing, and in a workspace where nothing ever does, the open
// never returns: the tool call never comes back, the turn never ends, and a
// stop then waits for that turn. Measured on dev in a test session, where the
// model called read_file on a named pipe and the session never ended.
//
// Two details are load-bearing:
//
//   - The open handle is judged, never the name. The file is opened first and
//     the handle is asked its kind, so a name swapped for a pipe between a
//     check and an open cannot reintroduce the wait. A Stat-then-open would
//     leave exactly that window.
//   - The open is non-blocking. A pipe named the way a start scan reads opens
//     at once with no writer instead of waiting for one; the kind check then
//     refuses it, which is what turns a hung session into a refusal.

// errNotRegularFile marks a read refused for the kind of file it found. A
// caller that treats every read failure alike need not look at it; a caller
// that must tell an absent file from an unreadable one still sees the original
// open error untouched, so os.IsNotExist keeps answering about the file.
var errNotRegularFile = errors.New("not a regular file")

// fileKindName is what a path holds, in the words the refusal uses.
func fileKindName(mode os.FileMode) string {
	switch {
	case mode.IsRegular():
		return "a regular file"
	case mode.IsDir():
		return "a directory"
	// A character device carries ModeDevice too, so it is named first.
	case mode&os.ModeCharDevice != 0:
		return "a character device"
	case mode&os.ModeDevice != 0:
		return "a block device"
	case mode&os.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&os.ModeSocket != 0:
		return "a socket"
	// A symbolic link has no case here: an fstat asks about the open handle, and
	// an open follows the link, so a handle never reports ModeSymlink.
	default:
		return "not a regular file"
	}
}

// notRegularError is the refusal for a path that holds kind. It names the kind
// so the model can act on the answer rather than retrying the same call.
func notRegularError(kind string) error {
	return fmt.Errorf("%w — it is %s, so nothing was read", errNotRegularFile, kind)
}

// regularFileHandle returns f when the handle's own kind is a regular file,
// and the refusal otherwise. f is closed on every error path, so a caller only
// ever holds a handle it is meant to read.
func regularFileHandle(f *os.File, err error) (*os.File, error) {
	if err != nil {
		return nil, err
	}
	info, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return nil, statErr
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, notRegularError(fileKindName(info.Mode()))
	}
	return f, nil
}

// openRegularFile opens path non-blockingly and returns the handle only when it
// holds a regular file.
func openRegularFile(path string) (*os.File, error) {
	return regularFileHandle(os.OpenFile(path, os.O_RDONLY|openNonBlock, 0))
}

// readRegularFile returns the content of the regular file at path. Any other
// kind of file, and any failure to open or read it, is an error and no content.
func readRegularFile(path string) ([]byte, error) {
	f, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(f)
	if closeErr := f.Close(); readErr == nil {
		readErr = closeErr
	}
	return data, readErr
}
