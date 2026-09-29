package ramfs

// RAMFS keeps files in pages (ADR 0105). A file's bytes live in pages located
// through its node the way a classic inode locates blocks: twelve direct
// references, one page of references, and one page of pages of references.
// A page is allocated only when bytes are written to it; a page never written
// is a hole and reads as zeros, so extending a file costs nothing until it is
// written. Truncation and removal hand every page they no longer need back to
// the store.
//
// The node table grows a chunk at a time up to its bound. Both bounds — nodes
// and pages — are limits of the instance, set when it is configured, and a
// request past either is refused rather than allowed to take the memory of
// the process serving it.

const (
	chunkNodes = 64
	maxChunks  = 1024
	// MaxNodes is the most nodes any instance can be configured for.
	MaxNodes = chunkNodes * maxChunks

	directPages = 12
	refsPerPage = PageSize / 4
	indirectEnd = directPages + refsPerPage
	doubleEnd   = indirectEnd + refsPerPage*refsPerPage
	// MaxFileSize is the most a file can index: a little over 4 GiB.
	MaxFileSize = uint64(doubleEnd) * PageSize

	// DefaultNodes and DefaultPages are the limits of an instance nothing
	// configured: 4096 nodes and 64 MiB of pages.
	DefaultNodes = 4096
	DefaultPages = 16384
)

type node struct {
	parent     NodeID
	size       uint64
	generation uint32
	kind       Kind
	used       bool
	created    Timestamp
	modified   Timestamp
	nameSize   uint16
	name       [MaxName]byte
	// firstChild and next link a directory's children in ascending index
	// order, as index+1; zero ends the list.
	firstChild uint32
	next       uint32
	direct     [directPages]PageRef
	indirect   PageRef
	double     PageRef
}

// NodeChunk is one allocation unit of the node table.
type NodeChunk [chunkNodes]node

// Limits bound an instance.
type Limits struct {
	Nodes int
	Pages uint64
}

// Usage is what an instance holds against its limits.
type Usage struct {
	Nodes, MaxNodes int
	Pages, MaxPages uint64
}

type FileSystem struct {
	store     Store
	chunks    [maxChunks]*NodeChunk
	allocated int
	limits    Limits
	nodes     int
	pages     uint64
	hint      int
	ready     bool
	now       func() (Timestamp, bool)
}

// SetClock supplies UTC time for metadata. It may be set before or after Init;
// existing unavailable timestamps are not fabricated retroactively.
func (fs *FileSystem) SetClock(clock func() (Timestamp, bool)) { fs.now = clock }

func (fs *FileSystem) timestamp() Timestamp {
	if fs.now == nil {
		return Timestamp{}
	}
	stamp, ok := fs.now()
	if !ok || !stamp.Available || !stamp.Valid() {
		return Timestamp{}
	}
	return stamp
}

func touch(entry *node, stamp Timestamp) {
	if stamp.Available {
		entry.modified = stamp
	}
}

// Configure sets the store and limits of an instance that has not been
// initialized. Nodes is capped at MaxNodes.
func (fs *FileSystem) Configure(store Store, limits Limits) bool {
	if fs.ready || store == nil || limits.Nodes < 1 || limits.Pages == 0 {
		return false
	}
	if limits.Nodes > MaxNodes {
		limits.Nodes = MaxNodes
	}
	fs.store, fs.limits = store, limits
	return true
}

// Init creates the root directory, configuring the default store and limits
// first if Configure was not called. A second call returns the existing root.
func (fs *FileSystem) Init() NodeID {
	if fs.ready {
		return fs.id(0)
	}
	if fs.store == nil {
		fs.store, fs.limits = &heapStore{}, Limits{Nodes: DefaultNodes, Pages: DefaultPages}
	}
	chunk := fs.store.Chunk()
	if chunk == nil {
		return 0
	}
	fs.chunks[0], fs.allocated = chunk, 1
	root := fs.at(0)
	stamp := fs.timestamp()
	root.generation, root.kind, root.used, root.created, root.modified = 1, KindDirectory, true, stamp, stamp
	fs.nodes, fs.ready = 1, true
	return fs.id(0)
}

