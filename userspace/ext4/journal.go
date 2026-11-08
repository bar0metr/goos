package ext4

// The JBD2 journal superblock. Every journal field is big-endian, unlike the
// filesystem it lives in. Mounting only inspects it: a journal with
// transactions to replay makes the volume unmountable until a recovery
// mount replays it.

const (
	jbd2Magic              = 0xC03B3998
	jbd2SuperblockV1       = 3
	jbd2SuperblockV2       = 4
	jbd2IncompatRevoke     = 0x1
	jbd2Incompat64Bit      = 0x2
	jbd2IncompatAsyncCommt = 0x4
	jbd2IncompatCsumV2     = 0x8
	jbd2IncompatCsumV3     = 0x10
	jbd2IncompatFastCommit = 0x20
	jbd2ChecksumCRC32C     = 4
	jbd2SuperblockBytes    = 1024
)

// Journal describes the internal journal as its superblock records it.
type Journal struct {
	Present   bool
	BlockSize uint32
	MaxLen    uint32
	First     uint32
	Sequence  uint32
	// Start is the first block of the oldest live transaction; zero means
	// the journal is empty.
	Start         uint32
	Errno         int32
	Compat        uint32
	Incompat      uint32
	ROCompat      uint32
	FastCommitLen uint32
}

// Clean reports whether the journal holds nothing to replay.
func (journal *Journal) Clean() bool { return journal.Start == 0 }

// openJournal reads the internal journal's superblock from the journal
// inode's first block.
func (fs *FS) openJournal() error {
	if fs.sb.Features.Compat&compatHasJournal == 0 {
		return nil
	}
	inode, err := fs.Inode(fs.sb.JournalInode)
	if err != nil {
		return err
	}
	if !inode.IsRegular() {
		return corrupt("journal inode is not a regular file")
	}
	mapping, err := fs.mapBlock(inode, 0)
	if err != nil {
		return err
	}
	if mapping.Hole || mapping.Unwritten {
		return corrupt("journal superblock not allocated")
	}
	data, err := fs.cache.get(mapping.Physical)
	if err != nil {
		return err
	}
	if be32(data, 0) != jbd2Magic {
		return corrupt("bad journal magic")
	}
	blockType := be32(data, 4)
	if blockType != jbd2SuperblockV1 && blockType != jbd2SuperblockV2 {
		return corrupt("journal superblock type invalid")
	}
	journal := Journal{
		Present:   true,
		BlockSize: be32(data, 0xC),
		MaxLen:    be32(data, 0x10),
		First:     be32(data, 0x14),
		Sequence:  be32(data, 0x18),
		Start:     be32(data, 0x1C),
		Errno:     int32(be32(data, 0x20)),
	}
	if blockType == jbd2SuperblockV2 {
		journal.Compat = be32(data, 0x24)
		journal.Incompat = be32(data, 0x28)
		journal.ROCompat = be32(data, 0x2C)
		journal.FastCommitLen = be32(data, 0x54)
	}
	if journal.BlockSize != fs.blockSize {
		return corrupt("journal block size differs from the filesystem")
	}
	if journal.First == 0 || journal.First >= journal.MaxLen || uint64(journal.MaxLen)*uint64(fs.blockSize) > inode.Size {
		return corrupt("journal geometry larger than the journal inode")
	}
	if journal.Start != 0 && (journal.Start < journal.First || journal.Start >= journal.MaxLen) {
		return corrupt("journal start outside the log")
	}
	if journal.Incompat&(jbd2IncompatCsumV2|jbd2IncompatCsumV3) != 0 {
		if data[0x50] != jbd2ChecksumCRC32C {
			return corrupt("journal checksum type unknown")
		}
		crc := crc32c(^uint32(0), data[:0xFC])
		crc = crc32cZeros(crc, 4)
		crc = crc32c(crc, data[0x100:jbd2SuperblockBytes])
		if crc != be32(data, 0xFC) {
			return corrupt("journal superblock checksum mismatch")
		}
	}
	fs.journal = journal
	return nil
}
