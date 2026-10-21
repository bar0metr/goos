package safesave

import (
	"bytes"
	"goos/internal/limits"
	"goos/kernel/vfs"
	"testing"
)

// modelFiles is a flat map of paths to contents. fail makes the named
// operation answer status on its nth call; limit makes writes stop short
// once a file would grow past it, as a full volume does.
type modelFiles struct {
	files   map[string][]byte
	open    map[vfs.Handle]string
	next    vfs.Handle
	calls   map[string]int
	failOp  string
	failAt  int
	failAs  vfs.Status
	limit   int
	removed []string
}

func newModel() *modelFiles {
	return &modelFiles{files: map[string][]byte{"/home/doc.txt": []byte("original")}, open: map[vfs.Handle]string{}, calls: map[string]int{}}
}

func (model *modelFiles) fails(op string) bool {
	model.calls[op]++
	return model.failOp == op && model.calls[op] == model.failAt
}

func (model *modelFiles) Create(path []byte) vfs.Status {
	if model.fails("create") {
		return model.failAs
	}
	if _, found := model.files[string(path)]; found {
		return vfs.StatusAlreadyExists
	}
	model.files[string(path)] = nil
	return vfs.StatusOK
}

func (model *modelFiles) Unlink(path []byte) vfs.Status {
	model.removed = append(model.removed, string(path))
	if model.fails("unlink") {
		return model.failAs
	}
	if _, found := model.files[string(path)]; !found {
		return vfs.StatusNotFound
	}
	delete(model.files, string(path))
	return vfs.StatusOK
}

func (model *modelFiles) OpenWrite(path []byte) (vfs.Handle, vfs.Status) {
	if model.fails("open") {
		return 0, model.failAs
	}
	if _, found := model.files[string(path)]; !found {
		return 0, vfs.StatusNotFound
	}
	model.next++
	model.open[model.next] = string(path)
	return model.next, vfs.StatusOK
}

func (model *modelFiles) Write(file vfs.Handle, data []byte) (int, vfs.Status) {
	if model.fails("write") {
		return 0, model.failAs
	}
	path := model.open[file]
	room := len(data)
	if model.limit != 0 && len(model.files[path])+room > model.limit {
		room = model.limit - len(model.files[path])
	}
	model.files[path] = append(model.files[path], data[:room]...)
	return room, vfs.StatusOK
}

func (model *modelFiles) Close(file vfs.Handle) vfs.Status {
	delete(model.open, file)
	if model.fails("close") {
		return model.failAs
	}
	return vfs.StatusOK
}

func (model *modelFiles) Replace(source, target []byte) vfs.Status {
	if model.fails("replace") {
		return model.failAs
	}
	model.files[string(target)] = model.files[string(source)]
	delete(model.files, string(source))
	return vfs.StatusOK
}

// chunks produces its text a few bytes at a time.
type chunks struct {
	text []byte
	step int
}

func (source *chunks) Fill(buffer []byte) int {
	count := min(source.step, len(buffer), len(source.text))
	copy(buffer, source.text[:count])
	source.text = source.text[count:]
	return count
}

func TestSaveReplacesTheTargetWithCompleteContents(t *testing.T) {
	model := newModel()
	var temp [limits.PathMax]byte
	status, _ := Save(model, []byte("/home/doc.txt"), make([]byte, 4), &chunks{text: []byte("edited text"), step: 3}, &temp)
	if status != vfs.StatusOK {
		t.Fatalf("save = %d", status)
	}
	if got := string(model.files["/home/doc.txt"]); got != "edited text" {
		t.Fatalf("target holds %q", got)
	}
	if _, found := model.files["/home/.doc.txt.~save"]; found || len(model.open) != 0 {
		t.Fatal("temporary file or handle left behind")
	}
}

func TestSaveCreatesANewFile(t *testing.T) {
	model := newModel()
	var temp [limits.PathMax]byte
	if status, _ := Save(model, []byte("/home/new.txt"), make([]byte, 8), &chunks{text: []byte("fresh"), step: 8}, &temp); status != vfs.StatusOK {
		t.Fatalf("save = %d", status)
	}
	if got := string(model.files["/home/new.txt"]); got != "fresh" {
		t.Fatalf("new file holds %q", got)
	}
}

