package vfsservice

import (
	"goos/kernel/ramfs"
	"goos/userspace/fsproto"
)

// Transport carries one request to a filesystem service and brings back the
// reply with the same correlation. It reports false when the service could not
// be reached, did not answer within its deadline, or answered with something
// that does not decode; it never waits without bound.
type Transport interface {
	// Exchange sends request and returns the reply, or, when none usable
	// came, why: StatusIOError for a malformed reply, StatusTimeout for none
	// in time, StatusPeerDied for a service that has gone.
	Exchange(request fsproto.Request) (fsproto.Reply, ramfs.Status)
}

// Remote is a namespace backend served by another process (ADR 0096). The VFS
// keeps its node IDs in open files and in its mount table; the filesystem
// service owns what they mean. A transport failure — a timeout, a dead
// service, a garbled reply — is an error on this backend's mounts and nothing
// else, and keeps its reason: the VFS itself, and every other mount, keep
// working.
type Remote struct {
	transport Transport
	name      [ramfs.MaxName]byte
	// attributes caches the kind and size of recently seen nodes, so a stat
	// and a read at the end of a file cost no exchange. It is coherent because
	// the VFS is the service's only client and runs one request at a time:
	// every change to a node goes through this backend, which updates or
	// drops the entry (ADR 0096).
	attributes [attributeEntries]ramfs.Stat
	nextEntry  int
	// pages indexes the file cache, the memory this backend shares with the
	// service; every read and write moves its bytes there (ADR 0098).
	pages PageCache
}

// AttachCache gives the backend the file-cache mapping it shares with its
// service.
func (remote *Remote) AttachCache(memory []byte) bool { return remote.pages.Attach(memory) }

// attributeEntries bounds the attribute cache. A command touches a handful of
// nodes; eight covers a path walk and the file at its end.
const attributeEntries = 8

func (remote *Remote) cached(id ramfs.NodeID) (ramfs.Stat, bool) {
	for _, stat := range remote.attributes {
		if stat.ID == id && id != 0 {
			return stat, true
		}
	}
	return ramfs.Stat{}, false
}

func (remote *Remote) remember(stat ramfs.Stat) {
	if stat.ID == 0 || stat.Kind == 0 {
		return
	}
	for index := range remote.attributes {
		if remote.attributes[index].ID == stat.ID {
			remote.attributes[index] = stat
			return
		}
	}
	remote.attributes[remote.nextEntry] = stat
	remote.nextEntry = (remote.nextEntry + 1) % attributeEntries
}

func (remote *Remote) forget(id ramfs.NodeID) {
	for index := range remote.attributes {
		if remote.attributes[index].ID == id {
			remote.attributes[index] = ramfs.Stat{}
		}
	}
}

// forgetAll drops both caches: after a rename or unlink, which can move node
// IDs, and after any failure, which may mean the service is gone.
func (remote *Remote) forgetAll() {
	remote.attributes = [attributeEntries]ramfs.Stat{}
	remote.pages.ForgetAll()
}

func (remote *Remote) call(request fsproto.Request) (fsproto.Reply, ramfs.Status) {
	if remote.transport == nil {
		remote.forgetAll()
		return fsproto.Reply{}, ramfs.StatusPeerDied
	}
	reply, failure := remote.transport.Exchange(request)
	if failure == ramfs.StatusOK && reply.Operation != request.Operation {
		failure = ramfs.StatusIOError
	}
	if failure != ramfs.StatusOK {
		remote.forgetAll()
		return fsproto.Reply{}, failure
	}
	return reply, reply.Status
}

// looked records the attributes a lookup reply carried.
func (remote *Remote) looked(reply fsproto.Reply, status ramfs.Status) (ramfs.NodeID, ramfs.Status) {
	if status == ramfs.StatusOK {
		remote.remember(statOf(reply))
	}
	return reply.Node, status
}

func (remote *Remote) named(operation fsproto.Operation, node, parent ramfs.NodeID, name ramfs.Name) (fsproto.Reply, ramfs.Status) {
	size := name.CopyTo(remote.name[:])
	if size == 0 {
		return fsproto.Reply{}, ramfs.StatusInvalidArgument
	}
	return remote.call(fsproto.Request{Operation: operation, Node: node, Parent: parent, Name: remote.name[:size]})
}

