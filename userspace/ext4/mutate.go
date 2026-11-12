package ext4

// The mutation operations. Each one runs as one transaction, or as a
// sequence of them when it moves more data or frees more blocks than one
// holds: a write commits a prefix of its data, a truncate frees from the
// end one extent per step. Every step reserves the metadata blocks it may
// stage; staging more is an internal error rather than a silent overrun,
// which keeps the estimates below honest for the journal that will size
// its transactions from them.
//
// These operations only run on volumes mounted with mountScratch. They do
// not keep an orphan list. Every path that drops an inode's last link
// (Unlink, Rmdir, and Rename over a last link) removes the name first and
// then frees the inode's blocks in release, whose steps may each commit;
// so does a Truncate across transactions. Stopping between those commits
// leaves an unreachable inode, or a short file, with blocks still
// allocated.

// Metadata credits, in blocks, at most depth-5 extent trees and 4 KiB
// blocks. A descriptor block is counted with every bitmap even when two
// share one.
const (
	// creditsAllocRun: the bitmap and descriptor of the group a run comes
	// from. A run never crosses a group.
	creditsAllocRun = 2
	// creditsFreeBlock: one block's bitmap and descriptor.
	creditsFreeBlock = 2
	// creditsFreeRun: an extent of up to 32768 blocks spans at most two
	// groups.
	creditsFreeRun = 2 * creditsFreeBlock
	// creditsExtentEdit: the path below the root, plus a new node and its
	// allocation at every level when each one splits and the root grows.
	creditsExtentEdit = extentMaxDepth + (extentMaxDepth+1)*(1+creditsAllocRun)
	// creditsDirInsert: the leaf, its index node, and a new leaf with its
	// allocation and the directory's extent edit.
	creditsDirInsert = 2 + 1 + creditsAllocRun + creditsExtentEdit
	// creditsInodeAlloc: inode bitmap, block bitmap of a group initialized
	// on first use, and the descriptor.
	creditsInodeAlloc = 3
	creditsCreate     = creditsInodeAlloc + creditsDirInsert
	// creditsMkdir and creditsSymlink add the new object's first block.
	creditsMkdir   = creditsCreate + 1 + creditsAllocRun
	creditsSymlink = creditsMkdir
	creditsLink    = creditsDirInsert
	// creditsRemove: the leaf holding the name.
	creditsRemove = 1
	// creditsRename: the insert, the removal, the replaced record and the
	// moved directory's first block.
	creditsRename = creditsDirInsert + 3
	// creditsWriteStep: one run allocated and mapped.
	creditsWriteStep = creditsAllocRun + creditsExtentEdit
	// creditsTrimStep: one extent freed, the path, every node on it freed
	// as it empties, and the root collapsing level by level.
	creditsTrimStep = creditsFreeRun + extentMaxDepth + extentMaxDepth*creditsFreeBlock +
		extentMaxDepth*(1+creditsFreeBlock)
	// creditsFreeInode: inode bitmap, descriptor, and the attribute block
	// with its bitmap and descriptor.
	creditsFreeInode = 2 + 1 + creditsFreeBlock
	// txnHeadroom is kept free for the inode table blocks and superblock
	// commit adds.
	txnHeadroom = 8
)

// mountScratch mounts a volume for mutation. It exists for host tests and
// tools working on disposable images; nothing in a GoOS service calls it,
// and Mount still refuses read-write while writeSupported is false. The
// volume must match the writable profile, use 4 KiB blocks, and carry free
// counts that agree with its descriptors and their bitmaps.
func mountScratch(device Device, now func() Timestamp) (*FS, error) {
	fs, err := Mount(device, Options{})
	if err != nil {
		return nil, err
	}
	if fs.negotiation.Support != ReadWrite {
		return nil, fail(KindReadOnly, fs.negotiation.Reason)
	}
	if fs.blockSize != 4096 {
		return nil, fail(KindUnsupported, "mutation needs 4 KiB blocks")
	}
	var freeBlocks uint64
	var freeInodes uint32
	for group := uint32(0); uint64(group) < fs.groups; group++ {
		desc, err := fs.group(group)
		if err != nil {
			return nil, err
		}
		freeBlocks += uint64(desc.FreeBlocks)
		freeInodes += desc.FreeInodes
	}
	if freeBlocks != fs.sb.FreeBlocks || freeInodes != fs.sb.FreeInodes {
		return nil, corrupt("superblock free counts disagree with the descriptors")
	}
	if err := fs.verifyGroupCounts(); err != nil {
		return nil, err
	}
	fs.mutable, fs.now, fs.state = true, now, StateReadyRW
	return fs, nil
}

