package ext4

// Directory mutation. Linear directories grow by whole blocks and stay
// linear. In a hash-indexed directory a name goes to the leaf its hash
// selects; a full leaf is split by hash into a new block whose index entry
// joins the parent node. A full index node is refused: growing the index by
// a level is not implemented, which bounds an indexed directory at the
// leaves one index node holds (507 at 4 KiB blocks).

// entryLoc is where a name lives in its directory.
type entryLoc struct {
	logical  uint64
	offset   int
	prev     int
	inode    uint32
	fileType uint8
}

func direntSize(nameLen int) int { return (8 + nameLen + 3) &^ 3 }

// maxDirBlocks keeps directories below 2 GiB, the size e2fsck accepts
// without large_dir.
func (fs *FS) maxDirBlocks() uint64 { return (1 << 31) / uint64(fs.blockSize) }

// leafLimit is where the records of a leaf block end.
func (fs *FS) leafLimit() int {
	if fs.sb.HasMetadataCsum() {
		return int(fs.blockSize) - dirTailSize
	}
	return int(fs.blockSize)
}

// sealLeaf recomputes a leaf block's checksum tail.
func (fs *FS) sealLeaf(dir *Inode, data []byte) {
	if fs.sb.HasMetadataCsum() {
		tail := len(data) - dirTailSize
		putLE32(data[tail+8:], crc32c(dir.csumSeed, data[:tail]))
	}
}

// sealDx recomputes the checksum of a hash-tree node whose entries start at
// offset.
func (fs *FS) sealDx(dir *Inode, data []byte, offset int) {
	if !fs.sb.HasMetadataCsum() {
		return
	}
	limit, count := int(le16(data, offset)), int(le16(data, offset+2))
	tail := offset + 8*limit
	crc := crc32c(dir.csumSeed, data[:offset+8*count])
	crc = crc32c(crc, data[tail:tail+4])
	crc = crc32cZeros(crc, 4)
	putLE32(data[tail+4:], crc)
}

// initLeaf formats an empty leaf: one unused record and the checksum tail.
func (fs *FS) initLeaf(data []byte) {
	for index := range data {
		data[index] = 0
	}
	limit := fs.leafLimit()
	putLE16(data[4:], uint16(limit))
	if fs.sb.HasMetadataCsum() {
		tail := len(data) - dirTailSize
		putLE16(data[tail+4:], dirTailSize)
		data[tail+7] = dirTailFileType
	}
}

func putDirent(data []byte, offset int, inode uint32, length int, name []byte, fileType uint8) {
	putLE32(data[offset:], inode)
	putLE16(data[offset+4:], uint16(length))
	data[offset+6] = byte(len(name))
	data[offset+7] = fileType
	copy(data[offset+8:], name)
	for index := offset + 8 + len(name); index < offset+direntSize(len(name)); index++ {
		data[index] = 0
	}
}

// leafSlot finds room for a record of need bytes in a leaf: an unused
// record that is large enough, or the slack after a live one. It returns
// the record's offset and how many of its bytes stay with it.
func (fs *FS) leafSlot(dir *Inode, data []byte, need int) (int, int, bool, error) {
	end, err := fs.leafEnd(dir, data)
	if err != nil {
		return 0, 0, false, err
	}
	reader := direntReader{data: data, end: end, fs: fs}
	for {
		raw, ok, err := reader.next()
		if err != nil || !ok {
			return 0, 0, false, err
		}
		used := 0
		if raw.inode != 0 {
			used = direntSize(raw.nameLen)
		}
		if raw.recLen-used >= need {
			return raw.offset, used, true, nil
		}
	}
}

// placeDirent writes a record into the slot leafSlot found.
func placeDirent(data []byte, offset int, used int, inode uint32, name []byte, fileType uint8) {
	length := recLen(data, offset)
	if used == 0 {
		putDirent(data, offset, inode, length, name, fileType)
		return
	}
	putLE16(data[offset+4:], uint16(used))
	putDirent(data, offset+used, inode, length-used, name, fileType)
}

// dirPhysical maps a directory block to its disk block.
func (fs *FS) dirPhysical(dir *Inode, logical uint64) (uint64, error) {
	mapping, err := fs.mapBlock(dir, logical)
	if err != nil {
		return 0, err
	}
	if mapping.Hole || mapping.Unwritten {
		return 0, corrupt("directory block not allocated")
	}
	return mapping.Physical, nil
}

// stageDirBlock returns a writable copy of a directory block.
func (fs *FS) stageDirBlock(dir *Inode, logical uint64) ([]byte, error) {
	physical, err := fs.dirPhysical(dir, logical)
	if err != nil {
		return nil, err
	}
	return fs.stage(physical)
}

