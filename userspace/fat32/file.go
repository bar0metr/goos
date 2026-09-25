package fat32

import "goos/kernel/ramfs"

// span is a run of physically consecutive clusters of one chain, starting at
// the cluster at ordinal. Data moves one span per device request, so a file
// laid out contiguously costs one request per transfer, not one per cluster.
type span struct {
	cluster uint32
	count   uint64
}

// runFrom returns the span that starts at cluster and extends while the
// chain continues into the next cluster, up to limit clusters. It reports the
// cluster after the span, zero at the end of the chain.
func (fs *FileSystem) runFrom(cluster uint32, limit uint64) (span, uint32, ramfs.Status) {
	run := span{cluster: cluster, count: 1}
	for {
		next, end, status := fs.next(cluster)
		if status != ramfs.StatusOK {
			return run, 0, status
		}
		if end {
			return run, 0, ramfs.StatusOK
		}
		if next != cluster+1 || run.count == limit {
			return run, next, ramfs.StatusOK
		}
		cluster = next
		run.count++
	}
}

// transfer moves length bytes at file offset through the chain starting at
// first, reading into or writing from data (zeroing when data is nil). The
// chain must already cover the range; a chain that ends early is corrupt.
func (fs *FileSystem) transfer(first uint32, offset uint64, data []byte, length uint64, write bool) (uint64, ramfs.Status) {
	if length == 0 {
		return 0, ramfs.StatusOK
	}
	ordinal := offset / fs.clusterSize
	cluster, found, status := fs.seek(first, ordinal)
	if status != ramfs.StatusOK {
		return 0, status
	}
	if !found {
		return 0, ramfs.StatusCorrupt
	}
	within := offset % fs.clusterSize
	done := uint64(0)
	for done < length {
		wanted := (within + length - done + fs.clusterSize - 1) / fs.clusterSize
		run, next, status := fs.runFrom(cluster, wanted)
		if status != ramfs.StatusOK {
			return done, status
		}
		chunk := run.count*fs.clusterSize - within
		if chunk > length-done {
			chunk = length - done
		}
		device := fs.clusterOffset(run.cluster) + within
		var ok bool
		switch {
		case !write:
			ok = fs.readData(device, data[done:done+chunk])
		case data == nil:
			ok = fs.zeroRange(device, chunk)
		default:
			ok = fs.write(device, data[done:done+chunk])
		}
		if !ok {
			return done, ramfs.StatusIOError
		}
		done += chunk
		within = 0
		ordinal += run.count
		if done < length {
			if next == 0 {
				return done, ramfs.StatusCorrupt
			}
			cluster = next
			fs.remember(first, uint32(ordinal), cluster)
		}
	}
	return done, ramfs.StatusOK
}

// reach makes the chain of a file hold at least count clusters and returns
// its first cluster, which is new when the file had none.
func (fs *FileSystem) reach(first uint32, count uint64) (uint32, ramfs.Status) {
	if count == 0 {
		return first, ramfs.StatusOK
	}
	if first == 0 {
		return fs.extend(0, count)
	}
	tail, have, status := fs.last(first)
	if status != ramfs.StatusOK || have >= count {
		return first, status
	}
	added, status := fs.extend(tail, count-have)
	if status == ramfs.StatusOK {
		fs.remember(first, uint32(have), added)
	}
	return first, status
}

func (fs *FileSystem) Read(id ramfs.NodeID, offset uint64, target []byte) (int, ramfs.Status) {
	_, _, entry, status := fs.fileNode(id)
	if status != ramfs.StatusOK {
		return 0, status
	}
	size := entrySizeOf(entry)
	if offset >= size || len(target) == 0 {
		return 0, ramfs.StatusOK
	}
	count := uint64(len(target))
	if count > size-offset {
		count = size - offset
	}
	first := entryCluster(entry)
	if first == 0 {
		// A file with bytes must have clusters to hold them.
		return 0, ramfs.StatusCorrupt
	}
	done, status := fs.transfer(first, offset, target, count, false)
	return int(done), status
}

