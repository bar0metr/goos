package fat32

import (
	"bytes"
	"fmt"
	"goos/kernel/ramfs"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// formatted returns a device of size bytes holding an empty volume laid out
// as layout says.
func formatted(t testing.TB, size uint64, layout Layout) *memoryDevice {
	t.Helper()
	device := &memoryDevice{data: make([]byte, size)}
	if !Format(device, layout) {
		t.Fatalf("format %d bytes %+v", size, layout)
	}
	return device
}

var geometries = []struct {
	name   string
	layout Layout
}{
	{"one-sector-clusters-one-fat", Layout{SectorsPerCluster: 1, FATs: 1}},
	{"4k-clusters-two-fats", Layout{SectorsPerCluster: 8, FATs: 2}},
	{"32k-clusters-two-fats", Layout{SectorsPerCluster: 64, FATs: 2}},
	{"second-fat-active-unmirrored", Layout{SectorsPerCluster: 4, FATs: 2, NoMirror: true, ActiveFAT: 1}},
}

// check walks the whole volume the way fsck does and fails the test on any
// inconsistency: FAT copies that differ, a chain that loops, crosses another,
// or leaves the data area, a file whose chain does not match its size, a
// directory whose "." or ".." is wrong, or a cluster marked used that nothing
// owns.
func check(t testing.TB, device *memoryDevice) {
	t.Helper()
	checkVolume(t, device, false)
}

// checkVolume is check; interrupted relaxes it to what an interrupted change
// may leave behind — lost clusters, a chain longer than its file, and FAT
// copies that were not all updated — while still refusing cross-links, loops,
// and chains shorter than their files.
func checkVolume(t testing.TB, device *memoryDevice, interrupted bool) {
	t.Helper()
	var fs FileSystem
	if _, ok := fs.Mount(device); !ok {
		t.Fatal("check: mount failed")
	}
	owned := make([]bool, fs.clusters+2)
	starts := map[uint32]bool{}
	claim := func(what string, first uint32, want int) bool {
		t.Helper()
		// An interrupted rename leaves one entry under both names: the same
		// whole chain twice, which is not a cross-link.
		if interrupted && starts[first] {
			return false
		}
		starts[first] = true
		count := 0
		for cluster := first; ; count++ {
			if !fs.valid(cluster) {
				t.Fatalf("check: %s: cluster %d outside the data area", what, cluster)
			}
			if owned[cluster] {
				t.Fatalf("check: %s: cluster %d is cross-linked or looped", what, cluster)
			}
			owned[cluster] = true
			value, status := fs.fat(cluster)
			if status != ramfs.StatusOK {
				t.Fatalf("check: %s: FAT unreadable", what)
			}
			if value >= endOfChain {
				break
			}
			cluster = value
		}
		if want >= 0 && (count+1 < want || !interrupted && count+1 != want) {
			t.Fatalf("check: %s: chain of %d clusters, size needs %d", what, count+1, want)
		}
		return true
	}
	var walk func(key, parent uint32, path string)
	walk = func(key, parent uint32, path string) {
		dir, status := fs.openDirectory(key)
		if status != ramfs.StatusOK {
			t.Fatalf("check: %s: %v", path, status)
		}
		if !claim(path, dir.first, -1) {
			return
		}
		for index := 0; index < fs.entries(&dir); index++ {
			entry, _ := fs.readEntry(&dir, index)
			if entry[0] == entryEnd {
				break
			}
			if entry[0] == entryDeleted || isLongEntry(entry) {
				continue
			}
			if entry[0] == '.' {
				switch {
				case key == 0:
					t.Fatalf("check: dot entry in the root")
				case index == 0 && (!sameShort(entry, dotName) || entryCluster(entry) != key):
					t.Fatalf("check: %s: '.' names %d", path, entryCluster(entry))
				case index == 1 && (!sameShort(entry, dotDotName) || entryCluster(entry) != parent):
					t.Fatalf("check: %s: '..' names %d, want %d", path, entryCluster(entry), parent)
				case index > 1:
					t.Fatalf("check: %s: dot entry at %d", path, index)
				}
				continue
			}
			name := path + "/" + strings.TrimSpace(string(entry[:11]))
			first := entryCluster(entry)
			switch {
			case isDirectory(entry):
				walk(first, key, name)
			case entrySizeOf(entry) == 0:
				if first != 0 {
					t.Fatalf("check: %s: empty file owns cluster %d", name, first)
				}
			default:
				claim(name, first, int(fs.clustersFor(entrySizeOf(entry))))
			}
		}
	}
	walk(0, 0, "")
	for cluster := uint32(2); cluster < fs.clusters+2; cluster++ {
		if value, _ := fs.fat(cluster); value != 0 && !owned[cluster] && !interrupted {
			t.Fatalf("check: cluster %d (%#x) is used but owned by nothing", cluster, value)
		}
	}
	if fs.fatCopies > 1 && !interrupted {
		first := device.data[fs.fatStart : fs.fatStart+fs.fatSize]
		for index := uint64(1); index < uint64(fs.fatCopies); index++ {
			start := fs.fatStart + index*fs.fatSize
			if !bytes.Equal(first, device.data[start:start+fs.fatSize]) {
				t.Fatalf("check: FAT copy %d differs from the first", index)
			}
		}
	}
}

func pattern(size int, seed byte) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7) ^ seed ^ byte(i>>9)
	}
	return data
}

