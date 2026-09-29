package linux

import "goos/kernel/compat/linux/abi"

// Dispatch is the bounded Linux syscall translation boundary. Phase 2 installs
// the entry path only; semantic syscall implementations begin in later phases.
func Dispatch(frame *abi.Frame) uint64 {
	if frame == nil {
		return abi.Negative(abi.EINVAL)
	}
	return abi.Negative(abi.ENOSYS)
}
