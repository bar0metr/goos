package ext4

const (
	superblockOffset = 1024
	superblockSize   = 1024
	superMagic       = 0xEF53

	stateValid  = 0x1
	stateErrors = 0x2
	stateOrphan = 0x4

	flagSignedHash   = 0x1
	flagUnsignedHash = 0x2

	revisionDynamic = 1
	goodOldInodes   = 11
	goodOldIsize    = 128

	// Bounds this implementation accepts. They keep every derived count in
	// range of the arithmetic below and every table it walks finite.
	minLogBlock = 0 // 1 KiB
	maxLogBlock = 2 // 4 KiB
	// maxGroups bounds the descriptor table: 2^20 groups of 4 KiB blocks is
	// 512 TiB, and 2^20 descriptors of 64 bytes is 64 MiB of table that is
	// read through the block cache, never held whole.
	maxGroups = 1 << 20
)

// Superblock holds the decoded fields this implementation uses.
type Superblock struct {
	InodesCount      uint32
	BlocksCount      uint64
	ReservedBlocks   uint64
	FreeBlocks       uint64
	FreeInodes       uint32
	FirstDataBlock   uint32
	LogBlockSize     uint32
	LogClusterSize   uint32
	BlocksPerGroup   uint32
	ClustersPerGroup uint32
	InodesPerGroup   uint32
	MountTime        uint32
	WriteTime        uint32
	State            uint16
	Errors           uint16
	Revision         uint32
	FirstInode       uint32
	InodeSize        uint16
	// ReservedGDTBlocks are the descriptor blocks resize_inode keeps after
	// each descriptor table copy.
	ReservedGDTBlocks uint16
	Features          FeatureSet
	UUID              [16]byte
	VolumeName        [16]byte
	JournalInode      uint32
	JournalDevice     uint32
	LastOrphan        uint32
	HashSeed          [4]uint32
	DefaultHash       uint8
	DescriptorSize    uint16
	Flags             uint32
	MinExtraIsize     uint16
	WantExtraIsize    uint16
	LogGroupsPerFlex  uint8
	ChecksumType      uint8
	ChecksumSeed      uint32
	ErrorCount        uint32
	OrphanFileInode   uint32
	Checksum          uint32
	JournalBlocks     [17]uint32
}

// BlockSize is the filesystem block size in bytes.
func (sb *Superblock) BlockSize() uint32 { return 1024 << sb.LogBlockSize }

// GroupCount is the number of block groups the block count implies.
func (sb *Superblock) GroupCount() uint64 {
	data := sb.BlocksCount - uint64(sb.FirstDataBlock)
	return (data + uint64(sb.BlocksPerGroup) - 1) / uint64(sb.BlocksPerGroup)
}

// HasMetadataCsum reports whether structures carry CRC32C checksums.
func (sb *Superblock) HasMetadataCsum() bool {
	return sb.Features.ROCompat&roCompatMetadataCsum != 0
}

// NeedsRecovery reports whether the journal holds transactions to replay.
func (sb *Superblock) NeedsRecovery() bool {
	return sb.Features.Incompat&incompatRecover != 0
}

// ParseSuperblock decodes and validates the 1024-byte primary superblock.
// capacity is the size in bytes of the partition the volume lives in.
func ParseSuperblock(raw []byte, capacity uint64) (*Superblock, error) {
	return parseSuperblock(raw, capacity, true)
}

