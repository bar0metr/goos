package framebuffer

const (
	terminalGround uint8 = iota
	terminalEscape
	terminalCSI
)

const terminalTabWidth = 8

// Terminal renders the bounded console control subset used by GoOS. It owns
// no history buffer and allocates no memory while consuming output.
type Terminal struct {
	buffer           Buffer
	columns          int
	rows             int
	column           int
	row              int
	foreground       uint32
	background       uint32
	utf8             UTF8Decoder
	state            uint8
	parameter        uint16
	digits           uint8
	validationMarker bool
}

func NewTerminal(buffer Buffer) (Terminal, bool) {
	columns, rows := buffer.Width()/GlyphWidth, buffer.Height()/GlyphHeight
	if columns == 0 || rows == 0 {
		return Terminal{}, false
	}
	terminal := Terminal{
		buffer: buffer, columns: columns, rows: rows,
		foreground: buffer.Color(224, 224, 224), background: buffer.Color(0, 0, 0),
	}
	terminal.buffer.Clear(terminal.background)
	return terminal, true
}

func (terminal *Terminal) Write(data []byte) {
	for _, value := range data {
		terminal.ConsumeByte(value)
	}
}

func (terminal *Terminal) WriteString(data string) {
	for index := 0; index < len(data); index++ {
		terminal.ConsumeByte(data[index])
	}
}

// EnableValidationMarker pins the validation-only checkerboard across terminal
// scrolling. Release builds never call this method.
func (terminal *Terminal) EnableValidationMarker() bool {
	if terminal == nil {
		return false
	}
	terminal.validationMarker = DrawValidationMarker(&terminal.buffer)
	return terminal.validationMarker
}

// RefreshValidationMarker restores the overlay after a complete output batch.
func (terminal *Terminal) RefreshValidationMarker() {
	if terminal != nil && terminal.validationMarker {
		DrawValidationMarker(&terminal.buffer)
	}
}

func (terminal *Terminal) ConsumeByte(value byte) {
	if terminal == nil || terminal.columns == 0 || terminal.rows == 0 {
		return
	}
	if terminal.state != terminalGround {
		terminal.consumeEscape(value)
		return
	}
	first, second, count := terminal.utf8.Decode(value)
	if count >= 1 {
		terminal.consumeScalar(first)
	}
	if count == 2 {
		terminal.consumeScalar(second)
	}
}

func (terminal *Terminal) consumeScalar(scalar rune) {
	switch scalar {
	case 0x1b:
		terminal.state = terminalEscape
	case '\n':
		terminal.column = 0
		terminal.nextRow()
	case '\r':
		terminal.column = 0
	case '\b':
		if terminal.column > 0 {
			terminal.column--
		} else if terminal.row > 0 {
			terminal.row--
			terminal.column = terminal.columns - 1
		}
	case '\t':
		next := (terminal.column/terminalTabWidth + 1) * terminalTabWidth
		if next >= terminal.columns {
			terminal.column = 0
			terminal.nextRow()
		} else {
			terminal.column = next
		}
	default:
		if scalar < 0x20 || scalar == 0x7f {
			return
		}
		terminal.buffer.DrawGlyph(terminal.column*GlyphWidth, terminal.row*GlyphHeight,
			GlyphFor(scalar), terminal.foreground, terminal.background)
		if terminal.validationMarker && terminal.row == terminal.rows-1 && terminal.column >= terminal.columns-2 {
			DrawValidationMarker(&terminal.buffer)
		}
		terminal.column++
		if terminal.column == terminal.columns {
			terminal.column = 0
			terminal.nextRow()
		}
	}
}

func (terminal *Terminal) consumeEscape(value byte) {
	if terminal.state == terminalEscape {
		if value == '[' {
			terminal.state, terminal.parameter, terminal.digits = terminalCSI, 0, 0
		} else {
			terminal.resetEscape()
		}
		return
	}
	if value >= '0' && value <= '9' {
		if terminal.digits == 3 {
			terminal.resetEscape()
			return
		}
		terminal.parameter = terminal.parameter*10 + uint16(value-'0')
		terminal.digits++
		return
	}
	switch value {
	case 'J':
		if terminal.digits == 1 && terminal.parameter == 2 {
			terminal.buffer.Clear(terminal.background)
			if terminal.validationMarker {
				DrawValidationMarker(&terminal.buffer)
			}
		}
	case 'H':
		if terminal.digits == 0 || terminal.parameter == 0 {
			terminal.column, terminal.row = 0, 0
		}
	case 'K':
		if terminal.digits == 0 || terminal.parameter == 0 {
			terminal.buffer.FillRect(terminal.column*GlyphWidth, terminal.row*GlyphHeight,
				(terminal.columns-terminal.column)*GlyphWidth, GlyphHeight, terminal.background)
			if terminal.validationMarker && terminal.row == terminal.rows-1 {
				DrawValidationMarker(&terminal.buffer)
			}
		}
	case 'D':
		amount := int(terminal.parameter)
		if terminal.digits == 0 || amount == 0 {
			amount = 1
		}
		if amount > terminal.column {
			amount = terminal.column
		}
		terminal.column -= amount
	case 'C':
		amount := int(terminal.parameter)
		if terminal.digits == 0 || amount == 0 {
			amount = 1
		}
		terminal.column += amount
		if terminal.column >= terminal.columns {
			terminal.column = terminal.columns - 1
		}
	case 'A':
		amount := int(terminal.parameter)
		if terminal.digits == 0 || amount == 0 {
			amount = 1
		}
		terminal.row -= amount
		if terminal.row < 0 {
			terminal.row = 0
		}
	case 'B':
		amount := int(terminal.parameter)
		if terminal.digits == 0 || amount == 0 {
			amount = 1
		}
		terminal.row += amount
		if terminal.row >= terminal.rows {
			terminal.row = terminal.rows - 1
		}
	}
	terminal.resetEscape()
}

func (terminal *Terminal) resetEscape() {
	terminal.state, terminal.parameter, terminal.digits = terminalGround, 0, 0
}

func (terminal *Terminal) nextRow() {
	terminal.row++
	if terminal.row < terminal.rows {
		return
	}
	if terminal.validationMarker {
		terminal.buffer.FillRect(
			terminal.buffer.Width()-ValidationMarkerSize,
			terminal.buffer.Height()-ValidationMarkerSize,
			ValidationMarkerSize,
			ValidationMarkerSize,
			terminal.background,
		)
	}
	terminal.buffer.scrollUp(GlyphHeight, terminal.background)
	if terminal.validationMarker {
		DrawValidationMarker(&terminal.buffer)
	}
	terminal.row = terminal.rows - 1
}

func (buffer *Buffer) scrollUp(pixelRows int, background uint32) {
	if buffer == nil || pixelRows <= 0 {
		return
	}
	if pixelRows >= buffer.height {
		buffer.Clear(background)
		return
	}
	if buffer.stride == buffer.width {
		copy(buffer.pixels[:(buffer.height-pixelRows)*buffer.stride],
			buffer.pixels[pixelRows*buffer.stride:buffer.height*buffer.stride])
		buffer.FillRect(0, buffer.height-pixelRows, buffer.width, pixelRows, background)
		return
	}
	for row := 0; row < buffer.height-pixelRows; row++ {
		target := buffer.pixels[row*buffer.stride : row*buffer.stride+buffer.width]
		source := buffer.pixels[(row+pixelRows)*buffer.stride : (row+pixelRows)*buffer.stride+buffer.width]
		copy(target, source)
	}
	buffer.FillRect(0, buffer.height-pixelRows, buffer.width, pixelRows, background)
}
