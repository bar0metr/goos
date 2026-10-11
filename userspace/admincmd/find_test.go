package admincmd

import (
	"goos/internal/limits"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"strings"
	"testing"
)

type directoryEntry struct {
	name string
	kind ramfs.Kind
}
type directories struct {
	entries  map[string][]directoryEntry
	borrowed [limits.NameMax]byte
	failure  vfs.Status
	chain    bool
	wide     int
}

func (dirs *directories) ReadDir(path []byte, visitor Visitor) vfs.Status {
	if dirs.failure != vfs.StatusOK {
		return dirs.failure
	}
	entries := dirs.entries[string(path)]
	if dirs.chain {
		entries = []directoryEntry{{"d", ramfs.KindDirectory}}
	}
	if dirs.wide != 0 {
		for index := 0; index < dirs.wide; index++ {
			if !visitor.Visit([]byte("x"), ramfs.KindFile, 0, ramfs.Timestamp{}) {
				break
			}
		}
		return vfs.StatusOK
	}
	for _, entry := range entries {
		name := dirs.borrowed[:copy(dirs.borrowed[:], entry.name)]
		if !visitor.Visit(name, entry.kind, 0, ramfs.Timestamp{}) {
			break
		}
		// Simulate the next request overwriting the borrowed reply name.
		for index := range dirs.borrowed {
			dirs.borrowed[index] = '?'
		}
	}
	return vfs.StatusOK
}

func TestFindExactBasenameAndBorrowedNames(t *testing.T) {
	dirs := &directories{entries: map[string][]directoryEntry{
		"/root":         {{"one", ramfs.KindDirectory}, {"file", ramfs.KindFile}, {"filename", ramfs.KindFile}},
		"/root/one":     {{"file", ramfs.KindFile}, {"two", ramfs.KindDirectory}},
		"/root/one/two": {{"file", ramfs.KindFile}},
	}}
	var finder Finder
	var out output
	if status := finder.Walk([]byte("/root"), []byte("file"), ramfs.KindDirectory, dirs, &out); status != OK {
		t.Fatal(status)
	}
	if out.String() != "/root/one/file\n/root/one/two/file\n/root/file\n" {
		t.Fatal("borrowed name corrupted path", out.String())
	}
	out.Reset()
	if status := finder.Walk([]byte("/root"), nil, ramfs.KindDirectory, dirs, &out); status != OK || !strings.HasPrefix(out.String(), "/root\n/root/one\n") {
		t.Fatal("all paths", status, out.String())
	}
	out.Reset()
	if status := finder.Walk([]byte("/root/file"), []byte("file"), ramfs.KindFile, dirs, &out); status != OK || out.String() != "/root/file\n" {
		t.Fatal("file root", status)
	}
}

func TestFindBoundsAndErrors(t *testing.T) {
	var finder Finder
	var out output
	if finder.Walk([]byte("/"), nil, ramfs.KindDirectory, &directories{chain: true}, &out) != LimitError {
		t.Fatal("depth bound")
	}
	out.Reset()
	if finder.Walk([]byte("/"), nil, ramfs.KindDirectory, &directories{wide: FindWork + 1}, &out) != LimitError {
		t.Fatal("work bound")
	}
	if finder.Walk([]byte("/"), nil, ramfs.KindDirectory, &directories{failure: vfs.StatusPermissionDenied}, &out) != InputError || finder.VFSStatus != vfs.StatusPermissionDenied {
		t.Fatal("directory error")
	}
	if finder.Walk([]byte("/"), nil, ramfs.KindDirectory, &directories{entries: map[string][]directoryEntry{"/": {{"../escape", ramfs.KindFile}}}}, &out) != InputError {
		t.Fatal("bad entry")
	}
	if finder.Walk([]byte(strings.Repeat("x", limits.PathMax+1)), nil, ramfs.KindFile, &directories{}, &out) != LimitError {
		t.Fatal("path bound")
	}
	if finder.Walk([]byte(strings.Repeat("x", limits.PathMax)), nil, ramfs.KindDirectory, &directories{entries: map[string][]directoryEntry{strings.Repeat("x", limits.PathMax): {{"a", ramfs.KindFile}}}}, &out) != LimitError {
		t.Fatal("joined path bound")
	}
	out.fail = true
	if finder.Walk([]byte("/"), nil, ramfs.KindFile, &directories{}, &out) != OutputError {
		t.Fatal("write failure")
	}
}
