package ext4

const (
	extentMagic    = 0xF30A
	extentMaxDepth = 5
	extentInitMax  = 32768
	// maxLogicalBlock is the largest logical block an extent can name.
	maxLogicalBlock = 0xFFFFFFFF
)

// Mapping describes a run of logical blocks: where it lives on disk, or that
// it is a hole. Unwritten runs read as zeros like holes do.
type Mapping struct {
	Logical   uint64
	Physical  uint64
	Length    uint64
	Hole      bool
	Unwritten bool
}

// mapBlock maps logical block of inode to the run containing it.
func (fs *FS) mapBlock(inode *Inode, logical uint64) (Mapping, error) {
	if logical > maxLogicalBlock {
		return Mapping{Logical: logical, Length: 1, Hole: true}, nil
	}
	if inode.Flags&inodeFlagExtents != 0 {
		return fs.mapExtent(inode, uint32(logical))
	}
	return fs.mapIndirect(inode, uint32(logical))
}

type extentHeader struct {
	entries uint16
	max     uint16
	depth   uint16
}

// parseExtentNode checks a node header against the space the node occupies
// and the depth its parent expects.
func parseExtentNode(node []byte, wantDepth int) (extentHeader, error) {
	if len(node) < 12 || le16(node, 0) != extentMagic {
		return extentHeader{}, corrupt("bad extent header magic")
	}
	header := extentHeader{entries: le16(node, 2), max: le16(node, 4), depth: le16(node, 6)}
	if header.max == 0 || 12+12*int(header.max) > len(node) || header.entries > header.max {
		return extentHeader{}, corrupt("extent node entry count does not fit")
	}
	if wantDepth >= 0 && int(header.depth) != wantDepth {
		return extentHeader{}, corrupt("extent tree depth inconsistent")
	}
	if header.depth > extentMaxDepth {
		return extentHeader{}, corrupt("extent tree too deep")
	}
	return header, nil
}

// mapExtent walks the extent tree from the inode root to the leaf covering
// logical. Depth strictly decreases at each step and is at most five, so
// the walk reads at most five blocks and cannot cycle. limit tracks the
// first logical block the next sibling at any level covers, which bounds
// the length of a hole.
func (fs *FS) mapExtent(inode *Inode, logical uint32) (Mapping, error) {
	node := inode.blockArea[:]
	header, err := parseExtentNode(node, -1)
	if err != nil {
		return Mapping{}, err
	}
	limit := uint64(maxLogicalBlock) + 1
	lower := uint64(0)
	for header.depth > 0 {
		chosen := -1
		var previous uint32
		for index := 0; index < int(header.entries); index++ {
			start := le32(node, 12+12*index)
			if index > 0 && start <= previous {
				return Mapping{}, corrupt("extent index entries out of order")
			}
			if uint64(start) < lower {
				return Mapping{}, corrupt("extent index entry below its parent range")
			}
			previous = start
			if start <= logical {
				chosen = index
			} else if uint64(start) < limit {
				limit = uint64(start)
				break
			}
		}
		if chosen < 0 {
			return Mapping{Logical: uint64(logical), Length: holeLength(uint64(logical), limit), Hole: true}, nil
		}
		entry := node[12+12*chosen:]
		lower = uint64(le32(entry, 0))
		child := join64(le32(entry, 4), uint32(le16(entry, 8)))
		if child < uint64(fs.sb.FirstDataBlock)+1 || child >= fs.sb.BlocksCount {
			return Mapping{}, corrupt("extent index points outside the volume")
		}
		data, err := fs.cache.get(child)
		if err != nil {
			return Mapping{}, err
		}
		next, err := parseExtentNode(data, int(header.depth)-1)
		if err != nil {
			return Mapping{}, err
		}
		if fs.sb.HasMetadataCsum() {
			tail := 12 + 12*int(next.max)
			if tail+4 > len(data) {
				return Mapping{}, corrupt("extent block has no room for its checksum")
			}
			if crc32c(inode.csumSeed, data[:tail]) != le32(data, tail) {
				return Mapping{}, corrupt("extent block checksum mismatch")
			}
		}
		node, header = data, next
	}
	var end uint64
	for index := 0; index < int(header.entries); index++ {
		entry := node[12+12*index:]
		start := uint64(le32(entry, 0))
		length := uint64(le16(entry, 4))
		unwritten := false
		if length > extentInitMax {
			length -= extentInitMax
			unwritten = true
		}
		if length == 0 {
			return Mapping{}, corrupt("zero-length extent")
		}
		if start < end || start < lower {
			return Mapping{}, corrupt("extents overlap or are out of order")
		}
		end = start + length
		if end > uint64(maxLogicalBlock)+1 {
			return Mapping{}, corrupt("extent beyond the largest logical block")
		}
		physical := join64(le32(entry, 8), uint32(le16(entry, 6)))
		if last, ok := addOK(physical, length); !ok || physical <= uint64(fs.sb.FirstDataBlock) || last > fs.sb.BlocksCount {
			return Mapping{}, corrupt("extent points outside the volume")
		}
		if uint64(logical) < start {
			if start < limit {
				limit = start
			}
			break
		}
		if uint64(logical) < end {
			offset := uint64(logical) - start
			return Mapping{
				Logical:   uint64(logical),
				Physical:  physical + offset,
				Length:    length - offset,
				Unwritten: unwritten,
			}, nil
		}
	}
	return Mapping{Logical: uint64(logical), Length: holeLength(uint64(logical), limit), Hole: true}, nil
}

