package ramfs

const (
	MaxNodes    = 32
	MaxName     = 63
	MaxFileSize = 4096
)

type Status uint8

const (
	StatusOK Status = iota
	StatusInvalidArgument
	StatusNotFound
	StatusAlreadyExists
	StatusNotDirectory
	StatusIsDirectory
	StatusNotEmpty
	StatusResourceExhausted
	// StatusPermissionDenied is what a read-only backend answers to a
	// mutation, so an immutable mount is distinguishable from a bad request.
	StatusPermissionDenied
	// StatusIOError is a backend that could not be reached or did not answer
	// usably: a filesystem served by another process that has died, stalled
	// past its deadline, or sent a malformed reply (ADR 0096). Nothing in the
	// kernel produces it.
	StatusIOError
)

type Kind uint8

const (
	KindFile Kind = iota + 1
	KindDirectory
)

type NodeID uint64

type Stat struct {
	ID   NodeID
	Kind Kind
	Size uint64
}

type Entry struct {
	Stat Stat
	Name [MaxName]byte
	Size uint8
}

// Name is the allocation-free representation used at filesystem boundaries.
// Its fields are private so every value accepted by a backend is valid.
type Name struct {
	data [MaxName]byte
	size uint8
}

func (name Name) CopyTo(target []byte) int {
	if !name.valid() || len(target) < int(name.size) {
		return 0
	}
	return copy(target, name.data[:name.size])
}

func MakeName(value []byte) (Name, bool) {
	var name Name
	if !validName(value) {
		return name, false
	}
	copy(name.data[:], value)
	name.size = uint8(len(value))
	return name, true
}

type node struct {
	parent     NodeID
	generation uint32
	kind       Kind
	name       [MaxName]byte
	nameSize   uint8
	data       [MaxFileSize]byte
	size       uint16
	used       bool
}

// reset clears a slot in place and gives it the supplied generation. Assigning
// a node literal instead would build the whole 4 KiB file body as a temporary,
// which the kernel's leaking heap never frees.
func (entry *node) reset(generation uint32) {
	*entry = node{}
	if generation == 0 {
		generation = 1
	}
	entry.generation = generation
}

type FileSystem struct {
	nodes [MaxNodes]node
}

func (fs *FileSystem) Init() NodeID {
	if fs.nodes[0].used {
		return fs.id(0)
	}
	fs.nodes[0].reset(1)
	fs.nodes[0].kind, fs.nodes[0].used = KindDirectory, true
	return fs.id(0)
}

func (fs *FileSystem) Root() NodeID {
	if !fs.nodes[0].used {
		return 0
	}
	return fs.id(0)
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
	if _, status := fs.directory(parent); status != StatusOK {
		return 0, status
	}
	for index := range fs.nodes {
		if fs.nodes[index].used && fs.nodes[index].parent == parent && sameName(&fs.nodes[index], name) {
			return fs.id(index), StatusOK
		}
	}
	return 0, StatusNotFound
}

func (fs *FileSystem) Read(id NodeID, offset uint64, target []byte) (int, Status) {
	entry, status := fs.lookup(id)
	if status != StatusOK {
		return 0, status
	}
	if entry.kind != KindFile {
		return 0, StatusIsDirectory
	}
	if offset >= uint64(entry.size) {
		return 0, StatusOK
	}
	count := len(target)
	remaining := int(uint64(entry.size) - offset)
	if count > remaining {
		count = remaining
	}
	copy(target[:count], entry.data[int(offset):int(offset)+count])
	return count, StatusOK
}

func (fs *FileSystem) Write(id NodeID, offset uint64, data []byte) (int, Status) {
	entry, status := fs.lookup(id)
	if status != StatusOK {
		return 0, status
	}
	if entry.kind != KindFile {
		return 0, StatusIsDirectory
	}
	if offset > MaxFileSize || uint64(len(data)) > MaxFileSize-offset {
		return 0, StatusResourceExhausted
	}
	copy(entry.data[int(offset):], data)
	end := offset + uint64(len(data))
	if end > uint64(entry.size) {
		entry.size = uint16(end)
	}
	return len(data), StatusOK
}

func (fs *FileSystem) Truncate(id NodeID, size uint64) Status {
	entry, status := fs.lookup(id)
	if status != StatusOK {
		return status
	}
	if entry.kind != KindFile {
		return StatusIsDirectory
	}
	if size > MaxFileSize {
		return StatusResourceExhausted
	}
	if size < uint64(entry.size) {
		for index := int(size); index < int(entry.size); index++ {
			entry.data[index] = 0
		}
	}
	entry.size = uint16(size)
	return StatusOK
}

