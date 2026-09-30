package linux

import "goos/kernel/handle"

const LinuxMaxFD = 64

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
	References uint8
}

type FDEntry struct {
	description uint8
	CloseOnExec bool
	open        bool
}

// FDTable maps Linux integers to bounded compatibility-owned descriptions.
// Object may identify a process-owned native handle or a VFS open ID, but that
// value is never returned to Linux userspace.
type FDTable struct {
	entries      [LinuxMaxFD]FDEntry
	descriptions [LinuxMaxFD]FileDescription
}

func (table *FDTable) Initialize(input, output, errorOutput handle.ID) bool {
	if table == nil || input == 0 || output == 0 || errorOutput == 0 {
		return false
	}
	*table = FDTable{}
	return table.install(0, FileDescription{Kind: FDStreamReader, Object: uint64(input)}) &&
		table.install(1, FileDescription{Kind: FDStreamWriter, Object: uint64(output)}) &&
		table.install(2, FileDescription{Kind: FDStreamWriter, Object: uint64(errorOutput)})
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
	description := table.descriptions[table.entries[fd].description]
	return description, description.valid()
}

// Duplicate adds another integer referring to the same open-file description.
func (table *FDTable) Duplicate(fd int, minimum int, closeOnExec bool) (int, bool) {
	if table == nil || minimum < 0 || minimum >= LinuxMaxFD || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return -1, false
	}
	descriptionIndex := table.entries[fd].description
	description := &table.descriptions[descriptionIndex]
	if !description.valid() || description.References >= LinuxMaxFD {
		return -1, false
	}
	for target := minimum; target < LinuxMaxFD; target++ {
		if table.entries[target].open {
			continue
		}
		description.References++
		table.entries[target] = FDEntry{description: descriptionIndex, CloseOnExec: closeOnExec, open: true}
		return target, true
	}
	return -1, false
}

// Close invalidates one Linux integer and reports the description only when
// its final reference is gone. The caller then releases any VFS-owned state.
func (table *FDTable) Close(fd int) (FileDescription, bool, bool) {
	if table == nil || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return FileDescription{}, false, false
	}
	index := table.entries[fd].description
	description := &table.descriptions[index]
	table.entries[fd] = FDEntry{}
	if description.References == 0 {
		return FileDescription{}, false, false
	}
	description.References--
	if description.References != 0 {
		return FileDescription{}, false, true
	}
	released := *description
	*description = FileDescription{}
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
	if table == nil || fd < 0 || fd >= LinuxMaxFD || table.entries[fd].open || !description.valid() {
		return false
	}
	for index := range table.descriptions {
		if table.descriptions[index].References != 0 {
			continue
		}
		description.References = 1
		table.descriptions[index] = description
		table.entries[fd] = FDEntry{description: uint8(index), open: true}
		return true
	}
	return false
}

func (description FileDescription) valid() bool {
	return description.Kind > FDInvalid && description.Kind <= FDSynthetic && description.Object != 0
}
