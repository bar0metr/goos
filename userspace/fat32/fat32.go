package fat32

import "goos/kernel/ramfs"

const (
	sectorSize  = uint64(512)
	rootNode    = ramfs.NodeID(1)
	maxFileSize = uint64(64 * 1024)
	// cacheSectors bounds the resident sector window. Directory entries and FAT
	// links are packed many to a sector, so a small window removes almost all
	// repeated device traffic for metadata scans.
	cacheSectors = 4

	entrySize         = uint64(32)
	entriesPerCluster = int(sectorSize / entrySize)
	// maxDirClusters bounds every directory, the root included, to four
	// one-sector clusters of sixteen entries each.
	maxDirClusters = 4
	maxDirEntries  = maxDirClusters * entriesPerCluster

	// A NodeID packs the directory key (0 for the root, otherwise the first
	// cluster of the directory) above slotBits and the entry index plus
	// slotBase below them. Slot 1 alone is the root node.
	slotBits   = 16
	slotMask   = uint64(1)<<slotBits - 1
	slotBase   = 2
	maxCluster = uint64(0x0fffffff)

	clusterMask    = uint32(0x0fffffff)
	endOfChain     = uint32(0x0ffffff8)
	endOfChainMark = uint32(0x0fffffff)

	entryEnd      = byte(0x00)
	entryDeleted  = byte(0xe5)
	attrVolume    = byte(0x08)
	attrDirectory = byte(0x10)
	attrArchive   = byte(0x20)

	// statusCorrupt reports on-disk structures that fail validation; ramfs has
	// no dedicated corruption status.
	statusCorrupt = ramfs.StatusResourceExhausted
)

var (
	dotName    = [11]byte{'.', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}
	dotDotName = [11]byte{'.', '.', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}
)

type Device interface {
	ReadAt(offset uint64, target []byte) bool
	WriteAt(offset uint64, data []byte) bool
	Flush() bool
}

type FileSystem struct {
	device        Device
	fatStart      uint64
	dataStart     uint64
	totalClusters uint32
	rootCluster   uint32
	sector        [512]byte
	cache         [cacheSectors]cachedSector
	victim        uint8
	// want, scan, and name are the per-operation name buffers. One request is
	// in flight at a time, so they never need to be allocated.
	want request
	scan chainScan
	name [maxLongName]byte
}

// cachedSector holds one resident device sector. The filesystem owns the
// device, so a write-through copy stays coherent without invalidation traffic.
type cachedSector struct {
	lba   uint64
	valid bool
	data  [512]byte
}

// resident returns the cached sector for lba, reading it from the device on a
// miss and replacing entries round-robin.
func (fs *FileSystem) resident(lba uint64) (*cachedSector, bool) {
	for index := range fs.cache {
		if entry := &fs.cache[index]; entry.valid && entry.lba == lba {
			return entry, true
		}
	}
	entry := &fs.cache[fs.victim]
	fs.victim = (fs.victim + 1) % cacheSectors
	entry.valid = false
	if !fs.device.ReadAt(lba*sectorSize, entry.data[:]) {
		return nil, false
	}
	entry.lba, entry.valid = lba, true
	return entry, true
}

// readAt serves bounded reads from resident sectors.
func (fs *FileSystem) readAt(offset uint64, target []byte) bool {
	for len(target) != 0 {
		entry, ok := fs.resident(offset / sectorSize)
		if !ok {
			return false
		}
		within := offset % sectorSize
		chunk := sectorSize - within
		if chunk > uint64(len(target)) {
			chunk = uint64(len(target))
		}
		copy(target[:chunk], entry.data[within:within+chunk])
		target = target[chunk:]
		offset += chunk
	}
	return true
}

