package linux

import (
	"goos/kernel/compat/linux/abi"
	"goos/kernel/objects"
	"goos/kernel/process"
)

// terminalStandardStreams marks, in the output and error arguments of
// initializeHeld, a table whose standard descriptors are the terminal the
// input argument names.
const terminalStandardStreams = ^uint64(0)

// Descriptor table sizes. A table keeps its first fdInlineEntries entries in
// itself and takes one page of FDChunkEntries entries from the page allocator
// each time a descriptor lands past what it holds, charged to the process
// that owns the table. A process with a handful of descriptors costs no
// pages; one that opens a thousand costs one.
const (
	// DefaultFDSoftLimit and DefaultFDHardLimit are the RLIMIT_NOFILE a Linux
	// launch starts with, the defaults of a Linux login.
	DefaultFDSoftLimit = 1024
	DefaultFDHardLimit = 4096
	// LinuxFDCeiling is the most descriptors a table may ever hold, the
	// nr_open of Linux. prlimit refuses a hard limit above it with EPERM.
	LinuxFDCeiling = 4096
	// FDChunkEntries is the number of entries one page of a table holds.
	FDChunkEntries  = 1024
	fdInlineEntries = 64
	fdChunkSlots    = (LinuxFDCeiling - fdInlineEntries + FDChunkEntries - 1) / FDChunkEntries
	// MaxOpenDescriptions bounds the open-file descriptions of every family
	// together, the file-max of Linux. A full pool is ENFILE.
	MaxOpenDescriptions = 4096
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
	// FDSocket is an IPv4 socket. Object names its entry in the family's
	// socket table, never the service's identity or a native handle.
	FDSocket
	// FDEvent is an eventfd counter. Object names its entry in the
	// kernel's counter table.
	FDEvent
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

// FDChunk is one page of descriptor entries.
type FDChunk struct {
	entries [FDChunkEntries]FDEntry
}

// fdStorage supplies the pages tables grow by. The kernel installs page
// storage at boot; host tests use the Go heap.
var fdStorage objects.Storage[FDChunk]

// SetFDStorage installs where descriptor tables take their pages. The kernel
// calls it once at boot, before the first Linux launch.
func SetFDStorage(storage objects.Storage[FDChunk]) { fdStorage = storage }

type descriptionSlot struct {
	familyID    uint64
	description FileDescription
	// next links a parked slot to the next one in its Released list, as
	// an index plus one.
	next uint16
	// marks counts close-on-exec descriptors while closeOnExecVFSHeld runs;
	// it is zero at every other time.
	marks uint16
	// parked marks a description whose last reference went but whose slot
	// a Released still holds for the caller to read.
	parked bool
}

var openDescriptions [MaxOpenDescriptions]descriptionSlot

func openDescriptionCountHeld() (descriptions int, references int) {
	guard.Check()
	for index := 0; index < len(openDescriptions); index++ {
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
//
// A table owns its pages. Copying a table by value aliases them, so the
// personality only moves tables through Inherit and releases them through
// the close-all paths.
type FDTable struct {
	familyID uint64
	// owner pays for the table's pages. ownerID guards against a process
	// slot reused by a later process: a charge or release then goes nowhere.
	owner   *process.Process
	ownerID process.ID
	// soft and hard are RLIMIT_NOFILE. A new descriptor must be below soft.
	soft int32
	hard int32
	// free is a descriptor below which every descriptor is open, the start
	// of the search for the lowest free one.
	free   int32
	chunks uint8
	inline [fdInlineEntries]FDEntry
	pages  [fdChunkSlots]*FDChunk
}

// capacity is the number of descriptors the table has entries for.
func (table *FDTable) capacity() int {
	return fdInlineEntries + int(table.chunks)*FDChunkEntries
}

// entry returns the entry of fd, or nil when the table has none for it.
func (table *FDTable) entry(fd int) *FDEntry {
	if fd < 0 || fd >= table.capacity() {
		return nil
	}
	if fd < fdInlineEntries {
		return &table.inline[fd]
	}
	fd -= fdInlineEntries
	return &table.pages[fd/FDChunkEntries].entries[fd%FDChunkEntries]
}

// openEntry returns the entry of fd when fd is open.
func (table *FDTable) openEntry(fd int) *FDEntry {
	if table == nil {
		return nil
	}
	if entry := table.entry(fd); entry != nil && entry.open {
		return entry
	}
	return nil
}

// growTo gives the table entries up to and including fd. A failed page or a
// charge the owner's quota refuses is ENOMEM, as a failed expand_files is on
// Linux.
func (table *FDTable) growTo(fd int) abi.Errno {
	if fd >= LinuxFDCeiling {
		return abi.EMFILE
	}
	for fd >= table.capacity() {
		if !table.grow() {
			return abi.ENOMEM
		}
	}
	return 0
}

// grow adds one page of entries.
func (table *FDTable) grow() bool {
	if int(table.chunks) >= fdChunkSlots || !table.charge() {
		return false
	}
	chunk, ok := objects.Or(fdStorage).New()
	if !ok || chunk == nil {
		table.uncharge()
		return false
	}
	table.pages[table.chunks] = chunk
	table.chunks++
	return true
}

func (table *FDTable) charge() bool {
	if table.owner == nil {
		return true
	}
	return table.owner.ID() == table.ownerID && table.owner.Charge(process.ResourcePages, 1)
}

func (table *FDTable) uncharge() {
	if table.owner != nil && table.owner.ID() == table.ownerID {
		table.owner.Release(process.ResourcePages, 1)
	}
}

// releasePages returns every page of a table whose descriptors are closed.
func (table *FDTable) releasePages() {
	for table.chunks > 0 {
		table.chunks--
		objects.Or(fdStorage).Free(table.pages[table.chunks])
		table.pages[table.chunks] = nil
		table.uncharge()
	}
}

// lowestFree finds the lowest descriptor at or above minimum that is not
// open and lies below the soft limit, growing the table to reach it.
func (table *FDTable) lowestFree(minimum int) (int, abi.Errno) {
	start := minimum
	fromHint := minimum <= int(table.free)
	if fromHint {
		start = int(table.free)
	}
	limit := int(table.soft)
	for fd := start; fd < limit; fd++ {
		entry := table.entry(fd)
		if entry == nil {
			if errno := table.growTo(fd); errno != 0 {
				return -1, errno
			}
			entry = table.entry(fd)
		}
		if entry.open {
			continue
		}
		if fromHint {
			table.free = int32(fd)
		}
		return fd, 0
	}
	return -1, abi.EMFILE
}

// opened records that fd became open.
func (table *FDTable) opened(fd int) {
	if fd == int(table.free) {
		table.free++
	}
}

func (table *FDTable) initializeHeld(familyID uint64, input, output, errorOutput uint64) bool {
	guard.Check()
	if table == nil || familyID == 0 || input == 0 || output == 0 || errorOutput == 0 {
		return false
	}
	table.reset(familyID)
	if terminal := TerminalID(input); errorOutput == terminalStandardStreams && output == terminalStandardStreams {
		// One read-write description on the terminal, as a login gets: the
		// three integers share it.
		if table.install(0, FileDescription{Kind: FDTerminal, Flags: abi.OpenReadWrite, Object: uint64(terminal)}) == 0 &&
			table.duplicateAtHeld(0, 1, false) == 0 && table.duplicateAtHeld(0, 2, false) == 0 {
			return true
		}
		table.closeAllHeld(nil)
		*table = FDTable{}
		return false
	}
	if table.install(0, FileDescription{Kind: FDStreamReader, Object: input}) == 0 &&
		table.install(1, FileDescription{Kind: FDStreamWriter, Object: output}) == 0 &&
		table.install(2, FileDescription{Kind: FDStreamWriter, Object: errorOutput}) == 0 {
		return true
	}
	table.closeAllHeld(nil)
	*table = FDTable{}
	return false
}

// reset prepares an empty table, which holds no pages, for a family.
func (table *FDTable) reset(familyID uint64) {
	*table = FDTable{}
	table.familyID = familyID
	table.soft, table.hard = DefaultFDSoftLimit, DefaultFDHardLimit
}

// inheritHeld makes table a copy of parent for a forked child that owner
// pays for. Both tables then share every open-file description, and each
// integer keeps its close-on-exec flag.
func (table *FDTable) inheritHeld(parent *FDTable, owner *process.Process) bool {
	guard.Check()
	if table == nil || parent == nil || table == parent || parent.familyID == 0 {
		return false
	}
	*table = FDTable{familyID: parent.familyID, soft: parent.soft, hard: parent.hard, free: parent.free}
	if owner != nil {
		table.owner, table.ownerID = owner, owner.ID()
	}
	for table.chunks < parent.chunks {
		if !table.grow() {
			table.releasePages()
			*table = FDTable{}
			return false
		}
	}
	for fd := 0; fd < parent.capacity(); fd++ {
		entry := parent.entry(fd)
		if !entry.open {
			continue
		}
		slot, valid := parent.description(entry.description)
		if !valid || slot.description.References == ^uint16(0) {
			table.closeAllHeld(nil)
			*table = FDTable{}
			return false
		}
		slot.description.References++
		*table.entry(fd) = *entry
	}
	return true
}

// setOwnerHeld names the process that pays for the table's pages. A table
// that already holds pages keeps the owner that paid for them.
func (table *FDTable) setOwnerHeld(owner *process.Process) bool {
	guard.Check()
	if table == nil || owner == nil || table.chunks != 0 {
		return false
	}
	table.owner, table.ownerID = owner, owner.ID()
	return true
}

func (table *FDTable) allocateHeld(description FileDescription, closeOnExec bool) (int, abi.Errno) {
	guard.Check()
	if table == nil || table.familyID == 0 || !description.valid() {
		return -1, abi.EINVAL
	}
	fd, errno := table.lowestFree(0)
	if errno != 0 {
		return -1, errno
	}
	if errno := table.install(fd, description); errno != 0 {
		return -1, errno
	}
	table.entry(fd).CloseOnExec = closeOnExec
	return fd, 0
}

// reserveHeld makes sure the next allocation finds a descriptor with an
// entry behind it. It reports EMFILE at the soft limit and ENOMEM when the
// table cannot grow.
func (table *FDTable) reserveHeld() abi.Errno {
	guard.Check()
	if table == nil || table.familyID == 0 {
		return abi.EBADF
	}
	_, errno := table.lowestFree(0)
	return errno
}

func (table *FDTable) lookupHeld(fd int) (FileDescription, bool) {
	guard.Check()
	entry := table.openEntry(fd)
	if entry == nil {
		return FileDescription{}, false
	}
	slot, valid := table.description(entry.description)
	if !valid {
		return FileDescription{}, false
	}
	return slot.description, true
}

func (table *FDTable) descriptionHeld(fd int) (*FileDescription, bool) {
	guard.Check()
	entry := table.openEntry(fd)
	if entry == nil {
		return nil, false
	}
	slot, valid := table.description(entry.description)
	if !valid {
		return nil, false
	}
	return &slot.description, true
}

func (table *FDTable) descriptionIndexHeld(fd int) (int, bool) {
	guard.Check()
	entry := table.openEntry(fd)
	if entry == nil {
		return 0, false
	}
	return int(entry.description), true
}

// duplicateHeld is F_DUPFD: EBADF when fd is not open, EINVAL for a minimum
// outside the soft limit, EMFILE when no descriptor is free.
func (table *FDTable) duplicateHeld(fd int, minimum int, closeOnExec bool) (int, abi.Errno) {
	guard.Check()
	source := table.openEntry(fd)
	if source == nil {
		return -1, abi.EBADF
	}
	if minimum < 0 || minimum >= int(table.soft) {
		return -1, abi.EINVAL
	}
	descriptionIndex := source.description
	slot, valid := table.description(descriptionIndex)
	if !valid {
		return -1, abi.EBADF
	}
	if slot.description.References == ^uint16(0) {
		return -1, abi.EMFILE
	}
	target, errno := table.lowestFree(minimum)
	if errno != 0 {
		return -1, errno
	}
	slot.description.References++
	*table.entry(target) = FDEntry{description: descriptionIndex, CloseOnExec: closeOnExec, open: true}
	table.opened(target)
	return target, 0
}

// duplicateAtHeld makes target, which must be free, another integer for the
// description behind fd. A target at or above the soft limit is EBADF, as
// dup2 reports it on Linux.
func (table *FDTable) duplicateAtHeld(fd int, target int, closeOnExec bool) abi.Errno {
	guard.Check()
	source := table.openEntry(fd)
	if source == nil || target < 0 || target >= int(table.soft) {
		return abi.EBADF
	}
	descriptionIndex := source.description
	slot, valid := table.description(descriptionIndex)
	if !valid {
		return abi.EBADF
	}
	if slot.description.References == ^uint16(0) {
		return abi.EMFILE
	}
	if errno := table.growTo(target); errno != 0 {
		return errno
	}
	destination := table.entry(target)
	if destination.open {
		return abi.EBUSY
	}
	slot.description.References++
	*destination = FDEntry{description: descriptionIndex, CloseOnExec: closeOnExec, open: true}
	table.opened(target)
	return 0
}

func (table *FDTable) closeOnExecHeld(fd int) (bool, bool) {
	guard.Check()
	entry := table.openEntry(fd)
	if entry == nil {
		return false, false
	}
	return entry.CloseOnExec, true
}

func (table *FDTable) setCloseOnExecHeld(fd int, closeOnExec bool) bool {
	guard.Check()
	entry := table.openEntry(fd)
	if entry == nil {
		return false
	}
	entry.CloseOnExec = closeOnExec
	return true
}

// dropHeld closes fd and reports the slot of its description, and whether
// that was the last reference. A last reference leaves the slot taken: the
// caller frees or parks it.
func (table *FDTable) dropHeld(fd int) (uint16, bool, bool) {
	entry := table.openEntry(fd)
	if entry == nil {
		return 0, false, false
	}
	index := entry.description
	slot, valid := table.description(index)
	if !valid {
		return 0, false, false
	}
	*entry = FDEntry{}
	if int32(fd) < table.free {
		table.free = int32(fd)
	}
	slot.description.References--
	return index, slot.description.References == 0, true
}

func (table *FDTable) closeHeld(fd int) (FileDescription, bool, bool) {
	guard.Check()
	index, last, ok := table.dropHeld(fd)
	if !ok {
		return FileDescription{}, false, false
	}
	if !last {
		return FileDescription{}, false, true
	}
	released := openDescriptions[index].description
	openDescriptions[index] = descriptionSlot{}
	return released, true, true
}

// Released collects the descriptions whose last reference went, which the
// caller releases once the Linux tables are no longer held. A description
// stays in its slot, parked, until Reset: the collection costs nothing per
// descriptor, however many a table held.
type Released struct {
	// items holds descriptions that have no slot: an exiting session
	// leader's controlling terminal.
	count int
	items [2]FileDescription
	// parked lists the slots, as index plus one, from head through next.
	parked int
	head   uint16
	tail   uint16
	// cursor and cursorSlot remember where At stopped, so a caller that
	// reads in order walks the list once.
	cursor     int
	cursorSlot uint16
}

func (released *Released) add(description FileDescription) {
	if released != nil && released.count < len(released.items) {
		released.items[released.count] = description
		released.count++
	}
}

// park keeps a slot whose last reference went for the caller to read, or
// frees it when there is no collection.
func (released *Released) park(index uint16) {
	slot := &openDescriptions[index]
	if released == nil {
		*slot = descriptionSlot{}
		return
	}
	slot.parked, slot.next = true, 0
	if released.tail == 0 {
		released.head = index + 1
	} else {
		openDescriptions[released.tail-1].next = index + 1
	}
	released.tail = index + 1
	released.parked++
}

// Len is the number of descriptions collected.
func (released *Released) Len() int { return released.count + released.parked }

// At returns the index-th description collected.
func (released *Released) At(index int) FileDescription {
	if index < released.count {
		return released.items[index]
	}
	position := index - released.count
	slot := released.head
	if released.cursorSlot != 0 && position == released.cursor+1 {
		slot = openDescriptions[released.cursorSlot-1].next
	} else {
		for step := 0; step < position && slot != 0; step++ {
			slot = openDescriptions[slot-1].next
		}
	}
	if slot == 0 {
		return FileDescription{}
	}
	released.cursor, released.cursorSlot = position, slot
	return openDescriptions[slot-1].description
}

// Reset frees the parked slots and empties the collection.
func (released *Released) Reset() {
	if released.parked != 0 {
		tablesLock.Lock()
		for slot := released.head; slot != 0; {
			next := openDescriptions[slot-1].next
			openDescriptions[slot-1] = descriptionSlot{}
			slot = next
		}
		tablesLock.Unlock()
	}
	*released = Released{}
}

// closeAllHeld closes every descriptor, collects in released the
// descriptions whose last reference went, and returns the table's pages.
func (table *FDTable) closeAllHeld(released *Released) {
	guard.Check()
	if table == nil {
		return
	}
	for fd := 0; fd < table.capacity(); fd++ {
		if index, last, ok := table.dropHeld(fd); ok && last {
			released.park(index)
		}
	}
	table.releasePages()
}

func (table *FDTable) install(fd int, description FileDescription) abi.Errno {
	if table == nil || table.familyID == 0 || !description.valid() {
		return abi.EINVAL
	}
	entry := table.entry(fd)
	if entry == nil || entry.open {
		return abi.EBADF
	}
	for index := 0; index < len(openDescriptions); index++ {
		if openDescriptions[index].familyID != 0 {
			continue
		}
		description.References = 1
		openDescriptions[index] = descriptionSlot{familyID: table.familyID, description: description}
		*entry = FDEntry{description: uint16(index), open: true}
		table.opened(fd)
		return 0
	}
	return abi.ENFILE
}

func (table *FDTable) description(index uint16) (*descriptionSlot, bool) {
	if table == nil || table.familyID == 0 || int(index) >= len(openDescriptions) {
		return nil, false
	}
	slot := &openDescriptions[index]
	if slot.familyID != table.familyID || slot.parked || !slot.description.valid() || slot.description.References == 0 {
		return nil, false
	}
	return slot, true
}

func (description FileDescription) valid() bool {
	return description.Kind > FDInvalid && description.Kind <= FDEvent && description.Object != 0
}

// limitsHeld reports RLIMIT_NOFILE.
func (table *FDTable) limitsHeld() (int, int) {
	guard.Check()
	if table == nil {
		return 0, 0
	}
	return int(table.soft), int(table.hard)
}

// setLimitsHeld changes RLIMIT_NOFILE as an unprivileged Linux process may:
// a soft limit above the hard one is EINVAL, and raising the hard limit or
// setting it past LinuxFDCeiling is EPERM. Descriptors already open above a
// lowered soft limit stay open.
func (table *FDTable) setLimitsHeld(soft, hard uint64) abi.Errno {
	guard.Check()
	if table == nil {
		return abi.ESRCH
	}
	if soft > hard {
		return abi.EINVAL
	}
	if hard > LinuxFDCeiling || hard > uint64(table.hard) {
		return abi.EPERM
	}
	table.soft, table.hard = int32(soft), int32(hard)
	return 0
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
// descriptions. Close-on-exec remains attached to each copied integer. owner
// pays for the pages of the copy; nil charges no one.
func (table *FDTable) Inherit(parent *FDTable, owner *process.Process) bool {
	tablesLock.Lock()
	result := table.inheritHeld(parent, owner)
	tablesLock.Unlock()
	return result
}

// SetOwner names the process charged for the pages the table takes as it
// grows. It refuses once the table holds pages.
func (table *FDTable) SetOwner(owner *process.Process) bool {
	tablesLock.Lock()
	result := table.setOwnerHeld(owner)
	tablesLock.Unlock()
	return result
}

// Allocate installs a new description on the lowest free descriptor. It
// reports EMFILE at the soft limit, ENOMEM when the table cannot grow, and
// ENFILE when the system holds MaxOpenDescriptions descriptions.
func (table *FDTable) Allocate(description FileDescription, closeOnExec bool) (int, abi.Errno) {
	tablesLock.Lock()
	result0, result1 := table.allocateHeld(description, closeOnExec)
	tablesLock.Unlock()
	return result0, result1
}

// Reserve reports whether Allocate would find a descriptor, growing the
// table if it has to. A caller checks before asking a service to open
// something it could not then name.
func (table *FDTable) Reserve() abi.Errno {
	tablesLock.Lock()
	result := table.reserveHeld()
	tablesLock.Unlock()
	return result
}

// HasFree is Reserve as a yes or no.
func (table *FDTable) HasFree() bool { return table.Reserve() == 0 }

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
// for the same open-file description: EBADF when fd names nothing, EINVAL
// when minimum is outside the soft limit, EMFILE when no integer is free,
// and ENOMEM when the table cannot grow.
func (table *FDTable) Duplicate(fd int, minimum int, closeOnExec bool) (int, abi.Errno) {
	tablesLock.Lock()
	result0, result1 := table.duplicateHeld(fd, minimum, closeOnExec)
	tablesLock.Unlock()
	return result0, result1
}

// DuplicateAt makes target, which must be free, another integer for the
// description behind fd.
func (table *FDTable) DuplicateAt(fd int, target int, closeOnExec bool) abi.Errno {
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

// CloseAll closes every descriptor, collects in released the descriptions
// whose last reference went, and returns the table's pages.
func (table *FDTable) CloseAll(released *Released) {
	tablesLock.Lock()
	table.closeAllHeld(released)
	tablesLock.Unlock()
}

// Capacity is the number of descriptors the table holds entries for. select
// reads no further: a descriptor past it cannot be open.
func (table *FDTable) Capacity() int {
	tablesLock.Lock()
	result := 0
	if table != nil {
		result = table.capacity()
	}
	tablesLock.Unlock()
	return result
}

// Limits reports RLIMIT_NOFILE, soft and hard.
func (table *FDTable) Limits() (int, int) {
	tablesLock.Lock()
	result0, result1 := table.limitsHeld()
	tablesLock.Unlock()
	return result0, result1
}

// SetLimits changes RLIMIT_NOFILE; see setLimitsHeld.
func (table *FDTable) SetLimits(soft, hard uint64) abi.Errno {
	tablesLock.Lock()
	result := table.setLimitsHeld(soft, hard)
	tablesLock.Unlock()
	return result
}
