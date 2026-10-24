//go:build goos_userspace

package sys

import "unsafe"

// The native-call entry points in userspace/runtime/amd64.S. Each loads its
// operation number into eax and enters the kernel with the arguments in rdi,
// rsi and rdx; the status comes back in rax. They are unexported: the typed
// wrappers are the only way in.

//go:linkname processExit goos_exit
func processExit(code uint64)

//go:linkname threadCreate goos_thread_create
func threadCreate(entry uintptr, argument uintptr, info unsafe.Pointer) uint64

//go:linkname threadExit goos_thread_exit
func threadExit(code uint64)

//go:linkname threadJoin goos_thread_join
func threadJoin(thread uint64, flags uint64, code unsafe.Pointer) uint64

//go:linkname threadSelf goos_thread_self
func threadSelf(info unsafe.Pointer) uint64

//go:linkname threadYield goos_thread_yield
func threadYield() uint64

//go:linkname threadSuspend goos_thread_suspend
func threadSuspend(snapshots unsafe.Pointer, capacity uint64, count unsafe.Pointer) uint64

//go:linkname threadResume goos_thread_resume
func threadResume() uint64

//go:linkname threadEntryAddress goos_thread_entry_address
func threadEntryAddress() uintptr

//go:linkname workerEntryAddress goos_worker_entry_address
func workerEntryAddress() uintptr

//go:linkname threadSetBase goos_thread_set_base
func threadSetBase(base uintptr) uint64

//go:linkname threadPreempt goos_thread_preempt
func threadPreempt(entry uintptr, ticks uint64) uint64

//go:linkname probeVectorSpin goos_probe_vector_spin
func probeVectorSpin(stop *uint32) uint64

//go:linkname probeXMMRoot goos_probe_xmm_root
func probeXMMRoot(index, token uint64, state *[2]uint32) uint64

//go:linkname probeGPRRoot goos_probe_gpr_root
func probeGPRRoot(index, token uint64, state *[2]uint32) uint64

//go:linkname threadWake goos_thread_wake
func threadWake(thread uint64) uint64

//go:linkname threadWakeTake goos_thread_wake_take
func threadWakeTake() uint64

//go:linkname threadHoldRange goos_thread_hold_range
func threadHoldRange(thread uint64, lo uintptr, hi uintptr) uint64

//go:linkname threadHoldWait goos_thread_hold_wait
func threadHoldWait(thread uint64, record unsafe.Pointer) uint64

//go:linkname threadHoldStep goos_thread_hold_step
func threadHoldStep(thread uint64) uint64

//go:linkname threadHoldCancel goos_thread_hold_cancel
func threadHoldCancel(thread uint64) uint64

