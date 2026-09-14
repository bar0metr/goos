//go:build uefi

package main

import (
	"goos/kernel/arch/amd64"
	"goos/kernel/boot"
	bootuefi "goos/kernel/boot/uefi"
	"goos/kernel/mm/pmm"
)

var (
	bootInfo      boot.Info
	pageAllocator pmm.Allocator
)

func main() {
	console := amd64.NewCOM1()
	console.Init()
	console.WriteString("GoOS 0.1-dev\n")
	console.WriteString("TinyGo kernel\n\n")
	console.WriteString("CPU: x86_64\n")
	console.WriteString("Boot: UEFI\n")
	console.WriteString("Security: capabilities (foundation)\n")

	status := bootuefi.CaptureAndExit(&bootInfo)
	if status != 0 {
		console.WriteString("PANIC: UEFI handoff failed: ")
		console.WriteHex(uint64(status))
		console.WriteString("\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("BOOT_SERVICES_EXITED\n")
	console.WriteString("Memory: ")
	console.WriteUint(bootInfo.UsableBytes / (1024 * 1024))
	console.WriteString(" MiB usable\n")

	pageAllocator.Init(bootInfo.MemoryRegions())
	page, ok := pageAllocator.Allocate()
	if !ok || !pageAllocator.Free(page) {
		console.WriteString("PANIC: physical page allocator self-test failed\n")
		for {
			amd64.Halt()
		}
	}
	stats := pageAllocator.Statistics()
	console.WriteString("Physical pages free: ")
	console.WriteUint(stats.FreePages)
	console.WriteString("\nPMM_OK\n")
	console.WriteString("Kernel initialized\n")
	console.WriteString("BOOT_OK\n")

	for {
		amd64.Halt()
	}
}
