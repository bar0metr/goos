package ext4

// Block and inode allocation. Every bitmap, descriptor and counter change
// goes through the open transaction; descriptor and bitmap checksums are
// recomputed whenever one changes.

const groupItableZeroed = 0x4

func setBit(bitmap []byte, bit uint64)       { bitmap[bit/8] |= 1 << (bit % 8) }
func clearBit(bitmap []byte, bit uint64)     { bitmap[bit/8] &^= 1 << (bit % 8) }
func testBit(bitmap []byte, bit uint64) bool { return bitmap[bit/8]&(1<<(bit%8)) != 0 }
func (fs *FS) groupStart(group uint32) uint64 {
	return uint64(fs.sb.FirstDataBlock) + uint64(group)*uint64(fs.sb.BlocksPerGroup)
}

// groupOf returns the group holding block.
func (fs *FS) groupOf(block uint64) uint32 {
	return uint32((block - uint64(fs.sb.FirstDataBlock)) / uint64(fs.sb.BlocksPerGroup))
}

// groupHasSuper reports whether group holds a superblock backup and a copy
// of the descriptor table: every group, or under sparse_super groups 0, 1
// and the powers of 3, 5 and 7.
func (fs *FS) groupHasSuper(group uint32) bool {
	if group <= 1 || fs.sb.Features.ROCompat&roCompatSparseSuper == 0 {
		return true
	}
	return isPowerOf(group, 3) || isPowerOf(group, 5) || isPowerOf(group, 7)
}

func isPowerOf(value uint32, base uint32) bool {
	for value%base == 0 {
		value /= base
	}
	return value == 1
}

// sealGroup recomputes the bitmap checksums and the descriptor checksum of
// group and stages the descriptor with desc's counts and flags. Bitmaps are
// read through the transaction, so staged changes are covered.
func (fs *FS) sealGroup(group uint32, desc *Group) error {
	wide := fs.sb.DescriptorSize >= 64
	if fs.sb.HasMetadataCsum() {
		if desc.Flags&groupBlockUninit == 0 {
			data, err := fs.cache.get(desc.BlockBitmap)
			if err != nil {
				return err
			}
			desc.BlockBitmapCsum = crc32c(fs.seed, data[:fs.sb.ClustersPerGroup/8])
		}
		if desc.Flags&groupInodeUninit == 0 {
			data, err := fs.cache.get(desc.InodeBitmap)
			if err != nil {
				return err
			}
			desc.InodeBitmapCsum = crc32c(fs.seed, data[:fs.sb.InodesPerGroup/8])
		}
		if !wide {
			desc.BlockBitmapCsum &= 0xFFFF
			desc.InodeBitmapCsum &= 0xFFFF
		}
	}
	block, offset := fs.descriptorLocation(group)
	data, err := fs.stage(block)
	if err != nil {
		return err
	}
	raw := data[offset : offset+int(fs.sb.DescriptorSize)]
	putLE16(raw[0xC:], uint16(desc.FreeBlocks))
	putLE16(raw[0xE:], uint16(desc.FreeInodes))
	putLE16(raw[0x10:], uint16(desc.UsedDirs))
	putLE16(raw[0x12:], desc.Flags)
	putLE16(raw[0x18:], uint16(desc.BlockBitmapCsum))
	putLE16(raw[0x1A:], uint16(desc.InodeBitmapCsum))
	putLE16(raw[0x1C:], uint16(desc.ItableUnused))
	if wide {
		putLE16(raw[0x2C:], uint16(desc.FreeBlocks>>16))
		putLE16(raw[0x2E:], uint16(desc.FreeInodes>>16))
		putLE16(raw[0x30:], uint16(desc.UsedDirs>>16))
		putLE16(raw[0x32:], uint16(desc.ItableUnused>>16))
		putLE16(raw[0x38:], uint16(desc.BlockBitmapCsum>>16))
		putLE16(raw[0x3A:], uint16(desc.InodeBitmapCsum>>16))
	}
	if crc, checked := fs.descriptorChecksum(group, raw); checked {
		putLE16(raw[0x1E:], crc)
	}
	return nil
}

