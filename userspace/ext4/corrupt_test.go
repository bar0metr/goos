package ext4

import (
	"encoding/binary"
	"math/rand"
	"os"
	"testing"
	"time"
)

// resealSuperblock recomputes the superblock checksum after an edit, so a
// test reaches the check it targets instead of the checksum.
func resealSuperblock(device *memDevice) {
	sb := device.data[superblockOffset : superblockOffset+superblockSize]
	putLE32(sb[0x3FC:], crc32c(^uint32(0), sb[:0x3FC]))
}

func setLE32(data []byte, offset int, value uint32) { putLE32(data[offset:], value) }

func expectKind(t *testing.T, device *memDevice, want Kind) {
	t.Helper()
	_, err := Mount(device, Options{})
	if KindOf(err) != want {
		t.Fatalf("mount: %v, want %v", err, want)
	}
}

func TestSuperblockRefusals(t *testing.T) {
	sbAt := superblockOffset
	cases := []struct {
		name   string
		edit   func([]byte)
		reseal bool
		want   Kind
	}{
		{"magic", func(d []byte) { d[sbAt+0x38] = 0 }, true, KindCorrupt},
		{"checksum", func(d []byte) { d[sbAt+0x10]++ }, false, KindCorrupt},
		{"unknown incompat", func(d []byte) { setLE32(d, sbAt+0x60, le32(d, sbAt+0x60)|0x80000000) }, true, KindUnsupported},
		{"unknown ro_compat", func(d []byte) { setLE32(d, sbAt+0x64, le32(d, sbAt+0x64)|0x80000000) }, true, KindUnsupported},
		{"inline_data", func(d []byte) { setLE32(d, sbAt+0x60, le32(d, sbAt+0x60)|incompatInlineData) }, true, KindUnsupported},
		{"bigalloc", func(d []byte) { setLE32(d, sbAt+0x64, le32(d, sbAt+0x64)|roCompatBigalloc) }, true, KindUnsupported},
		{"meta_bg", func(d []byte) { setLE32(d, sbAt+0x60, le32(d, sbAt+0x60)|incompatMetaBG) }, true, KindUnsupported},
		{"journal_dev", func(d []byte) { setLE32(d, sbAt+0x60, le32(d, sbAt+0x60)|incompatJournalDev) }, true, KindUnsupported},
		{"orphan_present", func(d []byte) { setLE32(d, sbAt+0x64, le32(d, sbAt+0x64)|roCompatOrphanPresent) }, true, KindUnsupported},
		{"needs recovery", func(d []byte) { setLE32(d, sbAt+0x60, le32(d, sbAt+0x60)|incompatRecover) }, true, KindNeedsRecovery},
		{"not clean", func(d []byte) { d[sbAt+0x3A] = 0 }, true, KindNeedsRecovery},
		{"64 KiB blocks", func(d []byte) { setLE32(d, sbAt+0x18, 6) }, true, KindUnsupported},
		{"huge block count", func(d []byte) { setLE32(d, sbAt+0x150, 1) }, true, KindCorrupt},
		{"inode count", func(d []byte) { setLE32(d, sbAt+0x0, le32(d, sbAt)+1) }, true, KindCorrupt},
		{"inode size", func(d []byte) { d[sbAt+0x58] = 200 }, true, KindCorrupt},
		{"blocks per group", func(d []byte) { setLE32(d, sbAt+0x20, 1<<20) }, true, KindCorrupt},
		{"journal inode", func(d []byte) { setLE32(d, sbAt+0xE0, 0) }, true, KindCorrupt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			device := fixture(t, "canonical")
			tc.edit(device.data)
			if tc.reseal {
				resealSuperblock(device)
			}
			expectKind(t, device, tc.want)
		})
	}
}

func TestReadOnlyFeaturesMountReadOnly(t *testing.T) {
	for _, mask := range []uint32{roCompatQuota, roCompatProject, roCompatReadonly} {
		device := fixture(t, "canonical")
		setLE32(device.data, superblockOffset+0x64, le32(device.data, superblockOffset+0x64)|mask)
		resealSuperblock(device)
		fs := mount(t, device)
		if fs.Status().Negotiation.Support != ReadOnly {
			t.Fatalf("ro_compat %#x negotiated %v", mask, fs.Status().Negotiation.Support)
		}
	}
}

