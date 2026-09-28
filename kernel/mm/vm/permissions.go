package vm

const (
	PageSize         = uint64(4096)
	IdentityMapLimit = uint64(1 << 30)
	// UserAddressStart leaves the null page unmapped while allowing ordinary
	// low ELF addresses such as 0x00400000.
	UserAddressStart = PageSize
	// UserAddressLimit ends the user range: the canonical lower half, less its
	// last page, so no user mapping ends at the non-canonical boundary
	// (ADR 0104).
	UserAddressLimit = uint64(0x00007ffffffff000)
	KernelImageBase  = uint64(0x400000)
	UserProbeAddress = uint64(0x00400000)
	// UserCodeAddress and UserDataAddress are the fixed base used by the
	// current native linker. The loader also accepts images at lower addresses.
	UserCodeAddress = uint64(0x40002000)
	UserDataAddress = uint64(0x40004000)
	// UserStackAddress is the stack of the fixed self-test and fault-probe
	// layout. The 64 KiB below it are its unmapped guard gap (ADR 0083, ADR
	// 0088); it sits above the self-test's shared-memory pages at 0x40010000
	// so that the gap stays clear of them.
	UserStackAddress = uint64(0x40030000)
	// UserImageAreaLimit ends the range images may be linked into: static
	// small-code-model executables live below 2 GiB.
	UserImageAreaLimit = uint64(2 << 30)
	// Callers that map shared memory at a fixed address use
	// [UserSharedAddress, UserSharedLimit); any other free address works too.
	UserSharedAddress = uint64(0x0000080000000000)
	UserSharedLimit   = uint64(0x0000081000000000)
	UserDMAAddress    = uint64(0x0000081000000000)
	// The boot system image is mapped read-only inside
	// [UserImageAddress, UserImageLimit).
	UserImageAddress = uint64(0x0000082000000000)
	UserImageLimit   = uint64(0x0000082040000000)
	// UserStagingAddress is where PID 1 maps its launch staging pages.
	UserStagingAddress = uint64(0x0000083000000000)
	// UserStartupAddress holds a process's read-only startup object, at most
	// kcall.MaxStartupSize bytes (ADR 0109).
	UserStartupAddress = uint64(0x0000084000000000)
	// [UserMapBase, UserMapLimit) is where the kernel places mappings whose
	// caller lets it choose the address.
	UserMapBase  = uint64(0x0000100000000000)
	UserMapLimit = uint64(0x0000700000000000)
	// UserStackTop is the initial stack pointer of every process; its stack
	// reservation lies below it and its guard below that.
	UserStackTop = uint64(0x00007fffff000000)
	// LeafTableSpan is the address range one leaf page table covers.
	LeafTableSpan = uint64(2 << 20)

	entryPresent      = uint64(1 << 0)
	entryWritable     = uint64(1 << 1)
	entryUser         = uint64(1 << 2)
	entryWriteThrough = uint64(1 << 3)
	entryCacheDisable = uint64(1 << 4)
	entryAccessed     = uint64(1 << 5)
	entryDirty        = uint64(1 << 6)
	// entryOwned uses an x86_64 software-available PTE bit. It marks private
	// process pages whose physical storage is reclaimed with the address space.
	entryOwned  = uint64(1 << 9)
	entryNX     = uint64(1 << 63)
	addressMask = uint64(0x000ffffffffff000)
)

// ValidateUserRange rejects wraparound and confines native user pointers to
// the lower canonical half above the null page.
func ValidateUserRange(address uint64, length uint64) bool {
	if length == 0 {
		return address >= UserAddressStart && address < UserAddressLimit
	}
	end := address + length
	return address >= UserAddressStart && address < UserAddressLimit && end >= address && end <= UserAddressLimit
}

type Permissions struct {
	Writable   bool
	Executable bool
	User       bool
}

func (permissions Permissions) Valid() bool {
	return !permissions.Writable || !permissions.Executable
}

func pageEntry(physical uint64, permissions Permissions) (uint64, bool) {
	if physical%PageSize != 0 || !permissions.Valid() {
		return 0, false
	}
	entry := physical&addressMask | entryPresent
	if permissions.Writable {
		entry |= entryWritable
	}
	if permissions.User {
		entry |= entryUser
	}
	if !permissions.Executable {
		entry |= entryNX
	}
	return entry, true
}

// uncachedPageEntry uses the architectural PCD+PWT combination. With the x86
// reset PAT this selects the UC memory type for device memory. The mapping is
// supervisor-only, writable and NX.
func uncachedPageEntry(physical uint64) (uint64, bool) {
	entry, ok := pageEntry(physical, Permissions{Writable: true})
	if !ok || physical&^addressMask != 0 {
		return 0, false
	}
	return entry | entryWriteThrough | entryCacheDisable, true
}