func (fs *FileSystem) Root() NodeID {
	if !fs.ready {
		return 0
	}
	return fs.id(0)
}

// Usage reports what the instance holds.
func (fs *FileSystem) Usage() Usage {
	return Usage{Nodes: fs.nodes, MaxNodes: fs.limits.Nodes, Pages: fs.pages, MaxPages: fs.limits.Pages}
}

// FilesystemInfo reports the configured page quota. Node limits bound metadata
// objects, while the page quota is the byte-allocation limit visible as
// filesystem capacity.
func (fs *FileSystem) FilesystemInfo() (FilesystemInfo, Status) {
	if !fs.ready || fs.pages > fs.limits.Pages {
		return FilesystemInfo{}, StatusIOError
	}
	total := uint64(fs.limits.Pages) * PageSize
	free := uint64(fs.limits.Pages-fs.pages) * PageSize
	return FilesystemInfo{Type: FilesystemRAMFS, TotalBytes: total, FreeBytes: free,
		AvailableBytes: free, CapacityAvailable: true}, StatusOK
}

func (fs *FileSystem) Create(parent NodeID, name []byte) (NodeID, Status) {
	value, ok := MakeName(name)
	if !ok {
		return 0, StatusInvalidArgument
	}
	return fs.CreateName(parent, value)
}

func (fs *FileSystem) Mkdir(parent NodeID, name []byte) (NodeID, Status) {
	value, ok := MakeName(name)
	if !ok {
		return 0, StatusInvalidArgument
	}
	return fs.MkdirName(parent, value)
}

func (fs *FileSystem) Open(parent NodeID, name []byte) (NodeID, Status) {
	value, ok := MakeName(name)
	if !ok {
		return 0, StatusInvalidArgument
	}
	return fs.OpenName(parent, value)
}

func (fs *FileSystem) CreateName(parent NodeID, name Name) (NodeID, Status) {
	if !name.valid() {
		return 0, StatusInvalidArgument
	}
	return fs.createName(parent, name, KindFile)
}

func (fs *FileSystem) MkdirName(parent NodeID, name Name) (NodeID, Status) {
	if !name.valid() {
		return 0, StatusInvalidArgument
	}
	return fs.createName(parent, name, KindDirectory)
}

func (fs *FileSystem) OpenName(parent NodeID, name Name) (NodeID, Status) {
	if !name.valid() {
		return 0, StatusInvalidArgument
	}
	directory, status := fs.directory(parent)
	if status != StatusOK {
		return 0, status
	}
	if index, found := fs.child(directory, name); found {
		return fs.id(index), StatusOK
	}
	return 0, StatusNotFound
}

func (fs *FileSystem) Read(id NodeID, offset uint64, target []byte) (int, Status) {
	entry, status := fs.file(id)
	if status != StatusOK {
		return 0, status
	}
	if offset >= entry.size || len(target) == 0 {
		return 0, StatusOK
	}
	count := uint64(len(target))
	if count > entry.size-offset {
		count = entry.size - offset
	}
	for done := uint64(0); done < count; {
		position := offset + done
		within := position % PageSize
		chunk := PageSize - within
		if chunk > count-done {
			chunk = count - done
		}
		part := target[done : done+chunk]
		if ref := fs.pageRef(entry, position/PageSize); ref != 0 {
			copy(part, fs.store.Page(ref)[within:within+chunk])
		} else {
			clear(part)
		}
		done += chunk
	}
	return int(count), StatusOK
}

// Write stores data at offset. The pages it needs, index pages included, are
// counted first, so a write the instance has no room for is refused whole
// with StatusNoSpace and changes nothing.
func (fs *FileSystem) Write(id NodeID, offset uint64, data []byte) (int, Status) {
	entry, status := fs.file(id)
	if status != StatusOK {
		return 0, status
	}
	if len(data) == 0 {
		return 0, StatusOK
	}
	length := uint64(len(data))
	if offset > MaxFileSize || length > MaxFileSize-offset {
		return 0, StatusResourceExhausted
	}
	end := offset + length
	if needed := fs.missing(entry, offset/PageSize, (end-1)/PageSize); needed > fs.limits.Pages-fs.pages {
		return 0, StatusNoSpace
	}
	done := uint64(0)
	for done < length {
		position := offset + done
		within := position % PageSize
		chunk := PageSize - within
		if chunk > length-done {
			chunk = length - done
		}
		ref, status := fs.ensurePage(entry, position/PageSize)
		if status != StatusOK {
			break
		}
		copy(fs.store.Page(ref)[within:within+chunk], data[done:done+chunk])
		done += chunk
	}
	if offset+done > entry.size {
		entry.size = offset + done
	}
	if done < length {
		// The store ran out below the instance's own limit.
		return int(done), StatusNoSpace
	}
	touch(entry, fs.timestamp())
	return int(done), StatusOK
}