func holeLength(logical uint64, limit uint64) uint64 {
	if limit <= logical {
		return 1
	}
	return limit - logical
}

// mapIndirect resolves a block-mapped (ext2/ext3 style) file for reading:
// twelve direct pointers, then single, double and triple indirect blocks.
// It returns one block at a time; read paths merge physically adjacent
// blocks themselves.
func (fs *FS) mapIndirect(inode *Inode, logical uint32) (Mapping, error) {
	perBlock := uint64(fs.blockSize / 4)
	index := uint64(logical)
	pointer := func(area []byte, slot uint64) uint64 { return uint64(le32(area, int(slot*4))) }
	var block uint64
	var path [3]uint64
	depth := 0
	switch {
	case index < 12:
		block = pointer(inode.blockArea[:], index)
	case index-12 < perBlock:
		block, path[0], depth = pointer(inode.blockArea[:], 12), index-12, 1
	case index-12-perBlock < perBlock*perBlock:
		rest := index - 12 - perBlock
		block, path[0], path[1], depth = pointer(inode.blockArea[:], 13), rest/perBlock, rest%perBlock, 2
	default:
		rest := index - 12 - perBlock - perBlock*perBlock
		if rest >= perBlock*perBlock*perBlock {
			return Mapping{Logical: uint64(logical), Length: 1, Hole: true}, nil
		}
		block, depth = pointer(inode.blockArea[:], 14), 3
		path[0], path[1], path[2] = rest/(perBlock*perBlock), rest/perBlock%perBlock, rest%perBlock
	}
	for level := 0; level < depth && block != 0; level++ {
		if block >= fs.sb.BlocksCount {
			return Mapping{}, corrupt("indirect block outside the volume")
		}
		data, err := fs.cache.get(block)
		if err != nil {
			return Mapping{}, err
		}
		block = pointer(data, path[level])
	}
	if block == 0 {
		return Mapping{Logical: uint64(logical), Length: 1, Hole: true}, nil
	}
	if block >= fs.sb.BlocksCount || block <= uint64(fs.sb.FirstDataBlock) {
		return Mapping{}, corrupt("data block outside the volume")
	}
	return Mapping{Logical: uint64(logical), Physical: block, Length: 1}, nil
}
