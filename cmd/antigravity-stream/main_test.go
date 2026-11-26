package main

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func TestDelayedStream(t *testing.T) {
	data := "data: " + strings.Repeat("x", 95000) + "\n\n"
	input := ": keep-alive\n\n" + data + ": keep-alive\r\n\r\n" + "data: done\n\n"
	body := io.NopCloser(strings.NewReader(input))
	s := &eventStream{Closer: body, lines: bufio.NewReader(body)}
	got, err := io.ReadAll(s)
	if err != nil || string(got) != data+"data: done\n\n" {
		t.Fatalf("stream mismatch, bytes=%d, err=%v", len(got), err)
	}
}
