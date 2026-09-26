// Package fat32 is the on-disk FAT32 format the FAT32 filesystem service
// serves (ADR 0096, ADR 0102). It speaks the node-ID backend interface the VFS
// mounts and reads and writes its volume through a Device, which the service
// backs with the block protocol.
//
// Everything a volume declares is untrusted input. Geometry is validated once
// at mount, every FAT entry is checked before it is followed, and every chain
// walk is bounded by the number of clusters the volume has, so a corrupt image
// ends in a typed error rather than a loop, a panic, or a write outside the
// volume. Linux fs/fat was the behavioural reference for the on-disk rules;
// none of its infrastructure is used.
package fat32

import "goos/kernel/ramfs"

const (
	// unit is the granularity of the metadata window and of every offset
	// the window is addressed by. Logical sectors are 512 to 4096 bytes and
	// every metadata structure starts on a sector, so any logical sector is
	// a whole number of units, and no FAT or directory entry straddles one:
	// the window never depends on the volume's sector size.
	unit     = uint64(512)
	rootNode = ramfs.NodeID(1)
	// maxFileSize is the FAT32 limit: the size field is 32 bits wide.
	maxFileSize = uint64(0xffffffff)
	// maxFATs is the number of FAT copies a volume may declare. Normal
	// volumes have two; one is accepted as well.
	maxFATs = 2

	entrySize = uint64(32)
	// maxDirEntries is the FAT limit on one directory: 65536 entries, 2 MiB.
	maxDirEntries = 65536

	// A NodeID packs the directory key (0 for the root, otherwise the first
	// cluster of the directory) above slotBits and the entry index plus
	// slotBase below them. Slot 1 alone is the root node.
	slotBits = 17
	slotMask = uint64(1)<<slotBits - 1
	slotBase = 2

	clusterMask = uint32(0x0fffffff)
	// A FAT32 entry at or above endOfChain ends its chain; badCluster marks a
	// cluster the formatter found unusable, which no chain may contain.
	// Values between the highest cluster and badCluster are reserved and are
	// corruption in a chain, as a link to cluster 1 or past the volume is.
	badCluster     = uint32(0x0ffffff7)
	endOfChain     = uint32(0x0ffffff8)
	endOfChainMark = uint32(0x0fffffff)
	// maxClusters is the most data clusters a FAT32 volume can number: the
	// highest cluster must stay below the reserved range.
	maxClusters = uint64(0x0ffffff0 - 2)

	entryEnd = byte(0x00)
	// entryDeleted marks a free entry; entryKanji is how a name whose first
	// byte really is 0xe5 is stored.
	entryDeleted = byte(0xe5)
	entryKanji   = byte(0x05)

	// Extended flags (BPB offset 40): bit 7 disables mirroring, and the low
	// four bits then name the one active FAT.
	extNoMirror   = uint16(0x80)
	extActiveMask = uint16(0x0f)
)

var (
	dotName    = [11]byte{'.', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}
	dotDotName = [11]byte{'.', '.', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}
)

type Device interface {
	ReadAt(offset uint64, target []byte) bool
	WriteAt(offset uint64, data []byte) bool
	Flush() bool
	// Capacity is the device size in bytes. The filesystem checks the geometry
	// the image declares against it, because every offset this code computes
	// comes from numbers the image supplies and an image is untrusted input.
	Capacity() uint64
}

