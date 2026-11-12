package ext4

// The orphan list keeps inodes whose cleanup spans transactions, as Linux's
// original list does: the superblock's s_last_orphan names the first
// inode, and each listed inode's i_dtime names the next. An inode joins in
// the transaction that starts the work and leaves in the one that ends it:
//
//   - an inode whose last link is gone, until release frees it. While a
//     caller holds it open (Pin), it stays listed with no links and keeps
//     its blocks;
//   - a file being shrunk, until every block past its new size is free.
//
// A writable mount finishes each listed inode before it accepts requests:
// one with no links is released, any other is truncated to its size.

// maxOrphanWalk bounds the list walk at mount; a longer list loops.
const maxOrphanWalk = 1 << 20

// orphanHead returns the first listed inode, or zero.
func (fs *FS) orphanHead() uint32 {
	if len(fs.orphans) == 0 {
		return 0
	}
	return fs.orphans[0]
}

func orphanIndex(list []uint32, number uint32) int {
	for index, listed := range list {
		if listed == number {
			return index
		}
	}
	return -1
}

// isOrphan reports whether inode number is on the committed or open list.
func (fs *FS) isOrphan(number uint32) bool {
	list := fs.orphans
	if fs.txn != nil {
		list = fs.txn.orphans
	}
	return orphanIndex(list, number) >= 0
}

// orphanInode reads an allocated inode whether or not it has links.
func (fs *FS) orphanInode(number uint32) (*Inode, error) {
	allocated, err := fs.inodeAllocated(number)
	if err != nil {
		return nil, err
	}
	if !allocated {
		return nil, corrupt("orphan list names a free inode")
	}
	inode, err := fs.readInode(number)
	if err != nil {
		return nil, err
	}
	if inode.Mode == 0 {
		return nil, corrupt("orphan list names an inode without a mode")
	}
	if err := fs.checkInode(inode); err != nil {
		return nil, err
	}
	return inode, nil
}

// addOrphan puts inode at the head of the list. Its i_dtime takes the old
// head; the superblock and inode table blocks this stages are in the
// commit headroom.
func (fs *FS) addOrphan(inode *Inode) {
	t := fs.txn
	if orphanIndex(t.orphans, inode.Number) >= 0 {
		return
	}
	next := uint32(0)
	if len(t.orphans) > 0 {
		next = t.orphans[0]
	}
	list := make([]uint32, 0, len(t.orphans)+1)
	list = append(list, inode.Number)
	t.orphans = append(list, t.orphans...)
	t.orphansEdited = true
	putLE32(inode.raw[0x14:], next)
	inode.dirty = true
}

// removeOrphan takes inode off the list, pointing its predecessor (or the
// superblock) at its successor.
func (fs *FS) removeOrphan(inode *Inode) error {
	t := fs.txn
	at := orphanIndex(t.orphans, inode.Number)
	if at < 0 {
		return nil
	}
	next := uint32(0)
	if at+1 < len(t.orphans) {
		next = t.orphans[at+1]
	}
	if at > 0 {
		previous, err := fs.liveInode(t.orphans[at-1])
		if err != nil {
			return err
		}
		putLE32(previous.raw[0x14:], next)
		previous.dirty = true
	}
	list := make([]uint32, 0, len(t.orphans)-1)
	list = append(list, t.orphans[:at]...)
	t.orphans = append(list, t.orphans[at+1:]...)
	t.orphansEdited = true
	putLE32(inode.raw[0x14:], 0)
	inode.dirty = true
	return nil
}

// loadOrphans reads the on-disk list into FS.orphans.
func (fs *FS) loadOrphans() error {
	fs.orphans = nil
	seen := make(map[uint32]bool)
	for number := fs.sb.LastOrphan; number != 0; {
		if number > fs.sb.InodesCount || number < fs.sb.FirstInode || seen[number] || len(seen) >= maxOrphanWalk {
			return corrupt("orphan list is malformed")
		}
		seen[number] = true
		inode, err := fs.orphanInode(number)
		if err != nil {
			return err
		}
		fs.orphans = append(fs.orphans, number)
		number = le32(inode.raw, 0x14)
	}
	return nil
}

// cleanupOrphans finishes the work of every listed inode, head first.
func (fs *FS) cleanupOrphans(report *Replay) error {
	for len(fs.orphans) > 0 {
		number := fs.orphans[0]
		err := fs.mutate(func() error {
			inode, err := fs.liveInode(number)
			if err != nil {
				return err
			}
			if err := fs.checkMutable(inode); err != nil {
				return err
			}
			if inode.Links == 0 {
				return fs.release(inode)
			}
			if !inode.IsRegular() {
				return corrupt("orphan list names a linked non-file")
			}
			return fs.trimTo(inode, inode.Size)
		})
		if err != nil {
			return err
		}
		report.Orphans++
	}
	return nil
}

// dropLastLink starts releasing an inode whose last link the running
// transaction removed. A pinned inode stays listed until Unpin.
func (fs *FS) dropLastLink(inode *Inode) error {
	fs.addOrphan(inode)
	if fs.pins[inode.Number] > 0 {
		return nil
	}
	return fs.release(inode)
}

// Pin records that a caller holds inode open. An inode whose last link
// goes while it is pinned keeps its blocks until the last Unpin. Pins live
// in memory: after a crash the writable mount releases such inodes from
// the orphan list.
func (fs *FS) Pin(given *Inode) error {
	if !fs.mutable {
		return fail(KindReadOnly, "volume is not open for mutation")
	}
	inode, err := fs.Inode(given.Number)
	if err != nil {
		return err
	}
	if inode.Generation != given.Generation {
		return fail(KindStale, "inode generation changed")
	}
	if fs.pins == nil {
		fs.pins = make(map[uint32]uint32)
	}
	fs.pins[given.Number]++
	return nil
}

// Unpin drops one pin and releases the inode when it was the last one and
// the inode has no links left.
func (fs *FS) Unpin(given *Inode) error {
	if fs.pins[given.Number] == 0 {
		return fail(KindInvalid, "inode is not pinned")
	}
	fs.pins[given.Number]--
	if fs.pins[given.Number] > 0 {
		return nil
	}
	delete(fs.pins, given.Number)
	if !fs.isOrphan(given.Number) {
		return nil
	}
	return fs.mutate(func() error {
		inode, err := fs.liveInode(given.Number)
		if err != nil {
			return err
		}
		if inode.Generation != given.Generation || inode.Links != 0 {
			return nil
		}
		return fs.release(inode)
	})
}

// HeldInode returns inode number as Inode does, and also while it has lost
// its last link but a caller still pins it: such an inode stays readable and
// writable until the last Unpin.
func (fs *FS) HeldInode(number uint32) (*Inode, error) {
	if fs.pins[number] != 0 && fs.isOrphan(number) {
		return fs.orphanInode(number)
	}
	return fs.Inode(number)
}
