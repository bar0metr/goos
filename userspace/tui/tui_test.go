package tui

import (
	"goos/userspace/terminal"
	"testing"
)

func TestRectChecksAndClipsGeometry(t *testing.T) {
	rect, ok := NewRect(10, 5, 20, 8)
	if !ok || rect.Intersect(Rect{X: 25, Y: 0, Width: 20, Height: 20}) != (Rect{X: 25, Y: 5, Width: 5, Height: 8}) {
		t.Fatal("intersection")
	}
	for _, value := range []Rect{{X: -1}, {Y: -1}, {Width: -1}, {Height: -1}, {X: maxInt, Width: 1}} {
		if _, ok := NewRect(value.X, value.Y, value.Width, value.Height); ok {
			t.Fatalf("accepted %+v", value)
		}
	}
	if _, ok := NewScreen(maxInt, 2, nil, nil, DefaultTheme); ok {
		t.Fatal("overflowing screen accepted")
	}
}

func TestScreenFlushesChangedRowsAndProtectsBottomRight(t *testing.T) {
	const columns, rows = 8, 3
	var cells [columns * rows]Cell
	var dirty [rows]bool
	screen, ok := NewScreen(columns, rows, cells[:], dirty[:], DefaultTheme)
	if !ok {
		t.Fatal("screen")
	}
	model := newModel(columns, rows)
	model.cells[len(model.cells)-1] = cellOf('#')
	var outputStorage [1024]byte
	output := terminal.NewOutput(outputStorage[:], model.feed)
	screen.Clear(screen.Bounds(), RoleGround)
	screen.Text(Rect{X: 1, Y: 1, Width: 4, Height: 1}, 0, -1, []byte("αbeta"), RoleText)
	screen.Text(Rect{X: 7, Y: 2, Width: 1, Height: 1}, 0, 0, []byte("X"), RoleText)
	screen.Flush(&output)
	if got := model.row(1); got != " beta   " {
		t.Fatalf("row=%q", got)
	}
	if model.cells[len(model.cells)-1] != cellOf('#') {
		t.Fatal("bottom-right cell was written")
	}
	writes := model.writes
	screen.Flush(&output)
	if model.writes != writes {
		t.Fatal("unchanged frame was written")
	}
}

func TestTextViewportScrollSelectionAndCursor(t *testing.T) {
	var cells [24]Cell
	var dirty [4]bool
	screen, _ := NewScreen(6, 4, cells[:], dirty[:], DefaultTheme)
	viewport := TextViewport{TopLine: 1, LeftColumn: 1, Cursor: Position{Line: 1, Column: 3}, CursorVisible: true,
		SelectionStart: Position{Line: 1, Column: 1}, SelectionEnd: Position{Line: 1, Column: 4}, SelectionVisible: true}
	viewport.Draw(&screen, Rect{Width: 4, Height: 2}, []byte("zero\nαbeta\nthird"), RoleText, RoleSelection, RoleCursor)
	if got := screenRow(&screen, 0); got[:4] != "beta" {
		t.Fatalf("row=%q", got)
	}
	if screen.cells[0].Role != RoleSelection || screen.cells[2].Role != RoleCursor || screen.cells[3].Role != RoleText {
		t.Fatalf("roles=%d,%d,%d", screen.cells[0].Role, screen.cells[2].Role, screen.cells[3].Role)
	}
	if got := screenRow(&screen, 1); got[:4] != "hird" {
		t.Fatalf("second=%q", got)
	}
	viewport.ScrollLines(-100)
	viewport.ScrollColumns(-100)
	if viewport.TopLine != 0 || viewport.LeftColumn != 0 {
		t.Fatal("scroll underflow")
	}
}