// Truncate sets a file's size. Pages past the new end are freed, and the
// bytes of the last kept page past it are zeroed, so a later extension reads
// zeros there as it does in a hole.
func (fs *FileSystem) Truncate(id NodeID, size uint64) Status {
	entry, status := fs.file(id)
	if status != StatusOK {
		return status
	}
	if size > MaxFileSize {
		return StatusResourceExhausted
	}
	if size < entry.size {
		keep := (size + PageSize - 1) / PageSize
		fs.release(entry, keep)
		if within := size % PageSize; within != 0 {
			if ref := fs.pageRef(entry, size/PageSize); ref != 0 {
				clear(fs.store.Page(ref)[within:])
			}
		}
	}
	entry.size = size
	touch(entry, fs.timestamp())
	return StatusOK
}

func (fs *FileSystem) Stat(id NodeID) (Stat, Status) {
	entry, status := fs.lookup(id)
	if status != StatusOK {
		return Stat{}, status
	}
	return fs.stat(id, entry), StatusOK
}

// ReadDir returns the first child of parent whose index is at least cursor,
// and the cursor after it. Children are kept in index order, so a cursor stays
// meaningful across removals.
func (fs *FileSystem) ReadDir(parent NodeID, cursor int) (Entry, int, Status) {
	directory, status := fs.directory(parent)
	if status != StatusOK {
		return Entry{}, cursor, status
	}
	if cursor < 0 || cursor > MaxNodes {
		return Entry{}, cursor, StatusInvalidArgument
	}
	for link := directory.firstChild; link != 0; {
		index := int(link - 1)
		child := fs.at(index)
		if index >= cursor {
			entry := Entry{Stat: fs.stat(fs.id(index), child), Name: child.name, Size: child.nameSize}
			return entry, index + 1, StatusOK
		}
		link = child.next
	}
	return Entry{}, cursor, StatusNotFound
}

func (fs *FileSystem) Rename(id NodeID, parent NodeID, name []byte) Status {
	value, ok := MakeName(name)
	if !ok {
		return StatusInvalidArgument
	}
	return fs.RenameName(id, parent, value)
}

func (fs *FileSystem) RenameName(id NodeID, parent NodeID, name Name) Status {
	if !name.valid() {
		return StatusInvalidArgument
	}
	entry, status := fs.lookup(id)
	if status != StatusOK {
		return status
	}
	if id == fs.Root() {
		return StatusInvalidArgument
	}
	directory, status := fs.directory(parent)
	if status != StatusOK {
		return status
	}
	if _, found := fs.child(directory, name); found {
		return StatusAlreadyExists
	}
	if entry.kind == KindDirectory && fs.descendsFrom(parent, id) {
		return StatusInvalidArgument
	}
	index := int(uint32(id)) - 1
	old, _ := fs.lookup(entry.parent)
	fs.unlink(old, index)
	entry.parent = parent
	entry.name = name.data
	entry.nameSize = name.size
	fs.link(directory, index)
	stamp := fs.timestamp()
	touch(old, stamp)
	touch(directory, stamp)
	return StatusOK
}

