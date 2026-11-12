package ext4

// Journal replay. Like Linux, replay reads the log three times from the
// start the journal superblock records. The scan finds the last
// transaction with a valid commit block; the revoke pass collects revoke
// records from the committed transactions; the replay pass writes every
// logged block a later revoke does not cancel to its home location.
//
// The scan stops at the first block that does not continue the log: a
// missing or foreign block, a sequence number out of place, a descriptor,
// revoke or commit block that fails its checksum, or a descriptor without
// a last tag. Such a block is either the torn or unwritten tail of a
// transaction that never committed, or damage to one that did. The writer
// tells them apart for us: it writes a transaction's commit block only
// after a flush made the rest of the transaction durable, and it starts the
// next transaction only after a second flush made the commit durable. So
// when the log past the stop still holds a valid commit block of the
// stopped transaction, or a valid block of the next one, the stopped
// transaction had committed, and replay refuses the volume as corrupt
// rather than drop it. A committed transaction whose data block fails its
// tag checksum, or names a block outside the filesystem or inside the
// journal, is damage too.
//
// Replay only rewrites blocks, so a power cut during replay leaves the log
// as it was and the next mount replays it again.

const (
	passScan = iota
	passRevoke
	passReplay
)

// Replay reports what a writable mount found in the journal.
type Replay struct {
	// Transactions is the number of committed transactions replayed.
	Transactions uint32
	// Blocks is the number of logged blocks written home.
	Blocks uint64
	// Revoked is the number of logged blocks a revoke record cancelled.
	Revoked uint64
	// Orphans is the number of inodes the orphan list cleanup finished.
	Orphans uint32
}

type logWalk struct {
	fs      *FS
	log     *journalLog
	pass    int
	end     uint32
	revokes map[uint64]uint32
	header  []byte
	data    []byte
	report  *Replay
}

// replay runs the three passes and returns the sequence number after the
// last committed transaction.
func (fs *FS) replay(log *journalLog, report *Replay) (uint32, error) {
	walk := &logWalk{fs: fs, log: log, revokes: make(map[uint64]uint32),
		header: make([]byte, fs.blockSize), data: make([]byte, fs.blockSize), report: report}
	end, err := walk.run()
	if err != nil {
		return 0, err
	}
	walk.end = end
	for _, pass := range []int{passRevoke, passReplay} {
		walk.pass = pass
		if _, err := walk.run(); err != nil {
			return 0, err
		}
	}
	report.Transactions = end - log.tailSeq
	return end, nil
}

func (walk *logWalk) read(position uint32, target []byte) error {
	block := walk.log.physical(position)
	if block == 0 {
		return corrupt("log block outside the journal inode")
	}
	if !walk.fs.device.ReadAt(block*uint64(walk.fs.blockSize), target) {
		return errIO
	}
	return nil
}

// run walks the log once. The scan returns the sequence of the first
// transaction that did not commit; the later passes stop there and treat
// any block that differs from what the scan saw as damage.
func (walk *logWalk) run() (uint32, error) {
	log := walk.log
	position, seq := log.tail, log.tailSeq
	scan := walk.pass == passScan
	steps := uint32(0)
	stop := func(reason string) (uint32, error) {
		if !scan {
			return 0, corrupt(reason + " during replay")
		}
		if err := walk.checkTail(position, seq, log.size()-steps+1); err != nil {
			return 0, err
		}
		return seq, nil
	}
	broken := false
	for scan || seq != walk.end {
		if steps >= log.size() {
			return 0, corrupt("journal log does not end")
		}
		steps++
		if err := walk.read(position, walk.header); err != nil {
			return 0, err
		}
		header := walk.header
		if be32(header, 0) != jbd2Magic || be32(header, 8) != seq {
			return stop("journal block out of sequence")
		}
		switch be32(header, 4) {
		case jbd2Descriptor:
			if !log.tailOK(header) {
				return stop("descriptor checksum mismatch")
			}
			next, bad, wellFormed, err := walk.descriptor(position, seq, &steps)
			if err != nil {
				return 0, err
			}
			if !wellFormed {
				return stop("descriptor without a last tag")
			}
			broken = broken || bad
			position = next
		case jbd2Revoke:
			if !log.tailOK(header) {
				return stop("revoke checksum mismatch")
			}
			if walk.pass == passRevoke {
				if err := walk.revoke(header, seq); err != nil {
					return 0, err
				}
			}
			position = log.next(position)
		case jbd2Commit:
			if !log.commitOK(header) {
				return stop("commit checksum mismatch")
			}
			if broken {
				return 0, corrupt("committed journal block fails its checksum")
			}
			position = log.next(position)
			seq++
		default:
			return stop("unexpected block in the journal log")
		}
	}
	return seq, nil
}

