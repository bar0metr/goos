//go:build uefi && amd64

package amd64

import (
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
	"sync/atomic"
	"unsafe"
)

// Application processors are started one at a time. The bootstrap processor
// fills the trampoline's parameter block for one processor, sends it INIT and
// start-up interrupts, and waits for it to report from Go on its own stack.
// A started processor parks with interrupts masked except for the wake-up
// interrupt, and joins the scheduler only when the bootstrap processor brings
// it online. A processor that misses a deadline stays offline: the bootstrap
// processor cancels it with a compare-and-swap, so a late processor finds the
// cancellation and parks for good instead of joining.

// apState is one word per processor that the two sides move forward with
// compare-and-swap.
const (
	apIdle      = 0
	apStarted   = 1
	apFailed    = 2
	apGo        = 3
	apOnline    = 4
	apCancelled = 5
	apJoining   = 6
)

var apStates [MaxCPUs]atomic.Uint32

// apStackTops are the processors' start-up stacks, which become the stacks
// their idle loops run on.
var apStackTops [MaxCPUs]uint64

//go:extern goos_ap_trampoline
var apTrampoline [4096]byte

//go:extern goos_ap_trampoline_layout
var apLayout [12]uint64

// Offsets in apLayout.
const (
	layoutFar32 = iota
	layoutProtected
	layoutFar64
	layoutLong
	layoutGDT
	layoutGDTR
	layoutCR3
	layoutStack
	layoutCPU
	layoutEntry
	layoutSize
	layoutEntryAddress
)

// patConfigured records that the bootstrap processor installed the GoOS PAT
// layout, which every processor must share.
var patConfigured bool

// NotePATConfigured records that ConfigurePAT succeeded on the bootstrap
// processor.
func NotePATConfigured() { patConfigured = true }

// Startup is the trampoline and page tables processors start through.
type Startup struct {
	page  uint64
	root  uint64
	pages [4]uint64
}

// PrepareStartup copies the trampoline to page, below 1 MiB, and builds the
// page tables that map it where a starting processor enables paging.
func PrepareStartup(allocator *pmm.Allocator, kernelRoot uint64, page uint64) (Startup, bool) {
	size := apLayout[layoutSize]
	if allocator == nil || page%vm.PageSize != 0 || page >= 1<<20 || size == 0 || size > vm.PageSize {
		return Startup{}, false
	}
	if !vm.MapKernelPage(kernelRoot, page) {
		return Startup{}, false
	}
	invalidatePage(page)
	// SAFETY: page is a usable low page the memory map gave no owner, now
	// mapped writable at its physical address in the kernel root.
	target := (*[vm.PageSize]byte)(unsafe.Pointer(uintptr(page)))
	for index := uint64(0); index < size; index++ {
		target[index] = apTrampoline[index]
	}
	putLittle(target[apLayout[layoutFar32]:], page+apLayout[layoutProtected], 4)
	putLittle(target[apLayout[layoutFar64]:], page+apLayout[layoutLong], 4)
	putLittle(target[apLayout[layoutGDTR]+2:], page+apLayout[layoutGDT], 4)
	root, tables, ok := vm.BuildTrampolineRoot(allocator, kernelRoot, page)
	if !ok {
		vm.UnmapPage(kernelRoot, page)
		invalidatePage(page)
		return Startup{}, false
	}
	startup := Startup{page: page, root: root, pages: tables}
	startup.put64(layoutCR3, root)
	startup.put64(layoutEntry, apLayout[layoutEntryAddress])
	return startup, true
}

func (startup *Startup) put64(field int, value uint64) {
	// SAFETY: the trampoline page stays mapped writable in the kernel root
	// until Release.
	target := (*[vm.PageSize]byte)(unsafe.Pointer(uintptr(startup.page)))
	putLittle(target[apLayout[field]:], value, 8)
}

func putLittle(target []byte, value uint64, size int) {
	for index := 0; index < size; index++ {
		target[index] = byte(value >> (8 * index))
	}
}

// Release unmaps the trampoline from the kernel root and frees the page
// tables processors started through. The low page itself stays reserved.
func (startup *Startup) Release(allocator *pmm.Allocator, kernelRoot uint64) {
	if startup.page == 0 {
		return
	}
	vm.UnmapPage(kernelRoot, startup.page)
	invalidatePage(startup.page)
	for index := range startup.pages {
		if startup.pages[index] != 0 {
			allocator.Free(startup.pages[index])
		}
	}
	*startup = Startup{}
}

