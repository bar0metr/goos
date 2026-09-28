package framebuffer

import "testing"

func testTerminal(t *testing.T, columns, rows, padding int) (Terminal, []uint32) {
	t.Helper()
	width, height, stride := columns*GlyphWidth, rows*GlyphHeight, columns*GlyphWidth+padding
	pixels := make([]uint32, stride*height)
	buffer := Buffer{pixels: pixels, width: width, height: height, stride: stride,
		red: 0xff, green: 0xff00, blue: 0xff0000}
	terminal, ok := NewTerminal(buffer)
	if !ok {
		t.Fatal("terminal geometry rejected")
	}
	return terminal, pixels
}

func TestTerminalRendersASCIIUTF8AndReplacement(t *testing.T) {
	terminal, _ := testTerminal(t, 8, 2, 3)
	terminal.Write([]byte{'A', 0xd0})
	terminal.Write([]byte{0x91, 0xff})
	if terminal.column != 3 || terminal.row != 0 {
		t.Fatalf("cursor = %d,%d", terminal.column, terminal.row)
	}
	drawn := false
	for y := 0; y < GlyphHeight && !drawn; y++ {
		for x := 0; x < GlyphWidth; x++ {
			drawn = terminal.buffer.pixels[y*terminal.buffer.stride+x] != terminal.background
			if drawn {
				break
			}
		}
	}
	if !drawn {
		t.Fatal("ASCII glyph was not rendered")
	}
}

func TestTerminalPlainControls(t *testing.T) {
	terminal, _ := testTerminal(t, 16, 3, 0)
	terminal.Write([]byte("abc\bX\tY\rZ\nQ"))
	if terminal.column != 1 || terminal.row != 1 {
		t.Fatalf("cursor = %d,%d", terminal.column, terminal.row)
	}
}

func TestTerminalIncrementalCSI(t *testing.T) {
	terminal, _ := testTerminal(t, 16, 3, 0)
	terminal.Write([]byte("abcdef"))
	terminal.Write([]byte("\x1b["))
	terminal.Write([]byte("3"))
	terminal.Write([]byte("D"))
	if terminal.column != 3 {
		t.Fatalf("cursor after CSI D = %d", terminal.column)
	}
	terminal.Write([]byte("\x1b[K"))
	for x := terminal.column * GlyphWidth; x < terminal.buffer.width; x++ {
		for y := 0; y < GlyphHeight; y++ {
			if terminal.buffer.pixels[y*terminal.buffer.stride+x] != terminal.background {
				t.Fatalf("erase-to-end left pixel at %d,%d", x, y)
			}
		}
	}
	terminal.Write([]byte("\x1b[2J\x1b[H"))
	if terminal.column != 0 || terminal.row != 0 {
		t.Fatalf("cursor after clear/home = %d,%d", terminal.column, terminal.row)
	}
}

func TestTerminalMovesAcrossRowsForWrappedRedraw(t *testing.T) {
	terminal, _ := testTerminal(t, 16, 4, 0)
	terminal.Write([]byte("abcdefghijklmnopq"))
	if terminal.column != 1 || terminal.row != 1 {
		t.Fatalf("wrapped cursor = %d,%d", terminal.column, terminal.row)
	}
	terminal.Write([]byte("\x1b[1A\r\x1b[6C"))
	if terminal.column != 6 || terminal.row != 0 {
		t.Fatalf("cursor after up/right = %d,%d", terminal.column, terminal.row)
	}
	terminal.Write([]byte("\x1b[2B\x1b[99C\x1b[9A"))
	if terminal.column != 15 || terminal.row != 0 {
		t.Fatalf("clamped cursor = %d,%d", terminal.column, terminal.row)
	}
}