// groupDescriptorOffset is where the first descriptor sits on the 4 KiB
// fixtures: block 1.
const groupDescriptorOffset = 4096

func TestGroupDescriptorChecks(t *testing.T) {
	for _, name := range []string{"canonical", "legacy"} {
		device := fixture(t, name)
		device.data[groupDescriptorOffset+0xC]++ // free blocks count
		expectKind(t, device, KindCorrupt)
	}
	device := fixture(t, "canonical")
	setLE32(device.data, groupDescriptorOffset+0x8, 0) // inode table over the superblock
	expectKind(t, device, KindCorrupt)
}

func TestBitmapChecksum(t *testing.T) {
	device := fixture(t, "canonical")
	fs := mount(t, device)
	desc, err := fs.group(0)
	if err != nil {
		t.Fatal(err)
	}
	device.data[desc.InodeBitmap*4096] ^= 0x80
	expectKind(t, device, KindCorrupt)
}

// corruptInodeOf flips a byte inside the on-disk inode of path.
func corruptInodeOf(t *testing.T, device *memDevice, path string, field int) {
	t.Helper()
	fs := mount(t, device)
	inode := resolve(t, fs, path)
	group, index, _ := fs.inodeLocation(inode.Number)
	desc, _ := fs.group(group)
	offset := desc.InodeTable*4096 + uint64(index)*uint64(fs.sb.InodeSize)
	device.data[offset+uint64(field)] ^= 0x01
}

func TestInodeChecksum(t *testing.T) {
	device := fixture(t, "canonical")
	corruptInodeOf(t, device, "/hello.txt", 0x4)
	fs := mount(t, device)
	root, _ := fs.Root()
	number, _, err := fs.Lookup(root, []byte("hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Inode(number); KindOf(err) != KindCorrupt {
		t.Fatalf("inode read: %v", err)
	}
}

func TestExtentBlockChecksum(t *testing.T) {
	device := fixture(t, "canonical")
	fs := mount(t, device)
	inode := resolve(t, fs, "/fragmented")
	child := join64(le32(inode.blockArea[:], 12+4), uint32(le16(inode.blockArea[:], 12+8)))
	device.data[child*4096+12] ^= 0x01
	fs = mount(t, device)
	if _, err := fs.ReadFile(resolve(t, fs, "/fragmented"), 0, make([]byte, 16)); KindOf(err) != KindCorrupt {
		t.Fatalf("read: %v", err)
	}
}

func TestDirectoryBlockChecksum(t *testing.T) {
	device := fixture(t, "canonical")
	fs := mount(t, device)
	dir := resolve(t, fs, "/dir")
	mapping, err := fs.mapBlock(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	device.data[mapping.Physical*4096+24] ^= 0x01 // inside the first name
	fs = mount(t, device)
	root, _ := fs.Root()
	number, _, _ := fs.Lookup(root, []byte("dir"))
	inode, _ := fs.Inode(number)
	if _, _, err := fs.Lookup(inode, []byte("sub")); KindOf(err) != KindCorrupt {
		t.Fatalf("lookup: %v", err)
	}
}

func TestHashTreeChecksum(t *testing.T) {
	device := fixture(t, "canonical")
	fs := mount(t, device)
	big := resolve(t, fs, "/big")
	mapping, _ := fs.mapBlock(big, 0)
	device.data[mapping.Physical*4096+dxRootEntries+12] ^= 0x01 // a hash in the root
	fs = mount(t, device)
	big = resolve(t, fs, "/big")
	if _, _, err := fs.Lookup(big, []byte("file-7")); KindOf(err) != KindCorrupt {
		t.Fatalf("lookup: %v", err)
	}
}

func TestDeviceErrors(t *testing.T) {
	device := fixture(t, "canonical")
	device.failAt = superblockOffset
	expectKind(t, device, KindIO)
	device = fixture(t, "canonical")
	fs := mount(t, device)
	inode := resolve(t, fs, "/hello.txt")
	mapping, _ := fs.mapBlock(inode, 0)
	device.failAt = int64(mapping.Physical * 4096)
	if _, err := fs.ReadFile(inode, 0, make([]byte, 5)); KindOf(err) != KindIO {
		t.Fatalf("read: %v", err)
	}
}

// TestRandomMetadataDamage flips bytes in the metadata region and walks the
// whole tree. Every outcome is allowed except a panic or a walk that does
// not finish: damage must surface as an error, never as a crash or a hang.
func TestRandomMetadataDamage(t *testing.T) {
	rounds := 60
	if os.Getenv("GOOS_STORAGE_STRESS") != "" {
		rounds = 3000
	}
	pristine := fixture(t, "canonical")
	// The metadata lives in the first groups' tables and the directory and
	// extent blocks; damage the first 3 MiB, where all of them sit.
	limit := metadataSpan
	random := rand.New(rand.NewSource(1))
	for round := 0; round < rounds; round++ {
		device := &memDevice{data: append([]byte(nil), pristine.data...), failAt: -1}
		for flips := 1 + random.Intn(8); flips > 0; flips-- {
			device.data[random.Intn(limit)] ^= byte(1 << random.Intn(8))
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("round %d: panic %v", round, recovered)
				}
			}()
			walkAll(device)
		}()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("round %d: walk did not finish", round)
		}
	}
}

