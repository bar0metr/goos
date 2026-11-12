package ext4

const (
	inodeFlagIndexDir = inodeFlagIndex
	// maxLinks is the link count above which a directory under dir_nlink
	// records 1, meaning "not counted".
	maxLinks = 65000
	// ext4BaseExtraIsize is the size of the fields Linux defines after the
	// first 128 bytes of an inode.
	ext4BaseExtraIsize = 32
)

// liveInode returns the copy of in-use inode number that the running
// operation works on, loading it on first use. Every change goes to this
// copy, and commit writes the dirty ones back.
func (fs *FS) liveInode(number uint32) (*Inode, error) {
	if inode := fs.live[number]; inode != nil {
		return inode, nil
	}
	read := fs.Inode
	if fs.isOrphan(number) {
		read = fs.orphanInode
	}
	inode, err := read(number)
	if err != nil {
		return nil, err
	}
	fs.live[number] = inode
	return inode, nil
}

// liveMatch returns the live copy of given and refuses it when the inode
// was freed and reused since given was read.
func (fs *FS) liveMatch(given *Inode) (*Inode, error) {
	inode, err := fs.liveInode(given.Number)
	if err != nil {
		return nil, err
	}
	if inode.Generation != given.Generation {
		return nil, fail(KindStale, "inode generation changed")
	}
	return inode, nil
}

// flushInodes stages every dirty live inode.
func (fs *FS) flushInodes() error {
	for _, inode := range fs.live {
		if !inode.dirty {
			continue
		}
		if err := fs.putInode(inode); err != nil {
			return err
		}
		inode.dirty = false
	}
	return nil
}

// setChecksumSeed derives the seed of the inode's own checksums and those
// of the blocks it owns from its number and generation.
func (fs *FS) setChecksumSeed(inode *Inode) {
	if fs.sb.HasMetadataCsum() {
		inode.csumSeed = crc32cLE32(crc32cLE32(fs.seed, inode.Number), inode.Generation)
	}
}

// putInode encodes inode into its raw bytes and stages them in the inode
// table. Bytes the decoder does not interpret, in-inode extended
// attributes among them, are kept as read.
func (fs *FS) putInode(inode *Inode) error {
	if fs.sb.Features.ROCompat&roCompatHugeFile == 0 && inode.Blocks512 > 0xFFFFFFFF {
		return fail(KindNoSpace, "block count needs huge_file")
	}
	if inode.Blocks512 >= 1<<48 {
		return fail(KindNoSpace, "block count above 48 bits")
	}
	group, index, err := fs.inodeLocation(inode.Number)
	if err != nil {
		return err
	}
	desc, err := fs.group(group)
	if err != nil {
		return err
	}
	inodeSize := uint64(fs.sb.InodeSize)
	byteOffset := uint64(index) * inodeSize
	block := desc.InodeTable + byteOffset/uint64(fs.blockSize)
	within := int(byteOffset % uint64(fs.blockSize))
	data, err := fs.stage(block)
	if err != nil {
		return err
	}
	raw := inode.raw
	putLE16(raw[0x0:], inode.Mode)
	putLE16(raw[0x2:], uint16(inode.UID))
	putLE32(raw[0x4:], uint32(inode.Size))
	putLE16(raw[0x18:], uint16(inode.GID))
	putLE16(raw[0x1A:], inode.Links)
	putLE32(raw[0x1C:], uint32(inode.Blocks512))
	putLE32(raw[0x20:], inode.Flags)
	copy(raw[inodeBlockArea:inodeBlockArea+inodeBlockSize], inode.blockArea[:])
	putLE32(raw[0x64:], inode.Generation)
	putLE32(raw[0x68:], uint32(inode.FileACL))
	putLE32(raw[0x6C:], uint32(inode.Size>>32))
	putLE16(raw[0x74:], uint16(inode.Blocks512>>32))
	putLE16(raw[0x76:], uint16(inode.FileACL>>32))
	putLE16(raw[0x78:], uint16(inode.UID>>16))
	putLE16(raw[0x7A:], uint16(inode.GID>>16))
	extra := func(field int) bool { return goodOldIsize+int(inode.ExtraIsize) >= field+4 }
	encodeTime(raw, 0x8, 0x8C, inode.Access, extra(0x8C))
	encodeTime(raw, 0xC, 0x84, inode.Change, extra(0x84))
	encodeTime(raw, 0x10, 0x88, inode.Modify, extra(0x88))
	if extra(0x94) {
		encodeTime(raw, 0x90, 0x94, inode.Create, true)
	}
	if fs.sb.HasMetadataCsum() {
		crc, high := fs.inodeChecksum(inode, raw)
		putLE16(raw[inodeChecksumLo:], uint16(crc))
		if high {
			putLE16(raw[inodeChecksumHi:], uint16(crc>>16))
		}
	}
	copy(data[within:within+int(inodeSize)], raw)
	return nil
}

