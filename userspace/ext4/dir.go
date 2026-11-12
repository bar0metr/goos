package ext4

const (
	// MaxNameLength is the longest ext4 name component in bytes.
	MaxNameLength = 255

	dirTailSize     = 12
	dirTailFileType = 0xDE
	dxTailSize      = 8
	dxRootInfo      = 0x18
	dxRootEntries   = 0x20
	dxNodeEntries   = 0x8

	FileTypeUnknown = 0
	FileTypeRegular = 1
	FileTypeDir     = 2
	FileTypeChar    = 3
	FileTypeBlock   = 4
	FileTypeFIFO    = 5
	FileTypeSocket  = 6
	FileTypeSymlink = 7
)

// DirEntry is one name in a directory. The name is copied out of the block
// it was read from.
type DirEntry struct {
	Inode   uint32
	Type    uint8
	nameLen uint8
	name    [MaxNameLength]byte
	// Offset is the byte position of the entry within the directory.
	Offset uint64
}

// Name is the exact byte spelling of the entry.
func (entry *DirEntry) Name() []byte { return entry.name[:entry.nameLen] }

// direntReader walks the entries of one directory block.
type direntReader struct {
	data   []byte
	end    int
	offset int
	fs     *FS
}

type rawDirent struct {
	inode    uint32
	recLen   int
	nameLen  int
	fileType uint8
	name     []byte
	offset   int
}

// recLen decodes a record length: values of 65536 and above wrap into the
// low bits on 64 KiB blocks, which this implementation refuses at mount.
func recLen(data []byte, offset int) int { return int(le16(data, offset+4)) }

// next returns the following record, or false at the end of the block.
func (reader *direntReader) next() (rawDirent, bool, error) {
	if reader.offset >= reader.end {
		return rawDirent{}, false, nil
	}
	if reader.end-reader.offset < 12 {
		return rawDirent{}, false, corrupt("directory entry header truncated")
	}
	offset := reader.offset
	entry := rawDirent{
		inode:    le32(reader.data, offset),
		recLen:   recLen(reader.data, offset),
		nameLen:  int(reader.data[offset+6]),
		fileType: reader.data[offset+7],
		offset:   offset,
	}
	if reader.fs.sb.Features.Incompat&incompatFiletype == 0 {
		entry.nameLen |= int(entry.fileType) << 8
		entry.fileType = FileTypeUnknown
	}
	minimum := (8 + entry.nameLen + 3) &^ 3
	if entry.recLen < 12 || entry.recLen%4 != 0 || entry.recLen < minimum || offset+entry.recLen > reader.end {
		return rawDirent{}, false, corrupt("directory entry record length invalid")
	}
	if entry.inode > reader.fs.sb.InodesCount {
		return rawDirent{}, false, corrupt("directory entry inode out of range")
	}
	if entry.inode != 0 {
		if entry.nameLen == 0 || entry.nameLen > MaxNameLength {
			return rawDirent{}, false, corrupt("directory entry name length invalid")
		}
		entry.name = reader.data[offset+8 : offset+8+entry.nameLen]
		for _, value := range entry.name {
			if value == 0 || value == '/' {
				return rawDirent{}, false, corrupt("directory entry name holds NUL or slash")
			}
		}
	}
	reader.offset += entry.recLen
	return entry, true, nil
}

func (fs *FS) isIndexed(dir *Inode) bool {
	return dir.Flags&inodeFlagIndex != 0 && fs.sb.Features.Compat&compatDirIndex != 0
}

// dirBlock reads block index of dir. A hole is reported with a nil slice.
func (fs *FS) dirBlock(dir *Inode, index uint64) ([]byte, error) {
	mapping, err := fs.mapBlock(dir, index)
	if err != nil {
		return nil, err
	}
	if mapping.Hole {
		return nil, nil
	}
	if mapping.Unwritten {
		return nil, corrupt("unwritten extent in a directory")
	}
	return fs.cache.get(mapping.Physical)
}

