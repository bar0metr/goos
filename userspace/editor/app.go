// Package editor implements the bounded document model, key handling and
// terminal-cell rendering of the native edit command. The whole document
// lives in one fixed gap buffer of MaxSize bytes; nothing is allocated from
// the file's size, and saving goes through userspace/safesave (ADR 0129).
package editor

import (
	"unicode/utf8"

	"goos/internal/limits"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/safesave"
	"goos/userspace/shell"
	"goos/userspace/terminal"
	"goos/userspace/tui"
)

const (
	// MaxSize is the largest document the editor holds: the text and the gap
	// share this storage, so a file of MaxSize bytes opens with no room to
	// insert (ADR 0131).
	MaxSize = 1 << 20
	// LoadChunk bounds one read while a file loads; it matches the 64 KiB
	// bulk transfer buffer.
	LoadChunk = 64 << 10

	DefaultColumns = 80
	DefaultRows    = 24
	MaxColumns     = 256
	MaxRows        = 128

	renderSize  = MaxRows * (MaxColumns*4 + 1)
	messageSize = 512
)

// Reader supplies a file's bytes in order. A read that returns zero bytes
// ends the file.
type Reader interface {
	Read(target []byte) (int, vfs.Status)
}

// Files is the part of the filesystem the editor uses after the file is
// loaded. Save replaces path with what source produces, as utility.SaveFile
// does.
type Files interface {
	Stat(path []byte) (ramfs.Kind, uint64, vfs.Status)
	Save(path []byte, source safesave.Source) (vfs.Status, safesave.Stage)
}

type dialogKind uint8

const (
	dialogNone dialogKind = iota
	dialogQuit
	dialogSaveAs
	dialogOverwrite
)

// App is the complete editor state. Its largest fields are the document
// storage and the fixed screen and render buffers. A package-level App lives
// in demand-zero memory, so the part of the storage a small file never
// reaches costs no pages.
type App struct {
	files  Files
	text   [MaxSize]byte
	buffer Buffer
	saving source

	path    [limits.PathMax]byte
	pathN   int
	cwd     [limits.PathMax]byte
	cwdN    int
	target  [limits.PathMax]byte
	targetN int

	// cursor is a byte offset into the text, always on a character
	// boundary. line and column locate it from 0; goal is the column Up and
	// Down aim for, so the cursor returns to it past shorter lines.
	cursor, line, column, goal int
	// topOffset is where the first visible line starts and topLine its
	// number; leftColumn is the first visible column.
	topOffset, topLine, leftColumn int

	modified bool
	quit     bool
	refused  bool
	dialog   dialogKind
	input    [limits.PathMax]byte
	inputN   int

	message      [messageSize]byte
	messageN     int
	messageError bool

	// Saves counts successful saves, for the validation build's marker.
	Saves int

	cells    [MaxColumns * MaxRows]tui.Cell
	dirty    [MaxRows]bool
	screen   tui.Screen
	render   [renderSize]byte
	status   [MaxColumns * 4]byte
	scratch  [MaxColumns * 4]byte
	position [48]byte

	columns, rows int
	display       bool
	displayCols   int
	displayRows   int
	hostCols      int
	hostRows      int
	sizeWanted    bool
	clear         bool
}

// Init prepares an empty document for path, which must be absolute; cwd
// resolves relative Save As names. display is true when columns and rows come
// from the framebuffer console; otherwise the editor asks the terminal.
func (app *App) Init(files Files, path, cwd []byte, columns, rows int, display bool) bool {
	if files == nil || len(path) == 0 || path[0] != '/' || len(path) > len(app.path) ||
		len(cwd) == 0 || cwd[0] != '/' || len(cwd) > len(app.cwd) {
		return false
	}
	// Fields are reset one by one: a composite literal of the whole App
	// would build a temporary as large as the document storage.
	app.files = files
	app.buffer.Reset(app.text[:], 0)
	app.pathN = copy(app.path[:], path)
	app.cwdN = copy(app.cwd[:], cwd)
	app.targetN, app.inputN = 0, 0
	app.cursor, app.line, app.column, app.goal = 0, 0, 0, 0
	app.topOffset, app.topLine, app.leftColumn = 0, 0, 0
	app.modified, app.quit, app.refused = false, false, false
	app.dialog = dialogNone
	app.messageN, app.messageError = 0, false
	app.Saves = 0
	app.display, app.displayCols, app.displayRows = display, columns, rows
	app.hostCols, app.hostRows = 0, 0
	app.columns, app.rows = 0, 0
	app.resize()
	app.sizeWanted = !display
	return true
}

