package editor

import (
	"bytes"
	"strings"
	"testing"

	"goos/kernel/vfs"
	"goos/userspace/terminal"
	"goos/userspace/tui"
)

func TestBufferMovesOnlyTheGap(t *testing.T) {
	var storage [16]byte
	var buffer Buffer
	if !buffer.Reset(storage[:], 0) || buffer.Len() != 0 || buffer.Free() != 16 {
		t.Fatal("reset")
	}
	buffer.Insert(0, []byte("helloworld"))
	buffer.Insert(5, []byte(", "))
	buffer.Delete(0, 1)
	buffer.Insert(0, []byte("H"))
	var out [32]byte
	if got := string(out[:buffer.Copy(out[:], 0)]); got != "Hello, world" {
		t.Fatalf("text=%q", got)
	}
	if got := string(out[:buffer.Copy(out[:3], 4)]); got != "o, " {
		t.Fatalf("copy across the gap=%q", got)
	}
	if buffer.At(7) != 'w' || buffer.Len() != 12 {
		t.Fatalf("at=%q len=%d", buffer.At(7), buffer.Len())
	}
	if buffer.Insert(3, []byte("12345")) || buffer.Len() != 12 {
		t.Fatal("insert past capacity accepted")
	}
	if buffer.Delete(10, 3) || buffer.Delete(-1, 1) || buffer.Insert(13, []byte("x")) {
		t.Fatal("out-of-range edit accepted")
	}
	if !buffer.Insert(12, []byte("!!!!")) || buffer.Free() != 0 || buffer.Insert(0, []byte("x")) {
		t.Fatal("filling the buffer")
	}
}

func TestInsertDeleteAndLineJoins(t *testing.T) {
	app := open(t, newFiles(), "ab\ncd")
	app.keys(key(terminal.KeyRight))
	app.keys(typed("XY")...)
	if document(app) != "aXYb\ncd" || !app.Modified() {
		t.Fatalf("text=%q modified=%v", document(app), app.Modified())
	}
	app.keys(key(terminal.KeyEnter))
	if line, column := app.Cursor(); document(app) != "aXY\nb\ncd" || line != 1 || column != 0 {
		t.Fatalf("split text=%q at %d:%d", document(app), line, column)
	}
	app.keys(key(terminal.KeyBackspace))
	if line, column := app.Cursor(); document(app) != "aXYb\ncd" || line != 0 || column != 3 {
		t.Fatalf("join text=%q at %d:%d", document(app), line, column)
	}
	app.keys(key(terminal.KeyEnd), key(terminal.KeyDelete))
	if line, column := app.Cursor(); document(app) != "aXYbcd" || line != 0 || column != 4 {
		t.Fatalf("delete join text=%q at %d:%d", document(app), line, column)
	}
	app.keys(key(terminal.KeyHome), key(terminal.KeyBackspace), key(terminal.KeyEnd), key(terminal.KeyDelete))
	if document(app) != "aXYbcd" {
		t.Fatalf("edits at the document edges changed %q", document(app))
	}
	app.keys(key(terminal.KeyTab))
	if document(app) != "aXYbcd\t" {
		t.Fatalf("tab text=%q", document(app))
	}
}

func TestMultiByteCharactersAreEditedWhole(t *testing.T) {
	app := open(t, newFiles(), "a€b")
	app.keys(key(terminal.KeyRight), key(terminal.KeyRight))
	if _, column := app.Cursor(); column != 2 || app.cursor != 4 {
		t.Fatalf("column=%d offset=%d", column, app.cursor)
	}
	app.keys(typed("é😀")...)
	if document(app) != "a€é😀b" {
		t.Fatalf("text=%q", document(app))
	}
	if _, column := app.Cursor(); column != 4 {
		t.Fatalf("column=%d", column)
	}
	app.keys(key(terminal.KeyBackspace), key(terminal.KeyLeft), key(terminal.KeyDelete))
	if document(app) != "a€b" {
		t.Fatalf("text=%q", document(app))
	}
	app.keys(key(terminal.KeyBackspace))
	if document(app) != "ab" || app.cursor != 1 {
		t.Fatalf("text=%q offset=%d", document(app), app.cursor)
	}
}