// writeChunks writes data at offset in pieces of chunk bytes, the way the
// VFS forwards a large write page by page.
func writeChunks(t testing.TB, fs *FileSystem, id ramfs.NodeID, offset uint64, data []byte, chunk int) {
	t.Helper()
	for done := 0; done < len(data); {
		end := done + chunk
		if end > len(data) {
			end = len(data)
		}
		count, status := fs.Write(id, offset+uint64(done), data[done:end])
		if status != ramfs.StatusOK || count != end-done {
			t.Fatalf("write at %d: %d %v", offset+uint64(done), count, status)
		}
		done = end
	}
}

func readAll(t testing.TB, fs *FileSystem, id ramfs.NodeID, size int, chunk int) []byte {
	t.Helper()
	data := make([]byte, size)
	for done := 0; done < size; {
		end := done + chunk
		if end > size {
			end = size
		}
		count, status := fs.Read(id, uint64(done), data[done:end])
		if status != ramfs.StatusOK || count != end-done {
			t.Fatalf("read at %d: %d %v", done, count, status)
		}
		done = end
	}
	return data
}

func TestFileSizesAcrossGeometries(t *testing.T) {
	sizes := []int{1, 4096, 64 * 1024, 64*1024 + 1, 1<<20 + 333}
	for _, geometry := range geometries {
		t.Run(geometry.name, func(t *testing.T) {
			device := formatted(t, 24<<20, geometry.layout)
			fs, root := mounted(t, device)
			for index, size := range sizes {
				name := fmt.Sprintf("file-%d.bin", index)
				id, status := fs.CreateName(root, mustName(t, name))
				if status != ramfs.StatusOK {
					t.Fatal(status)
				}
				data := pattern(size, byte(index))
				writeChunks(t, fs, id, 0, data, 4096)
				if got := readAll(t, fs, id, size, 4096); !bytes.Equal(got, data) {
					t.Fatalf("%s: content differs", name)
				}
			}
			check(t, device)
			reopened, root := mounted(t, device)
			for index, size := range sizes {
				id, status := reopened.OpenName(root, mustName(t, fmt.Sprintf("file-%d.bin", index)))
				if status != ramfs.StatusOK {
					t.Fatal(status)
				}
				if stat, _ := reopened.Stat(id); stat.Size != uint64(size) {
					t.Fatalf("size %d, want %d", stat.Size, size)
				}
				if got := readAll(t, reopened, id, size, 65536); !bytes.Equal(got, pattern(size, byte(index))) {
					t.Fatalf("file %d differs after remount", index)
				}
			}
		})
	}
}