type FileSystem struct {
	device Device
	// sectorSize is the volume's logical sector size, from the BPB.
	sectorSize uint64
	fatStart   uint64
	fatSize    uint64
	fatCopies  uint32
	// fatRead is the copy entries are read from; fatFirst and fatCopies say
	// which copies a change is written to. With mirroring on, reads come from
	// the first copy and writes go to all of them.
	fatRead     uint32
	fatFirst    uint32
	fatTotal    uint32
	dataStart   uint64
	clusterSize uint64
	// clusters is the number of data clusters; valid cluster numbers are
	// [2, clusters+2). It bounds every chain walk: a chain longer than the
	// volume has clusters must loop.
	clusters    uint32
	rootCluster uint32
	// fsInfo is the byte offset of the FSInfo sector, zero when the volume
	// has none or it did not validate.
	fsInfo uint64
	// hint is where the next free-cluster search starts; hintDirty says the
	// FSInfo copy of it is stale.
	hint      uint32
	hintDirty bool
	// free is the number of free clusters once freeKnown; countDirty says the
	// FSInfo copy is stale. recordedFree is what FSInfo last said.
	free         uint32
	freeKnown    bool
	countDirty   bool
	recordedFree uint32
	health       Health
	readOnly     bool
	mounted      bool
	// refusal is why the last Mount refused its volume.
	refusal ramfs.Status
	// now is the wall clock timestamps are taken from; nil, or one that
	// reports no time, leaves them unrecorded.
	now Clock

	cache     [cacheSectors]cachedSector
	clock     uint64
	positions [positionSlots]position
	victim    int
	// want, scan, and name are the per-operation name buffers. One request is
	// in flight at a time, so they never need to be allocated.
	want    request
	scan    chainScan
	name    [maxLongName]byte
	zero    [zeroSize]byte
	scratch [zeroSize]byte
}

// Mount validates the geometry an image declares before deriving a single
// offset from it. Everything this filesystem computes — where the FATs are,
// where a cluster is, how far a chain may run — comes from these fields, and
// the image is untrusted input: a volume that does not fit its own device is
// refused rather than read. Refused reports why.
func (fs *FileSystem) Mount(device Device) (ramfs.NodeID, bool) {
	root, status := fs.mount(device)
	fs.refusal = status
	return root, status == ramfs.StatusOK
}

// Refused is why the last Mount refused its volume: an I/O error for a device
// that could not be read, Unsupported for a volume this service does not
// implement (not FAT32, sectors over 4 KiB, more than two FATs, a FAT32
// version other than 0.0), and Corrupt for geometry that contradicts itself
// or the device when no valid backup boot sector could stand in for it.
func (fs *FileSystem) Refused() ramfs.Status { return fs.refusal }

// Health reports what the last successful mount noticed about the volume.
func (fs *FileSystem) Health() Health { return fs.health }

func (fs *FileSystem) mount(device Device) (ramfs.NodeID, ramfs.Status) {
	now := fs.now
	fs.reset()
	fs.now = now
	var boot [unit]byte
	if device == nil || !device.ReadAt(0, boot[:]) {
		return 0, ramfs.StatusIOError
	}
	capacity := device.Capacity()
	if capacity < unit {
		return 0, ramfs.StatusUnsupported
	}
	geo, status := parseBoot(boot[:], capacity)
	switch status {
	case ramfs.StatusOK:
		fs.health.Backup = checkBackup(device, boot[:], &geo, capacity)
	case ramfs.StatusCorrupt:
		// A primary that fails validation may still have a good backup. It
		// is used for this mount only, read-only; the primary is not
		// rewritten, since deciding which copy is right is a repair.
		recovered, ok := recoverBoot(device, capacity, boot[:])
		if !ok {
			return 0, ramfs.StatusCorrupt
		}
		geo = recovered
		fs.health.Boot, fs.health.Backup, fs.health.ReadOnly = BootBackup, BackupMatches, true
	default:
		return 0, status
	}
	fs.device, fs.sectorSize = device, geo.sectorSize
	fs.fatStart, fs.fatSize = geo.reserved*geo.sectorSize, geo.fatSectors*geo.sectorSize
	fs.fatCopies, fs.fatTotal = geo.copies, geo.copies
	fs.dataStart = (geo.reserved + uint64(geo.copies)*geo.fatSectors) * geo.sectorSize
	fs.clusterSize = geo.perCluster * geo.sectorSize
	fs.clusters, fs.rootCluster = uint32(geo.clusters), geo.root
	if geo.flags&extNoMirror != 0 {
		active := uint32(geo.flags & extActiveMask)
		fs.fatRead, fs.fatFirst, fs.fatCopies = active, active, 1
	}
	fs.health.ShortFAT, fs.health.FewClusters = geo.shortFAT, geo.fewClusters
	fs.readOnly = fs.health.ReadOnly
	fs.hint = 2
	fs.readFSInfo(&geo)
	fs.mounted = true
	return rootNode, ramfs.StatusOK
}