func TestSavePreservesAStaleTemporaryFile(t *testing.T) {
	model := newModel()
	model.files["/home/.doc.txt.~save"] = []byte("left by a crash")
	var temp [limits.PathMax]byte
	source := &chunks{text: []byte("second"), step: 8}
	for attempt := 0; attempt < 2; attempt++ {
		status, stage := Save(model, []byte("/home/doc.txt"), make([]byte, 8), source, &temp)
		if status != vfs.StatusAlreadyExists || stage != StageTemporary {
			t.Fatalf("save = %d at %d", status, stage)
		}
	}
	if got := string(model.files["/home/doc.txt"]); got != "original" {
		t.Fatalf("target holds %q", got)
	}
	if got := string(model.files["/home/.doc.txt.~save"]); got != "left by a crash" {
		t.Fatalf("temporary file holds %q", got)
	}
	if len(model.calls) != 1 || model.calls["create"] != 2 || len(model.open) != 0 || len(model.removed) != 0 || string(source.text) != "second" {
		t.Fatalf("refused save touched existing state: calls=%v removed=%v source=%q", model.calls, model.removed, source.text)
	}
}

// interleavedFiles pauses the owner between Create and OpenWrite, and checks
// the completed temporary file just before its atomic replacement.
type interleavedFiles struct {
	Files
	beforeOpen    func()
	beforeReplace func()
}

func (files interleavedFiles) OpenWrite(path []byte) (vfs.Handle, vfs.Status) {
	files.beforeOpen()
	return files.Files.OpenWrite(path)
}

func (files interleavedFiles) Replace(source, target []byte) vfs.Status {
	files.beforeReplace()
	return files.Files.Replace(source, target)
}

func TestConcurrentSaveRefusesTheOwnersTemporaryFile(t *testing.T) {
	model := newModel()
	// Separate buffers, paths, and sources model independent callers/processes;
	// only the filesystem is shared. No scheduler timing is needed.
	var ownerTemp, loserTemp [limits.PathMax]byte
	ownerBuffer, loserBuffer := make([]byte, 4), make([]byte, 8)
	ownerSource := &chunks{text: []byte("complete owner text"), step: 3}
	loserSource := &chunks{text: []byte("loser text"), step: 8}
	files := interleavedFiles{
		Files: model,
		beforeOpen: func() {
			if _, found := model.files["/home/.doc.txt.~save"]; !found {
				t.Fatal("owner has not created its temporary file")
			}
			status, stage := Save(model, []byte("/home/doc.txt"), loserBuffer, loserSource, &loserTemp)
			if status != vfs.StatusAlreadyExists || stage != StageTemporary {
				t.Fatalf("competing save = %d at %d", status, stage)
			}
			if len(model.calls) != 1 || model.calls["create"] != 2 || len(model.open) != 0 || len(model.removed) != 0 {
				t.Fatalf("loser did more than attempt creation: calls=%v removed=%v", model.calls, model.removed)
			}
			if contents, found := model.files["/home/.doc.txt.~save"]; !found || len(contents) != 0 {
				t.Fatalf("owner's temporary file changed: exists=%t contents=%q", found, contents)
			}
			if string(model.files["/home/doc.txt"]) != "original" || string(loserSource.text) != "loser text" {
				t.Fatal("loser changed the target or consumed its source")
			}
		},
		beforeReplace: func() {
			if got := string(model.files["/home/doc.txt"]); got != "original" {
				t.Fatalf("target changed before replacement: %q", got)
			}
			if got := string(model.files["/home/.doc.txt.~save"]); got != "complete owner text" {
				t.Fatalf("replacement is incomplete: %q", got)
			}
			if len(model.open) != 0 {
				t.Fatal("replacement attempted before close")
			}
		},
	}
	if status, stage := Save(files, []byte("/home/doc.txt"), ownerBuffer, ownerSource, &ownerTemp); status != vfs.StatusOK || stage != 0 {
		t.Fatalf("owner save = %d at %d", status, stage)
	}
	if got := string(model.files["/home/doc.txt"]); got != "complete owner text" {
		t.Fatalf("target holds %q", got)
	}
	if model.calls["open"] != 1 || model.calls["replace"] != 1 || len(model.removed) != 0 {
		t.Fatalf("unexpected save operations: calls=%v removed=%v", model.calls, model.removed)
	}
	if _, found := model.files["/home/.doc.txt.~save"]; found || len(model.open) != 0 {
		t.Fatal("temporary file or handle left behind")
	}
}

