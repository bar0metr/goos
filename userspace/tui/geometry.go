// Package tui provides bounded terminal-cell rendering primitives above the
// low-level ANSI contract in userspace/terminal.
package tui

// Rect is a half-open terminal-cell rectangle.
type Rect struct {
	X, Y          int
	Width, Height int
}

// NewRect validates non-negative geometry and coordinate addition.
func NewRect(x, y, width, height int) (Rect, bool) {
	if x < 0 || y < 0 || width < 0 || height < 0 || x > maxInt-width || y > maxInt-height {
		return Rect{}, false
	}
	return Rect{X: x, Y: y, Width: width, Height: height}, true
}

func (rect Rect) Right() int  { return rect.X + rect.Width }
func (rect Rect) Bottom() int { return rect.Y + rect.Height }
func (rect Rect) Empty() bool { return rect.Width == 0 || rect.Height == 0 }
func (rect Rect) Valid() bool {
	return rect.X >= 0 && rect.Y >= 0 && rect.Width >= 0 && rect.Height >= 0 &&
		rect.X <= maxInt-rect.Width && rect.Y <= maxInt-rect.Height
}

// Intersect clips two validated rectangles.
func (rect Rect) Intersect(other Rect) Rect {
	if !rect.Valid() || !other.Valid() {
		return Rect{}
	}
	left, top := max(rect.X, other.X), max(rect.Y, other.Y)
	right, bottom := min(rect.Right(), other.Right()), min(rect.Bottom(), other.Bottom())
	if right <= left || bottom <= top {
		return Rect{X: left, Y: top}
	}
	return Rect{X: left, Y: top, Width: right - left, Height: bottom - top}
}

// Inset removes cells from every edge, returning an empty rectangle when the
// inset consumes it.
func (rect Rect) Inset(amount int) Rect {
	if amount <= 0 {
		return rect
	}
	if amount > rect.Width/2 || amount > rect.Height/2 {
		return Rect{X: rect.X + rect.Width/2, Y: rect.Y + rect.Height/2}
	}
	return Rect{X: rect.X + amount, Y: rect.Y + amount, Width: rect.Width - 2*amount, Height: rect.Height - 2*amount}
}

const maxInt = int(^uint(0) >> 1)