// leafEnd verifies the checksum tail of a leaf block and returns where its
// entries end.
func (fs *FS) leafEnd(dir *Inode, data []byte) (int, error) {
	if !fs.sb.HasMetadataCsum() {
		return len(data), nil
	}
	tail := len(data) - dirTailSize
	if le32(data, tail) != 0 || recLen(data, tail) != dirTailSize || data[tail+6] != 0 || data[tail+7] != dirTailFileType {
		return 0, corrupt("directory block has no checksum tail")
	}
	entry := fs.cache.resident(data)
	if entry != nil && entry.checked && entry.seed == dir.csumSeed {
		return tail, nil
	}
	if crc32c(dir.csumSeed, data[:tail]) != le32(data, tail+8) {
		return 0, corrupt("directory block checksum mismatch")
	}
	if entry != nil {
		entry.checked, entry.seed = true, dir.csumSeed
	}
	return tail, nil
}

// isIndexNode reports whether a block of an indexed directory is an
// internal hash-tree node: one empty record spanning the block.
func isIndexNode(data []byte) bool {
	return le32(data, 0) == 0 && recLen(data, 0) == len(data) && data[6] == 0
}

// dxNode describes the entry array of a hash-tree root or internal node.
type dxNode struct {
	data   []byte
	offset int
	count  int
}

func (node dxNode) hash(index int) uint32 {
	if index == 0 {
		return 0
	}
	return le32(node.data, node.offset+8*index)
}

func (node dxNode) block(index int) uint32 { return le32(node.data, node.offset+8*index+4) }

// parseDxEntries checks the limit/count header at offset and the dx tail.
func (fs *FS) parseDxEntries(dir *Inode, data []byte, offset int) (dxNode, error) {
	space := len(data) - offset
	if fs.sb.HasMetadataCsum() {
		space -= dxTailSize
	}
	limit, count := int(le16(data, offset)), int(le16(data, offset+2))
	if limit != space/8 || count == 0 || count > limit {
		return dxNode{}, corrupt("hash tree node count or limit invalid")
	}
	if fs.sb.HasMetadataCsum() {
		tail := offset + 8*limit
		crc := crc32c(dir.csumSeed, data[:offset+8*count])
		crc = crc32c(crc, data[tail:tail+4])
		crc = crc32cZeros(crc, 4)
		if crc != le32(data, tail+4) {
			return dxNode{}, corrupt("hash tree node checksum mismatch")
		}
	}
	node := dxNode{data: data, offset: offset, count: count}
	for index := 2; index < count; index++ {
		if node.hash(index) < node.hash(index-1) {
			return dxNode{}, corrupt("hash tree entries out of order")
		}
	}
	return node, nil
}

// dxRoot validates block 0 of an indexed directory and returns its hash
// version and depth.
func (fs *FS) dxRoot(dir *Inode, data []byte) (uint8, int, error) {
	if le16(data, 4) != 12 || data[6] != 1 || data[8] != '.' ||
		le16(data, 12+4) != uint16(len(data)-12) || data[12+6] != 2 || data[20] != '.' || data[21] != '.' {
		return 0, 0, corrupt("hash tree root has no dot entries")
	}
	if le32(data, dxRootInfo) != 0 || data[dxRootInfo+5] != 8 {
		return 0, 0, corrupt("hash tree root info invalid")
	}
	version := data[dxRootInfo+4]
	levels := int(data[dxRootInfo+6])
	maxLevels := 2
	if fs.sb.Features.Incompat&incompatLargeDir != 0 {
		maxLevels = 3
	}
	if levels >= maxLevels {
		return 0, 0, corrupt("hash tree too deep")
	}
	if version <= hashTEA && fs.sb.Flags&flagUnsignedHash != 0 {
		version += 3
	}
	return version, levels, nil
}

// dirBlocks is the block count of a directory, which must be whole blocks.
func (fs *FS) dirBlocks(dir *Inode) (uint64, error) {
	if dir.Size%uint64(fs.blockSize) != 0 {
		return 0, corrupt("directory size not a multiple of the block size")
	}
	return dir.Size / uint64(fs.blockSize), nil
}

