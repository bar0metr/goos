package tty

// Event is what one input byte did to the line.
type Event uint8

const (
	// EventNone changed nothing a caller must act on.
	EventNone Event = iota
	// EventEcho means Echo holds bytes to write to the terminal.
	EventEcho
	// EventLine means a line is complete; Line returns it until Next.
	EventLine
	// EventInterrupt means Ctrl-C was typed; the line was discarded.
	EventInterrupt
	// EventEndOfInput means Ctrl-D was typed on an empty line.
	EventEndOfInput
	// EventRedraw means the terminal was cleared and the prompt and line
	// should be shown again.
	EventRedraw
)

const (
	keyInterrupt   = 0x03
	keyEndOfInput  = 0x04
	keyBackspace   = 0x08
	keyClearScreen = 0x0c
	keyKillLine    = 0x15
	keyEscape      = 0x1b
	keyDelete      = 0x7f
)

// escape tracks a terminal escape sequence being skipped. Keys such as the
// arrows arrive as ESC [ A; without this their tail would be typed as text.
type escape uint8

const (
	escapeNone escape = iota
	escapeStarted
	escapeSequence
)

// Discipline is the terminal's line editor. It works on one line at a time and
// allocates nothing; echo is returned for the caller to write.
type Discipline struct {
	data     [MaxLine]byte
	size     int
	complete bool
	escape   escape
	echo     [3 * MaxLine]byte
	echoSize int
}

// Feed applies one input byte. A completed line stays available through Line
// until Next is called; input meanwhile only ends it with Ctrl-C.
func (line *Discipline) Feed(value byte) Event {
	line.echoSize = 0
	if value == keyInterrupt {
		line.discard()
		line.appendEcho("^C\n")
		return EventInterrupt
	}
	if line.complete {
		return EventNone
	}
	switch line.escape {
	case escapeStarted:
		line.escape = escapeNone
		if value == '[' || value == 'O' {
			line.escape = escapeSequence
		}
		return EventNone
	case escapeSequence:
		// A control sequence ends with a byte from @ to ~.
		if value >= '@' && value <= '~' {
			line.escape = escapeNone
		}
		return EventNone
	}
	switch value {
	case '\r', '\n':
		line.complete = true
		line.appendEcho("\n")
		return EventLine
	case keyBackspace, keyDelete:
		if line.size == 0 {
			return EventNone
		}
		line.size--
		line.data[line.size] = 0
		line.appendEcho("\b \b")
		return EventEcho
	case keyKillLine:
		for line.size != 0 {
			line.size--
			line.appendEcho("\b \b")
		}
		line.discard()
		return EventEcho
	case keyEndOfInput:
		if line.size != 0 {
			return EventNone
		}
		return EventEndOfInput
	case keyClearScreen:
		return EventRedraw
	case keyEscape:
		line.escape = escapeStarted
		return EventNone
	}
	if value < ' ' || value > '~' || line.size == MaxLine {
		return EventNone
	}
	line.data[line.size] = value
	line.size++
	line.echo[0], line.echoSize = value, 1
	return EventEcho
}

// Line returns the completed line, or false while one is being typed.
func (line *Discipline) Line() ([]byte, bool) {
	return line.data[:line.size], line.complete
}

// Next discards the completed line and starts the next one.
func (line *Discipline) Next() { line.discard() }

// Pending returns what has been typed of the current line.
func (line *Discipline) Pending() []byte {
	if line.complete {
		return nil
	}
	return line.data[:line.size]
}

// Echo returns the bytes the last Feed asks to be written.
func (line *Discipline) Echo() []byte { return line.echo[:line.echoSize] }

func (line *Discipline) discard() {
	clear(line.data[:])
	line.size, line.complete, line.escape = 0, false, escapeNone
}

func (line *Discipline) appendEcho(text string) {
	line.echoSize += copy(line.echo[line.echoSize:], text)
}
