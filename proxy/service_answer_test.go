package main

import (
	"errors"
	"io"
	"testing"
)

// zeros is an answer of zero bytes that never ends.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// A service's answer of the bound's size is read whole; one byte more ends
// with the error, not with a cut answer that looks whole.
func TestAServiceAnswerOverTheBoundIsAnError(t *testing.T) {
	for _, c := range []struct {
		name string
		size int64
		want error
	}{
		{"an answer of the bound's size", maxServiceAnswerBytes, nil},
		{"an answer one byte over the bound", maxServiceAnswerBytes + 1, errServiceAnswerTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, err := io.Copy(io.Discard, serviceAnswer(io.LimitReader(zeros{}, c.size)))
			if !errors.Is(err, c.want) || (c.want == nil && n != c.size) {
				t.Errorf("read %d bytes, error %v; want the error %v", n, err, c.want)
			}
		})
	}
}
