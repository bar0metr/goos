package ext4

// State is where a volume stands in the mount sequence. A read-only mount
// reaches ReadyRO; a writable one passes through Recovering to ReadyRW and
// ends in CleanUnmounted, or in Failed after a write or flush error.
type State uint8

const (
	StateNoVolume State = iota
	StateProbing
	StateValidatedRO
	StateNeedsRecovery
	StateRecovering
	StateReadyRW
	StateReadyRO
	StateQuiescing
	StateCleanUnmounted
	StateFailed
	StateAbortedRO
)

var stateNames = [...]string{
	StateNoVolume:       "no-volume",
	StateProbing:        "probing",
	StateValidatedRO:    "validated-ro",
	StateNeedsRecovery:  "needs-recovery",
	StateRecovering:     "recovering",
	StateReadyRW:        "ready-rw",
	StateReadyRO:        "ready-ro",
	StateQuiescing:      "quiescing",
	StateCleanUnmounted: "clean-unmounted",
	StateFailed:         "failed",
	StateAbortedRO:      "aborted-ro",
}

func (state State) String() string {
	if int(state) < len(stateNames) {
		return stateNames[state]
	}
	return "unknown"
}

// Options select how a volume is mounted.
type Options struct {
	// Writable asks for a read-write mount. It replays the journal and
	// finishes the orphan list first, so it also recovers a volume a
	// read-only mount refuses.
	Writable bool
	// Clock stamps inode times and commit blocks; a writable mount needs
	// one.
	Clock func() Timestamp
}

// writeSupported gates every read-write mount. Writable mounts need the
// writable profile of the feature matrix, 4 KiB blocks, an internal
// journal with a version 2 superblock, and a device whose flush succeeds.
const writeSupported = true

// FS is a mounted ext4 volume.
type FS struct {
	sb          *Superblock
	device      Device
	cache       blockCache
	blockSize   uint32
	groups      uint64
	seed        uint32
	journal     Journal
	negotiation Negotiation
	state       State
	scratch     []byte
	// mutable is set by a writable mount once recovery has finished.
	mutable bool
	txn     *txn
	// live holds the inodes the running operation has loaded or created.
	live    map[uint32]*Inode
	now     func() Timestamp
	commits uint64
	log     *journalLog
	// orphans is the committed orphan list, head first; pins counts the
	// callers holding each inode open.
	orphans []uint32
	pins    map[uint32]uint32
	// beforeReplay skips the bitmap checksums while a writable mount
	// opens the journal: a home write the last flush did not cover may
	// leave a bitmap newer than its descriptor until replay rewrites both.
	beforeReplay bool
	recovery     Replay
}

// Mount probes device, validates the volume and mounts it. It refuses
// volumes whose features, state or metadata it cannot interpret, and every
// refusal names the first check that failed. A read-only mount of a volume
// whose journal or orphan list holds work is refused with
// KindNeedsRecovery: it is never shown as if it were consistent. A
// writable mount does that work first.
func Mount(device Device, options Options) (*FS, error) {
	fs, err := probe(device)
	if err != nil {
		return nil, err
	}
	if options.Writable {
		return fs.mountWritable(options)
	}
	sb := fs.sb
	if sb.NeedsRecovery() {
		return nil, fail(KindNeedsRecovery, "journal holds transactions to replay")
	}
	if sb.Features.ROCompat&roCompatOrphanPresent != 0 {
		return nil, fail(KindNeedsRecovery, "orphan file holds inodes to clean up")
	}
	if sb.State&stateValid == 0 {
		return nil, fail(KindNeedsRecovery, "volume was not cleanly unmounted")
	}
	if sb.LastOrphan != 0 {
		return nil, fail(KindNeedsRecovery, "orphan list holds inodes to clean up")
	}
	if err := fs.open(); err != nil {
		return nil, err
	}
	if !fs.journal.Clean() {
		return nil, fail(KindNeedsRecovery, "journal is not empty")
	}
	fs.state = StateReadyRO
	return fs, nil
}

// probe reads and parses the superblock and negotiates features.
func probe(device Device) (*FS, error) {
	capacity := device.Capacity()
	if capacity < superblockOffset+superblockSize {
		return nil, corrupt("partition smaller than the superblock")
	}
	raw := make([]byte, superblockSize)
	if !device.ReadAt(superblockOffset, raw) {
		return nil, errIO
	}
	sb, err := ParseSuperblock(raw, capacity)
	if err != nil {
		return nil, err
	}
	fs := &FS{sb: sb, device: device, blockSize: sb.BlockSize(), groups: sb.GroupCount(), state: StateProbing}
	fs.negotiation = Negotiate(sb.Features)
	if fs.negotiation.Support == Reject {
		return nil, fail(KindUnsupported, fs.negotiation.Reason)
	}
	if sb.HasMetadataCsum() || sb.Features.Incompat&incompatCsumSeed != 0 {
		fs.seed = sb.checksumSeed()
	}
	return fs, nil
}

