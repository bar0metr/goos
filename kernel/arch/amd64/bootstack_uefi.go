//go:build uefi && amd64

package amd64

import (
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
)

var bootStackBottom, bootStackTop uint64

// RunOnBootStack moves the boot task to a stack of BootStackSize with an
// unmapped guard of BootStackGuardSize below it and continues there in the
// kernel's goos_boot_continue, which never returns. It returns false, still on
// the old stack, only when the stack cannot be made.
//
// The old stack is the goroutine stack TinyGo's runtime allocated for main.
// Nothing returns to it: the kernel's garbage collector is the leaking one,
// which scans no stacks, and the boot task never yields to TinyGo's scheduler,
// so no runtime code looks at the goroutine's recorded stack again.
func RunOnBootStack(allocator *pmm.Allocator, pageTable uint64) bool {
	guardPages := uint64(BootStackGuardSize / pmm.PageSize)
	allocation, ok := allocator.AllocateContiguous(BootStackSize/pmm.PageSize + guardPages)
	if !ok {
		return false
	}
	for page := uint64(0); page < guardPages; page++ {
		guard := allocation + page*pmm.PageSize
		if !vm.UnmapPage(pageTable, guard) {
			return false
		}
		invalidatePage(guard)
		if vm.IsMapped(pageTable, guard) {
			return false
		}
	}
	bootStackBottom = allocation + guardPages*pmm.PageSize
	bootStackTop = bootStackBottom + BootStackSize
	switchToBootStack(bootStackTop)
	return true
}

// BootStackGuarded reports whether the caller runs on the boot stack and the
// whole guard below it is unmapped in pageTable. The validation build checks
// it.
func BootStackGuarded(pageTable uint64) bool {
	stack := StackPointer()
	if bootStackTop == 0 || stack < bootStackBottom || stack >= bootStackTop {
		return false
	}
	for guard := bootStackBottom - BootStackGuardSize; guard < bootStackBottom; guard += pmm.PageSize {
		if vm.IsMapped(pageTable, guard) {
			return false
		}
	}
	return true
}

// switchToBootStack loads top into the stack pointer and calls
// goos_boot_continue on it. It does not return.
//
//export goos_switch_to_boot_stack
func switchToBootStack(top uint64)
