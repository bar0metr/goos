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
	// columns is the physical terminal width. column tracks the current cursor
	// column and startColumn where the active edited line began.
	columns     int
	rows        int
	column      int
	startColumn int
	buffer      [MaxPrompt + MaxLine + 16]byte
}

const DefaultColumns = 80
const DefaultRows = 25

// SetColumns supplies the physical terminal width. Invalid values retain the
// serial-compatible fallback and cannot turn redraw arithmetic into division
// by zero or make its fixed buffer insufficient.
func (screen *Screen) SetColumns(columns int) {
	screen.SetGeometry(columns, DefaultRows)
}

// SetGeometry supplies the visible terminal dimensions.
func (screen *Screen) SetGeometry(columns, rows int) {
	if columns < 16 {
		columns = DefaultColumns
	}
	if rows < 2 {
		rows = DefaultRows
	}
	screen.columns = columns
	screen.rows = rows
}

func (screen *Screen) width() int {
	if screen.columns < 16 {
		return DefaultColumns
	}
	return screen.columns
}

func (screen *Screen) height() int {
	if screen.rows < 2 {
		return DefaultRows
	}
	return screen.rows
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
	width := screen.width()
	cursorRows := (screen.startColumn + shown) / width
	occupiedRows := (screen.startColumn + shown - 1) / width
	startColumn := screen.startColumn
	if occupiedRows >= screen.height() {
		// Earlier rows have already scrolled off the visible terminal.
		occupiedRows = screen.height() - 1
		cursorRows = occupiedRows
		startColumn = 0
	}
	if cursorRows == 0 {
		if startColumn != 0 {
			// Back over exactly the line's columns: the row also holds output.
			size += appendCSI(screen.buffer[size:], shown, 'D')
		} else {
			size += copy(screen.buffer[size:], "\r")
		}
		size += copy(screen.buffer[size:], "\x1b[K")
		screen.column = startColumn
		return screen.buffer[:size]
	}
	// Return from the wrapped cursor position to the first row, clear every
	// occupied row, then restore the cursor to the line's starting column.
	size += appendCSI(screen.buffer[size:], cursorRows, 'A')
	size += copy(screen.buffer[size:], "\r")
	size += appendCSI(screen.buffer[size:], startColumn, 'C')
	size += copy(screen.buffer[size:], "\x1b[K")
	for row := 0; row < occupiedRows; row++ {
		size += appendCSI(screen.buffer[size:], 1, 'B')
		size += copy(screen.buffer[size:], "\r\x1b[K")
	}
	size += appendCSI(screen.buffer[size:], occupiedRows, 'A')
	size += copy(screen.buffer[size:], "\r")
	size += appendCSI(screen.buffer[size:], startColumn, 'C')
	screen.column = startColumn
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
		screen.startColumn = screen.column
		return nil
	}
	size := 0
	if screen.column != 0 {
		size += copy(screen.buffer[size:], "\n")
		screen.column = 0
	}
	screen.startColumn = screen.column
	size += copy(screen.buffer[size:], prompt)
	size += copy(screen.buffer[size:], pending)
	screen.observe(screen.buffer[:size])
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
		screen.active, screen.prompt, screen.startColumn = true, 0, screen.column
	}
	screen.observe(echo)
	if echo[len(echo)-1] == '\n' {
		screen.active, screen.prompt, screen.shown = false, 0, 0
		return
	}
	screen.shown = screen.prompt + pending
}

// Output records program output the caller has written with the line hidden.
func (screen *Screen) Output(data []byte) {
	screen.observe(data)
}

// Cleared records that the whole screen was cleared and the cursor homed.
func (screen *Screen) Cleared() {
	screen.active, screen.prompt, screen.shown, screen.column, screen.startColumn = false, 0, 0, 0, 0
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

func appendCSI(target []byte, amount int, command byte) int {
	if amount == 0 {
		return 0
	}
	size := copy(target, "\x1b[")
	size += appendDecimal(target[size:], amount)
	target[size] = command
	return size + 1
}

// observe advances the cursor model for the plain terminal bytes used by the
// TTY and commands. CSI generated by Screen is never passed back through it.
func (screen *Screen) observe(data []byte) {
	width := screen.width()
	for _, value := range data {
		switch value {
		case '\n', '\r':
			screen.column = 0
		case '\b':
			if screen.column > 0 {
				screen.column--
			} else {
				screen.column = width - 1
			}
		case '\t':
			screen.column = (screen.column + 8) &^ 7
			if screen.column >= width {
				screen.column %= width
			}
		default:
			if value >= ' ' {
				screen.column++
				if screen.column == width {
					screen.column = 0
				}
			}
		}
	}
}