// growDir appends a block to dir and returns its logical number and a
// zeroed writable copy.
func (fs *FS) growDir(dir *Inode) (uint64, []byte, error) {
	logical := dir.Size / uint64(fs.blockSize)
	if logical+1 > fs.maxDirBlocks() {
		return 0, nil, fail(KindNoSpace, "directory at its maximum size")
	}
	goal := fs.inodeGoal(dir)
	if logical > 0 {
		if previous, err := fs.mapBlock(dir, logical-1); err == nil && !previous.Hole {
			goal = previous.Physical + 1
		}
	}
	block, _, err := fs.allocBlocks(goal, 1)
	if err != nil {
		return 0, nil, err
	}
	if err := fs.insertExtent(dir, extent{logical: uint32(logical), length: 1, physical: block}); err != nil {
		return 0, nil, err
	}
	data, err := fs.stageZero(block, true)
	if err != nil {
		return 0, nil, err
	}
	dir.Size += uint64(fs.blockSize)
	dir.Blocks512 += uint64(fs.blockSize / 512)
	dir.dirty = true
	return logical, data, nil
}

// addEntry links name to inode in dir. The caller has checked that the
// name is absent.
func (fs *FS) addEntry(dir *Inode, name []byte, inode uint32, fileType uint8) error {
	if fs.isIndexed(dir) {
		return fs.addIndexed(dir, name, inode, fileType)
	}
	blocks, err := fs.dirBlocks(dir)
	if err != nil {
		return err
	}
	need := direntSize(len(name))
	for index := uint64(0); index < blocks; index++ {
		data, err := fs.dirBlock(dir, index)
		if err != nil {
			return err
		}
		if data == nil {
			continue
		}
		offset, used, ok, err := fs.leafSlot(dir, data, need)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if data, err = fs.stageDirBlock(dir, index); err != nil {
			return err
		}
		placeDirent(data, offset, used, inode, name, fileType)
		fs.sealLeaf(dir, data)
		return nil
	}
	_, data, err := fs.growDir(dir)
	if err != nil {
		return err
	}
	fs.initLeaf(data)
	placeDirent(data, 0, 0, inode, name, fileType)
	fs.sealLeaf(dir, data)
	return nil
}

// dxDescent is the walk from the hash-tree root to a leaf.
type dxDescent struct {
	frames  [3]dxFrame
	levels  int
	leaf    uint64
	hash    uint32
	version uint8
}

func (fs *FS) dxDescend(dir *Inode, name []byte) (dxDescent, error) {
	var descent dxDescent
	blocks, err := fs.dirBlocks(dir)
	if err != nil {
		return descent, err
	}
	root, err := fs.dirBlock(dir, 0)
	if err != nil {
		return descent, err
	}
	if root == nil {
		return descent, corrupt("hole in an indexed directory")
	}
	version, levels, err := fs.dxRoot(dir, root)
	if err != nil {
		return descent, err
	}
	hash, ok := dirHash(name, version, &fs.sb.HashSeed)
	if !ok {
		return descent, fail(KindUnsupported, "directory hash version")
	}
	descent.levels, descent.hash, descent.version = levels, hash, version
	block, offset := uint64(0), dxRootEntries
	for level := 0; level <= levels; level++ {
		data, err := fs.dirBlock(dir, block)
		if err != nil {
			return descent, err
		}
		if data == nil {
			return descent, corrupt("hole in an indexed directory")
		}
		if level > 0 && !isIndexNode(data) {
			return descent, corrupt("hash tree node is not an index block")
		}
		node, err := fs.parseDxEntries(dir, data, offset)
		if err != nil {
			return descent, err
		}
		at := 0
		for index := 1; index < node.count && node.hash(index) <= hash; index++ {
			at = index
		}
		descent.frames[level] = dxFrame{block: block, at: at, count: node.count}
		block, offset = uint64(node.block(at)), dxNodeEntries
		if block == 0 || block >= blocks {
			return descent, corrupt("hash tree points outside the directory")
		}
	}
	descent.leaf = block
	return descent, nil
}

// dxEntry is a live record collected while splitting a leaf.
type dxEntry struct {
	inode    uint32
	fileType uint8
	hash     uint32
	name     []byte
}

