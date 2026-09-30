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

// RetainDirectory records path for one directory description of a family and
// returns the reference the description stores. The reference carries a
// generation, so a stale one cannot name a slot that was reused.
func RetainDirectory(familyID uint64, path []byte) (uint64, bool) {
	if familyID == 0 || len(path) == 0 || len(path) > limits.PathMax || path[0] != '/' {
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
		slot.owner, slot.size = familyID, uint16(copy(slot.path[:], path))
		return uint64(slot.generation)<<32 | uint64(index+1), true
	}
	return 0, false
}

// DirectoryPath returns the path behind a reference the family holds. The slice
// aliases the table until the description is released.
func DirectoryPath(familyID uint64, reference uint64) ([]byte, bool) {
	slot, found := directory(familyID, reference)
	if !found {
		return nil, false
	}
	return slot.path[:slot.size], true
}

func ReleaseDirectory(familyID uint64, reference uint64) bool {
	slot, found := directory(familyID, reference)
	if !found {
		return false
	}
	slot.owner, slot.size = 0, 0
	return true
}

// releaseDirectories frees every slot when its family is gone.
func releaseDirectories(familyID uint64) {
	for index := range directorySlots {
		if familyID != 0 && directorySlots[index].owner == familyID {
			directorySlots[index].owner, directorySlots[index].size = 0, 0
		}
	}
}

func directory(familyID uint64, reference uint64) (*directorySlot, bool) {
	index := int(uint32(reference)) - 1
	if familyID == 0 || index < 0 || index >= len(directorySlots) {
		return nil, false
	}
	slot := &directorySlots[index]
	if slot.owner != familyID || slot.generation != uint32(reference>>32) {
		return nil, false
	}
	return slot, true
}
