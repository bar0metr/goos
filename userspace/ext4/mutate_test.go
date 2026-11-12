package ext4

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// scratchDevice is a writable volume in memory that can fail a chosen read
// or write.
type scratchDevice struct {
	data      []byte
	reads     int
	writes    int
	flushes   int
	failRead  int
	failWrite int
}

func (device *scratchDevice) ReadAt(offset uint64, target []byte) bool {
	device.reads++
	if device.failRead != 0 && device.reads == device.failRead {
		return false
	}
	end := offset + uint64(len(target))
	if end < offset || end > uint64(len(device.data)) {
		return false
	}
	copy(target, device.data[offset:end])
	return true
}

func (device *scratchDevice) WriteAt(offset uint64, data []byte) bool {
	device.writes++
	if device.failWrite != 0 && device.writes == device.failWrite {
		return false
	}
	end := offset + uint64(len(data))
	if end < offset || end > uint64(len(device.data)) {
		return false
	}
	copy(device.data[offset:end], data)
	return true
}

func (device *scratchDevice) Flush() bool      { device.flushes++; return true }
func (device *scratchDevice) Capacity() uint64 { return uint64(len(device.data)) }

// mountScratch mounts a disposable volume for mutation.
func mountScratch(device Device, now func() Timestamp) (*FS, error) {
	return Mount(device, Options{Writable: true, Clock: now})
}

// settled returns device as the next writable mount would leave it after
// a clean unmount: a copy, recovered when the volume is still mounted or
// dirty, so read-only checks can open it.
func settled(device Device) (Device, error) {
	scratchCopy, isScratch := device.(*scratchDevice)
	if !isScratch {
		return device, nil
	}
	raw := scratchCopy.data[superblockOffset : superblockOffset+superblockSize]
	if le32(raw, 0x60)&incompatRecover == 0 && le16(raw, 0x3A)&stateValid != 0 {
		return device, nil
	}
	copied := &scratchDevice{data: append([]byte(nil), scratchCopy.data...)}
	fs, err := mountScratch(copied, testClock())
	if err != nil {
		return nil, fmt.Errorf("recovery: %w", err)
	}
	if err := fs.Unmount(); err != nil {
		return nil, fmt.Errorf("unmount after recovery: %w", err)
	}
	return copied, nil
}

func testClock() func() Timestamp {
	nanos := uint32(0)
	return func() Timestamp {
		nanos++
		return Timestamp{Seconds: 1800000000, Nanos: nanos}
	}
}

func scratch(t testing.TB, name string) (*FS, *scratchDevice) {
	t.Helper()
	device := &scratchDevice{data: fixture(t, name).data}
	fs, err := mountScratch(device, testClock())
	if err != nil {
		t.Fatalf("scratch mount: %v", err)
	}
	return fs, device
}

