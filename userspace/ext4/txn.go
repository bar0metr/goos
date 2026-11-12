package ext4

// Mutations stage every block they change in a transaction held in memory.
// Reads made while the transaction is open see the staged copies first, so
// an operation observes its own changes; nothing reaches the device until
// commit, and an abort only drops the staged copies. Commit writes file data
// first, flushes, then writes metadata and flushes again. That order is the
// one ordered-data journaling needs, but without a journal a power cut in
// the middle of the metadata writes still leaves a damaged volume: commit
// is only used on scratch devices (see mountScratch).

const (
	// maxTxnCredits bounds the metadata blocks one transaction stages. A
	// JBD2 transaction may use at most a quarter of the journal, and the
	// smallest journal the supported profile creates is 1024 blocks.
	maxTxnCredits = 256
	// maxTxnData bounds the file data blocks one transaction holds: 1 MiB
	// at 4 KiB blocks.
	maxTxnData = 256
	// maxTxnFrees bounds the freed runs one transaction remembers.
	maxTxnFrees = 512
	// maxStepFrees is the most runs one step frees: an extent, every node
	// on a path, and the root collapsing level by level.
	maxStepFrees = 1 + 2*extentMaxDepth
)

// stagedBlock is one block a transaction will write.
type stagedBlock struct {
	data []byte
	// meta marks filesystem metadata; file data is written before it.
	meta bool
}

// freedRun is a run of blocks freed in the open transaction. They stay
// unavailable until it commits, so a block is never freed and reused by
// the same transaction; journal replay depends on that.
type freedRun struct {
	start uint64
	count uint64
}

type txn struct {
	blocks map[uint64]*stagedBlock
	meta   int
	data   int
	freed  []freedRun
	// freeBlocks and freeInodes are the changes to the superblock
	// counters, applied at commit.
	freeBlocks int64
	freeInodes int64
	// stepStart and stepCost check that each step stages no more metadata
	// than it reserved.
	stepStart int
	stepCost  int
}

// begin opens a transaction.
func (fs *FS) begin() error {
	if !fs.mutable {
		return fail(KindReadOnly, "volume is not open for mutation")
	}
	if fs.txn != nil {
		return fail(KindInternal, "transaction already open")
	}
	fs.txn = &txn{blocks: make(map[uint64]*stagedBlock), stepCost: -1}
	fs.cache.overlay = fs.txn.blocks
	return nil
}

// abort drops the open transaction and every inode loaded under it.
func (fs *FS) abort() {
	fs.txn = nil
	fs.cache.overlay = nil
	fs.live = nil
}

// mutate runs one operation in a transaction, committing on success and
// aborting on failure. An operation that reserves more than one transaction
// holds commits the earlier ones as it goes; an abort then drops only the
// last.
func (fs *FS) mutate(operation func() error) error {
	if err := fs.begin(); err != nil {
		return err
	}
	fs.live = make(map[uint32]*Inode)
	if err := operation(); err != nil {
		fs.abort()
		return err
	}
	err := fs.commit()
	fs.live = nil
	return err
}

// reserve starts a step that stages at most meta metadata blocks and data
// file blocks. When the open transaction lacks room, it is committed first
// and a new one opened; reserve reports whether that happened.
func (fs *FS) reserve(meta int, data int) (bool, error) {
	if meta > maxTxnCredits-txnHeadroom || data > maxTxnData {
		return false, fail(KindInternal, "step larger than a transaction")
	}
	committed := false
	if t := fs.txn; t.meta+meta > maxTxnCredits-txnHeadroom || t.data+data > maxTxnData ||
		len(t.freed)+maxStepFrees > maxTxnFrees {
		if err := fs.commit(); err != nil {
			return false, err
		}
		if err := fs.begin(); err != nil {
			return false, err
		}
		committed = true
	}
	fs.txn.stepStart, fs.txn.stepCost = fs.txn.meta, meta
	return committed, nil
}

func (t *txn) charge(meta bool) error {
	if !meta {
		t.data++
		if t.data > maxTxnData {
			return fail(KindInternal, "transaction holds too much file data")
		}
		return nil
	}
	t.meta++
	if t.meta > maxTxnCredits {
		return fail(KindInternal, "transaction exceeded its credits")
	}
	if t.stepCost >= 0 && t.meta-t.stepStart > t.stepCost {
		return fail(KindInternal, "step exceeded its credits")
	}
	return nil
}

func (fs *FS) checkStageable(block uint64) error {
	if block >= fs.sb.BlocksCount {
		return corrupt("staged block beyond the volume")
	}
	return nil
}

// stage returns the transaction's writable copy of a metadata block,
// reading it on first use.
func (fs *FS) stage(block uint64) ([]byte, error) {
	t := fs.txn
	if staged := t.blocks[block]; staged != nil {
		return staged.data, nil
	}
	if err := fs.checkStageable(block); err != nil {
		return nil, err
	}
	current, err := fs.cache.get(block)
	if err != nil {
		return nil, err
	}
	if err := t.charge(true); err != nil {
		return nil, err
	}
	data := make([]byte, fs.blockSize)
	copy(data, current)
	t.blocks[block] = &stagedBlock{data: data, meta: true}
	return data, nil
}

