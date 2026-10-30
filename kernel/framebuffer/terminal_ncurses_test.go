package framebuffer

import (
	"bytes"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// testdata/ncdu-initial.bin is what static ncdu 1.22 wrote with TERM=goos on a
// 120x40 serial terminal from its first byte to the end of its first redraw
// after the scan, with the service's size questions in place.
var ncduScreen = []string{
	"ncdu 1.22 ~ Use the arrow keys to navigate, press ? for help",
	"--- /system/linux " + strings.Repeat("-", 102),
	"  363.5 KiB [#################]  ncdu",
	"  134.0 KiB [######           ]  abi-probe",
	"   34.0 KiB [#                ]  tty-probe",
	"   29.5 KiB [#                ]  musl-hello",
	"   23.5 KiB [#                ]  fsprobe",
	"   23.5 KiB [#                ]  dirprobe",
	"    8.0 KiB [                 ] /terminfo",
	"    2.0 KiB [                 ]  hello-pie",
	"    1.0 KiB [                 ]  hello",
}

const ncduStatus = "*Total disk usage: 619.0 KiB   Apparent size: 616.3 KiB   Items: 16"

func ncduWant(rows int) []string {
	want := make([]string, rows)
	copy(want, ncduScreen)
	want[rows-1] = ncduStatus
	return want
}

// deferredTerminal is the part of a VT100-class terminal that differs from
// Terminal: the cursor stays on the last column until the next character.
type deferredTerminal struct {
	cells           [][]rune
	row, column     int
	pending, noWrap bool
	savedRow        int
	savedColumn     int
	savedPending    bool
}

func newDeferredTerminal(columns, rows int) *deferredTerminal {
	terminal := &deferredTerminal{cells: make([][]rune, rows)}
	for row := range terminal.cells {
		terminal.cells[row] = []rune(strings.Repeat(" ", columns))
	}
	return terminal
}

var deferredToken = regexp.MustCompile(`\x1b\[[?0-9;]*[@-~]|\x1b[78]|(?s:.)`)

func (terminal *deferredTerminal) write(data []byte) {
	for _, token := range deferredToken.FindAll(data, -1) {
		switch text := string(token); {
		case strings.HasPrefix(text, "\x1b["):
			terminal.control(text[2:])
		case text == "\x1b7":
			terminal.savedRow, terminal.savedColumn, terminal.savedPending = terminal.row, terminal.column, terminal.pending
		case text == "\x1b8":
			terminal.row, terminal.column, terminal.pending = terminal.savedRow, terminal.savedColumn, terminal.savedPending
		case text == "\r":
			terminal.column, terminal.pending = 0, false
		case text == "\n":
			terminal.row, terminal.pending = min(terminal.row+1, len(terminal.cells)-1), false
		default:
			terminal.put([]rune(text)[0])
		}
	}
}

func (terminal *deferredTerminal) put(value rune) {
	if terminal.pending {
		terminal.column, terminal.pending = 0, false
		terminal.row = min(terminal.row+1, len(terminal.cells)-1)
	}
	line := terminal.cells[terminal.row]
	line[terminal.column] = value
	if terminal.column < len(line)-1 {
		terminal.column++
	} else {
		terminal.pending = !terminal.noWrap
	}
}

func (terminal *deferredTerminal) control(sequence string) {
	final := sequence[len(sequence)-1]
	if sequence[0] == '?' {
		if sequence[:len(sequence)-1] == "?7" {
			terminal.noWrap = final == 'l'
		}
		return
	}
	arguments := strings.Split(sequence[:len(sequence)-1], ";")
	argument := func(index, fallback int) int {
		if index < len(arguments) {
			if value, err := strconv.Atoi(arguments[index]); err == nil && value != 0 {
				return value
			}
		}
		return fallback
	}
	rows, columns := len(terminal.cells), len(terminal.cells[0])
	switch final {
	case 'A', 'B', 'C', 'D', 'G', 'H', 'f', 'J', 'K':
		terminal.pending = false
	}
	switch final {
	case 'A':
		terminal.row = clampCell(terminal.row-argument(0, 1), rows)
	case 'B':
		terminal.row = clampCell(terminal.row+argument(0, 1), rows)
	case 'C':
		terminal.column = clampCell(terminal.column+argument(0, 1), columns)
	case 'D':
		terminal.column = clampCell(terminal.column-argument(0, 1), columns)
	case 'G':
		terminal.column = clampCell(argument(0, 1)-1, columns)
	case 'H', 'f':
		terminal.row, terminal.column = clampCell(argument(0, 1)-1, rows), clampCell(argument(1, 1)-1, columns)
	case 'K':
		for column := terminal.column; column < columns; column++ {
			terminal.cells[terminal.row][column] = ' '
		}
	case 'J':
		from := terminal.row + 1
		if argument(0, 0) == 2 {
			from = 0
		} else {
			for column := terminal.column; column < columns; column++ {
				terminal.cells[terminal.row][column] = ' '
			}
		}
		for row := from; row < rows; row++ {
			for column := range terminal.cells[row] {
				terminal.cells[row][column] = ' '
			}
		}
	}
}

func compareScreen(t *testing.T, got, want []string) {
	t.Helper()
	for row := range want {
		if got[row] != want[row] {
			t.Errorf("row %d = %q, want %q", row, got[row], want[row])
		}
	}
}

func readNCDU(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/ncdu-initial.bin")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNCDURedrawOnADeferredWrapTerminal(t *testing.T) {
	terminal := newDeferredTerminal(120, 40)
	terminal.write(readNCDU(t))
	got := make([]string, 40)
	for row, line := range terminal.cells {
		got[row] = strings.TrimRight(string(line), " ")
	}
	compareScreen(t, got, ncduWant(40))
}

func TestDeferredModelKeepsARelativeMoveOnTheFullRow(t *testing.T) {
	// The defect in one line: ncurses told am without xenl sends this for the
	// row below a full one, and it lands on the full row.
	terminal := newDeferredTerminal(4, 2)
	terminal.write([]byte("abcd\x1b[1Gx"))
	if got := string(terminal.cells[0]); got != "xbcd" {
		t.Fatalf("row 0 = %q", got)
	}
}

// cellMask reports which pixels of a cell differ from its top-left corner's
// colour class, so that colours and cursor inversion do not matter.
func cellMask(terminal *Terminal, column, row int) [GlyphWidth * GlyphHeight]bool {
	var mask [GlyphWidth * GlyphHeight]bool
	stride := terminal.buffer.stride
	base := row*GlyphHeight*stride + column*GlyphWidth
	first := terminal.buffer.pixels[base]
	for y := 0; y < GlyphHeight; y++ {
		for x := 0; x < GlyphWidth; x++ {
			mask[y*GlyphWidth+x] = terminal.buffer.pixels[base+y*stride+x] != first
		}
	}
	return mask
}

func TestNCDURedrawOnTheFramebuffer(t *testing.T) {
	terminal, _ := testTerminal(t, 120, 40, 0)
	reference, _ := testTerminal(t, 1, 1, 0)
	// The service does not ask a display for its size.
	terminal.Write(bytes.ReplaceAll(readNCDU(t), []byte("\x1b7\x1b[999;999H\x1b[6n\x1b8"), nil))
	want := ncduWant(40)
	for row := 0; row < 40; row++ {
		line := want[row] + strings.Repeat(" ", 120-utf8.RuneCountInString(want[row]))
		for column, value := range []rune(line) {
			reference.Write([]byte("\x1b[?7l\x1b[H" + string(value)))
			if cellMask(&terminal, column, row) != cellMask(&reference, 0, 0) {
				t.Errorf("cell %d,%d is not %q", column, row, value)
			}
		}
	}
	if cellPixel(&terminal, 119, 39) == terminal.defaultBackground {
		t.Fatal("the status bar stops short of the bottom-right cell")
	}
}

func TestTerminalIgnoresANewlineBehindAWrap(t *testing.T) {
	terminal, _ := testTerminal(t, 4, 3, 0)
	terminal.Write([]byte("abcd\n"))
	if terminal.column != 0 || terminal.row != 1 {
		t.Fatalf("cursor = %d,%d", terminal.column, terminal.row)
	}
	terminal.Write([]byte("\n"))
	if terminal.row != 2 {
		t.Fatalf("a second newline stayed on row %d", terminal.row)
	}
}

// After a full row, LF, CR, and what follows them must leave the framebuffer
// where a terminal that defers the wrap leaves it. Every LF here follows a CR:
// a lone LF keeps the column on a terminal and returns to 0 on the framebuffer.
func TestTerminalMatchesDeferredWrapAfterAFullRow(t *testing.T) {
	for _, sequence := range []string{
		"abcd\r",
		"abcd\r\n",
		"abcd\rX",
		"abcd\r\nX",
		"abcd\r\r\n",
		"abcd\r\n\r\n",
		"abcdX",
		"abcdXY\r\n",
		"abcd\r\nefgh\r\n",
		"abcdefgh\r\n",
	} {
		framebuffer, _ := testTerminal(t, 4, 4, 0)
		model := newDeferredTerminal(4, 4)
		framebuffer.Write([]byte(sequence))
		model.write([]byte(sequence))
		if framebuffer.row != model.row || framebuffer.column != model.column {
			t.Errorf("%q: cursor = %d,%d, deferred terminal %d,%d", sequence,
				framebuffer.column, framebuffer.row, model.column, model.row)
		}
		reference, _ := testTerminal(t, 1, 1, 0)
		for row, line := range model.cells {
			for column, value := range line {
				reference.Write([]byte("\x1b[?7l\x1b[H" + string(value)))
				if cellMask(&framebuffer, column, row) != cellMask(&reference, 0, 0) {
					t.Errorf("%q: cell %d,%d is not %q", sequence, column, row, value)
				}
			}
		}
	}
}

// On the bottom row the wrap has already scrolled, which a deferred terminal
// has not, but the cursor still returns to the row that holds the text.
func TestTerminalCarriageReturnAfterABottomRowWrap(t *testing.T) {
	terminal, _ := testTerminal(t, 4, 2, 0)
	reference, _ := testTerminal(t, 1, 1, 0)
	terminal.Write([]byte("\x1b[2;1Habcd\rX"))
	reference.Write([]byte("\x1b[?7l\x1b[HX"))
	if terminal.row != 0 || terminal.column != 1 || cellMask(&terminal, 0, 0) != cellMask(&reference, 0, 0) {
		t.Fatalf("cursor = %d,%d", terminal.column, terminal.row)
	}
}

func TestTerminalAutowrapOffKeepsTheLastColumn(t *testing.T) {
	terminal, _ := testTerminal(t, 4, 2, 0)
	terminal.Write([]byte("\x1b[?7l\x1b[2;1Habcde"))
	if terminal.column != 3 || terminal.row != 1 {
		t.Fatalf("cursor with autowrap off = %d,%d", terminal.column, terminal.row)
	}
	terminal.Write([]byte("\x1b[?7h\x1b[1;1Habcd"))
	if terminal.column != 0 || terminal.row != 1 {
		t.Fatalf("cursor with autowrap on = %d,%d", terminal.column, terminal.row)
	}
}

// The compiled entry must keep the capabilities the fixtures rely on; bw is
// boolean 0, am 1, and xenl 4.
func TestGoosTerminfoDescribesBothOutputs(t *testing.T) {
	entry, err := os.ReadFile("../../userspace/linux/terminfo/db/g/goos")
	if err != nil {
		t.Fatal(err)
	}
	names := int(entry[2]) | int(entry[3])<<8
	booleans := entry[12+names:]
	if booleans[0] != 0 || booleans[1] != 1 || booleans[4] != 1 {
		t.Fatalf("bw %d am %d xenl %d", booleans[0], booleans[1], booleans[4])
	}
	for _, capability := range []string{"\x1b[?7l", "\x1b[?7h"} {
		if !bytes.Contains(entry, []byte(capability)) {
			t.Errorf("entry lacks %q", capability)
		}
	}
}