// initBlockBitmap builds and stages the block bitmap of a group whose
// descriptor marks it uninitialized: the bits of its superblock backup,
// descriptor table copy and reserved descriptor blocks, of every group's
// bitmaps and inode table that lie inside it, and the padding past its last
// block. The result must agree with the descriptor's free count.
func (fs *FS) initBlockBitmap(group uint32, desc *Group) error {
	data, err := fs.stageZero(desc.BlockBitmap, true)
	if err != nil {
		return err
	}
	start, count := fs.groupStart(group), fs.groupBlocks(group)
	mark := func(first uint64, length uint64) {
		for block := first; block < first+length; block++ {
			if block >= start && block-start < count {
				setBit(data, block-start)
			}
		}
	}
	if fs.groupHasSuper(group) {
		tableBlocks := (fs.groups*uint64(fs.sb.DescriptorSize) + uint64(fs.blockSize) - 1) / uint64(fs.blockSize)
		first := start
		if group == 0 {
			first = 0
		}
		mark(first, start-first+1+tableBlocks+uint64(fs.sb.ReservedGDTBlocks))
	}
	itableBlocks := uint64(fs.sb.InodesPerGroup) * uint64(fs.sb.InodeSize) / uint64(fs.blockSize)
	for other := uint32(0); uint64(other) < fs.groups; other++ {
		otherDesc, err := fs.group(other)
		if err != nil {
			return err
		}
		mark(otherDesc.BlockBitmap, 1)
		mark(otherDesc.InodeBitmap, 1)
		mark(otherDesc.InodeTable, itableBlocks)
	}
	for bit := count; bit < uint64(fs.blockSize)*8; bit++ {
		setBit(data, bit)
	}
	free := uint64(0)
	for bit := uint64(0); bit < count; bit++ {
		if !testBit(data, bit) {
			free++
		}
	}
	if free != uint64(desc.FreeBlocks) {
		return corrupt("uninitialized group free count disagrees with its metadata")
	}
	desc.Flags &^= groupBlockUninit
	return nil
}

// initInodeBitmap stages an empty inode bitmap for a group whose descriptor
// marks it uninitialized; bits past the group's inodes are padding.
func (fs *FS) initInodeBitmap(desc *Group) error {
	data, err := fs.stageZero(desc.InodeBitmap, true)
	if err != nil {
		return err
	}
	for bit := uint64(fs.sb.InodesPerGroup); bit < uint64(fs.blockSize)*8; bit++ {
		setBit(data, bit)
	}
	desc.Flags &^= groupInodeUninit
	return nil
}

// availableBlocks is what an unprivileged allocation may still take: the
// free count less the blocks reserved for the superuser.
func (fs *FS) availableBlocks() uint64 {
	free := int64(fs.sb.FreeBlocks) + fs.txn.freeBlocks
	if free <= int64(fs.sb.ReservedBlocks) {
		return 0
	}
	return uint64(free) - fs.sb.ReservedBlocks
}

// allocBlocks allocates one run of at most want blocks, searching from goal
// and then the following groups. The run never crosses a group.
func (fs *FS) allocBlocks(goal uint64, want uint64) (uint64, uint64, error) {
	t := fs.txn
	if want == 0 {
		return 0, 0, fail(KindInternal, "empty allocation")
	}
	if available := fs.availableBlocks(); available == 0 {
		return 0, 0, fail(KindNoSpace, "no free blocks")
	} else if want > available {
		want = available
	}
	if goal <= uint64(fs.sb.FirstDataBlock) || goal >= fs.sb.BlocksCount {
		goal = uint64(fs.sb.FirstDataBlock) + 1
	}
	first := fs.groupOf(goal)
	for step := uint64(0); step < fs.groups; step++ {
		group := uint32((uint64(first) + step) % fs.groups)
		desc, err := fs.group(group)
		if err != nil {
			return 0, 0, err
		}
		if desc.FreeBlocks == 0 {
			continue
		}
		start, count := fs.groupStart(group), fs.groupBlocks(group)
		from := uint64(0)
		if step == 0 {
			from = goal - start
		}
		var bitmap []byte
		if desc.Flags&groupBlockUninit != 0 {
			if err := fs.initBlockBitmap(group, &desc); err != nil {
				return 0, 0, err
			}
			bitmap, err = fs.stage(desc.BlockBitmap)
		} else {
			bitmap, err = fs.cache.get(desc.BlockBitmap)
		}
		if err != nil {
			return 0, 0, err
		}
		bit, run := findRun(bitmap, from, count, want, start, t)
		if run == 0 && from > 0 {
			bit, run = findRun(bitmap, 0, count, want, start, t)
		}
		if run == 0 {
			continue
		}
		if run > uint64(desc.FreeBlocks) {
			return 0, 0, corrupt("group free block count below its bitmap's free run")
		}
		if bitmap, err = fs.stage(desc.BlockBitmap); err != nil {
			return 0, 0, err
		}
		for index := uint64(0); index < run; index++ {
			setBit(bitmap, bit+index)
		}
		desc.FreeBlocks -= uint32(run)
		if err := fs.sealGroup(group, &desc); err != nil {
			return 0, 0, err
		}
		t.freeBlocks -= int64(run)
		return start + bit, run, nil
	}
	return 0, 0, fail(KindNoSpace, "no free blocks")
}

