//go:build unix

package main

import "syscall"

// openNonBlock makes an open return at once rather than wait for something the
// file may never get. A named pipe opened for reading without it waits for a
// writer; with it the open succeeds at once on the empty pipe, and the kind
// check that follows is what refuses the read.
//
// The tag is unix rather than linux because the flag has the same name and the
// same meaning on every platform this builds for, even though its value differs
// by platform (0x800 on Linux, 0x4 on Darwin): a session on a developer's Mac
// meets the same pipe in the same workspace as one in a container.
const openNonBlock = syscall.O_NONBLOCK
