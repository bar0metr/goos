package ext4

// The JBD2 log format, shared by the commit writer and replay. A journal
// block starts with a 12-byte big-endian header: magic, block type and the
// sequence number of its transaction. A transaction is one or more
// descriptor blocks, each followed by the copies of the metadata blocks its
// tags name, any revoke blocks, and a commit block. Field offsets follow the
// Linux 6.12 documentation (docs/development/ext4-reference-versions.md).

const (
	jbd2Descriptor = 1
	jbd2Commit     = 2
	jbd2Revoke     = 5

	jbd2FlagEscape   = 0x1
	jbd2FlagSameUUID = 0x2
	jbd2FlagLastTag  = 0x8

	jbd2HeaderBytes  = 12
	jbd2RevokeHeader = 16
	jbd2TailBytes    = 4
	jbd2UUIDBytes    = 16

	// jbd2CompatChecksum is the CRC32 commit checksum that predates
	// checksum v2. Replay refuses it rather than skipping the check.
	jbd2CompatChecksum = 0x1
	// jbd2KnownIncompat lists the incompat bits replay interprets.
	jbd2KnownIncompat = jbd2IncompatRevoke | jbd2Incompat64Bit | jbd2IncompatCsumV2 | jbd2IncompatCsumV3

	// maxLogRuns bounds the extents the journal inode may be split into.
	maxLogRuns = 1024
	// maxRevokes bounds the revoke table replay builds from the log.
	maxRevokes = 1 << 20
)

// logRun maps journal blocks [logical, logical+length) to the volume.
type logRun struct {
	logical  uint64
	physical uint64
	length   uint64
}

// journalLog is the journal as the writer and replay use it.
type journalLog struct {
	runs  []logRun
	first uint32
	// last is one past the final log block (s_maxlen).
	last uint32
	// super is the whole first block of the journal; its first 1024
	// bytes are the journal superblock.
	super      []byte
	superBlock uint64
	seed       uint32
	csum       int
	is64       bool
	// tail and tailSeq are what the journal superblock on disk records as
	// the start of the log; head and seq are where and with which number
	// the next transaction goes.
	tail    uint32
	tailSeq uint32
	head    uint32
	seq     uint32
	// journaled maps each block copied into the log since tail to the
	// last transaction that copied it. A block freed while it is listed
	// gets a revoke record, so replay never writes an old copy over the
	// block's next owner.
	journaled map[uint64]uint32
}

// tidAfter reports whether transaction a comes after b, allowing the
// sequence to wrap.
func tidAfter(a uint32, b uint32) bool { return int32(a-b) > 0 }

// loadLog maps the journal inode and reads the journal superblock that
// openJournal validated.
func (fs *FS) loadLog() (*journalLog, error) {
	inode, err := fs.Inode(fs.sb.JournalInode)
	if err != nil {
		return nil, err
	}
	journal := &fs.journal
	log := &journalLog{first: journal.First, last: journal.MaxLen}
	for logical := uint64(0); logical < uint64(journal.MaxLen); {
		mapping, err := fs.mapBlock(inode, logical)
		if err != nil {
			return nil, err
		}
		if mapping.Hole || mapping.Unwritten || mapping.Length == 0 {
			return nil, corrupt("journal inode has a hole")
		}
		length := mapping.Length
		if rest := uint64(journal.MaxLen) - logical; length > rest {
			length = rest
		}
		if end, ok := addOK(mapping.Physical, length); !ok || end > fs.sb.BlocksCount {
			return nil, corrupt("journal block beyond the volume")
		}
		if n := len(log.runs); n > 0 && log.runs[n-1].physical+log.runs[n-1].length == mapping.Physical {
			log.runs[n-1].length += length
		} else {
			if len(log.runs) >= maxLogRuns {
				return nil, fail(KindUnsupported, "journal inode too fragmented")
			}
			log.runs = append(log.runs, logRun{logical: logical, physical: mapping.Physical, length: length})
		}
		logical += length
	}
	log.superBlock = log.runs[0].physical
	log.super = make([]byte, fs.blockSize)
	if !fs.device.ReadAt(log.superBlock*uint64(fs.blockSize), log.super) {
		return nil, errIO
	}
	if be32(log.super, 4) != jbd2SuperblockV2 {
		return nil, fail(KindUnsupported, "version 1 journal superblock")
	}
	if journal.Compat&jbd2CompatChecksum != 0 {
		return nil, fail(KindUnsupported, "journal uses CRC32 commit checksums")
	}
	if journal.Incompat&^jbd2KnownIncompat != 0 {
		return nil, fail(KindUnsupported, "journal has async commits, fast commits or unknown features")
	}
	if journal.Errno != 0 {
		return nil, fail(KindNeedsRecovery, "journal recorded an abort")
	}
	log.setFeatures(journal.Incompat)
	log.tail, log.tailSeq = journal.Start, journal.Sequence
	return log, nil
}

// setFeatures derives the tag layout and checksum version from the
// journal's incompat word.
func (log *journalLog) setFeatures(incompat uint32) {
	log.is64 = incompat&jbd2Incompat64Bit != 0
	log.csum = 0
	switch {
	case incompat&jbd2IncompatCsumV3 != 0:
		log.csum = 3
	case incompat&jbd2IncompatCsumV2 != 0:
		log.csum = 2
	}
	log.seed = crc32c(^uint32(0), log.super[0x30:0x40])
}

// size is the number of log blocks after the superblock area.
func (log *journalLog) size() uint32 { return log.last - log.first }

// physical returns the volume block holding log block position.
func (log *journalLog) physical(position uint32) uint64 {
	for index := range log.runs {
		run := &log.runs[index]
		if uint64(position)-run.logical < run.length {
			return run.physical + uint64(position) - run.logical
		}
	}
	return 0
}

