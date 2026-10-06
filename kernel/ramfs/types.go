package ramfs

import "goos/internal/limits"

// The types below are the node-ID backend contract every filesystem the VFS
// mounts speaks, RAMFS and the FAT32 service alike.

// MaxName is the native name limit (ADR 0110).
const MaxName = limits.NameMax

// MaxVolumeName bounds the optional human-readable filesystem label carried
// across native protocols.
const MaxVolumeName = 64

// FilesystemType is a stable native filesystem identity, not an on-disk magic
// value.
type FilesystemType uint8

const (
	FilesystemRAMFS FilesystemType = iota + 1
	FilesystemSystemImage
	FilesystemFAT32
	LastFilesystemType = FilesystemFAT32
)

// FilesystemInfo describes one mounted backend. CapacityAvailable separates a
// backend with no capacity model from an empty filesystem.
type FilesystemInfo struct {
	Type              FilesystemType
	TotalBytes        uint64
	FreeBytes         uint64
	AvailableBytes    uint64
	ReadOnly          bool
	CapacityAvailable bool
	VolumeName        [MaxVolumeName]byte
	VolumeNameSize    uint8
}

func (info FilesystemInfo) Valid() bool {
	if info.Type == 0 {
		return info.TotalBytes == 0 && info.FreeBytes == 0 && info.AvailableBytes == 0 &&
			!info.ReadOnly && !info.CapacityAvailable && info.VolumeNameSize == 0
	}
	if info.Type < FilesystemRAMFS || info.Type > LastFilesystemType || info.VolumeNameSize > MaxVolumeName {
		return false
	}
	if !info.CapacityAvailable {
		return info.TotalBytes == 0 && info.FreeBytes == 0 && info.AvailableBytes == 0
	}
	return info.FreeBytes <= info.TotalBytes && info.AvailableBytes <= info.FreeBytes
}

func (info *FilesystemInfo) SetVolumeName(name []byte) bool {
	if info == nil || len(name) > MaxVolumeName {
		return false
	}
	for index := range info.VolumeName {
		info.VolumeName[index] = 0
	}
	copy(info.VolumeName[:], name)
	info.VolumeNameSize = uint8(len(name))
	return true
}

func (info FilesystemInfo) Name() []byte { return info.VolumeName[:info.VolumeNameSize] }

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
	// StatusNoSpace is a backend whose storage is full: no free cluster or
	// directory slot is left for the request.
	StatusNoSpace
	// StatusCorrupt is on-disk metadata that fails validation: a cluster
	// chain that loops, leaves the data area, or ends before the file does.
	StatusCorrupt
	// StatusTimeout is a backend served by another process that did not
	// answer within its deadline; StatusPeerDied one whose process has gone.
	// Both used to be reported as StatusIOError (GoOS 0.1.3 §12).
	StatusTimeout
	StatusPeerDied
	// StatusUnsupported is an operation or an on-disk feature the backend
	// does not implement.
	StatusUnsupported
	// LastStatus is the highest status a backend may report; decoders refuse
	// anything above it.
	LastStatus = StatusUnsupported
)

type Kind uint8

const (
	KindFile Kind = iota + 1
	KindDirectory
)

type NodeID uint64

// Timestamp is a native UTC timestamp. Available distinguishes a missing
// filesystem field from the Unix epoch; unavailable timestamps must be zero.
type Timestamp struct {
	Seconds     int64
	Nanoseconds uint32
	Available   bool
}

// MetadataChange is what a SetMetadata call changes: the modification time,
// the read-only attribute of a file, or both.
type MetadataChange struct {
	SetModified bool
	Modified    Timestamp
	SetReadOnly bool
	ReadOnly    bool
}

// Valid reports whether the change asks for something and names a usable
// time.
func (change MetadataChange) Valid() bool {
	return (change.SetModified || change.SetReadOnly) &&
		(!change.SetModified || change.Modified.Available && change.Modified.Valid())
}

func (stamp Timestamp) Valid() bool {
	return stamp.Available && stamp.Nanoseconds < 1_000_000_000 ||
		!stamp.Available && stamp.Seconds == 0 && stamp.Nanoseconds == 0
}

// Attributes are stable native object properties, not raw filesystem bits.
type Attributes uint8

const (
	AttrDirectory Attributes = 1 << iota
	AttrReadOnly
	AttrHidden
	AttrSystem
	AttrArchive
	LastAttribute = AttrArchive
)

type Stat struct {
	ID                  NodeID
	Kind                Kind
	Size                uint64
	Created             Timestamp
	Modified            Timestamp
	Attributes          Attributes
	AttributesAvailable bool
	// MountID is assigned by VFS. Backends and the filesystem-service wire
	// protocol leave it zero because it describes namespace placement.
	MountID uint64
}

func (stat Stat) Valid() bool {
	return stat.Kind >= KindFile && stat.Kind <= KindDirectory && stat.Created.Valid() && stat.Modified.Valid() &&
		stat.Attributes <= (LastAttribute*2-1) && (stat.AttributesAvailable || stat.Attributes == 0)
}

type Entry struct {
	Stat Stat
	Name [MaxName]byte
	Size uint16
}

// Name is the allocation-free representation used at filesystem boundaries.
// Its fields are private so every value accepted by a backend is valid.
type Name struct {
	data [MaxName]byte
	size uint16
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
	name.size = uint16(len(value))
	return name, true
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