// stageZero returns a zeroed writable copy of a block the transaction has
// just allocated; its old contents are never read.
func (fs *FS) stageZero(block uint64, meta bool) ([]byte, error) {
	t := fs.txn
	if staged := t.blocks[block]; staged != nil {
		for index := range staged.data {
			staged.data[index] = 0
		}
		staged.meta = meta
		return staged.data, nil
	}
	if err := fs.checkStageable(block); err != nil {
		return nil, err
	}
	if err := t.charge(meta); err != nil {
		return nil, err
	}
	data := make([]byte, fs.blockSize)
	t.blocks[block] = &stagedBlock{data: data, meta: meta}
	return data, nil
}

// stageData returns a writable copy of a file data block. Its contents are
// read from the device only when the caller keeps part of them.
func (fs *FS) stageData(block uint64, keep bool) ([]byte, error) {
	t := fs.txn
	if staged := t.blocks[block]; staged != nil {
		return staged.data, nil
	}
	if !keep {
		return fs.stageZero(block, false)
	}
	if err := fs.checkStageable(block); err != nil {
		return nil, err
	}
	if err := t.charge(false); err != nil {
		return nil, err
	}
	data := make([]byte, fs.blockSize)
	if !fs.device.ReadAt(block*uint64(fs.blockSize), data) {
		return nil, errIO
	}
	t.blocks[block] = &stagedBlock{data: data}
	return data, nil
}

// wasFreed reports whether block was freed by the open transaction.
func (t *txn) wasFreed(block uint64) bool {
	for index := range t.freed {
		run := &t.freed[index]
		if block >= run.start && block-run.start < run.count {
			return true
		}
	}
	return false
}

func (t *txn) noteFreed(start uint64, count uint64) error {
	if n := len(t.freed); n > 0 && t.freed[n-1].start+t.freed[n-1].count == start {
		t.freed[n-1].count += count
		return nil
	}
	if len(t.freed) >= maxTxnFrees {
		return fail(KindInternal, "transaction freed too many runs")
	}
	t.freed = append(t.freed, freedRun{start: start, count: count})
	return nil
}

// commit writes the open transaction. A failed write leaves the device in
// an unknown state, so the volume stops accepting mutations.
func (fs *FS) commit() error {
	t := fs.txn
	t.stepCost = -1
	if err := fs.flushInodes(); err != nil {
		fs.abort()
		return err
	}
	if err := fs.flushSuperblock(); err != nil {
		fs.abort()
		return err
	}
	order := make([]uint64, 0, len(t.blocks))
	for block := range t.blocks {
		order = append(order, block)
	}
	sortBlocks(order)
	size := uint64(fs.blockSize)
	for pass := 0; pass < 2; pass++ {
		wantMeta := pass == 1
		for _, block := range order {
			staged := t.blocks[block]
			if staged.meta != wantMeta {
				continue
			}
			if !fs.device.WriteAt(block*size, staged.data) {
				return fs.failWrite()
			}
		}
		if !fs.device.Flush() {
			return fs.failWrite()
		}
	}
	for _, block := range order {
		fs.cache.update(block, t.blocks[block].data)
	}
	fs.sb.FreeBlocks = uint64(int64(fs.sb.FreeBlocks) + t.freeBlocks)
	fs.sb.FreeInodes = uint32(int64(fs.sb.FreeInodes) + t.freeInodes)
	fs.txn = nil
	fs.cache.overlay = nil
	fs.commits++
	return nil
}

func (fs *FS) failWrite() error {
	fs.abort()
	fs.cache.invalidate()
	fs.mutable = false
	fs.state = StateFailed
	return errWrite
}

// flushSuperblock stages the primary superblock with the new free counts.
// Backup superblocks keep their counts; Linux updates them only when it
// resizes the volume.
func (fs *FS) flushSuperblock() error {
	t := fs.txn
	if t.freeBlocks == 0 && t.freeInodes == 0 {
		return nil
	}
	block := uint64(superblockOffset) / uint64(fs.blockSize)
	within := superblockOffset % int(fs.blockSize)
	data, err := fs.stage(block)
	if err != nil {
		return err
	}
	raw := data[within : within+superblockSize]
	freeBlocks := uint64(int64(fs.sb.FreeBlocks) + t.freeBlocks)
	freeInodes := uint32(int64(fs.sb.FreeInodes) + t.freeInodes)
	putLE32(raw[0xC:], uint32(freeBlocks))
	if fs.sb.Features.Incompat&incompat64Bit != 0 {
		putLE32(raw[0x158:], uint32(freeBlocks>>32))
	}
	putLE32(raw[0x10:], freeInodes)
	putLE32(raw[0x3FC:], crc32c(^uint32(0), raw[:0x3FC]))
	return nil
}

// sortBlocks sorts block numbers ascending. A transaction holds a few
// hundred blocks, so a shell sort is enough and needs no reflection.
func sortBlocks(blocks []uint64) {
	for gap := len(blocks) / 2; gap > 0; gap /= 2 {
		for index := gap; index < len(blocks); index++ {
			value := blocks[index]
			at := index
			for at >= gap && blocks[at-gap] > value {
				blocks[at] = blocks[at-gap]
				at -= gap
			}
			blocks[at] = value
		}
	}
}