func TestTerminalBackspaceCrossesAWrappedRow(t *testing.T) {
	terminal, _ := testTerminal(t, 16, 3, 0)
	terminal.Write([]byte("abcdefghijklmnop"))
	if terminal.column != 0 || terminal.row != 1 {
		t.Fatalf("wrapped cursor = %d,%d", terminal.column, terminal.row)
	}
	terminal.Write([]byte("\b \b"))
	if terminal.column != 15 || terminal.row != 0 {
		t.Fatalf("cursor after wrapped erase = %d,%d", terminal.column, terminal.row)
	}
}

func TestTerminalScrollsWithoutTouchingPadding(t *testing.T) {
	terminal, pixels := testTerminal(t, 2, 2, 2)
	stride := terminal.buffer.stride
	terminal.Write([]byte("A\nB\n"))
	if terminal.row != 1 || terminal.column != 0 {
		t.Fatalf("cursor after scroll = %d,%d", terminal.column, terminal.row)
	}
	for row := 0; row < terminal.buffer.height; row++ {
		for column := terminal.buffer.width; column < stride; column++ {
			if pixels[row*stride+column] != 0 {
				t.Fatalf("padding changed at %d,%d", column, row)
			}
		}
	}
}

func TestTerminalWriteAllocatesNothing(t *testing.T) {
	terminal, _ := testTerminal(t, 16, 4, 0)
	data := []byte("GoOS Привет\n\x1b[2D!\n")
	if allocations := testing.AllocsPerRun(100, func() { terminal.Write(data) }); allocations != 0 {
		t.Fatalf("terminal write allocated %.1f objects", allocations)
	}
}

func TestTerminalMalformedCSIResetsBoundedParser(t *testing.T) {
	terminal, _ := testTerminal(t, 8, 2, 0)
	terminal.Write([]byte("\x1b[1234DA"))
	if terminal.state != terminalGround || terminal.column != 2 {
		t.Fatalf("parser state %d cursor %d", terminal.state, terminal.column)
	}
}

func TestTerminalValidationMarkerRemainsPinnedAcrossScroll(t *testing.T) {
	terminal, pixels := testTerminal(t, 80, 25, 0)
	if !terminal.EnableValidationMarker() {
		t.Fatal("validation marker was not enabled")
	}
	for row := 0; row < terminal.rows+3; row++ {
		terminal.WriteString("scroll\n")
	}
	for column := 0; column < terminal.columns; column++ {
		terminal.ConsumeByte('X')
	}
	terminal.WriteString("\x1b[2J\x1b[K")
	terminal.buffer.FillRect(terminal.buffer.Width()-ValidationMarkerSize,
		terminal.buffer.Height()-ValidationMarkerSize, ValidationMarkerSize, ValidationMarkerSize, terminal.background)
	terminal.RefreshValidationMarker()
	magenta := terminal.buffer.Color(255, 0, 255)
	cyan := terminal.buffer.Color(0, 255, 255)
	markerPixels := 0
	for y := 0; y < terminal.buffer.Height(); y++ {
		for x := 0; x < terminal.buffer.Width(); x++ {
			pixel := pixels[y*terminal.buffer.stride+x]
			if pixel != magenta && pixel != cyan {
				continue
			}
			markerPixels++
			if x < terminal.buffer.Width()-ValidationMarkerSize || y < terminal.buffer.Height()-ValidationMarkerSize {
				t.Fatalf("validation marker trail at (%d,%d)", x, y)
			}
		}
	}
	if markerPixels != ValidationMarkerSize*ValidationMarkerSize {
		t.Fatalf("validation marker has %d pixels", markerPixels)
	}
}

// cellPixel returns the pixel at the top-left of cell column,row.
func cellPixel(terminal *Terminal, column, row int) uint32 {
	return terminal.buffer.pixels[row*GlyphHeight*terminal.buffer.stride+column*GlyphWidth]
}