// ReplaceName gives the file id the name of the file already called name in
// parent and removes that file in the same call, keeping its creation time
// (ADR 0129). Nothing else runs in between, so no reader sees neither file.
// With no such file it is a rename.
func (fs *FileSystem) ReplaceName(id NodeID, parent NodeID, name Name) Status {
	if !name.valid() {
		return StatusInvalidArgument
	}
	entry, status := fs.lookup(id)
	if status != StatusOK {
		return status
	}
	if entry.kind != KindFile {
		return StatusIsDirectory
	}
	directory, status := fs.directory(parent)
	if status != StatusOK {
		return status
	}
	index, found := fs.child(directory, name)
	if !found {
		return fs.RenameName(id, parent, name)
	}
	target := fs.at(index)
	if target.kind != KindFile {
		return StatusIsDirectory
	}
	if fs.id(index) == id {
		return StatusInvalidArgument
	}
	created := target.created
	if status = fs.UnlinkName(parent, name); status != StatusOK {
		return status
	}
	if status = fs.RenameName(id, parent, name); status == StatusOK {
		entry.created = created
	}
	return status
}

func (fs *FileSystem) Unlink(parent NodeID, name []byte) Status {
	value, ok := MakeName(name)
	if !ok {
		return StatusInvalidArgument
	}
	return fs.UnlinkName(parent, value)
}

// UnlinkName removes a file, freeing its pages, or an empty directory.
func (fs *FileSystem) UnlinkName(parent NodeID, name Name) Status {
	if !name.valid() {
		return StatusInvalidArgument
	}
	directory, status := fs.directory(parent)
	if status != StatusOK {
		return status
	}
	index, found := fs.child(directory, name)
	if !found {
		return StatusNotFound
	}
	entry := fs.at(index)
	if entry.kind == KindDirectory && entry.firstChild != 0 {
		return StatusNotEmpty
	}
	fs.release(entry, 0)
	fs.unlink(directory, index)
	touch(directory, fs.timestamp())
	generation := entry.generation + 1
	if generation == 0 {
		generation = 1
	}
	*entry = node{generation: generation}
	fs.nodes--
	if index < fs.hint {
		fs.hint = index
	}
	return StatusOK
}

func (fs *FileSystem) createName(parent NodeID, name Name, kind Kind) (NodeID, Status) {
	if !fs.ready {
		fs.Init()
	}
	directory, status := fs.directory(parent)
	if status != StatusOK {
		return 0, status
	}
	if _, found := fs.child(directory, name); found {
		return 0, StatusAlreadyExists
	}
	index, status := fs.allocateNode()
	if status != StatusOK {
		return 0, status
	}
	entry := fs.at(index)
	generation := entry.generation
	if generation == 0 {
		generation = 1
	}
	stamp := fs.timestamp()
	*entry = node{parent: parent, generation: generation, kind: kind, used: true, name: name.data, nameSize: name.size, created: stamp, modified: stamp}
	fs.link(directory, index)
	touch(directory, stamp)
	fs.nodes++
	return fs.id(index), StatusOK
}

// allocateNode finds a free slot, growing the table by a chunk when every
// slot is taken and the limit allows.
func (fs *FileSystem) allocateNode() (int, Status) {
	if fs.nodes >= fs.limits.Nodes {
		return 0, StatusNoSpace
	}
	for index := fs.hint; index < fs.allocated*chunkNodes; index++ {
		if !fs.at(index).used {
			fs.hint = index + 1
			return index, StatusOK
		}
	}
	if fs.allocated == maxChunks {
		return 0, StatusNoSpace
	}
	chunk := fs.store.Chunk()
	if chunk == nil {
		return 0, StatusNoSpace
	}
	index := fs.allocated * chunkNodes
	fs.chunks[fs.allocated] = chunk
	fs.allocated++
	fs.hint = index + 1
	return index, StatusOK
}

func (fs *FileSystem) at(index int) *node { return &fs.chunks[index/chunkNodes][index%chunkNodes] }

func (fs *FileSystem) id(index int) NodeID {
	return NodeID(uint64(fs.at(index).generation)<<32 | uint64(index+1))
}

func (fs *FileSystem) stat(id NodeID, entry *node) Stat {
	attributes := Attributes(0)
	if entry.kind == KindDirectory {
		attributes = AttrDirectory
	}
	return Stat{ID: id, Kind: entry.kind, Size: entry.size, Created: entry.created, Modified: entry.modified,
		Attributes: attributes, AttributesAvailable: true}
}

