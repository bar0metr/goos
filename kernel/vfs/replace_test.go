package vfs

import (
	"goos/kernel/ramfs"
	"testing"
)

func writeFile(t *testing.T, namespace *Namespace, path string, text string) {
	t.Helper()
	if status := namespace.Create([]byte(path), false); status != StatusOK && status != StatusAlreadyExists {
		t.Fatalf("create %s: %d", path, status)
	}
	file, status := namespace.Open(1, []byte(path), Write)
	if status != StatusOK {
		t.Fatalf("open %s: %d", path, status)
	}
	if count, status := namespace.Write(1, file, []byte(text)); status != StatusOK || count != len(text) {
		t.Fatalf("write %s: %d %d", path, count, status)
	}
	namespace.Close(1, file)
}

func readFile(t *testing.T, namespace *Namespace, path string) string {
	t.Helper()
	file, status := namespace.Open(1, []byte(path), Read)
	if status != StatusOK {
		t.Fatalf("open %s: %d", path, status)
	}
	defer namespace.Close(1, file)
	buffer := make([]byte, 256)
	count, status := namespace.Read(1, file, buffer)
	if status != StatusOK {
		t.Fatalf("read %s: %d", path, status)
	}
	return string(buffer[:count])
}

func TestReplaceSwapsContentsAndKeepsCreationTime(t *testing.T) {
	namespace, fs := mountedNamespace(t)
	now := int64(1000)
	fs.SetClock(func() (ramfs.Timestamp, bool) {
		now++
		return ramfs.Timestamp{Seconds: now, Available: true}, true
	})
	writeFile(t, namespace, "/doc", "old")
	created, _ := namespace.Stat([]byte("/doc"))
	writeFile(t, namespace, "/.doc.~save", "new contents")
	if status := namespace.Replace([]byte("/.doc.~save"), []byte("/doc")); status != StatusOK {
		t.Fatalf("replace = %d", status)
	}
	if text := readFile(t, namespace, "/doc"); text != "new contents" {
		t.Fatalf("target holds %q", text)
	}
	if _, status := namespace.Stat([]byte("/.doc.~save")); status != StatusNotFound {
		t.Fatalf("source still resolves: %d", status)
	}
	if stat, _ := namespace.Stat([]byte("/doc")); stat.Created != created.Created {
		t.Fatalf("creation time %v, want %v", stat.Created, created.Created)
	}
}

func TestReplaceWithoutTargetRenames(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	writeFile(t, namespace, "/.new.~save", "fresh")
	if status := namespace.Replace([]byte("/.new.~save"), []byte("/new")); status != StatusOK {
		t.Fatalf("replace onto a missing name = %d", status)
	}
	if text := readFile(t, namespace, "/new"); text != "fresh" {
		t.Fatalf("renamed file holds %q", text)
	}
}

func TestReplaceRefusals(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	writeFile(t, namespace, "/doc", "old")
	writeFile(t, namespace, "/temp", "new")
	if namespace.Create([]byte("/dir"), true) != StatusOK {
		t.Fatal("mkdir failed")
	}
	if status := namespace.Replace([]byte("/temp"), []byte("/dir")); status != StatusIsDirectory {
		t.Fatalf("replace a directory = %d", status)
	}
	if status := namespace.Replace([]byte("/dir"), []byte("/doc")); status != StatusIsDirectory {
		t.Fatalf("replace with a directory = %d", status)
	}
	if status := namespace.Replace([]byte("/doc"), []byte("/doc")); status != StatusInvalidArgument {
		t.Fatalf("replace a file with itself = %d", status)
	}
	if status := namespace.Replace([]byte("/missing"), []byte("/doc")); status != StatusNotFound {
		t.Fatalf("replace from a missing file = %d", status)
	}
	// Another session holding the target open keeps it.
	held, status := namespace.Open(2, []byte("/doc"), Read)
	if status != StatusOK {
		t.Fatal(status)
	}
	if status := namespace.Replace([]byte("/temp"), []byte("/doc")); status != StatusBusy {
		t.Fatalf("replace an open file = %d", status)
	}
	namespace.Close(2, held)
	if text := readFile(t, namespace, "/doc"); text != "old" {
		t.Fatalf("refused replace changed the target to %q", text)
	}
	persistent := &ramfs.FileSystem{}
	if namespace.Mount([]byte("/persist"), persistent, persistent.Init()) != StatusOK {
		t.Fatal("mount failed")
	}
	writeFile(t, namespace, "/persist/doc", "other")
	if status := namespace.Replace([]byte("/temp"), []byte("/persist/doc")); status != StatusUnsupported {
		t.Fatalf("cross-mount replace = %d", status)
	}
	if status := namespace.Replace([]byte("/temp"), []byte("/persist")); status != StatusBusy {
		t.Fatalf("replace a mount root = %d", status)
	}
}