// Root asks the service for the root of the volume it has mounted. A new
// service instance may be answering, so nothing cached is kept.
func (remote *Remote) Root() (ramfs.NodeID, ramfs.Status) {
	remote.forgetAll()
	reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationMount})
	return reply.Node, status
}

// Flush asks the service to make completed writes durable.
func (remote *Remote) Flush() bool {
	_, status := remote.call(fsproto.Request{Operation: fsproto.OperationFlush})
	return status == ramfs.StatusOK
}

func (remote *Remote) FilesystemInfo() (ramfs.FilesystemInfo, ramfs.Status) {
	reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationFilesystemInfo})
	if status != ramfs.StatusOK {
		return ramfs.FilesystemInfo{}, status
	}
	if reply.Filesystem.Type == 0 || !reply.Filesystem.Valid() {
		return ramfs.FilesystemInfo{}, ramfs.StatusIOError
	}
	return reply.Filesystem, ramfs.StatusOK
}

func (remote *Remote) OpenName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	return remote.looked(remote.named(fsproto.OperationOpen, 0, parent, name))
}

func (remote *Remote) AcquireObject(id ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationAcquire, Node: id})
	if status == ramfs.StatusOK && reply.Node == 0 {
		remote.forgetAll()
		return 0, ramfs.StatusIOError
	}
	if status == ramfs.StatusOK {
		if stat, known := remote.cached(id); known {
			stat.ID = reply.Node
			remote.remember(stat)
		}
		remote.pages.Rebind(id, reply.Node)
		// Mutations through the stable object update its cookie's attributes.
		// Do not leave a second, path-location keyed copy behind to go stale.
		remote.forget(id)
	}
	return reply.Node, status
}

func (remote *Remote) ReleaseObject(id ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationRelease, Node: id})
	if status != ramfs.StatusOK || reply.Node == 0 {
		remote.forget(id)
		remote.pages.Forget(id)
		if status == ramfs.StatusOK {
			status = ramfs.StatusIOError
		}
		return 0, status
	}
	if reply.Node == id {
		return id, status
	}
	if stat, known := remote.cached(id); known {
		stat.ID = reply.Node
		remote.remember(stat)
	}
	remote.forget(id)
	remote.pages.Rebind(id, reply.Node)
	return reply.Node, status
}

func (remote *Remote) CreateName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	// The parent's own entry may change with what it contains.
	remote.forget(parent)
	return remote.looked(remote.named(fsproto.OperationCreate, 0, parent, name))
}

func (remote *Remote) MkdirName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	// The parent's own entry may change with what it contains.
	remote.forget(parent)
	return remote.looked(remote.named(fsproto.OperationMkdir, 0, parent, name))
}

func (remote *Remote) RenameName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	remote.forgetAll()
	_, status := remote.named(fsproto.OperationRename, id, parent, name)
	return status
}

func (remote *Remote) ReplaceName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	remote.forgetAll()
	_, status := remote.named(fsproto.OperationReplace, id, parent, name)
	return status
}

func (remote *Remote) UnlinkName(parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	remote.forgetAll()
	_, status := remote.named(fsproto.OperationUnlink, 0, parent, name)
	return status
}

// Read serves target from the file cache, asking the service for each page it
// does not hold. A page shorter than a page is the end of the file.
func (remote *Remote) Read(id ramfs.NodeID, offset uint64, target []byte) (int, ramfs.Status) {
	// A read that starts at or past the end of a regular file of known size
	// is the end of the file, and needs no exchange to find that out.
	if stat, known := remote.cached(id); known && stat.Kind == ramfs.KindFile && offset >= stat.Size {
		return 0, ramfs.StatusOK
	}
	total := 0
	for total < len(target) {
		position := offset + uint64(total)
		within := position % fsproto.PageSize
		page := position / fsproto.PageSize
		// A miss with more of the read to come fetches the pages that follow
		// with it, as many as are missing and wanted, in one exchange.
		if pages := (within + uint64(len(target)-total) + fsproto.PageSize - 1) / fsproto.PageSize; pages > 1 && remote.pages.Ready() {
			if _, _, cached := remote.pages.Lookup(id, page); !cached {
				if status := remote.fill(id, page, pages); status != ramfs.StatusOK {
					return total, status
				}
			}
		}
		slot, length, status := remote.page(id, page)
		if status != ramfs.StatusOK {
			return total, status
		}
		if within >= length {
			break
		}
		copied := copy(target[total:], remote.pages.Slot(slot)[within:length])
		total += copied
		if length < fsproto.PageSize && within+uint64(copied) == length {
			// The page ends the file, and the read has reached its end.
			break
		}
	}
	return total, ramfs.StatusOK
}