func TestInvalidUTF8StepsOneByteAndIsKept(t *testing.T) {
	text := "x\xff\xe2\x82y\n"
	app := open(t, newFiles(), text)
	for range 4 {
		app.keys(key(terminal.KeyRight))
	}
	if _, column := app.Cursor(); column != 4 || app.cursor != 4 {
		t.Fatalf("column=%d offset=%d", column, app.cursor)
	}
	app.keys(key(terminal.KeyLeft), key(terminal.KeyLeft))
	if app.cursor != 2 {
		t.Fatalf("offset=%d", app.cursor)
	}
	if document(app) != text {
		t.Fatal("moving changed the text")
	}
}

func TestVerticalMovesRememberTheColumn(t *testing.T) {
	app := open(t, newFiles(), "long line one\nab\nanother long line\n")
	for range 9 {
		app.keys(key(terminal.KeyRight))
	}
	app.keys(key(terminal.KeyDown))
	if line, column := app.Cursor(); line != 1 || column != 2 {
		t.Fatalf("short line at %d:%d", line, column)
	}
	app.keys(key(terminal.KeyDown))
	if line, column := app.Cursor(); line != 2 || column != 9 {
		t.Fatalf("goal column lost at %d:%d", line, column)
	}
	app.keys(key(terminal.KeyDown))
	if line, column := app.Cursor(); line != 3 || column != 0 {
		t.Fatalf("final empty line at %d:%d", line, column)
	}
	app.keys(key(terminal.KeyDown), key(terminal.KeyUp), key(terminal.KeyUp), key(terminal.KeyUp), key(terminal.KeyUp))
	if line, column := app.Cursor(); line != 0 || column != 9 {
		t.Fatalf("back at the top at %d:%d", line, column)
	}
	app.keys(key(terminal.KeyLeft), key(terminal.KeyDown), key(terminal.KeyDown))
	if line, column := app.Cursor(); line != 2 || column != 8 {
		t.Fatalf("horizontal move did not reset the goal: %d:%d", line, column)
	}
}

func numberedLines(count, width int) string {
	var builder strings.Builder
	for index := 0; index < count; index++ {
		builder.WriteString(strings.Repeat(string(rune('a'+index%26)), width))
		builder.WriteByte('\n')
	}
	return builder.String()
}

func TestScrollingKeepsTheCursorVisible(t *testing.T) {
	app := open(t, newFiles(), numberedLines(100, 10))
	for range 30 {
		app.keys(key(terminal.KeyDown))
	}
	if app.topLine != 30-app.bodyRows()+1 || app.line != 30 {
		t.Fatalf("top=%d line=%d", app.topLine, app.line)
	}
	if app.topOffset != app.topLine*11 {
		t.Fatalf("top offset=%d for line %d", app.topOffset, app.topLine)
	}
	app.keys(key(terminal.KeyPageDown))
	if app.line != 52 || app.line-app.topLine != 21 {
		t.Fatalf("page down line=%d top=%d", app.line, app.topLine)
	}
	app.keys(key(terminal.KeyPageUp), key(terminal.KeyPageUp), key(terminal.KeyPageUp))
	if app.line != 0 || app.topLine != 0 || app.topOffset != 0 {
		t.Fatalf("page up line=%d top=%d", app.line, app.topLine)
	}
	for range 10 {
		app.keys(key(terminal.KeyPageDown))
	}
	if app.line != 100 || app.line < app.topLine || app.line >= app.topLine+app.bodyRows() {
		t.Fatalf("end line=%d top=%d", app.line, app.topLine)
	}
	if app.topOffset != app.lineStart(app.topOffset) {
		t.Fatal("top is not a line start")
	}

	wide := open(t, newFiles(), strings.Repeat("x", 200)+"\nshort\n")
	wide.keys(key(terminal.KeyEnd))
	if _, column := wide.Cursor(); column != 200 || wide.leftColumn != 200-80+1 {
		t.Fatalf("column=%d left=%d", column, wide.leftColumn)
	}
	wide.keys(key(terminal.KeyDown))
	if wide.leftColumn != 5 {
		t.Fatalf("left=%d", wide.leftColumn)
	}
	render(wide)
	if got := row(wide, 0); !strings.HasPrefix(got, strings.Repeat("x", 80)) {
		t.Fatalf("row=%q", got)
	}
}

