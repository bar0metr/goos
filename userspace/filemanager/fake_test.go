package filemanager

import (
	"sort"
	"strings"
	"testing"

	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/terminal"
)

// memoryFS is a model filesystem: paths to files and directories. Paths
// under /ro refuse changes, and anything under /other is another filesystem.
type memoryFS struct {
	files       map[string][]byte
	directories map[string]bool
	calls       []string
	failCopy    vfs.Status
}

func newMemoryFS(paths ...string) *memoryFS {
	filesystem := &memoryFS{files: map[string][]byte{}, directories: map[string]bool{"/": true}}
	for _, path := range paths {
		if strings.HasSuffix(path, "/") {
			filesystem.directories[strings.TrimSuffix(path, "/")] = true
		} else {
			filesystem.files[path] = []byte("contents of " + path)
		}
	}
	return filesystem
}

func parentOf(path string) string {
	index := strings.LastIndex(path, "/")
	if index <= 0 {
		return "/"
	}
	return path[:index]
}

func (filesystem *memoryFS) ReadDir(path []byte, visitor Visitor) vfs.Status {
	directory := string(path)
	if !filesystem.directories[directory] {
		return vfs.StatusNotFound
	}
	var names []string
	for child := range filesystem.directories {
		if child != "/" && parentOf(child) == directory {
			names = append(names, child)
		}
	}
	for child := range filesystem.files {
		if parentOf(child) == directory {
			names = append(names, child)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, child := range names {
		name := child[strings.LastIndex(child, "/")+1:]
		kind, size := ramfs.KindFile, uint64(len(filesystem.files[child]))
		if filesystem.directories[child] {
			kind, size = ramfs.KindDirectory, 0
		}
		if !visitor.Visit([]byte(name), kind, size) {
			break
		}
	}
	return vfs.StatusOK
}

func (filesystem *memoryFS) Stat(path []byte) (ramfs.Kind, uint64, vfs.Status) {
	if filesystem.directories[string(path)] {
		return ramfs.KindDirectory, 0, vfs.StatusOK
	}
	if data, ok := filesystem.files[string(path)]; ok {
		return ramfs.KindFile, uint64(len(data)), vfs.StatusOK
	}
	return 0, 0, vfs.StatusNotFound
}

func (filesystem *memoryFS) writable(path string) vfs.Status {
	if strings.HasPrefix(path, "/ro") {
		return vfs.StatusPermissionDenied
	}
	if !filesystem.directories[parentOf(path)] {
		return vfs.StatusNotFound
	}
	return vfs.StatusOK
}

func (filesystem *memoryFS) exists(path string) bool {
	_, file := filesystem.files[path]
	return file || filesystem.directories[path]
}

func (filesystem *memoryFS) Mkdir(path []byte) vfs.Status {
	filesystem.calls = append(filesystem.calls, "mkdir "+string(path))
	if status := filesystem.writable(string(path)); status != vfs.StatusOK {
		return status
	}
	if filesystem.exists(string(path)) {
		return vfs.StatusAlreadyExists
	}
	filesystem.directories[string(path)] = true
	return vfs.StatusOK
}

func (filesystem *memoryFS) Rename(source, target []byte) vfs.Status {
	filesystem.calls = append(filesystem.calls, "rename "+string(source)+" "+string(target))
	from, to := string(source), string(target)
	if strings.HasPrefix(from, "/other") != strings.HasPrefix(to, "/other") {
		return vfs.StatusUnsupported
	}
	if status := filesystem.writable(to); status != vfs.StatusOK {
		return status
	}
	if filesystem.exists(to) {
		return vfs.StatusAlreadyExists
	}
	data, ok := filesystem.files[from]
	if !ok {
		return vfs.StatusNotFound
	}
	delete(filesystem.files, from)
	filesystem.files[to] = data
	return vfs.StatusOK
}

func (filesystem *memoryFS) Unlink(path []byte) vfs.Status {
	filesystem.calls = append(filesystem.calls, "unlink "+string(path))
	name := string(path)
	if status := filesystem.writable(name); status != vfs.StatusOK {
		return status
	}
	if filesystem.directories[name] {
		for child := range filesystem.files {
			if parentOf(child) == name {
				return vfs.StatusNotEmpty
			}
		}
		delete(filesystem.directories, name)
		return vfs.StatusOK
	}
	if _, ok := filesystem.files[name]; !ok {
		return vfs.StatusNotFound
	}
	delete(filesystem.files, name)
	return vfs.StatusOK
}

func (filesystem *memoryFS) CopyFile(source, target []byte, overwrite bool) (vfs.Status, bool) {
	filesystem.calls = append(filesystem.calls, "copy "+string(source)+" "+string(target))
	data, ok := filesystem.files[string(source)]
	if !ok {
		return vfs.StatusNotFound, false
	}
	if status := filesystem.writable(string(target)); status != vfs.StatusOK {
		return status, true
	}
	if filesystem.failCopy != vfs.StatusOK {
		return filesystem.failCopy, true
	}
	if filesystem.exists(string(target)) && !overwrite {
		return vfs.StatusAlreadyExists, true
	}
	filesystem.files[string(target)] = append([]byte(nil), data...)
	return vfs.StatusOK, false
}

// cells is a model terminal: it applies CUP, erase and text, and fails the
// test on any write that would wrap past the last column or scroll.
type cells struct {
	t             *testing.T
	columns, rows int
	grid          [][]rune
	row, column   int
}

func newCells(t *testing.T, columns, rows int) *cells {
	screen := &cells{t: t, columns: columns, rows: rows}
	screen.clear()
	return screen
}

func (screen *cells) clear() {
	screen.grid = make([][]rune, screen.rows)
	for row := range screen.grid {
		screen.grid[row] = []rune(strings.Repeat(" ", screen.columns))
	}
}

func (screen *cells) write(data []byte) {
	text := []rune(string(data))
	for index := 0; index < len(text); index++ {
		if text[index] != 0x1b {
			if screen.column >= screen.columns {
				screen.t.Fatalf("text past the last column at row %d: %q", screen.row, string(text[index:]))
			}
			if screen.row == screen.rows-1 && screen.column == screen.columns-1 {
				screen.t.Fatalf("wrote the bottom-right corner, which scrolls")
			}
			screen.grid[screen.row][screen.column] = text[index]
			screen.column++
			continue
		}
		end := index + 2
		for end < len(text) && (text[end] < '@' || text[end] > '~') {
			end++
		}
		sequence := string(text[index+2 : end])
		switch text[end] {
		case 'H':
			var row, column int
			if sequence != "" {
				parts := strings.Split(sequence, ";")
				row, column = atoi(parts[0])-1, atoi(parts[1])-1
			}
			if row < 0 || row >= screen.rows || column < 0 || column >= screen.columns {
				screen.t.Fatalf("cursor moved off the screen: %q", sequence)
			}
			screen.row, screen.column = row, column
		case 'J':
			screen.clear()
		}
		index = end
	}
}

func atoi(text string) int {
	value := 0
	for _, digit := range text {
		value = value*10 + int(digit-'0')
	}
	return value
}

func (screen *cells) line(row int) string { return strings.TrimRight(string(screen.grid[row]), " ") }

func (screen *cells) text() string {
	var lines []string
	for row := range screen.grid {
		lines = append(lines, screen.line(row))
	}
	return strings.Join(lines, "\n")
}

// harness drives an App with keys and renders every frame onto cells.
type harness struct {
	t      *testing.T
	app    *App
	fs     *memoryFS
	screen *cells
	output terminal.Output
	frame  []byte
}

func newHarness(t *testing.T, filesystem *memoryFS, cwd string, columns, rows int) *harness {
	h := &harness{t: t, app: new(App), fs: filesystem, screen: newCells(t, columns, rows)}
	// A frame arrives in pieces that may split a sequence; the model reads it
	// whole.
	h.output = terminal.NewOutput(make([]byte, 1024), func(value []byte) { h.frame = append(h.frame, value...) })
	h.app.Init(filesystem, []byte(cwd), columns, rows)
	h.render()
	return h
}

func (h *harness) render() {
	h.app.Render(&h.output)
	h.screen.write(h.frame)
	h.frame = h.frame[:0]
}

func (h *harness) keys(keys ...terminal.Key) {
	for _, key := range keys {
		h.app.Handle(terminal.Event{Key: key})
		h.render()
	}
}

func (h *harness) typeText(text string) {
	for _, value := range text {
		h.app.Handle(terminal.Event{Key: terminal.KeyRune, Rune: value})
	}
	h.render()
}

func (h *harness) selected() string {
	_, name, _ := h.app.Active().Current()
	return string(name)
}

func (h *harness) status() string {
	text, _ := h.app.Status()
	return string(text)
}
