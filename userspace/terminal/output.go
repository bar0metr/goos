// Package terminal is the userspace side of the GoOS terminal contract: the
// bounded ANSI output subset the console renders (cursor addressing, erase,
// 16 SGR colours, bold, underline, reverse video, cursor visibility) and the
// key sequences it delivers. Full-screen programs use
// it instead of writing escape strings by hand. It knows nothing of the
// framebuffer or COM1: the same bytes drive both (ADR 0124).
package terminal

// Color is one of the 16 ANSI colours, or Default for the terminal's own
// foreground or background. The numbers are the ANSI ones: 0-7 normal, 8-15
// bright. The console chooses the RGB values.
type Color int8

const (
	Default Color = iota - 1
	Black
	Red
	Green
	Yellow
	Blue
	Magenta
	Cyan
	White
	BrightBlack
	BrightRed
	BrightGreen
	BrightYellow
	BrightBlue
	BrightMagenta
	BrightCyan
	BrightWhite
)

// Output assembles terminal output in a caller-provided buffer and hands it to
// sink whole when the buffer fills or Flush is called. It allocates nothing;
// one batch per frame keeps a redraw from interleaving with other output.
type Output struct {
	buffer []byte
	size   int
	sink   func([]byte)
}

// NewOutput writes through sink using storage as the batch buffer.
func NewOutput(storage []byte, sink func([]byte)) Output {
	return Output{buffer: storage, sink: sink}
}

// Flush hands everything assembled so far to the sink.
func (output *Output) Flush() {
	if output.size != 0 && output.sink != nil {
		output.sink(output.buffer[:output.size])
	}
	output.size = 0
}

// Pending returns what has been assembled and not yet flushed; Discard drops
// it. Together they let a caller compose a piece of output, such as one row,
// and decide whether to send it.
func (output *Output) Pending() []byte { return output.buffer[:output.size] }
func (output *Output) Discard()        { output.size = 0 }

// Write appends bytes as they are. Callers that print names use Text, which
// neutralises control bytes.
func (output *Output) Write(value []byte) {
	for len(value) != 0 {
		if output.size == len(output.buffer) {
			output.Flush()
			if len(output.buffer) == 0 {
				return
			}
		}
		copied := copy(output.buffer[output.size:], value)
		output.size += copied
		value = value[copied:]
	}
}

// WriteString appends text as it is.
func (output *Output) WriteString(value string) {
	for index := 0; index < len(value); index++ {
		output.writeByte(value[index])
	}
}

func (output *Output) writeByte(value byte) {
	if output.size == len(output.buffer) {
		output.Flush()
		if len(output.buffer) == 0 {
			return
		}
	}
	output.buffer[output.size] = value
	output.size++
}

func (output *Output) writeNumber(value int) {
	if value < 0 {
		value = 0
	}
	var digits [10]byte
	position := len(digits)
	for {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	output.Write(digits[position:])
}

// MoveTo places the cursor at a zero-based row and column (CUP).
func (output *Output) MoveTo(row, column int) {
	output.WriteString("\x1b[")
	output.writeNumber(row + 1)
	output.writeByte(';')
	output.writeNumber(column + 1)
	output.writeByte('H')
}

// ClearScreen erases the whole screen in the current background and homes
// the cursor.
func (output *Output) ClearScreen() { output.WriteString("\x1b[2J\x1b[H") }

// EraseLine erases the cursor's whole row; EraseToEnd from the cursor on.
func (output *Output) EraseLine()  { output.WriteString("\x1b[2K") }
func (output *Output) EraseToEnd() { output.WriteString("\x1b[K") }

// RequestSize asks the terminal how large it is: the cursor goes to the
// bottom-right corner, where the terminal clamps it, and DSR 6 asks where it
// is. A terminal that answers sends ESC [ rows ; columns R, which Parser
// returns as a KeyPosition event; the framebuffer console does not answer.
// The cursor is left in the corner, so nothing may be written after it
// before the next MoveTo.
func (output *Output) RequestSize() { output.WriteString("\x1b[999;999H\x1b[6n") }

// HideCursor and ShowCursor switch the console's block cursor off and on.
func (output *Output) HideCursor() { output.WriteString("\x1b[?25l") }
func (output *Output) ShowCursor() { output.WriteString("\x1b[?25h") }

// ResetStyle returns to the terminal's default colours (SGR 0).
func (output *Output) ResetStyle() { output.WriteString("\x1b[0m") }

// SetStyle selects a foreground and background in one sequence.
func (output *Output) SetStyle(foreground, background Color) {
	output.WriteString("\x1b[")
	output.writeNumber(ForegroundCode(foreground))
	output.writeByte(';')
	output.writeNumber(BackgroundCode(background))
	output.writeByte('m')
}

func (output *Output) SetForeground(color Color) {
	output.WriteString("\x1b[")
	output.writeNumber(ForegroundCode(color))
	output.writeByte('m')
}

func (output *Output) SetBackground(color Color) {
	output.WriteString("\x1b[")
	output.writeNumber(BackgroundCode(color))
	output.writeByte('m')
}

// Reset leaves the terminal as a shell expects it: default colours, a
// visible cursor, a clear screen and the cursor home.
func (output *Output) Reset() {
	output.ResetStyle()
	output.ShowCursor()
	output.ClearScreen()
}

// ForegroundCode is the SGR parameter selecting color as the foreground.
func ForegroundCode(color Color) int {
	switch {
	case color >= Black && color <= White:
		return 30 + int(color)
	case color >= BrightBlack && color <= BrightWhite:
		return 90 + int(color-BrightBlack)
	}
	return 39
}

// BackgroundCode is the SGR parameter selecting color as the background.
func BackgroundCode(color Color) int {
	switch {
	case color >= Black && color <= White:
		return 40 + int(color)
	case color >= BrightBlack && color <= BrightWhite:
		return 100 + int(color-BrightBlack)
	}
	return 49
}

// Text writes value in exactly width columns: cut at a character boundary
// when longer, padded with spaces when shorter. Controls are shown as '?'
// (Printable), so a file name cannot carry escape sequences to the terminal.
// It returns the columns the text itself took.
func (output *Output) Text(value []byte, width int) int {
	used := 0
	for index := 0; index < len(value) && used < width; {
		var character []byte
		character, index = Printable(value, index)
		output.Write(character)
		used++
	}
	output.Pad(width - used)
	return used
}

// TextString is Text for a string literal.
func (output *Output) TextString(value string, width int) int {
	used := 0
	for index := 0; index < len(value) && used < width; {
		size := characterSize(value, index)
		output.WriteString(value[index : index+size])
		used++
		index += size
	}
	output.Pad(width - used)
	return used
}

// Pad writes count spaces.
func (output *Output) Pad(count int) {
	for ; count > 0; count-- {
		output.writeByte(' ')
	}
}

// Repeat writes character count times.
func (output *Output) Repeat(character string, count int) {
	for ; count > 0; count-- {
		output.WriteString(character)
	}
}
