package editor

import (
	"strings"
	"testing"

	"goos/kernel/vfs"
	"goos/userspace/terminal"
	"goos/userspace/tui"
)

func TestSaveReplacesTheFileAndClearsModified(t *testing.T) {
	files := newFiles()
	app := open(t, files, "original")
	app.keys(key(terminal.KeyEnd))
	app.keys(typed(" text")...)
	app.keys(key(terminal.KeyF2))
	if app.Modified() || string(files.files["/home/doc.txt"]) != "original text" {
		t.Fatalf("modified=%v file=%q", app.Modified(), files.files["/home/doc.txt"])
	}
	if message(app) != "Saved 13 bytes" || app.Saves != 1 || len(files.temporaries()) != 0 {
		t.Fatalf("message=%q saves=%d temporaries=%v", message(app), app.Saves, files.temporaries())
	}
	app.keys(typed("!")...)
	if !app.Modified() || message(app) != "" {
		t.Fatalf("edit after save: modified=%v message=%q", app.Modified(), message(app))
	}
}

func TestSaveCreatesANewFile(t *testing.T) {
	files := newFiles()
	app := new(App)
	app.Init(files, []byte("/home/new.txt"), []byte("/home"), 80, 24, true)
	app.NewFile()
	app.keys(typed("fresh")...)
	app.keys(key(terminal.KeyEnter), key(terminal.KeyF2))
	if got, found := files.files["/home/new.txt"]; !found || string(got) != "fresh\n" || app.Modified() {
		t.Fatalf("file=%q found=%v modified=%v", got, found, app.Modified())
	}
}

func TestFailedSaveKeepsTheDocumentAndTheFile(t *testing.T) {
	cases := []struct {
		name   string
		inject func(*memoryFiles)
		reason string
	}{
		{"read-only", func(files *memoryFiles) { files.createFail = vfs.StatusPermissionDenied }, "read-only filesystem"},
		{"full", func(files *memoryFiles) { files.limit = 4 }, "no space left on device"},
		{"busy", func(files *memoryFiles) { files.replace = vfs.StatusBusy }, "the file is open elsewhere"},
		{"peer died", func(files *memoryFiles) { files.writeFail = vfs.StatusPeerDied }, "storage service unavailable"},
		{"unsupported", func(files *memoryFiles) { files.replace = vfs.StatusUnsupported }, "operation not supported"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			files := newFiles()
			app := open(t, files, "original")
			app.keys(typed("new ")...)
			test.inject(files)
			app.keys(key(terminal.KeyF2))
			text, isError := app.Message()
			if string(text) != "Save failed: "+test.reason || !isError {
				t.Fatalf("message=%q error=%v", text, isError)
			}
			if !app.Modified() || app.Quit() || document(app) != "new original" {
				t.Fatalf("modified=%v quit=%v text=%q", app.Modified(), app.Quit(), document(app))
			}
			if string(files.files["/home/doc.txt"]) != "original" || len(files.temporaries()) != 0 {
				t.Fatalf("file=%q temporaries=%v", files.files["/home/doc.txt"], files.temporaries())
			}
			render(app)
			if cell, _ := app.screen.Cell(0, 23); cell.Role != tui.RoleWarning || !strings.HasPrefix(row(app, 23), "Save failed: ") {
				t.Fatalf("message row=%q role=%d", row(app, 23), cell.Role)
			}
			// The editor carries on, and a later save succeeds.
			*files = *newFiles()
			app.files = files
			app.keys(key(terminal.KeyF2))
			if app.Modified() || string(files.files["/home/doc.txt"]) != "new original" {
				t.Fatalf("retry modified=%v file=%q", app.Modified(), files.files["/home/doc.txt"])
			}
		})
	}
}

func TestSaveAsSwitchesThePathOnlyOnSuccess(t *testing.T) {
	files := newFiles()
	app := open(t, files, "text")
	app.keys(key(terminal.KeyF3))
	if app.dialog != dialogSaveAs || string(app.input[:app.inputN]) != "/home/doc.txt" {
		t.Fatalf("dialog=%d input=%q", app.dialog, app.input[:app.inputN])
	}
	render(app)
	if !strings.Contains(row(app, 11), "/home/doc.txt_") {
		t.Fatalf("input row=%q", row(app, 11))
	}
	// Replace the prefilled name with a relative one.
	for range len("/home/doc.txt") {
		app.keys(key(terminal.KeyBackspace))
	}
	app.keys(typed("sub/copy.txt")...)
	files.createFail = vfs.StatusNoSpace
	app.keys(key(terminal.KeyEnter))
	if string(app.Path()) != "/home/doc.txt" || message(app) != "Save failed: no space left on device" {
		t.Fatalf("path=%q message=%q", app.Path(), message(app))
	}
	files.createFail = vfs.StatusOK
	app.keys(key(terminal.KeyF3))
	for range len("/home/doc.txt") {
		app.keys(key(terminal.KeyBackspace))
	}
	app.keys(typed("sub/copy.txt")...)
	app.keys(key(terminal.KeyEnter))
	if string(app.Path()) != "/home/sub/copy.txt" || string(files.files["/home/sub/copy.txt"]) != "text" {
		t.Fatalf("path=%q files=%v", app.Path(), files.files)
	}
	app.keys(typed("more ")...)
	app.keys(key(terminal.KeyF2))
	if string(files.files["/home/sub/copy.txt"]) != "more text" || string(files.files["/home/doc.txt"]) != "original" {
		t.Fatal("save after Save As went to the old path")
	}
}

