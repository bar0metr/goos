package fat32

import "goos/kernel/ramfs"

func (fs *FileSystem) OpenName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	dir, status := fs.parentAndName(parent, name, false)
	if status != ramfs.StatusOK {
		return 0, status
	}
	result, status := fs.find(&dir, 0, true)
	if status != ramfs.StatusOK {
		return 0, status
	}
	if !result.found {
		return 0, ramfs.StatusNotFound
	}
	return dir.node(result.at.index()), ramfs.StatusOK
}

func (fs *FileSystem) CreateName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	dir, at, entry, marker, status := fs.reserve(parent, name)
	if status != ramfs.StatusOK {
		return 0, status
	}
	entry[11] = attrArchive
	fs.stampCreated(&entry)
	if status = fs.place(&dir, &at, entry, marker); status == ramfs.StatusOK {
		fs.touchDirectory(&dir)
	}
	if synced := fs.sync(); status == ramfs.StatusOK {
		status = synced
	}
	if status != ramfs.StatusOK {
		return 0, status
	}
	return dir.node(at.index()), ramfs.StatusOK
}

// MkdirName creates a directory of one zeroed cluster holding "." and "..".
// Per the FAT specification ".." records cluster 0 when the parent is the
// root.
func (fs *FileSystem) MkdirName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	dir, at, entry, marker, status := fs.reserve(parent, name)
	if status != ramfs.StatusOK {
		return 0, status
	}
	cluster, status := fs.allocate()
	if status != ramfs.StatusOK {
		return 0, status
	}
	entry[11] = attrDirectory
	setEntryCluster(&entry, cluster)
	fs.stampCreated(&entry)
	// "." and ".." carry the new directory's times, as mkfs and Linux write
	// them.
	dot := dotEntry(dotName, cluster, entry)
	dotDot := dotEntry(dotDotName, dir.key, entry)
	base := fs.clusterOffset(cluster)
	switch {
	case !fs.zeroCluster(cluster) || !fs.write(base, dot[:]) || !fs.write(base+entrySize, dotDot[:]):
		status = ramfs.StatusIOError
	default:
		status = fs.place(&dir, &at, entry, marker)
	}
	if status != ramfs.StatusOK {
		fs.freeChain(cluster)
		fs.sync()
		return 0, status
	}
	fs.touchDirectory(&dir)
	if status = fs.sync(); status != ramfs.StatusOK {
		return 0, status
	}
	return dir.node(at.index()), ramfs.StatusOK
}

func (fs *FileSystem) Stat(id ramfs.NodeID) (ramfs.Stat, ramfs.Status) {
	if id == rootNode && fs.mounted {
		return ramfs.Stat{ID: id, Kind: ramfs.KindDirectory, Attributes: ramfs.AttrDirectory, AttributesAvailable: true}, ramfs.StatusOK
	}
	_, _, entry, status := fs.node(id)
	if status != ramfs.StatusOK {
		return ramfs.Stat{}, status
	}
	return entryStat(id, entry), ramfs.StatusOK
}

// ReadDir returns the first listed entry at or after cursor and the cursor
// after it. A cursor this call returned points just past a short entry, so
// the next entry's long chain starts at or after it and the scan can begin
// there instead of at the start of the directory.
func (fs *FileSystem) ReadDir(parent ramfs.NodeID, cursor int) (ramfs.Entry, int, ramfs.Status) {
	if cursor < 0 {
		return ramfs.Entry{}, cursor, ramfs.StatusInvalidArgument
	}
	dir, status := fs.directoryOf(parent)
	if status != ramfs.StatusOK {
		return ramfs.Entry{}, cursor, status
	}
	fs.scan.reset()
	for index := cursor; index < fs.entries(&dir); index++ {
		entry, status := fs.readEntry(&dir, index)
		if status != ramfs.StatusOK {
			return ramfs.Entry{}, cursor, status
		}
		if entry[0] == entryEnd {
			break
		}
		if isLongEntry(entry) {
			fs.scan.long(entry, index)
			continue
		}
		if !listed(entry) {
			fs.scan.reset()
			continue
		}
		size, _, _ := fs.resolveName(entry, index)
		if size == 0 {
			fs.scan.reset()
			continue
		}
		result := ramfs.Entry{Stat: entryStat(dir.node(index), entry), Size: uint16(size)}
		copy(result.Name[:], fs.name[:size])
		return result, index + 1, ramfs.StatusOK
	}
	return ramfs.Entry{}, cursor, ramfs.StatusNotFound
}

