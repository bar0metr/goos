package ext4

import "encoding/binary"

const (
	RootInode = 2

	modeTypeMask = 0xF000
	ModeFIFO     = 0x1000
	ModeChar     = 0x2000
	ModeDir      = 0x4000
	ModeBlock    = 0x6000
	ModeRegular  = 0x8000
	ModeSymlink  = 0xA000
	ModeSocket   = 0xC000

	inodeFlagEncrypt    = 0x800
	inodeFlagIndex      = 0x1000
	inodeFlagHugeFile   = 0x40000
	inodeFlagExtents    = 0x80000
	inodeFlagVerity     = 0x100000
	inodeFlagEAInode    = 0x200000
	inodeFlagInlineData = 0x10000000
	inodeFlagCasefold   = 0x40000000

	inodeChecksumLo = 0x7C
	inodeChecksumHi = 0x82
	inodeBlockArea  = 0x28
	inodeBlockSize  = 60
)

// Timestamp is a decoded inode time: seconds since the Unix epoch, with the
// extra epoch bits applied, and nanoseconds when the inode stores them.
type Timestamp struct {
	Seconds int64
	Nanos   uint32
}

// Inode holds the decoded fields of one inode.
type Inode struct {
	Number     uint32
	Mode       uint16
	UID        uint32
	GID        uint32
	Size       uint64
	Links      uint16
	Blocks512  uint64
	Flags      uint32
	Generation uint32
	FileACL    uint64
	Access     Timestamp
	Change     Timestamp
	Modify     Timestamp
	Create     Timestamp
	HasCreate  bool
	ExtraIsize uint16
	// blockArea is i_block: the extent root, block map or fast symlink.
	blockArea [inodeBlockSize]byte
	// raw holds the whole on-disk inode for extended attribute access.
	raw []byte
	// csumSeed starts the checksum chains of blocks this inode owns.
	csumSeed uint32
}

// Type is the file type bits of the mode.
func (inode *Inode) Type() uint16 { return inode.Mode & modeTypeMask }

func (inode *Inode) IsDir() bool { return inode.Type() == ModeDir }

func (inode *Inode) IsRegular() bool { return inode.Type() == ModeRegular }

func (inode *Inode) IsSymlink() bool { return inode.Type() == ModeSymlink }

// DeviceNumber returns the device a character or block device inode names,
// in Linux's 32-bit encoding (major in bits 8-19, minor in bits 0-7 and
// 20-31), and zero for every other kind. Linux writes the old 16-bit form to
// the first word of i_block when it fits and the new form to the second word
// otherwise; the old form is the new form's low half.
func (inode *Inode) DeviceNumber() uint32 {
	if inode.Type() != ModeChar && inode.Type() != ModeBlock {
		return 0
	}
	if old := binary.LittleEndian.Uint32(inode.blockArea[0:]); old != 0 {
		return old & 0xFFFF
	}
	return binary.LittleEndian.Uint32(inode.blockArea[4:])
}

// Permissions are the permission and set-id bits of the mode.
func (inode *Inode) Permissions() uint16 { return inode.Mode &^ modeTypeMask }

// inodeLocation returns the group and byte offset of inode number.
func (fs *FS) inodeLocation(number uint32) (uint32, uint32, error) {
	if number == 0 || number > fs.sb.InodesCount {
		return 0, 0, corrupt("inode number out of range")
	}
	index := number - 1
	return index / fs.sb.InodesPerGroup, index % fs.sb.InodesPerGroup, nil
}

// inodeAllocated reports whether the inode bitmap marks number in use.
func (fs *FS) inodeAllocated(number uint32) (bool, error) {
	group, index, err := fs.inodeLocation(number)
	if err != nil {
		return false, err
	}
	desc, err := fs.group(group)
	if err != nil {
		return false, err
	}
	if fs.groupsInitialized() {
		if desc.Flags&groupInodeUninit != 0 {
			return false, nil
		}
	}
	bitmap, err := fs.cache.get(desc.InodeBitmap)
	if err != nil {
		return false, err
	}
	return bitmap[index/8]&(1<<(index%8)) != 0, nil
}

// readInode loads, verifies and decodes inode number. It does not check
// that the inode is in use; Inode does.
func (fs *FS) readInode(number uint32) (*Inode, error) {
	group, index, err := fs.inodeLocation(number)
	if err != nil {
		return nil, err
	}
	desc, err := fs.group(group)
	if err != nil {
		return nil, err
	}
	inodeSize := uint32(fs.sb.InodeSize)
	byteOffset := uint64(index) * uint64(inodeSize)
	block := desc.InodeTable + byteOffset/uint64(fs.blockSize)
	within := int(byteOffset % uint64(fs.blockSize))
	data, err := fs.cache.get(block)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, inodeSize)
	copy(raw, data[within:within+int(inodeSize)])
	return fs.decodeInode(number, raw)
}

