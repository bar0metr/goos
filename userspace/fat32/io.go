package fat32

const (
	// cacheSectors bounds the resident metadata window. Directory entries and
	// FAT links are packed many to a sector, so a small window removes almost
	// all repeated device traffic for metadata scans.
	cacheSectors = 16
	// zeroSize is the buffer zeroing writes are issued from.
	zeroSize = 4096
)

// cachedSector holds one resident device sector. The filesystem owns the
// device, so a write-through copy stays coherent without invalidation traffic.
type cachedSector struct {
	lba   uint64
	stamp uint64
	valid bool
	data  [sectorSize]byte
}

// resident returns the cached sector for lba, reading it from the device on a
// miss and replacing the entry used longest ago.
func (fs *FileSystem) resident(lba uint64) (*cachedSector, bool) {
	fs.clock++
	if entry := fs.lookup(lba); entry != nil {
		entry.stamp = fs.clock
		return entry, true
	}
	entry := &fs.cache[0]
	for index := range fs.cache {
		candidate := &fs.cache[index]
		if !candidate.valid {
			entry = candidate
			break
		}
		if candidate.stamp < entry.stamp {
			entry = candidate
		}
	}
	entry.valid = false
	if !fs.device.ReadAt(lba*sectorSize, entry.data[:]) {
		return nil, false
	}
	entry.lba, entry.stamp, entry.valid = lba, fs.clock, true
	return entry, true
}

func (fs *FileSystem) lookup(lba uint64) *cachedSector {
	for index := range fs.cache {
		if entry := &fs.cache[index]; entry.valid && entry.lba == lba {
			return entry
		}
	}
	return nil
}

// readMeta serves metadata reads — FAT entries, directory entries — from
// resident sectors.
func (fs *FileSystem) readMeta(offset uint64, target []byte) bool {
	for len(target) != 0 {
		entry, ok := fs.resident(offset / sectorSize)
		if !ok {
			return false
		}
		within := offset % sectorSize
		chunk := sectorSize - within
		if chunk > uint64(len(target)) {
			chunk = uint64(len(target))
		}
		copy(target[:chunk], entry.data[within:within+chunk])
		target = target[chunk:]
		offset += chunk
	}
	return true
}

// readData reads file data straight from the device. File data is never
// cached here: the VFS keeps a page cache of it, and streaming it through this
// window would only evict the metadata. The window is write-through, so the
// device always holds the current bytes.
func (fs *FileSystem) readData(offset uint64, target []byte) bool {
	return fs.device.ReadAt(offset, target)
}

// write writes through to the device and keeps resident sectors coherent: a
// resident sector the write covers is updated on success and dropped when the
// device write fails, so it never holds superseded bytes.
func (fs *FileSystem) write(offset uint64, data []byte) bool {
	ok := fs.device.WriteAt(offset, data)
	end := offset + uint64(len(data))
	for index := range fs.cache {
		entry := &fs.cache[index]
		start := entry.lba * sectorSize
		if !entry.valid || start >= end || start+sectorSize <= offset {
			continue
		}
		if !ok {
			entry.valid = false
			continue
		}
		from, to := offset, end
		if from < start {
			from = start
		}
		if to > start+sectorSize {
			to = start + sectorSize
		}
		copy(entry.data[from-start:to-start], data[from-offset:to-offset])
	}
	return ok
}