// open validates the group descriptors, the root and the journal
// superblock.
func (fs *FS) open() error {
	fs.cache.init(fs.device, fs.blockSize, fs.sb.BlocksCount)
	fs.scratch = make([]byte, fs.blockSize)
	if err := fs.validateGroups(); err != nil {
		return err
	}
	root, err := fs.Inode(RootInode)
	if err != nil {
		return err
	}
	if !root.IsDir() {
		return corrupt("root inode is not a directory")
	}
	return fs.openJournal()
}

// mountWritable replays the journal, finishes the orphan list and opens
// the volume for mutation. The journal superblock records a live log and
// the superblock needs_recovery before the first transaction, so a power
// cut from here on leaves a volume the next writable mount recovers.
func (fs *FS) mountWritable(options Options) (*FS, error) {
	switch {
	case !writeSupported:
		return nil, fail(KindReadOnly, "read-write ext4 mounts are not enabled in this build")
	case fs.negotiation.Support != ReadWrite:
		return nil, fail(KindReadOnly, fs.negotiation.Reason)
	case fs.blockSize != 4096:
		return nil, fail(KindUnsupported, "read-write needs 4 KiB blocks")
	case fs.sb.Features.Compat&compatHasJournal == 0:
		return nil, fail(KindReadOnly, "read-write needs an internal journal")
	case fs.sb.State&stateErrors != 0:
		return nil, fail(KindNeedsRecovery, "volume recorded errors; check it with e2fsck")
	case options.Clock == nil:
		return nil, fail(KindInvalid, "writable mount without a clock")
	}
	// A device that cannot flush cannot order a commit after its log.
	if !fs.device.Flush() {
		return nil, fail(KindUnsupported, "device cannot flush its write cache")
	}
	fs.now = options.Clock
	fs.state = StateRecovering
	fs.beforeReplay = true
	if err := fs.open(); err != nil {
		return nil, err
	}
	log, err := fs.loadLog()
	if err != nil {
		return nil, err
	}
	fs.log = log
	fs.beforeReplay = false
	if log.tail != 0 {
		end, err := fs.replay(log, &fs.recovery)
		if err != nil {
			return nil, err
		}
		// The sequence after a replay skips one number: blocks of the
		// transaction that never committed may still carry it.
		log.seq = end + 1
		if !fs.device.Flush() || !fs.writeLogSuper(0, log.seq) || !fs.device.Flush() {
			return nil, errWrite
		}
		if err := fs.reload(); err != nil {
			return nil, err
		}
		log = fs.log
	} else {
		log.seq = log.tailSeq
		if err := fs.validateGroups(); err != nil {
			return nil, err
		}
	}
	if err := fs.verifyGroupCounts(); err != nil {
		return nil, err
	}
	if err := fs.loadOrphans(); err != nil {
		return nil, err
	}
	fs.enableFeatures()
	log.head = log.first
	log.journaled = make(map[uint64]uint32)
	if !fs.writeLogSuper(log.head, log.seq) || !fs.device.Flush() {
		return nil, errWrite
	}
	if err := fs.markMounted(true); err != nil {
		return nil, err
	}
	fs.mutable = true
	if err := fs.cleanupOrphans(&fs.recovery); err != nil {
		fs.mutable = false
		fs.state = StateFailed
		return nil, err
	}
	fs.state = StateReadyRW
	return fs, nil
}

// reload reads the volume again after replay rewrote its metadata.
func (fs *FS) reload() error {
	log := fs.log
	fresh, err := probe(fs.device)
	if err != nil {
		return err
	}
	fs.sb, fs.negotiation, fs.seed = fresh.sb, fresh.negotiation, fresh.seed
	if fs.negotiation.Support != ReadWrite {
		return fail(KindReadOnly, fs.negotiation.Reason)
	}
	if err := fs.open(); err != nil {
		return err
	}
	reloaded, err := fs.loadLog()
	if err != nil {
		return err
	}
	reloaded.seq = log.seq
	fs.log = reloaded
	return nil
}

