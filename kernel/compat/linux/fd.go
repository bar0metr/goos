package linux

import (
	"goos/kernel/process"
)

const (
	LinuxMaxFD          = 64
	MaxOpenDescriptions = process.MaxProcesses * LinuxMaxFD
)

type FDKind uint8

const (
	FDInvalid FDKind = iota
	FDStreamReader
	FDStreamWriter
	FDVFSFile
	FDVFSDirectory
	FDSynthetic
)

type FileDescription struct {
	Kind       FDKind
	Flags      uint32
	Object     uint64
	Offset     uint64
	References uint16
}

type FDEntry struct {
	description uint16
	CloseOnExec bool
	open        bool
}

type descriptionSlot struct {
	familyID    uint64
	description FileDescription
}

var openDescriptions [MaxOpenDescriptions]descriptionSlot

// FDTable maps Linux integers to bounded compatibility-owned descriptions.
// Object may identify a kernel stream endpoint or a VFS open ID, but that value
// is never returned to Linux userspace.
type FDTable struct {
	familyID uint64
	entries  [LinuxMaxFD]FDEntry
}

func (table *FDTable) Initialize(familyID uint64, input, output, errorOutput uint64) bool {
	if table == nil || familyID == 0 || input == 0 || output == 0 || errorOutput == 0 {
		return false
	}
	*table = FDTable{}
	table.familyID = familyID
	if table.install(0, FileDescription{Kind: FDStreamReader, Object: input}) &&
		table.install(1, FileDescription{Kind: FDStreamWriter, Object: output}) &&
		table.install(2, FileDescription{Kind: FDStreamWriter, Object: errorOutput}) {
		return true
	}
	table.CloseAll(nil)
	*table = FDTable{}
	return false
}

// Inherit copies descriptor integers and retains their shared open-file
// descriptions. Close-on-exec remains attached to each copied integer.
func (table *FDTable) Inherit(parent *FDTable) bool {
	if table == nil || parent == nil || table == parent || parent.familyID == 0 {
		return false
	}
	*table = FDTable{familyID: parent.familyID}
	for fd := range parent.entries {
		entry := parent.entries[fd]
		if !entry.open {
			continue
		}
		slot, valid := parent.description(entry.description)
		if !valid || slot.description.References == ^uint16(0) {
			table.CloseAll(nil)
			return false
		}
		slot.description.References++
		table.entries[fd] = entry
	}
	return true
}

func (table *FDTable) Allocate(description FileDescription, closeOnExec bool) (int, bool) {
	if table == nil || !description.valid() {
		return -1, false
	}
	for fd := 0; fd < LinuxMaxFD; fd++ {
		if table.entries[fd].open {
			continue
		}
		if !table.install(fd, description) {
			return -1, false
		}
		table.entries[fd].CloseOnExec = closeOnExec
		return fd, true
	}
	return -1, false
}

// HasFree reports whether Allocate would find a descriptor. A caller checks
// before asking a service to open something it could not then name.
func (table *FDTable) HasFree() bool {
	if table == nil {
		return false
	}
	for fd := 0; fd < LinuxMaxFD; fd++ {
		if !table.entries[fd].open {
			return true
		}
	}
	return false
}

func (table *FDTable) Lookup(fd int) (FileDescription, bool) {
	if table == nil || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return FileDescription{}, false
	}
	slot, valid := table.description(table.entries[fd].description)
	if !valid {
		return FileDescription{}, false
	}
	return slot.description, true
}

// Description returns the shared open-file description behind fd for
// updating its offset. Every descriptor that duplicates fd sees the change.
func (table *FDTable) Description(fd int) (*FileDescription, bool) {
	if table == nil || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return nil, false
	}
	slot, valid := table.description(table.entries[fd].description)
	if !valid {
		return nil, false
	}
	return &slot.description, true
}

// DescriptionIndex identifies the description behind fd among those the
// table holds. Duplicated descriptors report the same index.
func (table *FDTable) DescriptionIndex(fd int) (int, bool) {
	if table == nil || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return 0, false
	}
	return int(table.entries[fd].description), true
}

