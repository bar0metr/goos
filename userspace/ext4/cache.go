package ext4

// Device is the bounded block slice a volume lives on. Offsets are relative
// to the start of the slice; the block service enforces the slice bounds a
// second time.
type Device interface {
	ReadAt(offset uint64, target []byte) bool
	WriteAt(offset uint64, data []byte) bool
	Flush() bool
	Capacity() uint64
}

// cacheBlocks bounds the resident metadata window.
const cacheBlocks = 32

type cachedBlock struct {
	block uint64
	stamp uint64
	valid bool
	data  []byte
}

// blockCache keeps recently used metadata blocks. File data bypasses it.
type blockCache struct {
	device  Device
	size    uint64
	limit   uint64
	clock   uint64
	entries [cacheBlocks]cachedBlock
	reads   uint64
	hits    uint64
	// overlay holds the blocks the running operation has staged and base
	// those of the pending transaction. Reads see overlay, then base, so
	// operations observe changes before they reach the device.
	overlay map[uint64]*stagedBlock
	base    map[uint64]*stagedBlock
}

// staged returns the staged copy of block, if a transaction holds one.
func (cache *blockCache) staged(block uint64) *stagedBlock {
	if staged := cache.overlay[block]; staged != nil {
		return staged
	}
	return cache.base[block]
}

func (cache *blockCache) init(device Device, blockSize uint32, blocks uint64) {
	cache.device, cache.size, cache.limit = device, uint64(blockSize), blocks
	backing := make([]byte, cacheBlocks*int(blockSize))
	for index := range cache.entries {
		cache.entries[index] = cachedBlock{data: backing[index*int(blockSize) : (index+1)*int(blockSize)]}
	}
}

// get returns the contents of block. The slice stays valid until the next
// get; callers copy what they keep.
func (cache *blockCache) get(block uint64) ([]byte, error) {
	if block >= cache.limit {
		return nil, corrupt("block number beyond the volume")
	}
	if staged := cache.staged(block); staged != nil {
		return staged.data, nil
	}
	cache.clock++
	cache.reads++
	victim := &cache.entries[0]
	for index := range cache.entries {
		entry := &cache.entries[index]
		if entry.valid && entry.block == block {
			entry.stamp = cache.clock
			cache.hits++
			return entry.data, nil
		}
		if !victim.valid {
			continue
		}
		if !entry.valid || entry.stamp < victim.stamp {
			victim = entry
		}
	}
	victim.valid = false
	if !cache.device.ReadAt(block*cache.size, victim.data) {
		return nil, errIO
	}
	victim.block, victim.stamp, victim.valid = block, cache.clock, true
	return victim.data, nil
}

// readBlocks reads count whole blocks starting at block straight from the
// device into target.
func (cache *blockCache) readBlocks(block uint64, target []byte) error {
	count := uint64(len(target)) / cache.size
	if end, ok := addOK(block, count); !ok || end > cache.limit {
		return corrupt("data run beyond the volume")
	}
	if len(cache.overlay) != 0 || len(cache.base) != 0 {
		return cache.readStaged(block, count, target)
	}
	if !cache.device.ReadAt(block*cache.size, target) {
		return errIO
	}
	return nil
}

// readStaged serves a run block by block, preferring staged contents.
func (cache *blockCache) readStaged(block uint64, count uint64, target []byte) error {
	size := cache.size
	for index := uint64(0); index < count; index++ {
		out := target[index*size : (index+1)*size]
		if staged := cache.staged(block + index); staged != nil {
			copy(out, staged.data)
			continue
		}
		if !cache.device.ReadAt((block+index)*size, out) {
			return errIO
		}
	}
	return nil
}

// update replaces the cached copy of block, if one is resident, after a
// transaction wrote it.
func (cache *blockCache) update(block uint64, data []byte) {
	for index := range cache.entries {
		entry := &cache.entries[index]
		if entry.valid && entry.block == block {
			copy(entry.data, data)
		}
	}
}

// invalidate drops every resident block.
func (cache *blockCache) invalidate() {
	for index := range cache.entries {
		cache.entries[index].valid = false
	}
}
