//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
)

func (handler nativeCallHandler) CPUInfo(address uint64, length uint64) kcall.Status {
	local := thisCPU()
	if _, found := handler.self(); !found {
		return kcall.StatusPermissionDenied
	}
	if length != kcall.CPUInfoSize || !cpuInventory.Summary.Encode(local.cpuInfoWire[:]) {
		return kcall.StatusInvalidArgument
	}
	if !vm.CopyOut(handler.root, address, local.cpuInfoWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) CPUEntry(index uint64, address uint64) kcall.Status {
	local := thisCPU()
	if _, found := handler.self(); !found {
		return kcall.StatusPermissionDenied
	}
	if index >= uint64(cpuInventory.CPUCount) {
		return kcall.StatusNotFound
	}
	cpuInventory.CPUs[index].Encode(local.cpuEntryWire[:])
	if !vm.CopyOut(handler.root, address, local.cpuEntryWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) CPUCacheInfo(index uint64, address uint64) kcall.Status {
	local := thisCPU()
	if _, found := handler.self(); !found {
		return kcall.StatusPermissionDenied
	}
	if index >= uint64(cpuInventory.CacheCount) {
		return kcall.StatusNotFound
	}
	cpuInventory.Caches[index].Encode(local.cpuCacheWire[:])
	if !vm.CopyOut(handler.root, address, local.cpuCacheWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// CPURuntime writes one processor's scheduler counters. Any registered
// process may read them, as it may read the CPU inventory; they name task
// identities and counts, never kernel addresses or scheduler state it could
// change.
func (handler nativeCallHandler) CPURuntime(index uint64, address uint64) kcall.Status {
	local := thisCPU()
	if _, found := handler.self(); !found {
		return kcall.StatusPermissionDenied
	}
	if index >= uint64(cpuInventory.CPUCount) || index >= amd64.MaxCPUs {
		return kcall.StatusNotFound
	}
	runtime := amd64.ReadCPURuntime(int(index))
	record := kcall.CPURuntime{Index: uint32(index), Online: runtime.Online, Current: uint64(runtime.Current),
		Ticks: runtime.Ticks, IdleTicks: runtime.IdleTicks, Switches: runtime.Scheduler.Switches,
		Steals: runtime.Scheduler.Steals, Migrations: runtime.Scheduler.Migrations,
		Reschedules: runtime.Reschedules, Shootdowns: runtime.Shootdowns}
	if !record.Encode(local.cpuRuntimeWire[:]) || !vm.CopyOut(handler.root, address, local.cpuRuntimeWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// TaskAffinity reads, and when mask is not zero first sets, the processors
// a process's tasks may run on. Handle zero names the calling task. Another
// process needs a process handle: one with Wait to read, with Write to set,
// which sets every task of the process. The mask read back, written to
// address unless it is zero, is that of the process's first task.
func (handler nativeCallHandler) TaskAffinity(processHandle uint64, mask uint64, address uint64) kcall.Status {
	local := thisCPU()
	owner, found := handler.self()
	if !found {
		return kcall.StatusPermissionDenied
	}
	tasks := local.affinityTasks[:0]
	if processHandle == 0 {
		tasks = append(tasks, amd64.CurrentTask())
	} else {
		required := capability.Wait
		if mask != 0 {
			required = capability.Write
		}
		authority, allowed := owner.Handles().Lookup(handle.ID(processHandle), handle.TypeProcess, required)
		if !allowed {
			return kcall.StatusPermissionDenied
		}
		target, exists := processRegistry.ByID(process.ID(authority.Object()))
		if !exists || len(target.Tasks()) == 0 {
			return kcall.StatusNotFound
		}
		tasks = append(tasks, target.Tasks()...)
	}
	if mask != 0 {
		if mask&amd64.OnlineMask() == 0 {
			return kcall.StatusInvalidArgument
		}
		for _, task := range tasks {
			if !amd64.SetTaskAffinity(task, mask) {
				return kcall.StatusNotFound
			}
		}
	}
	if address == 0 {
		return kcall.StatusOK
	}
	current, alive := amd64.TaskAffinity(tasks[0])
	if !alive {
		return kcall.StatusNotFound
	}
	putUint64(local.affinityWire[:], current)
	if !vm.CopyOut(handler.root, address, local.affinityWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}
