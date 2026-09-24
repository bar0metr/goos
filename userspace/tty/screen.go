package tty

// Screen tracks what the terminal service has put on the console, so that the
// line being edited can be taken off the screen before output is written and
// put back after it (ADR 0089). Every byte the service writes passes through
// it; it allocates nothing and returns the bytes to write.
//
// The line is the prompt of the outstanding read followed by what has been
// typed. Editing only happens at its end, so the cursor is always after it,
// and taking it away is going back over the columns it holds and clearing to
// the end of the line.
type Screen struct {
	// active is set while an edited line is on the screen, from when it is
	// shown or its first key is echoed until it ends or is taken away.
	active bool
	// prompt and shown are the columns the prompt and the whole line hold.
	prompt int
	shown  int
	// fromMid records whether the line began after text on the same row, so
	// that taking it away returns to that text rather than to column zero.
	fromMid bool
	// midLine is set when the cursor is not at the start of a row.
	midLine bool
	buffer  [MaxPrompt + MaxLine + 16]byte
}

// Hide returns the bytes that take the line off the screen, leaving the cursor
// where the line began. Nothing is returned when no line is shown.
func (screen *Screen) Hide() []byte {
	if !screen.active {
		return nil
	}
	screen.active = false
	shown := screen.shown
	screen.prompt, screen.shown = 0, 0
	if shown == 0 {
		return nil
	}
	size := 0
	if screen.fromMid {
		// Back over exactly the line's columns: the row also holds output.
		size += copy(screen.buffer[size:], "\x1b[")
		size += appendDecimal(screen.buffer[size:], shown)
		size += copy(screen.buffer[size:], "D")
	} else {
		size += copy(screen.buffer[size:], "\r")
	}
	size += copy(screen.buffer[size:], "\x1b[K")
	screen.midLine = screen.fromMid
	return screen.buffer[:size]
}

// Show returns the bytes that put a line on the screen: the prompt and what
// has been typed. When output has left the cursor inside a row and there is
// something to show, the line starts on the next row, so a prompt never trails
// a program's unfinished output. The caller hides any line shown before.
func (screen *Screen) Show(prompt, pending []byte) []byte {
	screen.active = true
	screen.prompt = len(prompt)
	screen.shown = len(prompt) + len(pending)
	if screen.shown == 0 {
		screen.fromMid = screen.midLine
		return nil
	}
	size := 0
	if screen.midLine {
		size += copy(screen.buffer[size:], "\n")
	}
	screen.fromMid = false
	size += copy(screen.buffer[size:], prompt)
	size += copy(screen.buffer[size:], pending)
	screen.midLine = true
	return screen.buffer[:size]
}

// Echoed records the echo of one key, which the caller writes, and the length
// of the line typed so far. An echo that ends the row ends the line.
func (screen *Screen) Echoed(echo []byte, pending int) {
	if len(echo) == 0 {
		return
	}
	if !screen.active {
		// Typing with no prompt shown starts a line of its own where the
		// cursor is.
		screen.active, screen.prompt, screen.fromMid = true, 0, screen.midLine
	}
	if echo[len(echo)-1] == '\n' {
		screen.active, screen.prompt, screen.shown, screen.midLine = false, 0, 0, false
		return
	}
	screen.shown = screen.prompt + pending
	screen.midLine = screen.shown != 0 || screen.fromMid
}

// Output records program output the caller has written with the line hidden.
func (screen *Screen) Output(data []byte) {
	if len(data) != 0 {
		screen.midLine = data[len(data)-1] != '\n'
	}
}

// Cleared records that the whole screen was cleared and the cursor homed.
func (screen *Screen) Cleared() {
	screen.active, screen.prompt, screen.shown, screen.midLine, screen.fromMid = false, 0, 0, false, false
}

// Shown reports whether a line is on the screen and how many columns it holds.
func (screen *Screen) Shown() (bool, int) { return screen.active, screen.shown }

func appendDecimal(target []byte, value int) int {
	var digits [8]byte
	position := len(digits)
	for {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	return copy(target, digits[position:])
}