// ReadDir emits the entries of dir starting at byte position cursor, skipping
// "." and "..", until emit returns false or the directory ends. It returns
// the cursor to continue from, which is dir.Size at the end.
func (fs *FS) ReadDir(dir *Inode, cursor uint64, emit func(*DirEntry) bool) (uint64, error) {
	if !dir.IsDir() {
		return 0, fail(KindNotDirectory, "")
	}
	blocks, err := fs.dirBlocks(dir)
	if err != nil {
		return 0, err
	}
	indexed := fs.isIndexed(dir)
	blockSize := uint64(fs.blockSize)
	var entry DirEntry
	for index := cursor / blockSize; index < blocks; index++ {
		data, err := fs.dirBlock(dir, index)
		if err != nil {
			return 0, err
		}
		if data == nil {
			if indexed {
				return 0, corrupt("hole in an indexed directory")
			}
			continue
		}
		if indexed && index == 0 {
			if _, _, err := fs.dxRoot(dir, data); err != nil {
				return 0, err
			}
			if _, err := fs.parseDxEntries(dir, data, dxRootEntries); err != nil {
				return 0, err
			}
			continue
		}
		if isIndexNode(data) && (indexed || !fs.sb.HasMetadataCsum()) {
			if indexed {
				if _, err := fs.parseDxEntries(dir, data, dxNodeEntries); err != nil {
					return 0, err
				}
			}
			continue
		}
		end, err := fs.leafEnd(dir, data)
		if err != nil {
			return 0, err
		}
		reader := direntReader{data: data, end: end, fs: fs}
		base := index * blockSize
		for {
			raw, ok, err := reader.next()
			if err != nil {
				return 0, err
			}
			if !ok {
				break
			}
			position := base + uint64(raw.offset)
			if position < cursor || raw.inode == 0 || isDotName(raw.name) {
				continue
			}
			entry.Inode, entry.Type, entry.Offset = raw.inode, raw.fileType, position
			entry.nameLen = uint8(copy(entry.name[:], raw.name))
			if !emit(&entry) {
				return position, nil
			}
		}
	}
	return dir.Size, nil
}

func isDotName(name []byte) bool {
	return (len(name) == 1 && name[0] == '.') || (len(name) == 2 && name[0] == '.' && name[1] == '.')
}

func checkName(name []byte) error {
	if len(name) == 0 {
		return fail(KindInvalid, "empty name")
	}
	if len(name) > MaxNameLength {
		return fail(KindNameTooLong, "")
	}
	for _, value := range name {
		if value == 0 || value == '/' {
			return fail(KindInvalid, "name holds NUL or slash")
		}
	}
	return nil
}

// searchLeaf scans one leaf block for name.
func (fs *FS) searchLeaf(dir *Inode, data []byte, name []byte) (uint32, uint8, bool, error) {
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
		if raw.inode != 0 && string(raw.name) == string(name) {
			return raw.inode, raw.fileType, true, nil
		}
	}
}

// Lookup finds name in dir and returns its inode number and the file type
// the entry records. "." and ".." are resolved from the directory itself.
func (fs *FS) Lookup(dir *Inode, name []byte) (uint32, uint8, error) {
	if !dir.IsDir() {
		return 0, 0, fail(KindNotDirectory, "")
	}
	if err := checkName(name); err != nil {
		return 0, 0, err
	}
	if fs.isIndexed(dir) {
		inode, fileType, found, err := fs.lookupIndexed(dir, name)
		if err != nil || found {
			return inode, fileType, err
		}
		return 0, 0, fail(KindNotFound, "")
	}
	blocks, err := fs.dirBlocks(dir)
	if err != nil {
		return 0, 0, err
	}
	for index := uint64(0); index < blocks; index++ {
		data, err := fs.dirBlock(dir, index)
		if err != nil {
			return 0, 0, err
		}
		if data == nil || (isIndexNode(data) && !fs.sb.HasMetadataCsum()) {
			continue
		}
		inode, fileType, found, err := fs.searchLeaf(dir, data, name)
		if err != nil || found {
			return inode, fileType, err
		}
	}
	return 0, 0, fail(KindNotFound, "")
}

// dxFrame is one level of a hash-tree descent: the node and the entry taken.
type dxFrame struct {
	block uint64
	at    int
	count int
}

