package ramfs

import "goos/internal/limits"

// The types below are the node-ID backend contract every filesystem the VFS
// mounts speaks, RAMFS and the FAT32 service alike.

// MaxName is the native name limit (ADR 0110).
const MaxName = limits.NameMax

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
	// LastStatus is the highest status a backend may report; decoders refuse
	// anything above it.
	LastStatus = StatusCorrupt
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
