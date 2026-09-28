package terminal

import "testing"

// feed decodes input a byte at a time and flushes at the end, as a program
// does with one read.
func feed(parser *Parser, input string) []Event {
	var events []Event
	for index := 0; index < len(input); index++ {
		if event, ok := parser.Feed(input[index]); ok {
			events = append(events, event)
		}
	}
	if event, ok := parser.Flush(); ok {
		events = append(events, event)
	}
	return events
}

func TestParserFunctionKeysByteAtATime(t *testing.T) {
	sequences := []string{"\x1b[11~", "\x1b[12~", "\x1b[13~", "\x1b[14~", "\x1b[15~", "\x1b[17~",
		"\x1b[18~", "\x1b[19~", "\x1b[20~", "\x1b[21~", "\x1b[23~", "\x1b[24~"}
	for index, sequence := range sequences {
		var parser Parser
		var events []Event
		// Each byte is its own read: a split sequence still decodes once.
		for position := 0; position < len(sequence); position++ {
			if event, ok := parser.Feed(sequence[position]); ok {
				events = append(events, event)
			}
			if position > 0 {
				if event, ok := parser.Flush(); ok {
					events = append(events, event)
				}
			}
		}
		if len(events) != 1 || events[0].Key != KeyF1+Key(index) {
			t.Fatalf("F%d: %#v", index+1, events)
		}
	}
}

func TestParserNavigationKeys(t *testing.T) {
	tests := map[string]Key{
		"\x1b[A": KeyUp, "\x1b[B": KeyDown, "\x1b[C": KeyRight, "\x1b[D": KeyLeft,
		"\x1b[H": KeyHome, "\x1b[F": KeyEnd, "\x1b[2~": KeyInsert, "\x1b[3~": KeyDelete,
		"\x1b[5~": KeyPageUp, "\x1b[6~": KeyPageDown,
		// Common alternates from host terminals on COM1.
		"\x1b[1~": KeyHome, "\x1b[4~": KeyEnd, "\x1b[7~": KeyHome, "\x1b[8~": KeyEnd,
		"\x1bOH": KeyHome, "\x1bOF": KeyEnd, "\x1bOA": KeyUp, "\x1bOP": KeyF1, "\x1bOS": KeyF4,
		"\x1b[[A": KeyF1, "\x1b[[E": KeyF5, "\x1b[1;5A": KeyUp, "\x1b[1;2P": KeyF1, "\x1b[15;2~": KeyF5,
	}
	for input, want := range tests {
		var parser Parser
		if events := feed(&parser, input); len(events) != 1 || events[0].Key != want {
			t.Fatalf("%q = %#v, want %d", input, events, want)
		}
	}
}

func TestParserControlsAndText(t *testing.T) {
	var parser Parser
	events := feed(&parser, "a\r\n\x08\x7f\t\x12Z")
	want := []Event{{Key: KeyRune, Rune: 'a'}, {Key: KeyEnter}, {Key: KeyEnter}, {Key: KeyBackspace},
		{Key: KeyBackspace}, {Key: KeyTab}, {Key: KeyRune, Rune: 'r', Ctrl: true}, {Key: KeyRune, Rune: 'Z'}}
	if len(events) != len(want) {
		t.Fatalf("events = %#v", events)
	}
	for index := range want {
		if events[index] != want[index] {
			t.Fatalf("event %d = %#v, want %#v", index, events[index], want[index])
		}
	}
}

func TestParserUTF8(t *testing.T) {
	var parser Parser
	events := feed(&parser, "ЖёÄ€😀")
	want := []rune{'Ж', 'ё', 'Ä', '€', '😀'}
	if len(events) != len(want) {
		t.Fatalf("events = %#v", events)
	}
	for index := range want {
		if events[index].Key != KeyRune || events[index].Rune != want[index] {
			t.Fatalf("event %d = %#v", index, events[index])
		}
	}
	// Overlong, surrogate, stray continuation and truncated input are dropped
	// without taking the next character with them.
	events = feed(&parser, "\xc0\xaf\xed\xa0\x80\x80\xd0x")
	if len(events) != 1 || events[0].Rune != 'x' {
		t.Fatalf("malformed UTF-8 = %#v", events)
	}
}

func TestParserEscapeKey(t *testing.T) {
	var parser Parser
	if events := feed(&parser, "\x1b"); len(events) != 1 || events[0].Key != KeyEscape {
		t.Fatalf("lone ESC = %#v", events)
	}
	if events := feed(&parser, "\x1b\x1b[A"); len(events) != 2 || events[0].Key != KeyEscape || events[1].Key != KeyUp {
		t.Fatalf("ESC then up = %#v", events)
	}
	if events := feed(&parser, "\x1bx"); len(events) != 1 || events[0].Rune != 'x' || !events[0].Alt {
		t.Fatalf("alt-x = %#v", events)
	}
	// A sequence still open at the end of a read waits for the rest.
	if events := feed(&parser, "\x1b[1"); len(events) != 0 {
		t.Fatalf("open sequence = %#v", events)
	}
	if events := feed(&parser, "1~"); len(events) != 1 || events[0].Key != KeyF1 {
		t.Fatalf("completed sequence = %#v", events)
	}
}

func TestParserBackToBackAndMalformed(t *testing.T) {
	var parser Parser
	events := feed(&parser, "\x1b[21~\x1b[B\x1b[99~\x1b[1234~q\x1b[1;2;3Aw\x1b[Zk\x1b[5~")
	want := []Key{KeyF10, KeyDown, KeyRune, KeyRune, KeyRune, KeyPageUp}
	if len(events) != len(want) {
		t.Fatalf("events = %#v", events)
	}
	for index := range want {
		if events[index].Key != want[index] {
			t.Fatalf("event %d = %#v", index, events[index])
		}
	}
}

func TestParserAllocatesNothing(t *testing.T) {
	var parser Parser
	input := []byte("\x1b[11~abcЖ\x1b[A\x1b")
	allocations := testing.AllocsPerRun(100, func() {
		for _, value := range input {
			parser.Feed(value)
		}
		parser.Flush()
	})
	if allocations != 0 {
		t.Fatalf("parser allocated %.1f objects", allocations)
	}
}