// writeAt writes through to the device and keeps the resident copy coherent.
// A resident sector is never left holding superseded bytes: it is updated on
// success and dropped when the device write fails.
func (fs *FileSystem) writeAt(offset uint64, data []byte) bool {
	for len(data) != 0 {
		lba := offset / sectorSize
		within := offset % sectorSize
		chunk := sectorSize - within
		if chunk > uint64(len(data)) {
			chunk = uint64(len(data))
		}
		resident := fs.lookup(lba)
		if !fs.device.WriteAt(offset, data[:chunk]) {
			if resident != nil {
				resident.valid = false
			}
			return false
		}
		switch {
		case resident != nil:
			copy(resident.data[within:within+chunk], data[:chunk])
		case chunk == sectorSize:
			// A full-sector write supplies every byte, so the sector can become
			// resident without reading it back.
			entry := &fs.cache[fs.victim]
			fs.victim = (fs.victim + 1) % cacheSectors
			copy(entry.data[:], data[:chunk])
			entry.lba, entry.valid = lba, true
		}
		data = data[chunk:]
		offset += chunk
	}
	return true
}

func (fs *FileSystem) lookup(lba uint64) *cachedSector {
	for index := range fs.cache {
		if entry := &fs.cache[index]; entry.valid && entry.lba == lba {
			return entry
		}
	}
	return nil
}

func (fs *FileSystem) Mount(device Device) (ramfs.NodeID, bool) {
	for index := range fs.cache {
		fs.cache[index] = cachedSector{}
	}
	fs.victim = 0
	if device == nil || !device.ReadAt(0, fs.sector[:]) || fs.sector[510] != 0x55 || fs.sector[511] != 0xaa || get16(fs.sector[11:13]) != 512 || fs.sector[13] != 1 || fs.sector[16] != 1 {
		return 0, false
	}
	reserved := uint64(get16(fs.sector[14:16]))
	fatSectors := uint64(get32(fs.sector[36:40]))
	total := uint64(get32(fs.sector[32:36]))
	root := get32(fs.sector[44:48])
	if reserved < 2 || fatSectors == 0 || total <= reserved+fatSectors || root < 2 {
		return 0, false
	}
	fs.device, fs.fatStart, fs.dataStart = device, reserved*sectorSize, (reserved+fatSectors)*sectorSize
	fs.totalClusters, fs.rootCluster = uint32(total-reserved-fatSectors), root
	if !fs.valid(root) {
		return 0, false
	}
	return rootNode, true
}

func (fs *FileSystem) OpenName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	dir, status := fs.parentAndName(parent, name)
	if status != ramfs.StatusOK {
		return 0, status
	}
	at, _, found, status := fs.find(&dir)
	if status != ramfs.StatusOK {
		return 0, status
	}
	if !found {
		return 0, ramfs.StatusNotFound
	}
	return dir.node(at.index()), ramfs.StatusOK
}

func (fs *FileSystem) CreateName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	dir, at, entry, status := fs.reserve(parent, name)
	if status != ramfs.StatusOK {
		return 0, status
	}
	entry[11] = attrArchive
	if !fs.commit(&dir, &at, entry) || !fs.device.Flush() {
		return 0, ramfs.StatusResourceExhausted
	}
	return dir.node(at.index()), ramfs.StatusOK
}

// MkdirName creates a one-cluster directory holding "." and "..". Per the FAT
// specification ".." records cluster 0 when the parent is the root.
func (fs *FileSystem) MkdirName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	dir, at, entry, status := fs.reserve(parent, name)
	if status != ramfs.StatusOK {
		return 0, status
	}
	cluster := fs.allocate()
	if cluster == 0 {
		return 0, ramfs.StatusResourceExhausted
	}
	dot := dotEntry(dotName, cluster)
	dotDot := dotEntry(dotDotName, dir.key)
	base := fs.clusterOffset(cluster)
	entry[11] = attrDirectory
	setEntryCluster(&entry, cluster)
	if !fs.writeAt(base, dot[:]) || !fs.writeAt(base+entrySize, dotDot[:]) || !fs.commit(&dir, &at, entry) || !fs.device.Flush() {
		fs.freeChain(cluster)
		return 0, ramfs.StatusResourceExhausted
	}
	return dir.node(at.index()), ramfs.StatusOK
}

