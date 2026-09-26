package fat32

import "goos/kernel/ramfs"

// FAT copies. With mirroring on — the normal case — every copy is meant to
// hold the same entries. The first copy is authoritative: it is the one read,
// as in Linux and Windows, and every change is written to all copies a whole
// unit at a time, so a unit that had drifted is brought back into agreement by
// the next change within it. Mounting does not compare the copies, since that
// costs a read of every FAT on every mount; CompareFATs does, for a checker.
// With mirroring off only the active copy is used and the others are expected
// to be stale, so there is nothing to compare.

// FATMismatch describes where the FAT copies disagree.
type FATMismatch struct {
	// Entries is how many cluster entries differ between the authoritative
	// copy and another one; zero when all copies agree.
	Entries uint64
	// First is the lowest cluster whose entries differ.
	First uint32
	// Copy is the first copy found to differ from the authoritative one.
	Copy uint32
}

// CompareFATs compares every mirrored FAT copy with the first, entry by entry
// over the entries the volume uses, reading straight from the device after
// writing out pending changes. It reports Unsupported when mirroring is off.
func (fs *FileSystem) CompareFATs() (FATMismatch, ramfs.Status) {
	var mismatch FATMismatch
	if !fs.mounted {
		return mismatch, ramfs.StatusIOError
	}
	if fs.fatCopies != fs.fatTotal {
		return mismatch, ramfs.StatusUnsupported
	}
	if !fs.flushFAT() {
		return mismatch, ramfs.StatusIOError
	}
	half := uint64(len(fs.scratch) / 2)
	first, second := fs.scratch[:half], fs.scratch[half:]
	end := (uint64(fs.clusters) + 2) * 4
	for other := uint32(1); other < fs.fatTotal; other++ {
		for offset := uint64(8); offset < end; offset += half {
			chunk := min(half, end-offset)
			if !fs.device.ReadAt(fs.fatStart+offset, first[:chunk]) ||
				!fs.device.ReadAt(fs.fatStart+uint64(other)*fs.fatSize+offset, second[:chunk]) {
				return mismatch, ramfs.StatusIOError
			}
			for index := uint64(0); index < chunk; index += 4 {
				if get32(first[index:]) == get32(second[index:]) {
					continue
				}
				if mismatch.Entries == 0 {
					mismatch.First, mismatch.Copy = uint32((offset+index)/4), other
				}
				mismatch.Entries++
			}
		}
	}
	return mismatch, ramfs.StatusOK
}
