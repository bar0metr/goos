package ext4

// Mutations stage every block they change in memory. Each operation runs
// in its own transaction, layered over the pending one that holds the
// operations finished since the last commit. Reads see the operation's
// copies first, then the pending ones, then the device. An operation that
// fails drops only its own layer; one that succeeds folds it into the
// pending transaction. Nothing reaches the device until the pending
// transaction commits through the JBD2 journal in ordered-data mode
// (journalw.go): when the caller syncs or unmounts, when the next step
// would not fit, when the batch passes half its bounds, or when the
// service's commit interval runs out (ADR 0227).

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
	// batchMeta, batchData and batchFrees are the pending sizes past which
	// a finished operation commits the batch at once, so the next
	// operation rarely has to commit in the middle.
	batchMeta  = maxTxnCredits / 2
	batchData  = maxTxnData / 2
	batchFrees = maxTxnFrees / 2
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

// txn is one layer of staged changes. In an operation's layer, meta and
// data count only the blocks the pending layer does not hold, so the two
// counts add up to what the commit will write.
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
	// orphans is the orphan list as this layer leaves it, head first. It
	// replaces FS.orphans at commit.
	orphans       []uint32
	orphansEdited bool
}

func newTxn(orphans []uint32) *txn {
	return &txn{blocks: make(map[uint64]*stagedBlock), stepCost: -1, orphans: orphans}
}

// orphanList is the orphan list as the operations so far leave it.
func (fs *FS) orphanList() []uint32 {
	switch {
	case fs.txn != nil:
		return fs.txn.orphans
	case fs.pending != nil && fs.pending.orphansEdited:
		return fs.pending.orphans
	}
	return fs.orphans
}

// begin opens the running operation's transaction.
func (fs *FS) begin() error {
	if !fs.mutable {
		return fail(KindReadOnly, "volume is not open for mutation")
	}
	if fs.txn != nil {
		return fail(KindInternal, "transaction already open")
	}
	fs.txn = newTxn(fs.orphanList())
	fs.cache.overlay = fs.txn.blocks
	return nil
}

// abort drops the running operation's transaction and every inode loaded
// under it. The pending transaction stays.
func (fs *FS) abort() {
	fs.txn = nil
	fs.cache.overlay = nil
	fs.live = nil
}

// discard drops the pending transaction as well, after a failed write.
func (fs *FS) discard() {
	fs.abort()
	fs.pending = nil
	fs.cache.base = nil
}

// mutate runs one operation in its own transaction, folding it into the
// pending one on success and dropping it on failure. An operation that
// reserves more than one transaction holds commits the earlier ones as it
// goes; an abort then drops only the last.
func (fs *FS) mutate(operation func() error) error {
	if err := fs.begin(); err != nil {
		return err
	}
	fs.live = make(map[uint32]*Inode)
	if err := operation(); err != nil {
		fs.abort()
		return err
	}
	if err := fs.fold(); err != nil {
		fs.abort()
		return err
	}
	fs.live = nil
	if p := fs.pending; p != nil && (p.meta > batchMeta || p.data > batchData || len(p.freed) > batchFrees) {
		return fs.Sync()
	}
	return nil
}

// fold stages the running operation's inodes and superblock counters and
// moves its layer into the pending transaction.
func (fs *FS) fold() error {
	t := fs.txn
	t.stepCost = -1
	if err := fs.flushInodes(); err != nil {
		return err
	}
	if err := fs.flushSuperblock(); err != nil {
		return err
	}
	p := fs.pending
	if p == nil {
		p = newTxn(fs.orphans)
		fs.pending = p
		fs.cache.base = p.blocks
	}
	for block, staged := range t.blocks {
		if old := p.blocks[block]; old != nil && old.meta != staged.meta {
			if staged.meta {
				p.meta, p.data = p.meta+1, p.data-1
			} else {
				p.meta, p.data = p.meta-1, p.data+1
			}
		}
		p.blocks[block] = staged
	}
	p.meta += t.meta
	p.data += t.data
	p.freed = append(p.freed, t.freed...)
	p.freeBlocks += t.freeBlocks
	p.freeInodes += t.freeInodes
	if t.orphansEdited {
		p.orphans, p.orphansEdited = t.orphans, true
	}
	fs.txn = nil
	fs.cache.overlay = nil
	return nil
}