// RenameName gives an entry a new name in parent, which may be another
// directory. Within one directory the short entry keeps its index whenever
// the new chain fits the run the old one occupied, which keeps the node ID. A
// move writes the new entry first, then repoints a moved directory's "..",
// and erases the old entry last, as Linux does: an interruption can leave the
// entry under both names, never under neither. A directory cannot be moved
// into its own subtree, and an existing target name is refused.
func (fs *FileSystem) RenameName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	if status := fs.mutable(); status != ramfs.StatusOK {
		return status
	}
	target, status := fs.parentAndName(parent, name, true)
	if status != ramfs.StatusOK {
		return status
	}
	source, status := fs.objectLocation(id)
	if status != ramfs.StatusOK {
		return status
	}
	dir, index, entry, status := fs.node(source)
	if status != ramfs.StatusOK {
		return status
	}
	moved := entryCluster(entry)
	if isDirectory(entry) && dir.key != target.key {
		inside, status := fs.within(target.key, moved)
		if status != ramfs.StatusOK {
			return status
		}
		if inside {
			return ramfs.StatusInvalidArgument
		}
	}
	count := fs.want.chain()
	result, status := fs.find(&target, count+1, true)
	if status != ramfs.StatusOK {
		return status
	}
	if result.found {
		// Only the entry itself may already answer to the new name: a change
		// of case, which rewrites its chain in place.
		if dir.key != target.key || result.at.index() != index {
			return ramfs.StatusAlreadyExists
		}
		if result, status = fs.find(&target, count+1, false); status != ramfs.StatusOK {
			return status
		}
	}
	old, status := fs.locate(&dir, index)
	if status != ramfs.StatusOK {
		return status
	}
	at := placement{count: count, short: fs.want.short}
	if count != 0 {
		if at.short, status = fs.alias(&target, &fs.want); status != ramfs.StatusOK {
			return status
		}
	}
	entry[12] = fs.want.caseFlags()
	if dir.key == target.key {
		at.start = index - count
		last := at.start - 1
		if at.start < old {
			at.start, last = result.free, index
			if status = fs.cover(&target, at.start+count+1); status != ramfs.StatusOK {
				return status
			}
		}
		if status = fs.place(&target, &at, entry, result.marker); status == ramfs.StatusOK {
			status = fs.erase(&dir, old, last)
		}
	} else {
		at.start = result.free
		if status = fs.cover(&target, at.start+count+1); status != ramfs.StatusOK {
			return status
		}
		if status = fs.place(&target, &at, entry, result.marker); status == ramfs.StatusOK && isDirectory(entry) {
			status = fs.repoint(moved, target.key)
		}
		if status == ramfs.StatusOK {
			status = fs.erase(&dir, old, index)
		}
		if status == ramfs.StatusOK {
			fs.touchDirectory(&dir)
		}
	}
	if status == ramfs.StatusOK {
		fs.moveObjects(source, target.node(at.start+count))
		fs.touchDirectory(&target)
	}
	if synced := fs.sync(); status == ramfs.StatusOK {
		status = synced
	}
	return status
}

// repoint sets the ".." entry of the directory at cluster to parent.
func (fs *FileSystem) repoint(cluster, parent uint32) ramfs.Status {
	if !fs.valid(cluster) {
		return ramfs.StatusCorrupt
	}
	dir, status := fs.openDirectory(cluster)
	if status != ramfs.StatusOK {
		return status
	}
	if fs.entries(&dir) < 2 {
		return ramfs.StatusCorrupt
	}
	entry, status := fs.readEntry(&dir, 1)
	if status != ramfs.StatusOK {
		return status
	}
	if !sameShort(entry, dotDotName) {
		return ramfs.StatusCorrupt
	}
	setEntryCluster(&entry, parent)
	return fs.writeEntry(&dir, 1, entry)
}

