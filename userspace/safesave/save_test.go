package safesave

import (
	"bytes"
	"fmt"
	"goos/internal/limits"
	"goos/kernel/vfs"
	"sync/atomic"
	"testing"
)

// modelFiles is a flat map of paths to contents. fail makes the named
// operation answer status on its nth call; limit makes writes stop short
// once a file would grow past it, as a full volume does.
type modelFiles struct {
	files    map[string][]byte
	open     map[vfs.Handle]string
	next     vfs.Handle
	calls    map[string]int
	failOp   string
	failAt   int
	failAs   vfs.Status
	limit    int
	tried    []string
	created  []string
	opened   []string
	replaced []string
	removed  []string
}

func newModel() *modelFiles {
	return &modelFiles{files: map[string][]byte{"/home/doc.txt": []byte("original")}, open: map[vfs.Handle]string{}, calls: map[string]int{}}
}

func (model *modelFiles) fails(op string) bool {
	model.calls[op]++
	return model.failOp == op && model.calls[op] == model.failAt
}

func (model *modelFiles) Create(path []byte) vfs.Status {
	model.tried = append(model.tried, string(path))
	if model.fails("create") {
		return model.failAs
	}
	if _, found := model.files[string(path)]; found {
		return vfs.StatusAlreadyExists
	}
	model.files[string(path)] = nil
	model.created = append(model.created, string(path))
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
	model.opened = append(model.opened, string(path))
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
	model.replaced = append(model.replaced, string(source))
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
	if len(model.files) != 1 || len(model.open) != 0 {
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

func candidate(id uint64) string {
	return fmt.Sprintf("/home/.doc.txt.~save.%016x", id)
}

func TestSaveSkipsAndPreservesStaleTemporaryFiles(t *testing.T) {
	model := newModel()
	legacy := "/home/.doc.txt.~save"
	model.files[legacy] = []byte("legacy stale file")
	model.files[candidate(1)] = []byte("stale candidate")
	var temp [limits.PathMax]byte
	var sequence atomic.Uint64
	status, stage := saveWithSequence(model, []byte("/home/doc.txt"), make([]byte, 8), &chunks{text: []byte("second"), step: 8}, &temp, &sequence)
	if status != vfs.StatusOK || stage != 0 {
		t.Fatalf("save = %d at %d", status, stage)
	}
	if string(model.files[legacy]) != "legacy stale file" || string(model.files[candidate(1)]) != "stale candidate" {
		t.Fatal("stale temporary file changed")
	}
	if string(model.files["/home/doc.txt"]) != "second" || len(model.open) != 0 || len(model.removed) != 0 {
		t.Fatal("save did not publish independently of stale files")
	}
	if len(model.tried) != 2 || model.tried[0] != candidate(1) || model.tried[1] != candidate(2) || len(model.opened) != 1 || model.opened[0] != candidate(2) || len(model.replaced) != 1 || model.replaced[0] != candidate(2) {
		t.Fatalf("save used a stale file: tried=%v opened=%v replaced=%v", model.tried, model.opened, model.replaced)
	}
}

// interleavedFiles pauses the first save between Create and OpenWrite and
// checks complete contents immediately before each atomic replacement.
type interleavedFiles struct {
	Files
	beforeOpen    func([]byte)
	beforeReplace func([]byte, []byte)
}

func (files interleavedFiles) OpenWrite(path []byte) (vfs.Handle, vfs.Status) {
	if files.beforeOpen != nil {
		files.beforeOpen(path)
	}
	return files.Files.OpenWrite(path)
}

func (files interleavedFiles) Replace(source, target []byte) vfs.Status {
	files.beforeReplace(source, target)
	return files.Files.Replace(source, target)
}

func TestConcurrentSavesOwnSeparateTemporaryFiles(t *testing.T) {
	for _, separateProcesses := range []bool{false, true} {
		t.Run(fmt.Sprintf("separate-processes=%t", separateProcesses), func(t *testing.T) {
			model := newModel()
			// Distinct counters model two processes starting at the same ID.
			// No mutable global hook or scheduler timing is needed.
			var firstSequence, secondSequence atomic.Uint64
			secondCounter := &firstSequence
			if separateProcesses {
				secondCounter = &secondSequence
			}
			var firstTemp, secondTemp [limits.PathMax]byte
			firstSource := &chunks{text: []byte("complete first text"), step: 3}
			secondSource := &chunks{text: []byte("complete second text"), step: 4}
			secondFiles := interleavedFiles{Files: model, beforeReplace: func(source, target []byte) {
				if string(model.files[string(target)]) != "original" || string(model.files[string(source)]) != "complete second text" || len(model.open) != 0 {
					t.Fatal("second save published before its contents were complete and closed")
				}
			}}
			firstFiles := interleavedFiles{
				Files: model,
				beforeOpen: func(firstPath []byte) {
					if contents, found := model.files[string(firstPath)]; !found || len(contents) != 0 {
						t.Fatal("first save has not reserved its empty temporary file")
					}
					status, stage := saveWithSequence(secondFiles, []byte("/home/doc.txt"), make([]byte, 8), secondSource, &secondTemp, secondCounter)
					if status != vfs.StatusOK || stage != 0 {
						t.Fatalf("second save = %d at %d", status, stage)
					}
					if len(model.created) != 2 || model.created[0] == model.created[1] || len(model.opened) != 1 || model.opened[0] == string(firstPath) || len(model.removed) != 0 {
						t.Fatalf("second save touched the first temp: created=%v opened=%v removed=%v", model.created, model.opened, model.removed)
					}
					if contents, found := model.files[string(firstPath)]; !found || len(contents) != 0 || string(model.files["/home/doc.txt"]) != "complete second text" {
						t.Fatal("second save changed the first temp or published partial contents")
					}
				},
				beforeReplace: func(source, target []byte) {
					if string(model.files[string(target)]) != "complete second text" || string(model.files[string(source)]) != "complete first text" || len(model.open) != 0 {
						t.Fatal("first save published before its contents were complete and closed")
					}
				},
			}
			status, stage := saveWithSequence(firstFiles, []byte("/home/doc.txt"), make([]byte, 4), firstSource, &firstTemp, &firstSequence)
			if status != vfs.StatusOK || stage != 0 {
				t.Fatalf("first save = %d at %d", status, stage)
			}
			wantCreates := 2
			if separateProcesses {
				wantCreates++ // second process first collides with the live first temp
			}
			if model.calls["create"] != wantCreates || model.calls["open"] != 2 || model.calls["replace"] != 2 || len(model.removed) != 0 {
				t.Fatalf("unexpected operations: %v removed=%v", model.calls, model.removed)
			}
			if string(model.files["/home/doc.txt"]) != "complete first text" || len(model.files) != 1 || len(model.open) != 0 {
				t.Fatal("saves did not finish with one complete winner and no temporary files")
			}
		})
	}
}

func TestCandidateCollisionsAreBoundedAndUntouched(t *testing.T) {
	model := newModel()
	for id := 1; id <= temporaryAttempts; id++ {
		model.files[candidate(uint64(id))] = []byte("another owner's file")
	}
	var sequence atomic.Uint64
	var temp [limits.PathMax]byte
	source := &chunks{text: []byte("not consumed"), step: 4}
	status, stage := saveWithSequence(model, []byte("/home/doc.txt"), make([]byte, 4), source, &temp, &sequence)
	if status != vfs.StatusBusy || stage != StageTemporary {
		t.Fatalf("save = %d at %d", status, stage)
	}
	if len(model.calls) != 1 || model.calls["create"] != temporaryAttempts || len(model.removed) != 0 || string(source.text) != "not consumed" || string(model.files["/home/doc.txt"]) != "original" {
		t.Fatalf("collision refusal touched state: calls=%v removed=%v source=%q", model.calls, model.removed, source.text)
	}
	for id := 1; id <= temporaryAttempts; id++ {
		if string(model.files[candidate(uint64(id))]) != "another owner's file" {
			t.Fatalf("candidate %d changed", id)
		}
	}
}

func TestTemporarySequenceDoesNotWrap(t *testing.T) {
	model := newModel()
	var sequence atomic.Uint64
	sequence.Store(^uint64(0) - 1)
	var temp [limits.PathMax]byte
	if status, _ := saveWithSequence(model, []byte("/home/doc.txt"), make([]byte, 4), &chunks{text: []byte("last"), step: 4}, &temp, &sequence); status != vfs.StatusOK {
		t.Fatalf("last ID save = %d", status)
	}
	source := &chunks{text: []byte("not consumed"), step: 4}
	status, stage := saveWithSequence(model, []byte("/home/doc.txt"), make([]byte, 4), source, &temp, &sequence)
	if status != vfs.StatusBusy || stage != StageTemporary || sequence.Load() != ^uint64(0) {
		t.Fatalf("exhausted sequence: status=%d stage=%d sequence=%d", status, stage, sequence.Load())
	}
	if len(model.tried) != 1 || model.tried[0] != candidate(^uint64(0)) || string(source.text) != "not consumed" || string(model.files["/home/doc.txt"]) != "last" {
		t.Fatal("exhausted counter reused a name or changed contents")
	}
}

func TestTemporaryIDsAreUniqueUnderContention(t *testing.T) {
	const workers = 16
	var sequence atomic.Uint64
	start := make(chan struct{})
	ids := make(chan uint64, workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			<-start
			id, _ := nextTemporaryID(&sequence)
			ids <- id
		}()
	}
	close(start)
	seen := map[uint64]bool{}
	for worker := 0; worker < workers; worker++ {
		id := <-ids
		if id == 0 || seen[id] {
			t.Fatalf("duplicate or unavailable ID %d", id)
		}
		seen[id] = true
	}
	if sequence.Load() != workers {
		t.Fatalf("sequence = %d", sequence.Load())
	}
}

// TestFailureAtEveryStepKeepsTheOriginal injects failures before the model
// changes the target. Replace failures leave the temporary pathname alone:
// a real backend may have committed the rename before reporting an error.
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
		wantFiles := 1
		if test.stage == StageReplace {
			wantFiles++
		}
		if len(model.files) != wantFiles || len(model.open) != 0 {
			t.Fatalf("%s#%d: files=%v open=%v", test.op, test.at, model.files, model.open)
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

func TestFailedCleanupDoesNotBlockForwardRetry(t *testing.T) {
	model := newModel()
	model.limit = 5
	model.failOp, model.failAt, model.failAs = "unlink", 1, vfs.StatusBackendError
	var temp [limits.PathMax]byte
	status, stage := Save(model, []byte("/home/doc.txt"), make([]byte, 4), &chunks{text: []byte("does not fit"), step: 4}, &temp)
	if status != vfs.StatusNoSpace || stage != StageWrite || string(model.files["/home/doc.txt"]) != "original" {
		t.Fatalf("owner save = %d at %d", status, stage)
	}
	leftover := model.created[0]
	status, stage = Save(model, []byte("/home/doc.txt"), make([]byte, 4), &chunks{text: []byte("retry"), step: 4}, &temp)
	if status != vfs.StatusOK || stage != 0 {
		t.Fatalf("retry save = %d at %d", status, stage)
	}
	if string(model.files["/home/doc.txt"]) != "retry" || string(model.files[leftover]) != "does " {
		t.Fatal("retry failed or changed the earlier incomplete file")
	}
	if len(model.created) != 2 || model.created[1] == leftover || len(model.removed) != 1 || model.removed[0] != leftover || len(model.open) != 0 {
		t.Fatalf("retry did not own a separate file: created=%v removed=%v open=%v", model.created, model.removed, model.open)
	}
}

func TestReplaceErrorDoesNotBlockForwardRetry(t *testing.T) {
	model := newModel()
	model.failOp, model.failAt, model.failAs = "replace", 1, vfs.StatusBackendError
	var temp [limits.PathMax]byte
	status, stage := Save(model, []byte("/home/doc.txt"), make([]byte, 8), &chunks{text: []byte("first"), step: 8}, &temp)
	if status != vfs.StatusBackendError || stage != StageReplace || string(model.files["/home/doc.txt"]) != "original" {
		t.Fatalf("first save = %d at %d", status, stage)
	}
	leftover := model.created[0]
	status, stage = Save(model, []byte("/home/doc.txt"), make([]byte, 8), &chunks{text: []byte("retry"), step: 8}, &temp)
	if status != vfs.StatusOK || stage != 0 || string(model.files["/home/doc.txt"]) != "retry" || string(model.files[leftover]) != "first" || len(model.removed) != 0 {
		t.Fatalf("forward retry = %d at %d, files=%v removed=%v", status, stage, model.files, model.removed)
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

func TestCandidatePathBounds(t *testing.T) {
	maxName := limits.NameMax - 1 - len(Suffix) - temporaryIDBytes
	maxTarget := limits.PathMax - 1 - len(Suffix) - temporaryIDBytes
	longTarget := func(size int) []byte {
		path := bytes.Repeat([]byte("/d"), (size-2)/2)
		path = append(path, '/')
		return append(path, bytes.Repeat([]byte{'n'}, size-len(path))...)
	}
	for _, test := range []struct {
		name   string
		target []byte
		status vfs.Status
	}{
		{"component fits", append([]byte("/"), bytes.Repeat([]byte{'n'}, maxName)...), vfs.StatusOK},
		{"component too long", append([]byte("/"), bytes.Repeat([]byte{'n'}, maxName+1)...), vfs.StatusNameTooLong},
		{"path fits", longTarget(maxTarget), vfs.StatusOK},
		{"path too long", longTarget(maxTarget + 1), vfs.StatusNameTooLong},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := newModel()
			var temp [limits.PathMax]byte
			var sequence atomic.Uint64
			path, status := reserveTemporary(model, test.target, &temp, &sequence)
			if status != test.status {
				t.Fatalf("candidate = %d, want %d", status, test.status)
			}
			if status != vfs.StatusOK && (len(model.calls) != 0 || sequence.Load() != 0) {
				t.Fatal("invalid candidate attempted reservation")
			}
			if status == vfs.StatusOK && (len(path) > limits.PathMax || len(model.created) != 1) {
				t.Fatal("candidate exceeded path bounds or was not reserved")
			}
		})
	}
}

// ambiguousReplace commits the rename and lets a new owner create the freed
// temporary pathname before returning a failed flush/completion status.
type ambiguousReplace struct {
	Files
	newOwner func([]byte)
}

func (files ambiguousReplace) Replace(source, target []byte) vfs.Status {
	if status := files.Files.Replace(source, target); status != vfs.StatusOK {
		return status
	}
	files.newOwner(source)
	return vfs.StatusBackendError
}

func TestReplaceErrorDoesNotUnlinkANewOwnersFile(t *testing.T) {
	model := newModel()
	files := ambiguousReplace{Files: model, newOwner: func(path []byte) {
		if status := model.Create(path); status != vfs.StatusOK {
			t.Fatalf("new owner create = %d", status)
		}
		model.files[string(path)] = []byte("new owner's incomplete save")
	}}
	var temp [limits.PathMax]byte
	status, stage := Save(files, []byte("/home/doc.txt"), make([]byte, 8), &chunks{text: []byte("committed save"), step: 8}, &temp)
	if status != vfs.StatusBackendError || stage != StageReplace {
		t.Fatalf("save = %d at %d", status, stage)
	}
	if string(model.files["/home/doc.txt"]) != "committed save" {
		t.Fatal("committed target changed")
	}
	if string(model.files[model.created[0]]) != "new owner's incomplete save" || len(model.removed) != 0 {
		t.Fatal("failed Replace cleanup touched another owner's temporary file")
	}
}
