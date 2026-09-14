package vm

const (
	PageSize         = uint64(4096)
	IdentityMapLimit = uint64(1 << 30)
	KernelImageBase  = uint64(0x400000)

	entryPresent  = uint64(1 << 0)
	entryWritable = uint64(1 << 1)
	entryUser     = uint64(1 << 2)
	entryNX       = uint64(1 << 63)
	addressMask   = uint64(0x000ffffffffff000)
)

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