func (fs *FileSystem) lookup(id NodeID) (*node, Status) {
	index := int(uint32(id)) - 1
	generation := uint32(uint64(id) >> 32)
	if !fs.ready || index < 0 || index >= fs.allocated*chunkNodes || generation == 0 {
		return nil, StatusNotFound
	}
	entry := fs.at(index)
	if !entry.used || entry.generation != generation {
		return nil, StatusNotFound
	}
	return entry, StatusOK
}

func (fs *FileSystem) directory(id NodeID) (*node, Status) {
	entry, status := fs.lookup(id)
	if status != StatusOK {
		return nil, status
	}
	if entry.kind != KindDirectory {
		return nil, StatusNotDirectory
	}
	return entry, StatusOK
}

func (fs *FileSystem) file(id NodeID) (*node, Status) {
	entry, status := fs.lookup(id)
	if status != StatusOK {
		return nil, status
	}
	if entry.kind != KindFile {
		return nil, StatusIsDirectory
	}
	return entry, StatusOK
}

// child finds name among the children of directory.
func (fs *FileSystem) child(directory *node, name Name) (int, bool) {
	for link := directory.firstChild; link != 0; {
		index := int(link - 1)
		entry := fs.at(index)
		if sameName(entry, name) {
			return index, true
		}
		link = entry.next
	}
	return 0, false
}

// link inserts index into directory's children, keeping them in index order.
func (fs *FileSystem) link(directory *node, index int) {
	next := &directory.firstChild
	for *next != 0 && int(*next-1) < index {
		next = &fs.at(int(*next - 1)).next
	}
	fs.at(index).next = *next
	*next = uint32(index + 1)
}

// unlink removes index from directory's children.
func (fs *FileSystem) unlink(directory *node, index int) {
	if directory == nil {
		return
	}
	for next := &directory.firstChild; *next != 0; next = &fs.at(int(*next - 1)).next {
		if int(*next-1) == index {
			*next = fs.at(index).next
			fs.at(index).next = 0
			return
		}
	}
}

func (fs *FileSystem) descendsFrom(candidate NodeID, ancestor NodeID) bool {
	for steps := 0; candidate != 0 && steps <= MaxNodes; steps++ {
		if candidate == ancestor {
			return true
		}
		entry, status := fs.lookup(candidate)
		if status != StatusOK {
			return false
		}
		candidate = entry.parent
	}
	return false
}

func sameName(entry *node, name Name) bool {
	if entry.nameSize != name.size {
		return false
	}
	for index := 0; index < int(name.size); index++ {
		if entry.name[index] != name.data[index] {
			return false
		}
	}
	return true
}

// getRef and setRef read and write reference slot of an index page.
func (fs *FileSystem) getRef(page PageRef, slot uint64) PageRef {
	data := fs.store.Page(page)[slot*4:]
	return PageRef(uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24)
}

func (fs *FileSystem) setRef(page PageRef, slot uint64, ref PageRef) {
	data := fs.store.Page(page)[slot*4:]
	data[0], data[1], data[2], data[3] = byte(ref), byte(ref>>8), byte(ref>>16), byte(ref>>24)
}

// pageRef returns the page holding file page number page, or zero for a hole.
func (fs *FileSystem) pageRef(entry *node, page uint64) PageRef {
	switch {
	case page < directPages:
		return entry.direct[page]
	case page < indirectEnd:
		if entry.indirect == 0 {
			return 0
		}
		return fs.getRef(entry.indirect, page-directPages)
	case page < doubleEnd:
		if entry.double == 0 {
			return 0
		}
		relative := page - indirectEnd
		inner := fs.getRef(entry.double, relative/refsPerPage)
		if inner == 0 {
			return 0
		}
		return fs.getRef(inner, relative%refsPerPage)
	}
	return 0
}

// newPage takes a page from the store and counts it.
func (fs *FileSystem) newPage() (PageRef, Status) {
	if fs.pages >= fs.limits.Pages {
		return 0, StatusNoSpace
	}
	ref, ok := fs.store.Allocate()
	if !ok {
		return 0, StatusNoSpace
	}
	fs.pages++
	return ref, StatusOK
}

