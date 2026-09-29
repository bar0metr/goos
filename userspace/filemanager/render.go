package filemanager

import (
	"goos/internal/timekeeping"
	"goos/kernel/ramfs"
	"goos/userspace/fsdisplay"
	"goos/userspace/terminal"
)

// The theme names ANSI colours by role; the console decides the RGB. It is a
// calm dark scheme: a near-black navy ground, steel-blue selection, muted
// teal frames and warm yellow directories.
const (
	colorGround         = terminal.Black
	colorText           = terminal.White
	colorDirectory      = terminal.BrightYellow
	colorParent         = terminal.BrightWhite
	colorFrameActive    = terminal.BrightCyan
	colorFrame          = terminal.Cyan
	colorHeader         = terminal.Cyan
	colorTitleActiveFg  = terminal.BrightWhite
	colorTitleActiveBg  = terminal.Cyan
	colorSelectionFg    = terminal.BrightWhite
	colorSelectionBg    = terminal.Blue
	colorInactiveCursor = terminal.BrightBlack
	colorWarning        = terminal.BrightYellow
	colorKeyNumber      = terminal.BrightYellow
	colorKeyLabelFg     = terminal.BrightWhite
	colorKeyLabelBg     = terminal.Cyan
	colorDialogBg       = terminal.BrightBlack
	colorDialogFrame    = terminal.BrightCyan
	colorDialogText     = terminal.BrightWhite
	colorFieldBg        = terminal.Black
)

// sizeWidth is the width of the size column, a space on each side included,
// and dateWidth that of the modification-time column. The date column is
// shown only while the name column keeps minNameWidth: at 80 columns the
// information line carries the time instead.
const (
	sizeWidth    = 10
	dateWidth    = 14
	minNameWidth = 20
)

// datesShown reports whether a panel inner columns wide has a date column.
func datesShown(inner int) bool { return inner-1-sizeWidth-1-dateWidth >= minNameWidth }

// rowStorage bounds one composed row: every cell as a three-byte box glyph
// or a UTF-8 character, plus the colour changes along it.
const rowStorage = maxColumns*4 + 1024

// screen remembers what each row last looked like, so a frame only sends the
// rows that changed: moving the selection redraws two rows, not the screen.
type screen struct {
	hashes  [maxRows]uint64
	valid   [maxRows]bool
	cleared bool
	row     [rowStorage]byte
	// foreground and background are the colours the row being composed
	// has selected, so a run of cells in one style costs one sequence.
	foreground, background terminal.Color
	styled                 bool
}

// invalidate forgets what is on the screen: the next frame clears it and
// draws every row.
func (screen *screen) invalidate() {
	screen.valid = [maxRows]bool{}
	screen.cleared = false
}

// Render writes the next frame to output, sending only the rows that differ
// from the last one, and flushes it.
func (app *App) Render(output *terminal.Output) {
	screen := &app.screen
	if !screen.cleared {
		output.HideCursor()
		output.SetStyle(colorText, colorGround)
		output.ClearScreen()
		screen.cleared = true
	}
	row := terminal.NewOutput(screen.row[:], nil)
	for index := 0; index < app.rows; index++ {
		row.Discard()
		screen.styled = false
		app.composeRow(&row, index)
		hash := fnv(row.Pending())
		if screen.valid[index] && screen.hashes[index] == hash {
			continue
		}
		screen.hashes[index], screen.valid[index] = hash, true
		output.MoveTo(index, 0)
		output.Write(row.Pending())
	}
	output.Flush()
}

func (app *App) composeRow(out *terminal.Output, row int) {
	if app.tooSmall {
		app.style(out, colorText, colorGround)
		if row == 0 {
			out.TextString("gfm: terminal too small (need at least 80x24); F10 quits", app.columns-1)
		}
		return
	}
	panelRows := app.rows - 2
	switch {
	case row < panelRows:
		left := app.columns / 2
		app.composePanelRow(out, 0, row, left)
		app.composePanelRow(out, 1, row, app.columns-left)
	case row == app.rows-2:
		app.composeStatus(out)
	default:
		app.composeKeyBar(out)
	}
	if app.dialog.kind != dialogNone {
		app.composeDialogRow(out, row)
	}
}

// style selects colours for the cells that follow, if they differ from the
// ones already selected in this row.
func (app *App) style(out *terminal.Output, foreground, background terminal.Color) {
	screen := &app.screen
	if screen.styled && screen.foreground == foreground && screen.background == background {
		return
	}
	out.SetStyle(foreground, background)
	screen.foreground, screen.background, screen.styled = foreground, background, true
}

