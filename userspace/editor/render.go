package editor

import (
	"goos/userspace/shell"
	"goos/userspace/terminal"
	"goos/userspace/tui"
)

// The fixed texts live in package variables, so drawing them converts
// nothing during a frame.
var (
	keyHints       = []byte("F2 Save  F3 Save As  F10 Quit")
	refusedHint    = []byte("Enter, Escape or F10 returns to the shell")
	quitTitle      = []byte(" Unsaved changes ")
	quitChoices    = []byte("y Save  n Discard  Esc Cancel")
	saveAsTitle    = []byte(" Save As ")
	saveAsPrompt   = []byte("File name:")
	saveAsChoices  = []byte("Enter Save  Esc Cancel")
	replaceChoices = []byte("y Replace  n Cancel")
	modifiedMarker = []byte(" [modified]")
	editPrefix     = []byte("edit: ")
)

// Render draws one bounded frame. File bytes pass through visibleText, which
// turns control bytes into visible cells before anything reaches the
// terminal layer.
func (app *App) Render(output *terminal.Output) {
	if output == nil {
		return
	}
	if app.clear {
		output.HideCursor()
		output.ClearScreen()
		app.clear = false
	}
	app.screen.Clear(app.screen.Bounds(), tui.RoleGround)
	body := tui.Rect{Width: app.columns, Height: app.bodyRows()}
	if app.refused {
		app.renderRefusal(body)
	} else {
		viewport := tui.TextViewport{
			Cursor:        tui.Position{Line: app.line - app.topLine, Column: app.column - app.leftColumn},
			CursorVisible: app.dialog == dialogNone,
		}
		viewport.Draw(&app.screen, body, app.visibleText(body.Width, body.Height), tui.RoleText, tui.RoleSelection, tui.RoleCursor)
	}
	if app.rows >= 2 {
		tui.StatusBar(&app.screen, tui.Rect{Y: app.rows - 2, Width: app.columns, Height: 1}, app.statusText(), tui.RoleStatus)
	}
	app.renderMessage(tui.Rect{Y: app.rows - 1, Width: app.columns, Height: 1})
	app.renderDialog()
	app.screen.Flush(output)
}

// visibleText copies the visible part of each visible line into the render
// buffer, one cell per character, with lines separated by newlines. Tab shows
// as one space and every other C0 control, DEL and the C1 controls show as
// '?'. The document itself keeps the bytes.
func (app *App) visibleText(width, height int) []byte {
	used := 0
	offset := app.topOffset
	length := app.buffer.Len()
	for row := 0; row < height; row++ {
		column := 0
		for offset < length && app.buffer.At(offset) != '\n' {
			size := app.characterSize(offset)
			if column >= app.leftColumn && column-app.leftColumn < width && used+size <= len(app.render) {
				used += app.cell(offset, size, app.render[used:])
			}
			offset += size
			column++
		}
		if offset >= length || row+1 == height || used == len(app.render) {
			break
		}
		app.render[used] = '\n'
		used++
		offset++
	}
	return app.render[:used]
}

// cell writes the character of size bytes at offset as display bytes.
func (app *App) cell(offset, size int, target []byte) int {
	lead := app.buffer.At(offset)
	switch {
	case size == 1 && lead == '\t':
		target[0] = ' '
		return 1
	case size == 1 && (lead < 0x20 || lead == 0x7f):
		target[0] = '?'
		return 1
	case size == 2 && lead == 0xc2 && app.buffer.At(offset+1) < 0xa0:
		// U+0080 to U+009F are the C1 controls, CSI among them.
		target[0] = '?'
		return 1
	}
	for index := 0; index < size; index++ {
		target[index] = app.buffer.At(offset + index)
	}
	return size
}

func (app *App) renderRefusal(body tui.Rect) {
	line := appendBounded(app.scratch[:0], editPrefix...)
	line = appendBounded(line, app.path[:app.pathN]...)
	app.screen.Text(body, 0, 0, line, tui.RoleText)
	app.screen.Text(body, 1, 0, app.message[:app.messageN], tui.RoleWarning)
}

