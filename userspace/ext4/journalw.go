package ext4

// The commit writer, in ordered-data mode. A transaction reaches the disk
// in this order:
//
//  1. file data blocks at their home locations;
//  2. descriptor blocks with the metadata copies, and revoke blocks, in
//     the log;
//  3. a flush, so the data and the log body are durable;
//  4. the commit block, then a flush: the transaction is now durable and
//     replay will apply it;
//  5. the metadata blocks at their home locations, without a flush.
//
// The home writes of step 5 become durable at the next flush. Log space
// is reclaimed only after one: reclaim flushes, then moves the log start
// in the journal superblock past every committed transaction, then
// flushes again before the next transaction may overwrite the old log.
// A power cut at any point therefore leaves either the old home blocks
// with their transaction still in the log, or durable home blocks.

// logBlocks counts the log blocks a transaction with meta metadata copies
// and revokes revoke records needs, commit block included.
func (log *journalLog) logBlocks(meta int, revokes int, blockSize int) uint32 {
	perDescriptor := (blockSize - jbd2HeaderBytes - log.tailBytes() - jbd2UUIDBytes) / log.tagBytes()
	descriptors := (meta + perDescriptor - 1) / perDescriptor
	perRevoke := (blockSize - jbd2RevokeHeader - log.tailBytes()) / log.recordBytes()
	revokeBlocks := (revokes + perRevoke - 1) / perRevoke
	return uint32(descriptors + meta + revokeBlocks + 1)
}

// maxRunBytes bounds one coalesced device write: the 128 KiB bulk window
// of the ext4 service's block endpoint.
const maxRunBytes = 128 << 10

// blockRun gathers writes of consecutive blocks into one device write, so
// a commit costs one block-service exchange per run instead of per block.
// The crash model is unchanged: until a flush, any sector of any write may
// land or not.
type blockRun struct {
	fs     *FS
	buffer []byte
	start  uint64
	size   int
}

func (fs *FS) newRun() *blockRun {
	if fs.runBuffer == nil {
		fs.runBuffer = make([]byte, maxRunBytes)
	}
	return &blockRun{fs: fs, buffer: fs.runBuffer}
}

// add queues data for block, writing out what was queued first unless
// block follows it.
func (run *blockRun) add(block uint64, data []byte) bool {
	blockSize := uint64(run.fs.blockSize)
	if run.size != 0 && (block != run.start+uint64(run.size)/blockSize || run.size+len(data) > len(run.buffer)) {
		if !run.flush() {
			return false
		}
	}
	if run.size == 0 {
		run.start = block
	}
	copy(run.buffer[run.size:], data)
	run.size += len(data)
	return true
}

// flush writes what is queued.
func (run *blockRun) flush() bool {
	if run.size == 0 {
		return true
	}
	ok := run.fs.device.WriteAt(run.start*uint64(run.fs.blockSize), run.buffer[:run.size])
	run.size = 0
	return ok
}

// writeLog queues block for the log head and advances it.
func (fs *FS) writeLog(run *blockRun, data []byte) bool {
	log := fs.log
	block := log.physical(log.head)
	if block == 0 || !run.add(block, data) {
		return false
	}
	log.head = log.next(log.head)
	return true
}