// addIndexed inserts into the leaf the name's hash selects, splitting it
// when full.
func (fs *FS) addIndexed(dir *Inode, name []byte, inode uint32, fileType uint8) error {
	descent, err := fs.dxDescend(dir, name)
	if err != nil {
		return err
	}
	data, err := fs.dirBlock(dir, descent.leaf)
	if err != nil {
		return err
	}
	if data == nil {
		return corrupt("hole in an indexed directory")
	}
	need := direntSize(len(name))
	offset, used, ok, err := fs.leafSlot(dir, data, need)
	if err != nil {
		return err
	}
	if ok {
		if data, err = fs.stageDirBlock(dir, descent.leaf); err != nil {
			return err
		}
		placeDirent(data, offset, used, inode, name, fileType)
		fs.sealLeaf(dir, data)
		return nil
	}
	parent := descent.frames[descent.levels]
	parentOffset := dxNodeEntries
	if descent.levels == 0 {
		parentOffset = dxRootEntries
	}
	parentData, err := fs.dirBlock(dir, parent.block)
	if err != nil {
		return err
	}
	if int(le16(parentData, parentOffset+2)) >= int(le16(parentData, parentOffset)) {
		return fail(KindNoSpace, "directory index node full")
	}
	entries, err := fs.leafEntries(dir, data, descent.version)
	if err != nil {
		return err
	}
	if len(entries) < 2 {
		return fail(KindNoSpace, "directory leaf cannot be split")
	}
	split := len(entries) / 2
	splitHash := entries[split].hash
	if splitHash == entries[split-1].hash {
		splitHash |= 1
	}
	newLogical, newData, err := fs.growDir(dir)
	if err != nil {
		return err
	}
	oldData, err := fs.stageDirBlock(dir, descent.leaf)
	if err != nil {
		return err
	}
	fs.packLeaf(oldData, entries[:split])
	fs.packLeaf(newData, entries[split:])
	fs.sealLeaf(dir, oldData)
	fs.sealLeaf(dir, newData)
	// The index entry's low bit marks a hash continued from the previous
	// leaf; a name equal to the split hash itself stays on the left.
	target := oldData
	if descent.hash >= splitHash {
		target = newData
	}
	offset, used, ok, err = fs.leafSlot(dir, target, need)
	if err != nil {
		return err
	}
	if !ok {
		return fail(KindNoSpace, "directory leaf full after a split")
	}
	placeDirent(target, offset, used, inode, name, fileType)
	fs.sealLeaf(dir, target)
	node, err := fs.stageDirBlock(dir, parent.block)
	if err != nil {
		return err
	}
	count := int(le16(node, parentOffset+2))
	at := parent.at + 1
	base := parentOffset + 8*at
	copy(node[base+8:parentOffset+8*(count+1)], node[base:parentOffset+8*count])
	putLE32(node[base:], splitHash)
	putLE32(node[base+4:], uint32(newLogical))
	putLE16(node[parentOffset+2:], uint16(count+1))
	fs.sealDx(dir, node, parentOffset)
	return nil
}

// leafEntries returns the live records of a leaf sorted by hash.
func (fs *FS) leafEntries(dir *Inode, data []byte, version uint8) ([]dxEntry, error) {
	end, err := fs.leafEnd(dir, data)
	if err != nil {
		return nil, err
	}
	reader := direntReader{data: data, end: end, fs: fs}
	var entries []dxEntry
	for {
		raw, ok, err := reader.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if raw.inode == 0 {
			continue
		}
		hash, _ := dirHash(raw.name, version, &fs.sb.HashSeed)
		entries = append(entries, dxEntry{inode: raw.inode, fileType: raw.fileType, hash: hash, name: append([]byte(nil), raw.name...)})
	}
	for index := 1; index < len(entries); index++ {
		for at := index; at > 0 && entries[at-1].hash > entries[at].hash; at-- {
			entries[at-1], entries[at] = entries[at], entries[at-1]
		}
	}
	return entries, nil
}

// packLeaf rewrites a leaf with entries back to back; the last record
// spans the rest of the block.
func (fs *FS) packLeaf(data []byte, entries []dxEntry) {
	fs.initLeaf(data)
	limit := fs.leafLimit()
	offset := 0
	for index := range entries {
		entry := &entries[index]
		length := direntSize(len(entry.name))
		if index == len(entries)-1 {
			length = limit - offset
		}
		putDirent(data, offset, entry.inode, length, entry.name, entry.fileType)
		offset += length
	}
}

// findEntry locates name in dir.
func (fs *FS) findEntry(dir *Inode, name []byte) (entryLoc, error) {
	if !dir.IsDir() {
		return entryLoc{}, fail(KindNotDirectory, "")
	}
	if err := checkName(name); err != nil {
		return entryLoc{}, err
	}
	if isDotName(name) {
		return entryLoc{}, fail(KindInvalid, "dot names cannot be changed")
	}
	if fs.isIndexed(dir) {
		return fs.findIndexed(dir, name)
	}
	blocks, err := fs.dirBlocks(dir)
	if err != nil {
		return entryLoc{}, err
	}
	for index := uint64(0); index < blocks; index++ {
		data, err := fs.dirBlock(dir, index)
		if err != nil {
			return entryLoc{}, err
		}
		if data == nil || (isIndexNode(data) && !fs.sb.HasMetadataCsum()) {
			continue
		}
		loc, found, err := fs.scanLeaf(dir, data, name)
		if err != nil {
			return entryLoc{}, err
		}
		if found {
			loc.logical = index
			return loc, nil
		}
	}
	return entryLoc{}, fail(KindNotFound, "")
}

