//go:build goos_userspace

package startup

import (
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/userspace/runtime/sys"
)

// Read returns the calling process's startup object. Only its header is
// copied: the object stays where the kernel mapped it, read-only, and the
// result aliases those pages.
func Read() (Data, bool) {
	var header [HeaderSize]byte
	if sys.StartupRead(header[:]) != kcall.StatusOK {
		return Data{}, false
	}
	size, ok := Size(header[:])
	if !ok {
		return Data{}, false
	}
	return Decode(sys.MappedAtStart(vm.UserStartupAddress, uint64(size)))
}