func (fs *FS) decodeInode(number uint32, raw []byte) (*Inode, error) {
	inode := &Inode{
		Number:     number,
		Mode:       le16(raw, 0x0),
		UID:        uint32(le16(raw, 0x2)) | uint32(le16(raw, 0x78))<<16,
		Size:       join64(le32(raw, 0x4), le32(raw, 0x6C)),
		GID:        uint32(le16(raw, 0x18)) | uint32(le16(raw, 0x7A))<<16,
		Links:      le16(raw, 0x1A),
		Blocks512:  join64(le32(raw, 0x1C), uint32(le16(raw, 0x74))),
		Flags:      le32(raw, 0x20),
		Generation: le32(raw, 0x64),
		FileACL:    join64(le32(raw, 0x68), uint32(le16(raw, 0x76))),
		raw:        raw,
	}
	copy(inode.blockArea[:], raw[inodeBlockArea:inodeBlockArea+inodeBlockSize])
	if len(raw) > goodOldIsize {
		inode.ExtraIsize = le16(raw, 0x80)
		if uint32(goodOldIsize)+uint32(inode.ExtraIsize) > uint32(len(raw)) || inode.ExtraIsize&3 != 0 {
			return nil, corrupt("inode extra size larger than the inode")
		}
	}
	if fs.sb.HasMetadataCsum() {
		inode.csumSeed = crc32cLE32(crc32cLE32(fs.seed, number), inode.Generation)
		if err := fs.verifyInodeChecksum(inode, raw); err != nil {
			return nil, err
		}
	}
	extra := func(field int) bool { return goodOldIsize+int(inode.ExtraIsize) >= field+4 }
	inode.Access = decodeTime(le32(raw, 0x8), raw, 0x8C, extra(0x8C))
	inode.Change = decodeTime(le32(raw, 0xC), raw, 0x84, extra(0x84))
	inode.Modify = decodeTime(le32(raw, 0x10), raw, 0x88, extra(0x88))
	if extra(0x94) {
		inode.Create = decodeTime(le32(raw, 0x90), raw, 0x94, true)
		inode.HasCreate = true
	}
	if inode.Flags&inodeFlagHugeFile != 0 && fs.sb.Features.ROCompat&roCompatHugeFile != 0 {
		inode.Blocks512 *= uint64(fs.blockSize / 512)
	}
	return inode, nil
}

// decodeTime applies the extra field: two epoch bits extend the signed
// 32-bit seconds, and the upper 30 bits hold nanoseconds.
func decodeTime(seconds uint32, raw []byte, extraOffset int, hasExtra bool) Timestamp {
	stamp := Timestamp{Seconds: int64(int32(seconds))}
	if hasExtra {
		extra := le32(raw, extraOffset)
		stamp.Seconds += int64(extra&3) << 32
		stamp.Nanos = extra >> 2
		if stamp.Nanos > 999999999 {
			stamp.Nanos = 0
		}
	}
	return stamp
}

func (fs *FS) verifyInodeChecksum(inode *Inode, raw []byte) error {
	hasHigh := len(raw) > goodOldIsize && goodOldIsize+int(inode.ExtraIsize) >= inodeChecksumHi+2
	crc := crc32c(inode.csumSeed, raw[:inodeChecksumLo])
	crc = crc32cZeros(crc, 2)
	crc = crc32c(crc, raw[inodeChecksumLo+2:goodOldIsize])
	if len(raw) > goodOldIsize {
		crc = crc32c(crc, raw[goodOldIsize:inodeChecksumHi])
		if hasHigh {
			crc = crc32cZeros(crc, 2)
			crc = crc32c(crc, raw[inodeChecksumHi+2:])
		} else {
			crc = crc32c(crc, raw[inodeChecksumHi:])
		}
	}
	stored := uint32(le16(raw, inodeChecksumLo))
	if hasHigh {
		stored |= uint32(le16(raw, inodeChecksumHi)) << 16
	} else {
		crc &= 0xFFFF
	}
	if crc != stored {
		return corrupt("inode checksum mismatch")
	}
	return nil
}

// Inode returns the in-use inode number. A free inode, one the bitmap does
// not mark, or one with no links is reported as stale: a directory entry or
// handle naming it refers to an object that no longer exists.
func (fs *FS) Inode(number uint32) (*Inode, error) {
	allocated, err := fs.inodeAllocated(number)
	if err != nil {
		return nil, err
	}
	if !allocated {
		return nil, fail(KindStale, "inode not allocated")
	}
	inode, err := fs.readInode(number)
	if err != nil {
		return nil, err
	}
	if inode.Mode == 0 || inode.Links == 0 {
		return nil, fail(KindStale, "inode has no links")
	}
	if err := fs.checkInode(inode); err != nil {
		return nil, err
	}
	return inode, nil
}

// checkInode refuses inodes whose layout this implementation cannot
// interpret safely.
func (fs *FS) checkInode(inode *Inode) error {
	switch inode.Type() {
	case ModeRegular, ModeDir, ModeSymlink, ModeFIFO, ModeChar, ModeBlock, ModeSocket:
	default:
		return corrupt("inode has an unknown file type")
	}
	if inode.Flags&inodeFlagInlineData != 0 {
		return fail(KindUnsupported, "inline data")
	}
	if inode.Flags&inodeFlagEncrypt != 0 {
		return fail(KindUnsupported, "encrypted inode")
	}
	if inode.Flags&inodeFlagCasefold != 0 {
		return fail(KindUnsupported, "case-insensitive directory")
	}
	if inode.Flags&inodeFlagExtents != 0 && fs.sb.Features.Incompat&incompatExtents == 0 {
		return corrupt("extent inode without the extent feature")
	}
	if inode.IsDir() && fs.sb.Features.ROCompat&roCompatLargeFile == 0 && inode.Size > 0xFFFFFFFF {
		return corrupt("directory size above 4 GiB")
	}
	return nil
}
