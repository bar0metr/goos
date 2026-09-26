package fat32

import "goos/kernel/ramfs"

// The FSInfo sector: a free-cluster count and a next-free hint. Both are
// advisory, and an image is untrusted input, so neither is believed. The hint
// only decides where an allocation search starts. The count is never taken
// from the sector: it is counted from the FAT the first time a change needs
// it, kept exact from then on, and written back with every sync.

const (
	fsInfoLead      = uint32(0x41615252)
	fsInfoStruct    = uint32(0x61417272)
	fsInfoTrail     = uint32(0xaa550000)
	fsInfoUnknown   = uint32(0xffffffff)
	fsInfoStructAt  = uint64(484)
	fsInfoFreeCount = uint64(488)
	fsInfoNextFree  = uint64(492)
	fsInfoTrailAt   = uint64(508)
)

// readFSInfo validates the FSInfo sector the BPB names and takes its next-free
// hint when the hint names a data cluster. The recorded count is kept only
// for diagnostics.
func (fs *FileSystem) readFSInfo(geo *geometry) {
	switch {
	case geo.badFSInfo:
		fs.health.FSInfo = FSInfoInvalid
		return
	case geo.fsInfo == 0:
		return
	}
	var info [unit]byte
	offset := geo.fsInfo * geo.sectorSize
	if !fs.device.ReadAt(offset, info[:]) || get32(info[0:]) != fsInfoLead ||
		get32(info[fsInfoStructAt:]) != fsInfoStruct || get32(info[fsInfoTrailAt:]) != fsInfoTrail {
		fs.health.FSInfo = FSInfoInvalid
		return
	}
	fs.health.FSInfo, fs.fsInfo = FSInfoValid, offset
	fs.recordedFree = get32(info[fsInfoFreeCount:])
	if next := get32(info[fsInfoNextFree:]); fs.valid(next) {
		fs.hint = next
	}
}

// writeFSInfo records the count, or "unknown" when it has not been counted,
// and the hint. It runs from sync after the FAT is on disk.
func (fs *FileSystem) writeFSInfo() bool {
	if fs.fsInfo == 0 || fs.readOnly || !fs.hintDirty && !fs.countDirty {
		return true
	}
	if fs.countDirty && !fs.freeKnown && !fs.countFree() {
		return false
	}
	var data [8]byte
	count := fsInfoUnknown
	if fs.freeKnown {
		count = fs.free
	}
	put32(data[0:4], count)
	put32(data[4:8], fs.hint)
	if !fs.write(fs.fsInfo+fsInfoFreeCount, data[:]) {
		return false
	}
	fs.recordedFree, fs.hintDirty, fs.countDirty = count, false, false
	return true
}

// countFree counts the free clusters by reading the FAT the entries are read
// from, a buffer at a time straight from the device. Dirty FAT sectors are
// written first so the device holds what the window does.
func (fs *FileSystem) countFree() bool {
	if !fs.flushFAT() {
		return false
	}
	base := fs.fatStart + uint64(fs.fatRead)*fs.fatSize
	first, end := uint64(2)*4, (uint64(fs.clusters)+2)*4
	free := uint32(0)
	for offset := first; offset < end; {
		chunk := uint64(len(fs.scratch))
		if chunk > end-offset {
			chunk = end - offset
		}
		if !fs.device.ReadAt(base+offset, fs.scratch[:chunk]) {
			return false
		}
		for index := uint64(0); index < chunk; index += 4 {
			if get32(fs.scratch[index:])&clusterMask == 0 {
				free++
			}
		}
		offset += chunk
	}
	fs.free, fs.freeKnown = free, true
	return true
}

// FreeClusters is the number of free data clusters, counted from the FAT on
// first use and kept exact afterwards.
func (fs *FileSystem) FreeClusters() (uint32, ramfs.Status) {
	if !fs.mounted {
		return 0, ramfs.StatusIOError
	}
	if !fs.freeKnown && !fs.countFree() {
		return 0, ramfs.StatusIOError
	}
	return fs.free, ramfs.StatusOK
}

// claimed and released keep the count exact once it is known.
func (fs *FileSystem) claimed() {
	if fs.freeKnown {
		fs.free--
	}
	fs.countDirty = true
}

func (fs *FileSystem) released() {
	if fs.freeKnown {
		fs.free++
	}
	fs.countDirty = true
}