//go:linkname ipcSend goos_ipc_send
func ipcSend(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcReceive goos_ipc_receive
func ipcReceive(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcWaitReceive goos_ipc_wait_receive
func ipcWaitReceive(channel uint64, descriptor unsafe.Pointer, size uintptr) uint64

//go:linkname consoleWrite goos_console_write
func consoleWrite(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname consoleRead goos_console_read
func consoleRead(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname consoleInterrupt goos_console_interrupt
func consoleInterrupt(device uint64) uint64

//go:linkname consoleInfo goos_console_info
func consoleInfo(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname clockRead goos_clock_read
func clockRead(address unsafe.Pointer, size uintptr) uint64

//go:linkname clockReadNanos goos_clock_read_nanos
func clockReadNanos(address unsafe.Pointer, size uintptr) uint64

//go:linkname clockNow goos_clock_now
func clockNow(clock uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname clockSet goos_clock_set
func clockSet(authority uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname startupRead goos_startup_read
func startupRead(address unsafe.Pointer, size uintptr) uint64

//go:linkname channelCreate goos_channel_create
func channelCreate(address unsafe.Pointer, size uintptr) uint64

//go:linkname handleClose goos_handle_close
func handleClose(id uint64) uint64

//go:linkname processWait goos_process_wait
func processWait(process uint64, result unsafe.Pointer, timeout uint64) uint64

//go:linkname systemInfoRead goos_system_info
func systemInfoRead(address unsafe.Pointer, size uintptr) uint64

//go:linkname cpuInfoRead goos_cpu_info
func cpuInfoRead(address unsafe.Pointer, size uintptr) uint64

//go:linkname cpuEntryRead goos_cpu_entry
func cpuEntryRead(index uint64, address unsafe.Pointer) uint64

//go:linkname cpuCacheInfoRead goos_cpu_cache_info
func cpuCacheInfoRead(index uint64, address unsafe.Pointer) uint64

//go:linkname cpuRuntimeRead goos_cpu_runtime
func cpuRuntimeRead(index uint64, address unsafe.Pointer) uint64

//go:linkname taskAffinity goos_task_affinity
func taskAffinity(process uint64, mask uint64, address unsafe.Pointer) uint64

//go:linkname portRead goos_port_read
func portRead(device uint64, specification uint64, address unsafe.Pointer) uint64

//go:linkname portWrite goos_port_write
func portWrite(device uint64, specification uint64, value uint64) uint64

//go:linkname dmaAllocate goos_dma_allocate
func dmaAllocate(device uint64, virtual uint64, address unsafe.Pointer) uint64

//go:linkname irqWait goos_irq_wait
func irqWait(irq uint64) uint64

//go:linkname irqTake goos_irq_take
func irqTake(irq uint64) uint64

//go:linkname irqPoll goos_irq_poll
func irqPoll(irq uint64) uint64

//go:linkname randomRead goos_random_read
func randomRead(address unsafe.Pointer, size uintptr) uint64

//go:linkname streamCreate goos_stream_create
func streamCreate(address unsafe.Pointer, size uintptr) uint64

//go:linkname streamRead goos_stream_read
func streamRead(stream uint64, descriptor unsafe.Pointer, size uintptr) uint64

//go:linkname streamWrite goos_stream_write
func streamWrite(stream uint64, descriptor unsafe.Pointer, size uintptr) uint64

//go:linkname streamWait goos_stream_wait
func streamWait(stream uint64, timeout uint64) uint64

//go:linkname waitAny goos_wait_any
func waitAny(interests unsafe.Pointer, count uintptr, result unsafe.Pointer) uint64

//go:linkname diagnostics goos_diagnostics
func diagnostics(device uint64, address uintptr) uint64

//go:linkname logWrite goos_log_write
func logWrite(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname logRead goos_log_read
func logRead(device uint64, index uint64, address unsafe.Pointer) uint64

//go:linkname logBounds goos_log_bounds
func logBounds(device uint64, address unsafe.Pointer) uint64

//go:linkname handleInfo goos_handle_info
func handleInfo(slot uint64, address unsafe.Pointer) uint64

//go:linkname processInfo goos_process_info
func processInfo(index uint64, address unsafe.Pointer) uint64

//go:linkname systemReset goos_system_reset
func systemReset(device uint64) uint64

//go:linkname memoryMap goos_memory_map
func memoryMap(memory uint64, address uint64, flags uint64) uint64

//go:linkname processKill goos_process_kill
func processKill(process uint64) uint64

//go:linkname imageMap goos_image_map
func imageMap(device uint64, virtual uint64, address unsafe.Pointer) uint64

//go:linkname validationFault goos_validation_fault
func validationFault(device uint64) uint64

//go:linkname systemPowerOff goos_system_power_off
func systemPowerOff(device uint64) uint64

//go:linkname powerOffReady goos_power_off_ready
func powerOffReady(device uint64) uint64

//go:linkname executableCreate goos_executable_create
func executableCreate(loader uint64, size uint64, address unsafe.Pointer) uint64

//go:linkname executableWrite goos_executable_write
func executableWrite(executable uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname executableSource goos_executable_source
func executableSource(loader uint64, session uint64, descriptor unsafe.Pointer) uint64

//go:linkname vmMap goos_vm_map
func vmMap(descriptor unsafe.Pointer, protection uint64, flags uint64) uint64

//go:linkname vmUnmap goos_vm_unmap
func vmUnmap(address uint64, length uint64) uint64

//go:linkname vmProtect goos_vm_protect
func vmProtect(address uint64, length uint64, protection uint64) uint64

//go:linkname vmDiscard goos_vm_discard
func vmDiscard(address uint64, length uint64) uint64

//go:linkname vmQuery goos_vm_query
func vmQuery(address uint64, record unsafe.Pointer) uint64

//go:linkname memoryCreate goos_memory_create
func memoryCreate(pages uint64, address unsafe.Pointer) uint64

//go:linkname handleDuplicate goos_handle_duplicate
func handleDuplicate(handle uint64, rights uint64, address unsafe.Pointer) uint64

//go:linkname logReadSequence goos_log_read_sequence
func logReadSequence(device uint64, cursor uint64, address unsafe.Pointer) uint64
