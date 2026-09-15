//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	"goos/kernel/boot"
	bootuefi "goos/kernel/boot/uefi"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/loader"
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
)

var (
	bootInfo        boot.Info
	pageAllocator   pmm.Allocator
	demoTaskA       uint64
	demoTaskB       uint64
	demoABlocked    uint64
	demoAResumed    uint64
	userExited      uint64
	kernelCallData  [kcall.MaxDebugWrite]byte
	nativeImageData [512]byte
	faultImageData  [256]byte
)

type nativeCallHandler struct {
	root uint64
}

func (handler nativeCallHandler) Exit(code uint64) kcall.Status {
	userExited = 1
	console := amd64.NewCOM1()
	console.WriteString("USER_EXIT_OK\n")
	return kcall.StatusOK
}

func (handler nativeCallHandler) DebugWrite(address uint64, length uint64) kcall.Status {
	console := amd64.NewCOM1()
	data := kernelCallData[:int(length)]
	if !vm.CopyIn(handler.root, address, data) {
		console.WriteString("USER_COPYIN_OK\n")
		return kcall.StatusInvalidArgument
	}
	for _, value := range data {
		console.WriteByte(value)
	}
	return kcall.StatusOK
}

// dispatchKernelCall is the architecture-neutral native-call boundary. The
// packed return keeps the assembly bridge simple: action occupies the upper
// half and the typed native status occupies the lower half.
//
//export goos_dispatch_kernel_call
func dispatchKernelCall(number uint64, arg0 uint64, arg1 uint64, root uint64) uint64 {
	result := kcall.Dispatch(kcall.Request{Operation: kcall.Operation(number), Arg0: arg0, Arg1: arg1}, nativeCallHandler{root: root})
	return uint64(result.Action)<<32 | uint64(result.Status)
}

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
	console.WriteString("Kernel\n\n")
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
	rootA, addressSpaceA := vm.CreateUserAddressSpace(&pageAllocator, pageTable)
	rootB, addressSpaceB := vm.CreateUserAddressSpace(&pageAllocator, pageTable)
	if addressSpaceA {
		addressSpaceA = vm.MapUserPage(rootA, vm.UserProbeAddress, physicalA, vm.Permissions{User: true, Writable: true})
	}
	if addressSpaceB {
		addressSpaceB = vm.MapUserPage(rootB, vm.UserProbeAddress, physicalB, vm.Permissions{User: true, Writable: true})
	}
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
	if !validA || !validB {
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

	userProgram := [...]byte{
		0xb9, 0x00, 0x00, 0x00, 0x01, // mov ecx, 0x01000000: remain preemptible.
		0xff, 0xc9, // dec ecx.
		0x75, 0xfc, // jnz dec ecx.
		0xb8, 0x02, 0x00, 0x00, 0x00, // mov eax, 2: DebugWrite.
		0x48, 0xbf, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // mov rdi, 0: invalid pointer.
		0xbe, 0x01, 0x00, 0x00, 0x00, // mov esi, 1.
		0xcd, 0x80,
		0x83, 0xf8, 0x01, // cmp eax, InvalidArgument.
		0x74, 0x02, // je valid write.
		0x0f, 0x0b,
		0xb8, 0x02, 0x00, 0x00, 0x00, // mov eax, 2: DebugWrite.
		0x48, 0xbf, 0x00, 0x40, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, // mov rdi, UserDataAddress.
		0xbe, 0x14, 0x00, 0x00, 0x00, // mov esi, 20.
		0xcd, 0x80,
		0x85, 0xc0, // test eax, eax.
		0x74, 0x02, // je exit.
		0x0f, 0x0b,
		0xb8, 0x01, 0x00, 0x00, 0x00, // mov eax, 1: Exit.
		0x31, 0xff, // xor edi, edi: status 0.
		0xcd, 0x80,
		0x0f, 0x0b, // ud2: returning to exited user code is always a bug.
	}
	userMessage := []byte("USER_DEBUG_WRITE_OK\n")
	faultProgram := []byte{0x0f, 0x0b}
	nativeBytes, nativeEncoded := loader.EncodeNative(nativeImageData[:], loader.ImageSource{
		Entry:    vm.UserCodeAddress,
		StackTop: vm.UserStackAddress + vm.PageSize - 16,
		Segments: []loader.SegmentSource{
			{VirtualAddress: vm.UserCodeAddress, MemorySize: uint32(len(userProgram)), Flags: loader.SegmentReadable | loader.SegmentExecutable, Data: userProgram[:]},
			{VirtualAddress: vm.UserStackAddress, MemorySize: uint32(vm.PageSize), Flags: loader.SegmentReadable | loader.SegmentWritable},
			{VirtualAddress: vm.UserDataAddress, MemorySize: uint32(len(userMessage)), Flags: loader.SegmentReadable, Data: userMessage},
		},
	})
	faultBytes, faultEncoded := loader.EncodeNative(faultImageData[:], loader.ImageSource{
		Entry:    vm.UserCodeAddress,
		StackTop: vm.UserStackAddress + vm.PageSize - 16,
		Segments: []loader.SegmentSource{
			{VirtualAddress: vm.UserCodeAddress, MemorySize: uint32(len(faultProgram)), Flags: loader.SegmentReadable | loader.SegmentExecutable, Data: faultProgram},
			{VirtualAddress: vm.UserStackAddress, MemorySize: uint32(vm.PageSize), Flags: loader.SegmentReadable | loader.SegmentWritable},
		},
	})
	loadedA, loadedImageA := loader.LoadNative(&pageAllocator, rootA, nativeBytes)
	loadedB, loadedImageB := loader.LoadNative(&pageAllocator, rootB, faultBytes)
	if !nativeEncoded || !faultEncoded || !loadedImageA || !loadedImageB {
		console.WriteString("PANIC: native user image setup failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("USER_RX_OK\n")
	console.WriteString("USER_STACK_NX_OK\n")
	copyOutProbe := []byte{0x5a}
	if vm.CopyOut(rootA, vm.UserDataAddress, copyOutProbe) || !vm.CopyOut(rootA, vm.UserStackAddress, copyOutProbe) {
		console.WriteString("PANIC: user copyout validation failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("USER_COPYOUT_OK\n")
	console.WriteString("KCALL_ABI_OK\n")
	console.WriteString("NATIVE_IMAGE_LOADER_OK\n")
	userTaskID, userTaskAdded := amd64.AddUserTask(&pageAllocator, rootA, loadedA.Entry, loadedA.StackTop)
	faultTaskID, faultTaskAdded := amd64.AddUserTask(&pageAllocator, rootB, loadedB.Entry, loadedB.StackTop)
	if !userTaskAdded || !faultTaskAdded || !processA.AddTask(sched.TaskID(userTaskID)) || !processB.AddTask(sched.TaskID(faultTaskID)) {
		console.WriteString("PANIC: userspace task scheduling failed\n")
		for {
			amd64.Halt()
		}
	}
	for userExited == 0 || amd64.UserFaults() == 0 {
		amd64.Halt()
	}
	if amd64.UserPreemptions() == 0 {
		console.WriteString("PANIC: userspace task was not preempted\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("USER_PREEMPT_OK\n")
	console.WriteString("PROCESS_CR3_SCHED_OK\n")
	console.WriteString("USER_FAULT_ISOLATION_OK\n")
	console.WriteString("USER_RING3_OK\n")
	console.WriteString("NATIVE_PROCESS_OK\n")
	console.WriteString("Kernel initialized\n")
	console.WriteString("BOOT_OK\n")

	for {
		amd64.Halt()
	}
}
