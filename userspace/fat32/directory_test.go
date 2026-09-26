package fat32

import (
	"goos/kernel/ramfs"
	"testing"
)

func mustName(t testing.TB, value string) ramfs.Name {
	t.Helper()
	name, ok := ramfs.MakeName([]byte(value))
	if !ok {
		t.Fatalf("name %q", value)
	}
	return name
}

func mounted(t testing.TB, device Device) (*FileSystem, ramfs.NodeID) {
	t.Helper()
	fs := &FileSystem{}
	root, ok := fs.Mount(device)
	if !ok {
		t.Fatal("mount failed")
	}
	return fs, root
}

func mkdir(t testing.TB, fs *FileSystem, parent ramfs.NodeID, name string) ramfs.NodeID {
	t.Helper()
	id, status := fs.MkdirName(parent, mustName(t, name))
	if status != ramfs.StatusOK {
		t.Fatalf("mkdir %s: %v", name, status)
	}
	return id
}

func writeFile(t testing.TB, fs *FileSystem, parent ramfs.NodeID, name, payload string) ramfs.NodeID {
	t.Helper()
	id, status := fs.CreateName(parent, mustName(t, name))
	if status != ramfs.StatusOK {
		t.Fatalf("create %s: %v", name, status)
	}
	if count, status := fs.Write(id, 0, []byte(payload)); status != ramfs.StatusOK || count != len(payload) {
		t.Fatalf("write %s: %d %v", name, count, status)
	}
	return id
}

func readFile(t *testing.T, fs *FileSystem, id ramfs.NodeID) string {
	t.Helper()
	target := make([]byte, 128)
	count, status := fs.Read(id, 0, target)
	if status != ramfs.StatusOK {
		t.Fatalf("read: %v", status)
	}
	return string(target[:count])
}

func listNames(t *testing.T, fs *FileSystem, dir ramfs.NodeID) []string {
	t.Helper()
	var names []string
	for cursor := 0; ; {
		entry, next, status := fs.ReadDir(dir, cursor)
		if status == ramfs.StatusNotFound {
			return names
		}
		if status != ramfs.StatusOK {
			t.Fatalf("readdir: %v", status)
		}
		names = append(names, string(entry.Name[:entry.Size]))
		cursor = next
	}
}

func fatValue(fs *FileSystem, cluster uint32) uint32 {
	value, _ := fs.fat(cluster)
	return value
}

func TestSubdirectoryFileLifecycle(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	demo := mkdir(t, fs, root, "demo")
	if stat, status := fs.Stat(demo); status != ramfs.StatusOK || stat.Kind != ramfs.KindDirectory {
		t.Fatalf("stat dir %+v %v", stat, status)
	}
	id := writeFile(t, fs, demo, "message.txt", "persistent\n")
	opened, status := fs.OpenName(demo, mustName(t, "message.txt"))
	if status != ramfs.StatusOK || opened != id {
		t.Fatalf("open %d %v", opened, status)
	}
	if got := readFile(t, fs, id); got != "persistent\n" {
		t.Fatalf("read %q", got)
	}
	if status := fs.Truncate(id, 0); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if stat, _ := fs.Stat(id); stat.Size != 0 || stat.Kind != ramfs.KindFile {
		t.Fatalf("truncated %+v", stat)
	}
	if _, status := fs.OpenName(root, mustName(t, "message.txt")); status != ramfs.StatusNotFound {
		t.Fatalf("file leaked into root: %v", status)
	}
	if _, status := fs.Read(demo, 0, make([]byte, 4)); status != ramfs.StatusIsDirectory {
		t.Fatalf("read directory: %v", status)
	}
	if _, status := fs.CreateName(id, mustName(t, "x")); status != ramfs.StatusNotDirectory {
		t.Fatalf("create under file: %v", status)
	}
}

func TestNestedDirectoriesAndReadDir(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	outer := mkdir(t, fs, root, "outer")
	inner := mkdir(t, fs, outer, "inner")
	deep := mkdir(t, fs, inner, "deep")
	id := writeFile(t, fs, deep, "leaf.txt", "nested")
	writeFile(t, fs, inner, "a.txt", "a")
	if got := readFile(t, fs, id); got != "nested" {
		t.Fatalf("read %q", got)
	}
	names := listNames(t, fs, inner)
	if len(names) != 2 || names[0] != "deep" || names[1] != "a.txt" {
		t.Fatalf("inner listing %v", names)
	}
	if names := listNames(t, fs, root); len(names) != 1 || names[0] != "outer" {
		t.Fatalf("root listing %v", names)
	}
	entry, _, _ := fs.ReadDir(inner, 0)
	if entry.Stat.Kind != ramfs.KindDirectory || entry.Stat.ID != deep {
		t.Fatalf("directory entry %+v", entry.Stat)
	}
	cluster := uint32(uint64(deep) >> slotBits)
	var dotDot [32]byte
	fs.readMeta(fs.clusterOffset(entryClusterOf(t, fs, deep))+entrySize, dotDot[:])
	if entryCluster(dotDot) != cluster {
		t.Fatalf("deep '..' = %d, want %d", entryCluster(dotDot), cluster)
	}
	fs.readMeta(fs.clusterOffset(entryClusterOf(t, fs, outer))+entrySize, dotDot[:])
	if entryCluster(dotDot) != 0 {
		t.Fatalf("outer '..' = %d, want 0 for the root", entryCluster(dotDot))
	}
}