// TestFileLargerThanSixteenMiB writes a file past 16 MiB in one request and
// reads it back in pages.
func TestFileLargerThanSixteenMiB(t *testing.T) {
	device := formatted(t, 40<<20, Layout{SectorsPerCluster: 8, FATs: 2})
	fs, root := mounted(t, device)
	id, _ := fs.CreateName(root, mustName(t, "big.bin"))
	size := 17<<20 + 5
	data := pattern(size, 0x5a)
	if count, status := fs.Write(id, 0, data); status != ramfs.StatusOK || count != size {
		t.Fatalf("write: %d %v", count, status)
	}
	if got := readAll(t, fs, id, size, 4096); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	check(t, device)
	if status := fs.UnlinkName(root, mustName(t, "big.bin")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	check(t, device)
}

// TestSequentialWriteCostIsLinear pins the position memory: appending page
// after page must not walk the chain from its first cluster each time, which
// would make device traffic grow with the square of the file size.
func TestSequentialWriteCostIsLinear(t *testing.T) {
	device := formatted(t, 16<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	id, _ := fs.CreateName(root, mustName(t, "stream.bin"))
	pages := 1024
	device.reads = 0
	writeChunks(t, fs, id, 0, pattern(pages*4096, 1), 4096)
	if device.reads > pages {
		t.Fatalf("appending %d pages issued %d device reads", pages, device.reads)
	}
	device.reads = 0
	readAll(t, fs, id, pages*4096, 4096)
	if device.reads > 3*pages {
		t.Fatalf("reading %d pages issued %d device reads", pages, device.reads)
	}
}

func TestLargeDirectories(t *testing.T) {
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	dir := mkdir(t, fs, root, "many")
	const files = 300
	for i := 0; i < files; i++ {
		writeFile(t, fs, dir, fmt.Sprintf("f%03d", i), fmt.Sprint(i))
	}
	parsed, _ := fs.directoryOf(dir)
	if parsed.clusters <= 4 {
		t.Fatalf("directory of %d files spans %d clusters", files, parsed.clusters)
	}
	names := listNames(t, fs, dir)
	if len(names) != files {
		t.Fatalf("listed %d of %d", len(names), files)
	}
	check(t, device)
	reopened, root := mounted(t, device)
	dir, _ = reopened.OpenName(root, mustName(t, "many"))
	for i := 0; i < files; i += 37 {
		id, status := reopened.OpenName(dir, mustName(t, fmt.Sprintf("f%03d", i)))
		if status != ramfs.StatusOK || readFile(t, reopened, id) != fmt.Sprint(i) {
			t.Fatalf("f%03d after remount: %v", i, status)
		}
	}
}

func TestManyLongNamesWithOnePrefix(t *testing.T) {
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 2, FATs: 2})
	fs, root := mounted(t, device)
	const files = 200
	for i := 0; i < files; i++ {
		writeFile(t, fs, root, fmt.Sprintf("Quarterly Report Draft %03d.document", i), fmt.Sprint(i))
	}
	aliases := map[string]bool{}
	for _, short := range shortNames(t, fs, 0) {
		if aliases[short] {
			t.Fatalf("alias %q used twice", short)
		}
		aliases[short] = true
	}
	if len(aliases) != files {
		t.Fatalf("%d aliases for %d files", len(aliases), files)
	}
	if !aliases["QUARTE~1DOC"] {
		t.Fatal("the first alias is not numbered")
	}
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("quarterly report draft %03d.DOCUMENT", i)
		id, status := fs.OpenName(root, mustName(t, name))
		if status != ramfs.StatusOK || readFile(t, fs, id) != fmt.Sprint(i) {
			t.Fatalf("%s: %v", name, status)
		}
	}
	check(t, device)
}

func TestDeepNesting(t *testing.T) {
	device := formatted(t, 4<<20, Layout{SectorsPerCluster: 1, FATs: 1})
	fs, root := mounted(t, device)
	parent := root
	for depth := 0; depth < 40; depth++ {
		parent = mkdir(t, fs, parent, fmt.Sprintf("level-%02d", depth))
	}
	id := writeFile(t, fs, parent, "bottom.txt", "deep")
	check(t, device)
	if readFile(t, fs, id) != "deep" {
		t.Fatal("bottom unreadable")
	}
	if status := fs.RenameName(mustOpen(t, fs, root, "level-00"), parent, mustName(t, "cycle")); status != ramfs.StatusInvalidArgument {
		t.Fatalf("move the top into the bottom: %v", status)
	}
}

func mustOpen(t *testing.T, fs *FileSystem, parent ramfs.NodeID, name string) ramfs.NodeID {
	t.Helper()
	id, status := fs.OpenName(parent, mustName(t, name))
	if status != ramfs.StatusOK {
		t.Fatalf("open %s: %v", name, status)
	}
	return id
}

func chainLength(t *testing.T, fs *FileSystem, id ramfs.NodeID) uint64 {
	t.Helper()
	first := entryClusterOf(t, fs, id)
	if first == 0 {
		return 0
	}
	_, count, status := fs.last(first)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	return count
}

