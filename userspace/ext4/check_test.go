package ext4

import (
	"bytes"
	"fmt"
	"testing"
)

// checkVolume remounts device read-only and cross-checks the whole volume:
// every block bitmap against the blocks the metadata and inodes claim,
// every free count, every inode bitmap against the in-use inodes, extent
// tree shape and checksums, block counts, directory structure, link
// counts and reachability. It is the oracle the mutation tests compare
// against; e2fsck is the independent one (tools/ext4fixture).
func checkVolume(t testing.TB, device Device) {
	t.Helper()
	if err := verifyVolume(device); err != nil {
		t.Fatalf("volume check: %v", err)
	}
}

type volumeCheck struct {
	fs        *FS
	owner     map[uint64]uint32
	xattrRefs map[uint64]uint32
	inUse     map[uint32]*Inode
	names     map[uint32]int
	subdirs   map[uint32]int
	parents   map[uint32]uint32
}

func verifyVolume(device Device) error {
	fs, err := Mount(device, Options{})
	if err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	check := &volumeCheck{
		fs:        fs,
		owner:     map[uint64]uint32{},
		xattrRefs: map[uint64]uint32{},
		inUse:     map[uint32]*Inode{},
		names:     map[uint32]int{},
		subdirs:   map[uint32]int{},
		parents:   map[uint32]uint32{},
	}
	if err := check.metadata(); err != nil {
		return err
	}
	if err := check.inodes(); err != nil {
		return err
	}
	if err := check.bitmaps(); err != nil {
		return err
	}
	return check.namespace()
}

func (check *volumeCheck) claim(block uint64, count uint64, inode uint32) error {
	for index := uint64(0); index < count; index++ {
		if previous, taken := check.owner[block+index]; taken {
			return fmt.Errorf("block %d claimed by inode %d and %d", block+index, previous, inode)
		}
		check.owner[block+index] = inode
	}
	return nil
}

