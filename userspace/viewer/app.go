// Package viewer implements the bounded model and terminal-cell rendering of
// the native gview command. It owns no file data: every buffer has a fixed
// maximum and file bytes arrive through Reader.
package viewer

import (
	"goos/kernel/vfs"
	"goos/userspace/terminal"
	"goos/userspace/tui"
)

const (
	DefaultColumns = 80
	DefaultRows    = 24
	MaxColumns     = 256
	MaxRows        = 128
	readWindowSize = 4096
	renderSize     = MaxRows * (MaxColumns*4 + 1)
)

// Reader supplies file bytes without changing the model's memory bound.
type Reader interface {
	ReadAt(offset uint64, target []byte) (int, vfs.Status)
}

// App is the complete viewer state. Its largest fields are fixed screen,
// render and read-window buffers, independent of the file size.
type App struct {
	reader Reader
	size   uint64
	path   [vfs.MaxPath]byte
	pathN  int

	window      [readWindowSize]byte
	windowStart uint64
	windowN     int
	windowValid bool

	cells  [MaxColumns * MaxRows]tui.Cell
	dirty  [MaxRows]bool
	screen tui.Screen
	render [renderSize]byte
	status [MaxColumns * 4]byte

	columns, rows int
	display       bool
	displayCols   int
	displayRows   int
	hostCols      int
	hostRows      int

	topOffset  uint64
	leftColumn int
	binary     bool
	quit       bool
	sizeWanted bool
	clear      bool
	failure    vfs.Status
}

// Init validates the bounded state, probes the beginning for clearly binary
// data and lays out the first frame.
func (app *App) Init(reader Reader, path []byte, size uint64, columns, rows int, display bool) vfs.Status {
	if reader == nil || len(path) == 0 || len(path) > len(app.path) {
		return vfs.StatusInvalidArgument
	}
	*app = App{reader: reader, size: size, display: display, displayCols: columns, displayRows: rows}
	app.pathN = copy(app.path[:], path)
	app.resize()
	app.sizeWanted = !display
	binary, status := app.detectBinary()
	if status != vfs.StatusOK {
		return status
	}
	app.binary = binary
	return vfs.StatusOK
}

func (app *App) Quit() bool            { return app.quit }
func (app *App) Binary() bool          { return app.binary }
func (app *App) TopOffset() uint64     { return app.topOffset }
func (app *App) LeftColumn() int       { return app.leftColumn }
func (app *App) Failure() vfs.Status   { return app.failure }
func (app *App) FollowsTerminal() bool { return !app.display }
func (app *App) SizeWanted() bool {
	wanted := app.sizeWanted
	app.sizeWanted = false
	return wanted
}

func (app *App) geometry() (int, int) {
	if app.display {
		return app.displayCols, app.displayRows
	}
	if app.hostCols != 0 && app.hostRows != 0 {
		return app.hostCols, app.hostRows
	}
	return DefaultColumns, DefaultRows
}

func (app *App) resize() {
	columns, rows := app.geometry()
	columns, rows = min(max(columns, 1), MaxColumns), min(max(rows, 1), MaxRows)
	if columns == app.columns && rows == app.rows {
		return
	}
	app.columns, app.rows = columns, rows
	app.screen, _ = tui.NewScreen(columns, rows, app.cells[:], app.dirty[:], tui.DefaultTheme)
	app.clear = true
}

// Handle applies navigation and resize events. Home and End address the file;
// Left and Right address horizontal columns.
func (app *App) Handle(event terminal.Event) {
	switch event.Key {
	case terminal.KeyEscape, terminal.KeyF10:
		app.quit = true
	case terminal.KeyF2:
		app.screen.Invalidate()
		app.clear = true
		app.sizeWanted = !app.display
	case terminal.KeyUp:
		app.moveUp(1)
	case terminal.KeyDown:
		app.moveDown(1)
	case terminal.KeyPageUp:
		app.moveUp(max(app.bodyRows(), 1))
	case terminal.KeyPageDown:
		app.moveDown(max(app.bodyRows(), 1))
	case terminal.KeyHome:
		app.topOffset, app.leftColumn = 0, 0
	case terminal.KeyEnd:
		app.moveEnd()
	case terminal.KeyLeft:
		app.leftColumn = max(app.leftColumn-8, 0)
	case terminal.KeyRight:
		if app.leftColumn <= int(^uint(0)>>1)-8 {
			app.leftColumn += 8
		}
	case terminal.KeyPosition:
		if !app.display && event.Column > 0 && event.Row > 0 {
			app.hostCols, app.hostRows = event.Column, event.Row
			app.resize()
		}
	}
}