func (fs *FileSystem) Read(id ramfs.NodeID, offset uint64, target []byte) (int, ramfs.Status) {
	_, _, entry, status := fs.fileNode(id)
	if status != ramfs.StatusOK {
		return 0, status
	}
	size := uint64(get32(entry[28:32]))
	if offset >= size {
		return 0, ramfs.StatusOK
	}
	count := uint64(len(target))
	if count > size-offset {
		count = size - offset
	}
	cluster := entryCluster(entry)
	var ok bool
	cluster, ok = fs.advance(cluster, uint32(offset/sectorSize))
	if !ok {
		return 0, statusCorrupt
	}
	done, within := uint64(0), offset%sectorSize
	for done < count {
		chunk := count - done
		if chunk > sectorSize-within {
			chunk = sectorSize - within
		}
		if !fs.readAt(fs.clusterOffset(cluster)+within, target[done:done+chunk]) {
			return int(done), ramfs.StatusResourceExhausted
		}
		done += chunk
		within = 0
		if done < count {
			cluster, ok = fs.next(cluster)
			if !ok {
				return int(done), statusCorrupt
			}
		}
	}
	return int(done), ramfs.StatusOK
}

func (fs *FileSystem) Write(id ramfs.NodeID, offset uint64, data []byte) (int, ramfs.Status) {
	dir, index, entry, status := fs.fileNode(id)
	if status != ramfs.StatusOK {
		return 0, status
	}
	if offset > maxFileSize || uint64(len(data)) > maxFileSize-offset {
		return 0, ramfs.StatusResourceExhausted
	}
	needed := uint32((offset + uint64(len(data)) + sectorSize - 1) / sectorSize)
	cluster := entryCluster(entry)
	if cluster != 0 && !fs.valid(cluster) {
		return 0, statusCorrupt
	}
	if needed != 0 && cluster == 0 {
		cluster = fs.allocate()
		if cluster == 0 {
			return 0, ramfs.StatusResourceExhausted
		}
		setEntryCluster(&entry, cluster)
	}
	if !fs.ensure(cluster, needed) {
		return 0, ramfs.StatusResourceExhausted
	}
	cluster, _ = fs.advance(cluster, uint32(offset/sectorSize))
	done, within := uint64(0), offset%sectorSize
	for done < uint64(len(data)) {
		chunk := uint64(len(data)) - done
		if chunk > sectorSize-within {
			chunk = sectorSize - within
		}
		if !fs.writeAt(fs.clusterOffset(cluster)+within, data[done:done+chunk]) {
			return int(done), ramfs.StatusResourceExhausted
		}
		done += chunk
		within = 0
		if done < uint64(len(data)) {
			cluster, _ = fs.next(cluster)
		}
	}
	size := uint64(get32(entry[28:32]))
	if offset+done > size {
		put32(entry[28:32], uint32(offset+done))
	}
	if !fs.writeEntry(&dir, index, entry) || !fs.device.Flush() {
		return int(done), ramfs.StatusResourceExhausted
	}
	return int(done), ramfs.StatusOK
}

func (fs *FileSystem) Truncate(id ramfs.NodeID, size uint64) ramfs.Status {
	dir, index, entry, status := fs.fileNode(id)
	if status != ramfs.StatusOK {
		return status
	}
	if size > uint64(get32(entry[28:32])) {
		return ramfs.StatusInvalidArgument
	}
	if size == 0 {
		fs.freeChain(entryCluster(entry))
		setEntryCluster(&entry, 0)
	}
	put32(entry[28:32], uint32(size))
	if !fs.writeEntry(&dir, index, entry) || !fs.device.Flush() {
		return ramfs.StatusResourceExhausted
	}
	return ramfs.StatusOK
}

func (fs *FileSystem) Stat(id ramfs.NodeID) (ramfs.Stat, ramfs.Status) {
	if id == rootNode {
		return ramfs.Stat{ID: id, Kind: ramfs.KindDirectory}, ramfs.StatusOK
	}
	_, _, entry, status := fs.node(id)
	if status != ramfs.StatusOK {
		return ramfs.Stat{}, status
	}
	return entryStat(id, entry), ramfs.StatusOK
}

