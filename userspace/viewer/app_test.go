package viewer

import (
	"bytes"
	"goos/kernel/vfs"
	"goos/userspace/terminal"
	"testing"
)

type memoryReader struct {
	data       []byte
	maximum    int
	reads      int
	failOffset uint64
}

func (reader *memoryReader) ReadAt(offset uint64, target []byte) (int, vfs.Status) {
	reader.reads++
	reader.maximum = max(reader.maximum, len(target))
	if reader.failOffset != 0 && offset >= reader.failOffset {
		return 0, vfs.StatusPeerDied
	}
	if offset >= uint64(len(reader.data)) {
		return 0, vfs.StatusOK
	}
	if reader.failOffset != 0 && offset+uint64(len(target)) > reader.failOffset {
		target = target[:reader.failOffset-offset]
	}
	return copy(target, reader.data[offset:]), vfs.StatusOK
}

func TestNavigationAndStatus(t *testing.T) {
	reader := &memoryReader{data: []byte("zero\none\nthree\nfour\n")}
	var app App
	if status := app.Init(reader, []byte("/docs/file.txt"), uint64(len(reader.data)), 80, 4, true); status != vfs.StatusOK {
		t.Fatal(status)
	}
	app.Handle(terminal.Event{Key: terminal.KeyDown})
	if app.TopOffset() != 5 {
		t.Fatalf("down offset=%d", app.TopOffset())
	}
	app.Handle(terminal.Event{Key: terminal.KeyHome})
	app.Handle(terminal.Event{Key: terminal.KeyEnd})
	if app.TopOffset() != 9 {
		t.Fatalf("end offset=%d", app.TopOffset())
	}
	text, status := app.visibleText(20, 3)
	if status != vfs.StatusOK || string(text) != "three\nfour\n" {
		t.Fatalf("text=%q status=%d", text, status)
	}
	if got := string(app.statusText()); got != "/docs/file.txt  size 20  offset 9  45%" {
		t.Fatalf("status=%q", got)
	}
	app.Handle(terminal.Event{Key: terminal.KeyEscape})
	if !app.Quit() {
		t.Fatal("escape did not quit")
	}
}

func TestLongPathLeavesStatusFieldsVisible(t *testing.T) {
	reader := &memoryReader{data: []byte("text")}
	path := make([]byte, 300)
	for index := range path {
		path[index] = 'p'
	}
	var app App
	if status := app.Init(reader, path, uint64(len(reader.data)), 80, 24, true); status != vfs.StatusOK {
		t.Fatal(status)
	}
	status := string(app.statusText())
	if len(status) > 80 || !bytes.HasSuffix([]byte(status), []byte("size 4  offset 0  0%")) {
		t.Fatalf("status=%q", status)
	}
}

func TestLongLineHorizontalClipUsesBoundedReads(t *testing.T) {
	data := make([]byte, 6006)
	for index := range data[:6000] {
		data[index] = 'a'
	}
	copy(data[6000:], "\ntail\n")
	reader := &memoryReader{data: data}
	var app App
	if status := app.Init(reader, []byte("/long"), uint64(len(data)), 10, 4, true); status != vfs.StatusOK {
		t.Fatal(status)
	}
	app.leftColumn = 5998
	text, status := app.visibleText(4, 2)
	if status != vfs.StatusOK || string(text) != "aa\n" {
		t.Fatalf("text=%q status=%d", text, status)
	}
	if reader.maximum > readWindowSize {
		t.Fatalf("read=%d", reader.maximum)
	}
}

func TestControlBytesAreSanitizedAndInvalidUTF8IsBounded(t *testing.T) {
	reader := &memoryReader{data: []byte{'a', 0x1b, 'b', '\t', 'c', '\n', 0xff}}
	var app App
	if status := app.Init(reader, []byte("/text"), uint64(len(reader.data)), 20, 4, true); status != vfs.StatusOK {
		t.Fatal(status)
	}
	text, status := app.visibleText(20, 2)
	if status != vfs.StatusOK || len(text) != 7 || text[0] != 'a' || text[1] != '?' || text[3] != ' ' || text[6] != 0xff {
		t.Fatalf("text=%v status=%d", text, status)
	}
	for _, value := range text {
		if value == 0x1b {
			t.Fatal("escape reached render data")
		}
	}
}

func TestBinaryAndReadFailure(t *testing.T) {
	for _, data := range [][]byte{{'a', 0, 'b'}, {1, 2, 3, 4, 5, 6, 7, 8, 'x'}} {
		reader := &memoryReader{data: data}
		var app App
		if status := app.Init(reader, []byte("/binary"), uint64(len(data)), 80, 24, true); status != vfs.StatusOK || !app.Binary() {
			t.Fatalf("binary=%v status=%d data=%v", app.Binary(), status, data)
		}
	}
	reader := &memoryReader{data: []byte("first\nsecond")}
	var app App
	if status := app.Init(reader, []byte("/failure"), uint64(len(reader.data)), 80, 24, true); status != vfs.StatusOK {
		t.Fatal(status)
	}
	reader.failOffset = 6
	app.windowValid = false
	app.moveDown(2)
	if app.Failure() != vfs.StatusPeerDied {
		t.Fatalf("failure=%d", app.Failure())
	}
}

type syntheticReader struct {
	size    uint64
	maximum int
}

func (reader *syntheticReader) ReadAt(offset uint64, target []byte) (int, vfs.Status) {
	reader.maximum = max(reader.maximum, len(target))
	if offset >= reader.size {
		return 0, vfs.StatusOK
	}
	count := len(target)
	if remaining := reader.size - offset; remaining < uint64(count) {
		count = int(remaining)
	}
	for index := 0; index < count; index++ {
		position := offset + uint64(index)
		if position%80 == 79 {
			target[index] = '\n'
		} else {
			target[index] = 'x'
		}
	}
	return count, vfs.StatusOK
}

func TestHugeFileKeepsFixedWindowAndFindsEnd(t *testing.T) {
	reader := &syntheticReader{size: 1 << 30}
	var app App
	if status := app.Init(reader, []byte("/huge"), reader.size, 80, 24, true); status != vfs.StatusOK {
		t.Fatal(status)
	}
	app.Handle(terminal.Event{Key: terminal.KeyEnd})
	if app.TopOffset() == 0 || app.TopOffset() >= reader.size || reader.maximum > readWindowSize {
		t.Fatalf("offset=%d maximum=%d", app.TopOffset(), reader.maximum)
	}
}

func TestChangedFramesAllocateNothing(t *testing.T) {
	reader := &memoryReader{data: []byte("first\nsecond\nthird\nfourth\n")}
	var app App
	if status := app.Init(reader, []byte("/steady"), uint64(len(reader.data)), 80, 24, true); status != vfs.StatusOK {
		t.Fatal(status)
	}
	var storage [8192]byte
	written := 0
	output := terminal.NewOutput(storage[:], func(value []byte) { written += len(value) })
	app.Render(&output)
	allocations := testing.AllocsPerRun(100, func() {
		app.Handle(terminal.Event{Key: terminal.KeyDown})
		app.Handle(terminal.Event{Key: terminal.KeyUp})
		app.Render(&output)
	})
	if allocations != 0 || written == 0 {
		t.Fatalf("allocations=%v written=%d", allocations, written)
	}
}