func TestTruncate(t *testing.T) {
	for _, geometry := range geometries {
		t.Run(geometry.name, func(t *testing.T) {
			device := formatted(t, 8<<20, geometry.layout)
			fs, root := mounted(t, device)
			id, _ := fs.CreateName(root, mustName(t, "t.bin"))
			data := pattern(200000, 3)
			writeChunks(t, fs, id, 0, data, 65536)

			// Smaller but not empty: the chain ends after the last cluster
			// still needed and the prefix survives.
			if status := fs.Truncate(id, 70001); status != ramfs.StatusOK {
				t.Fatal(status)
			}
			if got := chainLength(t, fs, id); got != fs.clustersFor(70001) {
				t.Fatalf("chain of %d clusters after shrink", got)
			}
			check(t, device)
			if got := readAll(t, fs, id, 70001, 4096); !bytes.Equal(got, data[:70001]) {
				t.Fatal("prefix lost")
			}

			// Larger again: the grown range reads as zero even though the
			// kept cluster and any reused ones still hold old bytes.
			if status := fs.Truncate(id, 150000); status != ramfs.StatusOK {
				t.Fatal(status)
			}
			got := readAll(t, fs, id, 150000, 4096)
			if !bytes.Equal(got[:70001], data[:70001]) || !bytes.Equal(got[70001:], make([]byte, 150000-70001)) {
				t.Fatal("grown range is not zero")
			}
			check(t, device)

			// To zero: the whole chain goes and the entry names no cluster.
			if status := fs.Truncate(id, 0); status != ramfs.StatusOK {
				t.Fatal(status)
			}
			if entryClusterOf(t, fs, id) != 0 {
				t.Fatal("empty file kept a cluster")
			}
			check(t, device)

			// From empty to a size: allocated and zero.
			if status := fs.Truncate(id, 5000); status != ramfs.StatusOK {
				t.Fatal(status)
			}
			if got := readAll(t, fs, id, 5000, 4096); !bytes.Equal(got, make([]byte, 5000)) {
				t.Fatal("grown empty file is not zero")
			}
			check(t, device)
			if status := fs.Truncate(id, maxFileSize+1); status != ramfs.StatusResourceExhausted {
				t.Fatalf("truncate past the FAT32 limit: %v", status)
			}
		})
	}
}

