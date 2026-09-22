//go:build uefi && amd64

package amd64

import (
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
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

// taskFPU holds the FXSAVE image of every scheduler slot. Every switch saves
// the outgoing task's SSE and x87 state and restores the incoming one's.
//
//go:extern goos_task_fpu
var taskFPU [64][FPUAreaSize]byte

// SetupBootstrapScheduler creates contexts for two demonstration tasks and one
// idle task. The currently executing boot task becomes slot zero on the first
// timer interrupt.
func SetupBootstrapScheduler(allocator *pmm.Allocator, pageTable uint64) bool {
	for slot := range taskFPU {
		InitialFPUArea(&taskFPU[slot])
	}
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

// KernelSleep blocks the calling kernel task and switches away at once instead
// of halting until the next tick. It returns after a wake, with the caller's
// interrupt state restored.
//
//export goos_kernel_sleep
func KernelSleep()

// DisableInterrupts enters the uniprocessor kernel exclusion domain that native
// calls already run in, and returns the flags RestoreInterrupts needs to leave it.
//
//export goos_interrupts_disable
func DisableInterrupts() uint64

//export goos_interrupts_restore
func RestoreInterrupts(flags uint64)

// SchedulerState is a snapshot of the architecture run queue.
type SchedulerState struct {
	Runnable uint64
	Dead     uint64
	Count    uint64
	Current  uint64
}

//export goos_scheduler_state_read
func readSchedulerState(state *SchedulerState)

// TaskStackTop returns the kernel-entry stack top of a user task slot, or zero
// for kernel tasks and unused slots.
//
//export goos_task_rsp0_read
func TaskStackTop(slot uint64) uint64

func ReadSchedulerState() SchedulerState {
	var state SchedulerState
	readSchedulerState(&state)
	return state
}

//export goos_wake_task
func WakeTask(index uint64) bool

//export goos_wake_task_id
func WakeTaskID(task uint64) bool

//export goos_task_id_for_slot
func TaskIDForSlot(slot uint64) uint64

//export goos_set_current_task_exit_code
func SetCurrentTaskExitCode(code uint64)

//export goos_take_exited_task
func TakeExitedTask() uint64

//export goos_task_exit_code
func TaskExitCode(task uint64) uint64

//export goos_reclaim_exited_task
func reclaimExitedTask(task uint64) uint64

// ReclaimExitedTask releases the guarded kernel stack after the dead task's
// exit record has been consumed. Its architecture slot remains reusable.
func ReclaimExitedTask(allocator *pmm.Allocator, kernelRoot uint64, task uint64) bool {
	if allocator == nil || kernelRoot == 0 {
		return false
	}
	if slot := uint32(task); slot != 0 && int(slot) <= len(taskFPU) {
		InitialFPUArea(&taskFPU[slot-1])
	}
	allocation := reclaimExitedTask(task)
	if allocation == 0 {
		return false
	}
	if !vm.MapKernelPage(kernelRoot, allocation) {
		return false
	}
	invalidatePage(allocation)
	ok := true
	for page := uint64(0); page < kernelStackPages+stackGuardPages; page++ {
		ok = allocator.Free(allocation+page*pmm.PageSize) && ok
	}
	return ok
}

//export goos_invalidate_page
func invalidatePage(address uint64)
