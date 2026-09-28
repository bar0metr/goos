package filemanager

import (
	"unicode/utf8"

	"goos/internal/limits"
	"goos/userspace/shell"
	"goos/userspace/terminal"
)

type dialogKind uint8

const (
	dialogNone dialogKind = iota
	dialogHelp
	dialogConfirm
	dialogInput
)

type action uint8

const (
	actionNone action = iota
	actionOverwrite
	actionDelete
	actionMkdir
	actionMove
)

// dialog is the modal box in front of the panels: help, a yes/no question,
// or a one-line text field. Only one is open at a time, and it takes every
// key until it closes.
type dialog struct {
	kind   dialogKind
	action action
	// title and question describe a confirmation; subject is the name it is
	// about.
	title       string
	question    string
	subject     [limits.NameMax]byte
	subjectSize int
	// input is the text field; limit bounds it in bytes.
	input     [shell.MaxPath]byte
	inputSize int
	limit     int
}

func (dialog *dialog) setSubject(name []byte) {
	dialog.subjectSize = copy(dialog.subject[:], name)
}

// confirm asks a yes/no question about name before action.
func (app *App) confirm(action action, title, question string, name []byte) {
	app.dialog = dialog{kind: dialogConfirm, action: action, title: title, question: question}
	app.dialog.setSubject(name)
}

func (app *App) handleDialog(event terminal.Event) {
	switch app.dialog.kind {
	case dialogHelp:
		app.dialog.kind = dialogNone
	case dialogConfirm:
		app.handleConfirm(event)
	case dialogInput:
		app.handleInput(event)
	}
}

// handleConfirm takes Enter or y as yes, and Escape or n as no.
func (app *App) handleConfirm(event terminal.Event) {
	yes := event.Key == terminal.KeyEnter || event.Key == terminal.KeyRune && !event.Ctrl && (event.Rune == 'y' || event.Rune == 'Y')
	no := event.Key == terminal.KeyEscape || event.Key == terminal.KeyF10 ||
		event.Key == terminal.KeyRune && !event.Ctrl && (event.Rune == 'n' || event.Rune == 'N')
	if !yes && !no {
		return
	}
	action := app.dialog.action
	app.dialog.kind = dialogNone
	if no {
		app.say("Cancelled", nil, "")
		return
	}
	switch action {
	case actionOverwrite:
		app.finishCopy(true)
	case actionDelete:
		app.finishDelete()
	}
}

// handleInput edits the text field: characters are added at the end,
// Backspace removes the last one, Enter confirms and Escape cancels.
func (app *App) handleInput(event terminal.Event) {
	field := &app.dialog
	switch {
	case event.Key == terminal.KeyEscape || event.Key == terminal.KeyF10:
		field.kind = dialogNone
		app.say("Cancelled", nil, "")
	case event.Key == terminal.KeyEnter:
		field.kind = dialogNone
		text := field.input[:field.inputSize]
		switch field.action {
		case actionMkdir:
			app.finishMkdir(text)
		case actionMove:
			app.finishMove(text)
		}
	case event.Key == terminal.KeyBackspace:
		if field.inputSize != 0 {
			_, size := utf8.DecodeLastRune(field.input[:field.inputSize])
			field.inputSize -= size
		}
	case event.Key == terminal.KeyRune && !event.Ctrl && !event.Alt && event.Rune >= 0x20 && event.Rune != 0x7f:
		if field.inputSize+utf8.RuneLen(event.Rune) <= field.limit {
			field.inputSize += utf8.EncodeRune(field.input[field.inputSize:], event.Rune)
		}
	}
}
