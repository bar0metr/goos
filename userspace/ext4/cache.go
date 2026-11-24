package ext4

import (
	"sync"
	"sync/atomic"
)

// Device is the bounded block slice a volume lives on. Offsets are relative
// to the start of the slice; the block service enforces the slice bounds a
// second time.
type Device interface {
	ReadAt(offset uint64, target []byte) bool
	WriteAt(offset uint64, data []byte) bool
	Flush() bool
	Capacity() uint64
}

// cacheBlocks bounds the resident metadata window: 1 MiB at 4 KiB blocks,
// enough for the inode table blocks of a few thousand files listed in hash
// order. A block can sit only in the cacheWays entries of its set, so a
// lookup compares four entries, not all of them.
const (
	cacheBlocks = 256
	cacheWays   = 4
	cacheSets   = cacheBlocks / cacheWays
)

type cachedBlock struct {
	block uint64
	stamp uint64
	valid bool
	data  []byte
}

// blockCache keeps recently used metadata blocks. File data bypasses it. Each
// set has its own lock; clock, reads and hits use atomics. get lends a slice
// under the set lock, and callers copy or consume it before releasing the borrow.
type blockCache struct {
	device  Device
	size    uint64
	limit   uint64
	entries [cacheBlocks]cachedBlock
	locks   [cacheSets]sync.Mutex
	clock   uint64
	reads   uint64
	hits    uint64
	// overlay holds the blocks the running operation has staged and base
	// those of the pending transaction. Reads see overlay, then base, so
	// operations observe changes before they reach the device. They are
	// written only by the single mutation owner behind a barrier, so staged
	// reads need no lock here.
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

// get returns the contents of block and an unlock the caller must run once it
// is done with the slice. The slice is borrowed: it aliases the cache's
// backing (or a staged copy), so the caller must not keep it past unlock and
// must not call get again on the same set before unlock. A staged copy wins;
// otherwise a miss reads the device into an owned buffer and fills the cache
// without holding the set lock across the device read.
func (cache *blockCache) get(block uint64) ([]byte, func(), error) {
	if block >= cache.limit {
		return nil, nil, corrupt("block number beyond the volume")
	}
	if staged := cache.staged(block); staged != nil {
		return staged.data, func() {}, nil
	}
	set := int(block % cacheSets)
	lock := &cache.locks[set]
	first := set * cacheWays

	lock.Lock()
	for index := 0; index < cacheWays; index++ {
		entry := &cache.entries[first+index]
		if entry.valid && entry.block == block {
			entry.stamp = atomic.AddUint64(&cache.clock, 1)
			atomic.AddUint64(&cache.hits, 1)
			return entry.data, lock.Unlock, nil
		}
	}
	lock.Unlock()

	data := make([]byte, cache.size)
	atomic.AddUint64(&cache.reads, 1)
	if !cache.device.ReadAt(block*cache.size, data) {
		return nil, nil, errIO
	}
	lock.Lock()
	// Another miss may have filled the block while the device was read; its
	// copy is equivalent, so keep the fresh data and drop the duplicate.
	victim := &cache.entries[first]
	for index := 0; index < cacheWays; index++ {
		entry := &cache.entries[first+index]
		if entry.valid && entry.block == block {
			return entry.data, lock.Unlock, nil
		}
		if !victim.valid {
			continue
		}
		if !entry.valid || entry.stamp < victim.stamp {
			victim = entry
		}
	}
	victim.valid = false
	copy(victim.data, data)
	victim.block = block
	victim.stamp = atomic.AddUint64(&cache.clock, 1)
	victim.valid = true
	return victim.data, lock.Unlock, nil
}

// withBlock runs fn with the borrowed contents of block and releases the
// borrow when fn returns. It keeps the borrow scoped where a caller needs the
// block only inside a closure, such as a validation loop.
func (cache *blockCache) withBlock(block uint64, fn func([]byte) error) error {
	data, unlock, err := cache.get(block)
	if err != nil {
		return err
	}
	defer unlock()
	return fn(data)
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
	lock := &cache.locks[int(block%cacheSets)]
	lock.Lock()
	defer lock.Unlock()
	first := int(block%cacheSets) * cacheWays
	for index := first; index < first+cacheWays; index++ {
		entry := &cache.entries[index]
		if entry.valid && entry.block == block {
			copy(entry.data, data)
		}
	}
}

// invalidate drops every resident block.
func (cache *blockCache) invalidate() {
	for set := 0; set < cacheSets; set++ {
		cache.locks[set].Lock()
		for index := 0; index < cacheWays; index++ {
			cache.entries[set*cacheWays+index].valid = false
		}
		cache.locks[set].Unlock()
	}
}
