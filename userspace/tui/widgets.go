package tui

// Text clears rect and draws one clipped line.
func Text(screen *Screen, rect Rect, value []byte, role Role) {
	if screen == nil {
		return
	}
	screen.Clear(rect, role)
	screen.Text(rect, 0, 0, value, role)
}

// Border draws a one-cell frame. boxDrawing selects the terminal's existing
// light box glyphs; false uses ASCII for a safe fallback.
func Border(screen *Screen, rect Rect, role Role, boxDrawing bool) Rect {
	if screen == nil {
		return Rect{}
	}
	rect = rect.Intersect(screen.Bounds())
	if rect.Width < 2 || rect.Height < 2 {
		return Rect{}
	}
	topLeft, topRight, bottomLeft, bottomRight := []byte("+"), []byte("+"), []byte("+"), []byte("+")
	horizontal, vertical := []byte("-"), []byte("|")
	if boxDrawing {
		topLeft, topRight, bottomLeft, bottomRight = []byte("┌"), []byte("┐"), []byte("└"), []byte("┘")
		horizontal, vertical = []byte("─"), []byte("│")
	}
	screen.putBytes(rect.X, rect.Y, topLeft, role)
	screen.putBytes(rect.Right()-1, rect.Y, topRight, role)
	screen.putBytes(rect.X, rect.Bottom()-1, bottomLeft, role)
	screen.putBytes(rect.Right()-1, rect.Bottom()-1, bottomRight, role)
	for column := rect.X + 1; column < rect.Right()-1; column++ {
		screen.putBytes(column, rect.Y, horizontal, role)
		screen.putBytes(column, rect.Bottom()-1, horizontal, role)
	}
	for row := rect.Y + 1; row < rect.Bottom()-1; row++ {
		screen.putBytes(rect.X, row, vertical, role)
		screen.putBytes(rect.Right()-1, row, vertical, role)
	}
	return rect.Inset(1)
}

// StatusBar fills rect and clips text into it.
func StatusBar(screen *Screen, rect Rect, text []byte, role Role) {
	if screen == nil {
		return
	}
	screen.Clear(rect, role)
	screen.Text(rect, 0, 0, text, role)
}

// Dialog describes the bounded common parts of a modal prompt. Empty Input
// and ConfirmText omit those rows.
type Dialog struct {
	Title       []byte
	Message     []byte
	Input       []byte
	ConfirmText []byte
	Width       int
	Height      int
	BoxDrawing  bool
}

// Draw centers a dialog inside bounds and returns its clipped content area.
func (dialog Dialog) Draw(screen *Screen, bounds Rect) Rect {
	if screen == nil {
		return Rect{}
	}
	bounds = bounds.Intersect(screen.Bounds())
	width, height := dialog.Width, dialog.Height
	if width < 4 {
		width = 4
	}
	if height < 3 {
		height = 3
	}
	width, height = min(width, bounds.Width), min(height, bounds.Height)
	if width < 2 || height < 2 {
		return Rect{}
	}
	box := Rect{X: bounds.X + (bounds.Width-width)/2, Y: bounds.Y + (bounds.Height-height)/2, Width: width, Height: height}
	screen.Clear(box, RoleDialog)
	content := Border(screen, box, RoleBorder, dialog.BoxDrawing)
	if content.Empty() {
		return content
	}
	if len(dialog.Title) != 0 && box.Width > 4 {
		title := Rect{X: box.X + 2, Y: box.Y, Width: box.Width - 4, Height: 1}
		screen.Text(title, 0, 0, dialog.Title, RoleDialog)
	}
	row := 0
	screen.Text(content, row, 0, dialog.Message, RoleDialog)
	row++
	if len(dialog.Input) != 0 && row < content.Height {
		screen.Clear(Rect{X: content.X, Y: content.Y + row, Width: content.Width, Height: 1}, RoleInput)
		screen.Text(content, row, 0, dialog.Input, RoleInput)
		row++
	}
	if len(dialog.ConfirmText) != 0 && content.Height != 0 {
		screen.Text(content, content.Height-1, 0, dialog.ConfirmText, RoleDialog)
	}
	return content
}

// ListViewport maps caller-owned item text to visible rows.
type ListViewport struct {
	Top      int
	Selected int
}

func (viewport *ListViewport) Clamp(count, height int) {
	if count <= 0 {
		viewport.Top, viewport.Selected = 0, 0
		return
	}
	viewport.Selected = min(max(viewport.Selected, 0), count-1)
	maximumTop := max(count-max(height, 0), 0)
	viewport.Top = min(max(viewport.Top, 0), maximumTop)
	if viewport.Selected < viewport.Top {
		viewport.Top = viewport.Selected
	}
	if height > 0 && viewport.Selected >= viewport.Top+height {
		viewport.Top = viewport.Selected - height + 1
	}
}

func (viewport *ListViewport) Draw(screen *Screen, rect Rect, items [][]byte, normal, selected Role) {
	if screen == nil {
		return
	}
	rect = rect.Intersect(screen.Bounds())
	viewport.Clamp(len(items), rect.Height)
	screen.Clear(rect, normal)
	for row := 0; row < rect.Height && viewport.Top+row < len(items); row++ {
		role := normal
		if viewport.Top+row == viewport.Selected {
			role = selected
			screen.Clear(Rect{X: rect.X, Y: rect.Y + row, Width: rect.Width, Height: 1}, role)
		}
		screen.Text(rect, row, 0, items[viewport.Top+row], role)
	}
}