func fileTypeOf(mode uint16) uint8 {
	switch mode & modeTypeMask {
	case ModeRegular:
		return FileTypeRegular
	case ModeDir:
		return FileTypeDir
	case ModeSymlink:
		return FileTypeSymlink
	case ModeFIFO:
		return FileTypeFIFO
	case ModeSocket:
		return FileTypeSocket
	case ModeChar:
		return FileTypeChar
	case ModeBlock:
		return FileTypeBlock
	}
	return FileTypeUnknown
}

// openDir returns the live, mutable copy of dir.
func (fs *FS) openDir(given *Inode) (*Inode, error) {
	dir, err := fs.liveMatch(given)
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() {
		return nil, fail(KindNotDirectory, "")
	}
	if err := fs.checkMutable(dir); err != nil {
		return nil, err
	}
	if fs.isIndexed(dir) {
		root, err := fs.dirBlock(dir, 0)
		if err != nil {
			return nil, err
		}
		if root == nil {
			return nil, corrupt("hole in an indexed directory")
		}
		version, _, err := fs.dxRoot(dir, root)
		if err != nil {
			return nil, err
		}
		if _, ok := dirHash(nil, version, &fs.sb.HashSeed); !ok {
			return nil, fail(KindUnsupported, "directory hash version")
		}
	}
	return dir, nil
}

// checkNewName refuses a name that cannot be created or already exists.
func (fs *FS) checkNewName(dir *Inode, name []byte) error {
	if err := checkName(name); err != nil {
		return err
	}
	if isDotName(name) {
		return fail(KindExists, "")
	}
	_, err := fs.findEntry(dir, name)
	switch KindOf(err) {
	case 0:
		return fail(KindExists, "")
	case KindNotFound:
		return nil
	}
	return err
}

// fresh rereads an inode after its operation committed.
func (fs *FS) fresh(number uint32, err error) (*Inode, error) {
	if err != nil {
		return nil, err
	}
	return fs.Inode(number)
}

// Create makes a regular file, FIFO or socket named name in dir. mode
// carries the type and permission bits.
func (fs *FS) Create(dir *Inode, name []byte, mode uint16, uid uint32, gid uint32) (*Inode, error) {
	switch mode & modeTypeMask {
	case ModeRegular, ModeFIFO, ModeSocket:
	default:
		return nil, fail(KindInvalid, "create makes files, FIFOs and sockets")
	}
	var number uint32
	err := fs.mutate(func() error {
		parent, err := fs.openDir(dir)
		if err != nil {
			return err
		}
		if err := fs.checkNewName(parent, name); err != nil {
			return err
		}
		if _, err := fs.reserve(creditsCreate, 0); err != nil {
			return err
		}
		if number, err = fs.allocInode(parent.Number, false); err != nil {
			return err
		}
		inode, err := fs.newInode(number, mode, uid, gid)
		if err != nil {
			return err
		}
		inode.Links = 1
		if mode&modeTypeMask == ModeRegular {
			initExtentRoot(inode)
		}
		if err := fs.addEntry(parent, name, number, fileTypeOf(mode)); err != nil {
			return err
		}
		fs.touch(parent, "mc")
		return nil
	})
	return fs.fresh(number, err)
}