func TestBackspaceAtTheTopLineScrollsUp(t *testing.T) {
	app := open(t, newFiles(), numberedLines(40, 3))
	for range 25 {
		app.keys(key(terminal.KeyDown))
	}
	app.keys(key(terminal.KeyPageUp))
	app.keys(key(terminal.KeyPageDown))
	for app.line != app.topLine {
		app.keys(key(terminal.KeyUp))
	}
	top := app.topLine
	app.keys(key(terminal.KeyHome), key(terminal.KeyBackspace))
	if app.line != top-1 || app.topLine != top-1 || app.topOffset != app.lineStart(app.cursor) {
		t.Fatalf("line=%d top=%d offset=%d", app.line, app.topLine, app.topOffset)
	}
}

func TestLoadRefusesLargeBinaryAndFailedFiles(t *testing.T) {
	app := new(App)
	app.Init(newFiles(), []byte("/big"), []byte("/"), 80, 24, true)
	if app.Load(&memoryReader{}, MaxSize+1) || !app.Refused() || !strings.Contains(message(app), "gview") {
		t.Fatalf("large file message=%q", message(app))
	}
	app.Handle(key(terminal.KeyEnter))
	if !app.Quit() {
		t.Fatal("refusal did not end on Enter")
	}

	// A file that grows past the limit after its size was read.
	grown := &memoryReader{data: bytes.Repeat([]byte("a"), MaxSize+10)}
	app.Init(newFiles(), []byte("/big"), []byte("/"), 80, 24, true)
	if app.Load(grown, 100) || !strings.Contains(message(app), "too large") {
		t.Fatalf("grown file message=%q", message(app))
	}
	if grown.largest > LoadChunk {
		t.Fatalf("read of %d bytes", grown.largest)
	}

	app.Init(newFiles(), []byte("/bin"), []byte("/"), 80, 24, true)
	if app.Load(&memoryReader{data: []byte("text\x00more")}, 9) || !strings.Contains(message(app), "binary") {
		t.Fatalf("binary message=%q", message(app))
	}

	app.Init(newFiles(), []byte("/dead"), []byte("/"), 80, 24, true)
	if app.Load(&memoryReader{fail: vfs.StatusPeerDied}, 9) || message(app) != "storage service unavailable" {
		t.Fatalf("failed read message=%q", message(app))
	}
	render(app)
	if !strings.HasPrefix(row(app, 0), "edit: /dead ") || !strings.HasPrefix(row(app, 1), "storage service unavailable") {
		t.Fatalf("refusal rows %q / %q", row(app, 0), row(app, 1))
	}
}

func TestFullSizeDocumentLoadsButRefusesInsertion(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcde\n"), MaxSize/16)
	reader := &memoryReader{data: data}
	app := new(App)
	app.Init(newFiles(), []byte("/full"), []byte("/"), 80, 24, true)
	if !app.Load(reader, uint64(len(data))) || app.Len() != MaxSize {
		t.Fatalf("load len=%d", app.Len())
	}
	if reader.largest > LoadChunk {
		t.Fatalf("read of %d bytes", reader.largest)
	}
	app.keys(typed("x")...)
	if app.Modified() || app.Len() != MaxSize || !strings.Contains(message(app), "1 MiB") {
		t.Fatalf("insert into full document: len=%d message=%q", app.Len(), message(app))
	}
	app.keys(key(terminal.KeyDelete))
	app.keys(typed("x")...)
	if app.Len() != MaxSize || document(app)[0] != 'x' {
		t.Fatal("insert after delete")
	}
}

func TestControlBytesNeverReachTheTerminal(t *testing.T) {
	text := "a\x1b[6nb\x07\x7f\tc\r\n\xc2\x9b31m\xff\n"
	files := newFiles()
	app := open(t, files, text)
	visible := app.visibleText(80, 22)
	if string(visible) != "a?[6nb?? c?\n?31m\xff\n" {
		t.Fatalf("visible=%q", visible)
	}
	output := render(app)
	if bytes.Contains(output, []byte("\x1b[6n")) || !bytes.Contains(output, []byte("?[6n")) {
		t.Fatal("file bytes reached the terminal as a control sequence")
	}
	if bytes.Contains(output, []byte("\xc2\x9b")) || bytes.Contains(output, []byte{0x07}) {
		t.Fatal("C1 or BEL reached the terminal")
	}
	app.keys(key(terminal.KeyEnd))
	app.keys(typed("!")...)
	app.keys(key(terminal.KeyF2))
	if got := string(files.files["/home/doc.txt"]); got != "a\x1b[6nb\x07\x7f\tc\r!\n\xc2\x9b31m\xff\n" {
		t.Fatalf("saved=%q", got)
	}
}

