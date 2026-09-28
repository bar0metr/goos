package filemanager

import (
	"goos/internal/limits"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/shell"
	"goos/userspace/terminal"
)

const (
	// MinColumns and MinRows are the smallest terminal the manager lays
	// itself out on.
	MinColumns = 80
	MinRows    = 24
	// maxColumns and maxRows bound the screen it tracks; a larger terminal
	// is used up to this size.
	maxColumns = 256
	maxRows    = 128
	// chromeRows are the rows that are not list rows: each panel's top
	// border, header, separator, information line and bottom border, then
	// the status line and the key bar.
	chromeRows = 7
)

// App is the whole manager: two panels, the one with the keyboard, what the
// status line says, and the dialog in front, if any.
type App struct {
	filesystem FileSystem
	panels     [2]Panel
	active     int
	columns    int
	rows       int
	tooSmall   bool
	quit       bool
	// The screen is the framebuffer's size on the framebuffer console, and on
	// COM1 the size the terminal at the far end reports (ADR 0125).
	display        bool
	displayColumns int
	displayRows    int
	hostColumns    int
	hostRows       int
	loaded         bool
	sizeWanted     bool
	status         [256]byte
	statusSize     int
	statusBad      bool
	dialog         dialog
	screen         screen
	// source and target hold the paths of an operation waiting for
	// confirmation; scratch any path being worked out; keep a name to select
	// again after a listing changes.
	source     [shell.MaxPath]byte
	sourceSize int
	target     [shell.MaxPath]byte
	targetSize int
	scratch    [shell.MaxPath]byte
	directory  [shell.MaxPath]byte
	changed    [shell.MaxPath]byte
	keep       [limits.NameMax]byte
	// Counters for the validation build's markers.
	Keys         int
	FunctionKeys int
	Directories  int
	Copies       int
}

// Init opens both panels on cwd. columns and rows are the framebuffer's
// size when display is set; otherwise the screen is 80x24 until the terminal
// reports its size, and follows it from then on. A screen smaller than
// MinColumns x MinRows gets a message instead of the panels until it grows.
func (app *App) Init(filesystem FileSystem, cwd []byte, columns, rows int, display bool) {
	app.filesystem = filesystem
	app.display, app.displayColumns, app.displayRows = display, columns, rows
	for index := range app.panels {
		if !app.panels[index].SetPath(cwd) {
			app.panels[index].SetPath([]byte("/"))
		}
	}
	app.resize()
	app.sizeWanted = !display
}

// FollowsTerminal reports whether the screen is the size a terminal reports,
// which the program has to keep asking for, rather than a display's.
func (app *App) FollowsTerminal() bool { return !app.display }

// geometry returns the screen the manager lays itself out on.
func (app *App) geometry() (int, int) {
	switch {
	case app.display:
		return app.displayColumns, app.displayRows
	case app.hostColumns != 0:
		return app.hostColumns, app.hostRows
	}
	return MinColumns, MinRows
}

// resize lays the manager out again when the screen changed size: the whole
// screen is cleared and drawn, and each selection kept in view.
func (app *App) resize() {
	columns, rows := app.geometry()
	app.tooSmall = columns < MinColumns || rows < MinRows
	columns, rows = min(columns, maxColumns), min(rows, maxRows)
	if columns == app.columns && rows == app.rows {
		return
	}
	app.columns, app.rows = columns, rows
	app.screen.invalidate()
	if app.tooSmall {
		return
	}
	if !app.loaded {
		app.loaded = true
		for index := range app.panels {
			app.load(&app.panels[index], nil)
		}
		return
	}
	for index := range app.panels {
		panel := &app.panels[index]
		panel.Select(panel.selected, app.listRows())
	}
}

// SizeWanted reports, once, that the terminal should be asked for its size
// with terminal.Output.RequestSize: at start and after F2.
func (app *App) SizeWanted() bool {
	wanted := app.sizeWanted
	app.sizeWanted = false
	return wanted
}

// Quit reports whether the user asked to leave.
func (app *App) Quit() bool { return app.quit }