// Mkdir makes a directory named name in dir.
func (fs *FS) Mkdir(dir *Inode, name []byte, perm uint16, uid uint32, gid uint32) (*Inode, error) {
	var number uint32
	err := fs.mutate(func() error {
		parent, err := fs.openDir(dir)
		if err != nil {
			return err
		}
		if err := fs.checkNewName(parent, name); err != nil {
			return err
		}
		if parent.Links >= maxLinks && fs.sb.Features.ROCompat&roCompatDirNlink == 0 {
			return fail(KindTooManyLinks, "")
		}
		if _, err := fs.reserve(creditsMkdir, 0); err != nil {
			return err
		}
		if number, err = fs.allocInode(parent.Number, true); err != nil {
			return err
		}
		child, err := fs.newInode(number, ModeDir|perm&^modeTypeMask, uid, gid)
		if err != nil {
			return err
		}
		child.Links = 2
		initExtentRoot(child)
		_, data, err := fs.growDir(child)
		if err != nil {
			return err
		}
		fs.formatDir(child, data, parent.Number)
		if err := fs.addEntry(parent, name, number, FileTypeDir); err != nil {
			return err
		}
		if err := fs.incLinks(parent); err != nil {
			return err
		}
		fs.touch(parent, "mc")
		return nil
	})
	return fs.fresh(number, err)
}

// Symlink makes a symbolic link named name in dir pointing at target. A
// target shorter than the inode's block area is stored inline, as Linux
// does.
func (fs *FS) Symlink(dir *Inode, name []byte, target []byte, uid uint32, gid uint32) (*Inode, error) {
	if len(target) == 0 || len(target) > maxSymlink {
		return nil, fail(KindNameTooLong, "symlink target length")
	}
	for _, value := range target {
		if value == 0 {
			return nil, fail(KindInvalid, "symlink target holds NUL")
		}
	}
	var number uint32
	err := fs.mutate(func() error {
		parent, err := fs.openDir(dir)
		if err != nil {
			return err
		}
		if err := fs.checkNewName(parent, name); err != nil {
			return err
		}
		if _, err := fs.reserve(creditsSymlink, 0); err != nil {
			return err
		}
		if number, err = fs.allocInode(parent.Number, false); err != nil {
			return err
		}
		link, err := fs.newInode(number, ModeSymlink|0o777, uid, gid)
		if err != nil {
			return err
		}
		link.Links = 1
		link.Size = uint64(len(target))
		if len(target) < inodeBlockSize {
			copy(link.blockArea[:], target)
		} else {
			initExtentRoot(link)
			block, _, err := fs.allocBlocks(fs.inodeGoal(link), 1)
			if err != nil {
				return err
			}
			if err := fs.insertExtent(link, extent{length: 1, physical: block}); err != nil {
				return err
			}
			link.Blocks512 += uint64(fs.blockSize / 512)
			data, err := fs.stageData(block, false)
			if err != nil {
				return err
			}
			copy(data, target)
		}
		if err := fs.addEntry(parent, name, number, FileTypeSymlink); err != nil {
			return err
		}
		fs.touch(parent, "mc")
		return nil
	})
	return fs.fresh(number, err)
}

// Link adds name in dir as another link to target, which must not be a
// directory.
func (fs *FS) Link(dir *Inode, name []byte, target *Inode) error {
	return fs.mutate(func() error {
		parent, err := fs.openDir(dir)
		if err != nil {
			return err
		}
		inode, err := fs.liveMatch(target)
		if err != nil {
			return err
		}
		if inode.IsDir() {
			return fail(KindIsDirectory, "")
		}
		if err := fs.checkMutable(inode); err != nil {
			return err
		}
		if err := fs.checkNewName(parent, name); err != nil {
			return err
		}
		if _, err := fs.reserve(creditsLink, 0); err != nil {
			return err
		}
		if err := fs.incLinks(inode); err != nil {
			return err
		}
		if err := fs.addEntry(parent, name, inode.Number, fileTypeOf(inode.Mode)); err != nil {
			return err
		}
		fs.touch(inode, "c")
		fs.touch(parent, "mc")
		return nil
	})
}

// checkReleasable refuses an inode whose blocks the mutation code could not
// free.
func (fs *FS) checkReleasable(inode *Inode) error {
	if err := fs.checkMutable(inode); err != nil {
		return err
	}
	eaBlocks := uint64(0)
	if inode.FileACL != 0 {
		eaBlocks = uint64(fs.blockSize / 512)
	}
	if inode.Flags&inodeFlagExtents == 0 && inode.Blocks512 != eaBlocks {
		return fail(KindUnsupported, "block-mapped inode")
	}
	return nil
}

