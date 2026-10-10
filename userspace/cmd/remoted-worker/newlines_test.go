package main

import (
	"goos/kernel/ipc"
	"goos/userspace/remoteservice"
	"goos/userspace/tty"
	"testing"
)

func TestPTYNewlines(t *testing.T) {
	for _, test := range []struct {
		name  string
		parts []string
		want  string
	}{
		{"bare LF", []string{"a\nb"}, "a\r\nb"},
		{"existing CRLF", []string{"a\r\nb"}, "a\r\nb"},
		{"split CRLF", []string{"a\r", "\n"}, "a\r\n"},
		{"empty cooked chunk", []string{"a\r", "", "\n"}, "a\r\n"},
		{"successive LF", []string{"\n\n"}, "\r\n\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var converter ptyNewlines
			var queue remoteservice.Queue
			for _, part := range test.parts {
				if !converter.Write(&queue, []byte(part), false) {
					t.Fatal("cooked write failed")
				}
			}
			if got := string(queue.Peek(queue.Len())); got != test.want {
				t.Fatalf("output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPTYNewlinesEmptyRawChunkKeepsCR(t *testing.T) {
	var converter ptyNewlines
	var queue remoteservice.Queue
	if !converter.Write(&queue, []byte("a\r"), false) ||
		!converter.Write(&queue, nil, true) ||
		!converter.Write(&queue, []byte("\n"), false) {
		t.Fatal("output write failed")
	}
	if got := string(queue.Peek(queue.Len())); got != "a\r\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestPTYNewlinesRawOutput(t *testing.T) {
	var converter ptyNewlines
	var queue remoteservice.Queue
	if !converter.Write(&queue, []byte("a\r"), false) ||
		!converter.Write(&queue, []byte("\n\r\n\x1b[H"), true) ||
		!converter.Write(&queue, []byte("\n"), false) {
		t.Fatal("output write failed")
	}
	if got, want := string(queue.Peek(queue.Len())), "a\r\n\r\n\x1b[H\r\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPTYEngineRawOutputStaysUnchanged(t *testing.T) {
	var converter ptyNewlines
	var queue remoteservice.Queue
	var engine *tty.Engine
	engine = tty.NewEngine(func(data []byte) bool {
		return converter.Write(&queue, data, engine != nil && engine.Raw())
	}, func(ipc.Message) bool { return true })
	request, ok := tty.Request(tty.OperationRawMode, nil, 1)
	if !ok {
		t.Fatal("raw mode request failed")
	}
	engine.Request(&request)
	engine.Output([]byte("\n"))
	if got := string(queue.Peek(queue.Len())); got != "\n" {
		t.Fatalf("raw output = %q", got)
	}
}