// credits is the metadata the pending and running transactions stage
// together; staged and freed count their data blocks and freed runs.
func (fs *FS) credits() (meta int, data int, freed int) {
	t := fs.txn
	meta, data, freed = t.meta, t.data, len(t.freed)
	if p := fs.pending; p != nil {
		meta, data, freed = meta+p.meta, data+p.data, freed+len(p.freed)
	}
	return meta, data, freed
}

// fits reports whether a step of meta and data blocks fits beside what
// the transactions hold.
func (fs *FS) fits(meta int, data int) bool {
	held, staged, freed := fs.credits()
	return held+meta <= maxTxnCredits-txnHeadroom && staged+data <= maxTxnData && freed+maxStepFrees <= maxTxnFrees
}

// reserve starts a step that stages at most meta metadata blocks and data
// file blocks. When the transactions lack room, the pending one commits
// first; when the running operation alone still lacks room, it commits
// too and a new transaction opens. reserve reports whether the running
// operation committed.
func (fs *FS) reserve(meta int, data int) (bool, error) {
	if meta > maxTxnCredits-txnHeadroom || data > maxTxnData {
		return false, fail(KindInternal, "step larger than a transaction")
	}
	committed := false
	if !fs.fits(meta, data) && fs.pending != nil {
		if err := fs.commitPending(); err != nil {
			return false, err
		}
	}
	if !fs.fits(meta, data) {
		if err := fs.fold(); err != nil {
			return false, err
		}
		if err := fs.commitPending(); err != nil {
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

func (fs *FS) charge(block uint64, meta bool) error {
	if p := fs.pending; p != nil && p.blocks[block] != nil {
		// The commit writes the block once; the pending layer paid for it.
		return nil
	}
	t := fs.txn
	held, staged, _ := fs.credits()
	if !meta {
		t.data++
		if staged+1 > maxTxnData {
			return fail(KindInternal, "transaction holds too much file data")
		}
		return nil
	}
	t.meta++
	if held+1 > maxTxnCredits {
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

// pendingCopy gives the running operation its own copy of a block the
// pending transaction holds, so an abort leaves the pending copy intact.
func (fs *FS) pendingCopy(block uint64) *stagedBlock {
	p := fs.pending
	if p == nil {
		return nil
	}
	held := p.blocks[block]
	if held == nil {
		return nil
	}
	staged := &stagedBlock{data: append(make([]byte, 0, len(held.data)), held.data...), meta: held.meta}
	fs.txn.blocks[block] = staged
	return staged
}

// stage returns the transaction's writable copy of a metadata block,
// reading it on first use.
func (fs *FS) stage(block uint64) ([]byte, error) {
	t := fs.txn
	if staged := t.blocks[block]; staged != nil {
		return staged.data, nil
	}
	if staged := fs.pendingCopy(block); staged != nil {
		return staged.data, nil
	}
	if err := fs.checkStageable(block); err != nil {
		return nil, err
	}
	current, err := fs.cache.get(block)
	if err != nil {
		return nil, err
	}
	if err := fs.charge(block, true); err != nil {
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
	staged := t.blocks[block]
	if staged == nil {
		staged = fs.pendingCopy(block)
	}
	if staged != nil {
		for index := range staged.data {
			staged.data[index] = 0
		}
		if staged.meta != meta {
			if err := fs.charge(block, meta); err != nil {
				return nil, err
			}
			staged.meta = meta
		}
		return staged.data, nil
	}
	if err := fs.checkStageable(block); err != nil {
		return nil, err
	}
	if err := fs.charge(block, meta); err != nil {
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
	if staged := fs.pendingCopy(block); staged != nil {
		return staged.data, nil
	}
	if !keep {
		return fs.stageZero(block, false)
	}
	if err := fs.checkStageable(block); err != nil {
		return nil, err
	}
	if err := fs.charge(block, false); err != nil {
		return nil, err
	}
	data := make([]byte, fs.blockSize)
	if !fs.device.ReadAt(block*uint64(fs.blockSize), data) {
		return nil, errIO
	}
	t.blocks[block] = &stagedBlock{data: data}
	return data, nil
}

// wasFreed reports whether block was freed by this layer.
func (t *txn) wasFreed(block uint64) bool {
	for index := range t.freed {
		run := &t.freed[index]
		if block >= run.start && block-run.start < run.count {
			return true
		}
	}
	return false
}

// freedSinceCommit reports whether block was freed by the running
// operation or by the pending transaction.
func (fs *FS) freedSinceCommit(block uint64) bool {
	if fs.pending != nil && fs.pending.wasFreed(block) {
		return true
	}
	return fs.txn.wasFreed(block)
}

// freeCounts are the superblock's free block and inode counts as the
// operations so far leave them.
func (fs *FS) freeCounts() (int64, int64) {
	blocks, inodes := int64(fs.sb.FreeBlocks), int64(fs.sb.FreeInodes)
	if p := fs.pending; p != nil {
		blocks, inodes = blocks+p.freeBlocks, inodes+p.freeInodes
	}
	if t := fs.txn; t != nil {
		blocks, inodes = blocks+t.freeBlocks, inodes+t.freeInodes
	}
	return blocks, inodes
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

// Pending reports whether finished operations wait for a commit.
func (fs *FS) Pending() bool { return fs.pending != nil }

func (fs *FS) pendingBlocks() int {
	if fs.pending == nil {
		return 0
	}
	return len(fs.pending.blocks)
}

// commitPending writes the pending transaction through the journal. The
// operations in it have already answered their callers, so any failure
// leaves the volume failed: the next writable mount replays what
// committed before.
func (fs *FS) commitPending() error {
	t := fs.pending
	if t == nil {
		return nil
	}
	var meta, data []uint64
	for block, staged := range t.blocks {
		// A block the transaction freed needs no copy: it is free once
		// the transaction commits, and a revoke record cancels any older
		// copy in the log.
		if t.wasFreed(block) {
			continue
		}
		if staged.meta {
			meta = append(meta, block)
		} else {
			data = append(data, block)
		}
	}
	sortBlocks(meta)
	sortBlocks(data)
	if len(meta) != 0 || len(data) != 0 || len(t.freed) != 0 {
		if err := fs.commitLog(t, meta, data, fs.revokesFor(t)); err != nil {
			return fs.failWrite()
		}
		fs.commits++
	}
	for _, block := range meta {
		fs.cache.update(block, t.blocks[block].data)
	}
	fs.sb.FreeBlocks = uint64(int64(fs.sb.FreeBlocks) + t.freeBlocks)
	fs.sb.FreeInodes = uint32(int64(fs.sb.FreeInodes) + t.freeInodes)
	if t.orphansEdited {
		fs.orphans = t.orphans
		fs.sb.LastOrphan = fs.orphanHead()
	}
	fs.pending = nil
	fs.cache.base = nil
	if r := fs.txn; r != nil {
		// The running operation's copies of blocks the batch held now
		// count against the next transaction.
		r.meta, r.data = 0, 0
		for _, staged := range r.blocks {
			if staged.meta {
				r.meta++
			} else {
				r.data++
			}
		}
	}
	return nil
}

func (fs *FS) failWrite() error {
	fs.discard()
	fs.cache.invalidate()
	fs.mutable = false
	fs.state = StateFailed
	return errWrite
}

// flushSuperblock stages the primary superblock with the new free counts
// and orphan list head. Backup superblocks keep their counts; Linux
// updates them only when it resizes the volume.
func (fs *FS) flushSuperblock() error {
	t := fs.txn
	if t.freeBlocks == 0 && t.freeInodes == 0 && !t.orphansEdited {
		return nil
	}
	free, inodes := fs.freeCounts()
	block := uint64(superblockOffset) / uint64(fs.blockSize)
	within := superblockOffset % int(fs.blockSize)
	data, err := fs.stage(block)
	if err != nil {
		return err
	}
	raw := data[within : within+superblockSize]
	freeBlocks, freeInodes := uint64(free), uint32(inodes)
	putLE32(raw[0xC:], uint32(freeBlocks))
	if fs.sb.Features.Incompat&incompat64Bit != 0 {
		putLE32(raw[0x158:], uint32(freeBlocks>>32))
	}
	putLE32(raw[0x10:], freeInodes)
	if t.orphansEdited {
		head := uint32(0)
		if len(t.orphans) > 0 {
			head = t.orphans[0]
		}
		putLE32(raw[0xE8:], head)
	}
	sealSuperblock(raw)
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

// sealSuperblock recomputes the superblock checksum when the volume has
// one.
func sealSuperblock(raw []byte) {
	if le32(raw, 0x64)&roCompatMetadataCsum != 0 {
		putLE32(raw[0x3FC:], crc32c(^uint32(0), raw[:0x3FC]))
	}
}
