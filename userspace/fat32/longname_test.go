package fat32

import (
	"goos/kernel/ramfs"
	"strings"
	"testing"
)

func rootEntry(t *testing.T, fs *FileSystem, index int) [32]byte {
	t.Helper()
	dir, status := fs.openDirectory(0)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	entry, status := fs.readEntry(&dir, index)
	if status != ramfs.StatusOK {
		t.Fatalf("entry %d unreadable", index)
	}
	return entry
}

func shortNames(t *testing.T, fs *FileSystem, key uint32) []string {
	t.Helper()
	dir, status := fs.openDirectory(key)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	var names []string
	for index := 0; index < fs.entries(&dir); index++ {
		entry, status := fs.readEntry(&dir, index)
		if status != ramfs.StatusOK {
			t.Fatalf("entry %d unreadable", index)
		}
		if entry[0] == entryEnd {
			break
		}
		if listed(entry) {
			names = append(names, string(entry[:11]))
		}
	}
	return names
}

func TestLongNameRoundTrip(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	dir := mkdir(t, fs, root, "Long Directory Name")
	for _, name := range []string{"startup-block-regression.txt", "Mixed Case Name.TXT", strings.Repeat("e", 39)} {
		id := writeFile(t, fs, dir, name, name)
		opened, status := fs.OpenName(dir, mustName(t, name))
		if status != ramfs.StatusOK || opened != id {
			t.Fatalf("open %q: %d %v", name, opened, status)
		}
		if got := readFile(t, fs, id); got != name {
			t.Fatalf("read %q got %q", name, got)
		}
	}
	names := listNames(t, fs, dir)
	if len(names) != 3 || names[0] != "startup-block-regression.txt" || names[1] != "Mixed Case Name.TXT" || names[2] != strings.Repeat("e", 39) {
		t.Fatalf("listing %v", names)
	}
	if names := listNames(t, fs, root); len(names) != 1 || names[0] != "Long Directory Name" {
		t.Fatalf("root listing %v", names)
	}
}