// composePanelRow draws row of panel index, width columns wide:
//
//	┌─ /path ──────────┐   top border with the directory
//	│ Name       │ Size│   header
//	│ entry      │  123│   list rows
//	├────────────┴─────┤
//	│ selected entry   │   information line
//	└──── 12 entries ──┘   bottom border with the count
func (app *App) composePanelRow(out *terminal.Output, index, row, width int) {
	panel := &app.panels[index]
	active := index == app.active
	frame := colorFrame
	if active {
		frame = colorFrameActive
	}
	inner := width - 2
	nameWidth := inner - 1 - sizeWidth
	dates := datesShown(inner)
	if dates {
		nameWidth -= 1 + dateWidth
	}
	list := app.listRows()
	switch {
	case row == 0:
		app.composeTitle(out, panel, active, frame, inner)
		return
	case row == 1:
		app.style(out, frame, colorGround)
		out.WriteString("│")
		app.style(out, colorHeader, colorGround)
		out.TextString(" Name", nameWidth)
		app.style(out, frame, colorGround)
		out.WriteString("│")
		app.style(out, colorHeader, colorGround)
		out.Pad(sizeWidth - 5)
		out.WriteString("Size ")
		if dates {
			app.style(out, frame, colorGround)
			out.WriteString("│")
			app.style(out, colorHeader, colorGround)
			out.TextString(" Modified", dateWidth)
		}
	case row < 2+list:
		app.style(out, frame, colorGround)
		out.WriteString("│")
		app.composeEntry(out, panel, active, panel.top+row-2, nameWidth, dates, frame)
	case row == 2+list:
		app.style(out, frame, colorGround)
		out.WriteString("├")
		out.Repeat("─", nameWidth)
		out.WriteString("┴")
		out.Repeat("─", sizeWidth)
		if dates {
			out.WriteString("┴")
			out.Repeat("─", dateWidth)
		}
		out.WriteString("┤")
		return
	case row == 3+list:
		app.style(out, frame, colorGround)
		out.WriteString("│")
		app.composeInformation(out, panel, inner)
	default:
		app.composeBottom(out, panel, frame, inner)
		return
	}
	app.style(out, frame, colorGround)
	out.WriteString("│")
}

func (app *App) composeTitle(out *terminal.Output, panel *Panel, active bool, frame terminal.Color, inner int) {
	app.style(out, frame, colorGround)
	out.WriteString("┌─")
	path := panel.Path()
	room := inner - 3
	var dots string
	if terminal.Width(path) > room {
		// Show the end of a long path: that is the part that changes.
		dots = "..."
		for terminal.Width(path) > room-3 {
			path = path[characterLength(path):]
		}
	}
	if active {
		app.style(out, colorTitleActiveFg, colorTitleActiveBg)
	} else {
		app.style(out, colorText, colorGround)
	}
	out.WriteString(" ")
	out.WriteString(dots)
	used := 1 + len(dots) + out.Text(path, terminal.Width(path)) + 1
	out.WriteString(" ")
	app.style(out, frame, colorGround)
	out.Repeat("─", inner-1-used)
	out.WriteString("┐")
}

func (app *App) composeEntry(out *terminal.Output, panel *Panel, active bool, index, nameWidth int, dates bool, frame terminal.Color) {
	if index >= panel.count {
		app.style(out, colorText, colorGround)
		out.Pad(nameWidth)
		app.style(out, frame, colorGround)
		out.WriteString("│")
		app.style(out, colorText, colorGround)
		out.Pad(sizeWidth)
		if dates {
			app.style(out, frame, colorGround)
			out.WriteString("│")
			app.style(out, colorText, colorGround)
			out.Pad(dateWidth)
		}
		return
	}
	entry, name := panel.Entry(index)
	foreground, background := colorText, colorGround
	switch {
	case IsParent(name):
		foreground = colorParent
	case entry.Kind == ramfs.KindDirectory:
		foreground = colorDirectory
	}
	separator := frame
	if index == panel.selected {
		background = colorInactiveCursor
		if active {
			background = colorSelectionBg
		}
		foreground, separator = colorSelectionFg, colorSelectionFg
	}
	app.style(out, foreground, background)
	out.WriteString(" ")
	app.writeName(out, name, entry.Kind == ramfs.KindDirectory && !IsParent(name), nameWidth-1)
	app.style(out, separator, background)
	out.WriteString("│")
	app.style(out, foreground, background)
	var size [24]byte
	text := entrySize(size[:0], entry, name)
	out.Pad(sizeWidth - 1 - len(text))
	out.Write(text)
	out.WriteString(" ")
	if dates {
		app.style(out, separator, background)
		out.WriteString("│")
		app.style(out, foreground, background)
		var date [24]byte
		text = date[:0]
		if !IsParent(name) {
			text = appendDate(text, entry.Modified, app.year)
		}
		out.WriteString(" ")
		out.Write(text)
		out.Pad(dateWidth - 1 - len(text))
	}
}

