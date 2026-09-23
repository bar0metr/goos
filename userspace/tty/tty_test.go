package tty

import (
	"strings"
	"testing"
)

func feed(line *Discipline, input string) ([]Event, string) {
	var events []Event
	var echo strings.Builder
	for index := 0; index < len(input); index++ {
		event := line.Feed(input[index])
		events = append(events, event)
		echo.Write(line.Echo())
	}
	return events, echo.String()
}

func TestDisciplineEditsAndCompletesALine(t *testing.T) {
	var line Discipline
	events, echo := feed(&line, "lz\bs -l\r")
	got, complete := line.Line()
	if !complete || string(got) != "ls -l" || events[len(events)-1] != EventLine {
		t.Fatalf("line = %q complete=%v events=%v", got, complete, events)
	}
	if echo != "lz\b \bs -l\n" {
		t.Fatalf("echo = %q", echo)
	}
	if line.Feed('x') != EventNone {
		t.Fatal("input after a completed line was accepted")
	}
	line.Next()
	if _, complete := line.Line(); complete || len(line.Pending()) != 0 {
		t.Fatal("next line not empty")
	}
}

func TestDisciplineSkipsEscapeSequences(t *testing.T) {
	var line Discipline
	feed(&line, "a\x1b[A\x1b[1;5Cb\x1bOPc\n")
	if got, _ := line.Line(); string(got) != "abc" {
		t.Fatalf("line = %q", got)
	}
}

func TestDisciplineControlKeys(t *testing.T) {
	var line Discipline
	if events, echo := feed(&line, "abc\x03"); events[3] != EventInterrupt || !strings.HasSuffix(echo, "^C\n") || len(line.Pending()) != 0 {
		t.Fatalf("interrupt: %v %q", events, echo)
	}
	if line.Feed(0x04) != EventEndOfInput {
		t.Fatal("Ctrl-D on an empty line is not end of input")
	}
	feed(&line, "ab")
	if line.Feed(0x04) != EventNone {
		t.Fatal("Ctrl-D with text ended the input")
	}
	if events, echo := feed(&line, "\x15"); events[0] != EventEcho || echo != "\b \b\b \b" || len(line.Pending()) != 0 {
		t.Fatalf("kill line: %v %q", events, echo)
	}
	if line.Feed(0x0c) != EventRedraw {
		t.Fatal("Ctrl-L does not redraw")
	}
	if line.Feed(0x07) != EventNone || line.Feed(0x80) != EventNone || len(line.Pending()) != 0 {
		t.Fatal("control or non-ASCII byte accepted")
	}
}

func TestDisciplineIsBounded(t *testing.T) {
	var line Discipline
	feed(&line, strings.Repeat("x", MaxLine+10))
	if len(line.Pending()) != MaxLine {
		t.Fatalf("pending = %d", len(line.Pending()))
	}
	feed(&line, strings.Repeat("\x15", 3))
	if allocations := testing.AllocsPerRun(10, func() { line.Feed('y'); line.Feed(0x15) }); allocations != 0 {
		t.Fatalf("feed allocated %v times", allocations)
	}
}

func TestProtocolRoundTrip(t *testing.T) {
	request, ok := Request(OperationReadLine, []byte("goos> "), 3)
	if !ok {
		t.Fatal("request refused")
	}
	operation, prompt, ok := DecodeRequest(&request)
	if !ok || operation != OperationReadLine || string(prompt) != "goos> " {
		t.Fatalf("decoded %d %q %v", operation, prompt, ok)
	}
	reply, ok := Reply(ResultLine, []byte("ls"), 3)
	if !ok {
		t.Fatal("reply refused")
	}
	result, line, ok := DecodeReply(&reply)
	if !ok || result != ResultLine || string(line) != "ls" {
		t.Fatalf("reply %d %q %v", result, line, ok)
	}
}

func TestProtocolRejectsMalformedMessages(t *testing.T) {
	if _, ok := Request(OperationReadLine, []byte("\x1b[2J"), 1); ok {
		t.Fatal("control sequence in a prompt accepted")
	}
	if _, ok := Request(OperationReadLine, []byte(strings.Repeat("p", MaxPrompt+1)), 1); ok {
		t.Fatal("oversized prompt accepted")
	}
	if _, ok := Request(OperationReadLine, nil, 0); ok {
		t.Fatal("zero correlation accepted")
	}
	if _, ok := Reply(ResultInterrupt, []byte("x"), 1); ok {
		t.Fatal("data on an interrupt accepted")
	}
	request, _ := Request(OperationRedraw, []byte("> "), 1)
	request.Payload[0] = 9
	if _, _, ok := DecodeRequest(&request); ok {
		t.Fatal("unknown operation decoded")
	}
	request, _ = Request(OperationRedraw, []byte("> "), 1)
	request.HandleCount = 1
	if _, _, ok := DecodeRequest(&request); ok {
		t.Fatal("request carrying a handle decoded")
	}
	reply, _ := Reply(ResultLine, []byte("ok"), 1)
	reply.Payload[1] = 0x1b
	if result, line, ok := DecodeReply(&reply); !ok || result != ResultLine || line[0] != 0x1b {
		// Lines are data for the client: they are bounded, not filtered.
		t.Fatalf("reply %d %q %v", result, line, ok)
	}
}