// Write stores data at offset, growing the file as needed. Every cluster the
// write needs is claimed before any byte is written, so a volume that runs
// out of space refuses the whole write and changes nothing. Bytes between the
// old end of the file and offset read as zero afterwards: they are written as
// zeros, because a cluster that is reused, or the tail of one a truncation
// kept, still holds whatever was there before.
func (fs *FileSystem) Write(id ramfs.NodeID, offset uint64, data []byte) (int, ramfs.Status) {
	dir, index, entry, status := fs.fileNode(id)
	if status != ramfs.StatusOK {
		return 0, status
	}
	if len(data) == 0 {
		return 0, ramfs.StatusOK
	}
	length := uint64(len(data))
	if offset > maxFileSize || length > maxFileSize-offset {
		return 0, ramfs.StatusResourceExhausted
	}
	size, end := entrySizeOf(entry), offset+length
	target := end
	if size > target {
		target = size
	}
	first, status := fs.reach(entryCluster(entry), fs.clustersFor(target))
	if status != ramfs.StatusOK {
		return 0, status
	}
	if first != entryCluster(entry) {
		// The chain is new. Record it before any data goes out, so that a
		// failure below leaves it owned by the file rather than lost.
		setEntryCluster(&entry, first)
		if status = fs.writeEntry(&dir, index, entry); status != ramfs.StatusOK {
			fs.freeChain(first)
			return 0, status
		}
	}
	if offset > size {
		if _, status = fs.transfer(first, size, nil, offset-size, true); status != ramfs.StatusOK {
			return 0, status
		}
	}
	done, status := fs.transfer(first, offset, data, length, true)
	if offset+done > size {
		put32(entry[28:32], uint32(offset+done))
		if written := fs.writeEntry(&dir, index, entry); status == ramfs.StatusOK {
			status = written
		}
	}
	if synced := fs.sync(); status == ramfs.StatusOK {
		status = synced
	}
	return int(done), status
}

// Truncate sets the size of a file. Shrinking records the new size first,
// then ends the chain after the last cluster still needed and frees the rest,
// the order Linux uses, so an interruption loses clusters rather than leaving
// the file pointing at freed ones. Growing claims clusters and writes zeros
// over the new range before the new size is recorded, so stale bytes are
// never exposed.
func (fs *FileSystem) Truncate(id ramfs.NodeID, size uint64) ramfs.Status {
	dir, index, entry, status := fs.fileNode(id)
	if status != ramfs.StatusOK {
		return status
	}
	if size > maxFileSize {
		return ramfs.StatusResourceExhausted
	}
	old, first := entrySizeOf(entry), entryCluster(entry)
	switch {
	case size == old:
		return ramfs.StatusOK
	case size < old:
		keep := fs.clustersFor(size)
		put32(entry[28:32], uint32(size))
		if keep == 0 {
			setEntryCluster(&entry, 0)
		}
		if status = fs.writeEntry(&dir, index, entry); status != ramfs.StatusOK {
			return status
		}
		if first != 0 {
			status = fs.cut(first, keep)
		}
	default:
		if first, status = fs.reach(first, fs.clustersFor(size)); status != ramfs.StatusOK {
			return status
		}
		if first != entryCluster(entry) {
			setEntryCluster(&entry, first)
		}
		if _, status = fs.transfer(first, old, nil, size-old, true); status != ramfs.StatusOK {
			// The new clusters stay with the file under its old size, where
			// a later truncation or unlink frees them.
			fs.writeEntry(&dir, index, entry)
			fs.sync()
			return status
		}
		put32(entry[28:32], uint32(size))
		status = fs.writeEntry(&dir, index, entry)
	}
	if synced := fs.sync(); status == ramfs.StatusOK {
		status = synced
	}
	return status
}

// cut keeps the first keep clusters of a chain and frees the rest; keep zero
// frees the whole chain.
func (fs *FileSystem) cut(first uint32, keep uint64) ramfs.Status {
	fs.forgetPositions()
	if keep == 0 {
		fs.freeChain(first)
		return ramfs.StatusOK
	}
	last, found, status := fs.seek(first, keep-1)
	if status != ramfs.StatusOK {
		return status
	}
	if !found {
		// The chain was already shorter than the new size needs.
		return ramfs.StatusCorrupt
	}
	next, end, status := fs.next(last)
	if status != ramfs.StatusOK || end {
		return status
	}
	if status = fs.setFAT(last, endOfChainMark); status != ramfs.StatusOK {
		return status
	}
	fs.freeChain(next)
	return ramfs.StatusOK
}