// release frees an inode whose last link went away: its blocks, from the
// end, its attribute block reference, and its bitmap bit.
func (fs *FS) release(inode *Inode) error {
	inode.Size = 0
	inode.dirty = true
	if inode.Flags&inodeFlagExtents != 0 {
		for {
			if _, err := fs.reserve(creditsTrimStep, 0); err != nil {
				return err
			}
			done, err := fs.trimStep(inode, 0)
			if err != nil {
				return err
			}
			if done {
				break
			}
		}
	}
	if _, err := fs.reserve(creditsFreeInode, 0); err != nil {
		return err
	}
	if inode.FileACL != 0 {
		if err := fs.dropXattrBlock(inode); err != nil {
			return err
		}
	}
	return fs.freeInode(inode)
}

// Unlink removes name from dir; the inode goes when its last link does.
func (fs *FS) Unlink(dir *Inode, name []byte) error {
	return fs.mutate(func() error {
		parent, err := fs.openDir(dir)
		if err != nil {
			return err
		}
		loc, err := fs.findEntry(parent, name)
		if err != nil {
			return err
		}
		inode, err := fs.liveInode(loc.inode)
		if err != nil {
			return err
		}
		if inode.IsDir() {
			return fail(KindIsDirectory, "")
		}
		if err := fs.checkReleasable(inode); err != nil {
			return err
		}
		if _, err := fs.reserve(creditsRemove, 0); err != nil {
			return err
		}
		if err := fs.removeEntry(parent, loc); err != nil {
			return err
		}
		fs.touch(parent, "mc")
		inode.Links--
		fs.touch(inode, "c")
		if inode.Links == 0 {
			return fs.release(inode)
		}
		return nil
	})
}

// Rmdir removes the empty directory name from dir.
func (fs *FS) Rmdir(dir *Inode, name []byte) error {
	return fs.mutate(func() error {
		parent, err := fs.openDir(dir)
		if err != nil {
			return err
		}
		loc, err := fs.findEntry(parent, name)
		if err != nil {
			return err
		}
		child, err := fs.liveInode(loc.inode)
		if err != nil {
			return err
		}
		if !child.IsDir() {
			return fail(KindNotDirectory, "")
		}
		if err := fs.checkReleasable(child); err != nil {
			return err
		}
		empty, err := fs.dirEmpty(child)
		if err != nil {
			return err
		}
		if !empty {
			return fail(KindNotEmpty, "")
		}
		if _, err := fs.reserve(creditsRemove, 0); err != nil {
			return err
		}
		if err := fs.removeEntry(parent, loc); err != nil {
			return err
		}
		decDirLinks(parent)
		fs.touch(parent, "mc")
		child.Links = 0
		return fs.release(child)
	})
}

// maxAncestors bounds the walk up from a rename's target directory: a path
// of at most 4095 bytes has fewer components.
const maxAncestors = 2048

// isAncestor reports whether directory number ancestor lies on the path
// from dir up to the root.
func (fs *FS) isAncestor(ancestor uint32, dir *Inode) (bool, error) {
	current := dir
	for steps := 0; steps < maxAncestors; steps++ {
		if current.Number == ancestor {
			return true, nil
		}
		if current.Number == RootInode {
			return false, nil
		}
		parent, err := fs.dotDot(current)
		if err != nil {
			return false, err
		}
		if current, err = fs.liveInode(parent); err != nil {
			return false, err
		}
		if !current.IsDir() {
			return false, corrupt("dot-dot names a non-directory")
		}
	}
	return false, corrupt("directory ancestry too deep")
}