// reset returns the filesystem to its unmounted state field by field: the
// structure is large, and a composite literal of it would be built on the
// heap by TinyGo.
func (fs *FileSystem) reset() {
	fs.device, fs.mounted, fs.readOnly, fs.health = nil, false, false, Health{}
	// No cluster is valid until a mount says otherwise, so nothing can
	// reach the device through geometry a refused mount left behind.
	fs.clusters, fs.rootCluster = 0, 0
	fs.fsInfo, fs.hint, fs.hintDirty = 0, 0, false
	fs.free, fs.freeKnown, fs.countDirty, fs.recordedFree = 0, false, false, 0
	fs.fatRead, fs.fatFirst = 0, 0
	for index := range fs.cache {
		fs.cache[index].valid, fs.cache[index].dirty = false, false
	}
	fs.forgetPositions()
}

// sync makes a completed change durable: the FAT sectors it changed, the
// FSInfo count and hint, then a device flush.
func (fs *FileSystem) sync() ramfs.Status {
	if !fs.flushFAT() || !fs.writeFSInfo() {
		return ramfs.StatusIOError
	}
	if !fs.device.Flush() {
		return ramfs.StatusIOError
	}
	return ramfs.StatusOK
}

// Flush makes every completed change durable.
func (fs *FileSystem) Flush() bool { return fs.mounted && fs.sync() == ramfs.StatusOK }

func (fs *FileSystem) valid(cluster uint32) bool {
	return cluster >= 2 && cluster-2 < fs.clusters
}

func (fs *FileSystem) clusterOffset(cluster uint32) uint64 {
	return fs.dataStart + uint64(cluster-2)*fs.clusterSize
}

// clustersFor is how many clusters hold size bytes.
func (fs *FileSystem) clustersFor(size uint64) uint64 {
	return (size + fs.clusterSize - 1) / fs.clusterSize
}

// fat reads the entry of cluster from the active FAT, without its reserved
// top four bits.
func (fs *FileSystem) fat(cluster uint32) (uint32, ramfs.Status) {
	var data [4]byte
	if !fs.valid(cluster) {
		return 0, ramfs.StatusCorrupt
	}
	if !fs.readMeta(fs.fatStart+uint64(fs.fatRead)*fs.fatSize+uint64(cluster)*4, data[:]) {
		return 0, ramfs.StatusIOError
	}
	return get32(data[:]) & clusterMask, ramfs.StatusOK
}

// setFAT changes the entry of cluster, keeping its reserved top four bits as
// the specification requires. The change is made in the resident sector of the
// FAT read from and marked dirty; flushFAT writes the whole sector to every FAT
// copy in use, as Linux mirrors whole FAT blocks, so a chain of many clusters
// costs a write per sector rather than one per entry per copy.
func (fs *FileSystem) setFAT(cluster, value uint32) ramfs.Status {
	if !fs.valid(cluster) || value&^clusterMask != 0 {
		return ramfs.StatusCorrupt
	}
	offset := fs.fatStart + uint64(fs.fatRead)*fs.fatSize + uint64(cluster)*4
	entry, ok := fs.resident(offset / unit)
	if !ok {
		return ramfs.StatusIOError
	}
	within := offset % unit
	data := entry.data[within : within+4]
	put32(data, get32(data)&^clusterMask|value)
	entry.dirty = true
	return ramfs.StatusOK
}

// next follows one link of a chain. It reports the end of the chain, or
// corruption for a link to a free, reserved, bad, or out-of-range cluster.
func (fs *FileSystem) next(cluster uint32) (uint32, bool, ramfs.Status) {
	value, status := fs.fat(cluster)
	switch {
	case status != ramfs.StatusOK:
		return 0, false, status
	case value >= endOfChain:
		return 0, true, ramfs.StatusOK
	case !fs.valid(value):
		return 0, false, ramfs.StatusCorrupt
	}
	return value, false, ramfs.StatusOK
}

// position remembers one place in one chain, so walking to a later cluster
// continues from it instead of from the first cluster. It plays the part of
// Linux's per-inode FAT cache and makes sequential access linear.
type position struct {
	first   uint32
	ordinal uint32
	cluster uint32
}

const positionSlots = 8