// ok unwraps a result whose error a test does not expect; a failure
// panics with the error, which fails the test with its stack.
func ok[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func wantKind(t testing.TB, err error, want Kind) {
	t.Helper()
	if KindOf(err) != want {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func root(t testing.TB, fs *FS) *Inode {
	t.Helper()
	inode, err := fs.Root()
	if err != nil {
		t.Fatal(err)
	}
	return inode
}

func lookupInode(t testing.TB, fs *FS, dir *Inode, name string) *Inode {
	t.Helper()
	number, _, err := fs.Lookup(dir, []byte(name))
	if err != nil {
		t.Fatalf("lookup %q: %v", name, err)
	}
	return ok(fs.Inode(number))
}

func TestMutationNeedsWritableMount(t *testing.T) {
	device := fixture(t, "canonical")
	fs := mount(t, device)
	_, err := fs.Create(root(t, fs), []byte("x"), ModeRegular|0o644, 0, 0)
	wantKind(t, err, KindReadOnly)
	_, err = fs.WriteFile(resolve(t, fs, "/hello.txt"), 0, []byte("x"))
	wantKind(t, err, KindReadOnly)
	wantKind(t, fs.Pin(root(t, fs)), KindReadOnly)
	_, err = Mount(&scratchDevice{data: device.data}, Options{Writable: true})
	wantKind(t, err, KindInvalid)
	for _, name := range []string{"legacy", "blockmap-1k"} {
		if _, err := mountScratch(&scratchDevice{data: fixture(t, name).data}, testClock()); err == nil {
			t.Fatalf("%s mounted for mutation outside the writable profile", name)
		}
	}
}

// TestScratchMountChecksCounts refuses a checksum-consistent volume whose
// descriptor counts disagree with its bitmaps: the allocators trust the
// counts.
func TestScratchMountChecksCounts(t *testing.T) {
	device := fixture(t, "canonical")
	setStrayBitmapBit(t, device)
	mount(t, device)
	_, err := mountScratch(&scratchDevice{data: device.data}, testClock())
	wantKind(t, err, KindCorrupt)
}

// TestScratchMountChecksInodePadding refuses a volume whose inode bitmap
// has a clear bit past the group's inodes. The bitmap checksum covers only
// the group's inodes, so the read-only mount still accepts it.
func TestScratchMountChecksInodePadding(t *testing.T) {
	device := fixture(t, "canonical")
	fs := mount(t, device)
	desc, err := fs.group(0)
	if err != nil {
		t.Fatal(err)
	}
	bitmap := device.data[desc.InodeBitmap*uint64(fs.blockSize):]
	clearBit(bitmap, uint64(fs.sb.InodesPerGroup))
	desc.InodeBitmapCsum = crc32c(fs.seed, bitmap[:fs.sb.InodesPerGroup/8])
	block, within := fs.descriptorLocation(0)
	raw := device.data[block*uint64(fs.blockSize)+uint64(within):]
	putLE16(raw[0x1A:], uint16(desc.InodeBitmapCsum))
	putLE16(raw[0x3A:], uint16(desc.InodeBitmapCsum>>16))
	crc, _ := fs.descriptorChecksum(0, raw[:fs.sb.DescriptorSize])
	putLE16(raw[0x1E:], crc)
	mount(t, device)
	_, err = mountScratch(&scratchDevice{data: device.data}, testClock())
	wantKind(t, err, KindCorrupt)
}

func TestCreateWriteRead(t *testing.T) {
	fs, device := scratch(t, "canonical")
	before := fs.Info()
	file := ok(fs.Create(root(t, fs), []byte("new.txt"), ModeRegular|0o640, 1000, 1001))
	payload := bytes.Repeat([]byte("ext4 mutation "), 1000)
	if n, err := fs.WriteFile(file, 0, payload); err != nil || n != len(payload) {
		t.Fatalf("write %d %v", n, err)
	}
	checkVolume(t, device)
	reread := mount(t, device)
	got := lookupInode(t, reread, root(t, reread), "new.txt")
	if got.UID != 1000 || got.GID != 1001 || got.Permissions() != 0o640 || got.Links != 1 {
		t.Fatalf("metadata %+v", got)
	}
	if !bytes.Equal(readAll(t, reread, got), payload) {
		t.Fatal("content differs")
	}
	used := before.FreeBlocks - reread.Info().FreeBlocks
	if want := uint64(len(payload)+4095) / 4096; used != want {
		t.Fatalf("used %d blocks, want %d", used, want)
	}
	if reread.Info().FreeInodes != before.FreeInodes-1 {
		t.Fatal("inode count")
	}
}

func TestMkdirRmdirRestoresCounts(t *testing.T) {
	fs, device := scratch(t, "canonical")
	before := fs.Info()
	top := root(t, fs)
	dir := ok(fs.Mkdir(top, []byte("d"), 0o755, 0, 0))
	sub := ok(fs.Mkdir(dir, []byte("sub"), 0o700, 0, 0))
	ok(fs.Create(sub, []byte("f"), ModeRegular|0o644, 0, 0))
	checkVolume(t, device)
	wantKind(t, fs.Rmdir(root(t, fs), []byte("d")), KindNotEmpty)
	wantKind(t, fs.Unlink(root(t, fs), []byte("d")), KindIsDirectory)
	sub = lookupInode(t, fs, lookupInode(t, fs, root(t, fs), "d"), "sub")
	if err := fs.Unlink(sub, []byte("f")); err != nil {
		t.Fatal(err)
	}
	dir = lookupInode(t, fs, root(t, fs), "d")
	if err := fs.Rmdir(dir, []byte("sub")); err != nil {
		t.Fatal(err)
	}
	if err := fs.Rmdir(root(t, fs), []byte("d")); err != nil {
		t.Fatal(err)
	}
	checkVolume(t, device)
	after := mount(t, device).Info()
	if after.FreeBlocks != before.FreeBlocks || after.FreeInodes != before.FreeInodes {
		t.Fatalf("free %d/%d, was %d/%d", after.FreeBlocks, after.FreeInodes, before.FreeBlocks, before.FreeInodes)
	}
	if links := root(t, mount(t, device)).Links; links != top.Links {
		t.Fatalf("root links %d, was %d", links, top.Links)
	}
}

func TestSymlinks(t *testing.T) {
	fs, device := scratch(t, "canonical")
	short := []byte("hello.txt")
	long := bytes.Repeat([]byte("a/"), 300)
	fast := ok(fs.Symlink(root(t, fs), []byte("short"), short, 0, 0))
	slow := ok(fs.Symlink(root(t, fs), []byte("long"), long, 0, 0))
	if !fs.fastSymlink(fast) || fs.fastSymlink(slow) {
		t.Fatal("symlink storage")
	}
	checkVolume(t, device)
	reread := mount(t, device)
	for name, want := range map[string][]byte{"short": short, "long": long} {
		got := ok(reread.Readlink(lookupInode(t, reread, root(t, reread), name)))
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: %q", name, got)
		}
	}
	for _, name := range []string{"short", "long", "sym-fast", "sym-slow"} {
		if err := fs.Unlink(root(t, fs), []byte(name)); err != nil {
			t.Fatalf("unlink %s: %v", name, err)
		}
	}
	checkVolume(t, device)
}

func TestHardLinks(t *testing.T) {
	fs, device := scratch(t, "canonical")
	hello := resolve(t, fs, "/hello.txt")
	dir := ok(fs.Mkdir(root(t, fs), []byte("d"), 0o755, 0, 0))
	if err := fs.Link(dir, []byte("again"), hello); err != nil {
		t.Fatal(err)
	}
	wantKind(t, fs.Link(root(t, fs), []byte("dirlink"), dir), KindIsDirectory)
	wantKind(t, fs.Link(root(t, fs), []byte("hello.txt"), hello), KindExists)
	checkVolume(t, device)
	if got := resolve(t, fs, "/hello.txt"); got.Links != 3 {
		t.Fatalf("links %d", got.Links)
	}
	for _, path := range [][2]string{{"/", "hello.txt"}, {"/", "link-to-hello"}} {
		if err := fs.Unlink(resolve(t, fs, path[0]), []byte(path[1])); err != nil {
			t.Fatal(err)
		}
	}
	survivor := resolve(t, fs, "/d/again")
	if survivor.Links != 1 || string(readAll(t, fs, survivor)) != "hello ext4\n" {
		t.Fatalf("survivor links %d", survivor.Links)
	}
	attrs := ok(fs.Xattrs(survivor))
	if len(attrs) != 2 {
		t.Fatalf("attributes lost: %d", len(attrs))
	}
	checkVolume(t, device)
}

func TestStaleGeneration(t *testing.T) {
	fs, _ := scratch(t, "canonical")
	first := ok(fs.Create(root(t, fs), []byte("a"), ModeRegular|0o644, 0, 0))
	if err := fs.Unlink(root(t, fs), []byte("a")); err != nil {
		t.Fatal(err)
	}
	second := ok(fs.Create(root(t, fs), []byte("b"), ModeRegular|0o644, 0, 0))
	if second.Number != first.Number || second.Generation == first.Generation {
		t.Fatalf("reuse: %d/%d generations %d/%d", first.Number, second.Number, first.Generation, second.Generation)
	}
	_, err := fs.WriteFile(first, 0, []byte("x"))
	wantKind(t, err, KindStale)
}

func TestRename(t *testing.T) {
	fs, device := scratch(t, "canonical")
	top := root(t, fs)
	a := ok(fs.Mkdir(top, []byte("a"), 0o755, 0, 0))
	b := ok(fs.Mkdir(top, []byte("b"), 0o755, 0, 0))
	ok(fs.Create(a, []byte("f"), ModeRegular|0o644, 0, 0))
	ok(fs.Mkdir(a, []byte("child"), 0o755, 0, 0))
	ok(fs.Mkdir(b, []byte("empty"), 0o755, 0, 0))
	refresh := func(path string) *Inode { return resolve(t, fs, path) }

	// Within one directory, then across, then over an existing file.
	if err := fs.Rename(refresh("/a"), []byte("f"), refresh("/a"), []byte("g")); err != nil {
		t.Fatal(err)
	}
	if err := fs.Rename(refresh("/a"), []byte("g"), refresh("/b"), []byte("g")); err != nil {
		t.Fatal(err)
	}
	if err := fs.Rename(refresh("/"), []byte("hello.txt"), refresh("/b"), []byte("g")); err != nil {
		t.Fatal(err)
	}
	checkVolume(t, device)
	// A directory over an empty one, across parents; its ".." follows.
	if err := fs.Rename(refresh("/a"), []byte("child"), refresh("/b"), []byte("empty")); err != nil {
		t.Fatal(err)
	}
	checkVolume(t, device)
	wantKind(t, fs.Rename(refresh("/"), []byte("a"), refresh("/b/empty"), []byte("a")), 0)
	checkVolume(t, device)
	// Refusals leave the volume as it was.
	snapshot := append([]byte(nil), device.data...)
	wantKind(t, fs.Rename(refresh("/"), []byte("b"), refresh("/b/empty/a"), []byte("loop")), KindInvalid)
	wantKind(t, fs.Rename(refresh("/"), []byte("b"), refresh("/"), []byte("dir")), KindNotEmpty)
	wantKind(t, fs.Rename(refresh("/"), []byte("b"), refresh("/"), []byte("empty")), KindNotDirectory)
	wantKind(t, fs.Rename(refresh("/"), []byte("empty"), refresh("/"), []byte("dir")), KindIsDirectory)
	wantKind(t, fs.Rename(refresh("/"), []byte("missing"), refresh("/"), []byte("x")), KindNotFound)
	if !bytes.Equal(snapshot, device.data) {
		t.Fatal("refused rename changed the volume")
	}
	// Onto another link of the same inode: nothing happens.
	if err := fs.Rename(refresh("/b"), []byte("g"), refresh("/"), []byte("link-to-hello")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapshot, device.data) {
		t.Fatal("rename onto the same inode changed the volume")
	}
	checkVolume(t, device)
}

func TestSparseAndLargeWrites(t *testing.T) {
	fs, device := scratch(t, "canonical")
	file := ok(fs.Create(root(t, fs), []byte("sparse-new"), ModeRegular|0o644, 0, 0))
	far := uint64(5) << 30
	if _, err := fs.WriteFile(file, far, []byte("tail")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.WriteFile(ok(fs.Inode(file.Number)), 10, []byte("head")); err != nil {
		t.Fatal(err)
	}
	// Two megabytes span several transactions.
	big := make([]byte, 2<<20)
	rand.New(rand.NewSource(1)).Read(big)
	if n, err := fs.WriteFile(ok(fs.Inode(file.Number)), 1<<20, big); err != nil || n != len(big) {
		t.Fatalf("big write %d %v", n, err)
	}
	checkVolume(t, device)
	reread := mount(t, device)
	inode := lookupInode(t, reread, root(t, reread), "sparse-new")
	if inode.Size != far+4 {
		t.Fatalf("size %d", inode.Size)
	}
	probe := func(offset uint64, want []byte) {
		got := make([]byte, len(want))
		if _, err := reread.ReadFile(inode, offset, got); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("at %d: %v", offset, err)
		}
	}
	probe(0, append(make([]byte, 10), "head"...))
	probe(1<<20, big)
	probe(far-3, append(make([]byte, 3), "tail"...))
	probe(4<<30, make([]byte, 4096))
}

func TestOverwriteAndAppend(t *testing.T) {
	fs, device := scratch(t, "canonical")
	digests := resolve(t, fs, "/digests")
	original := readAll(t, fs, digests)
	patch := []byte("PATCHED")
	if _, err := fs.WriteFile(digests, 4090, patch); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.WriteFile(resolve(t, fs, "/digests"), uint64(len(original)), []byte("appended")); err != nil {
		t.Fatal(err)
	}
	checkVolume(t, device)
	want := append(append([]byte(nil), original...), "appended"...)
	copy(want[4090:], patch)
	if got := readAll(t, fs, resolve(t, fs, "/digests")); !bytes.Equal(got, want) {
		t.Fatal("content differs")
	}
}

func TestUnwrittenConversion(t *testing.T) {
	fs, device := scratch(t, "canonical")
	empty := resolve(t, fs, "/empty")
	before, err := fs.mapBlock(empty, 3)
	if err != nil || !before.Unwritten {
		t.Fatalf("fixture lost its unwritten extent: %+v %v", before, err)
	}
	if _, err := fs.WriteFile(empty, 3*4096+100, []byte("inside")); err != nil {
		t.Fatal(err)
	}
	inode := resolve(t, fs, "/empty")
	after := ok(fs.mapBlock(inode, 3))
	if after.Unwritten || after.Physical != before.Physical {
		t.Fatalf("mapping after write %+v, before %+v", after, before)
	}
	if neighbour := ok(fs.mapBlock(inode, 2)); !neighbour.Unwritten {
		t.Fatal("conversion spread past the written block")
	}
	got := make([]byte, 4096)
	ok(fs.ReadFile(inode, 3*4096, got))
	if want := append(make([]byte, 100), "inside"...); !bytes.Equal(got[:106], want) || !bytes.Equal(got[106:], make([]byte, 4096-106)) {
		t.Fatal("converted block content")
	}
	checkVolume(t, device)
}

func TestTruncate(t *testing.T) {
	fs, device := scratch(t, "canonical")
	before := fs.Info().FreeBlocks
	fragmented := resolve(t, fs, "/fragmented")
	blocks := fragmented.Blocks512 / 8
	if err := fs.Truncate(fragmented, 100*8192+10); err != nil {
		t.Fatal(err)
	}
	checkVolume(t, device)
	inode := resolve(t, fs, "/fragmented")
	got := make([]byte, 4096)
	ok(fs.ReadFile(inode, 100*8192, got))
	if string(got[:10]) != "block 0100" || !bytes.Equal(got[10:], make([]byte, 4086)) {
		t.Fatalf("tail block %q", got[:12])
	}
	if err := fs.Truncate(inode, 1<<30); err != nil {
		t.Fatal(err)
	}
	if err := fs.Truncate(resolve(t, fs, "/fragmented"), 0); err != nil {
		t.Fatal(err)
	}
	checkVolume(t, device)
	inode = resolve(t, fs, "/fragmented")
	if inode.Blocks512 != 0 || le16(inode.blockArea[:], 6) != 0 {
		t.Fatalf("truncated inode keeps %d sectors, depth %d", inode.Blocks512, le16(inode.blockArea[:], 6))
	}
	if freed := fs.Info().FreeBlocks - before; freed != blocks {
		t.Fatalf("freed %d of %d blocks", freed, blocks)
	}
}

// TestDeepExtentTree grows a tree past what depth 1 holds and shrinks it
// back, so leaves and index nodes split, the root grows, and nodes are
// freed and collapsed.
func TestDeepExtentTree(t *testing.T) {
	fs, device := scratch(t, "canonical")
	file := ok(fs.Create(root(t, fs), []byte("deep"), ModeRegular|0o644, 0, 0))
	const extents = 1500
	order := rand.New(rand.NewSource(2)).Perm(extents)
	for _, index := range order {
		if _, err := fs.WriteFile(ok(fs.Inode(file.Number)), uint64(index)*8192, []byte(fmt.Sprintf("x%05d", index))); err != nil {
			t.Fatalf("write %d: %v", index, err)
		}
	}
	inode := ok(fs.Inode(file.Number))
	if depth := le16(inode.blockArea[:], 6); depth < 2 {
		t.Fatalf("depth %d after %d extents", depth, extents)
	}
	checkVolume(t, device)
	for index := 0; index < extents; index += 97 {
		got := make([]byte, 6)
		ok(fs.ReadFile(inode, uint64(index)*8192, got))
		if string(got) != fmt.Sprintf("x%05d", index) {
			t.Fatalf("extent %d: %q", index, got)
		}
	}
	if err := fs.Truncate(inode, 3*8192); err != nil {
		t.Fatal(err)
	}
	checkVolume(t, device)
	inode = ok(fs.Inode(file.Number))
	if depth := le16(inode.blockArea[:], 6); depth != 0 {
		t.Fatalf("depth %d after truncating to two extents", depth)
	}
}

// TestIndexedDirectory adds names to a Linux-built hash tree until leaves
// split, removes half, and checks every remaining name through the index.
func TestIndexedDirectory(t *testing.T) {
	for _, image := range []string{"hash-tea", "hash-legacy"} {
		t.Run(image, func(t *testing.T) {
			fs, device := scratch(t, image)
			names := resolve(t, fs, "/names")
			if !fs.isIndexed(names) {
				t.Fatal("fixture directory is not indexed")
			}
			startBlocks := names.Size / 4096
			for index := 0; index < 2500; index++ {
				name := []byte(fmt.Sprintf("added-%d-\xe9\xff", index))
				if _, err := fs.Create(resolve(t, fs, "/names"), name, ModeRegular|0o644, 0, 0); err != nil {
					t.Fatalf("create %d: %v", index, err)
				}
			}
			if grown := resolve(t, fs, "/names").Size / 4096; grown <= startBlocks {
				t.Fatal("no leaf split")
			}
			checkVolume(t, device)
			for index := 0; index < 2500; index += 2 {
				if err := fs.Unlink(resolve(t, fs, "/names"), []byte(fmt.Sprintf("added-%d-\xe9\xff", index))); err != nil {
					t.Fatalf("unlink %d: %v", index, err)
				}
			}
			for index := 0; index < 600; index += 3 {
				if err := fs.Unlink(resolve(t, fs, "/names"), []byte(fmt.Sprintf("name-%d", index))); err != nil {
					t.Fatalf("unlink fixture name %d: %v", index, err)
				}
			}
			checkVolume(t, device)
		})
	}
}

// TestLinearDirectoryGrowth fills a new directory past one block.
func TestLinearDirectoryGrowth(t *testing.T) {
	fs, device := scratch(t, "canonical")
	dir := ok(fs.Mkdir(root(t, fs), []byte("many"), 0o755, 0, 0))
	for index := 0; index < 600; index++ {
		if _, err := fs.Create(ok(fs.Inode(dir.Number)), []byte(fmt.Sprintf("file-with-a-longer-name-%04d", index)), ModeRegular|0o644, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if size := ok(fs.Inode(dir.Number)).Size; size < 3*4096 {
		t.Fatalf("directory size %d", size)
	}
	checkVolume(t, device)
}

// TestNoSpace fills the volume, checks the write reports what committed and
// that removing the file returns every block.
func TestNoSpace(t *testing.T) {
	fs, device := scratch(t, "canonical")
	before := fs.Info()
	file := ok(fs.Create(root(t, fs), []byte("fill"), ModeRegular|0o644, 0, 0))
	chunk := make([]byte, 4<<20)
	offset := uint64(0)
	for {
		n, err := fs.WriteFile(ok(fs.Inode(file.Number)), offset, chunk)
		offset += uint64(n)
		if err != nil {
			wantKind(t, err, KindNoSpace)
			break
		}
	}
	checkVolume(t, device)
	inode := ok(fs.Inode(file.Number))
	if inode.Size != offset || fs.Info().AvailableBytes != 0 {
		t.Fatalf("size %d, written %d, available %d", inode.Size, offset, fs.Info().AvailableBytes)
	}
	_, err := fs.Mkdir(root(t, fs), []byte("d"), 0o755, 0, 0)
	wantKind(t, err, KindNoSpace)
	if err := fs.Unlink(root(t, fs), []byte("fill")); err != nil {
		t.Fatal(err)
	}
	checkVolume(t, device)
	if after := fs.Info(); after.FreeBlocks != before.FreeBlocks || after.FreeInodes != before.FreeInodes {
		t.Fatalf("free %d/%d, was %d/%d", after.FreeBlocks, after.FreeInodes, before.FreeBlocks, before.FreeInodes)
	}
}

func TestInodeExhaustion(t *testing.T) {
	fs, device := scratch(t, "canonical")
	dir := ok(fs.Mkdir(root(t, fs), []byte("inodes"), 0o755, 0, 0))
	count := 0
	for {
		_, err := fs.Create(ok(fs.Inode(dir.Number)), []byte(fmt.Sprintf("%d", count)), ModeRegular|0o644, 0, 0)
		if err != nil {
			wantKind(t, err, KindNoSpace)
			break
		}
		count++
	}
	if fs.Info().FreeInodes != 0 {
		t.Fatalf("stopped with %d free inodes", fs.Info().FreeInodes)
	}
	checkVolume(t, device)
}

// TestReadFaults fails each device read of an operation in turn: every
// failure must leave the volume byte for byte as it was.
func TestReadFaults(t *testing.T) {
	operations := map[string]func(fs *FS) error{
		"create": func(fs *FS) error {
			top, err := fs.Root()
			if err != nil {
				return err
			}
			_, err = fs.Create(top, []byte("new"), ModeRegular|0o644, 0, 0)
			return err
		},
		"mkdir": func(fs *FS) error {
			top, err := fs.Root()
			if err != nil {
				return err
			}
			_, err = fs.Mkdir(top, []byte("new"), 0o755, 0, 0)
			return err
		},
		"write": func(fs *FS) error {
			inode, err := pathOf(fs, "/fragmented")
			if err != nil {
				return err
			}
			_, err = fs.WriteFile(inode, 8192*3+4096, bytes.Repeat([]byte("w"), 9000))
			return err
		},
		"unlink": func(fs *FS) error {
			top, err := fs.Root()
			if err != nil {
				return err
			}
			return fs.Unlink(top, []byte("fragmented"))
		},
		"rename": func(fs *FS) error {
			top, err := fs.Root()
			if err != nil {
				return err
			}
			to, err := pathOf(fs, "/big")
			if err != nil {
				return err
			}
			return fs.Rename(top, []byte("dir"), to, []byte("moved"))
		},
		"truncate": func(fs *FS) error {
			inode, err := pathOf(fs, "/fragmented")
			if err != nil {
				return err
			}
			return fs.Truncate(inode, 5000)
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			for fault := 1; ; fault++ {
				fs, device := scratch(t, "canonical")
				snapshot := append([]byte(nil), device.data...)
				device.failRead = device.reads + fault
				err := operation(fs)
				device.failRead = 0
				if err == nil {
					checkVolume(t, device)
					return
				}
				if KindOf(err) != KindIO {
					t.Fatalf("fault %d: %v", fault, err)
				}
				// Multi-transaction operations may commit a prefix; the rest
				// must still be consistent.
				if !bytes.Equal(snapshot, device.data) && device.writes == 0 {
					t.Fatalf("fault %d changed the volume without writing", fault)
				}
				checkVolume(t, device)
				if fault > 2000 {
					t.Fatal("operation never completed")
				}
			}
		})
	}
}

// pathOf resolves an absolute path, passing read failures back.
func pathOf(fs *FS, path string) (*Inode, error) {
	inode, err := fs.Root()
	if err != nil {
		return nil, err
	}
	for _, part := range bytes.Split([]byte(path[1:]), []byte("/")) {
		number, _, err := fs.Lookup(inode, part)
		if err != nil {
			return nil, err
		}
		if inode, err = fs.Inode(number); err != nil {
			return nil, err
		}
	}
	return inode, nil
}

// TestWriteFailureStopsMutation fails a commit write: the volume refuses
// further mutation instead of building on an unknown state.
func TestWriteFailureStopsMutation(t *testing.T) {
	fs, device := scratch(t, "canonical")
	device.failWrite = device.writes + 2
	_, err := fs.Create(root(t, fs), []byte("x"), ModeRegular|0o644, 0, 0)
	wantKind(t, err, KindIO)
	if fs.State() != StateFailed {
		t.Fatalf("state %v", fs.State())
	}
	_, err = fs.Create(root(t, fs), []byte("y"), ModeRegular|0o644, 0, 0)
	wantKind(t, err, KindReadOnly)
}

// TestAbortLeavesNoTrace checks that refused operations write nothing.
func TestAbortLeavesNoTrace(t *testing.T) {
	fs, device := scratch(t, "canonical")
	writes := device.writes
	_, err := fs.Create(root(t, fs), []byte("hello.txt"), ModeRegular|0o644, 0, 0)
	wantKind(t, err, KindExists)
	_, err = fs.Create(root(t, fs), []byte("a/b"), ModeRegular|0o644, 0, 0)
	wantKind(t, err, KindInvalid)
	_, err = fs.Mkdir(resolve(t, fs, "/hello.txt"), []byte("x"), 0o755, 0, 0)
	wantKind(t, err, KindNotDirectory)
	wantKind(t, fs.Unlink(root(t, fs), []byte("..")), KindInvalid)
	_, err = fs.Symlink(root(t, fs), []byte("s"), []byte("a\x00b"), 0, 0)
	wantKind(t, err, KindInvalid)
	if device.writes != writes {
		t.Fatal("a refused operation wrote")
	}
}

// TestRandomOperations runs a seeded mix of operations against a model of
// the namespace and file contents, checking the volume and the model as it
// goes.
func TestRandomOperations(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			runRandomOperations(t, seed, 400)
		})
	}
}

