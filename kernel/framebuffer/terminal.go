package framebuffer

const (
	terminalGround uint8 = iota
	terminalEscape
	terminalCSI
)

const (
	terminalTabWidth = 8
	// A control sequence holds at most this many parameters of at most this
	// many digits; anything longer is malformed and abandoned.
	terminalMaxParameters = 8
	terminalMaxDigits     = 3
	// terminalDefaultColor selects the terminal's own foreground or
	// background instead of a palette entry.
	terminalDefaultColor = -1
	// terminalUnderlineRow is the glyph row SGR 4 sets, the one '_' uses.
	terminalUnderlineRow  = GlyphHeight - 2
	terminalUnderlineBits = uint16(0xfff0)
)

// terminalPalette is the standard 16-colour ANSI palette: colours 0-7 are the
// normal ones SGR 30-37 and 40-47 select, 8-15 the bright ones SGR 90-97 and
// 100-107 select. The ANSI numbers are the contract; these RGB values are an
// implementation detail tuned for a calm dark console.
var terminalPalette = [16][3]uint8{
	{0x0b, 0x12, 0x1c}, {0x9b, 0x4d, 0x57}, {0x5f, 0x87, 0x5f}, {0xa5, 0x8b, 0x4a},
	{0x3e, 0x5c, 0x80}, {0x80, 0x60, 0x80}, {0x3f, 0x7a, 0x80}, {0xc6, 0xc8, 0xca},
	{0x4c, 0x55, 0x63}, {0xd1, 0x6d, 0x78}, {0x8f, 0xbf, 0x7f}, {0xd6, 0xb9, 0x6b},
	{0x7f, 0xa4, 0xc8}, {0xa9, 0x87, 0xb0}, {0x7f, 0xc4, 0xcc}, {0xe6, 0xe8, 0xea},
}

// Terminal renders the bounded console control subset used by GoOS. It owns
// no history buffer and allocates no memory while consuming output.
//
// Writing the last column wraps at once: the cursor moves to the start of the
// next row, scrolling when that was the bottom row. Right after that wrap a
// newline is ignored and a carriage return goes back to the start of the full
// row, so a full row followed by LF, CR, or CR LF ends where it does on a
// terminal that defers the wrap. With autowrap off (?7l) the cursor stays on
// the last column. Backspace in column 0 moves to the last column of the row
// above.
//
// The cursor is a block drawn by inverting its cell's colour bits after each
// write and inverted back before the next one, so the cell's own pixels never
// need to be stored.
type Terminal struct {
	buffer            Buffer
	columns           int
	rows              int
	column            int
	row               int
	foreground        uint32
	background        uint32
	defaultForeground uint32
	defaultBackground uint32
	palette           [16]uint32
	// foregroundIndex and backgroundIndex are the palette entries SGR chose,
	// or terminalDefaultColor; bold brightens a normal foreground.
	foregroundIndex int8
	backgroundIndex int8
	bold            bool
	reverse         bool
	underline       bool
	// cursorHidden is set by CSI ?25l; cursorDrawn records that the cell at
	// cursorColumn, cursorRow is currently inverted.
	cursorHidden bool
	// noAutowrap is set by CSI ?7l; wrapped records that the last character
	// wrapped the cursor, which changes what LF and CR right behind it do.
	noAutowrap   bool
	wrapped      bool
	cursorDrawn  bool
	cursorColumn int
	cursorRow    int
	utf8         UTF8Decoder
	state        uint8
	// parameters counts the parameters of the sequence being parsed, digits
	// those of its last one; private marks a sequence begun with '?'.
	parameter        [terminalMaxParameters]uint16
	parameters       uint8
	digits           uint8
	private          bool
	validationMarker bool
}

func NewTerminal(buffer Buffer) (Terminal, bool) {
	columns, rows := buffer.Width()/GlyphWidth, buffer.Height()/GlyphHeight
	if columns == 0 || rows == 0 {
		return Terminal{}, false
	}
	terminal := Terminal{
		buffer: buffer, columns: columns, rows: rows,
		defaultForeground: buffer.Color(224, 224, 224), defaultBackground: buffer.Color(0, 0, 0),
	}
	for index := range terminalPalette {
		color := &terminalPalette[index]
		terminal.palette[index] = buffer.Color(color[0], color[1], color[2])
	}
	terminal.resetStyle()
	terminal.buffer.Clear(terminal.background)
	return terminal, true
}

func (terminal *Terminal) Write(data []byte) {
	if !terminal.usable() {
		return
	}
	terminal.eraseCursor()
	for _, value := range data {
		terminal.consumeByte(value)
	}
	terminal.drawCursor()
}

func (terminal *Terminal) WriteString(data string) {
	if !terminal.usable() {
		return
	}
	terminal.eraseCursor()
	for index := 0; index < len(data); index++ {
		terminal.consumeByte(data[index])
	}
	terminal.drawCursor()
}

func (terminal *Terminal) usable() bool {
	return terminal != nil && terminal.columns != 0 && terminal.rows != 0
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
	if !terminal.usable() {
		return
	}
	terminal.eraseCursor()
	terminal.consumeByte(value)
	terminal.drawCursor()
}

