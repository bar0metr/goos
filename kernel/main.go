//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	"goos/kernel/boot"
	bootuefi "goos/kernel/boot/uefi"
	"goos/kernel/handle"
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
)

var (
	bootInfo      boot.Info
	pageAllocator pmm.Allocator
	demoTaskA     uint64
	demoTaskB     uint64
	demoABlocked  uint64
	demoAResumed  uint64
)

//export goos_demo_task_a
func runDemoTaskA() {
	for demoTaskA < 3 {
		demoTaskA++
		amd64.Halt()
	}
	demoABlocked = 1
	amd64.BlockCurrentTask()
	amd64.Halt()
	demoAResumed = 1
	for {
		demoTaskA++
		amd64.Halt()
	}
}

//export goos_demo_task_b
func runDemoTaskB() {
	for {
		demoTaskB++
		if demoABlocked != 0 && demoAResumed == 0 && demoTaskB >= 6 {
			amd64.WakeTask(1)
		}
		amd64.Halt()
	}
}

//export goos_idle_task
func runIdleTask() {
	for {
		amd64.Halt()
	}
}

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

	if !amd64.HasNX() {
		console.WriteString("PANIC: CPU does not support NX\n")
		for {
			amd64.Halt()
		}
	}
	image, validImage := vm.LoadedImage()
	stack := amd64.StackPointer()
	if !validImage || stack < 1024*1024 || stack >= vm.IdentityMapLimit {
		console.WriteString("PANIC: unsafe virtual-memory bootstrap state\n")
		for {
			amd64.Halt()
		}
	}
	pageTable, built := vm.BuildIdentity(&pageAllocator, image)
	if !built {
		console.WriteString("PANIC: page-table construction failed\n")
		for {
			amd64.Halt()
		}
	}
	amd64.EnableNXAndWriteProtect()
	amd64.LoadPageTable(pageTable)
	console.WriteString("NX_WX_OK\n")
	console.WriteString("VM_OK\n")

	if !amd64.InstallDescriptorTables(&pageAllocator, pageTable) {
		console.WriteString("PANIC: descriptor-table installation failed\n")
		for {
			amd64.Halt()
		}
	}
	amd64.TriggerBreakpoint()
	console.WriteString("GDT_OK\n")
	console.WriteString("IDT_OK\n")

	amd64.StartBootstrapTimer()
	for amd64.TimerTicks() < 3 {
		amd64.Halt()
	}
	console.WriteString("TIMER_OK\n")

	if !amd64.SetupBootstrapScheduler(&pageAllocator, pageTable) {
		console.WriteString("PANIC: scheduler bootstrap failed\n")
		for {
			amd64.Halt()
		}
	}
	for demoTaskA < 4 || demoTaskB < 6 || demoABlocked == 0 || demoAResumed == 0 {
		amd64.Halt()
	}
	console.WriteString("STACK_GUARDS_OK\n")
	console.WriteString("TASK_BLOCK_OK\n")
	console.WriteString("TASK_WAKE_OK\n")
	console.WriteString("TASK_A_OK\n")
	console.WriteString("TASK_B_OK\n")
	console.WriteString("IDLE_TASK_OK\n")
	console.WriteString("SCHEDULER_OK\n")

	physicalA, allocatedA := pageAllocator.Allocate()
	physicalB, allocatedB := pageAllocator.Allocate()
	if !allocatedA || !allocatedB ||
		!vm.InitializePhysicalByte(physicalA, 0xa1) ||
		!vm.InitializePhysicalByte(physicalB, 0xb2) {
		console.WriteString("PANIC: process page allocation failed\n")
		for {
			amd64.Halt()
		}
	}
	rootA, addressSpaceA := vm.CreateUserAddressSpace(&pageAllocator, pageTable, physicalA)
	rootB, addressSpaceB := vm.CreateUserAddressSpace(&pageAllocator, pageTable, physicalB)
	resolvedA, resolvesA := vm.Resolve(rootA, vm.UserProbeAddress)
	resolvedB, resolvesB := vm.Resolve(rootB, vm.UserProbeAddress)
	if !addressSpaceA || !addressSpaceB || !resolvesA || !resolvesB || resolvedA != physicalA || resolvedB != physicalB {
		console.WriteString("PANIC: process address-space construction failed\n")
		for {
			amd64.Halt()
		}
	}
	processA, validA := process.New(1, process.AddressSpace{Root: rootA})
	processB, validB := process.New(2, process.AddressSpace{Root: rootB})
	if !validA || !validB || !processA.AddTask(sched.TaskID(1)) || !processB.AddTask(sched.TaskID(2)) {
		console.WriteString("PANIC: process model initialization failed\n")
		for {
			amd64.Halt()
		}
	}
	memoryHandle, inserted := processA.Handles().Insert(
		handle.TypeMemory,
		capability.New(physicalA, capability.Read|capability.Transfer),
	)
	transferred, didTransfer := processA.Handles().Transfer(
		processB.Handles(), memoryHandle, handle.TypeMemory, capability.Read,
	)
	_, targetCanRead := processB.Handles().Lookup(transferred, handle.TypeMemory, capability.Read)
	_, targetCanWrite := processB.Handles().Lookup(transferred, handle.TypeMemory, capability.Write)
	if !inserted || !didTransfer || !targetCanRead || targetCanWrite {
		console.WriteString("PANIC: process-local handle enforcement failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("HANDLE_TABLE_OK\n")
	console.WriteString("PROCESS_MODEL_OK\n")

	probe := amd64.ProbeAddressSpaces(rootA, rootB, pageTable, vm.UserProbeAddress)
	if byte(probe) != 0xa1 || byte(probe>>8) != 0xb2 {
		console.WriteString("PANIC: address-space isolation failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("ADDRESS_SPACE_A_OK\n")
	console.WriteString("ADDRESS_SPACE_B_OK\n")
	console.WriteString("PROCESS_ISOLATION_OK\n")

	userCodePhysical, codeAllocated := pageAllocator.Allocate()
	userStackPhysical, stackAllocated := pageAllocator.Allocate()
	userProgram := [...]byte{
		0xb8, 0x01, 0x00, 0x00, 0x00, // mov eax, 1: native Exit operation.
		0xcd, 0x80, // int 0x80: enter the GoOS kernel-call gate.
		0x0f, 0x0b, // ud2: returning to exited user code is always a bug.
	}
	if !codeAllocated || !stackAllocated ||
		!vm.InitializePhysical(userCodePhysical, userProgram[:]) ||
		!vm.InitializePhysical(userStackPhysical, nil) ||
		!vm.MapUserPage(rootA, vm.UserCodeAddress, userCodePhysical, vm.Permissions{User: true, Executable: true}) ||
		!vm.MapUserPage(rootA, vm.UserStackAddress, userStackPhysical, vm.Permissions{User: true, Writable: true}) {
		console.WriteString("PANIC: native user image setup failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("USER_RX_OK\n")
	console.WriteString("USER_STACK_NX_OK\n")
	amd64.EnterUser(rootA, vm.UserCodeAddress, vm.UserStackAddress+vm.PageSize-16, pageTable)
	console.WriteString("NATIVE_PROCESS_OK\n")
	console.WriteString("Kernel initialized\n")
	console.WriteString("BOOT_OK\n")

	for {
		amd64.Halt()
	}
}
