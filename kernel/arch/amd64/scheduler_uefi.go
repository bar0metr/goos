//go:build uefi && amd64

package amd64

import (
	"goos/internal/validation"
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
	"goos/kernel/sched"
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

// linuxUserFPU is the FXSAVE image of a Linux task's own x87 and SSE state
// during its current system call. The entry path saves it and the return
// path restores it, so kernel code may use SSE registers in between.
//
//go:extern goos_linux_user_fpu
var linuxUserFPU [64][FPUAreaSize]byte

// LinuxUserFPU returns the saved x87 and SSE state of the current task's
// Linux system call, which the return path restores.
func LinuxUserFPU(task sched.TaskID) (*[FPUAreaSize]byte, bool) {
	slot, _, valid := task.Parts()
	if !valid || int(slot) >= len(linuxUserFPU) || TaskIDForSlot(uint64(slot)) != uint64(task) {
		return nil, false
	}
	return &linuxUserFPU[slot], true
}

// SetTaskFPU replaces the x87 and SSE state a task that is not running
// resumes with, as a forked child resumes with its parent's.
func SetTaskFPU(task sched.TaskID, area *[FPUAreaSize]byte) bool {
	slot, _, valid := task.Parts()
	if !valid || int(slot) >= len(taskFPU) || TaskIDForSlot(uint64(slot)) != uint64(task) || area == nil {
		return false
	}
	taskFPU[slot] = *area
	return true
}

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

// UserRegisters is the ring 3 state a new user task starts with.
type UserRegisters struct {
	RAX, RBX, RCX, RDX, RSI, RDI, RBP uint64
	R8, R9, R10, R11, R12, R13, R14   uint64
	R15                               uint64
	RIP, RFlags, RSP                  uint64
}

// LinuxCallRegisters returns the general-purpose registers the current task
// pushed on entry to the Linux system call it is making. The entry path pushes
// them at the top of the task's kernel stack, RAX lowest and R15 highest.
func LinuxCallRegisters(task sched.TaskID) (UserRegisters, bool) {
	slot, _, valid := task.Parts()
	if !valid || int(slot) >= len(taskKernelStacks) || TaskIDForSlot(uint64(slot)) != uint64(task) || taskKernelStacks[slot].top == 0 {
		return UserRegisters{}, false
	}
	// SAFETY: the task's kernel stack is mapped for its whole life, and the
	// fifteen pushed words lie directly below its entry top.
	saved := (*[15]uint64)(unsafe.Pointer(uintptr(taskKernelStacks[slot].top - 15*8)))
	return UserRegisters{
		RAX: saved[0], RCX: saved[1], RDX: saved[2], RBX: saved[3], RBP: saved[4], RSI: saved[5], RDI: saved[6],
		R8: saved[7], R9: saved[8], R10: saved[9], R11: saved[10], R12: saved[11], R13: saved[12], R14: saved[13], R15: saved[14],
	}, true
}

// SetLinuxCallRegisters replaces the general-purpose registers the current
// task's Linux system call returns with. The return path pops them from the
// top of the task's kernel stack; R11 also supplies the returned RFLAGS.
func SetLinuxCallRegisters(task sched.TaskID, registers UserRegisters) bool {
	slot, _, valid := task.Parts()
	if !valid || int(slot) >= len(taskKernelStacks) || TaskIDForSlot(uint64(slot)) != uint64(task) || taskKernelStacks[slot].top == 0 {
		return false
	}
	// SAFETY: see LinuxCallRegisters.
	saved := (*[15]uint64)(unsafe.Pointer(uintptr(taskKernelStacks[slot].top - 15*8)))
	saved[0], saved[1], saved[2], saved[3], saved[4] = registers.RAX, registers.RCX, registers.RDX, registers.RBX, registers.RBP
	saved[5], saved[6], saved[7], saved[8], saved[9] = registers.RSI, registers.RDI, registers.R8, registers.R9, registers.R10
	saved[10], saved[11], saved[12], saved[13], saved[14] = registers.R11, registers.R12, registers.R13, registers.R14, registers.R15
	return true
}

// userFlagsCleared are the RFLAGS bits a user task never resumes with: TF,
// DF, IOPL, NT, RF, VM, and AC. IF is always set.
const userFlagsCleared = uint64(0x77700)

// AddUserTask installs a runnable ring 3 context with its own address-space
// root and guarded kernel-entry stack. The scheduler updates TSS.RSP0 before
// selecting this task, so userspace interrupt frames remain task-local.
func AddUserTask(allocator *pmm.Allocator, pageTable uint64, entry uint64, userStack uint64) (uint64, bool) {
	return AddUserTaskWithRegisters(allocator, pageTable, UserRegisters{RIP: entry, RSP: userStack, RFlags: initialRFlags})
}

// AddUserTaskWithRegisters is AddUserTask for a task that resumes with every
// general-purpose register given, as a forked child resumes where its parent
// made the call.
func AddUserTaskWithRegisters(allocator *pmm.Allocator, pageTable uint64, registers UserRegisters) (uint64, bool) {
	if allocator == nil || pageTable == 0 || registers.RIP == 0 || registers.RSP == 0 || activeKernelRoot == 0 {
		return 0, false
	}
	allocation, ok := allocator.AllocateContiguous(kernelStackPages + stackGuardPages)
	if !ok {
		return 0, false
	}
	if !unmapGuardPages(activeKernelRoot, allocation) {
		abandonPreparedUserTask(allocator, pageTable, allocation, 0, 0)
		return 0, false
	}
	stackStart := allocation + stackGuardPages*pmm.PageSize
	virtualStart, aliasOK := vm.KernelAlias(stackStart)
	if !aliasOK {
		abandonPreparedUserTask(allocator, pageTable, allocation, 0, 0)
		return 0, false
	}
	if !mapProcessEntryState(allocator, pageTable) {
		abandonPreparedUserTask(allocator, pageTable, allocation, virtualStart, 0)
		return 0, false
	}
	mappedPages := uint64(0)
	for page := uint64(0); page < kernelStackPages; page++ {
		physical := stackStart + page*pmm.PageSize
		virtual := virtualStart + page*pmm.PageSize
		if injectedTaskStackFault(mappedPages, virtualStart) ||
			!vm.MapProcessKernelPage(allocator, pageTable, virtual, physical, true, false) {
			abandonPreparedUserTask(allocator, pageTable, allocation, virtualStart, mappedPages)
			return 0, false
		}
		mappedPages++
	}
	guard, _ := vm.KernelAlias(allocation)
	if injectedTaskStackFault(mappedPages, virtualStart) || vm.IsMapped(activeKernelRoot, guard) || vm.IsMapped(pageTable, guard) {
		abandonPreparedUserTask(allocator, pageTable, allocation, virtualStart, mappedPages)
		return 0, false
	}
	kernelStackTop := virtualStart + kernelStackPages*pmm.PageSize - 64
	context := initialUserContext(stackStart, &registers)
	taskID := installUserTask(context, pageTable, kernelStackTop)
	if taskID == 0 {
		abandonPreparedUserTask(allocator, pageTable, allocation, virtualStart, mappedPages)
		return 0, false
	}
	taskKernelStacks[uint32(taskID)-1] = taskKernelStack{physical: allocation, virtual: virtualStart, top: kernelStackTop}
	return taskID, true
}

// The validation build can fail one task creation at a chosen stage, so a
// self-test can check that the rollback returns every page and slot.
var (
	taskStackFaultArmed   bool
	taskStackFaultAfter   uint64
	taskStackFaultVirtual uint64
)

// InjectTaskStackFault makes the next AddUserTaskWithRegisters fail once
// mappedPages of its kernel-entry stack are mapped. KernelStackPages fails it
// after the last mapping, just before the task is installed. Normal builds
// refuse.
func InjectTaskStackFault(mappedPages uint64) bool {
	if !validation.Enabled || mappedPages > kernelStackPages {
		return false
	}
	taskStackFaultArmed, taskStackFaultAfter, taskStackFaultVirtual = true, mappedPages, 0
	return true
}

// InjectedTaskStack returns the kernel-entry stack address and size the last
// injected failure abandoned.
func InjectedTaskStack() (uint64, uint64) {
	return taskStackFaultVirtual, kernelStackPages * pmm.PageSize
}

// KernelStackPages is the number of pages a task's kernel-entry stack maps.
func KernelStackPages() uint64 { return kernelStackPages }

func injectedTaskStackFault(mappedPages, virtualStart uint64) bool {
	if !validation.Enabled || !taskStackFaultArmed || mappedPages != taskStackFaultAfter {
		return false
	}
	taskStackFaultArmed, taskStackFaultVirtual = false, virtualStart
	return true
}

func abandonPreparedUserTask(allocator *pmm.Allocator, pageTable, allocation, virtualStart, mappedPages uint64) {
	for page := uint64(0); page < mappedPages; page++ {
		virtual := virtualStart + page*pmm.PageSize
		vm.UnmapProcessKernelPage(pageTable, virtual)
		invalidatePage(virtual)
	}
	for page := uint64(0); page < stackGuardPages; page++ {
		guard := allocation + page*pmm.PageSize
		if !vm.IsMapped(activeKernelRoot, guard) {
			vm.MapKernelPage(activeKernelRoot, guard)
		}
		invalidatePage(guard)
		if alias, mapped := vm.KernelAlias(guard); mapped {
			invalidatePage(alias)
		}
	}
	freeTaskStackAllocation(allocator, allocation)
}

func freeTaskStackAllocation(allocator *pmm.Allocator, allocation uint64) {
	for page := uint64(0); page < kernelStackPages+stackGuardPages; page++ {
		allocator.Free(allocation + page*pmm.PageSize)
	}
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

// PrepareTaskAddressSpace maps the entry state and existing guarded kernel
// stack into a replacement user address space before it becomes visible.
func PrepareTaskAddressSpace(allocator *pmm.Allocator, task sched.TaskID, pageTable uint64) bool {
	slot, _, valid := task.Parts()
	if !valid || pageTable == 0 || TaskIDForSlot(uint64(slot)) != uint64(task) || !mapProcessEntryState(allocator, pageTable) {
		return false
	}
	stack := taskKernelStacks[slot]
	if stack.physical == 0 || stack.virtual == 0 || stack.top == 0 {
		return false
	}
	stackStart := stack.physical + stackGuardPages*pmm.PageSize
	for page := uint64(0); page < kernelStackPages; page++ {
		physical := stackStart + page*pmm.PageSize
		virtual := stack.virtual + page*pmm.PageSize
		if !vm.MapProcessKernelPage(allocator, pageTable, virtual, physical, true, false) {
			return false
		}
	}
	guard, _ := vm.KernelAlias(stack.physical)
	return !vm.IsMapped(pageTable, guard)
}

// ReplaceTaskAddressSpace changes the saved root of the current live task.
// The prepared root supplies the next return to ring 3 and later entries.
//
//export goos_task_root_replace
func ReplaceTaskAddressSpace(task sched.TaskID, pageTable uint64) bool

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

// initialUserContext writes the frame the scheduler restores: fifteen
// general-purpose registers in its pop order, then the IRETQ frame.
func initialUserContext(stackStart uint64, registers *UserRegisters) uint64 {
	stackEnd := stackStart + kernelStackPages*pmm.PageSize
	contextAddress := stackEnd - 64 - contextWords*8
	// SAFETY: the guarded allocation owns the mapped kernel-stack pages. The
	// synthetic context lies wholly below the reserved interrupt-entry top.
	context := (*[contextWords]uint64)(unsafe.Pointer(uintptr(contextAddress)))
	context[0], context[1], context[2], context[3] = registers.R15, registers.R14, registers.R13, registers.R12
	context[4], context[5], context[6], context[7] = registers.R11, registers.R10, registers.R9, registers.R8
	context[8], context[9], context[10] = registers.RDI, registers.RSI, registers.RBP
	context[11], context[12], context[13], context[14] = registers.RDX, registers.RCX, registers.RBX, registers.RAX
	context[15] = registers.RIP
	context[16] = uint64(userCodeSelector)
	context[17] = registers.RFlags&^userFlagsCleared | initialRFlags
	context[18] = registers.RSP
	context[19] = uint64(userDataSelector)
	contextAlias, _ := vm.KernelAlias(contextAddress)
	return contextAlias
}

//export goos_scheduler_install
func installScheduler(taskA uint64, taskB uint64, idle uint64, kernelRoot uint64)

//export goos_scheduler_install_user
func installUserTask(context uint64, pageTable uint64, kernelStackTop uint64) uint64

// SetTaskFSBase changes the saved FS base of one live task and updates the CPU
// immediately when it is the current task.
//
//export goos_task_fs_base_set
func SetTaskFSBase(task sched.TaskID, base uint64) bool

//export goos_task_fs_base_read
func readTaskFSBase(task sched.TaskID, base *uint64) bool

// TaskFSBase reads the saved FS base of one live task.
func TaskFSBase(task sched.TaskID) (uint64, bool) {
	var base uint64
	ok := readTaskFSBase(task, &base)
	return base, ok
}

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

// FreeTaskSlots counts unused slots and dead slots whose exit state and
// kernel stack have both been reclaimed.
func FreeTaskSlots() int {
	state := ReadSchedulerState()
	free := sched.MaxTasks - int(state.Count)
	for slot := uint64(0); slot < state.Count && slot < sched.MaxTasks; slot++ {
		if state.Dead&(uint64(1)<<slot) != 0 && TaskStackTop(slot) == 0 {
			free++
		}
	}
	return free
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
//
// liveRoot is the address space the task ran in when that address space
// outlives it, as a thread's process does; its mapping of the task's
// kernel-entry stack is removed there. It is zero when the address space is
// gone or about to be destroyed.
func ReclaimExitedTask(allocator *pmm.Allocator, kernelRoot uint64, task uint64, liveRoot uint64) bool {
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
	if liveRoot != 0 {
		for page := uint64(0); page < kernelStackPages; page++ {
			virtual := stack.virtual + page*pmm.PageSize
			vm.UnmapProcessKernelPage(liveRoot, virtual)
			invalidatePage(virtual)
		}
	}
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
