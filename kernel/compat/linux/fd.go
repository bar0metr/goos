package linux

import (
	"goos/kernel/compat/linux/abi"
	"goos/kernel/process"
)

// terminalStandardStreams marks, in the output and error arguments of
// initializeHeld, a table whose standard descriptors are the terminal the
// input argument names.
const terminalStandardStreams = ^uint64(0)

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
	// FDTerminal is a Linux terminal. Object is its TerminalID, never a
	// native handle or stream endpoint.
	FDTerminal
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

func openDescriptionCountHeld() (descriptions int, references int) {
	guard.Check()
	for index := range openDescriptions {
		if openDescriptions[index].familyID != 0 {
			descriptions++
			references += int(openDescriptions[index].description.References)
		}
	}
	return descriptions, references
}

// FDTable maps Linux integers to bounded compatibility-owned descriptions.
// Object may identify a kernel stream endpoint or a VFS open ID, but that value
// is never returned to Linux userspace.
type FDTable struct {
	familyID uint64
	entries  [LinuxMaxFD]FDEntry
}

func (table *FDTable) initializeHeld(familyID uint64, input, output, errorOutput uint64) bool {
	guard.Check()
	if table == nil || familyID == 0 || input == 0 || output == 0 || errorOutput == 0 {
		return false
	}
	*table = FDTable{}
	table.familyID = familyID
	if terminal := TerminalID(input); errorOutput == terminalStandardStreams && output == terminalStandardStreams {
		// One read-write description on the terminal, as a login gets: the
		// three integers share it.
		if table.install(0, FileDescription{Kind: FDTerminal, Flags: abi.OpenReadWrite, Object: uint64(terminal)}) &&
			table.duplicateAtHeld(0, 1, false) && table.duplicateAtHeld(0, 2, false) {
			return true
		}
		table.closeAllHeld(nil)
		*table = FDTable{}
		return false
	}
	if table.install(0, FileDescription{Kind: FDStreamReader, Object: input}) &&
		table.install(1, FileDescription{Kind: FDStreamWriter, Object: output}) &&
		table.install(2, FileDescription{Kind: FDStreamWriter, Object: errorOutput}) {
		return true
	}
	table.closeAllHeld(nil)
	*table = FDTable{}
	return false
}

func (table *FDTable) inheritHeld(parent *FDTable) bool {
	guard.Check()
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
			table.closeAllHeld(nil)
			return false
		}
		slot.description.References++
		table.entries[fd] = entry
	}
	return true
}

func (table *FDTable) allocateHeld(description FileDescription, closeOnExec bool) (int, bool) {
	guard.Check()
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

func (table *FDTable) hasFreeHeld() bool {
	guard.Check()
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

func (table *FDTable) lookupHeld(fd int) (FileDescription, bool) {
	guard.Check()
	if table == nil || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return FileDescription{}, false
	}
	slot, valid := table.description(table.entries[fd].description)
	if !valid {
		return FileDescription{}, false
	}
	return slot.description, true
}

func (table *FDTable) descriptionHeld(fd int) (*FileDescription, bool) {
	guard.Check()
	if table == nil || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return nil, false
	}
	slot, valid := table.description(table.entries[fd].description)
	if !valid {
		return nil, false
	}
	return &slot.description, true
}

func (table *FDTable) descriptionIndexHeld(fd int) (int, bool) {
	guard.Check()
	if table == nil || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return 0, false
	}
	return int(table.entries[fd].description), true
}

