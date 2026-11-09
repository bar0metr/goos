// Package volume is PID 1's policy for additional filesystem volumes (ADR
// 0222): which partition a mount names, which slot it takes, which driver
// serves it, whether a restarted instance still serves the same volume, and
// when to give up. It holds no handles and does no I/O, so the host tests
// exercise every decision PID 1 makes.
package volume

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fsdetect"
	"goos/userspace/partition"
)

// Slots is the number of additional volume slots, numbered from 1.
const Slots = vfs.MaxVolumes

// MaxRestarts bounds the replacements of one mounted volume's service. A
// volume whose driver keeps crashing stays offline until it is unmounted.
const MaxRestarts = 3

// Status is the outcome of a volume operation, as the mount command reports
// it.
type Status uint8

const (
	OK Status = iota
	InvalidSource
	InvalidPath
	InvalidType
	NoSlot
	PathInUse
	PartitionInUse
	NoPartition
	Unrecognized
	Ambiguous
	TypeMismatch
	// Refused: the driver refused the volume; Detail says why.
	Refused
	NotMounted
	Busy
	LaunchFailed
	Failed
	lastStatus = Failed
)

var statusText = [...]string{
	OK:             "ok",
	InvalidSource:  "invalid source: use part:N, guid:GUID or name:LABEL",
	InvalidPath:    "invalid mount path: use " + vfs.VolumeRoot + "NAME",
	InvalidType:    "unknown filesystem type",
	NoSlot:         "every volume slot is in use",
	PathInUse:      "a volume is already mounted there",
	PartitionInUse: "the partition is already mounted",
	NoPartition:    "no such partition",
	Unrecognized:   "unsupported filesystem",
	Ambiguous:      "ambiguous filesystem signatures",
	TypeMismatch:   "the partition holds another filesystem type",
	Refused:        "the driver refused the volume",
	NotMounted:     "nothing is mounted there",
	Busy:           "files are open on the volume",
	LaunchFailed:   "the filesystem service did not start",
	Failed:         "the volume failed",
}

func (status Status) String() string {
	if int(status) < len(statusText) {
		return statusText[status]
	}
	return "unknown status"
}

// Valid reports whether status is defined.
func (status Status) Valid() bool { return status <= lastStatus }

// ParseSource turns a mount source into a partition selector: part:N for a
// partition number, guid:GUID for a GPT unique GUID, name:LABEL for a GPT
// partition name. The whole device is never a source: it holds the
// persistent volume or a partition table.
func ParseSource(text []byte) (partition.Selector, bool) {
	kind, value, found := cut(text, ':')
	if !found || len(value) == 0 {
		return partition.Selector{}, false
	}
	switch string(kind) {
	case "part":
		number := uint32(0)
		for _, digit := range value {
			if digit < '0' || digit > '9' || number > 1<<20 {
				return partition.Selector{}, false
			}
			number = number*10 + uint32(digit-'0')
		}
		return partition.Selector{Kind: partition.SelectIndex, Index: number}, number != 0
	case "guid":
		guid, ok := partition.ParseGUID(string(value))
		return partition.Selector{Kind: partition.SelectGUID, GUID: guid}, ok && guid != partition.GUID{}
	case "name":
		return partition.NameSelector(string(value))
	}
	return partition.Selector{}, false
}

func cut(text []byte, separator byte) ([]byte, []byte, bool) {
	for index, value := range text {
		if value == separator {
			return text[:index], text[index+1:], true
		}
	}
	return text, nil, false
}

// Request is one mount: what to mount, where, and as which type (zero for
// whatever the probe recognizes).
type Request struct {
	Source   partition.Selector
	Path     [vfs.MaxVolumePath]byte
	PathSize uint8
	Type     ramfs.FilesystemType
}

// MakeRequest checks and builds a request from its textual parts.
func MakeRequest(kind, source, path []byte) (Request, Status) {
	var request Request
	if len(kind) == 0 {
		kind = []byte("auto")
	}
	requested, ok := fsdetect.TypeNamed(kind)
	if !ok {
		return request, InvalidType
	}
	selector, ok := ParseSource(source)
	if !ok {
		return request, InvalidSource
	}
	if !vfs.ValidVolumePath(path) {
		return request, InvalidPath
	}
	request.Source, request.Type = selector, requested
	request.PathSize = uint8(copy(request.Path[:], path))
	return request, OK
}

// PathBytes is the request's mount path.
func (request *Request) PathBytes() []byte { return request.Path[:request.PathSize] }

// State is where a slot stands.
type State uint8

