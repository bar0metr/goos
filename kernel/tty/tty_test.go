package tty

import "testing"

func TestProtocolRoundTrip(t *testing.T) {
	message, ok := Message(OperationWrite, 9, []byte("hello"))
	if !ok {
		t.Fatal("write message rejected")
	}
	operation, data, ok := Parse(message)
	if !ok || operation != OperationWrite || string(data) != "hello" {
		t.Fatalf("unexpected TTY message: %d %q", operation, data)
	}
}

func TestProtocolRejectsUnknownAndOversizedMessages(t *testing.T) {
	if _, ok := Message(0, 0, nil); ok {
		t.Fatal("unknown operation accepted")
	}
	oversized := make([]byte, 256)
	if _, ok := Message(OperationWrite, 0, oversized); ok {
		t.Fatal("oversized output accepted")
	}
}

func TestLineEditor(t *testing.T) {
	var editor LineEditor
	for _, value := range []byte{'a', 'b', 8, 'c'} {
		if _, _, accepted := editor.Feed(value); !accepted {
			t.Fatalf("input %d rejected", value)
		}
	}
	line, complete, accepted := editor.Feed('\n')
	if !accepted || !complete || string(line) != "ac" {
		t.Fatalf("unexpected line: %q", line)
	}
	editor.Reset()
	if _, _, accepted := editor.Feed(0); accepted {
		t.Fatal("control byte accepted")
	}
}
