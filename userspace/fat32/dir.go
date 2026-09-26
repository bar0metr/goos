package fat32

import "goos/kernel/ramfs"

// directory is a resolved directory: its key (0 for the root, otherwise its
// first cluster), its first cluster, and the number of clusters in its chain,
// which open has walked and bounded.
type directory struct {
	key      uint32
	first    uint32
	clusters uint64
}

func (fs *FileSystem) entriesPerCluster() int { return int(fs.clusterSize / entrySize) }

func (fs *FileSystem) entries(dir *directory) int {
	return int(dir.clusters) * fs.entriesPerCluster()
}

// node encodes an entry position. Root entries keep the historical IDs
// index+2 because the root key is 0.
func (dir *directory) node(index int) ramfs.NodeID {
	return ramfs.NodeID(uint64(dir.key)<<slotBits | uint64(index+slotBase))
}

// openDirectory resolves a directory key. The whole chain is walked, so a
// chain that loops, leaves the data area, or is longer than a directory may
// be is reported as corrupt before any entry is used; the walk is remembered,
// so repeating it costs one step. A subdirectory must begin with a "." entry
// naming itself, which is how a key that was reused for file data, or never
// was a directory, is told apart from a real one.
func (fs *FileSystem) openDirectory(key uint32) (directory, ramfs.Status) {
	dir := directory{key: key, first: key}
	if key == 0 {
		dir.first = fs.rootCluster
	}
	_, count, status := fs.last(dir.first)
	if status != ramfs.StatusOK {
		return dir, status
	}
	if count*uint64(fs.entriesPerCluster()) > maxDirEntries {
		return dir, ramfs.StatusCorrupt
	}
	dir.clusters = count
	if key != 0 {
		entry, status := fs.readEntry(&dir, 0)
		if status != ramfs.StatusOK {
			return dir, status
		}
		if !sameShort(entry, dotName) || !isDirectory(entry) || entryCluster(entry) != key {
			return dir, ramfs.StatusCorrupt
		}
	}
	return dir, ramfs.StatusOK
}

// directoryOf resolves a directory node to its directory.
func (fs *FileSystem) directoryOf(id ramfs.NodeID) (directory, ramfs.Status) {
	if !fs.mounted {
		return directory{}, ramfs.StatusNotFound
	}
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
	if !fs.valid(cluster) {
		return directory{}, ramfs.StatusCorrupt
	}
	return fs.openDirectory(cluster)
}