// TestFailureAtEveryStepKeepsTheOriginal injects each failure a save can
// meet — a read-only or full volume, a filesystem error, a VFS that died, a
// refused replacement — and checks the original is untouched and the
// temporary file is gone.
func TestFailureAtEveryStepKeepsTheOriginal(t *testing.T) {
	cases := []struct {
		op     string
		at     int
		status vfs.Status
		stage  Stage
	}{
		{"create", 1, vfs.StatusPermissionDenied, StageTemporary},
		{"create", 1, vfs.StatusNoSpace, StageTemporary},
		{"open", 1, vfs.StatusPeerDied, StageTemporary},
		{"write", 1, vfs.StatusBackendError, StageWrite},
		{"write", 3, vfs.StatusNoSpace, StageWrite},
		{"write", 2, vfs.StatusPeerDied, StageWrite},
		{"close", 1, vfs.StatusBackendError, StageWrite},
		{"replace", 1, vfs.StatusBusy, StageReplace},
		{"replace", 1, vfs.StatusPermissionDenied, StageReplace},
		{"replace", 1, vfs.StatusPeerDied, StageReplace},
	}
	for _, test := range cases {
		model := newModel()
		model.failOp, model.failAt, model.failAs = test.op, test.at, test.status
		var temp [limits.PathMax]byte
		status, stage := Save(model, []byte("/home/doc.txt"), make([]byte, 4), &chunks{text: []byte("a long replacement text"), step: 4}, &temp)
		if status != test.status || stage != test.stage {
			t.Fatalf("%s#%d: save = %d at %d, want %d at %d", test.op, test.at, status, stage, test.status, test.stage)
		}
		if got := string(model.files["/home/doc.txt"]); got != "original" {
			t.Fatalf("%s#%d: original became %q", test.op, test.at, got)
		}
		if _, found := model.files["/home/.doc.txt.~save"]; found {
			t.Fatalf("%s#%d: temporary file left behind", test.op, test.at)
		}
	}
}

func TestShortWriteIsNoSpace(t *testing.T) {
	model := newModel()
	model.limit = 5
	var temp [limits.PathMax]byte
	status, stage := Save(model, []byte("/home/doc.txt"), make([]byte, 4), &chunks{text: []byte("does not fit"), step: 4}, &temp)
	if status != vfs.StatusNoSpace || stage != StageWrite {
		t.Fatalf("save = %d at %d", status, stage)
	}
	if got := string(model.files["/home/doc.txt"]); got != "original" {
		t.Fatalf("original became %q", got)
	}
}

func TestFailedCleanupLeavesTheTemporaryFileFailClosed(t *testing.T) {
	model := newModel()
	model.limit = 5
	model.failOp, model.failAt, model.failAs = "unlink", 1, vfs.StatusBackendError
	var temp [limits.PathMax]byte
	status, stage := Save(model, []byte("/home/doc.txt"), make([]byte, 4), &chunks{text: []byte("does not fit"), step: 4}, &temp)
	if status != vfs.StatusNoSpace || stage != StageWrite {
		t.Fatalf("owner save = %d at %d", status, stage)
	}
	if len(model.removed) != 1 || len(model.open) != 0 {
		t.Fatal("owner did not attempt cleanup and close its handle")
	}
	source := &chunks{text: []byte("retry"), step: 4}
	status, stage = Save(model, []byte("/home/doc.txt"), make([]byte, 4), source, &temp)
	if status != vfs.StatusAlreadyExists || stage != StageTemporary {
		t.Fatalf("retry save = %d at %d", status, stage)
	}
	if got := string(model.files["/home/doc.txt"]); got != "original" {
		t.Fatalf("target holds %q", got)
	}
	if got := string(model.files["/home/.doc.txt.~save"]); got != "does " {
		t.Fatalf("temporary file holds %q", got)
	}
	if len(model.removed) != 1 || model.calls["open"] != 1 || model.calls["replace"] != 0 || string(source.text) != "retry" {
		t.Fatalf("retry touched the leftover file: calls=%v removed=%v source=%q", model.calls, model.removed, source.text)
	}
}

func TestTemporaryPathStaysInTheTargetDirectory(t *testing.T) {
	var out [limits.PathMax]byte
	if path, status := TemporaryPath([]byte("/home/notes/todo"), &out); status != vfs.StatusOK || string(path) != "/home/notes/.todo.~save" {
		t.Fatalf("temporary path %q (%d)", path, status)
	}
	if path, status := TemporaryPath([]byte("/top"), &out); status != vfs.StatusOK || string(path) != "/.top.~save" {
		t.Fatalf("temporary path %q (%d)", path, status)
	}
	for _, bad := range []string{"", "/", "relative", "/dir/", "/dir/..", "/dir/.", "/dir/a\x00b"} {
		if _, status := TemporaryPath([]byte(bad), &out); status != vfs.StatusInvalidArgument {
			t.Fatalf("%q accepted (%d)", bad, status)
		}
	}
	long := append([]byte("/"), bytes.Repeat([]byte{'n'}, limits.NameMax)...)
	if _, status := TemporaryPath(long, &out); status != vfs.StatusNameTooLong {
		t.Fatalf("NameMax target = %d", status)
	}
}
