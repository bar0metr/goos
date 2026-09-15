//go:build uefi && amd64

package amd64

import (
	"goos/kernel/mm/pmm"
	"unsafe"
)

const (
	kernelStackPages = uint64(4)
	stackGuardPages  = uint64(1)
	contextWords     = 20
	initialRFlags    = uint64(0x202)
)

//go:extern goos_task_entry_table
var taskEntryTable [3]uintptr

// SetupBootstrapScheduler creates contexts for two demonstration tasks and one
// idle task. The currently executing boot task becomes slot zero on the first
// timer interrupt.
func SetupBootstrapScheduler(allocator *pmm.Allocator, pageTable uint64) bool {
	var contexts [3]uint64
	for index := range contexts {
		if taskEntryTable[index] == 0 {
			return false
		}
		allocation, ok := allocator.AllocateContiguous(kernelStackPages + stackGuardPages)
		if !ok {
			return false
		}
		if !unmapGuardPage(pageTable, allocation) {
			return false
		}
		stackStart := allocation + stackGuardPages*pmm.PageSize
		contexts[index] = initialContext(stackStart, taskEntryTable[index])
	}
	installScheduler(contexts[0], contexts[1], contexts[2], pageTable)
	return true
}

// AddUserTask installs a runnable ring 3 context with its own address-space
// root and guarded kernel-entry stack. The scheduler updates TSS.RSP0 before
// selecting this task, so userspace interrupt frames remain task-local.
func AddUserTask(allocator *pmm.Allocator, pageTable uint64, entry uint64, userStack uint64) (uint64, bool) {
	if pageTable == 0 || entry == 0 || userStack == 0 {
		return 0, false
	}
	allocation, ok := allocator.AllocateContiguous(kernelStackPages + stackGuardPages)
	if !ok || !unmapGuardPage(pageTable, allocation) {
		return 0, false
	}
	stackStart := allocation + stackGuardPages*pmm.PageSize
	kernelStackTop := stackStart + kernelStackPages*pmm.PageSize - 64
	taskID := installUserTask(initialUserContext(stackStart, entry, userStack), pageTable, kernelStackTop)
	return taskID, taskID != 0
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

func initialUserContext(stackStart uint64, entry uint64, userStack uint64) uint64 {
	stackEnd := stackStart + kernelStackPages*pmm.PageSize
	contextAddress := stackEnd - 64 - contextWords*8
	// SAFETY: the guarded allocation owns four mapped kernel-stack pages. The
	// synthetic context lies wholly below the reserved interrupt-entry top.
	context := (*[contextWords]uint64)(unsafe.Pointer(uintptr(contextAddress)))
	for index := range context {
		context[index] = 0
	}
	context[15] = entry
	context[16] = uint64(userCodeSelector)
	context[17] = initialRFlags
	context[18] = userStack
	context[19] = uint64(userDataSelector)
	return contextAddress
}

//export goos_scheduler_install
func installScheduler(taskA uint64, taskB uint64, idle uint64, kernelRoot uint64)

//export goos_scheduler_install_user
func installUserTask(context uint64, pageTable uint64, kernelStackTop uint64) uint64

//export goos_user_preemptions_read
func UserPreemptions() uint64

//export goos_user_faults_read
func UserFaults() uint64

//export goos_block_current_task
func BlockCurrentTask()

//export goos_wake_task
func WakeTask(index uint64) bool

//export goos_invalidate_page
func invalidatePage(address uint64)