// Duplicate adds another integer, the lowest free one at or above minimum,
// for the same open-file description. It returns -2 when fd names nothing
// and -1 when no integer is free, so a caller can tell EBADF from EMFILE.
func (table *FDTable) Duplicate(fd int, minimum int, closeOnExec bool) (int, bool) {
	if table == nil || minimum < 0 || minimum >= LinuxMaxFD || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return -2, false
	}
	descriptionIndex := table.entries[fd].description
	slot, valid := table.description(descriptionIndex)
	if !valid || slot.description.References == ^uint16(0) {
		return -1, false
	}
	for target := minimum; target < LinuxMaxFD; target++ {
		if table.entries[target].open {
			continue
		}
		slot.description.References++
		table.entries[target] = FDEntry{description: descriptionIndex, CloseOnExec: closeOnExec, open: true}
		return target, true
	}
	return -1, false
}

// DuplicateAt makes target, which must be free, another integer for the
// description behind fd.
func (table *FDTable) DuplicateAt(fd int, target int, closeOnExec bool) bool {
	if table == nil || target < 0 || target >= LinuxMaxFD || fd < 0 || fd >= LinuxMaxFD ||
		!table.entries[fd].open || table.entries[target].open {
		return false
	}
	descriptionIndex := table.entries[fd].description
	slot, valid := table.description(descriptionIndex)
	if !valid || slot.description.References == ^uint16(0) {
		return false
	}
	slot.description.References++
	table.entries[target] = FDEntry{description: descriptionIndex, CloseOnExec: closeOnExec, open: true}
	return true
}

// CloseOnExec reports the close-on-exec flag of one descriptor. The flag
// belongs to the integer; descriptors that share a description keep their
// own.
func (table *FDTable) CloseOnExec(fd int) (bool, bool) {
	if table == nil || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return false, false
	}
	return table.entries[fd].CloseOnExec, true
}

func (table *FDTable) SetCloseOnExec(fd int, closeOnExec bool) bool {
	if table == nil || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return false
	}
	table.entries[fd].CloseOnExec = closeOnExec
	return true
}

// Close invalidates one Linux integer and reports the description only when
// its final reference is gone. The caller then releases any VFS-owned state.
func (table *FDTable) Close(fd int) (FileDescription, bool, bool) {
	if table == nil || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return FileDescription{}, false, false
	}
	index := table.entries[fd].description
	slot, valid := table.description(index)
	if !valid {
		return FileDescription{}, false, false
	}
	description := &slot.description
	table.entries[fd] = FDEntry{}
	if description.References == 0 {
		return FileDescription{}, false, false
	}
	description.References--
	if description.References != 0 {
		return FileDescription{}, false, true
	}
	released := *description
	*slot = descriptionSlot{}
	return released, true, true
}

func (table *FDTable) CloseAll(visit func(FileDescription)) {
	if table == nil {
		return
	}
	for fd := 0; fd < LinuxMaxFD; fd++ {
		if released, last, ok := table.Close(fd); ok && last && visit != nil {
			visit(released)
		}
	}
}

func (table *FDTable) install(fd int, description FileDescription) bool {
	if table == nil || table.familyID == 0 || fd < 0 || fd >= LinuxMaxFD || table.entries[fd].open || !description.valid() {
		return false
	}
	for index := range openDescriptions {
		if openDescriptions[index].familyID != 0 {
			continue
		}
		description.References = 1
		openDescriptions[index] = descriptionSlot{familyID: table.familyID, description: description}
		table.entries[fd] = FDEntry{description: uint16(index), open: true}
		return true
	}
	return false
}

func (table *FDTable) description(index uint16) (*descriptionSlot, bool) {
	if table == nil || table.familyID == 0 || int(index) >= len(openDescriptions) {
		return nil, false
	}
	slot := &openDescriptions[index]
	if slot.familyID != table.familyID || !slot.description.valid() || slot.description.References == 0 {
		return nil, false
	}
	return slot, true
}

func (description FileDescription) valid() bool {
	return description.Kind > FDInvalid && description.Kind <= FDSynthetic && description.Object != 0
}
