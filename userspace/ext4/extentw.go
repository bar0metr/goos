package ext4

// Extent tree mutation. An edit walks from the root in the inode to the
// leaf covering a logical block, staging each node on the way, rewrites
// that leaf, and splits nodes upward when one overflows; a full root moves
// its records into a new child and the tree grows one level. Index keys
// always equal the first logical block their child covers, which e2fsck
// checks. Working state is one path of at most six nodes.

// extent is one decoded leaf record.
type extent struct {
	logical   uint32
	length    uint32
	physical  uint64
	unwritten bool
}

func (e extent) end() uint64 { return uint64(e.logical) + uint64(e.length) }

func decodeExtent(record []byte) extent {
	length := uint32(le16(record, 4))
	e := extent{logical: le32(record, 0), physical: join64(le32(record, 8), uint32(le16(record, 6)))}
	if length > extentInitMax {
		e.length, e.unwritten = length-extentInitMax, true
	} else {
		e.length = length
	}
	return e
}

func encodeExtent(record []byte, e extent) {
	length := e.length
	if e.unwritten {
		length += extentInitMax
	}
	putLE32(record[0:], e.logical)
	putLE16(record[4:], uint16(length))
	putLE16(record[6:], uint16(e.physical>>32))
	putLE32(record[8:], uint32(e.physical))
}

func encodeIndex(record []byte, logical uint32, child uint64) {
	putLE32(record[0:], logical)
	putLE32(record[4:], uint32(child))
	putLE16(record[8:], uint16(child>>32))
	putLE16(record[10:], 0)
}

// maxExtentLength is the longest run one record maps.
func maxExtentLength(unwritten bool) uint32 {
	if unwritten {
		return extentInitMax - 1
	}
	return extentInitMax
}

// record is one 12-byte extent or index entry.
type record [12]byte

func recordKey(entry *record) uint32 { return le32(entry[:], 0) }

type extNode struct {
	// block is the node's block, zero for the root in the inode.
	block uint64
	data  []byte
	// at is the entry an index node's descent took.
	at int
}

type extPath struct {
	nodes [extentMaxDepth + 1]extNode
	// depth is the root's depth: nodes[depth] is the leaf.
	depth int
}

func (fs *FS) nodeCapacity(level int) int {
	if level == 0 {
		return (inodeBlockSize - 12) / 12
	}
	return (int(fs.blockSize) - 12) / 12
}

// extentPath stages the nodes from the root to the leaf covering logical.
// When logical precedes every key, the descent takes the first entry.
func (fs *FS) extentPath(inode *Inode, logical uint32) (*extPath, error) {
	root := inode.blockArea[:]
	header, err := parseExtentNode(root, -1)
	if err != nil {
		return nil, err
	}
	path := &extPath{depth: int(header.depth)}
	path.nodes[0] = extNode{data: root}
	node := root
	for level := 0; level < path.depth; level++ {
		count := int(le16(node, 2))
		if count == 0 {
			return nil, corrupt("empty extent index node")
		}
		at := 0
		for index := 1; index < count && le32(node, 12+12*index) <= logical; index++ {
			at = index
		}
		path.nodes[level].at = at
		entry := node[12+12*at:]
		child := join64(le32(entry, 4), uint32(le16(entry, 8)))
		if child <= uint64(fs.sb.FirstDataBlock) || child >= fs.sb.BlocksCount {
			return nil, corrupt("extent index points outside the volume")
		}
		data, err := fs.stage(child)
		if err != nil {
			return nil, err
		}
		next, err := parseExtentNode(data, path.depth-level-1)
		if err != nil {
			return nil, err
		}
		if fs.sb.HasMetadataCsum() {
			tail := 12 + 12*int(next.max)
			if tail+4 > len(data) || crc32c(inode.csumSeed, data[:tail]) != le32(data, tail) {
				return nil, corrupt("extent block checksum mismatch")
			}
		}
		path.nodes[level+1] = extNode{block: child, data: data}
		node = data
	}
	return path, nil
}

func nodeRecords(data []byte) []record {
	count := int(le16(data, 2))
	records := make([]record, count, count+3)
	for index := range records {
		copy(records[index][:], data[12+12*index:])
	}
	return records
}

// writeNode encodes records into a node at depth with room for capacity
// entries and seals a block node's checksum.
func (fs *FS) writeNode(inode *Inode, data []byte, block uint64, depth int, capacity int, records []record) {
	putLE16(data[0:], extentMagic)
	putLE16(data[2:], uint16(len(records)))
	putLE16(data[4:], uint16(capacity))
	putLE16(data[6:], uint16(depth))
	for index := 0; index < capacity; index++ {
		entry := data[12+12*index : 24+12*index]
		if index < len(records) {
			copy(entry, records[index][:])
		} else {
			for at := range entry {
				entry[at] = 0
			}
		}
	}
	if block == 0 {
		inode.dirty = true
		return
	}
	if fs.sb.HasMetadataCsum() {
		tail := 12 + 12*capacity
		putLE32(data[tail:], crc32c(inode.csumSeed, data[:tail]))
	}
}