// markMounted writes the superblock directly, outside any transaction. A
// mounted volume carries needs_recovery and lacks the valid state bit, as
// Linux leaves it; clean unmount reverses both. The free counts are the
// sums of the descriptors: Linux recomputes them at mount the same way.
func (fs *FS) markMounted(mounted bool) error {
	block := uint64(superblockOffset) / uint64(fs.blockSize)
	within := superblockOffset % int(fs.blockSize)
	current, err := fs.cache.get(block)
	if err != nil {
		return err
	}
	data := make([]byte, fs.blockSize)
	copy(data, current)
	raw := data[within : within+superblockSize]
	var freeBlocks uint64
	var freeInodes uint32
	for group := uint32(0); uint64(group) < fs.groups; group++ {
		desc, err := fs.group(group)
		if err != nil {
			return err
		}
		freeBlocks += uint64(desc.FreeBlocks)
		freeInodes += desc.FreeInodes
	}
	incompat := le32(raw, 0x60)
	state := le16(raw, 0x3A)
	if mounted {
		incompat |= incompatRecover
		state &^= stateValid
	} else {
		incompat &^= incompatRecover
		state |= stateValid
	}
	putLE32(raw[0x60:], incompat)
	putLE16(raw[0x3A:], state)
	putLE32(raw[0xC:], uint32(freeBlocks))
	if fs.sb.Features.Incompat&incompat64Bit != 0 {
		putLE32(raw[0x158:], uint32(freeBlocks>>32))
	}
	putLE32(raw[0x10:], freeInodes)
	sealSuperblock(raw)
	if !fs.device.WriteAt(block*uint64(fs.blockSize), data) || !fs.device.Flush() {
		return errWrite
	}
	fs.cache.update(block, data)
	fs.sb.FreeBlocks, fs.sb.FreeInodes = freeBlocks, freeInodes
	fs.sb.State = state
	fs.sb.Features.Incompat = incompat
	return nil
}

// Sync makes every committed transaction durable at its home location and
// empties the log. Each operation is already durable in the log when it
// returns; Sync only shortens the next replay.
func (fs *FS) Sync() error {
	if !fs.mutable {
		if fs.state == StateFailed {
			return errWrite
		}
		return nil
	}
	if err := fs.reclaim(); err != nil {
		return fs.failWrite()
	}
	return nil
}

// Unmount releases pinned inodes that lost their last link, empties the
// log and marks the volume clean. After a failed write the volume stays
// as the journal left it, for the next writable mount to recover.
func (fs *FS) Unmount() error {
	if !fs.mutable {
		if fs.state == StateFailed {
			return errWrite
		}
		fs.state = StateCleanUnmounted
		return nil
	}
	for number := range fs.pins {
		delete(fs.pins, number)
	}
	if err := fs.cleanupPinned(); err != nil {
		return err
	}
	fs.state = StateQuiescing
	log := fs.log
	if !fs.device.Flush() || !fs.writeLogSuper(0, log.seq) || !fs.device.Flush() {
		return fs.failWrite()
	}
	if err := fs.markMounted(false); err != nil {
		return fs.failWrite()
	}
	fs.mutable = false
	fs.state = StateCleanUnmounted
	return nil
}