// checkTail looks through count log blocks from position, where the scan
// stopped in transaction seq, for proof that seq had committed: a valid
// commit block of seq, or a valid block of seq+1.
func (walk *logWalk) checkTail(position uint32, seq uint32, count uint32) error {
	log := walk.log
	block := walk.data
	for index := uint32(0); index < count; index++ {
		if err := walk.read(position, block); err != nil {
			return err
		}
		position = log.next(position)
		if be32(block, 0) != jbd2Magic {
			continue
		}
		valid := false
		switch be32(block, 4) {
		case jbd2Descriptor, jbd2Revoke:
			valid = log.tailOK(block)
		case jbd2Commit:
			valid = log.commitOK(block)
		}
		switch tid := be32(block, 8); {
		case !valid:
		case tid == seq && be32(block, 4) == jbd2Commit:
			return corrupt("committed journal transaction is damaged before its commit block")
		case tid == seq+1:
			return corrupt("committed journal transaction is damaged; a later one follows it")
		}
	}
	return nil
}

// descriptor handles the tags of one descriptor block and the data blocks
// after it. It returns the position after them, whether a data block
// failed its checksum, and whether the descriptor ended with a last tag.
func (walk *logWalk) descriptor(position uint32, seq uint32, steps *uint32) (uint32, bool, bool, error) {
	log, fs := walk.log, walk.fs
	descriptor := walk.header
	limit := len(descriptor) - log.tailBytes()
	size := log.tagBytes()
	bad := false
	last := false
	tags := make([]journalTag, 0, 64)
	for offset := jbd2HeaderBytes; offset+size <= limit; {
		tag := log.decodeTag(descriptor[offset:])
		offset += size
		if tag.flags&jbd2FlagSameUUID == 0 {
			offset += jbd2UUIDBytes
		}
		if offset > limit {
			break
		}
		tags = append(tags, tag)
		if tag.flags&jbd2FlagLastTag != 0 {
			last = true
			break
		}
	}
	if !last {
		return 0, false, false, nil
	}
	for _, tag := range tags {
		position = log.next(position)
		if *steps >= log.size() {
			return 0, false, false, corrupt("journal log does not end")
		}
		*steps++
		if walk.pass == passRevoke {
			continue
		}
		if tag.block >= fs.sb.BlocksCount || log.inLog(tag.block) {
			return 0, false, false, corrupt("journal names a block outside the filesystem")
		}
		if walk.pass == passReplay {
			if last, revoked := walk.revokes[tag.block]; revoked && !tidAfter(seq, last) {
				walk.report.Revoked++
				continue
			}
		}
		if err := walk.read(position, walk.data); err != nil {
			return 0, false, false, err
		}
		if !log.tagOK(tag, seq, walk.data) {
			if walk.pass == passReplay {
				return 0, false, false, corrupt("committed journal block fails its checksum")
			}
			bad = true
			continue
		}
		if walk.pass != passReplay {
			continue
		}
		if tag.flags&jbd2FlagEscape != 0 {
			putBE32(walk.data, jbd2Magic)
		}
		if !fs.device.WriteAt(tag.block*uint64(fs.blockSize), walk.data) {
			return 0, false, false, errWrite
		}
		walk.report.Blocks++
	}
	return log.next(position), bad, true, nil
}

// revoke records the blocks a revoke block cancels for transactions up to
// and including seq.
func (walk *logWalk) revoke(data []byte, seq uint32) error {
	log := walk.log
	count := int(be32(data, jbd2HeaderBytes))
	record := log.recordBytes()
	if count < jbd2RevokeHeader || count > len(data)-log.tailBytes() || (count-jbd2RevokeHeader)%record != 0 {
		return corrupt("revoke block length invalid")
	}
	for offset := jbd2RevokeHeader; offset < count; offset += record {
		block := uint64(be32(data, offset))
		if log.is64 {
			block = be64(data, offset)
		}
		last, known := walk.revokes[block]
		if !known && len(walk.revokes) >= maxRevokes {
			return fail(KindUnsupported, "journal revokes too many blocks")
		}
		if !known || tidAfter(seq, last) {
			walk.revokes[block] = seq
		}
	}
	return nil
}