// lookupIndexed descends the hash tree to the leaf that holds name's hash,
// then follows the collision chain: a following leaf whose starting hash
// equals the name's hash with the low bit set continues the same hash.
func (fs *FS) lookupIndexed(dir *Inode, name []byte) (uint32, uint8, bool, error) {
	blocks, err := fs.dirBlocks(dir)
	if err != nil {
		return 0, 0, false, err
	}
	root, err := fs.dirBlock(dir, 0)
	if err != nil {
		return 0, 0, false, err
	}
	if root == nil {
		return 0, 0, false, corrupt("hole in an indexed directory")
	}
	version, levels, err := fs.dxRoot(dir, root)
	if err != nil {
		return 0, 0, false, err
	}
	if isDotName(name) {
		return 0, 0, false, fail(KindInvalid, "dot names are resolved by the caller")
	}
	hash, ok := dirHash(name, version, &fs.sb.HashSeed)
	if !ok {
		return 0, 0, false, fail(KindUnsupported, "directory hash version")
	}
	var frames [3]dxFrame
	block, offset := uint64(0), dxRootEntries
	for level := 0; level <= levels; level++ {
		data, err := fs.dirBlock(dir, block)
		if err != nil {
			return 0, 0, false, err
		}
		if data == nil {
			return 0, 0, false, corrupt("hole in an indexed directory")
		}
		if level > 0 && !isIndexNode(data) {
			return 0, 0, false, corrupt("hash tree node is not an index block")
		}
		node, err := fs.parseDxEntries(dir, data, offset)
		if err != nil {
			return 0, 0, false, err
		}
		at := 0
		for index := 1; index < node.count && node.hash(index) <= hash; index++ {
			at = index
		}
		frames[level] = dxFrame{block: block, at: at, count: node.count}
		block, offset = uint64(node.block(at)), dxNodeEntries
		if block == 0 || block >= blocks {
			return 0, 0, false, corrupt("hash tree points outside the directory")
		}
	}
	// Each leaf visit either finds the name or advances the deepest frame,
	// so the loop ends after at most the directory's block count.
	for visited := uint64(0); visited < blocks; visited++ {
		data, err := fs.dirBlock(dir, block)
		if err != nil {
			return 0, 0, false, err
		}
		if data == nil {
			return 0, 0, false, corrupt("hole in an indexed directory")
		}
		inode, fileType, found, err := fs.searchLeaf(dir, data, name)
		if err != nil || found {
			return inode, fileType, found, err
		}
		next, startHash, more, err := fs.nextLeaf(dir, frames[:levels+1])
		if err != nil || !more || startHash&^1 != hash || startHash&1 == 0 {
			return 0, 0, false, err
		}
		block = next
		if block == 0 || block >= blocks {
			return 0, 0, false, corrupt("hash tree points outside the directory")
		}
	}
	return 0, 0, false, corrupt("hash tree collision chain does not end")
}

// nextLeaf advances the descent to the following leaf and returns it with
// the hash its index entry starts at.
func (fs *FS) nextLeaf(dir *Inode, frames []dxFrame) (uint64, uint32, bool, error) {
	level := len(frames) - 1
	for level >= 0 && frames[level].at+1 >= frames[level].count {
		level--
	}
	if level < 0 {
		return 0, 0, false, nil
	}
	frames[level].at++
	var startHash uint32
	for advanced := level; level < len(frames); level++ {
		data, err := fs.dirBlock(dir, frames[level].block)
		if err != nil {
			return 0, 0, false, err
		}
		if data == nil {
			return 0, 0, false, corrupt("hole in an indexed directory")
		}
		offset := dxNodeEntries
		if level == 0 {
			offset = dxRootEntries
		}
		node, err := fs.parseDxEntries(dir, data, offset)
		if err != nil {
			return 0, 0, false, err
		}
		if frames[level].at >= node.count {
			return 0, 0, false, corrupt("hash tree node changed under the descent")
		}
		if level == advanced {
			startHash = node.hash(frames[level].at)
		}
		child := uint64(node.block(frames[level].at))
		if level+1 < len(frames) {
			frames[level+1] = dxFrame{block: child, at: 0}
			childData, err := fs.dirBlock(dir, child)
			if err != nil {
				return 0, 0, false, err
			}
			if childData == nil || !isIndexNode(childData) {
				return 0, 0, false, corrupt("hash tree node is not an index block")
			}
			frames[level+1].count = int(le16(childData, dxNodeEntries+2))
			continue
		}
		return child, startHash, true, nil
	}
	return 0, 0, false, nil
}