func (fs *FileSystem) Stat(id NodeID) (Stat, Status) {
	entry, status := fs.lookup(id)
	if status != StatusOK {
		return Stat{}, status
	}
	return Stat{ID: id, Kind: entry.kind, Size: uint64(entry.size)}, StatusOK
}

func (fs *FileSystem) ReadDir(parent NodeID, cursor int) (Entry, int, Status) {
	if _, status := fs.directory(parent); status != StatusOK || cursor < 0 || cursor > MaxNodes {
		if status != StatusOK {
			return Entry{}, cursor, status
		}
		return Entry{}, cursor, StatusInvalidArgument
	}
	for index := cursor; index < len(fs.nodes); index++ {
		node := &fs.nodes[index]
		if node.used && node.parent == parent {
			entry := Entry{Stat: Stat{ID: fs.id(index), Kind: node.kind, Size: uint64(node.size)}, Name: node.name, Size: node.nameSize}
			return entry, index + 1, StatusOK
		}
	}
	return Entry{}, MaxNodes, StatusNotFound
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
	if status != StatusOK || id == fs.Root() {
		if status != StatusOK {
			return status
		}
		return StatusInvalidArgument
	}
	if _, status = fs.directory(parent); status != StatusOK {
		return status
	}
	if _, status = fs.OpenName(parent, name); status == StatusOK {
		return StatusAlreadyExists
	}
	if entry.kind == KindDirectory && fs.descendsFrom(parent, id) {
		return StatusInvalidArgument
	}
	entry.parent = parent
	entry.name = [MaxName]byte{}
	entry.name = name.data
	entry.nameSize = name.size
	return StatusOK
}

func (fs *FileSystem) Unlink(parent NodeID, name []byte) Status {
	value, ok := MakeName(name)
	if !ok {
		return StatusInvalidArgument
	}
	return fs.UnlinkName(parent, value)
}

func (fs *FileSystem) UnlinkName(parent NodeID, name Name) Status {
	if !name.valid() {
		return StatusInvalidArgument
	}
	id, status := fs.OpenName(parent, name)
	if status != StatusOK {
		return status
	}
	entry, _ := fs.lookup(id)
	if entry.kind == KindDirectory {
		for index := range fs.nodes {
			if fs.nodes[index].used && fs.nodes[index].parent == id {
				return StatusNotEmpty
			}
		}
	}
	entry.reset(entry.generation + 1)
	return StatusOK
}

func (fs *FileSystem) createName(parent NodeID, name Name, kind Kind) (NodeID, Status) {
	if fs.Root() == 0 {
		fs.Init()
	}
	if _, status := fs.directory(parent); status != StatusOK {
		return 0, status
	}
	if _, status := fs.OpenName(parent, name); status == StatusOK {
		return 0, StatusAlreadyExists
	}
	for index := 1; index < len(fs.nodes); index++ {
		entry := &fs.nodes[index]
		if !entry.used {
			entry.reset(entry.generation)
			entry.parent, entry.kind, entry.name, entry.nameSize, entry.used = parent, kind, name.data, name.size, true
			return fs.id(index), StatusOK
		}
	}
	return 0, StatusResourceExhausted
}

func (fs *FileSystem) lookup(id NodeID) (*node, Status) {
	index := int(uint32(id)) - 1
	generation := uint32(uint64(id) >> 32)
	if index < 0 || index >= len(fs.nodes) || generation == 0 {
		return nil, StatusNotFound
	}
	entry := &fs.nodes[index]
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

func (fs *FileSystem) id(index int) NodeID {
	return NodeID(uint64(fs.nodes[index].generation)<<32 | uint64(index+1))
}

func (fs *FileSystem) descendsFrom(candidate NodeID, ancestor NodeID) bool {
	for candidate != 0 {
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

func validName(name []byte) bool {
	if len(name) == 0 || len(name) > MaxName || (len(name) == 1 && name[0] == '.') || (len(name) == 2 && name[0] == '.' && name[1] == '.') {
		return false
	}
	for _, value := range name {
		if value == 0 || value == '/' {
			return false
		}
	}
	return true
}

func (name Name) valid() bool {
	return name.size != 0 && name.size <= MaxName
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