// commitLog writes transaction t through the journal. meta and data
// list the staged blocks to write, ascending; revokes lists the logged
// blocks the transaction freed.
func (fs *FS) commitLog(t *txn, meta []uint64, data []uint64, revokes []uint64) error {
	log := fs.log
	size := int(fs.blockSize)
	need := log.logBlocks(len(meta), len(revokes), size)
	if need >= log.size() {
		return fail(KindInternal, "transaction larger than the journal")
	}
	if log.used()+need >= log.size() {
		if err := fs.reclaim(); err != nil {
			return err
		}
	}
	seq := log.seq
	run := fs.newRun()
	for _, block := range data {
		if !run.add(block, t.blocks[block].data) {
			return errWrite
		}
	}
	if !run.flush() {
		return errWrite
	}
	// Descriptor blocks and their copies.
	descriptor := make([]byte, size)
	copyBuffer := make([]byte, size)
	limit := size - log.tailBytes()
	tagSize := log.tagBytes()
	for start := 0; start < len(meta); {
		for index := range descriptor {
			descriptor[index] = 0
		}
		putHeader(descriptor, jbd2Descriptor, seq)
		offset := jbd2HeaderBytes
		end := start
		for end < len(meta) {
			room := tagSize
			if end == start {
				room += jbd2UUIDBytes
			}
			if offset+room > limit {
				break
			}
			offset += room
			end++
		}
		offset = jbd2HeaderBytes
		for index := start; index < end; index++ {
			block := meta[index]
			contents := t.blocks[block].data
			flags := uint32(0)
			copy(copyBuffer, contents)
			if be32(copyBuffer, 0) == jbd2Magic {
				putBE32(copyBuffer, 0)
				flags |= jbd2FlagEscape
			}
			if index != start {
				flags |= jbd2FlagSameUUID
			}
			if index == end-1 {
				flags |= jbd2FlagLastTag
			}
			fs.encodeTag(descriptor[offset:], block, flags, log.tagChecksum(seq, copyBuffer))
			offset += tagSize
			if index == start {
				copy(descriptor[offset:], log.super[0x30:0x40])
				offset += jbd2UUIDBytes
			}
		}
		log.sealTail(descriptor)
		if !fs.writeLog(run, descriptor) {
			return errWrite
		}
		for index := start; index < end; index++ {
			copy(copyBuffer, t.blocks[meta[index]].data)
			if be32(copyBuffer, 0) == jbd2Magic {
				putBE32(copyBuffer, 0)
			}
			if !fs.writeLog(run, copyBuffer) {
				return errWrite
			}
		}
		start = end
	}
	// Revoke blocks.
	record := log.recordBytes()
	for start := 0; start < len(revokes); {
		for index := range descriptor {
			descriptor[index] = 0
		}
		putHeader(descriptor, jbd2Revoke, seq)
		offset := jbd2RevokeHeader
		for start < len(revokes) && offset+record <= limit {
			if log.is64 {
				putBE64(descriptor[offset:], revokes[start])
			} else {
				putBE32(descriptor[offset:], uint32(revokes[start]))
			}
			offset += record
			start++
		}
		putBE32(descriptor[jbd2HeaderBytes:], uint32(offset))
		log.sealTail(descriptor)
		if !fs.writeLog(run, descriptor) {
			return errWrite
		}
	}
	if !run.flush() || !fs.device.Flush() {
		return errWrite
	}
	// The commit block.
	for index := range descriptor {
		descriptor[index] = 0
	}
	putHeader(descriptor, jbd2Commit, seq)
	stamp := fs.now()
	putBE64(descriptor[0x30:], uint64(stamp.Seconds))
	putBE32(descriptor[0x38:], stamp.Nanos)
	if log.csum != 0 {
		putBE32(descriptor[0x10:], log.blockChecksum(descriptor, 0x10))
	}
	if !fs.writeLog(run, descriptor) || !run.flush() || !fs.device.Flush() {
		return errWrite
	}
	log.seq++
	for _, block := range revokes {
		delete(log.journaled, block)
	}
	for _, block := range meta {
		log.journaled[block] = seq
		if !run.add(block, t.blocks[block].data) {
			return errWrite
		}
	}
	if !run.flush() {
		return errWrite
	}
	return nil
}

func (fs *FS) encodeTag(raw []byte, block uint64, flags uint32, checksum uint32) {
	log := fs.log
	putBE32(raw, uint32(block))
	switch {
	case log.csum == 3:
		putBE32(raw[4:], flags)
		putBE32(raw[8:], uint32(block>>32))
		putBE32(raw[12:], checksum)
	default:
		raw[4], raw[5] = 0, 0
		raw[6], raw[7] = byte(flags>>8), byte(flags)
		if log.is64 {
			putBE32(raw[8:], uint32(block>>32))
		}
	}
}

// reclaim makes every home write durable and then empties the log by
// moving its start to the head.
func (fs *FS) reclaim() error {
	log := fs.log
	if !fs.device.Flush() || !fs.writeLogSuper(log.head, log.seq) || !fs.device.Flush() {
		return errWrite
	}
	log.journaled = make(map[uint64]uint32)
	return nil
}

// revokesFor lists the logged blocks the transaction freed.
func (fs *FS) revokesFor(t *txn) []uint64 {
	var revokes []uint64
	for block := range fs.log.journaled {
		if t.wasFreed(block) {
			revokes = append(revokes, block)
		}
	}
	sortBlocks(revokes)
	return revokes
}
