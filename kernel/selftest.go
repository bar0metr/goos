//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	kernelconsole "goos/kernel/console"
	"goos/kernel/device"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/loader"
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/kernel/tty"
	supervisor "goos/userspace/init"
)

// The self-tests below run only in validation builds. They exercise the
// kernel's own mechanisms in place: a scheduling demonstration, two fixed
// processes that probe isolation, IPC, TTY rights, and fault containment, and
// in-kernel checks of RAMFS, VFS, bounded channels, and shared memory. The
// RAMFS and VFS checks live in filesystem_selftest_on.go, compiled only into
// validation builds, so the normal kernel does not link a filesystem (ADR 0084).

var (
	selfTestRootA      uint64
	selfTestRootB      uint64
	selfTestRootLinux  uint64
	selfTestLinuxEntry uint64
	selfTestLinuxStack uint64
	selfTestLoadedA    loader.LoadedImage
	selfTestLoadedB    loader.LoadedImage
)

const selfTestLinuxProcessID = process.ID(0x7fff0001)

func awaitSchedulerSelfTest() {
	for demoTaskA < 4 || demoTaskB < 6 || demoABlocked == 0 || demoAResumed == 0 {
		amd64.Halt()
	}
	mark("STACK_GUARDS_OK")
	mark("TASK_BLOCK_OK")
	mark("TASK_WAKE_OK")
	mark("TASK_A_OK")
	mark("TASK_B_OK")
	mark("IDLE_TASK_OK")
	mark("SCHEDULER_OK")
}