// StartProcessor starts processor cpu, whose APIC ID is apicID, and reports
// whether it reached Go on its own stack. wait sleeps for a number of
// microseconds. The INIT, start-up, start-up sequence and its delays are the
// architectural ones.
func StartProcessor(startup *Startup, allocator *pmm.Allocator, kernelRoot uint64, cpu int, apicID uint32, wait func(micros uint64)) bool {
	if startup.page == 0 || cpu <= 0 || cpu >= MaxCPUs || apStates[cpu].Load() != apIdle {
		return false
	}
	allocation, ok := allocator.AllocateContiguous(kernelStackPages + stackGuardPages)
	if !ok || !unmapGuardPages(kernelRoot, allocation) {
		return false
	}
	top := allocation + (kernelStackPages+stackGuardPages)*pmm.PageSize - 64
	rsp0, ok := vm.KernelAlias(top)
	if !ok || !prepareCPUTables(allocator, kernelRoot, cpu, rsp0) {
		return false
	}
	apStackTops[cpu] = top
	cpuAPIC[cpu] = apicID
	cpuData[cpu][cpuCurrent] = uint64(idleBase + cpu)
	taskRoots[idleBase+cpu] = kernelRoot
	startup.put64(layoutStack, top)
	startup.put64(layoutCPU, uint64(cpu))
	command := uint32(startup.page >> 12)
	flags := DisableInterrupts()
	sendInterrupt(apicID, icrAssert|icrInit)
	RestoreInterrupts(flags)
	wait(10000)
	for attempt := 0; attempt < 2; attempt++ {
		flags = DisableInterrupts()
		sendInterrupt(apicID, icrAssert|icrStartup|command)
		RestoreInterrupts(flags)
		for elapsed := uint64(0); elapsed < 100000; elapsed += 100 {
			if apStates[cpu].Load() == apStarted {
				return true
			}
			wait(100)
		}
	}
	return !apStates[cpu].CompareAndSwap(apIdle, apFailed)
}

// BringOnline lets a started processor join the scheduler and waits for it.
// It reports false, leaving the processor parked for good, when it does not
// join in time.
func BringOnline(cpu int, wait func(micros uint64)) bool {
	if cpu <= 0 || cpu >= MaxCPUs || !apStates[cpu].CompareAndSwap(apStarted, apGo) {
		return false
	}
	sendIPI(cpu, rescheduleVector)
	for elapsed := uint64(0); elapsed < 100000; elapsed += 100 {
		if apStates[cpu].Load() == apOnline {
			return true
		}
		wait(100)
	}
	if apStates[cpu].CompareAndSwap(apGo, apCancelled) {
		return false
	}
	// The processor is joining; that takes no longer than one scheduler
	// lock acquisition.
	for apStates[cpu].Load() != apOnline {
		cpuPause()
	}
	return true
}

// apMain is where a started processor enters Go, on its own stack, with
// interrupts disabled and the kernel root loaded. It never returns.
//
//export goos_ap_main
func apMain(index uint64) {
	cpu := int(index)
	EnableNXAndWriteProtect()
	if patConfigured {
		ConfigurePAT()
	}
	if !loadCPUTables(cpu) || !installLinuxSyscallFor(cpu) {
		park()
	}
	enableLocalAPIC()
	if !apStates[cpu].CompareAndSwap(apIdle, apStarted) {
		park()
	}
	for apStates[cpu].Load() == apStarted {
		waitForInterrupt()
	}
	if !apStates[cpu].CompareAndSwap(apGo, apJoining) {
		park()
	}
	startLocalTimer()
	flags := lockScheduler()
	runState.Online(cpu)
	online.Store(runState.OnlineMask())
	cpuData[cpu][cpuEnabled] = 1
	unlockScheduler(flags)
	apStates[cpu].Store(apOnline)
	// The start-up stack becomes this processor's idle loop. Its context is
	// saved in the idle slot at the first interrupt.
	enableInterrupts()
	for {
		Halt()
	}
}

// park stops a processor that cannot or may not run tasks.
func park() {
	DisableInterrupts()
	for {
		Halt()
	}
}

// ProcessorState reports where a processor stopped in its start-up.
func ProcessorState(cpu int) uint32 {
	if cpu < 0 || cpu >= MaxCPUs {
		return apFailed
	}
	return apStates[cpu].Load()
}