func TestResizeFollowsTheTerminalReport(t *testing.T) {
	app := new(App)
	app.Init(newFiles(), []byte("/new"), []byte("/"), 0, 0, false)
	if !app.SizeWanted() || app.SizeWanted() || app.columns != DefaultColumns || !app.FollowsTerminal() {
		t.Fatal("serial start")
	}
	app.Handle(terminal.Event{Key: terminal.KeyPosition, Row: 50, Column: 132})
	if app.columns != 132 || app.rows != 50 || app.bodyRows() != 48 {
		t.Fatalf("geometry %dx%d", app.columns, app.rows)
	}
	app.Handle(terminal.Event{Key: terminal.KeyPosition, Row: 999, Column: 999})
	if app.columns != MaxColumns || app.rows != MaxRows {
		t.Fatalf("clamped geometry %dx%d", app.columns, app.rows)
	}
}

func TestLayoutAt80x24(t *testing.T) {
	app := open(t, newFiles(), "first line\nsecond\n")
	app.keys(key(terminal.KeyDown), key(terminal.KeyEnd))
	app.keys(typed("!")...)
	render(app)
	if got := row(app, 0); got != "first line"+strings.Repeat(" ", 70) {
		t.Fatalf("row 0=%q", got)
	}
	if got := row(app, 1); !strings.HasPrefix(got, "second! ") {
		t.Fatalf("row 1=%q", got)
	}
	cursor, _ := app.screen.Cell(7, 1)
	if cursor.Role != tui.RoleCursor {
		t.Fatalf("cursor role=%d", cursor.Role)
	}
	status := row(app, 22)
	if !strings.HasPrefix(status, "/home/doc.txt [modified]") || !strings.HasSuffix(status, " 2:8 ") || len(status) != 80 {
		t.Fatalf("status=%q", status)
	}
	if got := row(app, 23); !strings.HasPrefix(got, "F2 Save  F3 Save As  F10 Quit") {
		t.Fatalf("message row=%q", got)
	}
	for index := 2; index < 22; index++ {
		if strings.TrimSpace(row(app, index)) != "" {
			t.Fatalf("row %d=%q", index, row(app, index))
		}
	}

	// A long path gives way to the position.
	long := new(App)
	path := "/" + strings.Repeat("d", 150) + "/name.txt"
	long.Init(newFiles(), []byte(path), []byte("/"), 80, 24, true)
	long.NewFile()
	render(long)
	status = row(long, 22)
	if !strings.HasPrefix(status, "...") || !strings.Contains(status, "/name.txt") || !strings.HasSuffix(status, " 1:1 ") {
		t.Fatalf("long status=%q", status)
	}
	if got := row(long, 23); !strings.HasPrefix(got, "New file") {
		t.Fatalf("new file message=%q", got)
	}
}

func TestSteadyFramesAllocateNothing(t *testing.T) {
	app := open(t, newFiles(), numberedLines(60, 30))
	var storage [1 << 16]byte
	written := 0
	output := terminal.NewOutput(storage[:], func(value []byte) { written += len(value) })
	app.Render(&output)
	allocations := testing.AllocsPerRun(100, func() {
		app.Handle(key(terminal.KeyDown))
		app.Handle(key(terminal.KeyRight))
		app.Handle(terminal.Event{Key: terminal.KeyRune, Rune: 'é'})
		app.Handle(key(terminal.KeyBackspace))
		app.Handle(key(terminal.KeyEnter))
		app.Handle(key(terminal.KeyBackspace))
		app.Render(&output)
		app.Handle(key(terminal.KeyUp))
		app.Handle(key(terminal.KeyPageDown))
		app.Handle(key(terminal.KeyPageUp))
		app.Render(&output)
	})
	if allocations != 0 || written == 0 {
		t.Fatalf("allocations=%v written=%d", allocations, written)
	}
	app.Handle(key(terminal.KeyF10))
	allocations = testing.AllocsPerRun(100, func() { app.Render(&output) })
	if allocations != 0 {
		t.Fatalf("dialog frame allocations=%v", allocations)
	}
}
