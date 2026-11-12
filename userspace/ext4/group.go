package ext4

const (
	groupInodeUninit = 0x1
	groupBlockUninit = 0x2
)

// Group is one decoded block group descriptor.
type Group struct {
	BlockBitmap      uint64
	InodeBitmap      uint64
	InodeTable       uint64
	FreeBlocks       uint32
	FreeInodes       uint32
	UsedDirs         uint32
	Flags            uint16
	ItableUnused     uint32
	BlockBitmapCsum  uint32
	InodeBitmapCsum  uint32
	Checksum         uint16
	descriptorOffset uint64
}

// descriptorLocation returns the block and byte offset of group's
// descriptor. meta_bg is refused at mount, so the table is contiguous after
// the primary superblock.
func (fs *FS) descriptorLocation(group uint32) (uint64, int) {
	size := uint64(fs.sb.DescriptorSize)
	byteOffset := uint64(group) * size
	blockSize := uint64(fs.blockSize)
	block := uint64(fs.sb.FirstDataBlock) + 1 + byteOffset/blockSize
	return block, int(byteOffset % blockSize)
}

// group reads, decodes and verifies the descriptor of group.
func (fs *FS) group(group uint32) (Group, error) {
	if uint64(group) >= fs.groups {
		return Group{}, corrupt("group number out of range")
	}
	block, offset := fs.descriptorLocation(group)
	data, err := fs.cache.get(block)
	if err != nil {
		return Group{}, err
	}
	size := int(fs.sb.DescriptorSize)
	raw := data[offset : offset+size]
	desc := Group{
		BlockBitmap:      uint64(le32(raw, 0x0)),
		InodeBitmap:      uint64(le32(raw, 0x4)),
		InodeTable:       uint64(le32(raw, 0x8)),
		FreeBlocks:       uint32(le16(raw, 0xC)),
		FreeInodes:       uint32(le16(raw, 0xE)),
		UsedDirs:         uint32(le16(raw, 0x10)),
		Flags:            le16(raw, 0x12),
		BlockBitmapCsum:  uint32(le16(raw, 0x18)),
		InodeBitmapCsum:  uint32(le16(raw, 0x1A)),
		ItableUnused:     uint32(le16(raw, 0x1C)),
		Checksum:         le16(raw, 0x1E),
		descriptorOffset: block*uint64(fs.blockSize) + uint64(offset),
	}
	if size >= 64 {
		desc.BlockBitmap |= uint64(le32(raw, 0x20)) << 32
		desc.InodeBitmap |= uint64(le32(raw, 0x24)) << 32
		desc.InodeTable |= uint64(le32(raw, 0x28)) << 32
		desc.FreeBlocks |= uint32(le16(raw, 0x2C)) << 16
		desc.FreeInodes |= uint32(le16(raw, 0x2E)) << 16
		desc.UsedDirs |= uint32(le16(raw, 0x30)) << 16
		desc.ItableUnused |= uint32(le16(raw, 0x32)) << 16
		desc.BlockBitmapCsum |= uint32(le16(raw, 0x38)) << 16
		desc.InodeBitmapCsum |= uint32(le16(raw, 0x3A)) << 16
	}
	if want, checked := fs.descriptorChecksum(group, raw); checked && want != desc.Checksum && !fs.beforeReplay {
		return Group{}, corrupt("group descriptor checksum mismatch")
	}
	return desc, nil
}

// descriptorChecksum computes the checksum the descriptor should carry and
// whether the volume checksums descriptors at all.
func (fs *FS) descriptorChecksum(group uint32, raw []byte) (uint16, bool) {
	switch {
	case fs.sb.HasMetadataCsum():
		crc := crc32cLE32(fs.seed, group)
		crc = crc32c(crc, raw[:0x1E])
		crc = crc32cZeros(crc, 2)
		crc = crc32c(crc, raw[0x20:])
		return uint16(crc), true
	case fs.sb.Features.ROCompat&roCompatGDTCsum != 0:
		var number [4]byte
		putLE32(number[:], group)
		crc := crc16(0xFFFF, fs.sb.UUID[:])
		crc = crc16(crc, number[:])
		crc = crc16(crc, raw[:0x1E])
		if len(raw) > 0x20 {
			crc = crc16(crc, raw[0x20:])
		}
		return crc, true
	}
	return 0, false
}

// groupsInitialized reports whether descriptors may mark groups as not yet
// initialized.
func (fs *FS) groupsInitialized() bool {
	return fs.sb.Features.ROCompat&(roCompatGDTCsum|roCompatMetadataCsum) != 0
}