// parseSuperblock skips the checksum when checksum is false. Only the
// recovery bootstrap does that, to find the journal that may repair a
// superblock whose last write tore.
func parseSuperblock(raw []byte, capacity uint64, checksum bool) (*Superblock, error) {
	if len(raw) < superblockSize {
		return nil, corrupt("superblock truncated")
	}
	if le16(raw, 0x38) != superMagic {
		return nil, corrupt("bad superblock magic")
	}
	sb := &Superblock{
		InodesCount:       le32(raw, 0x0),
		BlocksCount:       uint64(le32(raw, 0x4)),
		ReservedBlocks:    uint64(le32(raw, 0x8)),
		FreeBlocks:        uint64(le32(raw, 0xC)),
		FreeInodes:        le32(raw, 0x10),
		FirstDataBlock:    le32(raw, 0x14),
		LogBlockSize:      le32(raw, 0x18),
		LogClusterSize:    le32(raw, 0x1C),
		BlocksPerGroup:    le32(raw, 0x20),
		ClustersPerGroup:  le32(raw, 0x24),
		InodesPerGroup:    le32(raw, 0x28),
		MountTime:         le32(raw, 0x2C),
		WriteTime:         le32(raw, 0x30),
		State:             le16(raw, 0x3A),
		Errors:            le16(raw, 0x3C),
		Revision:          le32(raw, 0x4C),
		FirstInode:        goodOldInodes,
		InodeSize:         goodOldIsize,
		JournalInode:      le32(raw, 0xE0),
		JournalDevice:     le32(raw, 0xE4),
		LastOrphan:        le32(raw, 0xE8),
		DefaultHash:       raw[0xFC],
		Flags:             le32(raw, 0x160),
		LogGroupsPerFlex:  raw[0x174],
		ChecksumType:      raw[0x175],
		ErrorCount:        le32(raw, 0x194),
		ChecksumSeed:      le32(raw, 0x270),
		OrphanFileInode:   le32(raw, 0x280),
		Checksum:          le32(raw, 0x3FC),
		ReservedGDTBlocks: le16(raw, 0xCE),
	}
	if sb.Revision >= revisionDynamic {
		sb.FirstInode = le32(raw, 0x54)
		sb.InodeSize = le16(raw, 0x58)
		sb.Features = FeatureSet{
			Compat:   le32(raw, 0x5C),
			Incompat: le32(raw, 0x60),
			ROCompat: le32(raw, 0x64),
		}
	}
	copy(sb.UUID[:], raw[0x68:0x78])
	copy(sb.VolumeName[:], raw[0x78:0x88])
	for index := range sb.HashSeed {
		sb.HashSeed[index] = le32(raw, 0xEC+4*index)
	}
	for index := range sb.JournalBlocks {
		sb.JournalBlocks[index] = le32(raw, 0x10C+4*index)
	}
	is64 := sb.Features.Incompat&incompat64Bit != 0
	if is64 {
		sb.DescriptorSize = le16(raw, 0xFE)
		sb.BlocksCount |= uint64(le32(raw, 0x150)) << 32
		sb.ReservedBlocks |= uint64(le32(raw, 0x154)) << 32
		sb.FreeBlocks |= uint64(le32(raw, 0x158)) << 32
	} else {
		sb.DescriptorSize = 32
	}
	if sb.Features.ROCompat&roCompatExtraIsize != 0 {
		sb.MinExtraIsize = le16(raw, 0x15C)
		sb.WantExtraIsize = le16(raw, 0x15E)
	}
	if sb.HasMetadataCsum() {
		if sb.ChecksumType != 1 {
			return nil, corrupt("unknown metadata checksum type")
		}
		if checksum && crc32c(^uint32(0), raw[:0x3FC]) != sb.Checksum {
			return nil, corrupt("superblock checksum mismatch")
		}
	}
	if err := sb.validate(capacity, is64); err != nil {
		return nil, err
	}
	return sb, nil
}

