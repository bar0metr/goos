package ext4

import "testing"

// inodeBlock returns the inode-table block that holds inode.
func inodeBlock(t *testing.T, fs *FS, inode *Inode) uint64 {
	t.Helper()
	group, index, err := fs.inodeLocation(inode.Number)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := fs.group(group)
	if err != nil {
		t.Fatal(err)
	}
	return desc.InodeTable + uint64(index)*uint64(fs.sb.InodeSize)/uint64(fs.blockSize)
}

// primeBlock fetches block into the prefetch and requires it to be resident,
// so the test proves a real prefetch hit, not a staged or device read.
func primeBlock(t *testing.T, fs *FS, block uint64) {
	t.Helper()
	if _, err := fs.prefetchBlock(&fs.list, block); err != nil {
		t.Fatal(err)
	}
	first := int(block%prefetchSets) * prefetchWays
	for index := 0; index < prefetchWays; index++ {
		entry := &fs.list.blocks[first+index]
		if entry.valid && entry.block == block {
			return
		}
	}
	t.Fatalf("block %d did not become resident in the prefetch", block)
}

// prefetchFind runs PrefetchDir over dir and returns the decoded inode of the
// first entry named name, and whether it was found.
func prefetchFind(t *testing.T, fs *FS, dir *Inode, name string) (Inode, bool) {
	t.Helper()
	var entries [MaxReaddirPrefetchEntries]DirEntry
	var inodes [MaxReaddirPrefetchEntries]Inode
	cursor := uint64(0)
	for {
		count, next, end, err := fs.PrefetchDir(&fs.list, dir, cursor, &entries, &inodes)
		if err != nil {
			t.Fatal(err)
		}
		for index := 0; index < count; index++ {
			if string(entries[index].Name()) == name {
				return inodes[index], true
			}
		}
		if end {
			return Inode{}, false
		}
		cursor = next
	}
}

// TestPrefetchSeesPendingInodeMutation requires a pending metadata change to
// win over an older resident prefetch copy of the same inode-table block
// (ADR 0231).
func TestPrefetchSeesPendingInodeMutation(t *testing.T) {
	fs, _ := scratch(t, "canonical")
	dir := resolve(t, fs, "/")
	file := lookupInode(t, fs, dir, "hello.txt")
	block := inodeBlock(t, fs, file)

	primeBlock(t, fs, block)
	if before, ok := prefetchFind(t, fs, dir, "hello.txt"); !ok || before.Size == 12345 {
		t.Fatalf("pre-listing: found=%v size=%d", ok, before.Size)
	}

	if err := fs.Truncate(file, 12345); err != nil {
		t.Fatal(err)
	}
	after, ok := prefetchFind(t, fs, dir, "hello.txt")
	if !ok {
		t.Fatal("hello.txt not listed after truncate")
	}
	if after.Size != 12345 {
		t.Fatalf("prefetch saw size %d after pending truncate, want 12345", after.Size)
	}
}

// TestPrefetchSeesPendingCreatedInode requires a newly allocated inode, still
// pending, to be listed with its staged metadata rather than a stale resident
// prefetch copy of a freed slot (ADR 0231).
func TestPrefetchSeesPendingCreatedInode(t *testing.T) {
	fs, _ := scratch(t, "canonical")
	dir := resolve(t, fs, "/big")

	// The next free inode's block is still on the device; prime the
	// prefetch with it before the create stages a new copy.
	block := nextFreeInodeBlock(t, fs)
	primeBlock(t, fs, block)

	created, err := fs.Create(dir, []byte("newfile"), ModeRegular|0o640, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if inodeBlock(t, fs, created) != block {
		t.Fatalf("newfile inode %d landed in a different block", created.Number)
	}
	listed, ok := prefetchFind(t, fs, dir, "newfile")
	if !ok {
		t.Fatal("newfile not listed")
	}
	if listed.Size != 0 || listed.Links != 1 || listed.Mode&0xFFF != 0o640 {
		t.Fatalf("newfile listed with stale metadata: size=%d links=%d mode=%#o", listed.Size, listed.Links, listed.Mode)
	}
}

// nextFreeInodeBlock scans the inode bitmap for the first free inode and
// returns its inode-table block.
func nextFreeInodeBlock(t *testing.T, fs *FS) uint64 {
	t.Helper()
	for number := uint32(1); ; number++ {
		allocated, err := fs.inodeAllocated(number)
		if err != nil {
			t.Fatal(err)
		}
		if allocated {
			continue
		}
		group, index, err := fs.inodeLocation(number)
		if err != nil {
			t.Fatal(err)
		}
		desc, err := fs.group(group)
		if err != nil {
			t.Fatal(err)
		}
		return desc.InodeTable + uint64(index)*uint64(fs.sb.InodeSize)/uint64(fs.blockSize)
	}
}

// TestPrefetchedInodeDoesNotRetainEvictableRawAlias forces a set eviction and
// requires every prefetched inode to carry scalar fields only, with no raw
// alias into a cache slot a later block can overwrite (ADR 0231).
func TestPrefetchedInodeDoesNotRetainEvictableRawAlias(t *testing.T) {
	fs := mount(t, fixture(t, "many"))
	dir := resolve(t, fs, "/many")

	// Five blocks in one set evict the first of the four ways, so a raw
	// alias into the prefetch would be overwritten within one listing.
	for k := uint64(0); k < prefetchWays+1; k++ {
		if _, err := fs.prefetchBlock(&fs.list, k*prefetchSets); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < prefetchWays; index++ {
		if entry := &fs.list.blocks[index]; entry.valid && entry.block == 0 {
			t.Fatal("first block of the set was not evicted")
		}
	}

	var entries [MaxReaddirPrefetchEntries]DirEntry
	var inodes [MaxReaddirPrefetchEntries]Inode
	cursor := uint64(0)
	for {
		count, next, end, err := fs.PrefetchDir(&fs.list, dir, cursor, &entries, &inodes)
		if err != nil {
			t.Fatal(err)
		}
		for index := 0; index < count; index++ {
			if inodes[index].raw != nil {
				t.Fatalf("prefetched inode %d retains a raw alias", inodes[index].Number)
			}
		}
		if end {
			break
		}
		cursor = next
	}
}
