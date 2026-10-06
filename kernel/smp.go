//go:build uefi

package main

// Processor start-up. The bootstrap processor starts the others once its own
// scheduler runs, one at a time, and leaves each parked; it brings them
// online when boot hands the machine to userspace. A processor that does not
// start, or does not join, stays offline and boot goes on (docs/kernel/smp.md).

import (
	"goos/internal/validation"
	"goos/kernel/arch/amd64"
	"goos/kernel/boot"
	kernelconsole "goos/kernel/console"
	kernelcpu "goos/kernel/cpu"
)

// cpuTable is the live state of the processors in cpuInventory.
var cpuTable kernelcpu.Table

// startProcessors sends every startable application processor its start-up
// signals and waits for each to reach the kernel on its own stack.
func startProcessors() {
	console := kernelconsole.New()
	if !cpuTable.Load(&cpuInventory) {
		console.WriteString("SMP: processor inventory unusable, running on CPU 0\n")
		return
	}
	if cpuTable.Count() == 1 {
		return
	}
	page, found := boot.LowPage(bootInfo.MemoryRegions())
	if !found {
		console.WriteString("SMP: no free page below 1 MiB, running on CPU 0\n")
		return
	}
	startup, prepared := amd64.PrepareStartup(&pageAllocator, kernelPageTable, page)
	if !prepared {
		console.WriteString("SMP: start-up trampoline unavailable, running on CPU 0\n")
		return
	}
	startable, started := 0, 0
	for index := 1; index < cpuTable.Count(); index++ {
		apicID, _ := cpuTable.APICID(index)
		if !cpuTable.Startable(index) {
			console.WriteString("SMP: CPU ")
			console.WriteUint(uint64(index))
			console.WriteString(" has APIC ID ")
			console.WriteUint(uint64(apicID))
			console.WriteString(", beyond the xAPIC destinations; left offline\n")
			continue
		}
		if !prepareScratch(index) {
			cpuTable.Transition(index, kernelcpu.Failed)
			console.WriteString("SMP: no memory for CPU ")
			console.WriteUint(uint64(index))
			console.WriteString("'s call scratch; left offline\n")
			continue
		}
		startable++
		if amd64.StartProcessor(&startup, &pageAllocator, kernelPageTable, index, apicID, waitMicros) {
			cpuTable.Transition(index, kernelcpu.Started)
			started++
			continue
		}
		cpuTable.Transition(index, kernelcpu.Failed)
		console.WriteString("SMP: CPU ")
		console.WriteUint(uint64(index))
		console.WriteString(" did not start; left offline\n")
	}
	// A processor that missed its deadline may still be on the way through
	// the trampoline, so its page tables stay unless every processor arrived.
	if started == startable {
		startup.Release(&pageAllocator, kernelPageTable)
		if validation.Enabled {
			mark("SMP_AP_START_OK")
		}
	}
}

// bringProcessorsOnline lets every started processor join the scheduler and
// publishes the result to the CPU information calls. The boot task calls it
// once, with interrupts disabled.
func bringProcessorsOnline() {
	console := kernelconsole.New()
	if cpuTable.Count() == 0 {
		return
	}
	online := 1
	for index := 1; index < cpuTable.Count(); index++ {
		if cpuTable.State(index) != kernelcpu.Started {
			continue
		}
		if amd64.BringOnline(index, waitMicros) {
			cpuTable.Transition(index, kernelcpu.Online)
			online++
			continue
		}
		cpuTable.Transition(index, kernelcpu.Failed)
		console.WriteString("SMP: CPU ")
		console.WriteUint(uint64(index))
		console.WriteString(" did not join the scheduler; left offline\n")
	}
	cpuTable.Publish(&cpuInventory)
	console.WriteString("SMP: ")
	console.WriteUint(uint64(online))
	console.WriteString(" of ")
	console.WriteUint(uint64(cpuInventory.CPUCount))
	console.WriteString(" processors online\n")
}