const (
	Free State = iota
	// Starting: probed and assigned a driver; the instance is launching.
	Starting
	// Mounted: the VFS has the volume in its namespace.
	Mounted
	// Lost: the instance stopped answering and is being replaced.
	Lost
	// Offline: the instance failed past its restart budget, or a replacement
	// found another volume; the mount answers errors until it is unmounted.
	Offline
	// Stopping: an unmount was accepted and the process is being ended.
	Stopping
)

// Slot is one additional volume.
type Slot struct {
	State      State
	Request    Request
	Driver     fsdetect.Driver
	Probe      fsdetect.Result
	Generation uint64
	Restarts   uint8
	// Detail is the VFS status of the last refused attach.
	Detail vfs.Status
}

// Table is the set of slots, slot 1 at index 0.
type Table struct {
	slots      [Slots]Slot
	generation uint64
}

// Slot returns slot number index, or nil.
func (table *Table) Slot(index uint8) *Slot {
	if index < 1 || index > Slots {
		return nil
	}
	return &table.slots[index-1]
}

// Find returns the slot mounted at path.
func (table *Table) Find(path []byte) (uint8, bool) {
	for index := range table.slots {
		slot := &table.slots[index]
		if slot.State != Free && string(slot.Request.PathBytes()) == string(path) {
			return uint8(index + 1), true
		}
	}
	return 0, false
}

// Reserve takes a free slot for request. A path or a partition another slot
// holds is refused: one partition never has two instances.
func (table *Table) Reserve(request Request) (uint8, Status) {
	free := uint8(0)
	for index := range table.slots {
		slot := &table.slots[index]
		if slot.State == Free {
			if free == 0 {
				free = uint8(index + 1)
			}
			continue
		}
		if string(slot.Request.PathBytes()) == string(request.PathBytes()) {
			return 0, PathInUse
		}
		if slot.Request.Source == request.Source {
			return 0, PartitionInUse
		}
	}
	if free == 0 {
		return 0, NoSlot
	}
	table.slots[free-1] = Slot{State: Starting, Request: request}
	return free, OK
}

// Release frees a slot.
func (table *Table) Release(index uint8) {
	if slot := table.Slot(index); slot != nil {
		*slot = Slot{}
	}
}

// Probed records what the probe of a slot found and picks its driver. The
// first probe of a mount records the volume's identity. A probe for a
// replacement instance must find the same type and identity; otherwise the
// partition holds another volume now, and the slot goes offline rather than
// serving it under the old mount's open handles.
func (table *Table) Probed(index uint8, result fsdetect.Result) Status {
	slot := table.Slot(index)
	if slot == nil {
		return Failed
	}
	switch result.Status {
	case fsdetect.Recognized:
	case fsdetect.Ambiguous:
		return Ambiguous
	default:
		return Unrecognized
	}
	if slot.Generation != 0 {
		if !slot.Probe.SameVolume(&result) {
			slot.State = Offline
			return TypeMismatch
		}
		return OK
	}
	driver, _, ok := fsdetect.Choose(&result, slot.Request.Type)
	if !ok {
		if slot.Request.Type != 0 && slot.Request.Type != result.Type {
			return TypeMismatch
		}
		return Unrecognized
	}
	slot.Driver, slot.Probe = driver, result
	return OK
}

// Launching gives the instance about to start a new generation.
func (table *Table) Launching(index uint8) uint64 {
	slot := table.Slot(index)
	if slot == nil {
		return 0
	}
	table.generation++
	slot.Generation = table.generation
	if slot.State != Mounted {
		slot.State = Starting
	}
	return slot.Generation
}

// Attached records the VFS's answer to an attach of generation.
func (table *Table) Attached(index uint8, generation uint64, status vfs.Status) Status {
	slot := table.Slot(index)
	if slot == nil || slot.Generation != generation {
		return Failed
	}
	if status != vfs.StatusOK {
		slot.Detail = status
		return Refused
	}
	slot.State, slot.Detail = Mounted, vfs.StatusOK
	return OK
}

// Lost records a replacement notice or an unplanned exit of generation. It
// reports whether to start a replacement: only for the current generation
// of a mounted volume, and only within the restart budget.
func (table *Table) Lost(index uint8, generation uint64) bool {
	slot := table.Slot(index)
	if slot == nil || slot.Generation != generation || (slot.State != Mounted && slot.State != Lost) {
		return false
	}
	if slot.Restarts >= MaxRestarts {
		slot.State = Offline
		return false
	}
	slot.Restarts++
	slot.State = Lost
	return true
}

// Stopping marks a slot whose unmount the VFS accepted.
func (table *Table) Stopping(index uint8) {
	if slot := table.Slot(index); slot != nil {
		slot.State = Stopping
	}
}
