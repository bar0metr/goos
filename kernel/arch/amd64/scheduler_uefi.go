//go:build uefi && amd64

package amd64

import (
	"goos/kernel/mm/pmm"
	"unsafe"
)

const (
	kernelStackPages = uint64(4)
	contextWords     = 20
	initialRFlags    = uint64(0x202)
)

//go:extern goos_task_entry_table
var taskEntryTable [3]uintptr

// SetupBootstrapScheduler creates contexts for two demonstration tasks and one
// idle task. The currently executing boot task becomes slot zero on the first
// timer interrupt.
func SetupBootstrapScheduler(allocator *pmm.Allocator) bool {
	var contexts [3]uint64
	for index := range contexts {
		if taskEntryTable[index] == 0 {
			return false
		}
		stackStart, ok := allocator.AllocateContiguous(kernelStackPages)
		if !ok {
			return false
		}
		contexts[index] = initialContext(stackStart, taskEntryTable[index])
	}
	installScheduler(contexts[0], contexts[1], contexts[2])
	return true
}

func initialContext(stackStart uint64, entry uintptr) uint64 {
	stackEnd := stackStart + kernelStackPages*pmm.PageSize
	// Keep Win64 home space above the initial RSP and preserve the required
	// eight-byte function-entry alignment even though IRETQ has no CALL site.
	initialRSP := stackEnd - 56
	contextAddress := initialRSP - contextWords*8
	// SAFETY: AllocateContiguous returned four exclusively owned identity-mapped
	// pages. The 144-byte frame and 56-byte ABI reserve are within that range.
	context := (*[contextWords]uint64)(unsafe.Pointer(uintptr(contextAddress)))
	for index := range context {
		context[index] = 0
	}
	context[15] = uint64(entry)
	context[16] = uint64(kernelCodeSelector)
	context[17] = initialRFlags
	context[18] = initialRSP
	context[19] = 0x10
	return contextAddress
}

//export goos_scheduler_install
func installScheduler(taskA uint64, taskB uint64, idle uint64)