// maxRun is the most pages one exchange with the service moves.
const maxRun = 16

// fill reads up to wanted pages of id from page on, stopping at the first one
// already cached, into a run of slots in one exchange. Pages past the end of
// the file are not filled.
func (remote *Remote) fill(id ramfs.NodeID, page uint64, wanted uint64) ramfs.Status {
	count := uint64(1)
	for count < wanted && count < maxRun {
		if _, _, cached := remote.pages.Lookup(id, page+count); cached {
			break
		}
		count++
	}
	start := remote.pages.ClaimRun(int(count))
	reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationRead, Node: id, Offset: page * fsproto.PageSize, Size: uint32(count * fsproto.PageSize), Slot: uint32(start)})
	if status != ramfs.StatusOK {
		return status
	}
	if reply.Value > count*fsproto.PageSize {
		remote.forgetAll()
		return ramfs.StatusIOError
	}
	for index := uint64(0); index < count && index*fsproto.PageSize < reply.Value; index++ {
		remote.pages.Fill(start+int(index), id, page+index, min(fsproto.PageSize, reply.Value-index*fsproto.PageSize))
	}
	return ramfs.StatusOK
}

// page returns the slot holding page of id, filling one from the service on a
// miss.
func (remote *Remote) page(id ramfs.NodeID, page uint64) (int, uint64, ramfs.Status) {
	if !remote.pages.Ready() {
		return 0, 0, ramfs.StatusIOError
	}
	if slot, length, found := remote.pages.Lookup(id, page); found {
		return slot, length, ramfs.StatusOK
	}
	slot := remote.pages.Victim()
	reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationRead, Node: id, Offset: page * fsproto.PageSize, Size: fsproto.PageSize, Slot: uint32(slot)})
	if status != ramfs.StatusOK {
		return 0, 0, status
	}
	if reply.Value > fsproto.PageSize {
		remote.forgetAll()
		return 0, 0, ramfs.StatusIOError
	}
	remote.pages.Fill(slot, id, page, reply.Value)
	return slot, reply.Value, ramfs.StatusOK
}

// Write sends data a page at a time from the file cache. A page the cache
// holds is updated in place and written from its slot; any other is written
// from the scratch slot and not cached. The service writes each piece through
// before it answers, so a write that returns is on the service's side, and an
// error is reported by the write that caused it. A short write ends it.
func (remote *Remote) Write(id ramfs.NodeID, offset uint64, data []byte) (int, ramfs.Status) {
	if !remote.pages.Ready() {
		return 0, ramfs.StatusIOError
	}
	total := 0
	for total < len(data) {
		position := offset + uint64(total)
		page, within := position/fsproto.PageSize, position%fsproto.PageSize
		if within+uint64(len(data)-total) > fsproto.PageSize {
			written, status := remote.writeRun(id, position, data[total:])
			total += written
			if status != ramfs.StatusOK {
				return total, status
			}
			if written == 0 {
				break
			}
			continue
		}
		chunk := min(uint64(len(data)-total), fsproto.PageSize-within)
		slot, length, found := remote.pages.Lookup(id, page)
		if !found {
			slot = scratchSlot
		}
		copy(remote.pages.Slot(slot)[within:within+chunk], data[total:uint64(total)+chunk])
		reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationWrite, Node: id, Offset: position, Size: uint32(chunk), Slot: uint32(slot)})
		if status == ramfs.StatusOK && reply.Value > chunk {
			remote.forgetAll()
			status = ramfs.StatusIOError
		}
		if status != ramfs.StatusOK {
			// The slot holds bytes the file may not.
			remote.pages.Forget(id)
			remote.forget(id)
			return total, status
		}
		// A write elsewhere may have moved the end of the file past a page
		// that used to end it.
		keep := -1
		if found {
			keep = slot
			if reply.Value < chunk {
				remote.pages.Drop(slot)
				keep = -1
			} else {
				remote.pages.Written(slot, length, within, reply.Value)
			}
		}
		remote.pages.DropPartial(id, keep)
		total += int(reply.Value)
		if reply.Value < chunk {
			break
		}
	}
	// Keep the known size so an open handle can still recognize EOF. The
	// service owns the new modification time, so mark only that field unknown;
	// a later path stat obtains the authoritative value.
	if total != 0 {
		if stat, known := remote.cached(id); known {
			if end := offset + uint64(total); end > stat.Size {
				stat.Size = end
			}
			stat.Modified = ramfs.Timestamp{}
			remote.remember(stat)
		}
	}
	return total, ramfs.StatusOK
}