// TooSmall reports whether the terminal is below the minimum size.
func (app *App) TooSmall() bool { return app.tooSmall }

// Active returns the panel with the keyboard, and Other the one without.
func (app *App) Active() *Panel { return &app.panels[app.active] }
func (app *App) Other() *Panel  { return &app.panels[1-app.active] }

// Status returns what the status line says and whether it is an error.
func (app *App) Status() ([]byte, bool) { return app.status[:app.statusSize], app.statusBad }

// listRows is how many entries each panel shows.
func (app *App) listRows() int { return app.rows - chromeRows }

// Handle applies one key.
func (app *App) Handle(event terminal.Event) {
	if event.Key == terminal.KeyPosition {
		// The terminal answered RequestSize: its size is where the cursor
		// was clamped. A display's size is fixed and needs no answer.
		if !app.display {
			app.hostColumns, app.hostRows = event.Column, event.Row
			app.resize()
		}
		return
	}
	app.Keys++
	if event.Key >= terminal.KeyF1 && event.Key <= terminal.KeyF12 {
		app.FunctionKeys++
	}
	if app.tooSmall {
		if event.Key == terminal.KeyF10 || event.Key == terminal.KeyEscape ||
			event.Key == terminal.KeyRune && !event.Ctrl && (event.Rune == 'q' || event.Rune == 'Q') {
			app.quit = true
		}
		return
	}
	if app.dialog.kind != dialogNone {
		app.handleDialog(event)
		return
	}
	panel, visible := app.Active(), app.listRows()
	switch event.Key {
	case terminal.KeyUp:
		panel.Move(-1, visible)
	case terminal.KeyDown:
		panel.Move(1, visible)
	case terminal.KeyPageUp:
		panel.Move(-visible, visible)
	case terminal.KeyPageDown:
		panel.Move(visible, visible)
	case terminal.KeyHome:
		panel.Select(0, visible)
	case terminal.KeyEnd:
		panel.Select(panel.Count()-1, visible)
	case terminal.KeyTab:
		app.active = 1 - app.active
	case terminal.KeyEnter:
		app.open()
		return
	case terminal.KeyBackspace:
		app.parent()
		return
	case terminal.KeyEscape:
	case terminal.KeyF1:
		app.dialog = dialog{kind: dialogHelp}
	case terminal.KeyF2:
		app.reread()
		return
	case terminal.KeyF3:
		app.fail("Viewer is not available in GoOS 0.1.6", nil, "")
		return
	case terminal.KeyF4:
		app.fail("Editor is not available in GoOS 0.1.6", nil, "")
		return
	case terminal.KeyF5:
		app.copy()
		return
	case terminal.KeyF6:
		app.startMove()
		return
	case terminal.KeyF7:
		app.dialog = dialog{kind: dialogInput, action: actionMkdir, limit: limits.NameMax}
		return
	case terminal.KeyF8:
		app.startDelete()
		return
	case terminal.KeyF10:
		app.quit = true
		return
	default:
		return
	}
	// A navigation key clears a message about the last operation.
	app.statusSize = 0
}

// open enters the selected directory.
func (app *App) open() {
	panel := app.Active()
	entry, name, ok := panel.Current()
	if !ok {
		return
	}
	if IsParent(name) {
		app.parent()
		return
	}
	if entry.Kind != ramfs.KindDirectory {
		app.fail("Viewer is not available in GoOS 0.1.6", nil, "")
		return
	}
	path, ok := shell.JoinName(panel.Path(), name, &app.scratch)
	if !ok {
		app.fail("Path too long: ", name, "")
		return
	}
	app.statusSize = 0
	app.enter(panel, path, nil)
}

// parent goes up one directory and selects the one it came from.
func (app *App) parent() {
	panel := app.Active()
	current := panel.Path()
	if len(current) <= 1 {
		return
	}
	keep := app.keep[:copy(app.keep[:], shell.BaseName(current))]
	app.statusSize = 0
	app.enter(panel, shell.Parent(current, &app.scratch), keep)
}