func (fs *FileSystem) remember(first, ordinal, cluster uint32) {
	for index := range fs.positions {
		if entry := &fs.positions[index]; entry.first == first {
			entry.ordinal, entry.cluster = ordinal, cluster
			return
		}
	}
	fs.positions[fs.victim] = position{first: first, ordinal: ordinal, cluster: cluster}
	fs.victim = (fs.victim + 1) % positionSlots
}

// forgetPositions drops every remembered position. Any change that cuts or
// frees a chain calls it, so a position never outlives the link it records.
func (fs *FileSystem) forgetPositions() {
	fs.positions = [positionSlots]position{}
}

// seek returns the cluster at ordinal in the chain starting at first. found
// is false, with no error, when the chain ends before it. The walk is bounded
// by the volume's cluster count, so a looped chain is reported as corrupt.
func (fs *FileSystem) seek(first uint32, ordinal uint64) (uint32, bool, ramfs.Status) {
	if !fs.valid(first) {
		return 0, false, ramfs.StatusCorrupt
	}
	if ordinal >= uint64(fs.clusters) {
		return 0, false, ramfs.StatusOK
	}
	cluster, at := first, uint32(0)
	for _, known := range fs.positions {
		if known.first == first && uint64(known.ordinal) <= ordinal && known.ordinal >= at && fs.valid(known.cluster) {
			cluster, at = known.cluster, known.ordinal
		}
	}
	for uint64(at) < ordinal {
		next, end, status := fs.next(cluster)
		if status != ramfs.StatusOK {
			return 0, false, status
		}
		if end {
			fs.remember(first, at, cluster)
			return 0, false, ramfs.StatusOK
		}
		cluster, at = next, at+1
	}
	fs.remember(first, at, cluster)
	return cluster, true, ramfs.StatusOK
}

// last returns the final cluster of a chain and how many clusters it has.
func (fs *FileSystem) last(first uint32) (uint32, uint64, ramfs.Status) {
	if !fs.valid(first) {
		return 0, 0, ramfs.StatusCorrupt
	}
	cluster, at := first, uint32(0)
	for _, known := range fs.positions {
		if known.first == first && known.ordinal >= at && fs.valid(known.cluster) {
			cluster, at = known.cluster, known.ordinal
		}
	}
	for {
		next, end, status := fs.next(cluster)
		if status != ramfs.StatusOK {
			return 0, 0, status
		}
		if end {
			fs.remember(first, at, cluster)
			return cluster, uint64(at) + 1, ramfs.StatusOK
		}
		if at+1 >= fs.clusters {
			return 0, 0, ramfs.StatusCorrupt
		}
		cluster, at = next, at+1
	}
}

// allocate claims one free cluster and marks it the end of a chain. The
// search starts at the hint and wraps once, as Linux's does, so filling a
// volume costs one pass over the FAT rather than one per cluster.
func (fs *FileSystem) allocate() (uint32, ramfs.Status) {
	cluster := fs.hint
	for scanned := uint32(0); scanned < fs.clusters; scanned++ {
		if !fs.valid(cluster) {
			cluster = 2
		}
		value, status := fs.fat(cluster)
		if status != ramfs.StatusOK {
			return 0, status
		}
		if value == 0 {
			if status = fs.setFAT(cluster, endOfChainMark); status != ramfs.StatusOK {
				return 0, status
			}
			fs.hint, fs.hintDirty = cluster+1, true
			if !fs.valid(fs.hint) {
				fs.hint = 2
			}
			fs.claimed()
			return cluster, ramfs.StatusOK
		}
		cluster++
	}
	return 0, ramfs.StatusNoSpace
}