// statusText is the path, the modified marker and the cursor's line:column,
// right-aligned. A long path gives up its beginning first.
func (app *App) statusText() []byte {
	right := appendUint(app.position[:0], uint64(app.line+1))
	right = appendBounded(right, ':')
	right = appendUint(right, uint64(app.column+1))

	marker := 0
	if app.modified {
		marker = len(modifiedMarker)
	}
	target := app.status[:0]
	available := app.columns - marker - len(right) - 3
	path := app.path[:app.pathN]
	if width := terminal.Width(path); width > available {
		if available > 3 {
			target = appendText(target, "...")
			path = tail(path, available-3)
		} else {
			path = nil
		}
	}
	target = appendBounded(target, path...)
	target = appendBounded(target, modifiedMarker[:marker]...)
	for pad := app.columns - terminal.Width(target) - len(right) - 1; pad > 0; pad-- {
		target = appendBounded(target, ' ')
	}
	return appendBounded(target, right...)
}

// renderMessage fills the bottom row: the last message, amber for an error,
// or the key summary.
func (app *App) renderMessage(rect tui.Rect) {
	switch {
	case app.refused:
		tui.Text(&app.screen, rect, refusedHint, tui.RoleText)
	case app.messageN != 0 && app.messageError:
		tui.Text(&app.screen, rect, app.message[:app.messageN], tui.RoleWarning)
	case app.messageN != 0:
		tui.Text(&app.screen, rect, app.message[:app.messageN], tui.RoleText)
	default:
		tui.Text(&app.screen, rect, keyHints, tui.RoleText)
	}
}

func (app *App) renderDialog() {
	if app.dialog == dialogNone {
		return
	}
	dialog := tui.Dialog{Width: min(max(app.columns-8, 24), 72), Height: 5, BoxDrawing: true}
	switch app.dialog {
	case dialogQuit:
		dialog.Title, dialog.ConfirmText = quitTitle, quitChoices
		line := appendText(app.scratch[:0], "Save changes to ")
		line = appendBounded(line, shell.BaseName(app.path[:app.pathN])...)
		dialog.Message = appendBounded(line, '?')
	case dialogOverwrite:
		dialog.Title, dialog.ConfirmText = saveAsTitle, replaceChoices
		line := appendText(app.scratch[:0], "Replace ")
		line = appendBounded(line, shell.BaseName(app.target[:app.targetN])...)
		dialog.Message = appendText(line, "? It exists.")
	case dialogSaveAs:
		dialog.Title, dialog.Message, dialog.ConfirmText = saveAsTitle, saveAsPrompt, saveAsChoices
		// The field shows the end of the name and a cursor after it.
		visible := max(dialog.Width-3, 1)
		line := appendBounded(app.scratch[:0], tail(app.input[:app.inputN], visible-1)...)
		dialog.Input = appendBounded(line, '_')
	}
	dialog.Draw(&app.screen, app.screen.Bounds())
}

// tail returns the end of value that fits in columns cells.
func tail(value []byte, columns int) []byte {
	if columns <= 0 {
		return nil
	}
	width := terminal.Width(value)
	if width <= columns {
		return value
	}
	return value[len(terminal.Cut(value, width-columns)):]
}

func appendText(target []byte, value string) []byte {
	for index := 0; index < len(value) && len(target) < cap(target); index++ {
		target = append(target, value[index])
	}
	return target
}

func appendBounded(target []byte, values ...byte) []byte {
	available := cap(target) - len(target)
	if len(values) > available {
		values = values[:available]
	}
	return append(target, values...)
}

func appendUint(target []byte, value uint64) []byte {
	var digits [20]byte
	index := len(digits)
	for {
		index--
		digits[index] = byte(value%10) + '0'
		value /= 10
		if value == 0 {
			return appendBounded(target, digits[index:]...)
		}
	}
}