// findRun returns the first free bit at or after from and the length of the
// free run there, at most want. Blocks the transaction freed count as used.
func findRun(bitmap []byte, from uint64, count uint64, want uint64, start uint64, t *txn) (uint64, uint64) {
	free := func(bit uint64) bool { return !testBit(bitmap, bit) && !t.wasFreed(start+bit) }
	for bit := from; bit < count; bit++ {
		if bitmap[bit/8] == 0xFF && bit%8 == 0 {
			bit += 7
			continue
		}
		if !free(bit) {
			continue
		}
		run := uint64(1)
		for run < want && bit+run < count && free(bit+run) {
			run++
		}
		return bit, run
	}
	return 0, 0
}

// freeBlocks returns a run to the bitmaps. Freeing a block that is already
// free is damage, not a no-op.
func (fs *FS) freeBlocks(start uint64, count uint64) error {
	t := fs.txn
	if start <= uint64(fs.sb.FirstDataBlock) || count == 0 {
		return corrupt("freeing a reserved block")
	}
	if end, ok := addOK(start, count); !ok || end > fs.sb.BlocksCount {
		return corrupt("freeing blocks beyond the volume")
	}
	for count > 0 {
		group := fs.groupOf(start)
		desc, err := fs.group(group)
		if err != nil {
			return err
		}
		if desc.Flags&groupBlockUninit != 0 {
			return corrupt("freeing a block in an uninitialized group")
		}
		first := start - fs.groupStart(group)
		chunk := fs.groupBlocks(group) - first
		if chunk > count {
			chunk = count
		}
		bitmap, err := fs.stage(desc.BlockBitmap)
		if err != nil {
			return err
		}
		for index := uint64(0); index < chunk; index++ {
			if !testBit(bitmap, first+index) {
				return corrupt("freeing a free block")
			}
			clearBit(bitmap, first+index)
		}
		if uint64(desc.FreeBlocks)+chunk > fs.groupBlocks(group) {
			return corrupt("group free block count above the group")
		}
		desc.FreeBlocks += uint32(chunk)
		if err := fs.sealGroup(group, &desc); err != nil {
			return err
		}
		if err := t.noteFreed(start, chunk); err != nil {
			return err
		}
		t.freeBlocks += int64(chunk)
		start += chunk
		count -= chunk
	}
	return nil
}

// allocInode allocates an inode number. A directory goes to a group with
// at least the average free inodes and the most free blocks, spreading
// directory trees; anything else starts at its parent's group.
func (fs *FS) allocInode(parent uint32, dir bool) (uint32, error) {
	t := fs.txn
	freeInodes := int64(fs.sb.FreeInodes) + t.freeInodes
	if freeInodes <= 0 {
		return 0, fail(KindNoSpace, "no free inodes")
	}
	first := (parent - 1) / fs.sb.InodesPerGroup
	if dir {
		average := uint64(freeInodes) / fs.groups
		best, bestFree := first, uint32(0)
		for group := uint32(0); uint64(group) < fs.groups; group++ {
			desc, err := fs.group(group)
			if err != nil {
				return 0, err
			}
			if desc.FreeInodes > 0 && uint64(desc.FreeInodes) >= average && desc.FreeBlocks > bestFree {
				best, bestFree = group, desc.FreeBlocks
			}
		}
		first = best
	}
	for step := uint64(0); step < fs.groups; step++ {
		group := uint32((uint64(first) + step) % fs.groups)
		desc, err := fs.group(group)
		if err != nil {
			return 0, err
		}
		if desc.FreeInodes == 0 {
			continue
		}
		if desc.Flags&groupBlockUninit != 0 {
			if err := fs.initBlockBitmap(group, &desc); err != nil {
				return 0, err
			}
		}
		var bitmap []byte
		if desc.Flags&groupInodeUninit != 0 {
			if err := fs.initInodeBitmap(&desc); err != nil {
				return 0, err
			}
		}
		if bitmap, err = fs.stage(desc.InodeBitmap); err != nil {
			return 0, err
		}
		lowest := uint32(0)
		if group == 0 {
			lowest = fs.sb.FirstInode - 1
		}
		for index := lowest; index < fs.sb.InodesPerGroup; index++ {
			if testBit(bitmap, uint64(index)) {
				continue
			}
			setBit(bitmap, uint64(index))
			desc.FreeInodes--
			if dir {
				desc.UsedDirs++
			}
			if used := fs.sb.InodesPerGroup - desc.ItableUnused; index >= used {
				desc.ItableUnused = fs.sb.InodesPerGroup - index - 1
			}
			if err := fs.sealGroup(group, &desc); err != nil {
				return 0, err
			}
			t.freeInodes--
			return group*fs.sb.InodesPerGroup + index + 1, nil
		}
		return 0, corrupt("group free inode count disagrees with its bitmap")
	}
	return 0, fail(KindNoSpace, "no free inodes")
}

