//go:build !unix

package main

// openNonBlock has no equivalent here, so the open is plain and the kind check
// alone decides. The wait this prevents is the unix one: on these platforms a
// named pipe is not a filesystem entry, so there is no such open to make
// non-blocking.
const openNonBlock = 0
