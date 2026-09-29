package editor

import (
	"strings"
	"testing"

	"goos/internal/limits"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/safesave"
	"goos/userspace/terminal"
)

// memoryFiles is a flat map of paths to contents behind the real safesave
// sequence. A failure is injected per operation; limit makes writes stop
// short once a file would grow past it, as a full volume does.
type memoryFiles struct {
	files      map[string][]byte
	dirs       map[string]bool
	open       map[vfs.Handle]string
	next       vfs.Handle
	createFail vfs.Status
	writeFail  vfs.Status
	replace    vfs.Status
	limit      int
	largest    int
	saves      int
}

func newFiles() *memoryFiles {
	return &memoryFiles{
		files: map[string][]byte{"/home/doc.txt": []byte("original")},
		dirs:  map[string]bool{"/": true, "/home": true, "/home/sub": true},
		open:  map[vfs.Handle]string{},
	}
}

func (files *memoryFiles) Stat(path []byte) (ramfs.Kind, uint64, vfs.Status) {
	if files.dirs[string(path)] {
		return ramfs.KindDirectory, 0, vfs.StatusOK
	}
	if data, found := files.files[string(path)]; found {
		return ramfs.KindFile, uint64(len(data)), vfs.StatusOK
	}
	return 0, 0, vfs.StatusNotFound
}

// Save runs the real safesave sequence through a small buffer, so a save
// takes several Fill calls.
func (files *memoryFiles) Save(path []byte, source safesave.Source) (vfs.Status, safesave.Stage) {
	files.saves++
	var buffer [7]byte
	var temp [limits.PathMax]byte
	return safesave.Save(files, path, buffer[:], source, &temp)
}

func (files *memoryFiles) Create(path []byte) vfs.Status {
	if files.createFail != vfs.StatusOK {
		return files.createFail
	}
	if _, found := files.files[string(path)]; found {
		return vfs.StatusAlreadyExists
	}
	if !files.dirs[parent(string(path))] {
		return vfs.StatusNotFound
	}
	files.files[string(path)] = nil
	return vfs.StatusOK
}

func (files *memoryFiles) Unlink(path []byte) vfs.Status {
	if _, found := files.files[string(path)]; !found {
		return vfs.StatusNotFound
	}
	delete(files.files, string(path))
	return vfs.StatusOK
}

func (files *memoryFiles) OpenWrite(path []byte) (vfs.Handle, vfs.Status) {
	if _, found := files.files[string(path)]; !found {
		return 0, vfs.StatusNotFound
	}
	files.next++
	files.open[files.next] = string(path)
	return files.next, vfs.StatusOK
}

func (files *memoryFiles) Write(file vfs.Handle, data []byte) (int, vfs.Status) {
	if files.writeFail != vfs.StatusOK {
		return 0, files.writeFail
	}
	files.largest = max(files.largest, len(data))
	path := files.open[file]
	room := len(data)
	if files.limit != 0 && len(files.files[path])+room > files.limit {
		room = files.limit - len(files.files[path])
	}
	files.files[path] = append(files.files[path], data[:room]...)
	return room, vfs.StatusOK
}

func (files *memoryFiles) Close(file vfs.Handle) vfs.Status {
	delete(files.open, file)
	return vfs.StatusOK
}

func (files *memoryFiles) Replace(source, target []byte) vfs.Status {
	if files.replace != vfs.StatusOK {
		return files.replace
	}
	files.files[string(target)] = files.files[string(source)]
	delete(files.files, string(source))
	return vfs.StatusOK
}

// temporaries lists leftover save files.
func (files *memoryFiles) temporaries() []string {
	var names []string
	for name := range files.files {
		if strings.HasSuffix(name, safesave.Suffix) {
			names = append(names, name)
		}
	}
	return names
}

func parent(path string) string {
	slash := strings.LastIndexByte(path, '/')
	if slash <= 0 {
		return "/"
	}
	return path[:slash]
}

// memoryReader serves data in reads no larger than it is asked for and
// records the largest request.
type memoryReader struct {
	data    []byte
	offset  int
	largest int
	fail    vfs.Status
}

func (reader *memoryReader) Read(target []byte) (int, vfs.Status) {
	if reader.fail != vfs.StatusOK {
		return 0, reader.fail
	}
	reader.largest = max(reader.largest, len(target))
	count := copy(target, reader.data[reader.offset:])
	reader.offset += count
	return count, vfs.StatusOK
}

// open returns an editor at 80x24 holding text as /home/doc.txt.
func open(t *testing.T, files *memoryFiles, text string) *App {
	t.Helper()
	app := new(App)
	if !app.Init(files, []byte("/home/doc.txt"), []byte("/home"), 80, 24, true) {
		t.Fatal("init")
	}
	if !app.Load(&memoryReader{data: []byte(text)}, uint64(len(text))) {
		message, _ := app.Message()
		t.Fatalf("load refused: %s", message)
	}
	return app
}

func (app *App) keys(events ...terminal.Event) {
	for _, event := range events {
		app.Handle(event)
	}
}

func key(value terminal.Key) terminal.Event { return terminal.Event{Key: value} }

func typed(text string) []terminal.Event {
	var events []terminal.Event
	for _, value := range text {
		events = append(events, terminal.Event{Key: terminal.KeyRune, Rune: value})
	}
	return events
}

func document(app *App) string {
	target := make([]byte, app.Len())
	return string(target[:app.Text(target)])
}

func message(app *App) string {
	text, _ := app.Message()
	return string(text)
}

// row reads back one row of the frame the last Render built.
func row(app *App, index int) string {
	var builder strings.Builder
	for column := 0; column < app.columns; column++ {
		cell, _ := app.screen.Cell(column, index)
		builder.Write(cell.Bytes[:cell.Size])
	}
	return builder.String()
}

func render(app *App) []byte {
	var storage [1 << 16]byte
	var written []byte
	output := terminal.NewOutput(storage[:], func(value []byte) { written = append(written, value...) })
	app.Render(&output)
	return written
}