func (fs *FileSystem) ReadDir(parent ramfs.NodeID, cursor int) (ramfs.Entry, int, ramfs.Status) {
	if cursor < 0 {
		return ramfs.Entry{}, cursor, ramfs.StatusInvalidArgument
	}
	dir, status := fs.directoryOf(parent)
	if status != ramfs.StatusOK {
		return ramfs.Entry{}, cursor, status
	}
	// The scan restarts at zero because a long chain precedes its short entry,
	// so a cursor in the middle of a directory cannot see it.
	fs.scan.reset()
	for index := 0; index < dir.entries(); index++ {
		entry, ok := fs.readEntry(&dir, index)
		if !ok {
			return ramfs.Entry{}, cursor, ramfs.StatusResourceExhausted
		}
		if entry[0] == entryEnd {
			break
		}
		if isLongEntry(entry) {
			fs.scan.long(entry, index)
			continue
		}
		if !listed(entry) || index < cursor {
			fs.scan.reset()
			continue
		}
		size, _ := fs.resolveName(entry, index)
		result := ramfs.Entry{Stat: entryStat(dir.node(index), entry), Size: uint8(size)}
		copy(result.Name[:], fs.name[:size])
		return result, index + 1, ramfs.StatusOK
	}
	return ramfs.Entry{}, cursor, ramfs.StatusNotFound
}

// RenameName renames an entry within its own directory. Moving an entry to
// another directory is reported as an invalid argument. The new chain reuses
// the run the old one occupied whenever it fits, which keeps the node ID.
func (fs *FileSystem) RenameName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	target, status := fs.parentAndName(parent, name)
	if status != ramfs.StatusOK {
		return status
	}
	dir, index, entry, status := fs.node(id)
	if status != ramfs.StatusOK {
		return status
	}
	if dir.key != target.key {
		return ramfs.StatusInvalidArgument
	}
	_, _, found, status := fs.find(&dir)
	if status != ramfs.StatusOK {
		return status
	}
	if found {
		return ramfs.StatusAlreadyExists
	}
	old, status := fs.locate(&dir, index)
	if status != ramfs.StatusOK {
		return status
	}
	at := placement{count: fs.want.chain(), short: fs.want.short}
	if at.count != 0 {
		if at.short, status = fs.alias(&dir, &fs.want); status != ramfs.StatusOK {
			return status
		}
	}
	at.start = index - at.count
	last := at.start - 1
	if at.start < old {
		if at.start, status = fs.freeRun(&dir, at.count+1); status != ramfs.StatusOK {
			return status
		}
		last = index
	}
	if !fs.commit(&dir, &at, entry) || !fs.erase(&dir, old, last) || !fs.device.Flush() {
		return ramfs.StatusResourceExhausted
	}
	return ramfs.StatusOK
}

// UnlinkName removes a file or an empty directory and frees its clusters.
func (fs *FileSystem) UnlinkName(parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	dir, status := fs.parentAndName(parent, name)
	if status != ramfs.StatusOK {
		return status
	}
	at, entry, found, status := fs.find(&dir)
	if status != ramfs.StatusOK {
		return status
	}
	if !found {
		return ramfs.StatusNotFound
	}
	if isDirectory(entry) {
		if status = fs.checkEmpty(entryCluster(entry)); status != ramfs.StatusOK {
			return status
		}
	}
	fs.freeChain(entryCluster(entry))
	if !fs.erase(&dir, at.start, at.index()) || !fs.device.Flush() {
		return ramfs.StatusResourceExhausted
	}
	return ramfs.StatusOK
}

// directory is a resolved directory: its key (0 for the root, otherwise its
// first cluster) and its bounded cluster chain.
type directory struct {
	key   uint32
	chain [maxDirClusters]uint32
	count int
}

func (dir *directory) entries() int { return dir.count * entriesPerCluster }

// node encodes an entry position. Root entries keep the historical IDs
// index+2 because the root key is 0.
func (dir *directory) node(index int) ramfs.NodeID {
	return ramfs.NodeID(uint64(dir.key)<<slotBits | uint64(index+slotBase))
}

// openDirectory walks a directory chain. A chain that is longer than
// maxDirClusters, loops, or leaves the data area is reported as corrupt.
func (fs *FileSystem) openDirectory(key uint32) (directory, ramfs.Status) {
	dir := directory{key: key}
	cluster := key
	if key == 0 {
		cluster = fs.rootCluster
	}
	for dir.count < maxDirClusters {
		if !fs.valid(cluster) {
			return dir, statusCorrupt
		}
		dir.chain[dir.count] = cluster
		dir.count++
		value, ok := fs.fat(cluster)
		if !ok {
			return dir, ramfs.StatusResourceExhausted
		}
		if value >= endOfChain {
			return dir, ramfs.StatusOK
		}
		cluster = value
	}
	return dir, statusCorrupt
}