func (terminal *Terminal) consumeByte(value byte) {
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
	wrapped := terminal.wrapped
	terminal.wrapped = false
	switch scalar {
	case 0x1b:
		terminal.state = terminalEscape
	case '\n':
		if wrapped {
			return
		}
		terminal.column = 0
		terminal.nextRow()
	case '\r':
		terminal.column = 0
		if wrapped && terminal.row > 0 {
			terminal.row--
		}
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
		glyph := GlyphFor(scalar)
		if terminal.underline {
			glyph[terminalUnderlineRow] |= terminalUnderlineBits
		}
		foreground, background := terminal.foreground, terminal.background
		if terminal.reverse {
			foreground, background = background, foreground
		}
		terminal.buffer.DrawGlyph(terminal.column*GlyphWidth, terminal.row*GlyphHeight,
			glyph, foreground, background)
		if terminal.validationMarker && terminal.row == terminal.rows-1 && terminal.column >= terminal.columns-2 {
			DrawValidationMarker(&terminal.buffer)
		}
		if terminal.column == terminal.columns-1 && terminal.noAutowrap {
			return
		}
		terminal.column++
		if terminal.column == terminal.columns {
			terminal.column = 0
			terminal.nextRow()
			terminal.wrapped = true
		}
	}
}

func (terminal *Terminal) consumeEscape(value byte) {
	if terminal.state == terminalEscape {
		if value == '[' {
			terminal.state = terminalCSI
			terminal.parameters, terminal.digits, terminal.private = 0, 0, false
			clear(terminal.parameter[:])
		} else {
			terminal.resetEscape()
		}
		return
	}
	switch {
	case value >= '0' && value <= '9':
		if terminal.parameters == 0 {
			terminal.parameters = 1
		}
		if terminal.digits == terminalMaxDigits {
			terminal.resetEscape()
			return
		}
		last := &terminal.parameter[terminal.parameters-1]
		*last = *last*10 + uint16(value-'0')
		terminal.digits++
		return
	case value == ';':
		if terminal.parameters == 0 {
			terminal.parameters = 1
		}
		if terminal.parameters == terminalMaxParameters {
			terminal.resetEscape()
			return
		}
		terminal.parameters++
		terminal.digits = 0
		return
	case value == '?' && terminal.parameters == 0 && !terminal.private:
		terminal.private = true
		return
	case value < '@' || value > '~':
		// Intermediate bytes and stray controls are outside the subset.
		terminal.resetEscape()
		return
	}
	if terminal.private {
		terminal.dispatchPrivate(value)
	} else {
		terminal.dispatch(value)
	}
	terminal.resetEscape()
}

// argument returns parameter index, or fallback when it is absent or zero.
func (terminal *Terminal) argument(index int, fallback int) int {
	if index >= int(terminal.parameters) || terminal.parameter[index] == 0 {
		return fallback
	}
	return int(terminal.parameter[index])
}

func (terminal *Terminal) dispatch(final byte) {
	switch final {
	case 'A':
		terminal.row = clampCell(terminal.row-terminal.argument(0, 1), terminal.rows)
	case 'B':
		terminal.row = clampCell(terminal.row+terminal.argument(0, 1), terminal.rows)
	case 'C':
		terminal.column = clampCell(terminal.column+terminal.argument(0, 1), terminal.columns)
	case 'D':
		terminal.column = clampCell(terminal.column-terminal.argument(0, 1), terminal.columns)
	case 'G':
		terminal.column = clampCell(terminal.argument(0, 1)-1, terminal.columns)
	case 'H', 'f':
		terminal.row = clampCell(terminal.argument(0, 1)-1, terminal.rows)
		terminal.column = clampCell(terminal.argument(1, 1)-1, terminal.columns)
	case 'J':
		terminal.eraseDisplay(terminal.argument(0, 0))
	case 'K':
		terminal.eraseLine(terminal.argument(0, 0))
	case 'm':
		terminal.selectGraphicRendition()
	}
}

// dispatchPrivate handles the DEC private modes: ?25 shows and hides the
// cursor, ?7 turns autowrap on and off. Other modes are ignored.
func (terminal *Terminal) dispatchPrivate(final byte) {
	if final != 'h' && final != 'l' {
		return
	}
	for index := 0; index < int(terminal.parameters); index++ {
		switch terminal.parameter[index] {
		case 25:
			terminal.cursorHidden = final == 'l'
		case 7:
			terminal.noAutowrap = final == 'l'
		}
	}
}

func clampCell(value, limit int) int {
	if value < 0 {
		return 0
	}
	if value >= limit {
		return limit - 1
	}
	return value
}

// eraseDisplay clears from the cursor to the end (0), from the start to the
// cursor (1), or everything (2), in the current background. The cursor stays.
func (terminal *Terminal) eraseDisplay(mode int) {
	switch mode {
	case 0:
		terminal.eraseLine(0)
		terminal.fillRows(terminal.row+1, terminal.rows)
	case 1:
		terminal.fillRows(0, terminal.row)
		terminal.eraseLine(1)
	case 2:
		terminal.buffer.Clear(terminal.background)
		if terminal.validationMarker {
			DrawValidationMarker(&terminal.buffer)
		}
	}
}