// allocNode allocates a tree node block near goal and charges it to inode.
func (fs *FS) allocNode(inode *Inode, goal uint64) (uint64, []byte, error) {
	block, _, err := fs.allocBlocks(goal, 1)
	if err != nil {
		return 0, nil, err
	}
	data, err := fs.stageZero(block, true)
	if err != nil {
		return 0, nil, err
	}
	inode.Blocks512 += uint64(fs.blockSize / 512)
	inode.dirty = true
	return block, data, nil
}

// storeNode writes records into the node at level, splitting it when they
// do not fit. tail says the edit appended past the node's last record, so
// a split leaves the earlier records together instead of halving them.
func (fs *FS) storeNode(inode *Inode, path *extPath, level int, records []record, tail bool) error {
	node := &path.nodes[level]
	capacity := fs.nodeCapacity(level)
	depth := path.depth - level
	if len(records) <= capacity {
		fs.writeNode(inode, node.data, node.block, depth, capacity, records)
		if len(records) > 0 {
			return fs.fixKey(inode, path, level, recordKey(&records[0]))
		}
		return nil
	}
	goal := fs.nodeGoal(inode, path)
	if level == 0 {
		if path.depth >= extentMaxDepth {
			return fail(KindNoSpace, "extent tree at its maximum depth")
		}
		block, data, err := fs.allocNode(inode, goal)
		if err != nil {
			return err
		}
		fs.writeNode(inode, data, block, depth, fs.nodeCapacity(1), records)
		var root [1]record
		encodeIndex(root[0][:], recordKey(&records[0]), block)
		fs.writeNode(inode, node.data, 0, depth+1, capacity, root[:])
		return nil
	}
	split := len(records) / 2
	if tail {
		split = len(records) - 1
	}
	left, right := records[:split], records[split:]
	block, data, err := fs.allocNode(inode, goal)
	if err != nil {
		return err
	}
	fs.writeNode(inode, data, block, depth, capacity, right)
	fs.writeNode(inode, node.data, node.block, depth, capacity, left)
	parent := &path.nodes[level-1]
	parentRecords := nodeRecords(parent.data)
	at := parent.at
	putLE32(parentRecords[at][:], recordKey(&left[0]))
	parentRecords = append(parentRecords, record{})
	copy(parentRecords[at+2:], parentRecords[at+1:])
	encodeIndex(parentRecords[at+1][:], recordKey(&right[0]), block)
	return fs.storeNode(inode, path, level-1, parentRecords, tail && at+1 == len(parentRecords)-1)
}

// fixKey makes the index entry above the node at level carry key, and
// repeats upward while the entry is its node's first.
func (fs *FS) fixKey(inode *Inode, path *extPath, level int, key uint32) error {
	for ; level > 0; level-- {
		parent := &path.nodes[level-1]
		offset := 12 + 12*parent.at
		if le32(parent.data, offset) == key {
			return nil
		}
		putLE32(parent.data[offset:], key)
		capacity := fs.nodeCapacity(level - 1)
		fs.writeNode(inode, parent.data, parent.block, path.depth-level+1, capacity, nodeRecords(parent.data))
		if parent.at != 0 {
			return nil
		}
	}
	return nil
}

// nodeGoal places new tree nodes next to the leaf they split from, or next
// to the inode's table when the tree has no blocks.
func (fs *FS) nodeGoal(inode *Inode, path *extPath) uint64 {
	if block := path.nodes[path.depth].block; block != 0 {
		return block + 1
	}
	return fs.inodeGoal(inode)
}

// inodeGoal is the first block of the inode's group.
func (fs *FS) inodeGoal(inode *Inode) uint64 {
	group := (inode.Number - 1) / fs.sb.InodesPerGroup
	return fs.groupStart(group)
}