// directoryOf resolves a directory node to its directory.
func (fs *FileSystem) directoryOf(id ramfs.NodeID) (directory, ramfs.Status) {
	if id == rootNode {
		return fs.openDirectory(0)
	}
	_, _, entry, status := fs.node(id)
	if status != ramfs.StatusOK {
		return directory{}, status
	}
	if !isDirectory(entry) {
		return directory{}, ramfs.StatusNotDirectory
	}
	cluster := entryCluster(entry)
	if cluster == 0 {
		return directory{}, statusCorrupt
	}
	return fs.openDirectory(cluster)
}

// parentAndName resolves the parent directory and decodes the requested name
// into fs.want.
func (fs *FileSystem) parentAndName(parent ramfs.NodeID, name ramfs.Name) (directory, ramfs.Status) {
	dir, status := fs.directoryOf(parent)
	if status != ramfs.StatusOK {
		return dir, status
	}
	if !fs.want.set(name) {
		return dir, ramfs.StatusInvalidArgument
	}
	return dir, ramfs.StatusOK
}

// reserve returns the run of entries a new child of parent needs: one entry
// for an 8.3 name, and a long chain plus a generated alias otherwise.
func (fs *FileSystem) reserve(parent ramfs.NodeID, name ramfs.Name) (directory, placement, [32]byte, ramfs.Status) {
	var entry [32]byte
	var at placement
	dir, status := fs.parentAndName(parent, name)
	if status != ramfs.StatusOK {
		return dir, at, entry, status
	}
	_, _, found, status := fs.find(&dir)
	if status != ramfs.StatusOK {
		return dir, at, entry, status
	}
	if found {
		return dir, at, entry, ramfs.StatusAlreadyExists
	}
	at.count, at.short = fs.want.chain(), fs.want.short
	if at.count != 0 {
		if at.short, status = fs.alias(&dir, &fs.want); status != ramfs.StatusOK {
			return dir, at, entry, status
		}
	}
	at.start, status = fs.freeRun(&dir, at.count+1)
	if status != ramfs.StatusOK {
		return dir, at, entry, status
	}
	copy(entry[:11], at.short[:])
	return dir, at, entry, ramfs.StatusOK
}

// node decodes id and returns the live entry it names.
func (fs *FileSystem) node(id ramfs.NodeID) (directory, int, [32]byte, ramfs.Status) {
	var entry [32]byte
	key, slot := uint64(id)>>slotBits, uint64(id)&slotMask
	if slot < slotBase || slot >= uint64(maxDirEntries+slotBase) || key > maxCluster || key != 0 && !fs.valid(uint32(key)) {
		return directory{}, 0, entry, ramfs.StatusNotFound
	}
	dir, status := fs.openDirectory(uint32(key))
	if status != ramfs.StatusOK {
		return dir, 0, entry, ramfs.StatusNotFound
	}
	index := int(slot - slotBase)
	entry, ok := fs.readEntry(&dir, index)
	if !ok || !listed(entry) {
		return dir, 0, entry, ramfs.StatusNotFound
	}
	return dir, index, entry, ramfs.StatusOK
}

func (fs *FileSystem) fileNode(id ramfs.NodeID) (directory, int, [32]byte, ramfs.Status) {
	dir, index, entry, status := fs.node(id)
	if status == ramfs.StatusOK && isDirectory(entry) {
		status = ramfs.StatusIsDirectory
	}
	return dir, index, entry, status
}

// checkEmpty accepts a directory that holds nothing but "." and "..".
func (fs *FileSystem) checkEmpty(cluster uint32) ramfs.Status {
	if cluster == 0 {
		return statusCorrupt
	}
	dir, status := fs.openDirectory(cluster)
	if status != ramfs.StatusOK {
		return status
	}
	for index := 0; index < dir.entries(); index++ {
		entry, ok := fs.readEntry(&dir, index)
		if !ok {
			return ramfs.StatusResourceExhausted
		}
		if entry[0] == entryEnd {
			return ramfs.StatusOK
		}
		if entry[0] != entryDeleted && entry[0] != '.' {
			return ramfs.StatusNotEmpty
		}
	}
	return ramfs.StatusOK
}