// enter shows path in panel. A directory that cannot be listed leaves the
// panel where it was.
func (app *App) enter(panel *Panel, path []byte, keep []byte) {
	previousPath := app.directory[:copy(app.directory[:], panel.Path())]
	panel.SetPath(path)
	panel.selected, panel.top = 0, 0
	if status := panel.Load(app.filesystem, keep, app.listRows()); status != vfs.StatusOK {
		app.fail("Cannot open ", path, ": "+shell.VFSReason(status, false))
		panel.SetPath(previousPath)
		app.load(panel, nil)
	}
}

// load lists a panel's directory, climbing towards the root while it cannot
// be listed: a directory removed behind the manager's back is left, not shown
// empty.
func (app *App) load(panel *Panel, keep []byte) {
	for {
		status := panel.Load(app.filesystem, keep, app.listRows())
		if status == vfs.StatusOK || panel.pathSize <= 1 {
			if status != vfs.StatusOK {
				app.fail("Cannot read /: ", nil, shell.VFSReason(status, false))
			}
			return
		}
		panel.SetPath(shell.Parent(panel.Path(), &app.scratch))
		keep = nil
	}
}

// reload lists a panel again, keeping the selected entry selected.
func (app *App) reload(panel *Panel) {
	var keep []byte
	if _, name, ok := panel.Current(); ok {
		keep = app.keep[:copy(app.keep[:], name)]
	}
	app.load(panel, keep)
}

// reread lists both panels again, repaints the whole screen, which also
// repairs it after output from elsewhere landed on it, and asks a terminal
// for its size again.
func (app *App) reread() {
	app.reload(&app.panels[0])
	app.reload(&app.panels[1])
	app.screen.invalidate()
	app.statusSize = 0
	app.sizeWanted = !app.display
}

// afterChange lists again every panel showing directory. It works on its own
// copy, since listing may reuse the buffer the caller's path is in.
func (app *App) afterChange(path []byte) {
	directory := app.changed[:copy(app.changed[:], path)]
	for index := range app.panels {
		if equalBytes(app.panels[index].Path(), directory) {
			app.reload(&app.panels[index])
		}
	}
}

// selection returns the path of the selected entry of the active panel, in
// app.source, or false when there is nothing an operation can act on.
func (app *App) selection() (Entry, []byte, []byte, bool) {
	entry, name, ok := app.Active().Current()
	if !ok || IsParent(name) {
		return Entry{}, nil, nil, false
	}
	path, ok := shell.JoinName(app.Active().Path(), name, &app.source)
	if !ok {
		app.fail("Path too long: ", name, "")
		return Entry{}, nil, nil, false
	}
	app.sourceSize = len(path)
	return entry, name, path, true
}

// copy copies the selected file into the other panel's directory, asking
// first when that would replace a file.
func (app *App) copy() {
	entry, name, source, ok := app.selection()
	if !ok {
		return
	}
	if entry.Kind == ramfs.KindDirectory {
		app.fail("Directory copy is not supported yet", nil, "")
		return
	}
	target, ok := shell.JoinName(app.Other().Path(), name, &app.target)
	if !ok {
		app.fail("Path too long: ", name, "")
		return
	}
	app.targetSize = len(target)
	if equalBytes(source, target) {
		app.fail("Cannot copy a file onto itself", nil, "")
		return
	}
	kind, _, status := app.filesystem.Stat(target)
	switch {
	case status == vfs.StatusOK && kind == ramfs.KindDirectory:
		app.fail("Copy failed: ", name, " is a directory in the target")
	case status == vfs.StatusOK:
		app.confirm(actionOverwrite, "Overwrite", "Overwrite ", name)
	case status == vfs.StatusNotFound:
		app.finishCopy(false)
	default:
		app.fail("Copy failed: ", nil, shell.VFSReason(status, false))
	}
}

func (app *App) finishCopy(overwrite bool) {
	source, target := app.source[:app.sourceSize], app.target[:app.targetSize]
	status, targetFailed := app.filesystem.CopyFile(source, target, overwrite)
	directory := shell.Parent(target, &app.scratch)
	if status != vfs.StatusOK {
		app.fail("Copy failed: ", nil, shell.VFSReason(status, targetFailed))
		// A copy that failed part way may have left a partial target.
		app.afterChange(directory)
		return
	}
	app.Copies++
	app.afterChange(directory)
	app.say("Copied ", shell.BaseName(target), "")
}