func entryClusterOf(t *testing.T, fs *FileSystem, id ramfs.NodeID) uint32 {
	t.Helper()
	_, _, entry, status := fs.node(id)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	return entryCluster(entry)
}

func TestRenameAcrossDirectories(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	a, b := mkdir(t, fs, root, "a"), mkdir(t, fs, root, "b")
	id := writeFile(t, fs, a, "file", "payload")
	cluster := entryClusterOf(t, fs, id)
	if status := fs.RenameName(id, a, mustName(t, "renamed")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	id, _ = fs.OpenName(a, mustName(t, "renamed"))
	if status := fs.RenameName(id, b, mustName(t, "moved-with-a-long-name")); status != ramfs.StatusOK {
		t.Fatalf("move: %v", status)
	}
	if names := listNames(t, fs, a); len(names) != 0 {
		t.Fatalf("source still lists %v", names)
	}
	moved, status := fs.OpenName(b, mustName(t, "moved-with-a-long-name"))
	if status != ramfs.StatusOK || entryClusterOf(t, fs, moved) != cluster || readFile(t, fs, moved) != "payload" {
		t.Fatalf("moved file: %v", status)
	}
	if _, status := fs.Stat(id); status != ramfs.StatusNotFound {
		t.Fatalf("the old node still resolves: %v", status)
	}
	other := writeFile(t, fs, a, "taken", "x")
	if status := fs.RenameName(other, b, mustName(t, "moved-with-a-long-name")); status != ramfs.StatusAlreadyExists {
		t.Fatalf("move onto an existing name: %v", status)
	}
}

func TestMoveDirectoryRepointsParent(t *testing.T) {
	device := formattedDevice()
	fs, root := mounted(t, device)
	outer := mkdir(t, fs, root, "outer")
	inner := mkdir(t, fs, outer, "inner")
	writeFile(t, fs, inner, "leaf", "deep")
	innerCluster := entryClusterOf(t, fs, inner)
	if status := fs.RenameName(inner, root, mustName(t, "top")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if parent, status := fs.parentKey(innerCluster); status != ramfs.StatusOK || parent != 0 {
		t.Fatalf("moved '..' = %d %v, want the root", parent, status)
	}
	top, _ := fs.OpenName(root, mustName(t, "top"))
	outer, _ = fs.OpenName(root, mustName(t, "outer"))
	if status := fs.RenameName(top, outer, mustName(t, "back")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if parent, _ := fs.parentKey(innerCluster); parent != entryClusterOf(t, fs, outer) {
		t.Fatalf("moved back '..' = %d", parent)
	}
	reopened, root := mounted(t, device)
	outer, _ = reopened.OpenName(root, mustName(t, "outer"))
	back, status := reopened.OpenName(outer, mustName(t, "back"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	leaf, status := reopened.OpenName(back, mustName(t, "leaf"))
	if status != ramfs.StatusOK || readFile(t, reopened, leaf) != "deep" {
		t.Fatalf("leaf after moves: %v", status)
	}
}

func TestMoveDirectoryIntoItselfIsRefused(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	outer := mkdir(t, fs, root, "outer")
	inner := mkdir(t, fs, outer, "inner")
	deep := mkdir(t, fs, inner, "deep")
	for _, parent := range []ramfs.NodeID{outer, inner, deep} {
		if status := fs.RenameName(outer, parent, mustName(t, "loop")); status != ramfs.StatusInvalidArgument {
			t.Fatalf("move into own subtree: %v", status)
		}
	}
	if names := listNames(t, fs, root); len(names) != 1 || names[0] != "outer" {
		t.Fatalf("root after refused moves %v", names)
	}
}

func TestUnlinkDirectory(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	demo := mkdir(t, fs, root, "demo")
	dirCluster := entryClusterOf(t, fs, demo)
	file := writeFile(t, fs, demo, "message.txt", "persistent")
	fileCluster := entryClusterOf(t, fs, file)
	name := mustName(t, "demo")
	if status := fs.UnlinkName(root, name); status != ramfs.StatusNotEmpty {
		t.Fatalf("unlink non-empty: %v", status)
	}
	if status := fs.UnlinkName(demo, mustName(t, "message.txt")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if status := fs.UnlinkName(root, name); status != ramfs.StatusOK {
		t.Fatalf("unlink empty: %v", status)
	}
	if fatValue(fs, dirCluster) != 0 || fatValue(fs, fileCluster) != 0 {
		t.Fatalf("clusters not freed: %#x %#x", fatValue(fs, dirCluster), fatValue(fs, fileCluster))
	}
	if _, status := fs.OpenName(root, name); status != ramfs.StatusNotFound {
		t.Fatalf("open removed: %v", status)
	}
	if _, status := fs.Stat(demo); status != ramfs.StatusNotFound {
		t.Fatalf("stale directory id: %v", status)
	}
}

func TestSubdirectoryPersistsAcrossRemount(t *testing.T) {
	device := formattedDevice()
	fs, root := mounted(t, device)
	demo := mkdir(t, fs, root, "demo")
	sub := mkdir(t, fs, demo, "sub")
	writeFile(t, fs, sub, "keep.txt", "survives")
	reopened, root := mounted(t, device)
	demo, status := reopened.OpenName(root, mustName(t, "demo"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	sub, status = reopened.OpenName(demo, mustName(t, "sub"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	id, status := reopened.OpenName(sub, mustName(t, "keep.txt"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if got := readFile(t, reopened, id); got != "survives" {
		t.Fatalf("read %q", got)
	}
}

func TestDirectoryGrowsToBound(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	demo := mkdir(t, fs, root, "demo")
	dir, status := fs.directoryOf(demo)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if status = fs.cover(&dir, maxDirEntries); status != ramfs.StatusOK {
		t.Fatalf("grow to the FAT limit: %v", status)
	}
	if dir.clusters != uint64(maxDirEntries/fs.entriesPerCluster()) {
		t.Fatalf("directory holds %d clusters", dir.clusters)
	}
	if status = fs.cover(&dir, maxDirEntries+1); status != ramfs.StatusNoSpace {
		t.Fatalf("grow past the FAT limit: %v", status)
	}
	// Every grown cluster was zeroed, so the directory still ends after "..".
	if names := listNames(t, fs, demo); len(names) != 0 {
		t.Fatalf("grown directory lists %v", names)
	}
	id := writeFile(t, fs, demo, "last.txt", "x")
	if _, _, _, status := fs.node(id); status != ramfs.StatusOK {
		t.Fatal(status)
	}
}

func TestCorruptDirectoryReportsError(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	demo := mkdir(t, fs, root, "demo")
	cluster := entryClusterOf(t, fs, demo)

	fs.setFAT(cluster, cluster)
	if _, status := fs.CreateName(demo, mustName(t, "a.txt")); status == ramfs.StatusOK {
		t.Fatal("looped directory chain accepted")
	}
	if _, _, status := fs.ReadDir(demo, 0); status == ramfs.StatusOK || status == ramfs.StatusNotFound {
		t.Fatalf("readdir on loop: %v", status)
	}
	if status := fs.UnlinkName(root, mustName(t, "demo")); status == ramfs.StatusOK {
		t.Fatal("unlinked looped directory")
	}

	fs.setFAT(cluster, endOfChainMark)
	_, index, entry, _ := fs.node(demo)
	dir, _ := fs.openDirectory(0)
	setEntryCluster(&entry, fs.clusters+100)
	fs.writeEntry(&dir, index, entry)
	if _, status := fs.OpenName(demo, mustName(t, "a.txt")); status == ramfs.StatusOK || status == ramfs.StatusNotFound {
		t.Fatalf("out-of-range directory cluster: %v", status)
	}
	if status := fs.UnlinkName(root, mustName(t, "demo")); status == ramfs.StatusOK {
		t.Fatal("unlinked corrupt directory")
	}
	for _, id := range []ramfs.NodeID{0, 1 << 60, ramfs.NodeID(cluster)<<slotBits | 1, ramfs.NodeID(fs.clusters+9) << slotBits} {
		if _, status := fs.Stat(id); status != ramfs.StatusNotFound {
			t.Fatalf("stat %#x: %v", id, status)
		}
	}
}

func TestCorruptFileChainReportsError(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	id := writeFile(t, fs, root, "loop.txt", string(make([]byte, 1500)))
	first := entryClusterOf(t, fs, id)
	fs.setFAT(first, fs.clusters+50)
	if _, status := fs.Read(id, 0, make([]byte, 1500)); status == ramfs.StatusOK {
		t.Fatal("chain leaving the data area accepted")
	}
	fs.setFAT(first, first)
	if _, status := fs.Read(id, 0, make([]byte, 1500)); status != ramfs.StatusOK {
		t.Fatalf("bounded looped read: %v", status)
	}
	if status := fs.UnlinkName(root, mustName(t, "loop.txt")); status != ramfs.StatusOK {
		t.Fatalf("unlink looped file: %v", status)
	}
	if fatValue(fs, first) != 0 {
		t.Fatal("looped chain not freed")
	}
}