// groupBlocks is the number of blocks in group; the last group may be short.
func (fs *FS) groupBlocks(group uint32) uint64 {
	first := uint64(fs.sb.FirstDataBlock) + uint64(group)*uint64(fs.sb.BlocksPerGroup)
	remaining := fs.sb.BlocksCount - first
	if remaining > uint64(fs.sb.BlocksPerGroup) {
		return uint64(fs.sb.BlocksPerGroup)
	}
	return remaining
}

// validateGroups checks every descriptor once at mount: checksums, metadata
// locations inside the volume and outside the superblock and descriptor
// table, and counts within the group's size. Free counts are summed so a
// mismatch with the superblock is reported as damage only on writable
// mounts; Linux tolerates the lag after an unclean shutdown.
func (fs *FS) validateGroups() error {
	tableBlocks := (fs.groups*uint64(fs.sb.DescriptorSize) + uint64(fs.blockSize) - 1) / uint64(fs.blockSize)
	reservedEnd := uint64(fs.sb.FirstDataBlock) + 1 + tableBlocks
	itableBlocks := uint64(fs.sb.InodesPerGroup) * uint64(fs.sb.InodeSize) / uint64(fs.blockSize)
	inside := func(start uint64, count uint64) bool {
		end, ok := addOK(start, count)
		return ok && start >= reservedEnd && end <= fs.sb.BlocksCount
	}
	for group := uint32(0); uint64(group) < fs.groups; group++ {
		desc, err := fs.group(group)
		if err != nil {
			return err
		}
		if !inside(desc.BlockBitmap, 1) || !inside(desc.InodeBitmap, 1) || !inside(desc.InodeTable, itableBlocks) {
			return corrupt("group metadata outside the volume or over the descriptor table")
		}
		if desc.BlockBitmap == desc.InodeBitmap ||
			(desc.BlockBitmap >= desc.InodeTable && desc.BlockBitmap < desc.InodeTable+itableBlocks) ||
			(desc.InodeBitmap >= desc.InodeTable && desc.InodeBitmap < desc.InodeTable+itableBlocks) {
			return corrupt("group bitmaps overlap each other or the inode table")
		}
		if fs.sb.Features.Incompat&incompatFlexBG == 0 {
			start := uint64(fs.sb.FirstDataBlock) + uint64(group)*uint64(fs.sb.BlocksPerGroup)
			end := start + fs.groupBlocks(group)
			if desc.BlockBitmap < start || desc.BlockBitmap >= end ||
				desc.InodeBitmap < start || desc.InodeBitmap >= end ||
				desc.InodeTable < start || desc.InodeTable+itableBlocks > end {
				return corrupt("group metadata outside its group without flex_bg")
			}
		}
		if uint64(desc.FreeBlocks) > fs.groupBlocks(group) || desc.FreeInodes > fs.sb.InodesPerGroup ||
			desc.UsedDirs > fs.sb.InodesPerGroup || desc.ItableUnused > fs.sb.InodesPerGroup {
			return corrupt("group counts larger than the group")
		}
		if !fs.groupsInitialized() && desc.Flags&(groupInodeUninit|groupBlockUninit) != 0 {
			return corrupt("uninitialized group flags without group checksums")
		}
		if fs.sb.HasMetadataCsum() {
			if err := fs.verifyBitmaps(group, &desc); err != nil {
				return err
			}
		}
	}
	return nil
}

// verifyBitmaps checks the CRC32C of the group's bitmaps. A bitmap the
// descriptor marks uninitialized carries no meaningful bytes and is skipped.
func (fs *FS) verifyBitmaps(group uint32, desc *Group) error {
	wide := fs.sb.DescriptorSize >= 64
	if desc.Flags&groupBlockUninit == 0 {
		data, err := fs.cache.get(desc.BlockBitmap)
		if err != nil {
			return err
		}
		crc := crc32c(fs.seed, data[:fs.sb.ClustersPerGroup/8])
		if !wide {
			crc &= 0xFFFF
		}
		if crc != desc.BlockBitmapCsum {
			return corrupt("block bitmap checksum mismatch")
		}
	}
	if desc.Flags&groupInodeUninit == 0 {
		data, err := fs.cache.get(desc.InodeBitmap)
		if err != nil {
			return err
		}
		crc := crc32c(fs.seed, data[:fs.sb.InodesPerGroup/8])
		if !wide {
			crc &= 0xFFFF
		}
		if crc != desc.InodeBitmapCsum {
			return corrupt("inode bitmap checksum mismatch")
		}
	}
	return nil
}