// Rename moves fromName in fromDir to toName in toDir, replacing what
// toName named. A directory can replace only an empty directory, and
// cannot move below itself. Renaming a name onto another link of the same
// inode changes nothing.
func (fs *FS) Rename(fromDir *Inode, fromName []byte, toDir *Inode, toName []byte) error {
	return fs.mutate(func() error {
		source, err := fs.openDir(fromDir)
		if err != nil {
			return err
		}
		target, err := fs.openDir(toDir)
		if err != nil {
			return err
		}
		from, err := fs.findEntry(source, fromName)
		if err != nil {
			return err
		}
		if err := checkName(toName); err != nil {
			return err
		}
		if isDotName(toName) {
			return fail(KindInvalid, "dot names cannot be changed")
		}
		moved, err := fs.liveInode(from.inode)
		if err != nil {
			return err
		}
		if err := fs.checkMutable(moved); err != nil {
			return err
		}
		to, err := fs.findEntry(target, toName)
		exists := err == nil
		if err != nil && KindOf(err) != KindNotFound {
			return err
		}
		if exists && to.inode == from.inode {
			return nil
		}
		crossing := moved.IsDir() && source.Number != target.Number
		if crossing {
			below, err := fs.isAncestor(moved.Number, target)
			if err != nil {
				return err
			}
			if below {
				return fail(KindInvalid, "directory moved below itself")
			}
		}
		var victim *Inode
		if exists {
			if victim, err = fs.liveInode(to.inode); err != nil {
				return err
			}
			if err := fs.checkReleasable(victim); err != nil {
				return err
			}
			switch {
			case moved.IsDir() && !victim.IsDir():
				return fail(KindNotDirectory, "")
			case !moved.IsDir() && victim.IsDir():
				return fail(KindIsDirectory, "")
			case victim.IsDir():
				empty, err := fs.dirEmpty(victim)
				if err != nil {
					return err
				}
				if !empty {
					return fail(KindNotEmpty, "")
				}
			}
		} else if crossing && target.Links >= maxLinks && fs.sb.Features.ROCompat&roCompatDirNlink == 0 {
			return fail(KindTooManyLinks, "")
		}
		if _, err := fs.reserve(creditsRename, 0); err != nil {
			return err
		}
		fileType := fileTypeOf(moved.Mode)
		if exists {
			err = fs.retarget(target, to, moved.Number, fileType)
		} else {
			err = fs.addEntry(target, toName, moved.Number, fileType)
		}
		if err != nil {
			return err
		}
		// The insert may have split or repacked the block holding the old
		// name, so find it again.
		if from, err = fs.findEntry(source, fromName); err != nil {
			return err
		}
		if from.inode != moved.Number {
			return corrupt("renamed entry changed under the rename")
		}
		if err := fs.removeEntry(source, from); err != nil {
			return err
		}
		if crossing {
			if err := fs.setDotDot(moved, target.Number); err != nil {
				return err
			}
			decDirLinks(source)
			if err := fs.incLinks(target); err != nil {
				return err
			}
		}
		fs.touch(moved, "c")
		fs.touch(source, "mc")
		fs.touch(target, "mc")
		if victim == nil {
			return nil
		}
		if victim.IsDir() {
			decDirLinks(target)
			victim.Links = 0
		} else {
			victim.Links--
		}
		fs.touch(victim, "c")
		if victim.Links == 0 {
			return fs.release(victim)
		}
		return nil
	})
}

// maxFileBlocks is the logical block count an extent tree can map.
const maxFileBlocks = uint64(maxLogicalBlock) + 1

func (fs *FS) checkFileSize(size uint64) error {
	if size > maxFileBlocks*uint64(fs.blockSize) {
		return fail(KindNoSpace, "file larger than an extent tree maps")
	}
	if size > 0x7FFFFFFF && fs.sb.Features.ROCompat&roCompatLargeFile == 0 {
		return fail(KindUnsupported, "file above 2 GiB without large_file")
	}
	return nil
}

// openFile returns the live, mutable copy of a regular file.
func (fs *FS) openFile(given *Inode) (*Inode, error) {
	inode, err := fs.liveMatch(given)
	if err != nil {
		return nil, err
	}
	if inode.IsDir() {
		return nil, fail(KindIsDirectory, "")
	}
	if !inode.IsRegular() {
		return nil, fail(KindInvalid, "not a regular file")
	}
	if err := fs.checkMutable(inode); err != nil {
		return nil, err
	}
	return inode, nil
}

// zeroTail clears the bytes from offset to the end of its block, so data
// past end of file reads as zeros once the file grows over it.
func (fs *FS) zeroTail(inode *Inode, offset uint64) error {
	within := offset % uint64(fs.blockSize)
	if within == 0 {
		return nil
	}
	mapping, err := fs.mapBlock(inode, offset/uint64(fs.blockSize))
	if err != nil || mapping.Hole || mapping.Unwritten {
		return err
	}
	if _, err := fs.reserve(0, 1); err != nil {
		return err
	}
	data, err := fs.stageData(mapping.Physical, true)
	if err != nil {
		return err
	}
	for index := within; index < uint64(len(data)); index++ {
		data[index] = 0
	}
	return nil
}

