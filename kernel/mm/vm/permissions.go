package vm

const (
	PageSize         = uint64(4096)
	IdentityMapLimit = uint64(1 << 30)
	UserAddressLimit = uint64(2 << 30)
	KernelImageBase  = uint64(0x400000)
	UserProbeAddress = uint64(0x40000000)
	UserCodeAddress  = uint64(0x40002000)
	UserStackAddress = uint64(0x40003000)
	UserDataAddress  = uint64(0x40004000)
	UserDMAAddress   = uint64(0x401f0000)

	entryPresent  = uint64(1 << 0)
	entryWritable = uint64(1 << 1)
	entryUser     = uint64(1 << 2)
	// entryOwned uses an x86_64 software-available PTE bit. It marks private
	// process pages whose physical storage is reclaimed with the address space.
	entryOwned  = uint64(1 << 9)
	entryNX     = uint64(1 << 63)
	addressMask = uint64(0x000ffffffffff000)
)

// ValidateUserRange rejects wraparound and confines native user pointers to
// the portion of the bootstrap address layout reserved for userspace.
func ValidateUserRange(address uint64, length uint64) bool {
	if length == 0 {
		return address >= IdentityMapLimit && address < UserAddressLimit
	}
	end := address + length
	return address >= IdentityMapLimit && address < UserAddressLimit && end >= address && end <= UserAddressLimit
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