// freeInode clears the inode's bitmap bit and records the deletion time.
// Its blocks must already be freed.
func (fs *FS) freeInode(inode *Inode) error {
	group, index, err := fs.inodeLocation(inode.Number)
	if err != nil {
		return err
	}
	desc, err := fs.group(group)
	if err != nil {
		return err
	}
	if desc.Flags&groupInodeUninit != 0 {
		return corrupt("freeing an inode in an uninitialized group")
	}
	bitmap, err := fs.stage(desc.InodeBitmap)
	if err != nil {
		return err
	}
	if !testBit(bitmap, uint64(index)) {
		return corrupt("freeing a free inode")
	}
	if desc.FreeInodes >= fs.sb.InodesPerGroup || (inode.IsDir() && desc.UsedDirs == 0) {
		return corrupt("group inode counts disagree with its bitmap")
	}
	clearBit(bitmap, uint64(index))
	desc.FreeInodes++
	if inode.IsDir() {
		desc.UsedDirs--
	}
	if err := fs.sealGroup(group, &desc); err != nil {
		return err
	}
	fs.txn.freeInodes++
	inode.Links = 0
	putLE32(inode.raw[0x14:], uint32(fs.now().Seconds))
	inode.dirty = true
	return nil
}

// verifyGroupCounts checks every initialized bitmap against its
// descriptor before a volume is opened for mutation: the free counts must
// equal the clear bits, padding bits must be set, and no inode in the
// unused table tail may be in use. The allocators trust the counts, so a
// checksum-consistent image whose counts disagree is refused here rather
// than letting a count wrap.
func (fs *FS) verifyGroupCounts() error {
	for group := uint32(0); uint64(group) < fs.groups; group++ {
		desc, err := fs.group(group)
		if err != nil {
			return err
		}
		if desc.Flags&groupBlockUninit == 0 {
			bitmap, err := fs.cache.get(desc.BlockBitmap)
			if err != nil {
				return err
			}
			count := fs.groupBlocks(group)
			if free := clearBits(bitmap, 0, count); free != uint64(desc.FreeBlocks) {
				return corrupt("group free block count disagrees with its bitmap")
			}
			if clearBits(bitmap, count, uint64(fs.blockSize)*8) != 0 {
				return corrupt("block bitmap padding bits clear")
			}
		}
		if desc.Flags&groupInodeUninit == 0 {
			bitmap, err := fs.cache.get(desc.InodeBitmap)
			if err != nil {
				return err
			}
			inodes := uint64(fs.sb.InodesPerGroup)
			if free := clearBits(bitmap, 0, inodes); free != uint64(desc.FreeInodes) {
				return corrupt("group free inode count disagrees with its bitmap")
			}
			if clearBits(bitmap, inodes, uint64(fs.blockSize)*8) != 0 {
				return corrupt("inode bitmap padding bits clear")
			}
			used := inodes - uint64(desc.ItableUnused)
			if clearBits(bitmap, used, inodes) != inodes-used {
				return corrupt("inode in use inside the unused table tail")
			}
		} else if desc.FreeInodes != fs.sb.InodesPerGroup || desc.UsedDirs != 0 {
			return corrupt("uninitialized inode group with inodes in use")
		}
	}
	return nil
}

// clearBits counts the clear bits in [from, to).
func clearBits(bitmap []byte, from uint64, to uint64) uint64 {
	clear := uint64(0)
	for bit := from; bit < to; bit++ {
		if !testBit(bitmap, bit) {
			clear++
		}
	}
	return clear
}
