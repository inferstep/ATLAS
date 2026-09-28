//go:build linux

package main

import (
	"errors"
	"os"
	"sync"
	"syscall"
)

// A deletion approval is bound to an object, and the object has to be HELD for
// the binding to mean anything. Device and inode numbers read at two separate
// moments are not a binding: the filesystem is free to hand a recreated file
// the number its predecessor just gave up, and ext4 does exactly that. What
// holds the object is a kernel reference to it, taken when the user is asked
// and kept until the removal attempt is over.
//
// The reference is an O_PATH descriptor opened with O_NOFOLLOW. O_PATH refers
// to the object without opening it for I/O, so a symlink is held as the link
// (never its target), an empty directory as the directory, and a file without
// reading it. While the descriptor is open the kernel keeps that object alive,
// and the object's own link count says whether any name still refers to it:
// unlinking the last name drives it to zero. That count is read from the held
// object, not from the path, so it cannot be answered by a replacement.
//
// Linux only. The other-platform file refuses to bind, which refuses the
// deletion before anyone is asked; nothing falls back to comparing numbers.

// oPath is O_PATH, which Go's syscall package does not export. The value is
// asm-generic (010000000) and the same on every Linux architecture.
const oPath = 0x200000

var errObjectIdentityUnavailable = errors.New("the object could not be held for approval")

// objectHandle is the held reference. It is created by inspection, carried by
// the approval, and released exactly once by whoever ends the approval's life:
// the tool after its attempt, the handshake on a refusal, the next grant that
// replaces it, or the loop's exit.
type objectHandle struct {
	mu       sync.Mutex
	fd       int
	dev, ino uint64
	released bool
}

// pinObjectFn is the one seam: tests substitute a failing pin to exercise the
// fail-closed path on a platform where the real one succeeds.
var pinObjectFn = pinObject

// pinObject takes the reference for the object AT path, following nothing.
func pinObject(path string) (*objectHandle, error) {
	fd, err := syscall.Open(path, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errObjectIdentityUnavailable
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return nil, errObjectIdentityUnavailable
	}
	return &objectHandle{fd: fd, dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}

// stillTheObjectAt reports whether the entry now at the inspected path IS the
// held object. Two facts, both from the kernel: the held object still has at
// least one name (its link count, read through the descriptor), and the path's
// current entry has the held object's device and inode. A recycled number
// cannot satisfy both, because a number is recycled only after the object that
// had it lost its last name -- which the first fact rules out. A released or
// nil handle answers false: no held object, no binding.
func (h *objectHandle) stillTheObjectAt(now os.FileInfo) bool {
	if h == nil || now == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return false
	}
	var held syscall.Stat_t
	if err := syscall.Fstat(h.fd, &held); err != nil || held.Nlink == 0 {
		return false
	}
	st, ok := now.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return uint64(st.Dev) == h.dev && uint64(st.Ino) == h.ino &&
		uint64(held.Dev) == h.dev && uint64(held.Ino) == h.ino
}

// release closes the descriptor. Idempotent: a second release is a no-op, and
// a use after release answers false rather than touching a closed descriptor.
func (h *objectHandle) release() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return
	}
	h.released = true
	syscall.Close(h.fd)
	h.fd = -1
}