// cleanupPinned releases the listed inodes that have no links: they were
// kept only for callers that held them open.
func (fs *FS) cleanupPinned() error {
	for _, number := range append([]uint32(nil), fs.orphans...) {
		err := fs.mutate(func() error {
			inode, err := fs.liveInode(number)
			if err != nil {
				return err
			}
			if inode.Links != 0 {
				return nil
			}
			return fs.release(inode)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Recovery reports what the writable mount replayed and cleaned up.
func (fs *FS) Recovery() Replay { return fs.recovery }

// Superblock returns the decoded superblock.
func (fs *FS) Superblock() *Superblock { return fs.sb }

// State reports where the volume stands.
func (fs *FS) State() State { return fs.state }

// Root returns the root directory inode.
func (fs *FS) Root() (*Inode, error) { return fs.Inode(RootInode) }

// ReadFile reads from a regular file or symlink at offset into target and
// returns the byte count, which is short only at end of file. Holes and
// unwritten extents read as zeros.
func (fs *FS) ReadFile(inode *Inode, offset uint64, target []byte) (int, error) {
	if inode.IsDir() {
		return 0, fail(KindIsDirectory, "")
	}
	if !inode.IsRegular() && !inode.IsSymlink() {
		return 0, fail(KindInvalid, "not a file")
	}
	if offset >= inode.Size {
		return 0, nil
	}
	if remaining := inode.Size - offset; uint64(len(target)) > remaining {
		target = target[:remaining]
	}
	blockSize := uint64(fs.blockSize)
	done := 0
	for done < len(target) {
		position := offset + uint64(done)
		logical := position / blockSize
		within := position % blockSize
		mapping, err := fs.mapBlock(inode, logical)
		if err != nil {
			return done, err
		}
		runBytes := mapping.Length*blockSize - within
		if mapping.Length > (^uint64(0)-within)/blockSize {
			runBytes = ^uint64(0)
		}
		chunk := uint64(len(target) - done)
		if chunk > runBytes {
			chunk = runBytes
		}
		out := target[done : done+int(chunk)]
		switch {
		case mapping.Hole || mapping.Unwritten:
			for index := range out {
				out[index] = 0
			}
		case within == 0 && chunk >= blockSize:
			chunk -= chunk % blockSize
			out = out[:chunk]
			if err := fs.cache.readBlocks(mapping.Physical, out); err != nil {
				return done, err
			}
		default:
			if chunk > blockSize-within {
				chunk = blockSize - within
				out = out[:chunk]
			}
			if err := fs.cache.readBlocks(mapping.Physical, fs.scratch); err != nil {
				return done, err
			}
			copy(out, fs.scratch[within:])
		}
		done += int(chunk)
	}
	return done, nil
}

// maxSymlink is the longest target a symlink may hold, matching the VFS
// path ceiling.
const maxSymlink = 4095

// Readlink returns the target of a symbolic link.
func (fs *FS) Readlink(inode *Inode) ([]byte, error) {
	if !inode.IsSymlink() {
		return nil, fail(KindNotSymlink, "")
	}
	if inode.Size == 0 || inode.Size > maxSymlink {
		return nil, corrupt("symlink length invalid")
	}
	eaBlocks := uint64(0)
	if inode.FileACL != 0 {
		eaBlocks = uint64(fs.blockSize / 512)
	}
	if inode.Flags&inodeFlagExtents == 0 && inode.Blocks512 == eaBlocks {
		if inode.Size >= inodeBlockSize {
			return nil, corrupt("fast symlink longer than the inode block area")
		}
		return append([]byte(nil), inode.blockArea[:inode.Size]...), nil
	}
	target := make([]byte, inode.Size)
	count, err := fs.ReadFile(inode, 0, target)
	if err != nil {
		return nil, err
	}
	if uint64(count) != inode.Size {
		return nil, corrupt("symlink shorter than its size")
	}
	for _, value := range target {
		if value == 0 {
			return nil, corrupt("symlink target holds NUL")
		}
	}
	return target, nil
}

// Info is the capacity and identity a filesystem reports.
type Info struct {
	BlockSize      uint32
	Blocks         uint64
	FreeBlocks     uint64
	AvailableBytes uint64
	Inodes         uint32
	FreeInodes     uint32
	ReadOnly       bool
	Label          []byte
	UUID           [16]byte
}

// Info reports capacity from the superblock counters.
func (fs *FS) Info() Info {
	available := uint64(0)
	if fs.sb.FreeBlocks > fs.sb.ReservedBlocks {
		available = (fs.sb.FreeBlocks - fs.sb.ReservedBlocks) * uint64(fs.blockSize)
	}
	return Info{
		BlockSize:      fs.blockSize,
		Blocks:         fs.sb.BlocksCount,
		FreeBlocks:     fs.sb.FreeBlocks,
		AvailableBytes: available,
		Inodes:         fs.sb.InodesCount,
		FreeInodes:     fs.sb.FreeInodes,
		ReadOnly:       fs.state != StateReadyRW,
		Label:          fs.sb.Label(),
		UUID:           fs.sb.UUID,
	}
}

// Status is what diagnostics show about a mounted volume.
type Status struct {
	State       State
	Negotiation Negotiation
	Features    FeatureSet
	SBState     uint16
	ErrorCount  uint32
	LastOrphan  uint32
	Journal     Journal
	CacheReads  uint64
	CacheHits   uint64
	// Commits counts the transactions this mount committed; Recovery is
	// what a writable mount replayed and cleaned up before it.
	Commits  uint64
	Recovery Replay
}

// Status reports the volume's mount state and counters.
func (fs *FS) Status() Status {
	return Status{
		State:       fs.state,
		Negotiation: fs.negotiation,
		Features:    fs.sb.Features,
		SBState:     fs.sb.State,
		ErrorCount:  fs.sb.ErrorCount,
		LastOrphan:  fs.sb.LastOrphan,
		Journal:     fs.journal,
		CacheReads:  fs.cache.reads,
		CacheHits:   fs.cache.hits,
		Commits:     fs.commits,
		Recovery:    fs.recovery,
	}
}
