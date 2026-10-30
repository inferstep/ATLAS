package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// endless is an answer of one byte, again and again, that never ends.
type endless byte

func (b endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

// A service's answer of the bound's size is read whole; one byte more ends
// with the error, not with a cut answer that looks whole. The sandbox's /shell
// answer, which carries the output of the model's code, is read the same way.
func TestAServiceAnswerOverTheBoundIsAnError(t *testing.T) {
	readAll := func(answer io.Reader) (int64, error) { return io.Copy(io.Discard, serviceAnswer(answer)) }
	decodeShell := func(answer io.Reader) (int64, error) {
		var out RunCommandOutput
		return 0, decodeShellResponse(answer, &out)
	}
	for _, c := range []struct {
		name   string
		read   func(io.Reader) (int64, error)
		answer io.Reader
		want   error
	}{
		{"an answer of the bound's size", readAll, io.LimitReader(endless(0), maxServiceAnswerBytes), nil},
		{"an answer one byte over the bound", readAll, io.LimitReader(endless(0), maxServiceAnswerBytes+1), errServiceAnswerTooLarge},
		{"a /shell answer over the bound", decodeShell,
			io.MultiReader(strings.NewReader(`{"stdout":"`), io.LimitReader(endless('a'), maxServiceAnswerBytes)), errServiceAnswerTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, err := c.read(c.answer)
			if !errors.Is(err, c.want) || (c.want == nil && n != maxServiceAnswerBytes) {
				t.Errorf("read %d bytes, error %v; want the error %v", n, err, c.want)
			}
		})
	}
}