// insertExtent maps e into the tree. The range must be a hole, or, when e
// is written, lie inside one unwritten extent whose blocks it keeps: that
// extent is split around e. Neighbours that continue each other logically
// and physically are merged.
func (fs *FS) insertExtent(inode *Inode, e extent) error {
	if e.length == 0 || e.length > maxExtentLength(e.unwritten) || e.end() > uint64(maxLogicalBlock)+1 {
		return fail(KindInternal, "extent length out of range")
	}
	path, err := fs.extentPath(inode, e.logical)
	if err != nil {
		return err
	}
	leaf := path.depth
	records := nodeRecords(path.nodes[leaf].data)
	merged := make([]extent, 0, len(records)+3)
	placed := false
	for index := range records {
		current := decodeExtent(records[index][:])
		switch {
		case current.end() <= uint64(e.logical):
			merged = append(merged, current)
		case uint64(current.logical) >= e.end():
			if !placed {
				merged, placed = append(merged, e), true
			}
			merged = append(merged, current)
		default:
			inside := current.logical <= e.logical && e.end() <= current.end()
			if !inside || !current.unwritten || e.unwritten ||
				current.physical+uint64(e.logical-current.logical) != e.physical {
				return corrupt("new extent overlaps a mapped range")
			}
			if current.logical < e.logical {
				merged = append(merged, extent{logical: current.logical, length: e.logical - current.logical, physical: current.physical, unwritten: true})
			}
			merged, placed = append(merged, e), true
			if rest := current.end() - e.end(); rest > 0 {
				merged = append(merged, extent{logical: uint32(e.end()), length: uint32(rest), physical: e.physical + uint64(e.length), unwritten: true})
			}
		}
	}
	if !placed {
		merged = append(merged, e)
	}
	merged = mergeExtents(merged)
	tail := len(merged) > 0 && merged[len(merged)-1].end() == e.end()
	records = records[:0]
	for index := range merged {
		var entry record
		encodeExtent(entry[:], merged[index])
		records = append(records, entry)
	}
	return fs.storeNode(inode, path, leaf, records, tail)
}

// mergeExtents joins neighbours that continue each other.
func mergeExtents(extents []extent) []extent {
	out := extents[:0]
	for index := range extents {
		current := extents[index]
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.unwritten == current.unwritten && last.end() == uint64(current.logical) &&
				last.physical+uint64(last.length) == current.physical &&
				last.length+current.length <= maxExtentLength(current.unwritten) {
				last.length += current.length
				continue
			}
		}
		out = append(out, current)
	}
	return out
}

// trimStep frees the mappings at or beyond logical block end in the last
// extent of the tree, removing nodes that empty and pulling a lone small
// child back into the root. It reports true once nothing lies at or beyond
// end. Each call touches one extent, so its credits are bounded.
func (fs *FS) trimStep(inode *Inode, end uint32) (bool, error) {
	path, err := fs.extentPath(inode, maxLogicalBlock)
	if err != nil {
		return false, err
	}
	leaf := path.depth
	records := nodeRecords(path.nodes[leaf].data)
	if len(records) == 0 {
		if leaf == 0 {
			return true, nil
		}
		return false, fs.removeNode(inode, path, leaf)
	}
	last := decodeExtent(records[len(records)-1][:])
	if last.end() <= uint64(end) {
		return true, fs.collapseRoot(inode)
	}
	var freeStart uint64
	var freeCount uint32
	if last.logical >= end {
		freeStart, freeCount = last.physical, last.length
		records = records[:len(records)-1]
	} else {
		keep := end - last.logical
		freeStart, freeCount = last.physical+uint64(keep), last.length-keep
		last.length = keep
		encodeExtent(records[len(records)-1][:], last)
	}
	if err := fs.freeBlocks(freeStart, uint64(freeCount)); err != nil {
		return false, err
	}
	inode.Blocks512 -= uint64(freeCount) * uint64(fs.blockSize/512)
	inode.dirty = true
	if len(records) == 0 && leaf > 0 {
		return false, fs.removeNode(inode, path, leaf)
	}
	return false, fs.storeNode(inode, path, leaf, records, false)
}

// removeNode frees an empty node and drops its entry from the parent,
// repeating upward while parents empty. An emptied root becomes an empty
// leaf.
func (fs *FS) removeNode(inode *Inode, path *extPath, level int) error {
	for ; level > 0; level-- {
		if err := fs.freeBlocks(path.nodes[level].block, 1); err != nil {
			return err
		}
		inode.Blocks512 -= uint64(fs.blockSize / 512)
		inode.dirty = true
		parent := &path.nodes[level-1]
		records := nodeRecords(parent.data)
		records = append(records[:parent.at], records[parent.at+1:]...)
		if len(records) > 0 {
			return fs.storeNode(inode, path, level-1, records, false)
		}
		if level-1 == 0 {
			path.depth = 0
			fs.writeNode(inode, parent.data, 0, 0, fs.nodeCapacity(0), nil)
			return nil
		}
	}
	return nil
}

// collapseRoot pulls the root's only child into the root while its records
// fit there, so a truncated tree is no deeper than it needs to be.
func (fs *FS) collapseRoot(inode *Inode) error {
	root := inode.blockArea[:]
	for {
		header, err := parseExtentNode(root, -1)
		if err != nil {
			return err
		}
		if header.depth == 0 || header.entries != 1 {
			return nil
		}
		path, err := fs.extentPath(inode, 0)
		if err != nil {
			return err
		}
		child := path.nodes[1]
		records := nodeRecords(child.data)
		if len(records) > fs.nodeCapacity(0) {
			return nil
		}
		if err := fs.freeBlocks(child.block, 1); err != nil {
			return err
		}
		inode.Blocks512 -= uint64(fs.blockSize / 512)
		fs.writeNode(inode, root, 0, int(header.depth)-1, fs.nodeCapacity(0), records)
	}
}