// NewFile marks the document as a file that does not exist yet; the first
// save creates it.
func (app *App) NewFile() { app.say(false, "New file", nil, "") }

// Load reads the file through reader into the document. A file larger than
// MaxSize, or one containing a NUL byte, is refused rather than loaded; so is
// a failed read. It returns false when it refused.
func (app *App) Load(reader Reader, size uint64) bool {
	if size > MaxSize {
		app.Refuse("file too large to edit (the limit is 1 MiB); view it with gview")
		return false
	}
	used := 0
	for {
		if used == len(app.text) {
			// The file may have grown since its size was read.
			var probe [1]byte
			count, status := reader.Read(probe[:])
			if status == vfs.StatusOK && count == 0 {
				break
			}
			if status != vfs.StatusOK {
				app.refuseStatus(status)
				return false
			}
			app.Refuse("file too large to edit (the limit is 1 MiB); view it with gview")
			return false
		}
		wanted := min(LoadChunk, len(app.text)-used)
		count, status := reader.Read(app.text[used : used+wanted])
		if status != vfs.StatusOK {
			app.refuseStatus(status)
			return false
		}
		if count < 0 || count > wanted {
			app.refuseStatus(vfs.StatusBackendError)
			return false
		}
		if count == 0 {
			break
		}
		for _, value := range app.text[used : used+count] {
			if value == 0 {
				app.Refuse("binary file (it contains NUL bytes); view it with gview")
				return false
			}
		}
		used += count
	}
	app.buffer.Reset(app.text[:], used)
	app.cursor, app.line, app.column, app.goal = 0, 0, 0, 0
	app.topOffset, app.topLine, app.leftColumn = 0, 0, 0
	app.modified = false
	return true
}

// Refuse replaces the editor with a message that the file cannot be edited
// and why. Any of Enter, Escape and F10 then ends the program.
func (app *App) Refuse(reason string) {
	app.refused = true
	app.say(true, "", nil, reason)
	app.buffer.Reset(app.text[:], 0)
}

func (app *App) refuseStatus(status vfs.Status) { app.Refuse(shell.VFSReason(status, false)) }

func (app *App) Quit() bool            { return app.quit }
func (app *App) Refused() bool         { return app.refused }
func (app *App) Modified() bool        { return app.modified }
func (app *App) FollowsTerminal() bool { return !app.display }
func (app *App) Path() []byte          { return app.path[:app.pathN] }
func (app *App) Len() int              { return app.buffer.Len() }

// Cursor returns the cursor's line and column, from 0.
func (app *App) Cursor() (int, int) { return app.line, app.column }

// Message returns the last error or information message and whether it is
// an error.
func (app *App) Message() ([]byte, bool) { return app.message[:app.messageN], app.messageError }

// Text copies the document into target and returns how much it copied.
func (app *App) Text(target []byte) int { return app.buffer.Copy(target, 0) }

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
	app.scroll()
}

// bodyRows is the height of the text area: the last two rows hold the status
// line and the message line.
func (app *App) bodyRows() int { return max(app.rows-2, 0) }