// TestWritePastEndZeroesTheGap reuses clusters full of old bytes and writes
// past the end of a file; the gap must read as zero.
func TestWritePastEndZeroesTheGap(t *testing.T) {
	device := formatted(t, 4<<20, Layout{SectorsPerCluster: 4, FATs: 2})
	fs, root := mounted(t, device)
	stale := writeFile(t, fs, root, "stale", string(bytes.Repeat([]byte{'S'}, 60000)))
	_ = stale
	if status := fs.UnlinkName(root, mustName(t, "stale")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	id := writeFile(t, fs, root, "sparse", "head")
	if count, status := fs.Write(id, 50000, []byte("tail")); status != ramfs.StatusOK || count != 4 {
		t.Fatalf("write past end: %d %v", count, status)
	}
	got := readAll(t, fs, id, 50004, 4096)
	if string(got[:4]) != "head" || string(got[50000:]) != "tail" || !bytes.Equal(got[4:50000], make([]byte, 49996)) {
		t.Fatal("gap holds stale bytes")
	}
	// Truncating back inside a cluster and writing past the end again must
	// not expose the bytes the truncation left in that cluster.
	if status := fs.Truncate(id, 2); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if _, status := fs.Write(id, 40, []byte("x")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	got = readAll(t, fs, id, 41, 41)
	if string(got[:2]) != "he" || !bytes.Equal(got[2:40], make([]byte, 38)) || got[40] != 'x' {
		t.Fatalf("tail after shrink and grow: %q", got)
	}
	check(t, device)
}

func TestDiskFullRefusesWholeWrite(t *testing.T) {
	device := formatted(t, 1<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	id, _ := fs.CreateName(root, mustName(t, "fill"))
	written := 0
	chunk := pattern(64*1024, 9)
	for {
		count, status := fs.Write(id, uint64(written), chunk)
		if status == ramfs.StatusNoSpace {
			if count != 0 {
				t.Fatalf("a refused write reported %d bytes", count)
			}
			break
		}
		if status != ramfs.StatusOK {
			t.Fatal(status)
		}
		written += count
	}
	if stat, _ := fs.Stat(id); stat.Size != uint64(written) {
		t.Fatalf("size %d after a refused write, want %d", stat.Size, written)
	}
	check(t, device)
	if _, status := fs.CreateName(root, mustName(t, "more")); status != ramfs.StatusOK {
		t.Fatalf("an entry in an existing cluster: %v", status)
	}
	if _, status := fs.MkdirName(root, mustName(t, "dir")); status != ramfs.StatusNoSpace && status != ramfs.StatusOK {
		t.Fatalf("mkdir on a full volume: %v", status)
	}
	// Freeing space makes it usable again.
	if status := fs.Truncate(id, 0); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if _, status := fs.Write(id, 0, chunk); status != ramfs.StatusOK {
		t.Fatalf("write after freeing: %v", status)
	}
	check(t, device)
}

func TestDeleteLargeAndNestedContent(t *testing.T) {
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 8, FATs: 2})
	fs, root := mounted(t, device)
	outer := mkdir(t, fs, root, "outer")
	inner := mkdir(t, fs, outer, "inner")
	big, _ := fs.CreateName(inner, mustName(t, "big"))
	writeChunks(t, fs, big, 0, pattern(3<<20, 4), 1<<20)
	writeFile(t, fs, outer, "small", "s")
	if status := fs.UnlinkName(root, mustName(t, "outer")); status != ramfs.StatusNotEmpty {
		t.Fatalf("remove a non-empty directory: %v", status)
	}
	for _, step := range []struct {
		parent ramfs.NodeID
		name   string
	}{{inner, "big"}, {outer, "inner"}, {outer, "small"}, {root, "outer"}} {
		if status := fs.UnlinkName(step.parent, mustName(t, step.name)); status != ramfs.StatusOK {
			t.Fatalf("unlink %s: %v", step.name, status)
		}
		check(t, device)
	}
	for cluster := uint32(3); cluster < fs.clusters+2; cluster++ {
		if fatValue(fs, cluster) != 0 {
			t.Fatalf("cluster %d still used after everything was removed", cluster)
		}
	}
}

func TestUnmirroredFATUsesOnlyTheActiveCopy(t *testing.T) {
	device := formatted(t, 4<<20, Layout{SectorsPerCluster: 1, FATs: 2, NoMirror: true, ActiveFAT: 1})
	fs, root := mounted(t, device)
	first := append([]byte(nil), device.data[fs.fatStart:fs.fatStart+fs.fatSize]...)
	id := writeFile(t, fs, root, "a.bin", string(pattern(5000, 1)))
	if !bytes.Equal(first, device.data[fs.fatStart:fs.fatStart+fs.fatSize]) {
		t.Fatal("the inactive FAT was written")
	}
	cluster := entryClusterOf(t, fs, id)
	if value := get32(device.data[fs.fatStart+fs.fatSize+uint64(cluster)*4:]); value == 0 {
		t.Fatal("the active FAT was not written")
	}
}

func TestFATEntryKeepsReservedBits(t *testing.T) {
	device := formatted(t, 4<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	for index := uint64(0); index < 2; index++ {
		put32(device.data[fs.fatStart+index*fs.fatSize+10*4:], 0xa0000000)
	}
	var reopened FileSystem
	reopened.Mount(device)
	reopened.hint = 10
	id := writeFile(t, &reopened, root, "x", "y")
	if entryClusterOf(t, &reopened, id) != 10 {
		t.Fatal("cluster 10 not taken")
	}
	if value := get32(device.data[fs.fatStart+10*4:]); value != 0xa0000000|endOfChainMark {
		t.Fatalf("entry %#x lost its reserved bits", value)
	}
}

func TestFSInfoHintIsUsedAndRefreshed(t *testing.T) {
	device := formatted(t, 4<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	info := device.data[512:1024]
	put32(info[fsInfoNextFree:], 100)
	fs, root := mounted(t, device)
	id := writeFile(t, fs, root, "hinted", "x")
	if cluster := entryClusterOf(t, fs, id); cluster != 100 {
		t.Fatalf("allocation started at %d, not the FSInfo hint", cluster)
	}
	if get32(info[fsInfoNextFree:]) != 101 || get32(info[fsInfoFreeCount:]) != fsInfoUnknown {
		t.Fatalf("FSInfo after a change: next %d count %#x", get32(info[fsInfoNextFree:]), get32(info[fsInfoFreeCount:]))
	}
	// A hint past the volume, or a sector without signatures, is ignored.
	put32(info[fsInfoNextFree:], 1<<27)
	fs, root = mounted(t, device)
	if fs.hint != 2 {
		t.Fatalf("hint %d from an impossible FSInfo value", fs.hint)
	}
	put32(info[0:4], 0)
	put32(info[fsInfoNextFree:], 200)
	if fs, _ = mounted(t, device); fs.fsInfo != 0 || fs.hint != 2 {
		t.Fatal("an FSInfo sector without signatures was trusted")
	}
}

func TestStaleDirectoryNodeIsNotFound(t *testing.T) {
	device := formatted(t, 4<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	dir := mkdir(t, fs, root, "gone")
	child := writeFile(t, fs, dir, "child", "c")
	fs.UnlinkName(dir, mustName(t, "child"))
	fs.UnlinkName(root, mustName(t, "gone"))
	// The directory's cluster now holds file data that looks like entries.
	fs.hint = uint32(uint64(child) >> slotBits)
	bogus := bytes.Repeat([]byte("A"), 512)
	writeFile(t, fs, root, "data", string(bogus))
	if _, status := fs.Stat(child); status != ramfs.StatusNotFound {
		t.Fatalf("stale node: %v", status)
	}
	if _, status := fs.Write(child, 0, []byte("x")); status != ramfs.StatusNotFound {
		t.Fatalf("write through a stale node: %v", status)
	}
	check(t, device)
}

func TestCaseOnlyRename(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	id := writeFile(t, fs, root, "readme.txt", "r")
	if status := fs.RenameName(id, root, mustName(t, "README.txt")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if names := listNames(t, fs, root); len(names) != 1 || names[0] != "README.txt" {
		t.Fatalf("listing %v", names)
	}
}

func TestCorruptChains(t *testing.T) {
	device := formatted(t, 4<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	id := writeFile(t, fs, root, "chain", string(pattern(4000, 2)))
	first := entryClusterOf(t, fs, id)
	second, _, _ := fs.next(first)
	third, _, _ := fs.next(second)

	// A link to a free cluster ends the file early.
	fs.setFAT(second, 0)
	if _, status := fs.Read(id, 0, make([]byte, 4000)); status != ramfs.StatusCorrupt {
		t.Fatalf("free link: %v", status)
	}
	// A link to a bad cluster.
	fs.setFAT(second, badCluster)
	if _, status := fs.Read(id, 0, make([]byte, 4000)); status != ramfs.StatusCorrupt {
		t.Fatalf("bad-cluster link: %v", status)
	}
	// A loop is bounded: growing the file must walk to its end and cannot.
	fs.setFAT(second, first)
	fs.forgetPositions()
	if status := fs.Truncate(id, 100000); status != ramfs.StatusCorrupt {
		t.Fatalf("grow a looped chain: %v", status)
	}
	if _, status := fs.Write(id, 3990, make([]byte, 5000)); status != ramfs.StatusCorrupt {
		t.Fatalf("append to a looped chain: %v", status)
	}
	// A file whose size needs more clusters than its chain has.
	fs.setFAT(second, third)
	fs.setFAT(third, endOfChainMark)
	fs.forgetPositions()
	_, index, entry, _ := fs.node(id)
	dir, _ := fs.openDirectory(0)
	put32(entry[28:32], 900000)
	fs.writeEntry(&dir, index, entry)
	if _, status := fs.Read(id, 800000, make([]byte, 10)); status != ramfs.StatusCorrupt {
		t.Fatalf("size beyond the chain: %v", status)
	}
	// None of it may reach outside the volume or panic; removal still works.
	if status := fs.UnlinkName(root, mustName(t, "chain")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
}

// model is what the filesystem should hold: one flat map of path to content
// across a few directories.
type model map[string][]byte

// TestRandomOperationsAgainstModel runs a seeded sequence of creates, writes,
// truncations, renames, moves, and removals, compares every file with a model,
// and checks the volume's consistency as it goes. GOOS_STORAGE_STRESS=1
// raises the operation count for make test-storage-stress.
func TestRandomOperationsAgainstModel(t *testing.T) {
	operations, seeds := 600, []int64{1, 2}
	if os.Getenv("GOOS_STORAGE_STRESS") == "1" {
		operations, seeds = 20000, []int64{1, 2, 3, 4, 5, 6, 7, 8}
	}
	for _, seed := range seeds {
		for _, geometry := range geometries[:2] {
			t.Run(fmt.Sprintf("%s-seed-%d", geometry.name, seed), func(t *testing.T) {
				randomOperations(t, seed, operations, geometry.layout)
			})
		}
	}
}

func randomOperations(t *testing.T, seed int64, operations int, layout Layout) {
	random := rand.New(rand.NewSource(seed))
	done := 0
	defer func() { t.Logf("%d of %d operations changed the volume", done, operations) }()
	device := formatted(t, 6<<20, layout)
	fs, root := mounted(t, device)
	dirs := map[string]ramfs.NodeID{"": root}
	for _, name := range []string{"alpha", "beta", "Long Directory Name"} {
		dirs[name] = mkdir(t, fs, root, name)
	}
	dirNames := []string{"", "alpha", "beta", "Long Directory Name"}
	files := model{}
	names := []string{"a", "b.txt", "c.dat", "A Long File Name.txt", "another-long-name-x", "z"}
	pick := func() (string, string) {
		return dirNames[random.Intn(len(dirNames))], names[random.Intn(len(names))]
	}
	open := func(dir, name string) (ramfs.NodeID, ramfs.Status) {
		return fs.OpenName(dirs[dir], mustName(t, name))
	}
	for step := 0; step < operations; step++ {
		dir, name := pick()
		path := dir + "/" + strings.ToLower(name)
		content, exists := files[path]
		switch op := random.Intn(6); op {
		case 0:
			_, status := fs.CreateName(dirs[dir], mustName(t, name))
			if exists != (status == ramfs.StatusAlreadyExists) || !exists && status != ramfs.StatusOK {
				t.Fatalf("step %d create %s: %v", step, path, status)
			}
			if !exists {
				files[path] = []byte{}
			}
		case 1, 2:
			if !exists {
				continue
			}
			id, status := open(dir, name)
			if status != ramfs.StatusOK {
				t.Fatalf("step %d open %s: %v", step, path, status)
			}
			offset := random.Intn(len(content) + 20000)
			data := pattern(1+random.Intn(40000), byte(step))
			count, status := fs.Write(id, uint64(offset), data)
			if status == ramfs.StatusNoSpace {
				continue
			}
			if status != ramfs.StatusOK || count != len(data) {
				t.Fatalf("step %d write %s: %d %v", step, path, count, status)
			}
			if end := offset + len(data); end > len(content) {
				content = append(content, make([]byte, end-len(content))...)
			}
			copy(content[offset:], data)
			files[path] = content
		case 3:
			if !exists {
				continue
			}
			id, _ := open(dir, name)
			size := random.Intn(len(content) + 30000)
			status := fs.Truncate(id, uint64(size))
			if status == ramfs.StatusNoSpace {
				continue
			}
			if status != ramfs.StatusOK {
				t.Fatalf("step %d truncate %s: %v", step, path, status)
			}
			if size <= len(content) {
				content = content[:size]
			} else {
				content = append(content, make([]byte, size-len(content))...)
			}
			files[path] = content
		case 4:
			status := fs.UnlinkName(dirs[dir], mustName(t, name))
			if exists != (status == ramfs.StatusOK) {
				t.Fatalf("step %d unlink %s: %v", step, path, status)
			}
			delete(files, path)
		case 5:
			if !exists {
				continue
			}
			toDir, toName := pick()
			toPath := toDir + "/" + strings.ToLower(toName)
			id, _ := open(dir, name)
			status := fs.RenameName(id, dirs[toDir], mustName(t, toName))
			_, taken := files[toPath]
			switch {
			case toPath == path:
				if status != ramfs.StatusOK {
					t.Fatalf("step %d rename in place %s: %v", step, path, status)
				}
			case taken:
				if status != ramfs.StatusAlreadyExists {
					t.Fatalf("step %d rename onto %s: %v", step, toPath, status)
				}
			case status == ramfs.StatusNoSpace:
			case status != ramfs.StatusOK:
				t.Fatalf("step %d rename %s to %s: %v", step, path, toPath, status)
			default:
				delete(files, path)
				files[toPath] = content
			}
		}
		done++
		if step%97 == 0 || step == operations-1 {
			verifyModel(t, fs, dirs, files)
			check(t, device)
		}
	}
	reopened, root := mounted(t, device)
	dirs[""] = root
	for _, name := range dirNames[1:] {
		dirs[name], _ = reopened.OpenName(root, mustName(t, name))
	}
	verifyModel(t, reopened, dirs, files)
}

func verifyModel(t *testing.T, fs *FileSystem, dirs map[string]ramfs.NodeID, files model) {
	t.Helper()
	count := 0
	for dir, id := range dirs {
		for _, listed := range listNames(t, fs, id) {
			path := dir + "/" + strings.ToLower(listed)
			if _, isDir := dirs[listed]; dir == "" && isDir {
				continue
			}
			content, ok := files[path]
			if !ok {
				t.Fatalf("%s is listed but should not exist", path)
			}
			node, status := fs.OpenName(id, mustName(t, listed))
			if status != ramfs.StatusOK {
				t.Fatalf("open %s: %v", path, status)
			}
			if stat, _ := fs.Stat(node); stat.Size != uint64(len(content)) {
				t.Fatalf("%s: size %d, want %d", path, stat.Size, len(content))
			}
			if got := readAll(t, fs, node, len(content), 8192); !bytes.Equal(got, content) {
				t.Fatalf("%s: content differs", path)
			}
			count++
		}
	}
	if count != len(files) {
		t.Fatalf("listed %d files, model has %d", count, len(files))
	}
}

// TestDeviceFailureIsContained fails the device at every point of a series of
// operations in turn. Each operation must end in a typed status, never a
// panic, and whatever the interrupted change left must still be a volume
// with no cross-linked or looped chain.
func TestDeviceFailureIsContained(t *testing.T) {
	for failAt := 1; failAt < 400; failAt += 3 {
		device := formatted(t, 2<<20, Layout{SectorsPerCluster: 2, FATs: 2})
		fs, root := mounted(t, device)
		device.writes, device.failAt = 0, failAt
		dir, status := fs.MkdirName(root, mustName(t, "Some Directory"))
		if status == ramfs.StatusOK {
			id, status := fs.CreateName(dir, mustName(t, "a long file name.bin"))
			if status == ramfs.StatusOK {
				fs.Write(id, 0, pattern(20000, 1))
				fs.Truncate(id, 3000)
				fs.Write(id, 9000, pattern(9000, 2))
				fs.RenameName(id, root, mustName(t, "moved"))
			}
			fs.UnlinkName(root, mustName(t, "moved"))
		}
		for _, status := range []ramfs.Status{status} {
			if status > ramfs.LastStatus {
				t.Fatalf("status %d", status)
			}
		}
		device.failAt = 0
		checkVolume(t, device, true)
	}
}

// TestExternalFsckAcceptsVolume has another implementation judge a volume
// this one built: macOS fsck_msdos or dosfstools fsck.fat, whichever the host
// has, run read-only over an image holding nested directories, long names,
// large and truncated files, moves, and removals.
func TestExternalFsckAcceptsVolume(t *testing.T) {
	var command []string
	for _, candidate := range [][]string{{"/sbin/fsck_msdos", "-n"}, {"fsck.fat", "-n", "-V"}} {
		if path, err := exec.LookPath(candidate[0]); err == nil {
			command = append([]string{path}, candidate[1:]...)
			break
		}
	}
	if command == nil {
		t.Skip("no FAT checker on this host")
	}
	for _, geometry := range geometries[:3] {
		t.Run(geometry.name, func(t *testing.T) {
			device := formatted(t, 40<<20, geometry.layout)
			fs, root := mounted(t, device)
			outer := mkdir(t, fs, root, "Outer Directory")
			inner := mkdir(t, fs, outer, "inner")
			for i := 0; i < 120; i++ {
				writeFile(t, fs, inner, fmt.Sprintf("Long File Name Number %03d.txt", i), fmt.Sprint(i))
			}
			big, _ := fs.CreateName(outer, mustName(t, "big.bin"))
			writeChunks(t, fs, big, 0, pattern(3<<20, 1), 1<<20)
			fs.Truncate(big, 1<<20+17)
			fs.Write(big, 2<<20, []byte("tail"))
			for i := 0; i < 120; i += 3 {
				fs.UnlinkName(inner, mustName(t, fmt.Sprintf("Long File Name Number %03d.txt", i)))
			}
			moved := mkdir(t, fs, root, "moved")
			fs.RenameName(inner, moved, mustName(t, "inner renamed"))
			fs.RenameName(big, root, mustName(t, "BIG.BIN"))
			check(t, device)
			image := t.TempDir() + "/volume.img"
			if err := os.WriteFile(image, device.data, 0o644); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command(command[0], append(command[1:], image)...).CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", command[0], err, output)
			}
		})
	}
}