// extend appends count clusters to the chain ending at tail, or starts a new
// chain when tail is zero, and returns the first new cluster. Clusters are
// claimed before they are linked, so a failure part way frees what it took
// and leaves the original chain ending where it did.
func (fs *FileSystem) extend(tail uint32, count uint64) (uint32, ramfs.Status) {
	if count == 0 {
		return 0, ramfs.StatusOK
	}
	if count > uint64(fs.clusters) {
		return 0, ramfs.StatusNoSpace
	}
	first, previous := uint32(0), uint32(0)
	for added := uint64(0); added < count; added++ {
		cluster, status := fs.allocate()
		if status == ramfs.StatusOK && previous != 0 {
			status = fs.setFAT(previous, cluster)
		}
		if status != ramfs.StatusOK {
			if cluster != 0 {
				fs.freeChain(cluster)
			}
			if first != 0 {
				fs.freeChain(first)
			}
			return 0, status
		}
		if first == 0 {
			first = cluster
		}
		previous = cluster
	}
	if tail != 0 {
		if status := fs.setFAT(tail, first); status != ramfs.StatusOK {
			fs.freeChain(first)
			return 0, status
		}
	}
	return first, ramfs.StatusOK
}

// freeChain releases a chain. Each link is cleared before it is followed, so
// a looped chain ends at the first revisited cluster, which then reads free;
// the walk is bounded by the cluster count either way. A link to a cluster
// outside the data area ends the walk as well: whatever it names is not part
// of this chain.
func (fs *FileSystem) freeChain(cluster uint32) {
	fs.forgetPositions()
	for steps := uint32(0); steps < fs.clusters && fs.valid(cluster); steps++ {
		next, status := fs.fat(cluster)
		if status != ramfs.StatusOK || next == 0 || fs.setFAT(cluster, 0) != ramfs.StatusOK {
			return
		}
		fs.released()
		if cluster < fs.hint {
			fs.hint, fs.hintDirty = cluster, true
		}
		cluster = next
	}
}

// zeroCluster clears a whole cluster, for a directory that must read as
// ending at its first entry.
func (fs *FileSystem) zeroCluster(cluster uint32) bool {
	return fs.zeroRange(fs.clusterOffset(cluster), fs.clusterSize)
}

// zeroRange writes zeros over length bytes at offset on the device.
func (fs *FileSystem) zeroRange(offset, length uint64) bool {
	for length != 0 {
		chunk := uint64(len(fs.zero))
		if chunk > length {
			chunk = length
		}
		if !fs.write(offset, fs.zero[:chunk]) {
			return false
		}
		offset += chunk
		length -= chunk
	}
	return true
}

func entryCluster(entry [32]byte) uint32 {
	return (uint32(get16(entry[20:22]))<<16 | uint32(get16(entry[26:28]))) & clusterMask
}
func setEntryCluster(entry *[32]byte, cluster uint32) {
	put16(entry[20:22], uint16(cluster>>16))
	put16(entry[26:28], uint16(cluster))
}
func entrySizeOf(entry [32]byte) uint64 { return uint64(get32(entry[28:32])) }
func isDirectory(entry [32]byte) bool   { return entry[11]&attrDirectory != 0 }

// listed reports whether entry is a live file or subdirectory: not free,
// deleted, a volume label or long-name fragment, nor "." or "..".
func listed(entry [32]byte) bool {
	return entry[0] != entryEnd && entry[0] != entryDeleted && entry[0] != '.' && entry[11]&attrVolume == 0
}
func entryStat(id ramfs.NodeID, entry [32]byte) ramfs.Stat {
	if isDirectory(entry) {
		return ramfs.Stat{ID: id, Kind: ramfs.KindDirectory}
	}
	return ramfs.Stat{ID: id, Kind: ramfs.KindFile, Size: entrySizeOf(entry)}
}

// dotEntry is a "." or ".." entry naming cluster, with the times of the
// directory entry it belongs to.
func dotEntry(name [11]byte, cluster uint32, times [32]byte) [32]byte {
	entry := times
	copy(entry[:11], name[:])
	entry[11], entry[12] = attrDirectory, 0
	setEntryCluster(&entry, cluster)
	put32(entry[28:32], 0)
	return entry
}

func lower(value byte) byte {
	if value >= 'A' && value <= 'Z' {
		return value + 32
	}
	return value
}
func get16(data []byte) uint16 { return uint16(data[0]) | uint16(data[1])<<8 }
func get32(data []byte) uint32 {
	return uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24
}
func put16(data []byte, value uint16) { data[0] = byte(value); data[1] = byte(value >> 8) }
func put32(data []byte, value uint32) {
	for i := 0; i < 4; i++ {
		data[i] = byte(value >> uint(i*8))
	}
}