// walkAll mounts device and reads every reachable directory, file and link,
// ignoring errors.
func walkAll(device Device) {
	fs, err := Mount(device, Options{})
	if err != nil {
		return
	}
	root, err := fs.Root()
	if err != nil {
		return
	}
	queue := []*Inode{root}
	seen := map[uint32]bool{root.Number: true}
	buffer := make([]byte, 65536)
	for len(queue) > 0 && len(seen) < 10000 {
		dir := queue[0]
		queue = queue[1:]
		var children []uint32
		fs.ReadDir(dir, 0, func(entry *DirEntry) bool {
			children = append(children, entry.Inode)
			fs.Lookup(dir, entry.Name())
			return true
		})
		for _, number := range children {
			if seen[number] {
				continue
			}
			seen[number] = true
			inode, err := fs.Inode(number)
			if err != nil {
				continue
			}
			fs.Xattrs(inode)
			switch {
			case inode.IsDir():
				queue = append(queue, inode)
			case inode.IsSymlink():
				fs.Readlink(inode)
			case inode.IsRegular():
				for offset := uint64(0); offset < inode.Size && offset < 1<<22; offset += uint64(len(buffer)) {
					if _, err := fs.ReadFile(inode, offset, buffer); err != nil {
						break
					}
				}
			}
		}
	}
}

// metadataSpan is the length of the canonical fixture prefix that holds
// its superblock, descriptors, bitmaps, inode tables, and the directory and
// extent blocks mke2fs placed after them.
const metadataSpan = 3 << 20

// FuzzImageDamage applies the input as (offset, xor) pairs to the metadata
// region of the canonical fixture and walks the result. Run it with
// go test -fuzz=FuzzImageDamage ./userspace/ext4; the seeds run as part of
// the normal suite.
func FuzzImageDamage(f *testing.F) {
	f.Add([]byte{0, 0, 4, 0x38, 0xff})    // superblock magic
	f.Add([]byte{0, 0, 16, 0x0c, 0x01})   // first descriptor
	f.Add([]byte{0, 0x10, 0, 0x00, 0x80}) // inside the inode tables
	f.Add([]byte{0, 0x20, 0, 0x10, 0x04, 0, 0x22, 0, 0x08, 0x02})
	pristine := fixture(f, "canonical")
	limit := uint32(metadataSpan)
	f.Fuzz(func(t *testing.T, patch []byte) {
		if len(patch) > 5*64 {
			patch = patch[:5*64]
		}
		device := &memDevice{data: append([]byte(nil), pristine.data...), failAt: -1}
		for ; len(patch) >= 5; patch = patch[5:] {
			offset := binary.BigEndian.Uint32(patch) % limit
			device.data[offset] ^= patch[4]
		}
		walkAll(device)
	})
}
