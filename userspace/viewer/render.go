package viewer

import (
	"goos/kernel/vfs"
	"goos/userspace/terminal"
	"goos/userspace/tui"
)

// Render draws one bounded frame. File control bytes are converted to visible
// question marks or spaces before they reach the terminal layer.
func (app *App) Render(output *terminal.Output) {
	if output == nil {
		return
	}
	if app.clear {
		output.ClearScreen()
		app.clear = false
	}
	app.screen.Clear(app.screen.Bounds(), tui.RoleGround)
	body := tui.Rect{Width: app.columns, Height: app.bodyRows()}
	switch {
	case app.failure != vfs.StatusOK:
		tui.Text(&app.screen, body, []byte("gview: file read failed"), tui.RoleText)
	case app.binary:
		tui.Text(&app.screen, body, []byte("gview: binary file; text display refused"), tui.RoleText)
	default:
		text, status := app.visibleText(body.Width, body.Height)
		if status != vfs.StatusOK {
			app.failure = status
			tui.Text(&app.screen, body, []byte("gview: file read failed"), tui.RoleText)
		} else {
			viewport := tui.TextViewport{}
			viewport.Draw(&app.screen, body, text, tui.RoleText, tui.RoleSelection, tui.RoleCursor)
		}
	}
	if app.rows > 0 {
		status := app.statusText()
		tui.StatusBar(&app.screen, tui.Rect{Y: app.rows - 1, Width: app.columns, Height: 1}, status, tui.RoleStatus)
	}
	app.screen.Flush(output)
}

func (app *App) visibleText(width, height int) ([]byte, vfs.Status) {
	used := 0
	offset := app.topOffset
	for row := 0; row < height; row++ {
		line, next, status := app.visibleLine(offset, app.leftColumn, width, app.render[used:])
		if status != vfs.StatusOK {
			return nil, status
		}
		used += line
		if row+1 < height && used < len(app.render) {
			app.render[used] = '\n'
			used++
		}
		if !next {
			break
		}
		var found bool
		offset, found, status = app.nextLine(offset)
		if status != vfs.StatusOK {
			return nil, status
		}
		if !found {
			break
		}
	}
	return app.render[:used], vfs.StatusOK
}

// visibleLine copies at most width one-cell glyphs after leftColumn. It still
// scans to the line ending so the next visible row has an exact file offset.
func (app *App) visibleLine(start uint64, leftColumn, width int, target []byte) (int, bool, vfs.Status) {
	column, used := 0, 0
	for offset := start; offset < app.size; {
		character, size, status := app.characterAt(offset)
		if status != vfs.StatusOK {
			return 0, false, status
		}
		if size == 1 && character[0] == '\n' {
			return used, true, vfs.StatusOK
		}
		offset += uint64(size)
		if size == 1 && character[0] == '\r' {
			if next, ok, nextStatus := app.byteAt(offset); nextStatus != vfs.StatusOK {
				return 0, false, nextStatus
			} else if ok && next == '\n' {
				continue
			}
			character[0] = '?'
		}
		if size == 1 && character[0] == '\t' {
			character[0] = ' '
		} else if size == 1 && (character[0] < 0x20 || character[0] == 0x7f) {
			character[0] = '?'
		}
		if column >= leftColumn && column-leftColumn < width && used+size <= len(target) {
			copy(target[used:], character[:size])
			used += size
		}
		column++
	}
	return used, false, vfs.StatusOK
}

func (app *App) characterAt(offset uint64) ([4]byte, int, vfs.Status) {
	var character [4]byte
	lead, ok, status := app.byteAt(offset)
	if status != vfs.StatusOK || !ok {
		return character, 0, status
	}
	character[0] = lead
	size := utf8Size(lead)
	if size == 1 {
		return character, 1, vfs.StatusOK
	}
	for index := 1; index < size; index++ {
		value, present, readStatus := app.byteAt(offset + uint64(index))
		if readStatus != vfs.StatusOK {
			return character, 0, readStatus
		}
		if !present || value&0xc0 != 0x80 {
			return character, 1, vfs.StatusOK
		}
		character[index] = value
	}
	return character, size, vfs.StatusOK
}

func utf8Size(lead byte) int {
	switch {
	case lead >= 0xc2 && lead <= 0xdf:
		return 2
	case lead >= 0xe0 && lead <= 0xef:
		return 3
	case lead >= 0xf0 && lead <= 0xf4:
		return 4
	}
	return 1
}

func (app *App) statusText() []byte {
	target := app.status[:0]
	// The numeric fields remain visible at the minimum 80-column geometry;
	// a long path yields its space first and is marked as truncated.
	pathLimit := max(app.columns-62, 0)
	if app.pathN <= pathLimit {
		target = appendBounded(target, app.path[:app.pathN]...)
	} else if pathLimit >= 3 {
		target = appendBounded(target, app.path[:pathLimit-3]...)
		target = appendText(target, "...")
	}
	target = appendText(target, "  size ")
	target = appendUint(target, app.size)
	target = appendText(target, "  offset ")
	target = appendUint(target, app.topOffset)
	target = appendText(target, "  ")
	target = appendUint(target, percentage(app.topOffset, app.size))
	target = appendBounded(target, '%')
	return target
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

func percentage(offset, size uint64) uint64 {
	if size == 0 || offset >= size {
		return 100
	}
	const maximum = ^uint64(0)
	if offset <= maximum/100 {
		return offset * 100 / size
	}
	divisor := size / 100
	if divisor == 0 {
		return 99
	}
	return min(offset/divisor, uint64(99))
}
