package main

import (
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/tty"
	"strings"
	"testing"
)

func TestRemoteOutputPrecedesNextPrompt(t *testing.T) {
	for _, chunks := range [][]string{
		{"remote connect", "ion admitted worker=4294967297\n"},
		{"first ", "second ", "third\n"},
		{strings.Repeat("x", 240), strings.Repeat("y", 240), strings.Repeat("z", 240), "\n"},
	} {
		var rendered strings.Builder
		engine := tty.NewEngine(func(value []byte) bool { rendered.Write(value); return true }, func(ipc.Message) bool { return true })
		index := 0
		read := func(buffer []byte) (int, kcall.Status) {
			if index == len(chunks) {
				return 0, kcall.StatusResourceExhausted
			}
			n := copy(buffer, chunks[index])
			index++
			return n, kcall.StatusOK
		}
		for {
			pending, _, closed, ok := drainStdoutBatch(read, engine.Output, func() int { return 4096 }, make([]byte, 256), 2)
			if !ok || closed {
				t.Fatal("stdout drain failed")
			}
			if !pending {
				break
			}
		}
		request, ok := tty.Request(tty.OperationReadLine, []byte("goos> "), 1)
		if !ok {
			t.Fatal("prompt request failed")
		}
		engine.Request(&request)
		want := strings.Join(chunks, "") + "goos> "
		if got := rendered.String(); got != want {
			t.Fatalf("rendered %q, want %q", got, want)
		}
	}
}

func TestRemoteOutputDefersPromptWhenQueueIsFull(t *testing.T) {
	reads := 0
	pending, activity, closed, ok := drainStdoutBatch(func([]byte) (int, kcall.Status) {
		reads++
		return 0, kcall.StatusResourceExhausted
	}, func([]byte) {}, func() int { return 1023 }, make([]byte, 256), 8)
	if !pending || activity || closed || !ok || reads != 0 {
		t.Fatalf("drain result pending=%t activity=%t closed=%t ok=%t reads=%d", pending, activity, closed, ok, reads)
	}
}
