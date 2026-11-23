package ext4

import (
	"sort"
	"testing"
)

// prefetchAll walks dir through PrefetchDir and returns the entries with
// their decoded inodes in cursor order.
func prefetchAll(t *testing.T, fs *FS, dir *Inode) []prefetchedEntry {
	t.Helper()
	var entries [MaxReaddirPrefetchEntries]DirEntry
	var inodes [MaxReaddirPrefetchEntries]Inode
	var result []prefetchedEntry
	cursor := uint64(0)
	for {
		count, next, end, err := fs.PrefetchDir(&fs.list, dir, cursor, &entries, &inodes)
		if err != nil {
			t.Fatal(err)
		}
		for index := 0; index < count; index++ {
			result = append(result, prefetchedEntry{name: string(entries[index].Name()), inode: entries[index].Inode, size: inodes[index].Size, mode: inodes[index].Mode, links: inodes[index].Links, device: inodes[index].DeviceNumber()})
		}
		if end {
			return result
		}
		cursor = next
	}
}

type prefetchedEntry struct {
	name   string
	inode  uint32
	size   uint64
	mode   uint16
	links  uint16
	device uint32
}

// interleavedAll walks dir the way the per-entry path does.
func interleavedAll(t *testing.T, fs *FS, dir *Inode) []prefetchedEntry {
	t.Helper()
	var result []prefetchedEntry
	if _, err := fs.ReadDir(dir, 0, func(entry *DirEntry) bool {
		inode, err := fs.Inode(entry.Inode)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, prefetchedEntry{name: string(entry.Name()), inode: entry.Inode, size: inode.Size, mode: inode.Mode, links: inode.Links, device: inode.DeviceNumber()})
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestPrefetchMatchesPerEntryListing lists the 10000-name indexed directory
// through both paths and requires the same entries in the same order: the
// prefetch must not change cursor order, inode identity, sizes, modes, link
// counts or device numbers (ADR 0231).
func TestPrefetchMatchesPerEntryListing(t *testing.T) {
	fs := mount(t, fixture(t, "many"))
	dir := resolve(t, fs, "/many")
	if !fs.isIndexed(dir) {
		t.Fatal("many is not indexed")
	}
	want := interleavedAll(t, fs, dir)
	got := prefetchAll(t, fs, dir)
	if len(got) != len(want) {
		t.Fatalf("prefetch listed %d entries, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("entry %d differs:\n got %+v\nwant %+v", index, got[index], want[index])
		}
	}
}

// TestPrefetchListsEveryNameOnce requires the prefetch to cover the whole
// directory with no duplicates.
func TestPrefetchListsEveryNameOnce(t *testing.T) {
	fs := mount(t, fixture(t, "many"))
	dir := resolve(t, fs, "/many")
	seen := map[string]bool{}
	for _, entry := range prefetchAll(t, fs, dir) {
		if seen[entry.name] {
			t.Fatalf("duplicate name %s", entry.name)
		}
		seen[entry.name] = true
	}
	if len(seen) != 10000 {
		t.Fatalf("listed %d distinct names, want 10000", len(seen))
	}
	for index := 0; index < 10000; index++ {
		name := "f" + string(rune('0'+index/10000)) + string(rune('0'+index/1000%10)) + string(rune('0'+index/100%10)) + string(rune('0'+index/10%10)) + string(rune('0'+index%10))
		if !seen[name] {
			t.Fatalf("missing %s", name)
		}
	}
}

// TestPrefetchListingReads proves the prefetch trades device reads for
// buffering: the interleaved per-entry walk reads the scattered inode table
// repeatedly, the prefetch reads each block once (ADR 0231).
func TestPrefetchListingReads(t *testing.T) {
	device := fixture(t, "many")
	fs := mount(t, device)
	dir := resolve(t, fs, "/many")
	interleavedAll(t, fs, dir)
	interleavedReads := device.reads
	device.reads = 0
	prefetchAll(t, fs, dir)
	prefetchReads := device.reads
	if interleavedReads < 4*prefetchReads {
		t.Fatalf("interleaved %d reads vs prefetch %d: the prefetch did not reduce device reads", interleavedReads, prefetchReads)
	}
	t.Logf("interleaved %d reads, prefetch %d reads", interleavedReads, prefetchReads)
}

// TestPrefetchPreservesHardLinksAndSpecialFiles lists the links fixture
// through both paths: three names of one inode and a FIFO and device nodes
// must keep their identity, link count and device number.
func TestPrefetchPreservesHardLinksAndSpecialFiles(t *testing.T) {
	fs := mount(t, fixture(t, "links"))
	want := interleavedAll(t, fs, resolve(t, fs, "/"))
	got := prefetchAll(t, fs, resolve(t, fs, "/"))
	sort.Slice(want, func(a, b int) bool { return want[a].name < want[b].name })
	sort.Slice(got, func(a, b int) bool { return got[a].name < got[b].name })
	if len(got) != len(want) {
		t.Fatalf("prefetch listed %d entries, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("entry %q differs:\n got %+v\nwant %+v", want[index].name, got[index], want[index])
		}
	}
}