func TestLongNameSurvivesRemount(t *testing.T) {
	device := formattedDevice()
	fs, root := mounted(t, device)
	writeFile(t, fs, root, "persisted-long-name.txt", "kept")
	reopened, root := mounted(t, device)
	id, status := reopened.OpenName(root, mustName(t, "persisted-long-name.txt"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if got := readFile(t, reopened, id); got != "kept" {
		t.Fatalf("read %q", got)
	}
}

// TestShortNameKeepsOneEntry pins the on-disk format for 8.3 names: no long
// entries are written and the directory still holds a single entry.
func TestShortNameKeepsOneEntry(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	writeFile(t, fs, root, "hello.txt", "x")
	if entry := rootEntry(t, fs, 0); string(entry[:11]) != "HELLO   TXT" || entry[11] != attrArchive {
		t.Fatalf("first entry %q attr %#x", entry[:11], entry[11])
	}
	if entry := rootEntry(t, fs, 1); entry[0] != entryEnd {
		t.Fatalf("second entry %#x, want the end marker", entry[0])
	}
}

func TestLongNameChecksumMismatchFallsBackToShortName(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	writeFile(t, fs, root, "long-name-file.txt", "x")
	dir, _ := fs.openDirectory(0)
	entry, _ := fs.readEntry(&dir, 0)
	if !isLongEntry(entry) {
		t.Fatalf("entry 0 is not a long entry: %#x", entry[11])
	}
	entry[13]++
	if fs.writeEntry(&dir, 0, entry) != ramfs.StatusOK {
		t.Fatal("write failed")
	}
	if _, status := fs.OpenName(root, mustName(t, "long-name-file.txt")); status != ramfs.StatusNotFound {
		t.Fatalf("broken chain still matched: %v", status)
	}
	names := listNames(t, fs, root)
	if len(names) != 1 || names[0] != "long-n~1.txt" {
		t.Fatalf("listing %v, want the short alias", names)
	}
	if _, status := fs.OpenName(root, mustName(t, "long-n~1.txt")); status != ramfs.StatusOK {
		t.Fatalf("alias lookup: %v", status)
	}
}

func TestLongNameOrdinalMismatchFallsBackToShortName(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	writeFile(t, fs, root, "another-long-name.txt", "x")
	dir, _ := fs.openDirectory(0)
	entry, _ := fs.readEntry(&dir, 0)
	entry[0] = longLast | 3
	if fs.writeEntry(&dir, 0, entry) != ramfs.StatusOK {
		t.Fatal("write failed")
	}
	if _, status := fs.OpenName(root, mustName(t, "another-long-name.txt")); status != ramfs.StatusNotFound {
		t.Fatalf("broken ordinals still matched: %v", status)
	}
	if names := listNames(t, fs, root); len(names) != 1 || names[0] != "anothe~1.txt" {
		t.Fatalf("listing %v, want the short alias", names)
	}
}

func TestAliasNumberingForSharedPrefix(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	for _, name := range []string{"shared-prefix-one.txt", "shared-prefix-two.txt", "shared-prefix-three.txt"} {
		writeFile(t, fs, root, name, "x")
	}
	aliases := shortNames(t, fs, 0)
	want := []string{"SHARED~1TXT", "SHARED~2TXT", "SHARED~3TXT"}
	if len(aliases) != len(want) {
		t.Fatalf("aliases %v", aliases)
	}
	for i, alias := range aliases {
		if alias != want[i] {
			t.Fatalf("alias %d = %q, want %q", i, alias, want[i])
		}
	}
	for _, name := range []string{"shared-prefix-one.txt", "shared-prefix-two.txt", "shared-prefix-three.txt"} {
		if _, status := fs.OpenName(root, mustName(t, name)); status != ramfs.StatusOK {
			t.Fatalf("open %q: %v", name, status)
		}
	}
}

func TestLongNameLengthBound(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	bound := strings.Repeat("b", maxLongName-4) + ".txt"
	id := writeFile(t, fs, root, bound, "at the bound")
	if opened, status := fs.OpenName(root, mustName(t, bound)); status != ramfs.StatusOK || opened != id {
		t.Fatalf("open at the bound: %v", status)
	}
	if names := listNames(t, fs, root); len(names) != 1 || names[0] != bound {
		t.Fatalf("listing %v", names)
	}
	if _, ok := ramfs.MakeName([]byte(strings.Repeat("b", maxLongName+1))); ok {
		t.Fatal("a name past the bound was accepted")
	}
	for _, name := range []string{"star*name", "pipe|name", "back\\slash", "trailing dot."} {
		encoded, ok := ramfs.MakeName([]byte(name))
		if !ok {
			t.Fatalf("name %q", name)
		}
		if _, status := fs.CreateName(root, encoded); status != ramfs.StatusInvalidArgument {
			t.Fatalf("create %q: %v", name, status)
		}
	}
}

func TestUnlinkRemovesWholeChain(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	writeFile(t, fs, root, "removed-long-name.txt", "x")
	dir, _ := fs.openDirectory(0)
	used := 0
	for ; used < fs.entries(&dir); used++ {
		entry, _ := fs.readEntry(&dir, used)
		if entry[0] == entryEnd {
			break
		}
	}
	if used != 3 {
		t.Fatalf("chain occupies %d entries, want 3", used)
	}
	if status := fs.UnlinkName(root, mustName(t, "removed-long-name.txt")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	for index := 0; index < used; index++ {
		if entry := rootEntry(t, fs, index); entry[0] != entryDeleted {
			t.Fatalf("entry %d left behind: %#x", index, entry[0])
		}
	}
	writeFile(t, fs, root, "reused-long-name.txt", "y")
	if names := listNames(t, fs, root); len(names) != 1 || names[0] != "reused-long-name.txt" {
		t.Fatalf("listing %v", names)
	}
}

func TestRenameMovesWholeChain(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	id := writeFile(t, fs, root, "renamed-long-name.txt", "payload")
	if status := fs.RenameName(id, root, mustName(t, "short.txt")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if names := listNames(t, fs, root); len(names) != 1 || names[0] != "short.txt" {
		t.Fatalf("after shrink %v", names)
	}
	id, status := fs.OpenName(root, mustName(t, "short.txt"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if status := fs.RenameName(id, root, mustName(t, "grown-back-to-a-long-name.txt")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if names := listNames(t, fs, root); len(names) != 1 || names[0] != "grown-back-to-a-long-name.txt" {
		t.Fatalf("after growth %v", names)
	}
	id, status = fs.OpenName(root, mustName(t, "grown-back-to-a-long-name.txt"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if got := readFile(t, fs, id); got != "payload" {
		t.Fatalf("read %q", got)
	}
}

// TestLongNameChainSpansClusters covers a chain whose entries do not fit in
// what is left of the current cluster.
func TestLongNameChainSpansClusters(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	for i := 0; i < fs.entriesPerCluster()-2; i++ {
		writeFile(t, fs, root, string([]byte{'f', byte('0' + i/10), byte('0' + i%10)}), "x")
	}
	long := strings.Repeat("s", maxLongName-4) + ".txt"
	id := writeFile(t, fs, root, long, "spanned")
	dir, _ := fs.openDirectory(0)
	if dir.clusters != 2 {
		t.Fatalf("directory holds %d clusters, want 2", dir.clusters)
	}
	if opened, status := fs.OpenName(root, mustName(t, long)); status != ramfs.StatusOK || opened != id {
		t.Fatalf("open spanning chain: %v", status)
	}
	if got := readFile(t, fs, id); got != "spanned" {
		t.Fatalf("read %q", got)
	}
}