func (fs *FileSystem) clusterOffset(cluster uint32) uint64 {
	return fs.dataStart + uint64(cluster-2)*sectorSize
}
func (fs *FileSystem) entryOffset(dir *directory, index int) (uint64, bool) {
	if index < 0 || index >= dir.entries() {
		return 0, false
	}
	cluster := dir.chain[index/entriesPerCluster]
	return fs.clusterOffset(cluster) + uint64(index%entriesPerCluster)*entrySize, true
}
func (fs *FileSystem) readEntry(dir *directory, index int) ([32]byte, bool) {
	var entry [32]byte
	offset, ok := fs.entryOffset(dir, index)
	if !ok || !fs.readAt(offset, entry[:]) {
		return entry, false
	}
	return entry, true
}
func (fs *FileSystem) writeEntry(dir *directory, index int, entry [32]byte) bool {
	offset, ok := fs.entryOffset(dir, index)
	return ok && fs.writeAt(offset, entry[:])
}

// resolveName writes the name an entry is listed and matched under into
// fs.name: the long name when its chain is intact, the 8.3 name otherwise. It
// also returns the first entry of the chain.
func (fs *FileSystem) resolveName(entry [32]byte, index int) (int, int) {
	short := shortOf(entry)
	if fs.scan.complete(short) {
		copy(fs.name[:], fs.scan.name[:fs.scan.size])
		return fs.scan.size, fs.scan.start
	}
	return decodeShort(short, &fs.name), index
}

// find looks for the name in fs.want. Matching is on the resolved name and
// case-insensitive, so an 8.3 entry and a long chain are found alike.
func (fs *FileSystem) find(dir *directory) (placement, [32]byte, bool, ramfs.Status) {
	var at placement
	fs.scan.reset()
	for i := 0; i < dir.entries(); i++ {
		entry, ok := fs.readEntry(dir, i)
		if !ok {
			return at, entry, false, ramfs.StatusResourceExhausted
		}
		if entry[0] == entryEnd {
			break
		}
		if isLongEntry(entry) {
			fs.scan.long(entry, i)
			continue
		}
		if !listed(entry) {
			fs.scan.reset()
			continue
		}
		size, start := fs.resolveName(entry, i)
		if fs.want.same(fs.name[:size]) {
			at.start, at.count = start, i-start
			return at, entry, true, ramfs.StatusOK
		}
		fs.scan.reset()
	}
	return at, [32]byte{}, false, ramfs.StatusOK
}

// locate returns the first entry of the chain covering the short entry at
// index.
func (fs *FileSystem) locate(dir *directory, index int) (int, ramfs.Status) {
	fs.scan.reset()
	for i := 0; i <= index && i < dir.entries(); i++ {
		entry, ok := fs.readEntry(dir, i)
		if !ok {
			return index, ramfs.StatusResourceExhausted
		}
		if isLongEntry(entry) {
			fs.scan.long(entry, i)
			continue
		}
		if i == index {
			_, start := fs.resolveName(entry, i)
			return start, ramfs.StatusOK
		}
		fs.scan.reset()
	}
	return index, ramfs.StatusOK
}

// freeRun finds count consecutive free slots, growing the directory by one
// zeroed cluster while it is below maxDirClusters. A run may span a cluster
// boundary because entry indices are linear over the whole chain.
func (fs *FileSystem) freeRun(dir *directory, count int) (int, ramfs.Status) {
	for {
		run, start := 0, 0
		for i := 0; i < dir.entries(); i++ {
			entry, ok := fs.readEntry(dir, i)
			if !ok {
				return 0, ramfs.StatusResourceExhausted
			}
			if entry[0] != entryEnd && entry[0] != entryDeleted {
				run = 0
				continue
			}
			if run == 0 {
				start = i
			}
			if run++; run == count {
				return start, ramfs.StatusOK
			}
		}
		if status := fs.grow(dir); status != ramfs.StatusOK {
			return 0, status
		}
	}
}

