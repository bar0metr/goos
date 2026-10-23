//go:build !goos_userspace

package sys

import (
	"goos/kernel/kcall"
	"unsafe"
)

// On the host there is no kernel. Every native call answers Unsupported, so
// the wrappers type-check, link, and can be tested for what they do before
// and after the call.

const unsupported = uint64(kcall.StatusUnsupported)

func processExit(code uint64) {}

func threadCreate(entry uintptr, argument uintptr, info unsafe.Pointer) uint64 { return unsupported }

func threadExit(code uint64) {}

func threadJoin(thread uint64, flags uint64, code unsafe.Pointer) uint64 { return unsupported }

func threadSelf(info unsafe.Pointer) uint64 { return unsupported }

func threadYield() uint64 { return unsupported }

func threadSuspend(snapshots unsafe.Pointer, capacity uint64, count unsafe.Pointer) uint64 {
	return unsupported
}

func threadResume() uint64 { return unsupported }

func threadEntryAddress() uintptr { return 0 }

func workerEntryAddress() uintptr { return 0 }

func threadSetBase(base uintptr) uint64 { return unsupported }

func threadPreempt(entry uintptr, ticks uint64) uint64 { return unsupported }

func probeVectorSpin(stop *uint32) uint64 { return 0 }

func threadWake(thread uint64) uint64 { return unsupported }

func threadWakeTake() uint64 { return unsupported }

func ipcSend(channel uint64, address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func ipcReceive(channel uint64, address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func ipcWaitReceive(channel uint64, descriptor unsafe.Pointer, size uintptr) uint64 {
	return unsupported
}

func consoleWrite(device uint64, address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func consoleRead(device uint64, address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func consoleInterrupt(device uint64) uint64 { return unsupported }

func consoleInfo(device uint64, address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func clockRead(address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func clockReadNanos(address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func clockNow(clock uint64, address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func clockSet(authority uint64, address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func startupRead(address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func channelCreate(address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func handleClose(id uint64) uint64 { return unsupported }

func processWait(process uint64, result unsafe.Pointer, timeout uint64) uint64 { return unsupported }

func systemInfoRead(address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func cpuInfoRead(address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func cpuEntryRead(index uint64, address unsafe.Pointer) uint64 { return unsupported }

func cpuCacheInfoRead(index uint64, address unsafe.Pointer) uint64 { return unsupported }

func cpuRuntimeRead(index uint64, address unsafe.Pointer) uint64 { return unsupported }

func taskAffinity(process uint64, mask uint64, address unsafe.Pointer) uint64 { return unsupported }

func portRead(device uint64, specification uint64, address unsafe.Pointer) uint64 { return unsupported }

func portWrite(device uint64, specification uint64, value uint64) uint64 { return unsupported }

func dmaAllocate(device uint64, virtual uint64, address unsafe.Pointer) uint64 { return unsupported }

func irqWait(irq uint64) uint64 { return unsupported }
func irqTake(irq uint64) uint64 { return unsupported }

func randomRead(address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func streamCreate(address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func streamRead(stream uint64, descriptor unsafe.Pointer, size uintptr) uint64 { return unsupported }

func streamWrite(stream uint64, descriptor unsafe.Pointer, size uintptr) uint64 { return unsupported }

func streamWait(stream uint64, timeout uint64) uint64 { return unsupported }

func waitAny(interests unsafe.Pointer, count uintptr, result unsafe.Pointer) uint64 {
	return unsupported
}

func diagnostics(device uint64, address uintptr) uint64 { return unsupported }

func logWrite(device uint64, address unsafe.Pointer, size uintptr) uint64 { return unsupported }

func logRead(device uint64, index uint64, address unsafe.Pointer) uint64 { return unsupported }

func logBounds(device uint64, address unsafe.Pointer) uint64 { return unsupported }

func handleInfo(slot uint64, address unsafe.Pointer) uint64 { return unsupported }

func processInfo(index uint64, address unsafe.Pointer) uint64 { return unsupported }

func systemReset(device uint64) uint64 { return unsupported }

func memoryMap(memory uint64, address uint64, flags uint64) uint64 { return unsupported }

func processKill(process uint64) uint64 { return unsupported }

func imageMap(device uint64, virtual uint64, address unsafe.Pointer) uint64 { return unsupported }

func validationFault(device uint64) uint64 { return unsupported }

func systemPowerOff(device uint64) uint64 { return unsupported }

func powerOffReady(device uint64) uint64 { return unsupported }

func executableCreate(loader uint64, size uint64, address unsafe.Pointer) uint64 {
	return unsupported
}

func executableWrite(executable uint64, address unsafe.Pointer, size uintptr) uint64 {
	return unsupported
}

func executableSource(loader uint64, session uint64, descriptor unsafe.Pointer) uint64 {
	return unsupported
}

func vmMap(descriptor unsafe.Pointer, protection uint64, flags uint64) uint64 { return unsupported }

func vmUnmap(address uint64, length uint64) uint64 { return unsupported }

func vmProtect(address uint64, length uint64, protection uint64) uint64 { return unsupported }

func vmDiscard(address uint64, length uint64) uint64 { return unsupported }

func vmQuery(address uint64, record unsafe.Pointer) uint64 { return unsupported }

func memoryCreate(pages uint64, address unsafe.Pointer) uint64 { return unsupported }

func handleDuplicate(handle uint64, rights uint64, address unsafe.Pointer) uint64 {
	return unsupported
}

func logReadSequence(device uint64, cursor uint64, address unsafe.Pointer) uint64 { return unsupported }

func irqPoll(irq uint64) uint64 { return unsupported }
