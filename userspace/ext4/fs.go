package ext4

// State is where a volume stands in the mount sequence. A read-only core
// only ever reaches ReadyRO; the other states exist so the service and its
// status output report the same names the recovery path will use.
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
	// Writable asks for a read-write mount.
	Writable bool
}

// writeSupported gates every read-write mount. It stays false until journal
// replay, ordered commits and orphan cleanup exist: until then a writable
// mount could leave a volume Linux would have to repair.
const writeSupported = false

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
	// mutable is set only by mountScratch: real mounts never stage writes
	// while writeSupported is false.
	mutable bool
	txn     *txn
	// live holds the inodes the running operation has loaded or created.
	live    map[uint32]*Inode
	now     func() Timestamp
	commits uint64
}

// Mount probes device, validates the volume and returns it mounted
// read-only. It refuses volumes whose features, state or metadata it cannot
// interpret, and every refusal names the first check that failed. A volume
// whose journal needs replay is refused with KindNeedsRecovery: it is never
// shown as if it were consistent.
func Mount(device Device, options Options) (*FS, error) {
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
	if options.Writable {
		switch {
		case !writeSupported:
			return nil, fail(KindReadOnly, "read-write ext4 mounts are not enabled in this build")
		case fs.negotiation.Support != ReadWrite:
			return nil, fail(KindReadOnly, fs.negotiation.Reason)
		}
	}
	if sb.NeedsRecovery() {
		return nil, fail(KindNeedsRecovery, "journal holds transactions to replay")
	}
	if sb.Features.ROCompat&roCompatOrphanPresent != 0 {
		return nil, fail(KindNeedsRecovery, "orphan file holds inodes to clean up")
	}
	if sb.State&stateValid == 0 {
		return nil, fail(KindNeedsRecovery, "volume was not cleanly unmounted")
	}
	if sb.HasMetadataCsum() || sb.Features.Incompat&incompatCsumSeed != 0 {
		fs.seed = sb.checksumSeed()
	}
	fs.cache.init(device, fs.blockSize, sb.BlocksCount)
	fs.scratch = make([]byte, fs.blockSize)
	if err := fs.validateGroups(); err != nil {
		return nil, err
	}
	root, err := fs.Inode(RootInode)
	if err != nil {
		return nil, err
	}
	if !root.IsDir() {
		return nil, corrupt("root inode is not a directory")
	}
	if err := fs.openJournal(); err != nil {
		return nil, err
	}
	if !fs.journal.Clean() {
		return nil, fail(KindNeedsRecovery, "journal is not empty")
	}
	fs.state = StateReadyRO
	return fs, nil
}

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
	}
}