func (fs *FS) findIndexed(dir *Inode, name []byte) (entryLoc, error) {
	descent, err := fs.dxDescend(dir, name)
	if err != nil {
		return entryLoc{}, err
	}
	blocks, err := fs.dirBlocks(dir)
	if err != nil {
		return entryLoc{}, err
	}
	block := descent.leaf
	for visited := uint64(0); visited < blocks; visited++ {
		data, err := fs.dirBlock(dir, block)
		if err != nil {
			return entryLoc{}, err
		}
		if data == nil {
			return entryLoc{}, corrupt("hole in an indexed directory")
		}
		loc, found, err := fs.scanLeaf(dir, data, name)
		if err != nil {
			return entryLoc{}, err
		}
		if found {
			loc.logical = block
			return loc, nil
		}
		next, startHash, more, err := fs.nextLeaf(dir, descent.frames[:descent.levels+1])
		if err != nil {
			return entryLoc{}, err
		}
		if !more || startHash&^1 != descent.hash || startHash&1 == 0 {
			return entryLoc{}, fail(KindNotFound, "")
		}
		if block = next; block == 0 || block >= blocks {
			return entryLoc{}, corrupt("hash tree points outside the directory")
		}
	}
	return entryLoc{}, corrupt("hash tree collision chain does not end")
}

// scanLeaf finds name in one leaf and remembers the record before it.
func (fs *FS) scanLeaf(dir *Inode, data []byte, name []byte) (entryLoc, bool, error) {
	end, err := fs.leafEnd(dir, data)
	if err != nil {
		return entryLoc{}, false, err
	}
	reader := direntReader{data: data, end: end, fs: fs}
	prev := -1
	for {
		raw, ok, err := reader.next()
		if err != nil || !ok {
			return entryLoc{}, false, err
		}
		if raw.inode != 0 && string(raw.name) == string(name) {
			return entryLoc{offset: raw.offset, prev: prev, inode: raw.inode, fileType: raw.fileType}, true, nil
		}
		prev = raw.offset
	}
}

// removeEntry unlinks the record at loc: the previous record absorbs it,
// or, first in its block, it is marked unused.
func (fs *FS) removeEntry(dir *Inode, loc entryLoc) error {
	data, err := fs.stageDirBlock(dir, loc.logical)
	if err != nil {
		return err
	}
	if loc.prev >= 0 {
		putLE16(data[loc.prev+4:], uint16(recLen(data, loc.prev)+recLen(data, loc.offset)))
	} else {
		putLE32(data[loc.offset:], 0)
	}
	fs.sealLeaf(dir, data)
	return nil
}

// retarget points the record at loc to another inode.
func (fs *FS) retarget(dir *Inode, loc entryLoc, inode uint32, fileType uint8) error {
	data, err := fs.stageDirBlock(dir, loc.logical)
	if err != nil {
		return err
	}
	putLE32(data[loc.offset:], inode)
	data[loc.offset+7] = fileType
	fs.sealLeaf(dir, data)
	return nil
}

// dotDot reads the parent a directory's ".." records.
func (fs *FS) dotDot(dir *Inode) (uint32, error) {
	data, err := fs.dirBlock(dir, 0)
	if err != nil {
		return 0, err
	}
	if data == nil || le16(data, 4) != 12 || data[12+6] != 2 || data[20] != '.' || data[21] != '.' {
		return 0, corrupt("directory has no dot-dot entry")
	}
	return le32(data, 12), nil
}

// setDotDot points a directory's ".." at parent.
func (fs *FS) setDotDot(dir *Inode, parent uint32) error {
	if _, err := fs.dotDot(dir); err != nil {
		return err
	}
	data, err := fs.stageDirBlock(dir, 0)
	if err != nil {
		return err
	}
	putLE32(data[12:], parent)
	if fs.isIndexed(dir) {
		fs.sealDx(dir, data, dxRootEntries)
	} else {
		fs.sealLeaf(dir, data)
	}
	return nil
}

// dirEmpty reports whether dir holds nothing but "." and "..".
func (fs *FS) dirEmpty(dir *Inode) (bool, error) {
	empty := true
	_, err := fs.ReadDir(dir, 0, func(*DirEntry) bool {
		empty = false
		return false
	})
	return empty, err
}

// formatDir writes the first block of a new directory.
func (fs *FS) formatDir(dir *Inode, data []byte, parent uint32) {
	fs.initLeaf(data)
	putDirent(data, 0, dir.Number, 12, []byte("."), FileTypeDir)
	putDirent(data, 12, parent, fs.leafLimit()-12, []byte(".."), FileTypeDir)
	fs.sealLeaf(dir, data)
}