// writeName writes a name in width columns, with a slash after a directory
// and a tilde where a long one was cut.
func (app *App) writeName(out *terminal.Output, name []byte, directory bool, width int) {
	suffix := 0
	if directory {
		suffix = 1
	}
	if terminal.Width(name)+suffix > width {
		out.Text(name, width-1)
		out.WriteString("~")
		return
	}
	used := out.Text(name, terminal.Width(name))
	if directory {
		out.WriteString("/")
	}
	out.Pad(width - used - suffix)
}

func (app *App) composeInformation(out *terminal.Output, panel *Panel, inner int) {
	app.style(out, colorText, colorGround)
	entry, name, ok := panel.Current()
	if !ok {
		out.Pad(inner)
		return
	}
	var size [48]byte
	text := entrySize(size[:0], entry, name)
	if !IsParent(name) && entry.Modified.Available {
		text = appendDate(append(text, "  "...), entry.Modified, app.year)
	}
	out.WriteString(" ")
	app.writeName(out, name, entry.Kind == ramfs.KindDirectory && !IsParent(name), inner-2-len(text)-1)
	out.WriteString(" ")
	out.Write(text)
	out.WriteString(" ")
}

func (app *App) composeBottom(out *terminal.Output, panel *Panel, frame terminal.Color, inner int) {
	var label [48]byte
	text := append(label[:0], ' ')
	count := panel.count
	if panel.pathSize > 1 {
		count-- // ".." is not an entry of the directory.
	}
	text = appendDecimal(text, uint64(count))
	if panel.truncated {
		text = append(text, "+ entries, list truncated "...)
	} else if count == 1 {
		text = append(text, " entry "...)
	} else {
		text = append(text, " entries "...)
	}
	app.style(out, frame, colorGround)
	out.WriteString("└")
	var described [64]byte
	capacity := capacityText(described[:0], panel, inner-len(text)-3)
	if len(capacity) != 0 {
		out.WriteString("─")
		app.style(out, colorText, colorGround)
		out.Write(capacity)
		app.style(out, frame, colorGround)
		out.Repeat("─", inner-len(text)-2-len(capacity))
	} else {
		out.Repeat("─", inner-len(text)-1)
	}
	if panel.truncated {
		app.style(out, colorWarning, colorGround)
	}
	out.Write(text)
	app.style(out, frame, colorGround)
	out.WriteString("─┘")
}

// composeStatus writes the status line: the last operation's message, or the
// full path of the selected entry.
func (app *App) composeStatus(out *terminal.Output) {
	if app.statusSize != 0 {
		foreground := colorText
		if app.statusBad {
			foreground = colorWarning
		}
		app.style(out, foreground, colorGround)
		out.WriteString(" ")
		out.Text(app.status[:app.statusSize], app.columns-1)
		return
	}
	app.style(out, colorText, colorGround)
	out.WriteString(" ")
	panel := app.Active()
	_, name, ok := panel.Current()
	if !ok || IsParent(name) {
		out.Text(panel.Path(), app.columns-1)
		return
	}
	path := panel.Path()
	used := out.Text(path, min(terminal.Width(path), app.columns-1))
	if len(path) > 1 {
		out.WriteString("/")
		used++
	}
	out.Text(name, app.columns-1-used)
}

var keyBar = [...]struct{ number, label string }{
	{"F1", "Help"}, {"F2", "Reread"}, {"F3", "View"}, {"F4", "Edit"}, {"F5", "Copy"},
	{"F6", "Move"}, {"F7", "Mkdir"}, {"F8", "Delete"}, {"F10", "Quit"},
}

