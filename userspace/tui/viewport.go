package tui

import "goos/userspace/terminal"

type Position struct {
	Line, Column int
}

// TextViewport draws directly from caller-owned text. Lines and horizontal
// columns are found during the frame; no line index or substring is allocated.
type TextViewport struct {
	TopLine, LeftColumn int
	Cursor              Position
	CursorVisible       bool
	SelectionStart      Position
	SelectionEnd        Position
	SelectionVisible    bool
}

func (viewport *TextViewport) ScrollLines(delta int) {
	if delta < 0 && viewport.TopLine < -delta {
		viewport.TopLine = 0
		return
	}
	viewport.TopLine += delta
	if viewport.TopLine < 0 {
		viewport.TopLine = 0
	}
}

func (viewport *TextViewport) ScrollColumns(delta int) {
	if delta < 0 && viewport.LeftColumn < -delta {
		viewport.LeftColumn = 0
		return
	}
	viewport.LeftColumn += delta
	if viewport.LeftColumn < 0 {
		viewport.LeftColumn = 0
	}
}

func (viewport *TextViewport) Draw(screen *Screen, rect Rect, text []byte, normal, selection, cursor Role) {
	if screen == nil {
		return
	}
	rect = rect.Intersect(screen.Bounds())
	screen.Clear(rect, normal)
	offset, line := lineOffset(text, max(viewport.TopLine, 0))
	for row := 0; row < rect.Height && offset <= len(text); row, line = row+1, line+1 {
		end := offset
		for end < len(text) && text[end] != '\n' {
			end++
		}
		viewport.drawLine(screen, rect, row, line, text[offset:end], normal, selection, cursor)
		if end == len(text) {
			break
		}
		offset = end + 1
	}
}

func (viewport *TextViewport) drawLine(screen *Screen, rect Rect, row, line int, value []byte, normal, selection, cursor Role) {
	column := 0
	for len(value) != 0 {
		character := terminal.Cut(value, 1)
		if len(character) == 0 {
			break
		}
		if column >= viewport.LeftColumn && column-viewport.LeftColumn < rect.Width {
			role := normal
			position := Position{Line: line, Column: column}
			if viewport.selected(position) {
				role = selection
			}
			if viewport.CursorVisible && position == viewport.Cursor {
				role = cursor
			}
			screen.putBytes(rect.X+column-viewport.LeftColumn, rect.Y+row, character, role)
		}
		column++
		if column-viewport.LeftColumn >= rect.Width {
			break
		}
		value = value[len(character):]
	}
	if viewport.CursorVisible && viewport.Cursor.Line == line && viewport.Cursor.Column == column && column >= viewport.LeftColumn && column-viewport.LeftColumn < rect.Width {
		screen.put(rect.X+column-viewport.LeftColumn, rect.Y+row, blank(cursor))
	}
}

func (viewport *TextViewport) selected(position Position) bool {
	if !viewport.SelectionVisible {
		return false
	}
	start, end := viewport.SelectionStart, viewport.SelectionEnd
	if positionLess(end, start) {
		start, end = end, start
	}
	return !positionLess(position, start) && positionLess(position, end)
}

func positionLess(left, right Position) bool {
	return left.Line < right.Line || left.Line == right.Line && left.Column < right.Column
}

func lineOffset(text []byte, wanted int) (int, int) {
	offset, line := 0, 0
	for offset < len(text) && line < wanted {
		if text[offset] == '\n' {
			line++
		}
		offset++
	}
	return offset, line
}
