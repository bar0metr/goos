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

type fsInfoValues struct {
	free uint32
	next uint32
}

func (fs *FileSystem) readFSInfoAt(offset uint64) (fsInfoValues, bool) {
	var info [unit]byte
	if offset == 0 || !fs.device.ReadAt(offset, info[:]) || get32(info[0:]) != fsInfoLead ||
		get32(info[fsInfoStructAt:]) != fsInfoStruct || get32(info[fsInfoTrailAt:]) != fsInfoTrail {
		return fsInfoValues{}, false
	}
	return fsInfoValues{free: get32(info[fsInfoFreeCount:]), next: get32(info[fsInfoNextFree:])}, true
}

// readFSInfo validates both copies. The primary governs when both are valid;
// the backup is the fallback when the primary is not. Differing or invalid
// peers are reported and reconciled only by a later clean sync.
func (fs *FileSystem) readFSInfo(geo *geometry) {
	if geo.badFSInfo {
		fs.health.FSInfo = FSInfoInvalid
	} else if geo.fsInfo != 0 {
		fs.fsInfoPrimary = geo.fsInfo * geo.sectorSize
	}
	if geo.badBackupFSInfo {
		fs.health.BackupFSInfo = BackupFSInfoInvalid
	} else if geo.backupFSInfo != 0 {
		fs.fsInfoBackup = geo.backupFSInfo * geo.sectorSize
	}
	primary, primaryOK := fs.readFSInfoAt(fs.fsInfoPrimary)
	backup, backupOK := fs.readFSInfoAt(fs.fsInfoBackup)
	if fs.fsInfoPrimary != 0 {
		if primaryOK {
			fs.health.FSInfo = FSInfoValid
		} else {
			fs.health.FSInfo = FSInfoInvalid
		}
	}
	if fs.fsInfoBackup != 0 {
		switch {
		case !backupOK:
			fs.health.BackupFSInfo = BackupFSInfoInvalid
		case primaryOK && primary != backup:
			fs.health.BackupFSInfo = BackupFSInfoDiffers
		default:
			fs.health.BackupFSInfo = BackupFSInfoMatches
		}
	}
	chosen, ok := primary, primaryOK
	fs.fsInfo = fs.fsInfoPrimary
	if !ok {
		chosen, ok, fs.fsInfo = backup, backupOK, fs.fsInfoBackup
	}
	if !ok {
		fs.fsInfo = 0
		return
	}
	fs.recordedFree = chosen.free
	if fs.valid(chosen.next) {
		fs.hint = chosen.next
	}
	fs.fsInfoSync = fs.fsInfoPrimary != 0 && !primaryOK || fs.fsInfoBackup != 0 && (!backupOK || primaryOK && primary != backup)
}

// writeFSInfo records the count, or "unknown" when it has not been counted,
// and the hint. It runs from sync after the FAT is on disk.
func (fs *FileSystem) writeFSInfo() bool {
	if fs.fsInfo == 0 || fs.readOnly || !fs.hintDirty && !fs.countDirty && !fs.fsInfoSync {
		return true
	}
	if fs.countDirty && !fs.freeKnown && !fs.countFree() {
		return false
	}
	var data [unit]byte
	put32(data[0:], fsInfoLead)
	put32(data[fsInfoStructAt:], fsInfoStruct)
	count := fsInfoUnknown
	if fs.freeKnown {
		count = fs.free
	}
	put32(data[fsInfoFreeCount:], count)
	put32(data[fsInfoNextFree:], fs.hint)
	put32(data[fsInfoTrailAt:], fsInfoTrail)
	for _, offset := range [2]uint64{fs.fsInfoPrimary, fs.fsInfoBackup} {
		if offset != 0 && !fs.write(offset, data[:]) {
			return false
		}
	}
	fs.recordedFree, fs.hintDirty, fs.countDirty, fs.fsInfoSync = count, false, false, false
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

// FilesystemInfo derives capacity from validated geometry and the live free
// cluster count. It never treats the advisory on-disk FSInfo count as
// authoritative.
func (fs *FileSystem) FilesystemInfo() (ramfs.FilesystemInfo, ramfs.Status) {
	freeClusters, status := fs.FreeClusters()
	if status != ramfs.StatusOK {
		return ramfs.FilesystemInfo{}, status
	}
	total, ok := checkedCapacity(uint64(fs.clusters), fs.clusterSize)
	if !ok {
		return ramfs.FilesystemInfo{}, ramfs.StatusCorrupt
	}
	free, ok := checkedCapacity(uint64(freeClusters), fs.clusterSize)
	if !ok || free > total {
		return ramfs.FilesystemInfo{}, ramfs.StatusCorrupt
	}
	return ramfs.FilesystemInfo{Type: ramfs.FilesystemFAT32, TotalBytes: total, FreeBytes: free,
		AvailableBytes: free, ReadOnly: fs.readOnly, CapacityAvailable: true}, ramfs.StatusOK
}

func checkedCapacity(count, size uint64) (uint64, bool) {
	if size != 0 && count > ^uint64(0)/size {
		return 0, false
	}
	return count * size, true
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