// composeKeyBar writes the function keys along the last row, each as wide as
// its label, with what is left over shared between them. It stops one column
// short of the corner: writing there would scroll the screen.
func (app *App) composeKeyBar(out *terminal.Output) {
	width := app.columns - 1
	natural := 0
	for _, key := range keyBar {
		natural += len(key.number) + len(key.label) + 2
	}
	spare := max(width-natural, 0) / len(keyBar)
	used := 0
	for _, key := range keyBar {
		room := len(key.number) + len(key.label) + 2 + spare
		if used+room > width {
			break
		}
		app.style(out, colorKeyNumber, colorGround)
		out.WriteString(key.number)
		app.style(out, colorKeyLabelFg, colorKeyLabelBg)
		out.WriteString(" ")
		out.TextString(key.label, room-len(key.number)-2)
		app.style(out, colorText, colorGround)
		out.WriteString(" ")
		used += room
	}
	app.style(out, colorText, colorGround)
	out.Pad(width - used)
}

// dialogBox returns where the open dialog sits and how many rows it has.
func (app *App) dialogBox() (top, left, width, height int) {
	width = 56
	height = 7
	if app.dialog.kind == dialogHelp {
		width, height = 60, len(helpLines)+4
	}
	if width > app.columns-4 {
		width = app.columns - 4
	}
	return (app.rows - 2 - height) / 2, (app.columns - width) / 2, width, height
}

var helpLines = [...]string{
	"Up, Down        move the selection",
	"PgUp, PgDn      move by a page",
	"Home, End       first or last entry",
	"Enter           open a directory, view a file",
	"Backspace       go to the parent directory",
	"Tab             switch panels",
	"F2              read both directories again",
	"F3, F4          view or edit the selected file",
	"F5              copy the file to the other panel",
	"F6              move or rename",
	"F7              create a directory",
	"F8              delete (asks first)",
	"F10             quit",
	"",
	"Press any key to close this help.",
}

func (app *App) composeDialogRow(out *terminal.Output, row int) {
	top, left, width, height := app.dialogBox()
	line := row - top
	if line < 0 || line >= height {
		return
	}
	out.MoveTo(row, left)
	inner := width - 2
	if line == 0 || line == height-1 {
		app.style(out, colorDialogFrame, colorDialogBg)
		if line == height-1 {
			out.WriteString("└")
			out.Repeat("─", inner)
			out.WriteString("┘")
			return
		}
		title := app.dialogTitle()
		out.WriteString("┌")
		fill := inner - len(title) - 2
		out.Repeat("─", fill/2)
		app.style(out, colorDialogText, colorDialogBg)
		out.WriteString(" ")
		out.WriteString(title)
		out.WriteString(" ")
		app.style(out, colorDialogFrame, colorDialogBg)
		out.Repeat("─", fill-fill/2)
		out.WriteString("┐")
		return
	}
	app.style(out, colorDialogFrame, colorDialogBg)
	out.WriteString("│")
	app.style(out, colorDialogText, colorDialogBg)
	out.WriteString(" ")
	app.composeDialogLine(out, line-1, inner-2)
	app.style(out, colorDialogText, colorDialogBg)
	out.WriteString(" ")
	app.style(out, colorDialogFrame, colorDialogBg)
	out.WriteString("│")
}

func (app *App) dialogTitle() string {
	switch app.dialog.kind {
	case dialogHelp:
		return "gfm keys"
	case dialogConfirm:
		return app.dialog.title
	}
	if app.dialog.action == actionMkdir {
		return "Create directory"
	}
	return "Move or rename"
}

// composeDialogLine writes line of the dialog's body in width columns.
func (app *App) composeDialogLine(out *terminal.Output, line, width int) {
	dialog := &app.dialog
	switch dialog.kind {
	case dialogHelp:
		if line >= 1 && line-1 < len(helpLines) {
			out.TextString(helpLines[line-1], width)
			return
		}
	case dialogConfirm:
		if line == 1 {
			used := out.TextString(dialog.question, len(dialog.question))
			out.WriteString("\"")
			used++
			name := dialog.subject[:dialog.subjectSize]
			if room := width - used - 2; terminal.Width(name) > room {
				used += out.Text(name, room-1)
				out.WriteString("~")
				used++
			} else {
				used += out.Text(name, terminal.Width(name))
			}
			out.WriteString("\"?")
			out.Pad(width - used - 2)
			return
		}
		if line == 3 {
			out.TextString("Enter = yes    Esc = cancel", width)
			return
		}
	case dialogInput:
		switch line {
		case 0:
			if dialog.action == actionMkdir {
				out.TextString("Name:", width)
			} else {
				out.TextString("Target path:", width)
			}
			return
		case 1:
			// The field shows the end of its text, where typing happens,
			// followed by a block standing in for the cursor.
			text := dialog.input[:dialog.inputSize]
			for terminal.Width(text) > width-1 {
				text = text[characterLength(text):]
			}
			app.style(out, colorDialogText, colorFieldBg)
			used := out.Text(text, terminal.Width(text))
			out.WriteString("_")
			out.Pad(width - used - 1)
			return
		case 3:
			out.TextString("Enter = OK    Esc = cancel", width)
			return
		}
	}
	out.Pad(width)
}