// Handle applies one key or terminal report.
func (app *App) Handle(event terminal.Event) {
	if event.Key == terminal.KeyPosition {
		if !app.display && event.Column > 0 && event.Row > 0 {
			app.hostCols, app.hostRows = event.Column, event.Row
			app.resize()
		}
		return
	}
	if app.refused {
		if event.Key == terminal.KeyEnter || event.Key == terminal.KeyEscape || event.Key == terminal.KeyF10 {
			app.quit = true
		}
		return
	}
	if app.dialog != dialogNone {
		app.handleDialog(event)
		return
	}
	switch event.Key {
	case terminal.KeyF2:
		app.save(app.path[:app.pathN])
	case terminal.KeyF3:
		app.dialog = dialogSaveAs
		app.inputN = copy(app.input[:], app.path[:app.pathN])
	case terminal.KeyF10:
		if app.modified {
			app.dialog = dialogQuit
		} else {
			app.quit = true
		}
	case terminal.KeyLeft:
		app.moveLeft()
	case terminal.KeyRight:
		app.moveRight()
	case terminal.KeyUp:
		app.moveUp(1)
	case terminal.KeyDown:
		app.moveDown(1)
	case terminal.KeyPageUp:
		app.page(-1)
	case terminal.KeyPageDown:
		app.page(1)
	case terminal.KeyHome:
		app.cursor = app.lineStart(app.cursor)
		app.settle(true)
	case terminal.KeyEnd:
		app.cursor = app.lineEnd(app.cursor)
		app.settle(true)
	case terminal.KeyEnter:
		app.insert([]byte{'\n'})
	case terminal.KeyTab:
		app.insert([]byte{'\t'})
	case terminal.KeyBackspace:
		app.backspace()
	case terminal.KeyDelete:
		app.deleteForward()
	case terminal.KeyRune:
		if event.Ctrl || event.Alt || event.Rune < 0x20 || event.Rune == 0x7f || !utf8.ValidRune(event.Rune) {
			return
		}
		var encoded [utf8.UTFMax]byte
		size := utf8.EncodeRune(encoded[:], event.Rune)
		app.insert(encoded[:size])
	}
}

// Movement. Every offset the editor keeps is a character boundary: a
// well-formed UTF-8 sequence is one character and any other byte is one on
// its own, as the terminal counts them.

func (app *App) moveLeft() {
	if app.cursor == 0 {
		return
	}
	if app.buffer.At(app.cursor-1) == '\n' {
		app.cursor--
		app.line--
	} else {
		app.cursor = app.previousCharacter(app.cursor)
	}
	app.settle(true)
}

func (app *App) moveRight() {
	if app.cursor == app.buffer.Len() {
		return
	}
	if app.buffer.At(app.cursor) == '\n' {
		app.cursor++
		app.line++
	} else {
		app.cursor += app.characterSize(app.cursor)
	}
	app.settle(true)
}

// moveUp and moveDown move up to lines lines, keep the goal column and
// return how many lines they moved.
func (app *App) moveUp(lines int) int {
	start := app.lineStart(app.cursor)
	moved := 0
	for ; moved < lines && start > 0; moved++ {
		start = app.lineStart(start - 1)
	}
	app.line -= moved
	app.cursor = app.offsetAtColumn(start, app.goal)
	app.settle(false)
	return moved
}

func (app *App) moveDown(lines int) int {
	start := app.lineStart(app.cursor)
	moved := 0
	for ; moved < lines; moved++ {
		end := app.lineEnd(start)
		if end == app.buffer.Len() {
			break
		}
		start = end + 1
	}
	app.line += moved
	app.cursor = app.offsetAtColumn(start, app.goal)
	app.settle(false)
	return moved
}

// page moves the cursor one screen of lines and scrolls the view with it:
// the cursor keeps its screen row unless the start of the file is closer.
func (app *App) page(direction int) {
	row := app.line - app.topLine
	if direction < 0 {
		app.moveUp(max(app.bodyRows(), 1))
	} else {
		app.moveDown(max(app.bodyRows(), 1))
	}
	start := app.lineStart(app.cursor)
	above := 0
	for ; above < row && start > 0; above++ {
		start = app.lineStart(start - 1)
	}
	app.topOffset, app.topLine = start, app.line-above
	app.scroll()
}

// Editing.

func (app *App) insert(value []byte) {
	if !app.buffer.Insert(app.cursor, value) {
		app.say(true, "Document is at the 1 MiB limit", nil, "")
		return
	}
	for _, character := range value {
		if character == '\n' {
			app.line++
		}
	}
	app.cursor += len(value)
	app.changed()
}

