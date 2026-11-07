package main

import (
	"fmt"
	"sort"
)

const overlayBlockSize = 512

type updateOverlay struct {
	device sliceDevice
	blocks map[uint64][]byte
}

func newUpdateOverlay(device sliceDevice) *updateOverlay {
	return &updateOverlay{device: device, blocks: make(map[uint64][]byte)}
}

func (tx *updateOverlay) Capacity() uint64 { return tx.device.Capacity() }
func (tx *updateOverlay) Flush() bool      { return true }

func (tx *updateOverlay) ReadAt(offset uint64, data []byte) bool {
	if offset > tx.Capacity() || uint64(len(data)) > tx.Capacity()-offset {
		return false
	}
	for len(data) > 0 {
		base := offset / overlayBlockSize * overlayBlockSize
		within := int(offset - base)
		n := min(len(data), overlayBlockSize-within)
		if block, ok := tx.blocks[base]; ok {
			copy(data[:n], block[within:within+n])
		} else if !tx.device.ReadAt(offset, data[:n]) {
			return false
		}
		offset += uint64(n)
		data = data[n:]
	}
	return true
}

func (tx *updateOverlay) WriteAt(offset uint64, data []byte) bool {
	if offset > tx.Capacity() || uint64(len(data)) > tx.Capacity()-offset {
		return false
	}
	for len(data) > 0 {
		base := offset / overlayBlockSize * overlayBlockSize
		within := int(offset - base)
		n := min(len(data), overlayBlockSize-within)
		block, ok := tx.blocks[base]
		if !ok {
			size := min(uint64(overlayBlockSize), tx.Capacity()-base)
			block = make([]byte, size)
			if !tx.device.ReadAt(base, block) {
				return false
			}
			tx.blocks[base] = block
		}
		copy(block[within:within+n], data[:n])
		offset += uint64(n)
		data = data[n:]
	}
	return true
}

// commit rolls back reported write failures when possible. It does not recover
// from process termination or power loss during the commit.
func (tx *updateOverlay) commit() error {
	offsets := make([]uint64, 0, len(tx.blocks))
	for offset := range tx.blocks {
		offsets = append(offsets, offset)
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	original := make([][]byte, len(offsets))
	for i, offset := range offsets {
		original[i] = make([]byte, len(tx.blocks[offset]))
		if !tx.device.ReadAt(offset, original[i]) {
			return fmt.Errorf("read original disk data at %d", offset)
		}
	}
	rollback := func(last int) error {
		failed := false
		for i := last; i >= 0; i-- {
			if !tx.device.WriteAt(offsets[i], original[i]) {
				failed = true
			}
		}
		if !tx.device.Flush() {
			failed = true
		}
		if failed {
			return fmt.Errorf("rollback failed; disk may be inconsistent")
		}
		return nil
	}
	for i, offset := range offsets {
		if !tx.device.WriteAt(offset, tx.blocks[offset]) {
			if err := rollback(i); err != nil {
				return fmt.Errorf("write disk data at %d: %w", offset, err)
			}
			return fmt.Errorf("write disk data at %d; changes rolled back", offset)
		}
	}
	if !tx.device.Flush() {
		if err := rollback(len(offsets) - 1); err != nil {
			return fmt.Errorf("sync disk: %w", err)
		}
		return fmt.Errorf("sync disk failed; changes rolled back")
	}
	return nil
}
