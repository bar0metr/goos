//go:build uefi

package main

import (
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
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
