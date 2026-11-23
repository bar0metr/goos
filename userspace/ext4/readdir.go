package ext4

import "sort"

// The listing prefetch removes the inode-at-a-time scattered reads of a
// large hash-ordered directory. A listing batch collects its entries first,
// orders them by inode-table block, and reads each block once; the prefetch
// keeps those blocks resident across listing calls, so every block of a
// 10000-name directory's inode table costs one device read for the whole
// listing instead of one scattered read per entry (ADR 0231).
const (
	// MaxReaddirPrefetchEntries bounds the entries one listing batch
	// collects and decodes.
	MaxReaddirPrefetchEntries = 256
	// MaxReaddirPrefetchBlocks bounds the inode-table blocks the listing
	// prefetch keeps resident. 4 MiB at 4 KiB blocks, which covers the
	// inode table of the fsbench 10000-name workload.
	MaxReaddirPrefetchBlocks = 1024
)

// listBlock is one resident block of the listing prefetch.
type listBlock struct {
	block uint64
	valid bool
	stamp uint64
	data  []byte
}

// ListPrefetch is the bounded scratch a directory listing reuses across
// ReadDirBatch calls. Its block buffers live in one allocation made at
// mount; nothing here grows with the directory.
type ListPrefetch struct {
	blocks [MaxReaddirPrefetchBlocks]listBlock
	order  [MaxReaddirPrefetchEntries]int
	clock  uint64
}

// List returns the mounted volume's listing prefetch.
func (fs *FS) List() *ListPrefetch { return &fs.list }

// init allocates the block buffers once for the mounted block size.
func (prefetch *ListPrefetch) init(blockSize uint32) {
	backing := make([]byte, MaxReaddirPrefetchBlocks*int(blockSize))
	for index := range prefetch.blocks {
		prefetch.blocks[index].data = backing[index*int(blockSize) : (index+1)*int(blockSize)]
	}
}

// invalidate forgets every resident block: a committed transaction may have
// rewritten inode-table or bitmap blocks the prefetch copied earlier.
func (prefetch *ListPrefetch) invalidate() {
	for index := range prefetch.blocks {
		prefetch.blocks[index].valid = false
	}
}

// fetch returns block's contents, reading it once from the device into the
// prefetch. The returned slice stays valid until a later fetch evicts the
// slot; the batch decodes everything it needs before its next fetch.
func (fs *FS) prefetchBlock(prefetch *ListPrefetch, block uint64) ([]byte, error) {
	if block >= fs.cache.limit {
		return nil, corrupt("block number beyond the volume")
	}
	prefetch.clock++
	target := -1
	for index := range prefetch.blocks {
		entry := &prefetch.blocks[index]
		if entry.valid && entry.block == block {
			entry.stamp = prefetch.clock
			return entry.data, nil
		}
		if !entry.valid {
			target = index
			break
		}
		if target < 0 || entry.stamp < prefetch.blocks[target].stamp {
			target = index
		}
	}
	entry := &prefetch.blocks[target]
	if staged := fs.cache.staged(block); staged != nil {
		copy(entry.data, staged.data)
	} else if !fs.cache.device.ReadAt(block*fs.cache.size, entry.data) {
		return nil, errIO
	}
	entry.block, entry.valid, entry.stamp = block, true, prefetch.clock
	return entry.data, nil
}

// PrefetchDir collects up to MaxReaddirPrefetchEntries entries of dir from
// cursor, checks each named inode is allocated, orders the batch by
// inode-table block, fetches each block once through prefetch and decodes
// the inodes while the block is resident. It returns the entries and their
// decoded, checksum-verified inodes in cursor order, the cursor to continue
// from, and whether the walk reached the directory end. The caller walks
// the returned range in order; the inode's raw bytes alias the prefetch
// blocks and stay valid until the next PrefetchDir call.
func (fs *FS) PrefetchDir(prefetch *ListPrefetch, dir *Inode, cursor uint64, entries *[MaxReaddirPrefetchEntries]DirEntry, inodes *[MaxReaddirPrefetchEntries]Inode) (count int, next uint64, end bool, err error) {
	if !dir.IsDir() {
		return 0, 0, false, fail(KindNotDirectory, "")
	}
	count = 0
	next, err = fs.ReadDir(dir, cursor, func(entry *DirEntry) bool {
		if count == MaxReaddirPrefetchEntries {
			return false
		}
		entries[count] = *entry
		count++
		return true
	})
	if err != nil {
		return 0, 0, false, err
	}
	// Allocated checks run in cursor order, so a stale entry stops the
	// listing exactly as the per-entry path does.
	for index := 0; index < count; index++ {
		allocated, err := fs.inodeAllocated(entries[index].Inode)
		if err != nil {
			return 0, 0, false, err
		}
		if !allocated {
			return 0, 0, false, fail(KindStale, "inode not allocated")
		}
	}
	// Order the batch by inode-table block, then inode number, so each
	// block is fetched once and decoded while resident.
	isize := uint64(fs.sb.InodeSize)
	var locations [MaxReaddirPrefetchEntries]struct {
		block  uint64
		within int
	}
	for index := 0; index < count; index++ {
		group, ino, err := fs.inodeLocation(entries[index].Inode)
		if err != nil {
			return 0, 0, false, err
		}
		desc, err := fs.group(group)
		if err != nil {
			return 0, 0, false, err
		}
		byteOffset := uint64(ino) * isize
		locations[index] = struct {
			block  uint64
			within int
		}{desc.InodeTable + byteOffset/uint64(fs.blockSize), int(byteOffset % uint64(fs.blockSize))}
		prefetch.order[index] = index
	}
	order := prefetch.order[:count]
	sort.Slice(order, func(a, b int) bool {
		if locations[order[a]].block != locations[order[b]].block {
			return locations[order[a]].block < locations[order[b]].block
		}
		return entries[order[a]].Inode < entries[order[b]].Inode
	})
	for at := 0; at < count; {
		block := locations[order[at]].block
		data, err := fs.prefetchBlock(prefetch, block)
		if err != nil {
			return 0, 0, false, err
		}
		for at < count && locations[order[at]].block == block {
			index := order[at]
			raw := data[locations[index].within : locations[index].within+int(isize)]
			if err := fs.decodeInodeInto(&inodes[index], entries[index].Inode, raw); err != nil {
				return 0, 0, false, err
			}
			at++
		}
	}
	// Live checks and layout validation in cursor order.
	for index := 0; index < count; index++ {
		if inodes[index].Mode == 0 || inodes[index].Links == 0 {
			return 0, 0, false, fail(KindStale, "inode has no links")
		}
		if err := fs.checkInode(&inodes[index]); err != nil {
			return 0, 0, false, err
		}
	}
	return count, next, next >= dir.Size, nil
}
