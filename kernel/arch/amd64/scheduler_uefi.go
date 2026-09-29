//go:build uefi && amd64

package amd64

import (
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
	"unsafe"
)

const (
	stackGuardPages = uint64(KernelStackGuardSize / pmm.PageSize)
	contextWords    = 20
	initialRFlags   = uint64(0x202)
)

//go:extern goos_task_entry_table
var taskEntryTable [3]uintptr

// taskFPU holds the FXSAVE image of every scheduler slot. Every switch saves
// the outgoing task's SSE and x87 state and restores the incoming one's.
//
//go:extern goos_task_fpu
var taskFPU [64][FPUAreaSize]byte

type taskKernelStack struct {
	physical uint64
	virtual  uint64
	top      uint64
}

var taskKernelStacks [64]taskKernelStack

//go:extern goos_entry_mapping_table
var entryMappingTable [3]uintptr

var activeKernelRoot uint64

// SetupBootstrapScheduler creates contexts for two demonstration tasks and one
// idle task. The currently executing boot task becomes slot zero on the first
// timer interrupt.
func SetupBootstrapScheduler(allocator *pmm.Allocator, pageTable uint64) bool {
	activeKernelRoot = pageTable
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
		if !unmapGuardPages(pageTable, allocation) {
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
	if !ok || activeKernelRoot == 0 || !unmapGuardPages(activeKernelRoot, allocation) {
		return 0, false
	}
	stackStart := allocation + stackGuardPages*pmm.PageSize
	virtualStart, aliasOK := vm.KernelAlias(stackStart)
	if !aliasOK {
		return 0, false
	}
	if !mapProcessEntryState(allocator, pageTable) {
		return 0, false
	}
	for page := uint64(0); page < kernelStackPages; page++ {
		physical := stackStart + page*pmm.PageSize
		virtual := virtualStart + page*pmm.PageSize
		if !vm.MapProcessKernelPage(allocator, pageTable, virtual, physical, true, false) {
			return 0, false
		}
	}
	guard, _ := vm.KernelAlias(allocation)
	if vm.IsMapped(activeKernelRoot, guard) || vm.IsMapped(pageTable, guard) {
		return 0, false
	}
	kernelStackTop := virtualStart + kernelStackPages*pmm.PageSize - 64
	context := initialUserContext(stackStart, entry, userStack)
	taskID := installUserTask(context, pageTable, kernelStackTop)
	if taskID == 0 {
		return 0, false
	}
	taskKernelStacks[uint32(taskID)-1] = taskKernelStack{physical: allocation, virtual: virtualStart, top: kernelStackTop}
	return taskID, true
}

func mapProcessEntryState(allocator *pmm.Allocator, pageTable uint64) bool {
	for _, handler := range interruptTable {
		if handler == 0 || !mapProcessAliasRange(allocator, pageTable, uint64(handler), 64, false, true) {
			return false
		}
	}
	if !mapProcessAliasRange(allocator, pageTable, uint64(entryMappingTable[0]), 32, false, true) ||
		!mapProcessAliasRange(allocator, pageTable, uint64(entryMappingTable[1]), 8, true, false) ||
		!mapProcessAliasRange(allocator, pageTable, uint64(entryMappingTable[2]), 128, false, true) ||
		!mapProcessAliasRange(allocator, pageTable, uint64(uintptr(unsafe.Pointer(&idt[0]))), uint64(len(idt))*16, true, false) ||
		!mapProcessAliasRange(allocator, pageTable, uint64(uintptr(unsafe.Pointer(&gdt[0]))), uint64(len(gdt))*8, true, false) ||
		!mapProcessAliasRange(allocator, pageTable, uint64(uintptr(unsafe.Pointer(&tss[0]))), uint64(len(tss)), true, false) {
		return false
	}
	if !mapProcessAliasRange(allocator, pageTable, uint64(linuxEntryMappingTable[0]), vm.PageSize, false, true) ||
		!mapProcessAliasRange(allocator, pageTable, uint64(linuxEntryMappingTable[1]), vm.PageSize, false, true) ||
		!mapProcessAliasRange(allocator, pageTable, uint64(linuxEntryMappingTable[2]), 32, true, false) {
		return false
	}
	return true
}

// PrepareUserAddressSpace installs the supervisor-only pages needed while a
// process root is active before the task itself exists.
func PrepareUserAddressSpace(allocator *pmm.Allocator, pageTable uint64) bool {
	return mapProcessEntryState(allocator, pageTable)
}

func mapProcessAliasRange(allocator *pmm.Allocator, pageTable, address, size uint64, writable, executable bool) bool {
	start := address &^ (vm.PageSize - 1)
	end := (address + size + vm.PageSize - 1) &^ (vm.PageSize - 1)
	if size == 0 || end < address {
		return false
	}
	for physical := start; physical < end; physical += vm.PageSize {
		virtual, ok := vm.KernelAlias(physical)
		if !ok || !vm.MapProcessKernelPage(allocator, pageTable, virtual, physical, writable, executable) {
			return false
		}
	}
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

func initialUserContext(stackStart uint64, entry uint64, userStack uint64) uint64 {
	stackEnd := stackStart + kernelStackPages*pmm.PageSize
	contextAddress := stackEnd - 64 - contextWords*8
	// SAFETY: the guarded allocation owns the mapped kernel-stack pages. The
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
	contextAlias, _ := vm.KernelAlias(contextAddress)
	return contextAlias
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

// TaskTicks returns the timer ticks charged to a task slot. The timer charges
// the task it interrupted, so this samples who was running rather than timing
// it exactly.
//
//export goos_task_ticks_read
func TaskTicks(slot uint64) uint64

// ClearTaskTicks resets a slot's counter so a reused slot starts from zero.
//
//export goos_task_ticks_clear
func ClearTaskTicks(slot uint64)

// TakeTaskFaultAddress returns and clears the address of the last user page
// fault a task slot took, or zero when its last fault was of another kind.
//
//export goos_task_fault_address_take
func TakeTaskFaultAddress(slot uint64) uint64

//export goos_set_current_task_exit_code
func SetCurrentTaskExitCode(code uint64)

// TerminateTask retires another task with an exit code, as a fault does. The
// caller cancels the task's wait registrations first.
//
//export goos_terminate_task
func TerminateTask(task uint64, code uint64) bool

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
	slot := uint32(task)
	if slot == 0 || int(slot) > len(taskFPU) {
		return false
	}
	InitialFPUArea(&taskFPU[slot-1])
	stack := taskKernelStacks[slot-1]
	if stack.physical == 0 || reclaimExitedTask(task) == 0 {
		return false
	}
	taskKernelStacks[slot-1] = taskKernelStack{}
	allocation := stack.physical
	for page := uint64(0); page < stackGuardPages; page++ {
		guard := allocation + page*pmm.PageSize
		if !vm.MapKernelPage(kernelRoot, guard) {
			return false
		}
		invalidatePage(guard)
		if alias, mapped := vm.KernelAlias(guard); mapped {
			invalidatePage(alias)
		}
	}
	ok := true
	for page := uint64(0); page < kernelStackPages+stackGuardPages; page++ {
		ok = allocator.Free(allocation+page*pmm.PageSize) && ok
	}
	return ok
}

//export goos_invalidate_page
func invalidatePage(address uint64)

// KernelStackGuard returns the unmapped guard below the kernel-entry stack
// that holds address, found among the user task slots, as [low, high).
func KernelStackGuard(address uint64) (uint64, uint64, bool) {
	state := ReadSchedulerState()
	for slot := uint64(0); slot < state.Count; slot++ {
		top := TaskStackTop(slot)
		if top == 0 {
			continue
		}
		// The top sits 64 bytes below the end of the usable pages.
		start := top + 64 - kernelStackPages*pmm.PageSize
		if address >= start && address < top+64 {
			return start - KernelStackGuardSize, start, true
		}
	}
	return 0, 0, false
}