func runKernelSelfTests(pageTable uint64) {
	local := thisCPU()
	checkHighMemory(pageTable)
	console := kernelconsole.New()
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
	addressSpaceA = addressSpaceA && amd64.PrepareUserAddressSpace(&pageAllocator, rootA)
	addressSpaceB = addressSpaceB && amd64.PrepareUserAddressSpace(&pageAllocator, rootB)
	if _, mapped := vm.Resolve(rootA, vm.KernelImageBase); mapped {
		panicSerial("process root retained the low kernel image")
	}
	if _, mapped := vm.Resolve(rootA, vm.KernelLowBase+vm.KernelImageBase); mapped {
		panicSerial("process root retained the complete kernel alias")
	}
	if framebufferVirtual != 0 {
		if _, mapped := vm.Resolve(rootA, framebufferVirtual); mapped {
			panicSerial("process root retained the framebuffer mapping")
		}
		if _, mapped := vm.Resolve(rootB, framebufferVirtual); mapped {
			panicSerial("process root retained the framebuffer mapping")
		}
		mark("FRAMEBUFFER_USER_ISOLATION_OK")
	}
	if addressSpaceA {
		_, addressSpaceA = vm.MapUserPageAt(&pageAllocator, rootA, vm.UserProbeAddress, physicalA, vm.Permissions{User: true, Writable: true}, false)
	}
	if addressSpaceB {
		_, addressSpaceB = vm.MapUserPageAt(&pageAllocator, rootB, vm.UserProbeAddress, physicalB, vm.Permissions{User: true, Writable: true}, false)
	}
	resolvedA, resolvesA := vm.Resolve(rootA, vm.UserProbeAddress)
	resolvedB, resolvesB := vm.Resolve(rootB, vm.UserProbeAddress)
	if !addressSpaceA || !addressSpaceB || !resolvesA || !resolvesB || resolvedA != physicalA || resolvedB != physicalB {
		console.WriteString("PANIC: process address-space construction failed\n")
		for {
			amd64.Halt()
		}
	}
	selfTestRootA, selfTestRootB = rootA, rootB
	// Process records are too large for a kernel stack; they live in pages
	// as every other process's do (ADR 0111).
	processA, validA := vm.NewObject[process.Process](&pageAllocator)
	processB, validB := vm.NewObject[process.Process](&pageAllocator)
	linuxProcess, validLinux := vm.NewObject[process.Process](&pageAllocator)
	validA = validA && process.Init(processA, 2, process.ABINative, process.AddressSpace{Root: rootA})
	validB = validB && process.Init(processB, 3, process.ABINative, process.AddressSpace{Root: rootB})
	rootLinux, addressSpaceLinux := vm.CreateUserAddressSpace(&pageAllocator, pageTable)
	addressSpaceLinux = addressSpaceLinux && amd64.PrepareUserAddressSpace(&pageAllocator, rootLinux)
	validLinux = validLinux && process.Init(linuxProcess, selfTestLinuxProcessID, process.ABILinuxX8664, process.AddressSpace{Root: rootLinux})
	if validA && validB {
		processA.SetName([]byte("selftest-a"))
		processB.SetName([]byte("selftest-b"))
	}
	if validLinux {
		linuxProcess.SetName([]byte("linux-entry"))
	}
	bootProcessA, bootProcessB = processA, processB
	if !validA || !validB || !addressSpaceLinux || !processRegistry.Add(processA) || !processRegistry.Add(processB) || !processRegistry.Add(linuxProcess) {
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
	mark("HANDLE_TABLE_OK")
	mark("PROCESS_MODEL_OK")
	// Owner 0 leaves this channel unaccounted: it belongs to the kernel's own
	// self-test, not to a process, and must not move PID 1's counters.
	registeredChannel, registeredChannelID, channelCreated := channelRegistry.Create(0)
	registeredObjectA, _ := ipc.EndpointObject(registeredChannelID, 0)
	registeredObjectB, _ := ipc.EndpointObject(registeredChannelID, 1)
	channelA, channelAInstalled := processA.Handles().Insert(handle.TypeChannel, capability.New(registeredObjectA, capability.Read|capability.Write|capability.Wait))
	channelB, channelBInstalled := processB.Handles().Insert(handle.TypeChannel, capability.New(registeredObjectB, capability.Read|capability.Write|capability.Wait))
	_, channelACanWrite := processA.Handles().Lookup(channelA, handle.TypeChannel, capability.Write)
	_, channelBCanRead := processB.Handles().Lookup(channelB, handle.TypeChannel, capability.Read)
	request := ipc.Message{ProtocolID: 0x474f4f53, Version: 1, Kind: ipc.KindRequest, Correlation: 1, PayloadSize: 4}
	copy(request.Payload[:], []byte("ping"))
	wireSize, wireEncoded := ipc.Encode(request, local.ipcWireData[:])
	wireRequest, wireDecoded := ipc.Decode(local.ipcWireData[:wireSize])
	ipcStatus := registeredChannel.Send(0, wireRequest, processA.Handles(), processB.Handles(), []ipc.Transfer{{
		Handle: memoryHandle,
		Type:   handle.TypeMemory,
		Rights: capability.Read,
	}})
	receivedRequest, receivedStatus := registeredChannel.Receive(1)
	_, receivedMemoryReadable := processB.Handles().Lookup(receivedRequest.Handles[0], handle.TypeMemory, capability.Read)
	reply := ipc.Message{ProtocolID: request.ProtocolID, Version: request.Version, Kind: ipc.KindReply, Correlation: request.Correlation}
	replyStatus := registeredChannel.Send(1, reply, nil, nil, nil)
	_, replyReceiveStatus := registeredChannel.Receive(0)
	if !channelCreated || !channelAInstalled || !channelBInstalled || !channelACanWrite || !channelBCanRead || !wireEncoded || !wireDecoded ||
		ipcStatus != ipc.StatusOK || receivedStatus != ipc.StatusOK || receivedRequest.HandleCount != 1 || !receivedMemoryReadable ||
		string(receivedRequest.Payload[:receivedRequest.PayloadSize]) != "ping" || replyStatus != ipc.StatusOK || replyReceiveStatus != ipc.StatusOK {
		console.WriteString("PANIC: typed IPC request/reply failed\n")
		for {
			amd64.Halt()
		}
	}
	if channelA != handle.ID(0x0000000100000002) || channelB != handle.ID(0x0000000100000002) {
		console.WriteString("PANIC: bootstrap channel handle layout changed\n")
		for {
			amd64.Halt()
		}
	}
	mark("IPC_WIRE_OK")
	mark("IPC_REQUEST_REPLY_OK")
	mark("IPC_HANDLE_TRANSFER_OK")
	consoleDeviceB, consoleGranted := processB.Handles().Insert(handle.TypeDevice, capability.New(1, capability.Write))
	_, consoleBCanWrite := processB.Handles().Lookup(consoleDeviceB, handle.TypeDevice, capability.Write)
	if !consoleGranted || !consoleBCanWrite || consoleDeviceB != handle.ID(0x0000000100000004) {
		console.WriteString("PANIC: TTY device capability setup failed\n")
		for {
			amd64.Halt()
		}
	}
	mark("TTY_CAPABILITY_OK")
	runFilesystemSelfTests(console)

	boundedChannel = ipc.NewChannel()
	notification := ipc.Message{ProtocolID: request.ProtocolID, Version: request.Version, Kind: ipc.KindNotification}
	for index := 0; index < ipc.QueueCapacity; index++ {
		if boundedChannel.Send(0, notification, nil, nil, nil) != ipc.StatusOK {
			console.WriteString("PANIC: IPC queue filled early\n")
			for {
				amd64.Halt()
			}
		}
	}
	if boundedChannel.Send(0, notification, nil, nil, nil) != ipc.StatusResourceExhausted || !boundedChannel.Close(0) {
		console.WriteString("PANIC: IPC bounds or close failed\n")
		for {
			amd64.Halt()
		}
	}
	for index := 0; index < ipc.QueueCapacity; index++ {
		if _, status := boundedChannel.Receive(1); status != ipc.StatusOK {
			console.WriteString("PANIC: IPC queue drain failed\n")
			for {
				amd64.Halt()
			}
		}
	}
	if _, status := boundedChannel.Receive(1); status != ipc.StatusPeerDied {
		console.WriteString("PANIC: IPC peer death missing\n")
		for {
			amd64.Halt()
		}
	}
	mark("IPC_BOUNDED_OK")
	mark("IPC_PEER_DEATH_OK")

	sharedObject, sharedCreated := sharedObjects.Create(&pageAllocator, vm.PhysicalPage, zeroPhysicalPage, 2, 0)
	sharedHandleA, sharedGranted := sharedObject.Grant(processA.Handles(), capability.Read|capability.Write|capability.Transfer)
	sharedHandleB, sharedTransferred := processA.Handles().Transfer(processB.Handles(), sharedHandleA, handle.TypeMemory, capability.Read)
	// Process A prints through a console capability like every other process;
	// the identity-authorized DebugWrite it used to exercise is retired, and
	// its program proves the retired call is refused (ADR 0094).
	consoleDeviceA, consoleGrantedA := processA.Handles().Insert(handle.TypeDevice, capability.New(device.ConsoleObject, capability.Write))
	if !consoleGrantedA || consoleDeviceA != handle.ID(0x0000000100000004) {
		panicSerial("self-test console capability setup failed")
	}
	const sharedAddress = uint64(0x40010000)
	_, sharedMappedA := sharedObject.Map(&pageAllocator, processA.Handles(), sharedHandleA, rootA, sharedAddress, 0, 2, true)
	_, sharedMappedB := sharedObject.Map(&pageAllocator, processB.Handles(), sharedHandleB, rootB, sharedAddress, 0, 2, false)
	sharedInitiallyZero := [8]byte{}
	sharedZeroRead := vm.CopyIn(rootB, sharedAddress, sharedInitiallyZero[:])
	sharedPayload := []byte("SHAREDOK")
	sharedReadback := [8]byte{}
	sharedWriteAddress := sharedAddress + vm.PageSize - 4
	sharedWritten := vm.CopyOut(rootA, sharedWriteAddress, sharedPayload)
	sharedRead := vm.CopyIn(rootB, sharedWriteAddress, sharedReadback[:])
	sharedPhysicalA0, sharedResolvedA0 := vm.Resolve(rootA, sharedAddress)
	sharedPhysicalB0, sharedResolvedB0 := vm.Resolve(rootB, sharedAddress)
	sharedPhysicalA1, sharedResolvedA1 := vm.Resolve(rootA, sharedAddress+vm.PageSize)
	sharedPhysicalB1, sharedResolvedB1 := vm.Resolve(rootB, sharedAddress+vm.PageSize)
	if !sharedCreated || !sharedGranted || !sharedTransferred || !sharedMappedA || !sharedMappedB || !sharedZeroRead || sharedInitiallyZero != [8]byte{} ||
		mapped(sharedObject.Map(&pageAllocator, processA.Handles(), memoryHandle, rootA, sharedAddress+4*vm.PageSize, 0, 1, false)) ||
		mapped(sharedObject.Map(&pageAllocator, processA.Handles(), sharedHandleA, rootA, sharedAddress+4*vm.PageSize, 2, 1, false)) || !sharedWritten || !sharedRead ||
		string(sharedReadback[:]) != "SHAREDOK" || vm.CopyOut(rootB, sharedAddress, []byte{1}) ||
		!sharedResolvedA0 || !sharedResolvedB0 || !sharedResolvedA1 || !sharedResolvedB1 ||
		sharedPhysicalA0 != sharedPhysicalB0 || sharedPhysicalA1 != sharedPhysicalB1 {
		console.WriteString("PANIC: shared-memory isolation failed\n")
		for {
			amd64.Halt()
		}
	}
	mark("SHMEM_OBJECT_OK")
	mark("SHMEM_ZERO_COPY_OK")
	mark("SHMEM_RIGHTS_OK")

	probe := amd64.ProbeAddressSpaces(rootA, rootB, pageTable, vm.UserProbeAddress)
	if byte(probe) != 0xa1 || byte(probe>>8) != 0xb2 {
		console.WriteString("PANIC: address-space isolation failed\n")
		for {
			amd64.Halt()
		}
	}
	mark("ADDRESS_SPACE_A_OK")
	mark("ADDRESS_SPACE_B_OK")
	mark("PROCESS_ISOLATION_OK")
	userProgram := [...]byte{
		0xb8, 0x05, 0x00, 0x00, 0x00, // mov eax, 5: IPCWaitReceive.
		0x48, 0xbf, 0x02, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, // mov rdi, channelA.
		0x48, 0xbe, 0x44, 0x40, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, // mov rsi, selfTestUserDataAddress+68.
		0xba, 0x18, 0x00, 0x00, 0x00, // mov edx, descriptor size: time out after the peer's shorter wait.
		0xcd, 0x80,
		0xb8, 0xff, 0xff, 0xff, 0x7f, // mov eax, 0x7fffffff: wrong-ABI Linux entry.
		0x0f, 0x05, // syscall.
		0x48, 0x83, 0xf8, 0xff, // cmp rax, -EPERM.
		0x74, 0x02,
		0x0f, 0x0b,
		0xb9, 0x00, 0x00, 0x00, 0x01, // mov ecx, 0x01000000: remain preemptible.
		0xff, 0xc9, // dec ecx.
		0x75, 0xfc, // jnz dec ecx.
		0x48, 0xbf, 0x00, 0x40, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, // mov rdi, selfTestUserDataAddress.
		0xbe, 0x14, 0x00, 0x00, 0x00, // mov esi, 20.
		0x41, 0xbc, 0xd0, 0x07, 0x00, 0x00, // mov r12d, 2000: entry-stack regression iterations.
		0xb8, 0x02, 0x00, 0x00, 0x00, // mov eax, 2: the retired DebugWrite.
		0xcd, 0x80,
		0x83, 0xf8, 0x08, // cmp eax, Unsupported: no identity-authorized console path.
		0x74, 0x02,
		0x0f, 0x0b,
		0x41, 0xff, 0xcc, // dec r12d.
		0x75, 0xed, // jnz to mov eax.
		0xb8, 0x06, 0x00, 0x00, 0x00, // mov eax, 6: ConsoleWrite.
		0x48, 0xbf, 0x04, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, // mov rdi, console capability.
		0x48, 0xbe, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // mov rsi, 0: invalid pointer.
		0xba, 0x01, 0x00, 0x00, 0x00, // mov edx, 1.
		0xcd, 0x80,
		0x83, 0xf8, 0x01, // cmp eax, InvalidArgument.
		0x74, 0x02, // je valid write.
		0x0f, 0x0b,
		0xb8, 0x06, 0x00, 0x00, 0x00, // mov eax, 6: ConsoleWrite.
		0x48, 0xbf, 0x04, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, // mov rdi, console capability.
		0x48, 0xbe, 0x00, 0x40, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, // mov rsi, selfTestUserDataAddress.
		0xba, 0x14, 0x00, 0x00, 0x00, // mov edx, 20.
		0xcd, 0x80,
		0x85, 0xc0, // test eax, eax.
		0x74, 0x02, // je forged handle check.
		0x0f, 0x0b,
		0xb8, 0x06, 0x00, 0x00, 0x00, // mov eax, 6: ConsoleWrite.
		0x48, 0xbf, 0x04, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, // the console slot with a forged generation.
		0x48, 0xbe, 0x00, 0x40, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00,
		0xba, 0x01, 0x00, 0x00, 0x00,
		0xcd, 0x80,
		0x83, 0xf8, 0x02, // cmp eax, PermissionDenied.
		0x74, 0x02,
		0x0f, 0x0b,
		0xb8, 0x03, 0x00, 0x00, 0x00, // mov eax, 3: IPCSend.
		0x48, 0xbf, 0x02, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, // mov rdi, channelA.
		0x48, 0xbe, 0x14, 0x40, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, // mov rsi, selfTestUserDataAddress+20.
		0xba, 0x30, 0x00, 0x00, 0x00, // mov edx, TTY wire size.
		0xcd, 0x80,
		0x85, 0xc0,
		0x74, 0x02,
		0x0f, 0x0b,
		0xb8, 0x01, 0x00, 0x00, 0x00, // mov eax, 1: Exit.
		0x31, 0xff, // xor edi, edi: status 0.
		0xcd, 0x80,
		0x0f, 0x0b, // ud2: returning to exited user code is always a bug.
	}
	linuxProgram := []byte{
		0xb8, 0xff, 0xff, 0xff, 0x7f, // mov eax, unsupported syscall.
		0xfd,       // std: FMASK and CLD must keep DF out of kernel and return state.
		0x0f, 0x05, // syscall.
		0x48, 0x83, 0xf8, 0xda, // cmp rax, -ENOSYS.
		0x74, 0x02,
		0x0f, 0x0b,
		0x9c, 0x5b, // pushfq; pop rbx.
		0xf7, 0xc3, 0x00, 0x04, 0x00, 0x00, // test ebx, DF.
		0x74, 0x02,
		0x0f, 0x0b,
		0xf7, 0xc3, 0x00, 0x02, 0x00, 0x00, // test ebx, IF.
		0x75, 0x02,
		0x0f, 0x0b,
		0xb8, 0xff, 0xff, 0xff, 0x7f,
		0x0f, 0x05,
		0x48, 0x83, 0xf8, 0xda,
		0x74, 0x02,
		0x0f, 0x0b,
		0xb8, 0x01, 0x00, 0x00, 0x00, // native Exit number must be rejected.
		0xcd, 0x80,
		0x83, 0xf8, 0x02, // cmp eax, native PermissionDenied.
		0x74, 0x02,
		0x0f, 0x0b,
		0x48, 0x31, 0xe4, // xor rsp, rsp: malformed return state must retire the task.
		0xb8, 0xff, 0xff, 0xff, 0x7f,
		0x0f, 0x05,
		0x0f, 0x0b, // ud2: an invalid return frame must never return.
	}
	linuxCodePage, linuxCodeAllocated := pageAllocator.Allocate()
	linuxStackPage, linuxStackAllocated := pageAllocator.Allocate()
	if !linuxCodeAllocated || !linuxStackAllocated {
		panicSerial("Linux SYSCALL self-test allocation failed")
	}
	linuxCodeReady := linuxCodeAllocated && vm.InitializePhysical(linuxCodePage, linuxProgram)
	linuxStackReady := linuxStackAllocated && vm.InitializePhysical(linuxStackPage, nil)
	linuxEntry := uint64(0x00400000)
	linuxStack := uint64(0x00501000)
	_, linuxCodeMapped := vm.MapUserPageAt(&pageAllocator, rootLinux, linuxEntry, linuxCodePage, vm.Permissions{User: true, Executable: true}, true)
	_, linuxStackMapped := vm.MapUserPageAt(&pageAllocator, rootLinux, linuxStack-vm.PageSize, linuxStackPage, vm.Permissions{User: true, Writable: true}, true)
	if !linuxCodeReady || !linuxStackReady || !linuxCodeMapped || !linuxStackMapped {
		panicSerial("Linux SYSCALL self-test mapping failed")
	}
	selfTestRootLinux, selfTestLinuxEntry, selfTestLinuxStack = rootLinux, linuxEntry, linuxStack-16
	userMessage := []byte("USER_CONSOLE_CAP_OK\n")
	userData := [20 + ipc.WireHeaderSize + 16 + 24]byte{}
	copy(userData[:], userMessage)
	putUint64(userData[68:76], vm.UserStackAddress)
	putUint64(userData[76:84], ipc.WireMaxSize)
	putUint64(userData[84:92], 10)
	userNotification, userTTYEncoded := tty.Message(tty.OperationWrite, 0, []byte("TTY_SERVICE_OK\n"))
	userWireSize, userWireEncoded := ipc.Encode(userNotification, userData[len(userMessage):])
	faultProgram := []byte{
		0xb8, 0x05, 0x00, 0x00, 0x00,
		0x48, 0xbf, 0x02, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x48, 0xbe, 0x00, 0x40, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00,
		0xba, 0x18, 0x00, 0x00, 0x00,
		0xcd, 0x80,
		0x83, 0xf8, 0x06,
		0x74, 0x02,
		0x0f, 0x0b,
		0xb8, 0x05, 0x00, 0x00, 0x00,
		0x48, 0xbf, 0x02, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x48, 0xbe, 0x18, 0x40, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00,
		0xba, 0x18, 0x00, 0x00, 0x00,
		0xcd, 0x80,
		0x85, 0xc0,
		0x74, 0x02,
		0x0f, 0x0b,
		0xb8, 0x06, 0x00, 0x00, 0x00,
		0x48, 0xbf, 0x04, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x48, 0xbe, 0x21, 0x00, 0x03, 0x40, 0x00, 0x00, 0x00, 0x00, // mov rsi, UserStackAddress+0x21.
		0xba, 0x0f, 0x00, 0x00, 0x00,
		0xcd, 0x80,
		0x85, 0xc0,
		0x74, 0x02,
		0x0f, 0x0b,
		0x48, 0xb8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // movabs rax, high entry trampoline.
		0x48, 0x8b, 0x00, // mov rax, [rax]: supervisor mapping must fault.
		0x0f, 0x0b,
	}
	putUint64(faultProgram[len(faultProgram)-13:len(faultProgram)-5], amd64.EntryProbeAddress())
	faultData := [48]byte{}
	putUint64(faultData[0:8], vm.UserStackAddress)
	putUint64(faultData[8:16], ipc.WireMaxSize)
	putUint64(faultData[16:24], 5)
	putUint64(faultData[24:32], vm.UserStackAddress)
	putUint64(faultData[32:40], ipc.WireMaxSize)
	putUint64(faultData[40:48], 1000)
	nativeBytes, nativeEncoded := loader.EncodeNative(nativeImageData[:], loader.ImageSource{
		Entry:    selfTestUserCodeAddress,
		StackTop: vm.UserStackAddress + vm.PageSize - 16,
		Segments: []loader.SegmentSource{
			{VirtualAddress: selfTestUserCodeAddress, MemorySize: uint32(len(userProgram)), Flags: loader.SegmentReadable | loader.SegmentExecutable, Data: userProgram[:]},
			{VirtualAddress: vm.UserStackAddress, MemorySize: uint32(vm.PageSize), Flags: loader.SegmentReadable | loader.SegmentWritable},
			{VirtualAddress: selfTestUserDataAddress, MemorySize: uint32(len(userData)), Flags: loader.SegmentReadable, Data: userData[:]},
		},
	})
	faultBytes, faultEncoded := loader.EncodeNative(faultImageData[:], loader.ImageSource{
		Entry:    selfTestUserCodeAddress,
		StackTop: vm.UserStackAddress + vm.PageSize - 16,
		Segments: []loader.SegmentSource{
			{VirtualAddress: selfTestUserCodeAddress, MemorySize: uint32(len(faultProgram)), Flags: loader.SegmentReadable | loader.SegmentExecutable, Data: faultProgram},
			{VirtualAddress: vm.UserStackAddress, MemorySize: uint32(vm.PageSize), Flags: loader.SegmentReadable | loader.SegmentWritable},
			{VirtualAddress: selfTestUserDataAddress, MemorySize: uint32(len(faultData)), Flags: loader.SegmentReadable, Data: faultData[:]},
		},
	})
	loadedA, loadedImageA := loader.LoadNative(&pageAllocator, rootA, nativeBytes)
	loadedB, loadedImageB := loader.LoadNative(&pageAllocator, rootB, faultBytes)
	if !userTTYEncoded || !userWireEncoded || userWireSize != ipc.WireHeaderSize+16 || !nativeEncoded || !faultEncoded || !loadedImageA || !loadedImageB {
		console.WriteString("PANIC: native user image setup failed\n")
		for {
			amd64.Halt()
		}
	}
	mark("USER_RX_OK")
	mark("USER_STACK_NX_OK")
	copyOutProbe := []byte{0x5a}
	if vm.CopyOut(rootA, selfTestUserDataAddress, copyOutProbe) || !vm.CopyOut(rootA, vm.UserStackAddress, copyOutProbe) {
		console.WriteString("PANIC: user copyout validation failed\n")
		for {
			amd64.Halt()
		}
	}
	mark("USER_COPYOUT_OK")
	checkSharedImagePages(faultBytes)
	mark("KCALL_ABI_OK")
	mark("NATIVE_IMAGE_LOADER_OK")
	selfTestLoadedA, selfTestLoadedB = loadedA, loadedB
}

func startSelfTestTasks() {
	checkThreadCreationRollback()
	userTaskID, userTaskAdded := amd64.AddUserTask(&pageAllocator, selfTestRootA, selfTestLoadedA.Entry, selfTestLoadedA.StackTop)
	faultTaskID, faultTaskAdded := amd64.AddUserTask(&pageAllocator, selfTestRootB, selfTestLoadedB.Entry, selfTestLoadedB.StackTop)
	linuxTaskID, linuxTaskAdded := amd64.AddUserTask(&pageAllocator, selfTestRootLinux, selfTestLinuxEntry, selfTestLinuxStack)
	if !userTaskAdded {
		panicSerial("self-test user task installation failed")
	}
	if !faultTaskAdded {
		panicSerial("self-test fault task installation failed")
	}
	if !linuxTaskAdded {
		panicSerial("Linux SYSCALL self-test task installation failed")
	}
	userSlot, _, validTask := sched.TaskID(userTaskID).Parts()
	stackTop := amd64.TaskStackTop(uint64(userSlot))
	stackPermissions, stackUser, stackMapped := vm.MappingPermissions(selfTestRootA, stackTop-1)
	guardLow, guardHigh, guardFound := amd64.KernelStackGuard(stackTop - 1)
	if !validTask || stackTop < vm.KernelLowBase || !stackMapped || stackUser || !stackPermissions.Writable || stackPermissions.Executable ||
		!guardFound || guardHigh == 0 || guardLow >= guardHigh || vm.IsMapped(selfTestRootA, guardHigh-vm.PageSize) || vm.IsMapped(kernelPageTable, guardHigh-vm.PageSize) {
		panicSerial("self-test kernel-entry stack mapping invalid")
	}
	if !processRegistry.BindTask(2, sched.TaskID(userTaskID)) || !processRegistry.BindTask(3, sched.TaskID(faultTaskID)) || !processRegistry.BindTask(selfTestLinuxProcessID, sched.TaskID(linuxTaskID)) ||
		!amd64.StartTask(sched.TaskID(userTaskID)) || !amd64.StartTask(sched.TaskID(faultTaskID)) || !amd64.StartTask(sched.TaskID(linuxTaskID)) {
		panicSerial("self-test task binding failed")
	}
}

func awaitValidationBoot() {
	console := kernelconsole.New()
	for userExited == 0 || amd64.UserFaults() == 0 || highKernelAccessFaults == 0 || initAnnounced == 0 || processExitDelivered == 0 || supervisorExitDelivered == 0 || serviceLaunchConfirmed < supervisor.MaxRestarts || stackGuardDeepFaults == 0 || userspaceSupervisorOK == 0 || capabilityManifestOK == 0 || linuxSyscallEntries.Load() < 2 || linuxSyscallABIRejections.Load() == 0 || linuxSyscallFrameRejections.Load() == 0 || nativeCallABIRejections.Load() == 0 {
		serviceKernel()
		if initSendObserved != 0 && initSendStatus != kcall.StatusOK {
			console.WriteString("PANIC: init launch IPC failed: ")
			console.WriteHex(uint64(initSendStatus))
			console.WriteString("\n")
			for {
				amd64.Halt()
			}
		}
		if initExitObserved != 0 {
			console.WriteString("PANIC: init process exited: ")
			console.WriteHex(initExitCode)
			console.WriteString("\n")
			for {
				amd64.Halt()
			}
		}
	}
	mark("INITRAMFS_DISCOVERY_OK")
	mark("PID1_INIT_OK")
	mark("PROCESS_EXIT_EVENT_OK")
	if processReclaimConfirmed == 0 {
		console.WriteString("PANIC: process resources were not reclaimed\n")
		for {
			amd64.Halt()
		}
	}
	mark("PROCESS_RECLAIM_OK")
	if processWaitConfirmed == 0 {
		console.WriteString("PANIC: process wait record missing\n")
		for {
			amd64.Halt()
		}
	}
	mark("PROCESS_WAIT_OK")
	mark("SUPERVISOR_CRASH_EVENT_OK")
	mark("LIFECYCLE_IPC_OK")
	mark("INIT_EXECUTABLE_LOOKUP_OK")
	mark("INIT_CAPABILITY_MANIFEST_OK")
	if startupABIConfirmed == 0 {
		console.WriteString("PANIC: native startup ABI not exercised\n")
		for {
			amd64.Halt()
		}
	}
	mark("NATIVE_STARTUP_ABI_OK")
	// The supervision scenario above relaunches the same image, so its code
	// must by now have been mapped from the shared cache at least once.
	if imagePages.Hits() == 0 {
		console.WriteString("PANIC: image pages were not shared\n")
		for {
			amd64.Halt()
		}
	}
	mark("IMAGE_PAGES_SHARED_OK")
	mark("PID1_SERVICE_LAUNCH_OK")
	if amd64.UserPreemptions() == 0 {
		console.WriteString("PANIC: userspace task was not preempted\n")
		for {
			amd64.Halt()
		}
	}
	mark("USER_PREEMPT_OK")
	mark("PROCESS_CR3_SCHED_OK")
	mark("USER_FAULT_ISOLATION_OK")
	mark("KERNEL_HIGH_ISOLATION_OK")
	if amd64.UserCallEntryCount() < 2000 || amd64.UserCallEntryDrift() != 0 {
		console.WriteString("PANIC: user entry stack pointer drifted\n")
		for {
			amd64.Halt()
		}
	}
	mark("USER_ENTRY_STACK_BOUNDED_OK")
	mark("LINUX_SYSCALL_OK")
	mark("LINUX_ABI_ISOLATION_OK")
	mark("LINUX_RETURN_FRAME_GUARD_OK")
	mark("USER_LOW_ADDRESS_EXEC_OK")
	mark("USER_STACK_GUARD_OK")
	mark("USER_RING3_OK")
	mark("NATIVE_PROCESS_OK")
	mark("TTY_RIGHTS_OK")
	console.WriteString("Kernel initialized\n")
	mark("BOOT_OK")
}

// pageFaultExit is the exit code of a task retired by a user page fault: the
// fault bit and vector 14.
const (
	pageFaultExit           = uint64(1)<<63 | 14
	selfTestUserCodeAddress = vm.UserProbeAddress + 2*vm.PageSize
	selfTestUserDataAddress = vm.UserProbeAddress + 4*vm.PageSize
)

// stackGuardFaults counts fault-probe runs that overflowed their stack into
// the unmapped gap below it. stackGuardDeepFaults counts those among them that
// faulted more than a page below the stack, where ADR 0083's single guard page
// ended: the probe allocates a 12 KiB frame, so its overflow proves that a
// frame larger than a page still lands in the gap (ADR 0083, ADR 0088).
var stackGuardFaults, stackGuardDeepFaults, highKernelAccessFaults uint64

// noteStackGuardFault counts an exit as a stack guard fault when the task died
// of a page fault anywhere in the gap below the probe layout's stack and the
// faulting page is unmapped in its address space. The process is still
// registered, so its address space can be inspected before teardown.
func noteStackGuardFault(taskID sched.TaskID, code uint64) {
	slot, _, valid := taskID.Parts()
	if !valid {
		return
	}
	address := amd64.TakeTaskFaultAddress(uint64(slot))
	owner, found := processRegistry.ByTask(taskID)
	if found && owner.ID() == 3 && code == pageFaultExit {
		probe := amd64.EntryProbeAddress()
		if address&^(vm.PageSize-1) == probe&^(vm.PageSize-1) {
			highKernelAccessFaults++
			mark("KERNEL_HIGH_ACCESS_FAULT_OK")
		}
	}
	if !found || code != pageFaultExit || address < vm.UserStackAddress-loader.StackGuardSize || address >= vm.UserStackAddress {
		return
	}
	if _, mapped := vm.Resolve(owner.AddressSpace().Root, address&^(vm.PageSize-1)); mapped {
		return
	}
	stackGuardFaults++
	if address < vm.UserStackAddress-vm.PageSize {
		stackGuardDeepFaults++
	}
}

// selfTestShared is a private cache, so the check below neither depends on nor
// disturbs the one launches use.
var selfTestShared loader.SharedPages

// checkSharedImagePages loads one image into two address spaces through a
// shared cache (ADR 0081) and requires that the code is one frame mapped by
// both, that the stack is not, that neither process can have the kernel write
// into the shared frame, and that tearing one process down leaves the frame
// allocated and mapped for the other.
func checkSharedImagePages(image []byte) {
	first, firstOK := vm.CreateUserAddressSpace(&pageAllocator, kernelPageTable)
	second, secondOK := vm.CreateUserAddressSpace(&pageAllocator, kernelPageTable)
	if !firstOK || !secondOK {
		panicSerial("shared image address spaces failed")
	}
	_, firstLoaded := loader.LoadImage(&pageAllocator, first, loader.Contiguous(image), &selfTestShared, loader.Origin{Key: 1}, nil)
	_, secondLoaded := loader.LoadImage(&pageAllocator, second, loader.Contiguous(image), &selfTestShared, loader.Origin{Key: 1}, nil)
	firstCode, firstCodeOK := vm.ResolveUser(first, selfTestUserCodeAddress)
	secondCode, secondCodeOK := vm.ResolveUser(second, selfTestUserCodeAddress)
	firstStack, _ := vm.ResolveUser(first, vm.UserStackAddress)
	secondStack, _ := vm.ResolveUser(second, vm.UserStackAddress)
	if !firstLoaded || !secondLoaded || !firstCodeOK || !secondCodeOK || firstCode != secondCode || firstStack == secondStack || selfTestShared.Hits() != 2 {
		panicSerial("image code was not shared, or its stack was")
	}
	probe := []byte{0xcc}
	if vm.CopyOut(first, selfTestUserCodeAddress, probe) || vm.CanCopyOut(second, selfTestUserCodeAddress, 1) || vm.CopyOut(second, selfTestUserDataAddress, probe) {
		panicSerial("a shared image page accepted a kernel write")
	}
	if !vm.DestroyUserAddressSpace(&pageAllocator, first) || !pageAllocator.IsAllocated(secondCode) {
		panicSerial("teardown released a shared image page")
	}
	if code, ok := vm.ResolveUser(second, selfTestUserCodeAddress); !ok || code != secondCode || !vm.DestroyUserAddressSpace(&pageAllocator, second) {
		panicSerial("teardown disturbed the other process's code")
	}
	mark("IMAGE_PAGES_ISOLATED_OK")
}

// highMemoryProbe is the buffer the high-memory check copies through.
var highMemoryProbe [16]byte

// checkHighMemory exercises a page above the identity map when the machine
// has one (ADR 0103): the allocator hands it out first, the kernel reaches it
// only through the direct map, and a process maps it like any other page.
func checkHighMemory(pageTable uint64) {
	before := pageAllocator.Statistics()
	if before.HighPages == 0 {
		mark("PMM_HIGH_NONE")
		return
	}
	physical, allocated := pageAllocator.Allocate()
	if !allocated || physical < pmm.LowLimit || !vm.InitializePhysicalByte(physical, 0x5a) {
		panicSerial("high page allocation failed")
	}
	page, viewed := vm.PhysicalPage(physical)
	if !viewed || page[0] != 0x5a || page[1] != 0 || page[vm.PageSize-1] != 0 {
		panicSerial("high page was not initialized through the direct map")
	}
	for index := range page {
		page[index] = byte(index * 7)
	}
	root, created := vm.CreateUserAddressSpace(&pageAllocator, pageTable)
	_, mapped := vm.MapUserPageAt(&pageAllocator, root, vm.UserProbeAddress, physical, vm.Permissions{User: true, Writable: true}, false)
	mapped = created && mapped
	tail := vm.UserProbeAddress + vm.PageSize - uint64(len(highMemoryProbe))
	if !mapped || !vm.CopyIn(root, tail, highMemoryProbe[:]) {
		panicSerial("high page could not be read through a process mapping")
	}
	for index, value := range highMemoryProbe {
		if value != byte((int(vm.PageSize)-len(highMemoryProbe)+index)*7) {
			panicSerial("high page read back wrong bytes")
		}
	}
	for index := range highMemoryProbe {
		highMemoryProbe[index] = byte(0xa0 + index)
	}
	if !vm.CopyOut(root, vm.UserProbeAddress+0x100, highMemoryProbe[:]) || page[0x100] != 0xa0 || page[0x10f] != 0xaf {
		panicSerial("high page write through a process mapping was lost")
	}
	if !vm.DestroyUserAddressSpace(&pageAllocator, root) || !pageAllocator.IsAllocated(physical) || !pageAllocator.Free(physical) {
		panicSerial("high page release failed")
	}
	if after := pageAllocator.Statistics(); after.FreePages != before.FreePages {
		panicSerial("high memory check leaked pages")
	}
	console := kernelconsole.New()
	console.WriteString("PMM high page: ")
	console.WriteHex(physical)
	console.WriteString("\n")
	mark("PMM_HIGH_OK")
}

// mapped is the success of a shared-memory mapping, for checks that expect a
// refusal and so never charge the tables it reports.
func mapped(_ int, ok bool) bool { return ok }