// UnlinkName removes a file or an empty directory. The entry goes first and
// its clusters after, so an interruption loses clusters rather than leaving
// an entry that names freed ones.
func (fs *FileSystem) UnlinkName(parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	if status := fs.mutable(); status != ramfs.StatusOK {
		return status
	}
	dir, status := fs.parentAndName(parent, name, false)
	if status != ramfs.StatusOK {
		return status
	}
	result, status := fs.find(&dir, 0, true)
	if status != ramfs.StatusOK {
		return status
	}
	if !result.found {
		return ramfs.StatusNotFound
	}
	if fs.objectOpen(dir.node(result.at.index())) {
		// GoOS does not implement delete-while-open. The directory entry and
		// cluster chain remain intact until every handle is closed.
		return ramfs.StatusPermissionDenied
	}
	cluster := entryCluster(result.entry)
	if isDirectory(result.entry) {
		if status = fs.checkEmpty(cluster); status != ramfs.StatusOK {
			return status
		}
	}
	if status = fs.erase(&dir, result.at.start, result.at.index()); status != ramfs.StatusOK {
		return status
	}
	if fs.valid(cluster) {
		fs.freeChain(cluster)
	}
	fs.touchDirectory(&dir)
	return fs.sync()
}

// parentAndName resolves the parent directory and decodes the requested name
// into fs.want.
func (fs *FileSystem) parentAndName(parent ramfs.NodeID, name ramfs.Name, creating bool) (directory, ramfs.Status) {
	if !fs.mounted {
		return directory{}, ramfs.StatusNotFound
	}
	dir, status := fs.directoryOf(parent)
	if status != ramfs.StatusOK {
		return dir, status
	}
	if !fs.want.set(name, creating) {
		return dir, ramfs.StatusInvalidArgument
	}
	return dir, ramfs.StatusOK
}

// reserve returns the run of entries a new child of parent needs — one entry
// for an 8.3 name, a long chain plus a generated alias otherwise — growing
// the directory to hold it. It also returns the end-of-directory index the
// scan met, which place needs.
func (fs *FileSystem) reserve(parent ramfs.NodeID, name ramfs.Name) (directory, placement, [32]byte, int, ramfs.Status) {
	var entry [32]byte
	var at placement
	if status := fs.mutable(); status != ramfs.StatusOK {
		return directory{}, at, entry, -1, status
	}
	dir, status := fs.parentAndName(parent, name, true)
	if status != ramfs.StatusOK {
		return dir, at, entry, -1, status
	}
	at.count, at.short = fs.want.chain(), fs.want.short
	result, status := fs.find(&dir, at.count+1, true)
	if status != ramfs.StatusOK {
		return dir, at, entry, -1, status
	}
	if result.found {
		return dir, at, entry, -1, ramfs.StatusAlreadyExists
	}
	if at.count != 0 {
		if at.short, status = fs.alias(&dir, &fs.want); status != ramfs.StatusOK {
			return dir, at, entry, -1, status
		}
	}
	at.start = result.free
	if status = fs.cover(&dir, at.start+at.count+1); status != ramfs.StatusOK {
		return dir, at, entry, -1, status
	}
	copy(entry[:11], at.short[:])
	entry[12] = fs.want.caseFlags()
	return dir, at, entry, result.marker, ramfs.StatusOK
}

// place commits a reserved run. When the run took the end-of-directory entry,
// the entry after it becomes the new end: nothing past the old end was ever
// live, but it need not have been zero, and it must not become visible.
func (fs *FileSystem) place(dir *directory, at *placement, entry [32]byte, marker int) ramfs.Status {
	if status := fs.commit(dir, at, entry); status != ramfs.StatusOK {
		return status
	}
	next := at.index() + 1
	if marker < 0 || at.index() < marker || next >= fs.entries(dir) {
		return ramfs.StatusOK
	}
	following, status := fs.readEntry(dir, next)
	if status != ramfs.StatusOK || following[0] == entryEnd {
		return status
	}
	return fs.writeEntry(dir, next, [32]byte{})
}
