//go:build uefi && amd64

package abi

import (
	"goos/kernel/compat/linux/elf"
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
)

var initialStackPage [vm.PageSize]byte

// InstallInitialStack materializes only the pages occupied by an InitialStack.
// The rest of the Linux stack reservation remains absent and demand-zero. A
// failure may leave owned pages mapped; the launch caller destroys the address
// space, as it does for a failed native load.
func InstallInitialStack(allocator *pmm.Allocator, root uint64, stack InitialStack) bool {
	if allocator == nil || root == 0 || stack.Top != vm.UserStackTop || stack.Pointer%16 != 0 ||
		stack.Pointer < stack.Top-elf.DefaultStackReserve || stack.Pointer >= stack.Top ||
		uint64(len(stack.Data)) != stack.Top-stack.Pointer {
		return false
	}
	first := stack.Pointer &^ (vm.PageSize - 1)
	for page := first; page < stack.Top; page += vm.PageSize {
		if vm.UserPageMapped(root, page) {
			return false
		}
	}
	for page := first; page < stack.Top; page += vm.PageSize {
		clear(initialStackPage[:])
		copyStart := max(page, stack.Pointer)
		copyEnd := min(page+vm.PageSize, stack.Top)
		copy(initialStackPage[copyStart-page:copyEnd-page], stack.Data[copyStart-stack.Pointer:copyEnd-stack.Pointer])
		physical, allocated := allocator.Allocate()
		if !allocated {
			return false
		}
		if !vm.InitializePhysical(physical, initialStackPage[:]) {
			allocator.Free(physical)
			return false
		}
		if _, mapped := vm.MapUserPageAt(allocator, root, page, physical, vm.Permissions{User: true, Writable: true}, true); !mapped {
			allocator.Free(physical)
			return false
		}
	}
	return true
}