// encodeTime stores stamp as the signed 32-bit seconds field plus, when the
// inode has room, the extra word: two epoch bits and the nanoseconds.
func encodeTime(raw []byte, offset int, extraOffset int, stamp Timestamp, hasExtra bool) {
	low := uint32(stamp.Seconds)
	putLE32(raw[offset:], low)
	if hasExtra {
		epoch := uint32((stamp.Seconds-int64(int32(low)))>>32) & 3
		putLE32(raw[extraOffset:], stamp.Nanos<<2|epoch)
	}
}

// touch marks inode dirty and sets the times named by which: 'a' access,
// 'c' change, 'm' modification.
func (fs *FS) touch(inode *Inode, which string) {
	now := fs.now()
	for index := 0; index < len(which); index++ {
		switch which[index] {
		case 'a':
			inode.Access = now
		case 'c':
			inode.Change = now
		case 'm':
			inode.Modify = now
		}
	}
	inode.dirty = true
}

// incLinks adds a link. A directory past maxLinks under dir_nlink records 1.
func (fs *FS) incLinks(inode *Inode) error {
	switch {
	case inode.IsDir() && inode.Links == 1 && fs.sb.Features.ROCompat&roCompatDirNlink != 0:
	case inode.Links >= maxLinks:
		if !inode.IsDir() || fs.sb.Features.ROCompat&roCompatDirNlink == 0 {
			return fail(KindTooManyLinks, "")
		}
		inode.Links = 1
	default:
		inode.Links++
	}
	inode.dirty = true
	return nil
}

// decDirLinks drops the link a removed subdirectory held on its parent. A
// count of 1 means "not counted" and stays.
func decDirLinks(dir *Inode) {
	if dir.Links > 2 {
		dir.Links--
	}
	dir.dirty = true
}

// newInode builds a fresh inode for number, which the allocator has just
// marked in use. The generation is one past the slot's previous value, so
// a handle naming the inode's last life no longer matches.
func (fs *FS) newInode(number uint32, mode uint16, uid uint32, gid uint32) (*Inode, error) {
	group, index, err := fs.inodeLocation(number)
	if err != nil {
		return nil, err
	}
	desc, err := fs.group(group)
	if err != nil {
		return nil, err
	}
	inodeSize := uint64(fs.sb.InodeSize)
	byteOffset := uint64(index) * inodeSize
	data, err := fs.cache.get(desc.InodeTable + byteOffset/uint64(fs.blockSize))
	if err != nil {
		return nil, err
	}
	within := int(byteOffset % uint64(fs.blockSize))
	generation := le32(data, within+0x64) + 1
	if generation == 0 {
		generation = 1
	}
	inode := &Inode{
		Number:     number,
		Mode:       mode,
		UID:        uid,
		GID:        gid,
		Generation: generation,
		raw:        make([]byte, inodeSize),
		dirty:      true,
	}
	if inodeSize > goodOldIsize {
		extra := uint16(ext4BaseExtraIsize)
		if fs.sb.WantExtraIsize > extra {
			extra = fs.sb.WantExtraIsize
		}
		if uint64(goodOldIsize)+uint64(extra) > inodeSize {
			extra = uint16(inodeSize - goodOldIsize)
		}
		inode.ExtraIsize = extra
		putLE16(inode.raw[0x80:], extra)
	}
	inode.HasCreate = goodOldIsize+int(inode.ExtraIsize) >= 0x94+4
	now := fs.now()
	inode.Access, inode.Change, inode.Modify = now, now, now
	if inode.HasCreate {
		inode.Create = now
	}
	fs.setChecksumSeed(inode)
	fs.live[number] = inode
	return inode, nil
}

// initExtentRoot gives inode an empty extent tree.
func initExtentRoot(inode *Inode) {
	inode.Flags |= inodeFlagExtents
	area := inode.blockArea[:]
	for index := range area {
		area[index] = 0
	}
	putLE16(area[0:], extentMagic)
	putLE16(area[4:], (inodeBlockSize-12)/12)
	inode.dirty = true
}

// checkMutable refuses inodes whose layout the mutation code does not
// maintain.
func (fs *FS) checkMutable(inode *Inode) error {
	if inode.Flags&inodeFlagHugeFile != 0 {
		return fail(KindUnsupported, "huge_file inode block counts")
	}
	if inode.Flags&(inodeFlagVerity|inodeFlagEAInode) != 0 {
		return fail(KindUnsupported, "verity or attribute-value inode")
	}
	if inode.IsRegular() || inode.IsDir() {
		if inode.Flags&inodeFlagExtents == 0 {
			return fail(KindUnsupported, "block-mapped inode")
		}
	}
	if inode.IsSymlink() && inode.Flags&inodeFlagExtents == 0 && !fs.fastSymlink(inode) {
		return fail(KindUnsupported, "block-mapped symlink")
	}
	return nil
}

// fastSymlink reports whether a symlink keeps its target in the inode.
func (fs *FS) fastSymlink(inode *Inode) bool {
	eaBlocks := uint64(0)
	if inode.FileACL != 0 {
		eaBlocks = uint64(fs.blockSize / 512)
	}
	return inode.IsSymlink() && inode.Flags&inodeFlagExtents == 0 && inode.Blocks512 == eaBlocks
}