func (app *App) bodyRows() int { return max(app.rows-1, 0) }

func (app *App) moveDown(lines int) {
	for ; lines > 0; lines-- {
		next, found, status := app.nextLine(app.topOffset)
		if status != vfs.StatusOK {
			app.failure = status
			return
		}
		if !found {
			return
		}
		app.topOffset = next
	}
}

func (app *App) moveUp(lines int) {
	for ; lines > 0; lines-- {
		previous, found, status := app.previousLine(app.topOffset)
		if status != vfs.StatusOK {
			app.failure = status
			return
		}
		if !found {
			return
		}
		app.topOffset = previous
	}
}

func (app *App) moveEnd() {
	app.topOffset = app.lastLineStart()
	app.moveUp(max(app.bodyRows()-1, 0))
}

func (app *App) lastLineStart() uint64 {
	if app.size == 0 {
		return 0
	}
	last, ok, status := app.byteAt(app.size - 1)
	if status != vfs.StatusOK || !ok {
		app.failure = status
		return app.topOffset
	}
	if last == '\n' {
		return app.size
	}
	start, _, status := app.previousLine(app.size)
	if status != vfs.StatusOK {
		app.failure = status
		return app.topOffset
	}
	return start
}

func (app *App) nextLine(start uint64) (uint64, bool, vfs.Status) {
	for offset := start; offset < app.size; offset++ {
		value, ok, status := app.byteAt(offset)
		if status != vfs.StatusOK || !ok {
			return start, false, status
		}
		if value == '\n' {
			return offset + 1, true, vfs.StatusOK
		}
	}
	return start, false, vfs.StatusOK
}

func (app *App) previousLine(start uint64) (uint64, bool, vfs.Status) {
	if start == 0 {
		return 0, false, vfs.StatusOK
	}
	offset := start - 1
	if value, ok, status := app.byteAt(offset); status != vfs.StatusOK {
		return start, false, status
	} else if !ok {
		return start, false, vfs.StatusBackendError
	} else if value == '\n' && offset != 0 {
		offset--
	}
	for {
		value, ok, status := app.byteAt(offset)
		if status != vfs.StatusOK || !ok {
			return start, false, status
		}
		if value == '\n' {
			return offset + 1, true, vfs.StatusOK
		}
		if offset == 0 {
			return 0, true, vfs.StatusOK
		}
		offset--
	}
}

func (app *App) detectBinary() (bool, vfs.Status) {
	limit := min(app.size, uint64(readWindowSize))
	controls := 0
	for offset := uint64(0); offset < limit; offset++ {
		value, ok, status := app.byteAt(offset)
		if status != vfs.StatusOK || !ok {
			return false, status
		}
		if value == 0 {
			return true, vfs.StatusOK
		}
		if value < 0x20 && value != '\n' && value != '\r' && value != '\t' || value == 0x7f {
			controls++
		}
	}
	return limit != 0 && uint64(controls)*8 > limit, vfs.StatusOK
}

func (app *App) byteAt(offset uint64) (byte, bool, vfs.Status) {
	if offset >= app.size {
		return 0, false, vfs.StatusOK
	}
	if !app.windowValid || offset < app.windowStart || offset-app.windowStart >= uint64(app.windowN) {
		remaining := app.size - offset
		wanted := len(app.window)
		if remaining < uint64(wanted) {
			wanted = int(remaining)
		}
		count, status := app.reader.ReadAt(offset, app.window[:wanted])
		if status != vfs.StatusOK {
			app.windowValid = false
			return 0, false, status
		}
		if count <= 0 || count > wanted {
			app.windowValid = false
			return 0, false, vfs.StatusBackendError
		}
		app.windowStart, app.windowN, app.windowValid = offset, count, true
	}
	return app.window[offset-app.windowStart], true, vfs.StatusOK
}