func TestSaveAsAsksBeforeReplacingAnotherFile(t *testing.T) {
	files := newFiles()
	files.files["/home/other.txt"] = []byte("keep")
	app := open(t, files, "text")
	app.keys(typed("new ")...)
	saveAs := func(name string) {
		app.keys(key(terminal.KeyF3))
		app.inputN = copy(app.input[:], name)
		app.keys(key(terminal.KeyEnter))
	}
	saveAs("other.txt")
	if app.dialog != dialogOverwrite {
		t.Fatalf("dialog=%d", app.dialog)
	}
	render(app)
	if !strings.Contains(row(app, 10), "Replace other.txt?") {
		t.Fatalf("dialog row=%q", row(app, 10))
	}
	app.keys(key(terminal.KeyEscape))
	if app.dialog != dialogNone || string(files.files["/home/other.txt"]) != "keep" || message(app) != "Cancelled" {
		t.Fatalf("escape: dialog=%d file=%q", app.dialog, files.files["/home/other.txt"])
	}
	saveAs("other.txt")
	app.keys(typed("n")...)
	if string(files.files["/home/other.txt"]) != "keep" || document(app) != "new text" {
		t.Fatal("n replaced the file or reached the document")
	}
	saveAs("other.txt")
	app.keys(typed("y")...)
	if string(files.files["/home/other.txt"]) != "new text" || string(app.Path()) != "/home/other.txt" || app.Modified() {
		t.Fatalf("file=%q path=%q", files.files["/home/other.txt"], app.Path())
	}

	saveAs("/home/sub")
	if message(app) != "Save failed: is a directory" || string(app.Path()) != "/home/other.txt" {
		t.Fatalf("directory target: message=%q", message(app))
	}
	saves := files.saves
	saveAs("./other.txt")
	if app.dialog != dialogNone || files.saves != saves+1 || message(app) != "Saved 8 bytes" {
		t.Fatalf("same path: dialog=%d message=%q", app.dialog, message(app))
	}
	saveAs("")
	if message(app) != "Save As needs a file name" {
		t.Fatalf("empty name: %q", message(app))
	}
	app.keys(key(terminal.KeyF3), key(terminal.KeyEscape))
	if app.dialog != dialogNone || message(app) != "Cancelled" {
		t.Fatal("escape did not cancel Save As")
	}
}

func TestQuitAsksAboutUnsavedChanges(t *testing.T) {
	clean := open(t, newFiles(), "text")
	clean.keys(key(terminal.KeyF10))
	if !clean.Quit() {
		t.Fatal("unmodified document did not quit")
	}

	files := newFiles()
	app := open(t, files, "text")
	app.keys(typed("x")...)
	app.keys(key(terminal.KeyF10))
	if app.Quit() || app.dialog != dialogQuit {
		t.Fatal("quit without asking")
	}
	render(app)
	if !strings.Contains(row(app, 10), "Save changes to doc.txt?") || !strings.Contains(row(app, 12), "y Save  n Discard  Esc Cancel") {
		t.Fatalf("dialog rows %q / %q", row(app, 10), row(app, 12))
	}
	// Keys that are not answers do nothing, and the text stays as it was.
	app.keys(typed("q")...)
	app.keys(key(terminal.KeyDown), key(terminal.KeyF10))
	if app.dialog != dialogQuit || document(app) != "xtext" {
		t.Fatalf("dialog=%d text=%q", app.dialog, document(app))
	}
	app.keys(key(terminal.KeyEscape))
	if app.Quit() || app.dialog != dialogNone || !app.Modified() {
		t.Fatal("escape did not cancel")
	}

	// A failed save keeps the editor running.
	files.replace = vfs.StatusBusy
	app.keys(key(terminal.KeyF10), key(terminal.KeyEnter))
	if app.Quit() || !app.Modified() || message(app) != "Save failed: the file is open elsewhere" {
		t.Fatalf("quit=%v message=%q", app.Quit(), message(app))
	}
	files.replace = vfs.StatusOK
	app.keys(key(terminal.KeyF10))
	app.keys(typed("y")...)
	if !app.Quit() || string(files.files["/home/doc.txt"]) != "xtext" {
		t.Fatalf("save and quit: quit=%v file=%q", app.Quit(), files.files["/home/doc.txt"])
	}

	discard := open(t, files, "text")
	discard.keys(typed("x")...)
	discard.keys(key(terminal.KeyF10))
	discard.keys(typed("n")...)
	if !discard.Quit() || string(files.files["/home/doc.txt"]) != "xtext" {
		t.Fatal("discard saved or did not quit")
	}
}