// eraseLine clears the cursor's row from the cursor to the end (0), from the
// start to the cursor inclusive (1), or entirely (2).
func (terminal *Terminal) eraseLine(mode int) {
	first, last := terminal.column, terminal.columns
	switch mode {
	case 1:
		first, last = 0, terminal.column+1
	case 2:
		first = 0
	case 0:
	default:
		return
	}
	terminal.buffer.FillRect(first*GlyphWidth, terminal.row*GlyphHeight,
		(last-first)*GlyphWidth, GlyphHeight, terminal.background)
	if terminal.validationMarker && terminal.row == terminal.rows-1 {
		DrawValidationMarker(&terminal.buffer)
	}
}

func (terminal *Terminal) fillRows(first, last int) {
	if first >= last {
		return
	}
	terminal.buffer.FillRect(0, first*GlyphHeight, terminal.buffer.Width(), (last-first)*GlyphHeight, terminal.background)
	if terminal.validationMarker && last == terminal.rows {
		DrawValidationMarker(&terminal.buffer)
	}
}

// selectGraphicRendition applies SGR parameters in order. Reverse video (7)
// swaps the colours of the glyphs written afterwards; erasing and scrolling
// keep using the background colour itself. Unsupported parameters are ignored; the extended colour forms 38 and 48 have their operands
// skipped so they are not read as other attributes.
func (terminal *Terminal) selectGraphicRendition() {
	count := int(terminal.parameters)
	if count == 0 {
		count = 1
	}
	for index := 0; index < count; index++ {
		value := int(terminal.parameter[index])
		switch {
		case value == 0:
			terminal.resetStyle()
			continue
		case value == 1:
			terminal.bold = true
		case value == 22:
			terminal.bold = false
		case value == 4:
			terminal.underline = true
		case value == 24:
			terminal.underline = false
		case value == 7:
			terminal.reverse = true
		case value == 27:
			terminal.reverse = false
		case value >= 30 && value <= 37:
			terminal.foregroundIndex = int8(value - 30)
		case value == 39:
			terminal.foregroundIndex = terminalDefaultColor
		case value >= 40 && value <= 47:
			terminal.backgroundIndex = int8(value - 40)
		case value == 49:
			terminal.backgroundIndex = terminalDefaultColor
		case value >= 90 && value <= 97:
			terminal.foregroundIndex = int8(value - 90 + 8)
		case value >= 100 && value <= 107:
			terminal.backgroundIndex = int8(value - 100 + 8)
		case value == 38 || value == 48:
			if index+1 < count && terminal.parameter[index+1] == 5 {
				index += 2
			} else if index+1 < count && terminal.parameter[index+1] == 2 {
				index += 4
			} else {
				index = count
			}
		}
	}
	terminal.applyStyle()
}

func (terminal *Terminal) resetStyle() {
	terminal.foregroundIndex, terminal.backgroundIndex = terminalDefaultColor, terminalDefaultColor
	terminal.bold, terminal.reverse, terminal.underline = false, false, false
	terminal.applyStyle()
}

func (terminal *Terminal) applyStyle() {
	terminal.foreground = terminal.defaultForeground
	if index := terminal.foregroundIndex; index != terminalDefaultColor {
		if terminal.bold && index < 8 {
			index += 8
		}
		terminal.foreground = terminal.palette[index]
	}
	terminal.background = terminal.defaultBackground
	if terminal.backgroundIndex != terminalDefaultColor {
		terminal.background = terminal.palette[terminal.backgroundIndex]
	}
}

// drawCursor inverts the cursor cell unless the cursor is hidden. Pixels under
// the validation marker are left alone so the marker stays exact.
func (terminal *Terminal) drawCursor() {
	if terminal.cursorHidden || terminal.cursorDrawn {
		return
	}
	terminal.cursorColumn, terminal.cursorRow = terminal.column, terminal.row
	terminal.invertCursorCell()
	terminal.cursorDrawn = true
}

// eraseCursor restores the cell drawCursor inverted.
func (terminal *Terminal) eraseCursor() {
	if !terminal.cursorDrawn {
		return
	}
	terminal.invertCursorCell()
	terminal.cursorDrawn = false
}

func (terminal *Terminal) invertCursorCell() {
	buffer := &terminal.buffer
	mask := buffer.red | buffer.green | buffer.blue
	x0, y0 := terminal.cursorColumn*GlyphWidth, terminal.cursorRow*GlyphHeight
	markerX, markerY := buffer.width-ValidationMarkerSize, buffer.height-ValidationMarkerSize
	for y := y0; y < y0+GlyphHeight && y < buffer.height; y++ {
		for x := x0; x < x0+GlyphWidth && x < buffer.width; x++ {
			if terminal.validationMarker && x >= markerX && y >= markerY {
				continue
			}
			buffer.pixels[y*buffer.stride+x] ^= mask
		}
	}
}

func (terminal *Terminal) resetEscape() {
	terminal.state, terminal.parameters, terminal.digits, terminal.private = terminalGround, 0, 0, false
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