// startMove asks where to move the selected entry, offering the other
// panel's directory. Editing the name there renames it.
func (app *App) startMove() {
	_, name, _, ok := app.selection()
	if !ok {
		return
	}
	app.dialog = dialog{kind: dialogInput, action: actionMove, limit: shell.MaxPath}
	app.dialog.setSubject(name)
	if target, ok := shell.JoinName(app.Other().Path(), name, &app.scratch); ok {
		app.dialog.inputSize = copy(app.dialog.input[:], target)
	}
}

func (app *App) finishMove(text []byte) {
	source := app.source[:app.sourceSize]
	target, ok := shell.ResolvePath(app.Active().Path(), text, &app.target)
	if !ok || len(target) <= 1 {
		app.fail("Invalid target path", nil, "")
		return
	}
	// Moving onto an existing directory moves into it.
	if kind, _, status := app.filesystem.Stat(target); status == vfs.StatusOK && kind == ramfs.KindDirectory {
		inside, ok := shell.JoinName(target, shell.BaseName(source), &app.scratch)
		if !ok {
			app.fail("Path too long", nil, "")
			return
		}
		target = app.target[:copy(app.target[:], inside)]
	}
	app.targetSize = len(target)
	if equalBytes(source, target) {
		return
	}
	from := shell.Parent(source, &app.directory)
	switch status := app.filesystem.Rename(source, target); status {
	case vfs.StatusOK:
		app.afterChange(from)
		app.afterChange(shell.Parent(target, &app.scratch))
		app.say("Moved to ", target, "")
	case vfs.StatusUnsupported:
		app.fail("Cross-filesystem move is not supported", nil, "")
	default:
		app.fail("Move failed: ", nil, shell.VFSReason(status, true))
	}
}

func (app *App) finishMkdir(name []byte) {
	if !shell.ValidName(name) {
		app.fail("Invalid directory name", nil, "")
		return
	}
	panel := app.Active()
	path, ok := shell.JoinName(panel.Path(), name, &app.target)
	if !ok {
		app.fail("Path too long", nil, "")
		return
	}
	if status := app.filesystem.Mkdir(path); status != vfs.StatusOK {
		app.fail("Mkdir failed: ", nil, shell.VFSReason(status, true))
		return
	}
	app.Directories++
	keep := app.keep[:copy(app.keep[:], name)]
	app.load(panel, keep)
	if other := app.Other(); equalBytes(other.Path(), panel.Path()) {
		app.reload(other)
	}
	app.say("Created ", name, "")
}

// startDelete asks before removing the selected entry.
func (app *App) startDelete() {
	_, name, _, ok := app.selection()
	if !ok {
		return
	}
	app.confirm(actionDelete, "Delete", "Delete ", name)
}

func (app *App) finishDelete() {
	source := app.source[:app.sourceSize]
	status := app.filesystem.Unlink(source)
	if status != vfs.StatusOK {
		app.fail("Delete failed: ", nil, shell.VFSReason(status, true))
		return
	}
	app.afterChange(shell.Parent(source, &app.directory))
	app.say("Deleted ", shell.BaseName(source), "")
}

// say and fail put a message on the status line, an error in fail's case:
// prefix, then name in quotes when there is one, then suffix.
func (app *App) say(prefix string, name []byte, suffix string) {
	app.message(false, prefix, name, suffix)
}

func (app *App) fail(prefix string, name []byte, suffix string) {
	app.message(true, prefix, name, suffix)
}

func (app *App) message(bad bool, prefix string, name []byte, suffix string) {
	size := copy(app.status[:], prefix)
	if name != nil {
		size += copy(app.status[size:], "\"")
		size += copy(app.status[size:], name)
		size += copy(app.status[size:], "\"")
	}
	size += copy(app.status[size:], suffix)
	app.statusSize, app.statusBad = size, bad
}
