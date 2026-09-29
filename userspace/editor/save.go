package editor

import (
	"unicode/utf8"

	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/safesave"
	"goos/userspace/shell"
	"goos/userspace/terminal"
)

// source hands the document to a save one buffer at a time, in order.
type source struct {
	buffer *Buffer
	offset int
}

func (source *source) Fill(target []byte) int {
	count := source.buffer.Copy(target, source.offset)
	source.offset += count
	return count
}

// save writes the document to path through safesave, which leaves the file
// as it was when any step fails. A failure is a message; the document keeps
// its modified state and the editor carries on.
func (app *App) save(path []byte) bool {
	app.saving = source{buffer: &app.buffer}
	status, stage := app.files.Save(path, &app.saving)
	if status != vfs.StatusOK {
		app.say(true, "Save failed: ", nil, saveReason(status, stage))
		return false
	}
	app.modified = false
	app.Saves++
	target := appendText(app.message[:0], "Saved ")
	target = appendUint(target, uint64(app.buffer.Len()))
	target = appendText(target, " bytes")
	app.messageN, app.messageError = len(target), false
	return true
}

// saveReason words a failed save. Replace refuses a file another session
// holds open with StatusBusy, which the shell's wording reserves for mounts.
func saveReason(status vfs.Status, stage safesave.Stage) string {
	if status == vfs.StatusBusy && stage == safesave.StageReplace {
		return "the file is open elsewhere"
	}
	return shell.VFSReason(status, true)
}

func (app *App) handleDialog(event terminal.Event) {
	switch app.dialog {
	case dialogQuit:
		switch {
		case yes(event):
			app.dialog = dialogNone
			if app.save(app.path[:app.pathN]) {
				app.quit = true
			}
		case isRune(event, 'n', 'N'):
			app.dialog = dialogNone
			app.quit = true
		case event.Key == terminal.KeyEscape:
			app.cancel()
		}
	case dialogOverwrite:
		switch {
		case yes(event):
			app.dialog = dialogNone
			app.saveAs()
		case isRune(event, 'n', 'N') || event.Key == terminal.KeyEscape:
			app.cancel()
		}
	case dialogSaveAs:
		app.handleInput(event)
	}
}

func yes(event terminal.Event) bool {
	return event.Key == terminal.KeyEnter || isRune(event, 'y', 'Y')
}

func isRune(event terminal.Event, lower, upper rune) bool {
	return event.Key == terminal.KeyRune && !event.Ctrl && !event.Alt && (event.Rune == lower || event.Rune == upper)
}

func (app *App) cancel() {
	app.dialog = dialogNone
	app.say(false, "Cancelled", nil, "")
}

// handleInput edits the Save As name: characters are added at the end,
// Backspace removes the last one, Enter confirms and Escape cancels.
func (app *App) handleInput(event terminal.Event) {
	switch {
	case event.Key == terminal.KeyEscape:
		app.cancel()
	case event.Key == terminal.KeyEnter:
		app.dialog = dialogNone
		app.submitSaveAs()
	case event.Key == terminal.KeyBackspace:
		if app.inputN != 0 {
			_, size := utf8.DecodeLastRune(app.input[:app.inputN])
			app.inputN -= size
		}
	case event.Key == terminal.KeyRune && !event.Ctrl && !event.Alt && event.Rune >= 0x20 && event.Rune != 0x7f && utf8.ValidRune(event.Rune):
		if app.inputN+utf8.RuneLen(event.Rune) <= len(app.input) {
			app.inputN += utf8.EncodeRune(app.input[app.inputN:], event.Rune)
		}
	}
}

// submitSaveAs resolves the typed name against the working directory, as
// commands resolve their arguments. The current file saves in place; an
// existing other file is replaced only after the user confirms.
func (app *App) submitSaveAs() {
	text := app.input[:app.inputN]
	if len(text) == 0 {
		app.say(true, "Save As needs a file name", nil, "")
		return
	}
	target, ok := shell.ResolvePath(app.cwd[:app.cwdN], text, &app.target)
	if !ok {
		reason := "invalid path"
		if shell.TooLong(text) {
			reason = "file name too long"
		}
		app.say(true, "Save failed: ", nil, reason)
		return
	}
	app.targetN = len(target)
	if sameBytes(target, app.path[:app.pathN]) {
		app.save(app.path[:app.pathN])
		return
	}
	kind, _, status := app.files.Stat(target)
	switch {
	case status == vfs.StatusNotFound:
		app.saveAs()
	case status != vfs.StatusOK:
		app.say(true, "Save failed: ", nil, shell.VFSReason(status, false))
	case kind == ramfs.KindDirectory:
		app.say(true, "Save failed: ", nil, "is a directory")
	default:
		app.dialog = dialogOverwrite
	}
}

// saveAs saves to the resolved target and, once that succeeds, makes it the
// document's path.
func (app *App) saveAs() {
	target := app.target[:app.targetN]
	if app.save(target) {
		app.pathN = copy(app.path[:], target)
	}
}

func sameBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
