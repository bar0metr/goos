//go:build uefi

package main

import (
	"goos/kernel/acpi"
	"goos/kernel/arch/amd64"
	kernelcpu "goos/kernel/cpu"
	"goos/kernel/cpu/x86"
)

type cpuidSource struct{}

func (cpuidSource) Query(leaf, subleaf uint32) x86.Registers {
	return amd64.CPUID(leaf, subleaf)
}

var cpuInventory kernelcpu.Inventory
var cpuProcessors acpi.Processors
var cpuProbe x86.Inventory

//go:noinline
func collectCPUInventory(memory acpi.Memory, root uint64) {
	valid := acpi.FindProcessorsInto(&cpuProcessors, memory, root)
	x86.ProbeInto(&cpuProbe, cpuidSource{})
	kernelcpu.Normalize(&cpuInventory, &cpuProbe, &cpuProcessors, valid)
}