// metadata claims the superblock copies, descriptor tables and their
// reserved blocks, bitmaps and inode tables.
func (check *volumeCheck) metadata() error {
	fs := check.fs
	tableBlocks := (fs.groups*uint64(fs.sb.DescriptorSize) + uint64(fs.blockSize) - 1) / uint64(fs.blockSize)
	itableBlocks := uint64(fs.sb.InodesPerGroup) * uint64(fs.sb.InodeSize) / uint64(fs.blockSize)
	for group := uint32(0); uint64(group) < fs.groups; group++ {
		if fs.groupHasSuper(group) {
			start := fs.groupStart(group)
			if err := check.claim(start, 1+tableBlocks+uint64(fs.sb.ReservedGDTBlocks), 0); err != nil {
				return err
			}
		}
		desc, err := fs.group(group)
		if err != nil {
			return err
		}
		for _, run := range [][2]uint64{{desc.BlockBitmap, 1}, {desc.InodeBitmap, 1}, {desc.InodeTable, itableBlocks}} {
			if err := check.claim(run[0], run[1], 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// inodes walks every inode the bitmaps mark in use.
func (check *volumeCheck) inodes() error {
	fs := check.fs
	for number := uint32(1); number <= fs.sb.InodesCount; number++ {
		allocated, err := fs.inodeAllocated(number)
		if err != nil {
			return err
		}
		if !allocated {
			continue
		}
		raw, err := fs.readInode(number)
		if err != nil {
			return fmt.Errorf("inode %d: %w", number, err)
		}
		if number < fs.sb.FirstInode {
			if err := check.reserved(raw); err != nil {
				return err
			}
			continue
		}
		if raw.Mode == 0 || raw.Links == 0 {
			return fmt.Errorf("inode %d marked in use with mode %o links %d", number, raw.Mode, raw.Links)
		}
		if err := check.inode(raw); err != nil {
			return err
		}
	}
	return nil
}

// reserved claims the blocks of the reserved inodes Linux fills: the root,
// the resize inode's double-indirect block and the journal.
func (check *volumeCheck) reserved(inode *Inode) error {
	switch inode.Number {
	case RootInode, check.fs.sb.JournalInode:
		return check.inode(inode)
	case 7:
		if inode.Mode == 0 {
			return nil
		}
		if dind := uint64(le32(inode.blockArea[:], 13*4)); dind != 0 {
			return check.claim(dind, 1, 7)
		}
	}
	return nil
}

func (check *volumeCheck) inode(inode *Inode) error {
	fs := check.fs
	if err := fs.checkInode(inode); err != nil {
		return fmt.Errorf("inode %d: %w", inode.Number, err)
	}
	check.inUse[inode.Number] = inode
	blocks := uint64(0)
	if inode.FileACL != 0 {
		check.xattrRefs[inode.FileACL]++
		blocks++
	}
	switch {
	case inode.Flags&inodeFlagExtents != 0:
		var last uint64
		used, err := check.extentNode(inode, inode.blockArea[:], -1, 0, &last)
		if err != nil {
			return fmt.Errorf("inode %d: %w", inode.Number, err)
		}
		blocks += used
		if inode.IsDir() && last*uint64(fs.blockSize) != inode.Size {
			return fmt.Errorf("directory %d maps %d blocks for size %d", inode.Number, last, inode.Size)
		}
	case fs.fastSymlink(inode), inode.Type() != ModeRegular && inode.Type() != ModeDir && inode.Type() != ModeSymlink:
	default:
		return fmt.Errorf("inode %d is block-mapped", inode.Number)
	}
	if want := blocks * uint64(fs.blockSize/512); inode.Blocks512 != want {
		return fmt.Errorf("inode %d records %d sectors, owns %d", inode.Number, inode.Blocks512, want)
	}
	return nil
}

// extentNode checks one node and claims its blocks. It returns the blocks
// owned below it and keeps last at the end of the highest mapping.
func (check *volumeCheck) extentNode(inode *Inode, node []byte, depth int, first uint32, last *uint64) (uint64, error) {
	fs := check.fs
	header, err := parseExtentNode(node, depth)
	if err != nil {
		return 0, err
	}
	if depth >= 0 && header.entries == 0 {
		return 0, fmt.Errorf("empty extent node at depth %d", depth)
	}
	if depth == -1 && header.depth > 0 && header.entries == 0 {
		return 0, fmt.Errorf("empty extent index root")
	}
	owned := uint64(0)
	for index := 0; index < int(header.entries); index++ {
		entry := node[12+12*index:]
		key := le32(entry, 0)
		if index == 0 && depth >= 0 && key != first {
			return 0, fmt.Errorf("index key %d differs from child's first block %d", first, key)
		}
		if header.depth == 0 {
			e := decodeExtent(entry[:12])
			if uint64(e.logical) < *last {
				return 0, fmt.Errorf("extents out of order at %d", e.logical)
			}
			*last = e.end()
			if e.length == 0 {
				return 0, fmt.Errorf("zero-length extent")
			}
			if err := check.claim(e.physical, uint64(e.length), inode.Number); err != nil {
				return 0, err
			}
			owned += uint64(e.length)
			if !e.unwritten && inode.IsRegular() && e.end()*uint64(fs.blockSize) > inode.Size+uint64(fs.blockSize)-1 && inode.Size != 0 {
				// A written extent past the last byte is allowed only up to
				// the block holding it.
				return 0, fmt.Errorf("written extent at %d past end of file %d", e.logical, inode.Size)
			}
			continue
		}
		child := join64(le32(entry, 4), uint32(le16(entry, 8)))
		if err := check.claim(child, 1, inode.Number); err != nil {
			return 0, err
		}
		owned++
		data := make([]byte, fs.blockSize)
		if !fs.device.ReadAt(child*uint64(fs.blockSize), data) {
			return 0, errIO
		}
		next, err := parseExtentNode(data, int(header.depth)-1)
		if err != nil {
			return 0, err
		}
		tail := 12 + 12*int(next.max)
		if int(next.max) != (int(fs.blockSize)-12)/12 || crc32c(inode.csumSeed, data[:tail]) != le32(data, tail) {
			return 0, fmt.Errorf("extent block %d capacity or checksum wrong", child)
		}
		below, err := check.extentNode(inode, data, int(header.depth)-1, key, last)
		if err != nil {
			return 0, err
		}
		owned += below
	}
	return owned, nil
}

// bitmaps compares each group's bitmaps and counts with what was claimed.
func (check *volumeCheck) bitmaps() error {
	fs := check.fs
	for block, refs := range check.xattrRefs {
		data := make([]byte, fs.blockSize)
		if !fs.device.ReadAt(block*uint64(fs.blockSize), data) {
			return errIO
		}
		if le32(data, 4) != refs {
			return fmt.Errorf("attribute block %d has refcount %d, %d users", block, le32(data, 4), refs)
		}
		if err := check.claim(block, 1, 0); err != nil {
			return err
		}
	}
	var freeBlocks uint64
	var freeInodes uint32
	for group := uint32(0); uint64(group) < fs.groups; group++ {
		desc, err := fs.group(group)
		if err != nil {
			return err
		}
		start, count := fs.groupStart(group), fs.groupBlocks(group)
		var bitmap []byte
		if desc.Flags&groupBlockUninit == 0 {
			data, err := fs.cache.get(desc.BlockBitmap)
			if err != nil {
				return err
			}
			bitmap = append([]byte(nil), data...)
			for bit := count; bit < uint64(fs.blockSize)*8; bit++ {
				if !testBit(bitmap, bit) {
					return fmt.Errorf("group %d block bitmap padding bit %d clear", group, bit)
				}
			}
		}
		free := uint32(0)
		for bit := uint64(0); bit < count; bit++ {
			_, claimed := check.owner[start+bit]
			used := claimed
			if bitmap != nil {
				used = testBit(bitmap, bit)
			}
			if used != claimed {
				return fmt.Errorf("group %d block %d: bitmap %v, claimed %v", group, start+bit, used, claimed)
			}
			if !used {
				free++
			}
		}
		if free != desc.FreeBlocks {
			return fmt.Errorf("group %d records %d free blocks, has %d", group, desc.FreeBlocks, free)
		}
		freeBlocks += uint64(free)
		inodesFree, dirs := uint32(0), uint32(0)
		for index := uint32(0); index < fs.sb.InodesPerGroup; index++ {
			number := group*fs.sb.InodesPerGroup + index + 1
			allocated, err := fs.inodeAllocated(number)
			if err != nil {
				return err
			}
			if !allocated {
				inodesFree++
				continue
			}
			if inode := check.inUse[number]; inode != nil && inode.IsDir() {
				dirs++
			}
			if desc.ItableUnused != 0 && index >= fs.sb.InodesPerGroup-desc.ItableUnused {
				return fmt.Errorf("inode %d in use inside the unused table tail", number)
			}
		}
		if inodesFree != desc.FreeInodes || dirs != desc.UsedDirs {
			return fmt.Errorf("group %d records %d free inodes and %d dirs, has %d and %d", group, desc.FreeInodes, desc.UsedDirs, inodesFree, dirs)
		}
		freeInodes += inodesFree
	}
	if freeBlocks != fs.sb.FreeBlocks || freeInodes != fs.sb.FreeInodes {
		return fmt.Errorf("superblock records %d/%d free, groups have %d/%d", fs.sb.FreeBlocks, fs.sb.FreeInodes, freeBlocks, freeInodes)
	}
	return nil
}

// namespace walks the tree from the root, checks each directory's dot
// entries, every name's type, that each name is found again by Lookup, and
// then link counts and that every in-use inode is reachable.
func (check *volumeCheck) namespace() error {
	fs := check.fs
	queue := []uint32{RootInode}
	seen := map[uint32]bool{RootInode: true}
	check.parents[RootInode] = RootInode
	check.names[RootInode] = 1
	for len(queue) > 0 {
		number := queue[0]
		queue = queue[1:]
		dir, err := fs.Inode(number)
		if err != nil {
			return fmt.Errorf("directory %d: %w", number, err)
		}
		parent, err := fs.dotDot(dir)
		if err != nil {
			return err
		}
		if parent != check.parents[number] {
			return fmt.Errorf("directory %d: dot-dot %d, reached from %d", number, parent, check.parents[number])
		}
		block, err := fs.dirBlock(dir, 0)
		if err != nil {
			return err
		}
		if le32(block, 0) != number || block[8] != '.' || block[6] != 1 {
			return fmt.Errorf("directory %d: dot entry wrong", number)
		}
		var children []DirEntry
		if _, err := fs.ReadDir(dir, 0, func(entry *DirEntry) bool {
			children = append(children, *entry)
			return true
		}); err != nil {
			return fmt.Errorf("directory %d: %w", number, err)
		}
		names := map[string]bool{}
		for index := range children {
			entry := &children[index]
			name := entry.Name()
			if names[string(name)] {
				return fmt.Errorf("directory %d lists %q twice", number, name)
			}
			names[string(name)] = true
			found, fileType, err := fs.Lookup(dir, name)
			if err != nil || found != entry.Inode || fileType != entry.Type {
				return fmt.Errorf("directory %d: lookup %q gave %d/%d %v, entry %d/%d", number, name, found, fileType, err, entry.Inode, entry.Type)
			}
			child := check.inUse[entry.Inode]
			if child == nil {
				return fmt.Errorf("directory %d: %q names free inode %d", number, name, entry.Inode)
			}
			if fileTypeOf(child.Mode) != entry.Type {
				return fmt.Errorf("directory %d: %q type %d, inode mode %o", number, name, entry.Type, child.Mode)
			}
			check.names[entry.Inode]++
			if child.IsDir() {
				if seen[entry.Inode] {
					return fmt.Errorf("directory %d reached twice", entry.Inode)
				}
				seen[entry.Inode] = true
				check.parents[entry.Inode] = number
				check.subdirs[number]++
				queue = append(queue, entry.Inode)
			}
		}
	}
	for number, inode := range check.inUse {
		if number == fs.sb.JournalInode {
			continue
		}
		links := check.names[number]
		if inode.IsDir() {
			want := 2 + check.subdirs[number]
			if int(inode.Links) != want && !(inode.Links == 1 && want >= maxLinks) {
				return fmt.Errorf("directory %d has %d links, wants %d", number, inode.Links, want)
			}
			if links != 1 {
				return fmt.Errorf("directory %d has %d names", number, links)
			}
			continue
		}
		if links == 0 {
			return fmt.Errorf("inode %d is in use but unreachable", number)
		}
		if int(inode.Links) != links {
			return fmt.Errorf("inode %d has %d links, %d names", number, inode.Links, links)
		}
	}
	return nil
}

// TestCheckerAcceptsFixtures keeps the oracle honest: every Linux image in
// the writable profile passes it unchanged.
func TestCheckerAcceptsFixtures(t *testing.T) {
	for _, name := range []string{"canonical", "hash-tea", "hash-legacy", "links"} {
		t.Run(name, func(t *testing.T) {
			checkVolume(t, fixture(t, name))
		})
	}
}

// setStrayBitmapBit marks one free block of group 0 used and reseals the
// bitmap and descriptor checksums, so only the counts disagree.
func setStrayBitmapBit(t testing.TB, device *memDevice) {
	t.Helper()
	fs := mount(t, device)
	desc, err := fs.group(0)
	if err != nil {
		t.Fatal(err)
	}
	offset := desc.BlockBitmap * uint64(fs.blockSize)
	bitmap := device.data[offset : offset+uint64(fs.blockSize)]
	bitmap[bytes.IndexByte(bitmap, 0)] = 1
	desc.BlockBitmapCsum = crc32c(fs.seed, bitmap[:fs.sb.ClustersPerGroup/8])
	block, within := fs.descriptorLocation(0)
	raw := device.data[block*4096+uint64(within):]
	putLE16(raw[0x18:], uint16(desc.BlockBitmapCsum))
	putLE16(raw[0x38:], uint16(desc.BlockBitmapCsum>>16))
	crc, _ := fs.descriptorChecksum(0, raw[:fs.sb.DescriptorSize])
	putLE16(raw[0x1E:], crc)
}

// TestCheckerCatchesDamage flips one bitmap bit and expects the oracle to
// notice.
func TestCheckerCatchesDamage(t *testing.T) {
	device := fixture(t, "canonical")
	setStrayBitmapBit(t, device)
	if err := verifyVolume(device); err == nil {
		t.Fatal("checker missed a stray bitmap bit")
	}
}
