package main

import (
	"errors"
	"fmt"
	"io"
)

// maxServiceAnswerBytes bounds an answer that the proxy reads from a service
// of the stack: llama-server, the V3 service, the lens and the sandbox. The
// largest real answer is a structural edit or a command's output of a few MiB;
// 64 MiB is far above that, and keeps a broken service that sends without end
// from filling the proxy's memory.
const maxServiceAnswerBytes = 64 << 20

var errServiceAnswerTooLarge = fmt.Errorf("the service's answer is over %d bytes", maxServiceAnswerBytes)

// serviceAnswer is a service's answer, read at most maxServiceAnswerBytes far.
// A read past that ends with errServiceAnswerTooLarge, not with a cut answer
// that looks whole.
func serviceAnswer(body io.Reader) io.Reader {
	return &boundedAnswer{body: body, left: maxServiceAnswerBytes}
}

type boundedAnswer struct {
	body io.Reader
	left int64
}

func (b *boundedAnswer) Read(p []byte) (int, error) {
	if b.left == 0 {
		// An answer of exactly the bound is whole; one byte more is not.
		var one [1]byte
		if n, err := b.body.Read(one[:]); n > 0 {
			return 0, errServiceAnswerTooLarge
		} else if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.body.Read(p)
	b.left -= int64(n)
	return n, err
}