func (fs *FileSystem) grow(dir *directory) ramfs.Status {
	if dir.count == maxDirClusters {
		return ramfs.StatusResourceExhausted
	}
	cluster := fs.allocate()
	if cluster == 0 {
		return ramfs.StatusResourceExhausted
	}
	if !fs.setFAT(dir.chain[dir.count-1], cluster) {
		fs.freeChain(cluster)
		return ramfs.StatusResourceExhausted
	}
	dir.chain[dir.count] = cluster
	dir.count++
	return ramfs.StatusOK
}
func (fs *FileSystem) valid(cluster uint32) bool {
	return cluster >= 2 && cluster < fs.totalClusters+2
}
func (fs *FileSystem) fat(cluster uint32) (uint32, bool) {
	var data [4]byte
	if !fs.valid(cluster) || !fs.readAt(fs.fatStart+uint64(cluster)*4, data[:]) {
		return 0, false
	}
	return get32(data[:]) & clusterMask, true
}
func (fs *FileSystem) setFAT(cluster, value uint32) bool {
	var data [4]byte
	put32(data[:], value)
	return fs.valid(cluster) && fs.writeAt(fs.fatStart+uint64(cluster)*4, data[:])
}
func (fs *FileSystem) next(cluster uint32) (uint32, bool) {
	value, ok := fs.fat(cluster)
	return value, ok && fs.valid(value)
}
func (fs *FileSystem) advance(cluster uint32, count uint32) (uint32, bool) {
	if !fs.valid(cluster) {
		return 0, false
	}
	for i := uint32(0); i < count; i++ {
		var ok bool
		cluster, ok = fs.next(cluster)
		if !ok {
			return 0, false
		}
	}
	return cluster, true
}
func (fs *FileSystem) allocate() uint32 {
	for cluster := uint32(3); cluster < fs.totalClusters+2; cluster++ {
		value, ok := fs.fat(cluster)
		if !ok {
			return 0
		}
		if value == 0 {
			if !fs.setFAT(cluster, endOfChainMark) {
				return 0
			}
			for i := range fs.sector {
				fs.sector[i] = 0
			}
			if !fs.writeAt(fs.clusterOffset(cluster), fs.sector[:]) {
				return 0
			}
			return cluster
		}
	}
	return 0
}
func (fs *FileSystem) ensure(first uint32, count uint32) bool {
	if count == 0 {
		return true
	}
	cluster := first
	for i := uint32(1); i < count; i++ {
		next, ok := fs.next(cluster)
		if !ok {
			next = fs.allocate()
			if next == 0 || !fs.setFAT(cluster, next) {
				return false
			}
		}
		cluster = next
	}
	return true
}

// freeChain releases a chain. Each link is cleared before it is followed, so
// a looped chain ends at the first revisited cluster.
func (fs *FileSystem) freeChain(cluster uint32) {
	for steps := uint32(0); steps < fs.totalClusters && fs.valid(cluster); steps++ {
		next, ok := fs.fat(cluster)
		if !ok || !fs.setFAT(cluster, 0) {
			return
		}
		cluster = next
	}
}
func entryCluster(entry [32]byte) uint32 {
	return uint32(get16(entry[20:22]))<<16 | uint32(get16(entry[26:28]))
}
func setEntryCluster(entry *[32]byte, cluster uint32) {
	put16(entry[20:22], uint16(cluster>>16))
	put16(entry[26:28], uint16(cluster))
}
func isDirectory(entry [32]byte) bool { return entry[11]&attrDirectory != 0 }

// listed reports whether entry is a live file or subdirectory: not free,
// deleted, a volume label or long-name fragment, nor "." or "..".
func listed(entry [32]byte) bool {
	return entry[0] != entryEnd && entry[0] != entryDeleted && entry[0] != '.' && entry[11]&attrVolume == 0
}
func entryStat(id ramfs.NodeID, entry [32]byte) ramfs.Stat {
	if isDirectory(entry) {
		return ramfs.Stat{ID: id, Kind: ramfs.KindDirectory}
	}
	return ramfs.Stat{ID: id, Kind: ramfs.KindFile, Size: uint64(get32(entry[28:32]))}
}
func dotEntry(name [11]byte, cluster uint32) [32]byte {
	var entry [32]byte
	copy(entry[:11], name[:])
	entry[11] = attrDirectory
	setEntryCluster(&entry, cluster)
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
