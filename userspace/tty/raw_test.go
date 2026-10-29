package tty

import (
	"strings"
	"testing"

	"goos/kernel/ipc"
)

func TestRawInputReturnsWhatWasTypedUnedited(t *testing.T) {
	var input RawInput
	// Keys the line discipline would edit or swallow pass through untouched.
	typed := "a\b\x7f\x15\x04\x0c\x1b[A\x1b[11~\x1b[21~\r"
	for index := 0; index < len(typed); index++ {
		input.Feed(typed[index])
	}
	var target [MaxPart]byte
	count := input.Take(target[:])
	if string(target[:count]) != typed || input.Len() != 0 {
		t.Fatalf("raw input = %q, %d left", target[:count], input.Len())
	}
	if input.Take(target[:]) != 0 {
		t.Fatal("empty input returned bytes")
	}
}

func TestRawInputKeepsEscapeWithItsSequence(t *testing.T) {
	var input RawInput
	for _, value := range []byte("ab\x1b[11~") {
		input.Feed(value)
	}
	var target [3]byte
	if count := input.Take(target[:]); string(target[:count]) != "ab" {
		t.Fatalf("first part = %q", target[:count])
	}
	var rest [8]byte
	if count := input.Take(rest[:]); string(rest[:count]) != "\x1b[11~" {
		t.Fatalf("second part = %q", rest[:count])
	}
	// An ESC that is all that is waiting is the Escape key and goes as it is.
	input.Feed(0x1b)
	if count := input.Take(rest[:1]); count != 1 || rest[0] != 0x1b {
		t.Fatal("a lone ESC was held back")
	}
}

func TestRawInputIsBounded(t *testing.T) {
	var input RawInput
	for index := 0; index < MaxLine+10; index++ {
		input.Feed(byte('a' + index%26))
	}
	if input.Len() != MaxLine || input.Dropped() != 10 {
		t.Fatalf("len %d dropped %d", input.Len(), input.Dropped())
	}
	var target [MaxPart]byte
	total := 0
	for {
		count := input.Take(target[:])
		if count == 0 {
			break
		}
		total += count
	}
	if total != MaxLine {
		t.Fatalf("took %d", total)
	}
	input.Feed('x')
	input.Reset()
	if input.Len() != 0 {
		t.Fatal("reset kept input")
	}
}

func TestResetSequenceRestoresTheTerminal(t *testing.T) {
	for _, part := range []string{"\x1b[0m", "\x1b[?25h", "\x1b[2J", "\x1b[H"} {
		if !strings.Contains(ResetSequence, part) {
			t.Fatalf("reset lacks %q", part)
		}
	}
}

func TestModeRequestsCarryNoPrompt(t *testing.T) {
	for _, operation := range []Operation{OperationRawMode, OperationLineMode, OperationGeometry, OperationContinue, OperationTransportMode} {
		message, ok := Request(operation, nil, 7)
		if !ok {
			t.Fatalf("operation %d refused", operation)
		}
		decoded, prompt, ok := DecodeRequest(&message)
		if !ok || decoded != operation || len(prompt) != 0 {
			t.Fatalf("operation %d round trip", operation)
		}
		if _, ok := Request(operation, []byte("x"), 7); ok {
			t.Fatalf("operation %d accepted a prompt", operation)
		}
		message.PayloadSize, message.Payload[1] = 2, 'x'
		if _, _, ok := DecodeRequest(&message); ok {
			t.Fatalf("operation %d decoded with a prompt", operation)
		}
	}
	// A client cannot send the session notice, which only PID 1 hands over,
	// as a request, nor an operation past the last.
	for _, operation := range []Operation{OperationSession, OperationTransportMode + 1, 0} {
		if _, ok := Request(operation, nil, 7); ok {
			t.Fatalf("operation %d accepted", operation)
		}
	}
}

func TestRawAndGeometryReplies(t *testing.T) {
	message, ok := Reply(ResultRaw, []byte("\x1b[21~"), 9)
	if !ok {
		t.Fatal("raw reply refused")
	}
	result, data, ok := DecodeReply(&message)
	if !ok || result != ResultRaw || string(data) != "\x1b[21~" {
		t.Fatalf("raw reply = %d %q", result, data)
	}
	if _, ok := Reply(ResultRaw, nil, 9); ok {
		t.Fatal("empty raw reply accepted")
	}
	if _, ok := Reply(ResultRaw, make([]byte, MaxPart+1), 9); ok {
		t.Fatal("oversized raw reply accepted")
	}
	message, ok = GeometryReply(106, 33, true, 9)
	if !ok {
		t.Fatal("geometry reply refused")
	}
	result, data, ok = DecodeReply(&message)
	columns, rows, display, valid := DecodeGeometry(data)
	if !ok || result != ResultGeometry || !valid || columns != 106 || rows != 33 || !display {
		t.Fatalf("geometry = %d %dx%d display %v", result, columns, rows, display)
	}
	message, _ = GeometryReply(80, 25, false, 9)
	_, data, _ = DecodeReply(&message)
	if _, _, display, valid := DecodeGeometry(data); !valid || display {
		t.Fatal("serial geometry claimed a display")
	}
	if _, _, _, valid := DecodeGeometry([]byte{80, 0, 25, 0, 4, 0}); valid {
		t.Fatal("unknown geometry flag accepted")
	}
	for _, payload := range [][]byte{nil, {1, 0, 2}, {1, 0, 2, 0}, {1, 0, 2, 0, 0, 1}} {
		if _, ok := Reply(ResultGeometry, payload, 9); ok {
			t.Fatalf("geometry payload %v accepted", payload)
		}
	}
	if _, _, _, ok := DecodeGeometry([]byte{0, 0, 5, 0, 0}); ok {
		t.Fatal("zero columns accepted")
	}
	if _, ok := GeometryReply(0x10000, 1, false, 9); ok {
		t.Fatal("oversized geometry accepted")
	}
	if _, ok := Reply(ResultAccepted, []byte("x"), 9); ok {
		t.Fatal("accepted reply with data")
	}
}

func TestTransportModeKeepsTheScreen(t *testing.T) {
	var output []byte
	engine := NewEngine(func(data []byte) bool { output = append(output, data...); return true }, func(ipc.Message) bool { return true })
	engine.SetGeometry(Geometry{Columns: 80, Rows: 24, Authoritative: true})
	request, _ := Request(OperationTransportMode, nil, 1)
	engine.Request(&request)
	engine.Output([]byte("hello\r\n"))
	output = output[:0]
	request, _ = Request(OperationLineMode, nil, 2)
	engine.Request(&request)
	if len(output) != 0 {
		t.Fatalf("leaving after plain text wrote %q", output)
	}
	request, _ = Request(OperationTransportMode, nil, 3)
	engine.Request(&request)
	engine.Output([]byte("\x1b[24;1H"))
	output = output[:0]
	request, _ = Request(OperationLineMode, nil, 4)
	engine.Request(&request)
	if string(output) != TransportResetSequence+"\r\n" {
		t.Fatalf("leaving mid-line wrote %q", output)
	}
}