func (table *FDTable) duplicateHeld(fd int, minimum int, closeOnExec bool) (int, bool) {
	guard.Check()
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

func (table *FDTable) duplicateAtHeld(fd int, target int, closeOnExec bool) bool {
	guard.Check()
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

func (table *FDTable) closeOnExecHeld(fd int) (bool, bool) {
	guard.Check()
	if table == nil || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return false, false
	}
	return table.entries[fd].CloseOnExec, true
}

func (table *FDTable) setCloseOnExecHeld(fd int, closeOnExec bool) bool {
	guard.Check()
	if table == nil || fd < 0 || fd >= LinuxMaxFD || !table.entries[fd].open {
		return false
	}
	table.entries[fd].CloseOnExec = closeOnExec
	return true
}

func (table *FDTable) closeHeld(fd int) (FileDescription, bool, bool) {
	guard.Check()
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

// Released collects the descriptions whose last reference went, which the
// caller releases once the Linux tables are no longer held.
type Released struct {
	count int
	// One more than the descriptors: an exiting session leader also gives
	// up its controlling terminal.
	items [LinuxMaxFD + 1]FileDescription
}

func (released *Released) add(description FileDescription) {
	if released != nil && released.count < len(released.items) {
		released.items[released.count] = description
		released.count++
	}
}

// Len is the number of descriptions collected.
func (released *Released) Len() int { return released.count }

// At returns the index-th description collected.
func (released *Released) At(index int) FileDescription { return released.items[index] }

// Reset empties the collection.
func (released *Released) Reset() { released.count = 0 }

func (table *FDTable) closeAllHeld(released *Released) {
	guard.Check()
	if table == nil {
		return
	}
	for fd := 0; fd < LinuxMaxFD; fd++ {
		if description, last, ok := table.closeHeld(fd); ok && last {
			released.add(description)
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
	return description.Kind > FDInvalid && description.Kind <= FDTerminal && description.Object != 0
}

// OpenDescriptionCount reports how many open-file descriptions exist in all
// families and how many descriptor references they hold together.
// Validation checks that a failed fork leaves both unchanged.
func OpenDescriptionCount() (descriptions int, references int) {
	tablesLock.Lock()
	result0, result1 := openDescriptionCountHeld()
	tablesLock.Unlock()
	return result0, result1
}

func (table *FDTable) Initialize(familyID uint64, input, output, errorOutput uint64) bool {
	tablesLock.Lock()
	result := table.initializeHeld(familyID, input, output, errorOutput)
	tablesLock.Unlock()
	return result
}

// Inherit copies descriptor integers and retains their shared open-file
// descriptions. Close-on-exec remains attached to each copied integer.
func (table *FDTable) Inherit(parent *FDTable) bool {
	tablesLock.Lock()
	result := table.inheritHeld(parent)
	tablesLock.Unlock()
	return result
}

func (table *FDTable) Allocate(description FileDescription, closeOnExec bool) (int, bool) {
	tablesLock.Lock()
	result0, result1 := table.allocateHeld(description, closeOnExec)
	tablesLock.Unlock()
	return result0, result1
}

// HasFree reports whether Allocate would find a descriptor. A caller checks
// before asking a service to open something it could not then name.
func (table *FDTable) HasFree() bool {
	tablesLock.Lock()
	result := table.hasFreeHeld()
	tablesLock.Unlock()
	return result
}

func (table *FDTable) Lookup(fd int) (FileDescription, bool) {
	tablesLock.Lock()
	result0, result1 := table.lookupHeld(fd)
	tablesLock.Unlock()
	return result0, result1
}

// Description returns the shared open-file description behind fd for
// updating its offset. Every descriptor that duplicates fd sees the change.
func (table *FDTable) Description(fd int) (*FileDescription, bool) {
	tablesLock.Lock()
	result0, result1 := table.descriptionHeld(fd)
	tablesLock.Unlock()
	return result0, result1
}

// DescriptionIndex identifies the description behind fd among those the
// table holds. Duplicated descriptors report the same index.
func (table *FDTable) DescriptionIndex(fd int) (int, bool) {
	tablesLock.Lock()
	result0, result1 := table.descriptionIndexHeld(fd)
	tablesLock.Unlock()
	return result0, result1
}

// Duplicate adds another integer, the lowest free one at or above minimum,
// for the same open-file description. It returns -2 when fd names nothing
// and -1 when no integer is free, so a caller can tell EBADF from EMFILE.
func (table *FDTable) Duplicate(fd int, minimum int, closeOnExec bool) (int, bool) {
	tablesLock.Lock()
	result0, result1 := table.duplicateHeld(fd, minimum, closeOnExec)
	tablesLock.Unlock()
	return result0, result1
}

// DuplicateAt makes target, which must be free, another integer for the
// description behind fd.
func (table *FDTable) DuplicateAt(fd int, target int, closeOnExec bool) bool {
	tablesLock.Lock()
	result := table.duplicateAtHeld(fd, target, closeOnExec)
	tablesLock.Unlock()
	return result
}

// CloseOnExec reports the close-on-exec flag of one descriptor. The flag
// belongs to the integer; descriptors that share a description keep their
// own.
func (table *FDTable) CloseOnExec(fd int) (bool, bool) {
	tablesLock.Lock()
	result0, result1 := table.closeOnExecHeld(fd)
	tablesLock.Unlock()
	return result0, result1
}

func (table *FDTable) SetCloseOnExec(fd int, closeOnExec bool) bool {
	tablesLock.Lock()
	result := table.setCloseOnExecHeld(fd, closeOnExec)
	tablesLock.Unlock()
	return result
}

// Close invalidates one Linux integer and reports the description only when
// its final reference is gone. The caller then releases any VFS-owned state.
func (table *FDTable) Close(fd int) (FileDescription, bool, bool) {
	tablesLock.Lock()
	result0, result1, result2 := table.closeHeld(fd)
	tablesLock.Unlock()
	return result0, result1, result2
}

// CloseAll closes every descriptor and collects in released the descriptions
// whose last reference went.
func (table *FDTable) CloseAll(released *Released) {
	tablesLock.Lock()
	table.closeAllHeld(released)
	tablesLock.Unlock()
}