func TestTerminalAbsoluteCursorPosition(t *testing.T) {
	terminal, _ := testTerminal(t, 20, 10, 0)
	terminal.Write([]byte("\x1b[4;7H"))
	if terminal.row != 3 || terminal.column != 6 {
		t.Fatalf("CUP cursor = %d,%d", terminal.column, terminal.row)
	}
	terminal.Write([]byte("\x1b[;3H"))
	if terminal.row != 0 || terminal.column != 2 {
		t.Fatalf("CUP with empty row = %d,%d", terminal.column, terminal.row)
	}
	terminal.Write([]byte("\x1b[99;999H"))
	if terminal.row != 9 || terminal.column != 19 {
		t.Fatalf("clamped CUP = %d,%d", terminal.column, terminal.row)
	}
	terminal.Write([]byte("\x1b[5G"))
	if terminal.row != 9 || terminal.column != 4 {
		t.Fatalf("CHA = %d,%d", terminal.column, terminal.row)
	}
	terminal.Write([]byte("\x1b[H"))
	if terminal.row != 0 || terminal.column != 0 {
		t.Fatalf("home = %d,%d", terminal.column, terminal.row)
	}
}

func TestTerminalEraseLineAndDisplay(t *testing.T) {
	terminal, _ := testTerminal(t, 10, 4, 0)
	for row := 0; row < 4; row++ {
		terminal.Write([]byte("\x1b[47m"))
		terminal.Write([]byte("\x1b[" + string(rune('1'+row)) + ";1H##########"))
	}
	terminal.Write([]byte("\x1b[0m"))
	white := terminal.palette[7]
	if cellPixel(&terminal, 0, 0) != white {
		t.Fatal("background fill was not drawn")
	}
	terminal.Write([]byte("\x1b[2;5H\x1b[2K"))
	if cellPixel(&terminal, 0, 1) != terminal.defaultBackground || cellPixel(&terminal, 9, 1) != terminal.defaultBackground {
		t.Fatal("ESC[2K left the line")
	}
	if terminal.row != 1 || terminal.column != 4 {
		t.Fatalf("ESC[2K moved the cursor to %d,%d", terminal.column, terminal.row)
	}
	terminal.Write([]byte("\x1b[3;4H\x1b[1K"))
	if cellPixel(&terminal, 3, 2) != terminal.defaultBackground || cellPixel(&terminal, 4, 2) != white {
		t.Fatal("ESC[1K erased the wrong cells")
	}
	terminal.Write([]byte("\x1b[1;6H\x1b[J"))
	if cellPixel(&terminal, 4, 0) != white || cellPixel(&terminal, 5, 0) != terminal.defaultBackground ||
		cellPixel(&terminal, 0, 3) != terminal.defaultBackground {
		t.Fatal("ESC[J erased the wrong cells")
	}
}

func TestTerminalSGRColors(t *testing.T) {
	terminal, _ := testTerminal(t, 40, 2, 0)
	for index := 0; index < 8; index++ {
		terminal.Write([]byte("\x1b[" + string(rune('3')) + string(rune('0'+index)) + "m"))
		if terminal.foreground != terminal.palette[index] {
			t.Fatalf("SGR 3%d foreground", index)
		}
		terminal.Write([]byte("\x1b[4" + string(rune('0'+index)) + "m"))
		if terminal.background != terminal.palette[index] {
			t.Fatalf("SGR 4%d background", index)
		}
		terminal.Write([]byte("\x1b[9" + string(rune('0'+index)) + "m"))
		if terminal.foreground != terminal.palette[8+index] {
			t.Fatalf("SGR 9%d bright foreground", index)
		}
		terminal.Write([]byte("\x1b[10" + string(rune('0'+index)) + "m"))
		if terminal.background != terminal.palette[8+index] {
			t.Fatalf("SGR 10%d bright background", index)
		}
	}
	terminal.Write([]byte("\x1b[39;49m"))
	if terminal.foreground != terminal.defaultForeground || terminal.background != terminal.defaultBackground {
		t.Fatal("SGR 39;49 did not restore defaults")
	}
	terminal.Write([]byte("\x1b[1;31m"))
	if terminal.foreground != terminal.palette[9] {
		t.Fatal("bold did not brighten a normal foreground")
	}
	terminal.Write([]byte("\x1b[m"))
	if terminal.foreground != terminal.defaultForeground || terminal.bold {
		t.Fatal("ESC[m did not reset")
	}
}