// next returns the log block after position, wrapping at the end.
func (log *journalLog) next(position uint32) uint32 {
	position++
	if position >= log.last {
		position = log.first
	}
	return position
}

// used counts the blocks between tail and head.
func (log *journalLog) used() uint32 {
	if log.head >= log.tail {
		return log.head - log.tail
	}
	return log.size() - (log.tail - log.head)
}

// inLog reports whether volume block lies inside the journal.
func (log *journalLog) inLog(block uint64) bool {
	for index := range log.runs {
		run := &log.runs[index]
		if block-run.physical < run.length {
			return true
		}
	}
	return false
}

// tagBytes is the size of one descriptor tag, without the UUID that
// follows a tag lacking jbd2FlagSameUUID.
func (log *journalLog) tagBytes() int {
	switch {
	case log.csum == 3:
		return 16
	case log.csum == 2 && log.is64:
		return 14
	case log.csum == 2:
		return 10
	case log.is64:
		return 12
	}
	return 8
}

func (log *journalLog) tailBytes() int {
	if log.csum != 0 {
		return jbd2TailBytes
	}
	return 0
}

// recordBytes is the size of one revoke record.
func (log *journalLog) recordBytes() int {
	if log.is64 {
		return 8
	}
	return 4
}

// blockChecksum is the CRC32C of a descriptor, revoke or commit block with
// the checksum field at offset zeroed.
func (log *journalLog) blockChecksum(data []byte, offset int) uint32 {
	crc := crc32c(log.seed, data[:offset])
	crc = crc32cZeros(crc, 4)
	return crc32c(crc, data[offset+4:])
}

// tailOK checks the checksum at the end of a descriptor or revoke block.
func (log *journalLog) tailOK(data []byte) bool {
	if log.csum == 0 {
		return true
	}
	offset := len(data) - jbd2TailBytes
	return log.blockChecksum(data, offset) == be32(data, offset)
}

func (log *journalLog) sealTail(data []byte) {
	if log.csum != 0 {
		offset := len(data) - jbd2TailBytes
		putBE32(data[offset:], log.blockChecksum(data, offset))
	}
}

// commitOK checks a commit block's checksum, kept in its first h_chksum
// word with the type and size bytes zero.
func (log *journalLog) commitOK(data []byte) bool {
	if log.csum == 0 {
		return true
	}
	return log.blockChecksum(data, 0x10) == be32(data, 0x10)
}

// tagChecksum covers the transaction sequence and the block as the log
// holds it, escaped.
func (log *journalLog) tagChecksum(seq uint32, data []byte) uint32 {
	var raw [4]byte
	putBE32(raw[:], seq)
	return crc32c(crc32c(log.seed, raw[:]), data)
}

// journalTag is one decoded descriptor tag.
type journalTag struct {
	block    uint64
	flags    uint32
	checksum uint32
}

// decodeTag reads the tag at data[0:tagBytes].
func (log *journalLog) decodeTag(data []byte) journalTag {
	tag := journalTag{block: uint64(be32(data, 0))}
	var high uint32
	if log.csum == 3 {
		tag.flags, high, tag.checksum = be32(data, 4), be32(data, 8), be32(data, 12)
	} else {
		tag.checksum, tag.flags = uint32(be16(data, 4)), uint32(be16(data, 6))
		if log.is64 {
			high = be32(data, 8)
		}
	}
	if log.is64 {
		tag.block |= uint64(high) << 32
	}
	return tag
}

// tagOK checks a data block against its tag. Checksum v2 keeps the low 16
// bits of the CRC.
func (log *journalLog) tagOK(tag journalTag, seq uint32, data []byte) bool {
	switch log.csum {
	case 3:
		return log.tagChecksum(seq, data) == tag.checksum
	case 2:
		return log.tagChecksum(seq, data)&0xFFFF == tag.checksum
	}
	return true
}

func putHeader(data []byte, blockType uint32, seq uint32) {
	putBE32(data, jbd2Magic)
	putBE32(data[4:], blockType)
	putBE32(data[8:], seq)
}

// writeSuper records start and seq in the journal superblock and writes
// it. A start of zero marks the journal empty.
func (fs *FS) writeLogSuper(start uint32, seq uint32) bool {
	log := fs.log
	raw := log.super
	putBE32(raw[0x18:], seq)
	putBE32(raw[0x1C:], start)
	if log.csum != 0 {
		crc := crc32c(^uint32(0), raw[:0xFC])
		crc = crc32cZeros(crc, 4)
		crc = crc32c(crc, raw[0x100:jbd2SuperblockBytes])
		putBE32(raw[0xFC:], crc)
	}
	if !fs.device.WriteAt(log.superBlock*uint64(fs.blockSize), raw) {
		return false
	}
	log.tail, log.tailSeq = start, seq
	return true
}

// enableFeatures sets the journal features the writer uses: revoke
// records always, 64-bit tags on a 64bit volume, and checksum v3 on a
// metadata_csum volume and none otherwise, as Linux does. It runs only
// while the log is empty.
func (fs *FS) enableFeatures() {
	log := fs.log
	raw := log.super
	incompat := be32(raw, 0x28) | jbd2IncompatRevoke
	if fs.sb.Features.Incompat&incompat64Bit != 0 {
		incompat |= jbd2Incompat64Bit
	}
	incompat &^= jbd2IncompatCsumV2 | jbd2IncompatCsumV3
	if fs.sb.HasMetadataCsum() {
		incompat |= jbd2IncompatCsumV3
		raw[0x50] = jbd2ChecksumCRC32C
	}
	putBE32(raw[0x28:], incompat)
	fs.journal.Incompat = incompat
	log.setFeatures(incompat)
}
