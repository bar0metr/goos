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