// dataGoal places new file data after the block before it, or at the start
// of the inode's group.
func (fs *FS) dataGoal(inode *Inode, logical uint64) uint64 {
	if logical > 0 {
		if previous, err := fs.mapBlock(inode, logical-1); err == nil && !previous.Hole {
			return previous.Physical + 1
		}
	}
	return fs.inodeGoal(inode)
}

// WriteFile writes data at offset, allocating blocks for holes and turning
// unwritten extents written. It returns the bytes written; on failure that
// is the prefix whose transactions committed.
func (fs *FS) WriteFile(file *Inode, offset uint64, data []byte) (int, error) {
	end, ok := addOK(offset, uint64(len(data)))
	if !ok {
		return 0, fail(KindInvalid, "write past the largest offset")
	}
	if err := fs.checkFileSize(end); err != nil {
		return 0, err
	}
	committed := 0
	err := fs.mutate(func() error {
		inode, err := fs.openFile(file)
		if err != nil {
			return err
		}
		if offset > inode.Size {
			if err := fs.zeroTail(inode, inode.Size); err != nil {
				return err
			}
		}
		blockSize := uint64(fs.blockSize)
		done := 0
		for done < len(data) {
			position := offset + uint64(done)
			logical := position / blockSize
			within := position % blockSize
			mapping, err := fs.mapBlock(inode, logical)
			if err != nil {
				return err
			}
			need := (within + uint64(len(data)-done) + blockSize - 1) / blockSize
			run := mapping.Length
			if run > need {
				run = need
			}
			if run > maxTxnData {
				run = maxTxnData
			}
			fresh := mapping.Hole || mapping.Unwritten
			meta := 0
			if fresh {
				meta = creditsWriteStep
			}
			if restarted, err := fs.reserve(meta, int(run)); err != nil {
				return err
			} else if restarted {
				committed = done
			}
			physical := mapping.Physical
			switch {
			case mapping.Hole:
				start, got, err := fs.allocBlocks(fs.dataGoal(inode, logical), run)
				if err != nil {
					return err
				}
				run, physical = got, start
				if err := fs.insertExtent(inode, extent{logical: uint32(logical), length: uint32(run), physical: start}); err != nil {
					return err
				}
				inode.Blocks512 += run * (blockSize / 512)
			case mapping.Unwritten:
				if err := fs.insertExtent(inode, extent{logical: uint32(logical), length: uint32(run), physical: physical}); err != nil {
					return err
				}
			}
			for index := uint64(0); index < run && done < len(data); index++ {
				from := uint64(0)
				if index == 0 {
					from = within
				}
				count := blockSize - from
				if rest := uint64(len(data) - done); count > rest {
					count = rest
				}
				keep := !fresh && count != blockSize
				block, err := fs.stageData(physical+index, keep)
				if err != nil {
					return err
				}
				copy(block[from:from+count], data[done:])
				done += int(count)
			}
			if end := offset + uint64(done); end > inode.Size {
				inode.Size = end
			}
			fs.touch(inode, "mc")
		}
		return nil
	})
	if err != nil {
		return committed, err
	}
	return len(data), nil
}

// Truncate sets the size of a regular file. Growing leaves a hole; shrinking
// frees the blocks past the new end and clears the rest of the last block.
func (fs *FS) Truncate(file *Inode, size uint64) error {
	if err := fs.checkFileSize(size); err != nil {
		return err
	}
	return fs.mutate(func() error {
		inode, err := fs.openFile(file)
		if err != nil {
			return err
		}
		if size == inode.Size {
			return nil
		}
		if size > inode.Size {
			if err := fs.zeroTail(inode, inode.Size); err != nil {
				return err
			}
			inode.Size = size
			fs.touch(inode, "mc")
			return nil
		}
		if err := fs.zeroTail(inode, size); err != nil {
			return err
		}
		inode.Size = size
		fs.touch(inode, "mc")
		blockSize := uint64(fs.blockSize)
		end := uint32((size + blockSize - 1) / blockSize)
		for {
			if _, err := fs.reserve(creditsTrimStep, 0); err != nil {
				return err
			}
			done, err := fs.trimStep(inode, end)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	})
}
