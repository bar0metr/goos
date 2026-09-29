package tui

import (
	"goos/userspace/terminal"
)

// Role names a semantic colour pair. Applications select a Theme rather than
// embedding ANSI colour numbers in widgets.
type Role uint8

const (
	RoleGround Role = iota
	RoleText
	RoleBorder
	RoleStatus
	RoleSelection
	RoleDialog
	RoleInput
	RoleCursor
	RoleCount
)

type Style struct {
	Foreground terminal.Color
	Background terminal.Color
}

type Theme [RoleCount]Style

var DefaultTheme = Theme{
	RoleGround:    {Foreground: terminal.White, Background: terminal.Black},
	RoleText:      {Foreground: terminal.White, Background: terminal.Black},
	RoleBorder:    {Foreground: terminal.BrightCyan, Background: terminal.Black},
	RoleStatus:    {Foreground: terminal.BrightWhite, Background: terminal.Cyan},
	RoleSelection: {Foreground: terminal.BrightWhite, Background: terminal.Blue},
	RoleDialog:    {Foreground: terminal.BrightWhite, Background: terminal.BrightBlack},
	RoleInput:     {Foreground: terminal.BrightWhite, Background: terminal.Black},
	RoleCursor:    {Foreground: terminal.Black, Background: terminal.BrightWhite},
}

// Cell is one terminal cell. GoOS renders every supported UTF-8 sequence in
// one cell; invalid bytes remain one replacement cell at the terminal layer.
type Cell struct {
	Bytes [4]byte
	Size  uint8
	Role  Role
}

func blank(role Role) Cell { return Cell{Bytes: [4]byte{' '}, Size: 1, Role: role} }

// Screen stores the desired cells and dirty rows in caller-owned memory.
type Screen struct {
	columns, rows int
	cells         []Cell
	dirty         []bool
	theme         Theme
}

// NewScreen validates dimensions and storage without multiplying past int.
func NewScreen(columns, rows int, cells []Cell, dirty []bool, theme Theme) (Screen, bool) {
	if columns <= 0 || rows <= 0 || columns > maxInt/rows || len(cells) < columns*rows || len(dirty) < rows {
		return Screen{}, false
	}
	screen := Screen{columns: columns, rows: rows, cells: cells[:columns*rows], dirty: dirty[:rows], theme: theme}
	for index := range screen.cells {
		screen.cells[index] = blank(RoleGround)
	}
	screen.Invalidate()
	return screen, true
}

func (screen *Screen) Columns() int { return screen.columns }
func (screen *Screen) Rows() int    { return screen.rows }
func (screen *Screen) Bounds() Rect { return Rect{Width: screen.columns, Height: screen.rows} }

// Invalidate makes the next Flush send every row.
func (screen *Screen) Invalidate() {
	for row := range screen.dirty {
		screen.dirty[row] = true
	}
}

// Clear fills a clipped region with spaces in role.
func (screen *Screen) Clear(rect Rect, role Role) {
	clipped := rect.Intersect(screen.Bounds())
	for row := clipped.Y; row < clipped.Bottom(); row++ {
		for column := clipped.X; column < clipped.Right(); column++ {
			screen.put(column, row, blank(role))
		}
	}
}

// Text draws one line at rect-relative row and column, clipped on every edge.
func (screen *Screen) Text(rect Rect, row, column int, value []byte, role Role) int {
	clipped := rect.Intersect(screen.Bounds())
	y := rect.Y + row
	x := rect.X + column
	if y < clipped.Y || y >= clipped.Bottom() {
		return 0
	}
	written := 0
	for len(value) != 0 {
		character := terminal.Cut(value, 1)
		if len(character) == 0 {
			break
		}
		if x >= clipped.X && x < clipped.Right() {
			screen.putBytes(x, y, character, role)
			written++
		}
		x++
		if x >= clipped.Right() {
			break
		}
		value = value[len(character):]
	}
	return written
}

func (screen *Screen) putBytes(column, row int, value []byte, role Role) {
	cell := Cell{Role: role}
	cell.Size = uint8(copy(cell.Bytes[:], value))
	screen.put(column, row, cell)
}

func (screen *Screen) put(column, row int, cell Cell) {
	if column < 0 || row < 0 || column >= screen.columns || row >= screen.rows || cell.Role >= RoleCount {
		return
	}
	index := row*screen.columns + column
	if screen.cells[index] == cell {
		return
	}
	screen.cells[index] = cell
	screen.dirty[row] = true
}

// Flush sends changed rows with absolute cursor addressing. The bottom-right
// cell is deliberately omitted because writing it may scroll a terminal.
func (screen *Screen) Flush(output *terminal.Output) {
	if output == nil {
		return
	}
	changed := false
	for row := 0; row < screen.rows; row++ {
		if !screen.dirty[row] {
			continue
		}
		screen.dirty[row], changed = false, true
		width := screen.columns
		if row == screen.rows-1 {
			width--
		}
		if width <= 0 {
			continue
		}
		output.MoveTo(row, 0)
		selected := RoleCount
		for column := 0; column < width; column++ {
			cell := screen.cells[row*screen.columns+column]
			if cell.Role >= RoleCount {
				cell = blank(RoleGround)
			}
			if cell.Role != selected {
				style := screen.theme[cell.Role]
				output.SetStyle(style.Foreground, style.Background)
				selected = cell.Role
			}
			output.Text(cell.Bytes[:cell.Size], 1)
		}
	}
	if changed {
		output.ResetStyle()
	}
	output.Flush()
}