type modelFile struct {
	data []byte
}

func runRandomOperations(t *testing.T, seed int64, steps int) *scratchDevice {
	random := rand.New(rand.NewSource(seed))
	fs, device := scratch(t, "canonical")
	dirs := []string{"/"}
	files := map[string]*modelFile{}
	pick := func(list []string) string { return list[random.Intn(len(list))] }
	join := func(dir string, name string) string {
		if dir == "/" {
			return "/" + name
		}
		return dir + "/" + name
	}
	fileNames := func() []string {
		var names []string
		for name := range files {
			names = append(names, name)
		}
		sortStrings(names)
		return names
	}
	for step := 0; step < steps; step++ {
		switch op := random.Intn(10); {
		case op < 2:
			dir := pick(dirs)
			name := fmt.Sprintf("f%d", step)
			if _, err := fs.Create(resolve(t, fs, dir), []byte(name), ModeRegular|0o644, 0, 0); err != nil {
				t.Fatalf("step %d create: %v", step, err)
			}
			files[join(dir, name)] = &modelFile{}
		case op < 3:
			dir := pick(dirs)
			name := fmt.Sprintf("d%d", step)
			if _, err := fs.Mkdir(resolve(t, fs, dir), []byte(name), 0o755, 0, 0); err != nil {
				t.Fatalf("step %d mkdir: %v", step, err)
			}
			dirs = append(dirs, join(dir, name))
		case op < 6 && len(files) > 0:
			path := pick(fileNames())
			file := files[path]
			offset := random.Intn(len(file.data) + 20000)
			if random.Intn(4) == 0 {
				offset += random.Intn(1 << 22)
			}
			data := make([]byte, random.Intn(20000)+1)
			random.Read(data)
			if _, err := fs.WriteFile(resolve(t, fs, path), uint64(offset), data); err != nil {
				t.Fatalf("step %d write: %v", step, err)
			}
			if end := offset + len(data); end > len(file.data) {
				file.data = append(file.data, make([]byte, end-len(file.data))...)
			}
			copy(file.data[offset:], data)
		case op < 7 && len(files) > 0:
			path := pick(fileNames())
			file := files[path]
			size := random.Intn(len(file.data) + 10000)
			if err := fs.Truncate(resolve(t, fs, path), uint64(size)); err != nil {
				t.Fatalf("step %d truncate: %v", step, err)
			}
			if size < len(file.data) {
				file.data = file.data[:size]
			} else {
				file.data = append(file.data, make([]byte, size-len(file.data))...)
			}
		case op < 8 && len(files) > 0:
			path := pick(fileNames())
			dir, name := splitPath(path)
			if err := fs.Unlink(resolve(t, fs, dir), []byte(name)); err != nil {
				t.Fatalf("step %d unlink: %v", step, err)
			}
			delete(files, path)
		case len(files) > 0:
			path := pick(fileNames())
			dir, name := splitPath(path)
			toDir := pick(dirs)
			toName := fmt.Sprintf("r%d", step)
			if err := fs.Rename(resolve(t, fs, dir), []byte(name), resolve(t, fs, toDir), []byte(toName)); err != nil {
				t.Fatalf("step %d rename: %v", step, err)
			}
			files[join(toDir, toName)] = files[path]
			delete(files, path)
		}
		if step%100 == 99 {
			checkVolume(t, device)
			fs = ok(mountScratch(device, testClock()))
			for path, file := range files {
				if got := readAll(t, fs, resolve(t, fs, path)); !bytes.Equal(got, file.data) {
					t.Fatalf("step %d: %s differs", step, path)
				}
			}
		}
	}
	checkVolume(t, device)
	return device
}

func splitPath(path string) (string, string) {
	at := bytes.LastIndexByte([]byte(path), '/')
	if at == 0 {
		return "/", path[1:]
	}
	return path[:at], path[at+1:]
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for at := index; at > 0 && values[at-1] > values[at]; at-- {
			values[at-1], values[at] = values[at], values[at-1]
		}
	}
}