func TestWidgetsFitMinimumScreen(t *testing.T) {
	const columns, rows = 80, 24
	var cells [columns * rows]Cell
	var dirty [rows]bool
	screen, _ := NewScreen(columns, rows, cells[:], dirty[:], DefaultTheme)
	inner := Border(&screen, screen.Bounds(), RoleBorder, true)
	if inner != (Rect{X: 1, Y: 1, Width: 78, Height: 22}) {
		t.Fatalf("inner=%+v", inner)
	}
	items := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	list := ListViewport{Selected: 2}
	list.Draw(&screen, Rect{X: 2, Y: 2, Width: 20, Height: 2}, items, RoleText, RoleSelection)
	if list.Top != 1 {
		t.Fatalf("top=%d", list.Top)
	}
	dialog := Dialog{Title: []byte("Title"), Message: []byte("Message"), Input: []byte("value"), ConfirmText: []byte("Enter = OK"), Width: 40, Height: 7, BoxDrawing: true}
	if content := dialog.Draw(&screen, screen.Bounds()); content.Width != 38 || content.Height != 5 {
		t.Fatalf("dialog=%+v", content)
	}
}

func TestSteadyFrameDoesNotAllocate(t *testing.T) {
	const columns, rows = 80, 24
	var cells [columns * rows]Cell
	var dirty [rows]bool
	screen, _ := NewScreen(columns, rows, cells[:], dirty[:], DefaultTheme)
	var outputStorage [8192]byte
	written := 0
	output := terminal.NewOutput(outputStorage[:], func(value []byte) { written += len(value) })
	viewport := TextViewport{Cursor: Position{Line: 0, Column: 2}, CursorVisible: true}
	text := []byte("steady frame\nsecond line")
	viewport.Draw(&screen, screen.Bounds(), text, RoleText, RoleSelection, RoleCursor)
	screen.Flush(&output)
	toggle := false
	allocations := testing.AllocsPerRun(100, func() {
		toggle = !toggle
		if toggle {
			viewport.Cursor.Column = 3
		} else {
			viewport.Cursor.Column = 2
		}
		viewport.Draw(&screen, screen.Bounds(), text, RoleText, RoleSelection, RoleCursor)
		screen.Flush(&output)
	})
	if allocations != 0 || written == 0 {
		t.Fatalf("allocations=%v written=%d", allocations, written)
	}
}

func screenRow(screen *Screen, row int) string {
	var value [256]byte
	size := 0
	for column := 0; column < screen.columns; column++ {
		cell := screen.cells[row*screen.columns+column]
		size += copy(value[size:], cell.Bytes[:cell.Size])
	}
	return string(value[:size])
}

type modelTerminal struct {
	columns, rows     int
	cursorRow, column int
	cells             []Cell
	writes            int
}

func newModel(columns, rows int) *modelTerminal {
	return &modelTerminal{columns: columns, rows: rows, cells: make([]Cell, columns*rows)}
}

func (model *modelTerminal) feed(value []byte) {
	model.writes++
	for index := 0; index < len(value); {
		if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '[' {
			index += 2
			first, second, haveSecond := 0, 0, false
			for index < len(value) {
				character := value[index]
				index++
				if character >= '0' && character <= '9' {
					if haveSecond {
						second = second*10 + int(character-'0')
					} else {
						first = first*10 + int(character-'0')
					}
					continue
				}
				if character == ';' {
					haveSecond = true
					continue
				}
				if character == 'H' {
					model.cursorRow, model.column = max(first-1, 0), max(second-1, 0)
				}
				break
			}
			continue
		}
		character := terminal.Cut(value[index:], 1)
		if len(character) == 0 {
			break
		}
		if model.cursorRow >= 0 && model.cursorRow < model.rows && model.column >= 0 && model.column < model.columns {
			cell := Cell{}
			cell.Size = uint8(copy(cell.Bytes[:], character))
			model.cells[model.cursorRow*model.columns+model.column] = cell
		}
		model.column++
		index += len(character)
	}
}

func (model *modelTerminal) row(index int) string {
	var value [256]byte
	size := 0
	for column := 0; column < model.columns; column++ {
		cell := model.cells[index*model.columns+column]
		if cell.Size == 0 {
			cell = cellOf(' ')
		}
		size += copy(value[size:], cell.Bytes[:cell.Size])
	}
	return string(value[:size])
}

func cellOf(value byte) Cell { return Cell{Bytes: [4]byte{value}, Size: 1} }