func (sb *Superblock) validate(capacity uint64, is64 bool) error {
	if sb.Revision > revisionDynamic {
		return fail(KindUnsupported, "superblock revision newer than dynamic")
	}
	if sb.LogBlockSize < minLogBlock || sb.LogBlockSize > maxLogBlock {
		return fail(KindUnsupported, "block size outside 1-4 KiB")
	}
	if sb.Features.ROCompat&roCompatBigalloc == 0 && sb.LogClusterSize != sb.LogBlockSize {
		return corrupt("cluster size differs from block size without bigalloc")
	}
	blockSize := uint64(sb.BlockSize())
	wantFirst := uint32(0)
	if blockSize == 1024 {
		wantFirst = 1
	}
	if sb.FirstDataBlock != wantFirst {
		return corrupt("first data block does not match block size")
	}
	if sb.BlocksPerGroup == 0 || uint64(sb.BlocksPerGroup) > 8*blockSize {
		return corrupt("blocks per group outside one bitmap block")
	}
	if sb.Features.ROCompat&roCompatBigalloc == 0 && sb.ClustersPerGroup != sb.BlocksPerGroup {
		return corrupt("clusters per group differs from blocks per group")
	}
	if sb.InodesPerGroup == 0 || uint64(sb.InodesPerGroup) > 8*blockSize {
		return corrupt("inodes per group outside one bitmap block")
	}
	if sb.InodeSize < goodOldIsize || sb.InodeSize&(sb.InodeSize-1) != 0 || uint64(sb.InodeSize) > blockSize {
		return corrupt("inode size is not a power of two between 128 and the block size")
	}
	if sb.InodeSize > goodOldIsize && uint32(sb.InodeSize) < goodOldIsize+4 {
		return corrupt("inode size leaves no room for extended fields")
	}
	if uint64(sb.InodesPerGroup)*uint64(sb.InodeSize)%blockSize != 0 {
		return corrupt("inode table does not fill whole blocks")
	}
	if sb.FirstInode < goodOldInodes || sb.FirstInode > sb.InodesCount {
		return corrupt("first non-reserved inode out of range")
	}
	if sb.BlocksCount <= uint64(sb.FirstDataBlock) {
		return corrupt("block count leaves no data blocks")
	}
	if !is64 && sb.BlocksCount > 0xFFFFFFFF {
		return corrupt("block count above 32 bits without 64bit")
	}
	if sb.ReservedBlocks > sb.BlocksCount || sb.FreeBlocks > sb.BlocksCount {
		return corrupt("reserved or free block count above block count")
	}
	if sb.FreeInodes > sb.InodesCount {
		return corrupt("free inode count above inode count")
	}
	size, ok := mulOK(sb.BlocksCount, blockSize)
	if !ok || size > capacity {
		return corrupt("volume larger than its partition")
	}
	groups := sb.GroupCount()
	if groups == 0 || groups > maxGroups {
		return fail(KindUnsupported, "group count outside the supported bound")
	}
	if inodes, _ := mulOK(groups, uint64(sb.InodesPerGroup)); inodes != uint64(sb.InodesCount) {
		return corrupt("inode count does not match groups times inodes per group")
	}
	if is64 {
		if sb.DescriptorSize < 64 || sb.DescriptorSize&(sb.DescriptorSize-1) != 0 || uint64(sb.DescriptorSize) > blockSize {
			return corrupt("64bit descriptor size invalid")
		}
	}
	if sb.Features.Compat&compatHasJournal != 0 {
		if sb.JournalDevice != 0 || sb.Features.Incompat&incompatJournalDev != 0 {
			return fail(KindUnsupported, "external journal")
		}
		if sb.JournalInode == 0 || sb.JournalInode > sb.InodesCount {
			return corrupt("journal inode out of range")
		}
	}
	if sb.Features.ROCompat&roCompatExtraIsize != 0 && sb.InodeSize > goodOldIsize {
		limit := uint32(sb.InodeSize) - goodOldIsize
		if uint32(sb.MinExtraIsize) > limit || uint32(sb.WantExtraIsize) > limit {
			return corrupt("extra inode size larger than the inode")
		}
	}
	return nil
}

// checksumSeed is the seed every metadata checksum chain starts from.
func (sb *Superblock) checksumSeed() uint32 {
	if sb.Features.Incompat&incompatCsumSeed != 0 {
		return sb.ChecksumSeed
	}
	return crc32c(^uint32(0), sb.UUID[:])
}

// Label is the volume name up to its first NUL.
func (sb *Superblock) Label() []byte {
	for index, value := range sb.VolumeName {
		if value == 0 {
			return sb.VolumeName[:index]
		}
	}
	return sb.VolumeName[:]
}
