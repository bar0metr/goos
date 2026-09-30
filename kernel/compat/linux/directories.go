package linux

import "goos/internal/limits"

// MaxOpenDirectories bounds the directory descriptions all Linux processes
// hold together. A directory has no open file in the VFS, so its description
// keeps the canonical path it was opened by.
const MaxOpenDirectories = 32

type directorySlot struct {
	owner      uint64
	generation uint32
	size       uint16
	path       [limits.PathMax]byte
}

var directorySlots [MaxOpenDirectories]directorySlot

// RetainDirectory records path for one directory description of owner and
// returns the reference the description stores. The reference carries a
// generation, so a stale one cannot name a slot that was reused.
func RetainDirectory(owner uint64, path []byte) (uint64, bool) {
	if owner == 0 || len(path) == 0 || len(path) > limits.PathMax || path[0] != '/' {
		return 0, false
	}
	for index := range directorySlots {
		slot := &directorySlots[index]
		if slot.owner != 0 {
			continue
		}
		slot.generation++
		if slot.generation == 0 {
			slot.generation = 1
		}
		slot.owner, slot.size = owner, uint16(copy(slot.path[:], path))
		return uint64(slot.generation)<<32 | uint64(index+1), true
	}
	return 0, false
}

// DirectoryPath returns the path behind a reference owner holds. The slice
// aliases the table until the description is released.
func DirectoryPath(owner uint64, reference uint64) ([]byte, bool) {
	slot, found := directory(owner, reference)
	if !found {
		return nil, false
	}
	return slot.path[:slot.size], true
}

func ReleaseDirectory(owner uint64, reference uint64) bool {
	slot, found := directory(owner, reference)
	if !found {
		return false
	}
	slot.owner, slot.size = 0, 0
	return true
}

// releaseDirectories frees every slot of a process that is going away.
func releaseDirectories(owner uint64) {
	for index := range directorySlots {
		if owner != 0 && directorySlots[index].owner == owner {
			directorySlots[index].owner, directorySlots[index].size = 0, 0
		}
	}
}

func directory(owner uint64, reference uint64) (*directorySlot, bool) {
	index := int(uint32(reference)) - 1
	if owner == 0 || index < 0 || index >= len(directorySlots) {
		return nil, false
	}
	slot := &directorySlots[index]
	if slot.owner != owner || slot.generation != uint32(reference>>32) {
		return nil, false
	}
	return slot, true
}
