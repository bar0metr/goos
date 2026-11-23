package ext4

import (
	"sync"
	"testing"
)

// raceDevice is a read-only in-memory device without the diagnostic read
// counter, so concurrent readers do not race on it under -race.
type raceDevice struct{ data []byte }

func (device *raceDevice) ReadAt(offset uint64, target []byte) bool {
	end := offset + uint64(len(target))
	if end < offset || end > uint64(len(device.data)) {
		return false
	}
	copy(target, device.data[offset:end])
	return true
}
func (device *raceDevice) WriteAt(uint64, []byte) bool { return false }
func (device *raceDevice) Flush() bool                 { return true }
func (device *raceDevice) Capacity() uint64            { return uint64(len(device.data)) }

// TestConcurrentCacheAccess reads, hits, misses and evicts metadata blocks
// from several goroutines at once and requires the shared block cache to stay
// race-free (ADR 0232). Run under -race.
func TestConcurrentCacheAccess(t *testing.T) {
	device := &raceDevice{data: fixture(t, "many").data}
	fs, err := Mount(device, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			// 4096 blocks span the whole cache several times, forcing fills
			// and evictions while other workers read.
			for round := 0; round < 512; round++ {
				block := uint64((worker*101 + round*47) % 4096)
				data, unlock, err := fs.cache.get(block)
				if err != nil {
					t.Error(err)
					return
				}
				if len(data) != int(fs.blockSize) || data[0] != device.data[block*uint64(fs.blockSize)] {
					t.Errorf("block %d contents differ", block)
					return
				}
				unlock()
			}
		}(worker)
	}
	group.Wait()
}