var monthNames = [...]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

// appendDate formats a modification time, in UTC, in twelve columns: the time
// of day for a date in the current year, the year otherwise, and a dash when
// the filesystem recorded none.
func appendDate(target []byte, stamp ramfs.Timestamp, current int64) []byte {
	if !stamp.Available {
		return append(target, '-')
	}
	year, month, day, hour, minute, _ := timekeeping.CivilUTC(stamp.Seconds)
	if month < 1 || month > 12 {
		return append(target, '-')
	}
	target = appendTwo(target, day, ' ')
	target = append(append(append(target, ' '), monthNames[month-1]...), ' ')
	if current == 0 || year == current {
		target = appendTwo(target, hour, '0')
		return appendTwo(append(target, ':'), minute, '0')
	}
	if year < 0 || year > 9999 {
		return append(target, " ????"...)
	}
	target = append(target, ' ')
	for divisor := int64(1000); divisor > 0; divisor /= 10 {
		target = append(target, byte('0'+year/divisor%10))
	}
	return target
}

// appendTwo writes a value below 100 in two columns, padded with pad.
func appendTwo(target []byte, value uint8, pad byte) []byte {
	if value < 10 {
		return append(target, pad, '0'+value)
	}
	return append(target, '0'+value/10, '0'+value%10)
}

// capacityText describes the filesystem of a panel in at most room columns:
// its type and free and total space, only the free space when that is all
// that fits, and nothing when not even that does. A read-only filesystem says
// so instead of reporting free space it cannot use.
func capacityText(target []byte, panel *Panel, room int) []byte {
	info, _, known := panel.Filesystem()
	if !known {
		return target
	}
	full := append(target, ' ')
	full = append(full, fsdisplay.TypeName(info.Type)...)
	switch {
	case info.ReadOnly:
		full = append(full, " ro"...)
	case info.CapacityAvailable:
		full = append(full, ' ')
		full = fsdisplay.AppendSize(full, info.AvailableBytes, true)
		full = append(full, " free / "...)
		full = fsdisplay.AppendSize(full, info.TotalBytes, true)
	}
	full = append(full, ' ')
	if len(full) <= room {
		return full
	}
	if info.ReadOnly || !info.CapacityAvailable {
		return target
	}
	short := fsdisplay.AppendSize(append(target, ' '), info.AvailableBytes, true)
	short = append(short, " free "...)
	if len(short) <= room {
		return short
	}
	return target
}

// entrySize formats the size column: <UP> for "..", <DIR> for a directory,
// the byte count for a file, scaled with a suffix once it has eight digits.
func entrySize(target []byte, entry Entry, name []byte) []byte {
	switch {
	case IsParent(name):
		return append(target, "<UP>"...)
	case entry.Kind == ramfs.KindDirectory:
		return append(target, "<DIR>"...)
	}
	return appendSize(target, entry.Size)
}

func appendSize(target []byte, size uint64) []byte {
	if size < 10_000_000 {
		return appendDecimal(target, size)
	}
	const units = "KMGTPE"
	unit, divisor := 0, uint64(1024)
	for size/divisor >= 1024 && unit < len(units)-1 {
		divisor *= 1024
		unit++
	}
	whole := size / divisor
	target = appendDecimal(target, whole)
	if whole < 10 {
		// One decimal while the whole part is one digit: 1.2G, but 755M.
		target = append(target, '.', byte('0'+size%divisor*10/divisor))
	}
	return append(target, units[unit])
}

func appendDecimal(target []byte, value uint64) []byte {
	var digits [20]byte
	position := len(digits)
	for {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	return append(target, digits[position:]...)
}

// characterLength is the byte length of the first character of text.
func characterLength(text []byte) int { return len(terminal.Cut(text, 1)) }

func fnv(data []byte) uint64 {
	hash := uint64(14695981039346656037)
	for _, value := range data {
		hash ^= uint64(value)
		hash *= 1099511628211
	}
	return hash
}
