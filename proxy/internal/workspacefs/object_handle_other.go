//go:build !linux

package workspacefs

import (
	"errors"
	"os"
)

// No held object reference is implemented for this platform, so no deletion
// approval can be bound to an object here. Inspection fails before the user is
// asked, and the deletion is refused. This is deliberate: comparing device and
// inode numbers at two moments is not a binding, and the platforms this stub
// covers have not been shown to offer one. Linux is implemented in
// object_handle_linux.go.

var ErrObjectIdentityUnavailable = errors.New("the object could not be held for approval on this platform")

// ObjectHoldSupported says whether this system can hold an object for a
// deletion approval. Tests that go through the approval read it.
const ObjectHoldSupported = false

type ObjectHandle struct{}

var PinObjectFn = PinObject

func PinObject(string) (*ObjectHandle, error)             { return nil, ErrObjectIdentityUnavailable }
func (h *ObjectHandle) StillTheObjectAt(os.FileInfo) bool { return false }
func (h *ObjectHandle) Release() {
	// Nothing is held on this platform.
}
