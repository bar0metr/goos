//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/internal/logfmt"
	"goos/kernel/arch/amd64"
	"goos/kernel/device"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
)

// HandleInfo describes one slot of the caller's own handle table: handle ID,
// type, and rights. It never reveals the kernel object behind the handle.
func (handler nativeCallHandler) HandleInfo(slot uint64, address uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	if slot >= handle.MaxHandles {
		return kcall.StatusInvalidArgument
	}
	id, typeID, authority, open := owner.Handles().At(int(slot))
	if !open {
		return kcall.StatusNotFound
	}
	record := handleInfoWire[:]
	clear(record)
	putUint64(record[0:8], uint64(id))
	record[8] = byte(typeID)
	putUint32(record[12:16], uint32(authority.Rights()))
	if !vm.CopyOut(handler.root, address, record) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// ProcessInfo reports the index-th live process: ID, scheduling state, task
// count, executable name, CPU ticks, and its resource account. Like SystemInfo
// it needs no capability: the record names no kernel object and reveals only
// what a process list already shows.
func (handler nativeCallHandler) ProcessInfo(index uint64, address uint64) kcall.Status {
	if _, found := processRegistry.ByRoot(handler.root); !found {
		return kcall.StatusPermissionDenied
	}
	if index >= process.MaxProcesses {
		return kcall.StatusNotFound
	}
	target, found := processRegistry.At(int(index))
	if !found {
		return kcall.StatusNotFound
	}
	state := amd64.ReadSchedulerState()
	status := kcall.ProcessWaiting
	for _, task := range target.Tasks() {
		slot, _, valid := task.Parts()
		switch {
		case !valid:
		case uint64(slot) == state.Current:
			status = kcall.ProcessRunning
		case state.Runnable&(1<<slot) != 0 && status != kcall.ProcessRunning:
			status = kcall.ProcessReady
		}
	}
	record := processInfoWire[:]
	clear(record)
	putUint64(record[0:8], uint64(target.ID()))
	record[8] = status
	record[9] = byte(target.TaskCount())
	copy(record[16:16+kcall.MaxProcessName], target.Name())
	putUint64(record[kcall.ProcessInfoCPUOffset:kcall.ProcessInfoCPUOffset+8], processCPUTicks(target))
	used, limits := target.Quota()
	for resource := 0; resource < kcall.ProcessResources; resource++ {
		offset := kcall.ProcessInfoUsedOffset + resource*4
		putUint32(record[offset:offset+4], used[resource])
		offset = kcall.ProcessInfoLimitsOffset + resource*4
		putUint32(record[offset:offset+4], limits[resource])
	}
	if !vm.CopyOut(handler.root, address, record) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// SystemReset restarts the machine. Only the holder of the system-control
// capability, granted to PID 1 at boot, may call it; it does not return.
func (handler nativeCallHandler) SystemReset(device uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(device), handle.TypeDevice, capability.Write)
	if !allowed || authority.Object() != deviceSystemControl {
		return kcall.StatusPermissionDenied
	}
	kernelLogUint(logfmt.Info, "system reset", "pid", uint64(owner.ID()))
	amd64.SystemReset()
	return kcall.StatusIOError
}

const deviceSystemControl = device.SystemControlObject