// writeRun writes the part of data that fits a run of up to maxRun pages from
// position in one exchange. The pages it wholly covers stay cached with what
// was written; partial ones and any stale copies are dropped.
func (remote *Remote) writeRun(id ramfs.NodeID, position uint64, data []byte) (int, ramfs.Status) {
	page, within := position/fsproto.PageSize, position%fsproto.PageSize
	count := min(uint64(maxRun), (within+uint64(len(data))+fsproto.PageSize-1)/fsproto.PageSize)
	chunk := min(uint64(len(data)), count*fsproto.PageSize-within)
	remote.pages.ForgetRange(id, page, page+count-1)
	start := remote.pages.ClaimRun(int(count))
	run := remote.pages.Run(start, int(count))
	copy(run[within:within+chunk], data[:chunk])
	reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationWrite, Node: id, Offset: position, Size: uint32(chunk), Slot: uint32(start)})
	if status == ramfs.StatusOK && reply.Value > chunk {
		remote.forgetAll()
		status = ramfs.StatusIOError
	}
	if status != ramfs.StatusOK {
		remote.pages.Forget(id)
		remote.forget(id)
		return 0, status
	}
	end := within + reply.Value
	for index := uint64(0); index < count; index++ {
		if index*fsproto.PageSize >= within && (index+1)*fsproto.PageSize <= end {
			remote.pages.Fill(start+int(index), id, page+index, fsproto.PageSize)
		}
	}
	remote.pages.DropPartial(id, -1)
	return int(reply.Value), ramfs.StatusOK
}

func (remote *Remote) Truncate(id ramfs.NodeID, size uint64) ramfs.Status {
	remote.pages.Forget(id)
	_, status := remote.call(fsproto.Request{Operation: fsproto.OperationTruncate, Node: id, Offset: size})
	remote.forget(id)
	return status
}

func (remote *Remote) Stat(id ramfs.NodeID) (ramfs.Stat, ramfs.Status) {
	if stat, known := remote.cached(id); known {
		return stat, ramfs.StatusOK
	}
	reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationStat, Node: id})
	if status != ramfs.StatusOK {
		return ramfs.Stat{}, status
	}
	stat := statOf(reply)
	remote.remember(stat)
	return stat, ramfs.StatusOK
}

func (remote *Remote) ReadDir(parent ramfs.NodeID, cursor int) (ramfs.Entry, int, ramfs.Status) {
	if cursor < 0 || uint64(cursor) > uint64(^uint32(0)) {
		return ramfs.Entry{}, 0, ramfs.StatusInvalidArgument
	}
	reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationReadDir, Parent: parent, Size: uint32(cursor)})
	if status != ramfs.StatusOK {
		return ramfs.Entry{}, 0, status
	}
	var entry ramfs.Entry
	if len(reply.Name) == 0 || len(reply.Name) > len(entry.Name) {
		return ramfs.Entry{}, 0, ramfs.StatusIOError
	}
	entry.Stat = statOf(reply)
	entry.Size = uint16(copy(entry.Name[:], reply.Name))
	return entry, int(reply.Cursor), ramfs.StatusOK
}

func statOf(reply fsproto.Reply) ramfs.Stat {
	return ramfs.Stat{ID: reply.Node, Kind: reply.Kind, Size: reply.Value,
		Created: reply.Created, Modified: reply.Modified, Attributes: reply.Attributes,
		AttributesAvailable: reply.AttributesAvailable}
}