func (app *App) backspace() {
	if app.cursor == 0 {
		return
	}
	start := app.previousCharacter(app.cursor)
	if app.buffer.At(app.cursor-1) == '\n' {
		start = app.cursor - 1
		app.line--
	}
	app.buffer.Delete(start, app.cursor-start)
	app.cursor = start
	app.changed()
}

func (app *App) deleteForward() {
	if app.cursor == app.buffer.Len() {
		return
	}
	size := 1
	if app.buffer.At(app.cursor) != '\n' {
		size = app.characterSize(app.cursor)
	}
	app.buffer.Delete(app.cursor, size)
	app.changed()
}

func (app *App) changed() {
	app.modified = true
	app.messageN, app.messageError = 0, false
	app.settle(true)
}

// settle recomputes the cursor's column after a move or an edit and scrolls
// it into view. An edit can join bytes into one character around the
// cursor; the cursor then moves to the end of that character. setGoal is
// false for vertical moves, which aim for the goal column instead of setting
// it.
func (app *App) settle(setGoal bool) {
	offset := app.lineStart(app.cursor)
	column := 0
	for offset < app.cursor {
		offset += app.characterSize(offset)
		column++
	}
	app.cursor, app.column = offset, column
	if setGoal {
		app.goal = column
	}
	app.scroll()
}

// scroll moves the view the least that shows the cursor. Every search is
// bounded by the visible lines or by the cursor's own line.
func (app *App) scroll() {
	rows := app.bodyRows()
	switch {
	case app.line < app.topLine || rows == 0:
		app.topOffset, app.topLine = app.lineStart(app.cursor), app.line
	case app.line >= app.topLine+rows:
		start := app.lineStart(app.cursor)
		for lines := 1; lines < rows && start > 0; lines++ {
			start = app.lineStart(start - 1)
		}
		app.topOffset, app.topLine = start, app.line-rows+1
	}
	width := max(app.columns, 1)
	if app.column < app.leftColumn {
		app.leftColumn = app.column
	} else if app.column >= app.leftColumn+width {
		app.leftColumn = app.column - width + 1
	}
}

func (app *App) lineStart(offset int) int {
	for offset > 0 && app.buffer.At(offset-1) != '\n' {
		offset--
	}
	return offset
}

func (app *App) lineEnd(offset int) int {
	length := app.buffer.Len()
	for offset < length && app.buffer.At(offset) != '\n' {
		offset++
	}
	return offset
}

// offsetAtColumn walks column characters from the line start at start and
// stops early at the end of the line.
func (app *App) offsetAtColumn(start, column int) int {
	length := app.buffer.Len()
	for ; column > 0 && start < length && app.buffer.At(start) != '\n'; column-- {
		start += app.characterSize(start)
	}
	return start
}

// characterSize mirrors the terminal's rule: the length of the well-formed
// UTF-8 sequence at offset, or 1 for a byte that starts none.
func (app *App) characterSize(offset int) int {
	lead := app.buffer.At(offset)
	size := 1
	switch {
	case lead < 0x80:
		return 1
	case lead >= 0xc2 && lead <= 0xdf:
		size = 2
	case lead >= 0xe0 && lead <= 0xef:
		size = 3
	case lead >= 0xf0 && lead <= 0xf4:
		size = 4
	default:
		return 1
	}
	if offset+size > app.buffer.Len() {
		return 1
	}
	for index := 1; index < size; index++ {
		if app.buffer.At(offset+index)&0xc0 != 0x80 {
			return 1
		}
	}
	return size
}

// previousCharacter finds the start of the character that ends at offset. A
// lead byte cannot be inside another sequence, so the first lead byte whose
// sequence ends exactly at offset is the forward scan's boundary too.
func (app *App) previousCharacter(offset int) int {
	for size := utf8.UTFMax; size > 1; size-- {
		if start := offset - size; start >= 0 && app.characterSize(start) == size {
			return start
		}
	}
	return offset - 1
}

// say sets the message line: prefix, then subject, then reason.
func (app *App) say(isError bool, prefix string, subject []byte, reason string) {
	target := app.message[:0]
	target = appendText(target, prefix)
	target = appendBounded(target, subject...)
	target = appendText(target, reason)
	app.messageN, app.messageError = len(target), isError
}