func (fs *FileSystem) freePage(ref PageRef) {
	if ref != 0 {
		fs.store.Free(ref)
		fs.pages--
	}
}

// ensurePage returns the page holding file page number page, allocating it
// and the index pages leading to it as needed.
func (fs *FileSystem) ensurePage(entry *node, page uint64) (PageRef, Status) {
	if ref := fs.pageRef(entry, page); ref != 0 {
		return ref, StatusOK
	}
	var status Status
	switch {
	case page < directPages:
		entry.direct[page], status = fs.newPage()
		return entry.direct[page], status
	case page < indirectEnd:
		if entry.indirect == 0 {
			if entry.indirect, status = fs.newPage(); status != StatusOK {
				return 0, status
			}
		}
		ref, status := fs.newPage()
		if status == StatusOK {
			fs.setRef(entry.indirect, page-directPages, ref)
		}
		return ref, status
	case page < doubleEnd:
		if entry.double == 0 {
			if entry.double, status = fs.newPage(); status != StatusOK {
				return 0, status
			}
		}
		relative := page - indirectEnd
		inner := fs.getRef(entry.double, relative/refsPerPage)
		if inner == 0 {
			if inner, status = fs.newPage(); status != StatusOK {
				return 0, status
			}
			fs.setRef(entry.double, relative/refsPerPage, inner)
		}
		ref, status := fs.newPage()
		if status == StatusOK {
			fs.setRef(inner, relative%refsPerPage, ref)
		}
		return ref, status
	}
	return 0, StatusResourceExhausted
}

// missing counts the pages, data and index, that writing file pages
// [first, last] would allocate.
func (fs *FileSystem) missing(entry *node, first, last uint64) uint64 {
	count := uint64(0)
	indirectCounted, doubleCounted := entry.indirect != 0, entry.double != 0
	innerCounted := ^uint64(0)
	for page := first; page <= last; page++ {
		if page >= indirectEnd && !doubleCounted {
			// Nothing below the double index exists yet: every page, each
			// inner index its range reaches, and the outer index are new.
			span := last - page + 1
			return count + span + (last-indirectEnd)/refsPerPage - (page-indirectEnd)/refsPerPage + 1 + 1
		}
		if fs.pageRef(entry, page) == 0 {
			count++
		}
		switch {
		case page >= directPages && page < indirectEnd && !indirectCounted:
			count++
			indirectCounted = true
		case page >= indirectEnd:
			outer := (page - indirectEnd) / refsPerPage
			if outer != innerCounted {
				innerCounted = outer
				if fs.getRef(entry.double, outer) == 0 {
					count++
				}
			}
		}
	}
	return count
}

// release frees every data page from file page keep on and the index pages
// that then index nothing.
func (fs *FileSystem) release(entry *node, keep uint64) {
	for page := keep; page < directPages; page++ {
		fs.freePage(entry.direct[page])
		entry.direct[page] = 0
	}
	if entry.indirect != 0 {
		from := uint64(0)
		if keep > directPages {
			from = keep - directPages
		}
		for slot := from; slot < refsPerPage; slot++ {
			if ref := fs.getRef(entry.indirect, slot); ref != 0 {
				fs.freePage(ref)
				fs.setRef(entry.indirect, slot, 0)
			}
		}
		if keep <= directPages {
			fs.freePage(entry.indirect)
			entry.indirect = 0
		}
	}
	if entry.double != 0 {
		for outer := uint64(0); outer < refsPerPage; outer++ {
			inner := fs.getRef(entry.double, outer)
			base := indirectEnd + outer*refsPerPage
			if inner == 0 || keep >= base+refsPerPage {
				continue
			}
			from := uint64(0)
			if keep > base {
				from = keep - base
			}
			for slot := from; slot < refsPerPage; slot++ {
				if ref := fs.getRef(inner, slot); ref != 0 {
					fs.freePage(ref)
					fs.setRef(inner, slot, 0)
				}
			}
			if keep <= base {
				fs.freePage(inner)
				fs.setRef(entry.double, outer, 0)
			}
		}
		if keep <= indirectEnd {
			fs.freePage(entry.double)
			entry.double = 0
		}
	}
}