func TestTerminalGlyphUsesSelectedColors(t *testing.T) {
	terminal, _ := testTerminal(t, 4, 1, 0)
	terminal.Write([]byte("\x1b[93;44m "))
	if cellPixel(&terminal, 0, 0) != terminal.palette[4] {
		t.Fatal("space was not drawn in the selected background")
	}
	terminal.Write([]byte("\x1b[1;1H#"))
	found := false
	for y := 0; y < GlyphHeight; y++ {
		for x := 0; x < GlyphWidth; x++ {
			if terminal.buffer.pixels[y*terminal.buffer.stride+x] == terminal.palette[11] {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("glyph was not drawn in the selected foreground")
	}
}

func TestTerminalSplitSGRAndCUP(t *testing.T) {
	terminal, _ := testTerminal(t, 20, 5, 0)
	for _, part := range []string{"\x1b", "[", "3", ";", "1", "2", "H", "\x1b[", "9", "2;", "4", "5m"} {
		terminal.Write([]byte(part))
	}
	if terminal.row != 2 || terminal.column != 11 {
		t.Fatalf("split CUP = %d,%d", terminal.column, terminal.row)
	}
	if terminal.foreground != terminal.palette[10] || terminal.background != terminal.palette[5] {
		t.Fatal("split SGR was not applied")
	}
}

func TestTerminalRejectsMalformedParameterLists(t *testing.T) {
	terminal, _ := testTerminal(t, 20, 5, 0)
	terminal.Write([]byte("\x1b[1;2;3;4;5;6;7;8;9H"))
	if terminal.state != terminalGround || terminal.row != 0 {
		t.Fatalf("too many parameters: state %d row %d", terminal.state, terminal.row)
	}
	terminal.Write([]byte("\x1b[1;2;3;4;5;6;7;8m"))
	if terminal.state != terminalGround {
		t.Fatal("eight parameters were not accepted")
	}
	terminal.Write([]byte("\x1b[3;99999H"))
	if terminal.state != terminalGround || terminal.row != 0 {
		t.Fatalf("long parameter: state %d row %d", terminal.state, terminal.row)
	}
	terminal.Write([]byte("\x1b[5\x01H"))
	if terminal.state != terminalGround {
		t.Fatal("stray control did not abandon the sequence")
	}
	terminal.Write([]byte("\x1b[?25l\x1b[?25h\x1b[ q"))
	if terminal.state != terminalGround {
		t.Fatal("private or intermediate sequence left the parser busy")
	}
}

func TestTerminalIgnoresUnsupportedSGR(t *testing.T) {
	terminal, _ := testTerminal(t, 20, 2, 0)
	terminal.Write([]byte("\x1b[32m\x1b[4;5;7;38;5;196;48;2;1;2;3m"))
	if terminal.foreground != terminal.palette[2] || terminal.background != terminal.defaultBackground {
		t.Fatal("unsupported SGR changed the colours")
	}
	terminal.Write([]byte("\x1b[38;2;1;2;3;41m"))
	if terminal.background != terminal.palette[1] {
		t.Fatal("attribute after a truecolour operand was lost")
	}
}

func TestTerminalColourGuardsPadding(t *testing.T) {
	terminal, pixels := testTerminal(t, 4, 3, 3)
	stride := terminal.buffer.stride
	terminal.Write([]byte("\x1b[46m\x1b[2J\x1b[2;1H\x1b[2K\x1b[J\x1b[1J\x1b[3;4HXYZ\n\n"))
	for row := 0; row < terminal.buffer.height; row++ {
		for column := terminal.buffer.width; column < stride; column++ {
			if pixels[row*stride+column] != 0 {
				t.Fatalf("padding changed at %d,%d", column, row)
			}
		}
	}
}
