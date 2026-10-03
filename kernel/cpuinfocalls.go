//go:build uefi

package main

import (
	"goos/internal/cpuinfo"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
)

var (
	cpuInfoWire  [cpuinfo.SummarySize]byte
	cpuEntryWire [cpuinfo.CPUEntrySize]byte
	cpuCacheWire [cpuinfo.CacheInfoSize]byte
)

func (handler nativeCallHandler) CPUInfo(address uint64, length uint64) kcall.Status {
	if _, found := processRegistry.ByRoot(handler.root); !found {
		return kcall.StatusPermissionDenied
	}
	if length != kcall.CPUInfoSize || !cpuInventory.Summary.Encode(cpuInfoWire[:]) {
		return kcall.StatusInvalidArgument
	}
	if !vm.CopyOut(handler.root, address, cpuInfoWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) CPUEntry(index uint64, address uint64) kcall.Status {
	if _, found := processRegistry.ByRoot(handler.root); !found {
		return kcall.StatusPermissionDenied
	}
	if index >= uint64(cpuInventory.CPUCount) {
		return kcall.StatusNotFound
	}
	cpuInventory.CPUs[index].Encode(cpuEntryWire[:])
	if !vm.CopyOut(handler.root, address, cpuEntryWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) CPUCacheInfo(index uint64, address uint64) kcall.Status {
	if _, found := processRegistry.ByRoot(handler.root); !found {
		return kcall.StatusPermissionDenied
	}
	if index >= uint64(cpuInventory.CacheCount) {
		return kcall.StatusNotFound
	}
	cpuInventory.Caches[index].Encode(cpuCacheWire[:])
	if !vm.CopyOut(handler.root, address, cpuCacheWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}