// node decodes id and returns the live entry it names. An ID that names no
// directory, or no live entry in one, is not found; only a device failure is
// reported as such.
func (fs *FileSystem) node(id ramfs.NodeID) (directory, int, [32]byte, ramfs.Status) {
	var entry [32]byte
	key, slot := uint64(id)>>slotBits, uint64(id)&slotMask
	if !fs.mounted || slot < slotBase || slot >= maxDirEntries+slotBase || key > uint64(clusterMask) || key != 0 && !fs.valid(uint32(key)) {
		return directory{}, 0, entry, ramfs.StatusNotFound
	}
	dir, status := fs.openDirectory(uint32(key))
	if status == ramfs.StatusIOError {
		return dir, 0, entry, status
	}
	if status != ramfs.StatusOK {
		return dir, 0, entry, ramfs.StatusNotFound
	}
	index := int(slot - slotBase)
	if index >= fs.entries(&dir) {
		return dir, 0, entry, ramfs.StatusNotFound
	}
	entry, status = fs.readEntry(&dir, index)
	if status != ramfs.StatusOK {
		return dir, 0, entry, status
	}
	if !listed(entry) {
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

// entryOffset is the device offset of entry index of dir, which must be
// within the chain open counted.
func (fs *FileSystem) entryOffset(dir *directory, index int) (uint64, ramfs.Status) {
	if index < 0 || index >= fs.entries(dir) {
		return 0, ramfs.StatusCorrupt
	}
	perCluster := fs.entriesPerCluster()
	cluster, found, status := fs.seek(dir.first, uint64(index/perCluster))
	if status != ramfs.StatusOK {
		return 0, status
	}
	if !found {
		return 0, ramfs.StatusCorrupt
	}
	return fs.clusterOffset(cluster) + uint64(index%perCluster)*entrySize, ramfs.StatusOK
}

func (fs *FileSystem) readEntry(dir *directory, index int) ([32]byte, ramfs.Status) {
	var entry [32]byte
	offset, status := fs.entryOffset(dir, index)
	if status != ramfs.StatusOK {
		return entry, status
	}
	if !fs.readMeta(offset, entry[:]) {
		return entry, ramfs.StatusIOError
	}
	return entry, ramfs.StatusOK
}

func (fs *FileSystem) writeEntry(dir *directory, index int, entry [32]byte) ramfs.Status {
	// An entry may name a chain whose FAT sectors are still in the window:
	// the chain reaches the disk first.
	if !fs.flushFAT() {
		return ramfs.StatusIOError
	}
	offset, status := fs.entryOffset(dir, index)
	if status != ramfs.StatusOK {
		return status
	}
	if !fs.write(offset, entry[:]) {
		return ramfs.StatusIOError
	}
	return ramfs.StatusOK
}

// resolveName writes the name an entry is listed and matched under into
// fs.name: the long name when its chain is intact and names something this
// system can open, the 8.3 name otherwise. It also returns the first entry of
// the chain and whether the name is a long one. An entry whose 8.3 name holds
// bytes no name may (a NUL, a control character, a reserved character) and
// that has no usable long name has no name at all: size is zero, and it is
// neither listed nor matched, though its slots stay in use.
func (fs *FileSystem) resolveName(entry [32]byte, index int) (int, int, bool) {
	if size, ok := fs.scan.complete(shortOf(entry), fs.name[:]); ok {
		return size, fs.scan.start, true
	}
	size := decodeShort(entry, fs.name[:])
	if !validLongName(fs.name[:size]) {
		return 0, index, false
	}
	return size, index, false
}

// matches reports whether the entry just resolved answers to the requested
// name: under the name it is listed by or, as in Windows and Linux, under its
// 8.3 alias. Matching the alias also keeps a new 8.3 name from duplicating an
// alias already in the directory.
func (fs *FileSystem) matches(entry [32]byte, size int, long bool) bool {
	if fs.want.same(fs.name[:size]) {
		return true
	}
	if !long {
		return false
	}
	var short [maxShortName]byte
	return fs.want.same(short[:decodeShort(entry, short[:])])
}

// search is the result of one pass over a directory: where the requested
// name is, if it is there, and where a run of free entries long enough for a
// new one begins. free may lie past the end of the chain, which then has to
// grow to reach it; marker is the index of the end-of-directory entry, or -1.
type search struct {
	at     placement
	entry  [32]byte
	found  bool
	free   int
	marker int
}

// find looks for the name in fs.want when match is set and, when need is
// nonzero, for need consecutive free entries, in a single pass. Matching is on the resolved
// name and case-insensitive, so an 8.3 entry and a long chain are found
// alike. Nothing is live after an end-of-directory entry, so the scan stops
// there.
func (fs *FileSystem) find(dir *directory, need int, match bool) (search, ramfs.Status) {
	result := search{free: -1, marker: -1}
	run, start := 0, 0
	fs.scan.reset()
	total := fs.entries(dir)
	index := 0
	for ; index < total; index++ {
		entry, status := fs.readEntry(dir, index)
		if status != ramfs.StatusOK {
			return result, status
		}
		if entry[0] == entryEnd {
			result.marker = index
			break
		}
		if entry[0] == entryDeleted {
			if run == 0 {
				start = index
			}
			if run++; run == need && result.free < 0 {
				result.free = start
			}
			fs.scan.reset()
			continue
		}
		run = 0
		if isLongEntry(entry) {
			fs.scan.long(entry, index)
			continue
		}
		if !listed(entry) {
			fs.scan.reset()
			continue
		}
		size, first, long := fs.resolveName(entry, index)
		if match && size != 0 && fs.matches(entry, size, long) {
			result.at, result.entry, result.found = placement{start: first, count: index - first, short: shortOf(entry)}, entry, true
			return result, ramfs.StatusOK
		}
		fs.scan.reset()
	}
	if need != 0 && result.free < 0 {
		// Everything from the open run, or from where the scan stopped, is
		// free: past the end marker and past the end of the chain alike.
		result.free = index
		if run != 0 {
			result.free = start
		}
	}
	return result, ramfs.StatusOK
}

// cover grows dir until it holds count entries. Each new cluster is zeroed
// before it is linked, so the directory never shows an unwritten entry, and
// no directory grows past the 65536 entries FAT allows.
func (fs *FileSystem) cover(dir *directory, count int) ramfs.Status {
	perCluster := fs.entriesPerCluster()
	if count > maxDirEntries {
		return ramfs.StatusNoSpace
	}
	needed := uint64((count + perCluster - 1) / perCluster)
	if needed <= dir.clusters {
		return ramfs.StatusOK
	}
	tail, have, status := fs.last(dir.first)
	if status != ramfs.StatusOK {
		return status
	}
	for have < needed {
		cluster, status := fs.allocate()
		if status != ramfs.StatusOK {
			return status
		}
		if !fs.zeroCluster(cluster) {
			fs.freeChain(cluster)
			return ramfs.StatusIOError
		}
		if status = fs.setFAT(tail, cluster); status != ramfs.StatusOK {
			fs.freeChain(cluster)
			return status
		}
		fs.remember(dir.first, uint32(have), cluster)
		tail, have = cluster, have+1
		dir.clusters = have
	}
	return ramfs.StatusOK
}

// checkEmpty accepts a directory that holds nothing but "." and "..".
func (fs *FileSystem) checkEmpty(cluster uint32) ramfs.Status {
	if !fs.valid(cluster) {
		return ramfs.StatusCorrupt
	}
	dir, status := fs.openDirectory(cluster)
	if status != ramfs.StatusOK {
		return status
	}
	for index := 0; index < fs.entries(&dir); index++ {
		entry, status := fs.readEntry(&dir, index)
		if status != ramfs.StatusOK {
			return status
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

// parentKey reads the ".." entry of a subdirectory: the key of its parent.
func (fs *FileSystem) parentKey(key uint32) (uint32, ramfs.Status) {
	dir, status := fs.openDirectory(key)
	if status != ramfs.StatusOK {
		return 0, status
	}
	if fs.entries(&dir) < 2 {
		return 0, ramfs.StatusCorrupt
	}
	entry, status := fs.readEntry(&dir, 1)
	if status != ramfs.StatusOK {
		return 0, status
	}
	if !sameShort(entry, dotDotName) || !isDirectory(entry) {
		return 0, ramfs.StatusCorrupt
	}
	parent := entryCluster(entry)
	if parent == fs.rootCluster {
		// Some formatters record the root by its cluster rather than zero.
		parent = 0
	}
	return parent, ramfs.StatusOK
}

// within reports whether directory key lies inside the subtree of the
// directory whose first cluster is ancestor, walking ".." links up to the
// root. The walk is bounded by the cluster count, so a ".." cycle is corrupt
// rather than endless.
func (fs *FileSystem) within(key, ancestor uint32) (bool, ramfs.Status) {
	for steps := uint32(0); steps <= fs.clusters; steps++ {
		if key == ancestor {
			return true, ramfs.StatusOK
		}
		if key == 0 {
			return false, ramfs.StatusOK
		}
		parent, status := fs.parentKey(key)
		if status != ramfs.StatusOK {
			return false, status
		}
		key = parent
	}
	return false, ramfs.StatusCorrupt
}
