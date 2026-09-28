//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/internal/logfmt"
	"goos/internal/timekeeping"
	"goos/internal/validation"
	"goos/internal/version"
	"goos/kernel/acpi"
	"goos/kernel/arch/amd64"
	"goos/kernel/boot"
	bootuefi "goos/kernel/boot/uefi"
	"goos/kernel/clock"
	kernelconsole "goos/kernel/console"
	"goos/kernel/device"
	"goos/kernel/executable"
	kernelframebuffer "goos/kernel/framebuffer"
	"goos/kernel/handle"
	"goos/kernel/initramfs"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/loader"
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
	"goos/kernel/pci"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/kernel/shmem"
	"goos/kernel/stream"
	kernelwait "goos/kernel/wait"
	execproto "goos/userspace/exec"
	"goos/userspace/fsproto"
	supervisor "goos/userspace/init"
)

var (
	bootInfo        boot.Info
	pageAllocator   pmm.Allocator
	demoTaskA       uint64
	demoTaskB       uint64
	demoABlocked    uint64
	demoAResumed    uint64
	userExited      uint64
	kernelCallData  [kcall.MaxConsoleWrite]byte
	nativeImageData [1024]byte
	faultImageData  [1024]byte
	boundedChannel  ipc.Channel
	ipcWireData     [ipc.WireMaxSize]byte
	sharedObjects   shmem.Table
	bootFileCache   *shmem.Object
	dispatchHandler nativeCallHandler
	// Native calls run one at a time in the kernel exclusion domain, so the
	// call paths share these buffers instead of allocating on a leaking heap.
	handleInfoWire        [kcall.HandleInfoSize]byte
	processInfoWire       [kcall.ProcessInfoSize]byte
	clockWire             [8]byte
	nanosWire             [8]byte
	timeWire              [kcall.ClockTimeSize]byte
	consoleByteWire       [1]byte
	systemInfoWire        [kcall.SystemInfoSize]byte
	portWire              [4]byte
	dmaWire               [24]byte
	channelWire           [16]byte
	streamCreateWire      [16]byte
	streamDescriptorWire  [24]byte
	streamResultWire      [8]byte
	receiveDescriptorWire [24]byte
	exitWire              [8]byte
	waitAnyResultWire     [kcall.WaitAnyResultSize]byte
	waitMaskWire          [8]byte
	launchTemplate        [supervisor.StagingSlotSize]byte
	channelRegistry       ipc.Registry
	streamRegistry        stream.Registry
	streamCallData        [kcall.MaxStreamIO]byte
	lifecycleChannel      *ipc.Channel
	lifecycleChannelID    uint64
	vfsChannelID          uint64
	bootProcessA          *process.Process
	bootProcessB          *process.Process
	bootInitProcess       *process.Process
	processRegistry       process.Registry
	ipcWaitSet            ipc.WaitSet
	ipcWakeResults        [ipc.MaxWaiters]ipc.WaitResult
	ipcWaitStatus         [sched.MaxTasks]kcall.Status
	processWaitSet        process.WaitSet
	processWakeResults    [process.MaxWaiters]process.WaitResult
	processWaitStatus     [sched.MaxTasks]kcall.Status
	streamWaitSet         ipc.WaitSet
	streamWakeResults     [ipc.MaxWaiters]ipc.WaitResult
	streamWaitStatus      [sched.MaxTasks]kcall.Status
	waitAnyTable          kernelwait.Table
	waitAnyCompletions    [sched.MaxTasks]kernelwait.Completion
	waitAnyEntries        [kernelwait.MaxObjects]kernelwait.Entry
	waitAnyWire           [kernelwait.MaxObjects * int(kcall.WaitAnyInterestSize)]byte
	// imagePages holds the read-only pages of system images that every
	// process running them maps (ADR 0081).
	imagePages              loader.SharedPages
	pendingLifecycle        [maxPendingLifecycle]ipc.Message
	pendingLifecycleHead    uint8
	pendingLifecycleCount   uint8
	lifecycleOverflows      uint64
	initAnnounced           uint64
	processExitDelivered    uint64
	processReclaimConfirmed uint64
	// Teardown steps that may fail are counted rather than allowed to abort
	// delivery, and reported once the boot sequence has settled.
	taskStackReclaimFailures    uint64
	addressSpaceReclaimFailures uint64
	processWaitConfirmed        uint64
	supervisorExitDelivered     uint64
	serviceLaunchConfirmed      uint64
	userspaceSupervisorOK       uint64
	capabilityManifestOK        uint64
	startupABIConfirmed         uint64
	initSendObserved            uint64
	initSendStatus              kcall.Status
	initExitObserved            uint64
	initExitCode                uint64
	kernelPageTable             uint64
	framebufferVirtual          uint64
	graphicalFramebuffer        kernelframebuffer.Buffer
	initName                    = []byte("init")
	initImagePath               = []byte("system/init")
	initProcessSlot             process.Process
	spawnedProcesses            [process.MaxProcesses]*process.Process
	nextProcessID               process.ID = 4
	virtioBlockPort             uint16
	bootRealtime                timekeeping.Time
	bootRealtimeCounter         uint64
)

// userExitAnnounced records that the validation marker for Exit was printed.
var userExitAnnounced uint64

type nativeCallHandler struct {
	root uint64
}

type pciConfig struct{}

func (pciConfig) Read32(bus, slot, function, offset uint8) uint32 {
	return amd64.PCIConfigRead32(bus, slot, function, offset)
}

func equalBytes(value []byte, expected string) bool {
	if len(value) != len(expected) {
		return false
	}
	for index := range value {
		if value[index] != expected[index] {
			return false
		}
	}
	return true
}

func decodeUint64(data []byte) uint64 {
	return uint64(data[0]) | uint64(data[1])<<8 | uint64(data[2])<<16 | uint64(data[3])<<24 |
		uint64(data[4])<<32 | uint64(data[5])<<40 | uint64(data[6])<<48 | uint64(data[7])<<56
}

func putUint64(data []byte, value uint64) {
	for index := 0; index < 8; index++ {
		data[index] = byte(value >> uint(index*8))
	}
}

func putUint32(data []byte, value uint32) {
	data[0] = byte(value)
	data[1] = byte(value >> 8)
	data[2] = byte(value >> 16)
	data[3] = byte(value >> 24)
}

func panicSerial(message string) {
	kernelconsole.Panic(message)
	for {
		amd64.Halt()
	}
}

// firmwareTableLimit bounds the firmware tables the kernel reads at boot,
// before its own page tables are loaded.
const firmwareTableLimit = uint64(4 << 30)

// inLoaderMemory reports whether address lies in memory the firmware loader
// allocated for the kernel.
func inLoaderMemory(address uint64) bool {
	for _, region := range bootInfo.MemoryRegions() {
		if region.Kind == boot.MemoryLoader && address >= region.Start && address < region.End() {
			return true
		}
	}
	return false
}

var consoleOverrunReported bool

func main() {
	console := kernelconsole.New()
	console.Init()
	console.WriteString(version.Name + " " + version.Release + "\n\n")
	console.WriteString("CPU: " + version.Machine + "\n")
	calendar, status := bootuefi.CalendarTime()
	bootRealtime, bootRealtimeCounter = calendar, amd64.ReadTSC()
	if status != 0 {
		console.WriteString("PANIC: UEFI calendar time invalid: ")
		console.WriteHex(uint64(status))
		console.WriteString("\n")
		for {
			amd64.Halt()
		}
	}

	status = bootuefi.LoadInitramfs(&bootInfo)
	if status == bootuefi.InitramfsTooLarge {
		console.WriteString("PANIC: initramfs is larger than the 4 MiB the loader reads\n")
		for {
			amd64.Halt()
		}
	}
	if status != 0 {
		console.WriteString("PANIC: initramfs load failed: ")
		console.WriteHex(uint64(status))
		console.WriteString("\n")
		for {
			amd64.Halt()
		}
	}
	status = bootuefi.DiscoverFramebuffer(&bootInfo)
	if status != 0 {
		console.WriteString("Graphics: GOP unavailable, using serial console: ")
		console.WriteHex(uint64(status))
		console.WriteString("\n")
	} else {
		console.WriteString("Graphics: GOP ")
		console.WriteUint(uint64(bootInfo.Framebuffer.Width))
		console.WriteString("x")
		console.WriteUint(uint64(bootInfo.Framebuffer.Height))
		console.WriteString("x32\n")
	}
	bootuefi.FindACPIRoot(&bootInfo)
	status = bootuefi.CaptureAndExit(&bootInfo)
	if status != 0 {
		console.WriteString("PANIC: UEFI handoff failed: ")
		console.WriteHex(uint64(status))
		console.WriteString("\n")
		for {
			amd64.Halt()
		}
	}
	mark("BOOT_SERVICES_EXITED")
	if bootInfo.Framebuffer.Available && bootInfo.Framebuffer.OverlapsUsable(bootInfo.MemoryRegions()) {
		console.WriteString("Graphics: framebuffer overlaps usable RAM, using serial console\n")
		bootInfo.Framebuffer = boot.FramebufferInfo{}
	}
	// ACPI says how to switch the machine off. The tables are read once, here,
	// as untrusted firmware input; a machine without them simply cannot power
	// off (ADR 0092). The firmware's page tables are still loaded and
	// identity-map all memory; firmware keeps its tables below 4 GiB.
	systemPowerOff, systemPowerOffFound = acpi.FindPowerOff(bootuefi.PhysicalMemory{Limit: firmwareTableLimit}, bootInfo.ACPIRoot)
	if systemPowerOffFound {
		mark("ACPI_POWEROFF_OK")
	}
	virtioBlock, virtioBlockFound := pci.FindLegacyVirtioBlock(pciConfig{})
	if !virtioBlockFound || virtioBlock.IRQ != 11 {
		console.WriteString("PANIC: legacy virtio-blk PCI device not found\n")
		for {
			amd64.Halt()
		}
	}
	command := uint16(amd64.PCIConfigRead32(virtioBlock.Bus, virtioBlock.Slot, virtioBlock.Function, 4))
	command = command&^uint16(1<<10) | 1<<0 | 1<<2
	amd64.PCIConfigWrite16(virtioBlock.Bus, virtioBlock.Slot, virtioBlock.Function, 4, command)
	virtioBlockPort = virtioBlock.IOBase
	mark("PCI_DISCOVERY_OK")

	// The allocator's bitmap is carved out of low memory and reached through
	// the firmware's identity map until the kernel's own map replaces it.
	if !pageAllocator.Init(bootInfo.MemoryRegions(), vm.PhysicalWords) {
		panicSerial("physical page allocator initialization failed")
	}
	page, ok := pageAllocator.Allocate()
	if !ok || !pageAllocator.Free(page) {
		console.WriteString("PANIC: physical page allocator self-test failed\n")
		for {
			amd64.Halt()
		}
	}
	memory := pageAllocator.Statistics()
	console.WriteString("Memory: ")
	console.WriteUint(memory.ManagedPages * pmm.PageSize / (1024 * 1024))
	console.WriteString(" MiB managed\n")
	if memory.UnmanagedPages != 0 {
		console.WriteString("Memory: ")
		console.WriteUint(memory.UnmanagedPages * pmm.PageSize / (1024 * 1024))
		console.WriteString(" MiB usable but unmanaged\n")
	}
	if validation.Enabled {
		console.WriteString("Physical pages free: ")
		console.WriteUint(memory.FreePages)
		console.WriteString("\nPhysical pages above 1 GiB: ")
		console.WriteUint(memory.HighPages)
		console.WriteString("\n")
	}
	mark("PMM_OK")

	if !amd64.HasNX() {
		console.WriteString("PANIC: CPU does not support NX\n")
		for {
			amd64.Halt()
		}
	}
	image, validImage := vm.LoadedImage()
	// main still runs on the stack TinyGo took from the loader's memory. With
	// more than a GiB of RAM the firmware places it above the identity map,
	// where MapLoaderMemory keeps it reachable.
	stack := amd64.StackPointer()
	if !validImage || stack < 1024*1024 || (stack >= vm.IdentityMapLimit && !inLoaderMemory(stack)) {
		console.WriteString("PANIC: unsafe virtual-memory bootstrap state\n")
		for {
			amd64.Halt()
		}
	}
	pageTable, built := vm.BuildIdentity(&pageAllocator, image)
	if built {
		built = vm.MapLoaderMemory(&pageAllocator, pageTable, bootInfo.MemoryRegions())
	}
	if built && bootInfo.Framebuffer.Available && !amd64.HasPAT() {
		console.WriteString("Graphics: CPU has no PAT, using serial console\n")
		bootInfo.Framebuffer = boot.FramebufferInfo{}
	}
	if built && bootInfo.Framebuffer.Available {
		if !amd64.ConfigurePAT() {
			console.WriteString("Graphics: PAT configuration failed, using serial console\n")
			bootInfo.Framebuffer = boot.FramebufferInfo{}
		} else {
			mark("PAT_CONFIGURED")
		}
	}
	if built && bootInfo.Framebuffer.Available {
		var mapped bool
		framebufferVirtual, mapped = vm.MapFramebuffer(&pageAllocator, pageTable, bootInfo.Framebuffer)
		if !mapped {
			console.WriteString("Graphics: framebuffer mapping failed, using serial console\n")
			framebufferVirtual = 0
			bootInfo.Framebuffer = boot.FramebufferInfo{}
		}
	}
	// The kernel's copies into user memory fault in demand-zero pages the
	// way the processor's accesses do (ADR 0104).
	vm.SetFaultResolver(resolveCopyFault)
	// Registry entries take their pages as they are created (ADR 0111).
	channelRegistry.SetStorage(pageObjects[ipc.Channel]{})
	streamRegistry.SetStorage(pageObjects[stream.Stream]{})
	sharedObjects.SetStorage(pageObjects[shmem.Object]{})
	executables.SetStorage(pageObjects[executable.Object]{})
	if !built {
		console.WriteString("PANIC: page-table construction failed\n")
		for {
			amd64.Halt()
		}
	}
	kernelPageTable = pageTable
	amd64.EnableNXAndWriteProtect()
	amd64.LoadPageTable(pageTable)
	amd64.SetKernelPageTable(pageTable)
	if framebufferVirtual != 0 {
		physical, mapped := vm.Resolve(pageTable, framebufferVirtual)
		permissions, user, present := vm.MappingPermissions(pageTable, framebufferVirtual)
		if !mapped || physical != bootInfo.Framebuffer.Address || !present || user ||
			!permissions.Writable || permissions.Executable {
			console.WriteString("Graphics: framebuffer mapping validation failed, using serial console\n")
			framebufferVirtual = 0
			bootInfo.Framebuffer = boot.FramebufferInfo{}
		} else {
			mark("FRAMEBUFFER_MAPPING_OK")
			var ready bool
			graphicalFramebuffer, ready = kernelframebuffer.New(framebufferVirtual, bootInfo.Framebuffer)
			if !ready {
				console.WriteString("Graphics: framebuffer construction failed, using serial console\n")
				framebufferVirtual = 0
				bootInfo.Framebuffer = boot.FramebufferInfo{}
			} else {
				drawFramebufferMilestone(&graphicalFramebuffer)
				mark("FRAMEBUFFER_PRIMITIVES_OK")
				if kernelconsole.AttachFramebuffer(graphicalFramebuffer) {
					mark("FRAMEBUFFER_TERMINAL_OK")
				} else {
					console.WriteString("Graphics: terminal construction failed, using serial console\n")
					framebufferVirtual = 0
					bootInfo.Framebuffer = boot.FramebufferInfo{}
				}
			}
		}
	}
	mark("NX_WX_OK")
	mark("VM_OK")

	// The boot task leaves the goroutine stack TinyGo's runtime gave main,
	// which has no guard, for one that has (ADR 0091). Everything from here
	// on, the service loop included, runs in bootContinue on that stack.
	bootVirtioBlock = virtioBlock
	if !amd64.RunOnBootStack(&pageAllocator, pageTable) {
		console.WriteString("PANIC: boot stack allocation failed\n")
		for {
			amd64.Halt()
		}
	}
}

// bootVirtioBlock carries the PCI discovery result across the stack switch.
var bootVirtioBlock pci.Device

// bootContinue is the rest of main, on the guarded boot stack. It never
// returns.
//
//export goos_boot_continue
func bootContinue() {
	console := kernelconsole.New()
	pageTable := kernelPageTable
	virtioBlock := bootVirtioBlock
	if validation.Enabled && !amd64.BootStackGuarded(pageTable) {
		panicSerial("boot task is not on its guarded stack")
	}
	mark("BOOT_STACK_GUARD_OK")

	if !amd64.InstallDescriptorTables(&pageAllocator, pageTable) {
		console.WriteString("PANIC: descriptor-table installation failed\n")
		for {
			amd64.Halt()
		}
	}
	if !amd64.ConfigureDeviceIRQs(pageTable) {
		console.WriteString("PANIC: device IRQ routing failed\n")
		for {
			amd64.Halt()
		}
	}
	// The UART interrupt is enabled only now: its gate exists, so the first
	// byte to arrive has a handler to drain it.
	amd64.EnableConsoleInterrupt()
	if validation.Enabled {
		amd64.TriggerBreakpoint()
	}
	mark("GDT_OK")
	mark("IDT_OK")

	amd64.StartBootstrapTimer(clock.PITDivisor)
	for amd64.TimerTicks() < 3 {
		amd64.Halt()
	}
	calibrateClock()
	if !realtimeClock.Init(bootRealtime, bootRealtimeCounter) {
		panicSerial("realtime clock initialization failed")
	}
	console.WriteString("Timer: ")
	console.WriteUint(clock.TicksPerSecond)
	console.WriteString(" Hz\n")
	mark("TIMER_OK")
	console.WriteString("Security: capabilities, NX, W^X\n")

	if !amd64.SetupBootstrapScheduler(&pageAllocator, pageTable) {
		console.WriteString("PANIC: scheduler bootstrap failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("Scheduler: preemptive round-robin\n")
	if validation.Enabled {
		awaitSchedulerSelfTest()
	}

	// PID 1's own pages are counted the same way a launched process's are, so
	// the reported memory of init means what it means for everything else.
	initPagesBefore := pageAllocator.Statistics().FreePages
	rootInit, addressSpaceInit := vm.CreateUserAddressSpace(&pageAllocator, pageTable)
	if !addressSpaceInit {
		panicSerial("PID 1 address-space construction failed")
	}
	initProcess := &initProcessSlot
	validInit := process.Init(initProcess, 1, process.AddressSpace{Root: rootInit})
	initProcess.SetName(initName)
	bootInitProcess = initProcess
	if !validInit || !processRegistry.Add(initProcess) {
		panicSerial("PID 1 process initialization failed")
	}
	// PID 1 creates the endpoints every other process communicates through, so
	// its share of each table is the rest of it. This is the only place the
	// system quota is granted, and GrantSystemQuota refuses any other process.
	if !initProcess.GrantSystemQuota() {
		panicSerial("PID 1 quota grant failed")
	}
	var lifecycleCreated bool
	lifecycleChannel, lifecycleChannelID, lifecycleCreated = bootChannel()
	lifecycleObject, _ := ipc.EndpointObject(lifecycleChannelID, 1)
	lifecycleHandle, lifecycleInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(lifecycleObject, capability.Read|capability.Write|capability.Wait))
	serviceDeviceHandle, serviceDeviceInstalled := initProcess.Handles().Insert(handle.TypeDevice, capability.New(1, capability.Read|capability.Write|capability.Transfer))
	_, vfsChannelID, vfsChannelCreated := bootChannel()
	vfsClientObject, _ := ipc.EndpointObject(vfsChannelID, 0)
	vfsServerObject, _ := ipc.EndpointObject(vfsChannelID, 1)
	vfsChannelHandle, vfsChannelInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(vfsClientObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	vfsServerHandle, vfsServerInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(vfsServerObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	_, vfsControlID, vfsControlCreated := bootChannel()
	vfsControlClientObject, _ := ipc.EndpointObject(vfsControlID, 0)
	vfsControlServerObject, _ := ipc.EndpointObject(vfsControlID, 1)
	vfsControlHandle, vfsControlInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(vfsControlClientObject, capability.Read|capability.Write|capability.Wait))
	vfsControlServerHandle, vfsControlServerInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(vfsControlServerObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	_, execControlID, execControlCreated := bootChannel()
	execClientObject, _ := ipc.EndpointObject(execControlID, 0)
	execServerObject, _ := ipc.EndpointObject(execControlID, 1)
	execServerHandle, execServerInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(execServerObject, capability.Read|capability.Write|capability.Wait))
	execClientHandle, execClientInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(execClientObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	_, blockChannelID, blockChannelCreated := bootChannel()
	blockClientObject, _ := ipc.EndpointObject(blockChannelID, 0)
	blockServerObject, _ := ipc.EndpointObject(blockChannelID, 1)
	blockClientHandle, blockClientInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(blockClientObject, capability.Read|capability.Write|capability.Wait))
	blockServerHandle, blockServerInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(blockServerObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	portObject, portObjectValid := device.PortObject(virtioBlock.IOBase, virtioBlock.IOSize)
	dmaObject, dmaObjectValid := device.DMAObject(1)
	irqObject, irqObjectValid := device.IRQObject(virtioBlock.IRQ)
	virtioPortHandle, virtioPortInstalled := initProcess.Handles().Insert(handle.TypeDevice, capability.New(portObject, capability.Read|capability.Write|capability.Transfer))
	virtioIRQHandle, virtioIRQInstalled := initProcess.Handles().Insert(handle.TypeIRQ, capability.New(irqObject, capability.Wait|capability.Transfer))
	virtioDMAHandle, virtioDMAInstalled := initProcess.Handles().Insert(handle.TypeDevice, capability.New(dmaObject, capability.Write|capability.Transfer))
	_, driverChannelID, driverChannelCreated := bootChannel()
	driverClientObject, _ := ipc.EndpointObject(driverChannelID, 0)
	driverServerObject, _ := ipc.EndpointObject(driverChannelID, 1)
	driverClientHandle, driverClientInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(driverClientObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	driverServerHandle, driverServerInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(driverServerObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	_, filesystemBlockChannelID, filesystemBlockChannelCreated := bootChannel()
	filesystemBlockClientObject, _ := ipc.EndpointObject(filesystemBlockChannelID, 0)
	filesystemBlockServerObject, _ := ipc.EndpointObject(filesystemBlockChannelID, 1)
	filesystemBlockClientHandle, filesystemBlockClientInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(filesystemBlockClientObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	filesystemBlockServerHandle, filesystemBlockServerInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(filesystemBlockServerObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	systemControlHandle, systemControlInstalled := initProcess.Handles().Insert(handle.TypeDevice, capability.New(device.SystemControlObject, capability.Write))
	launchStaging, launchStagingCreated := sharedObjects.Create(&pageAllocator, vm.PhysicalPage, zeroPhysicalPage, supervisor.StagingPages, 0)
	launchStagingHandle, launchStagingGranted := launchStaging.Grant(initProcess.Handles(), capability.Read|capability.Write)
	launchStagingMapped := launchStagingCreated && launchStagingGranted && mapPinnedShared(initProcess, launchStaging, launchStagingHandle, supervisor.StagingAddress)
	execPage, execPageCreated := sharedObjects.Create(&pageAllocator, vm.PhysicalPage, zeroPhysicalPage, uint32(execproto.AreaSize/vm.PageSize), 0)
	execPageHandle, execPageGranted := execPage.Grant(initProcess.Handles(), capability.Read|capability.Write|capability.Transfer)
	// The file cache is the memory the VFS and the filesystem service share:
	// file data moves through it instead of through messages (ADR 0098). PID 1
	// hands each of them a copy of the right to map it, at every launch.
	var fileCacheCreated bool
	bootFileCache, fileCacheCreated = sharedObjects.Create(&pageAllocator, vm.PhysicalPage, zeroPhysicalPage, fsproto.CacheSize/fsproto.PageSize, 0)
	sharedRegistered := launchStagingMapped && execPageCreated && execPageGranted && fileCacheCreated
	imageObject, imageObjectValid := device.ImageObject(1)
	// PID 1 maps the image to find executables in it and launches them with
	// Execute (ADR 0093). It hands the filesystem service a Read-only copy to
	// mount at /system, which cannot be used to launch anything.
	systemImageHandle, systemImageInstalled := initProcess.Handles().Insert(handle.TypeDevice, capability.New(imageObject, capability.Read|capability.Execute|capability.Transfer))
	// Reading the log is its own capability, so PID 1 can give it to the log
	// service and the shell without giving them the console's input.
	logDeviceHandle, logDeviceInstalled := initProcess.Handles().Insert(handle.TypeDevice, capability.New(device.LogObject, capability.Read|capability.Transfer))
	// PID 1 creates sealed executable headers and binds them to VFS sources
	// (ADR 0121). The right stays with PID 1.
	loaderHandle, loaderInstalled := initProcess.Handles().Insert(handle.TypeDevice, capability.New(device.LoaderObject, capability.Write))
	fileCacheHandle, fileCacheGranted := bootFileCache.Grant(initProcess.Handles(), capability.Read|capability.Write|capability.Transfer)
	if !lifecycleCreated || !lifecycleInstalled || !serviceDeviceInstalled || !vfsChannelCreated || !vfsChannelInstalled || !vfsServerInstalled || !vfsControlCreated || !vfsControlInstalled || !vfsControlServerInstalled || !execControlCreated || !execServerInstalled || !execClientInstalled || !blockChannelCreated || !blockClientInstalled || !blockServerInstalled || !portObjectValid || !dmaObjectValid || !irqObjectValid || !virtioPortInstalled || !virtioIRQInstalled || !virtioDMAInstalled || !driverChannelCreated || !driverClientInstalled || !driverServerInstalled || !filesystemBlockChannelCreated || !filesystemBlockClientInstalled || !filesystemBlockServerInstalled || !systemControlInstalled || !imageObjectValid || !systemImageInstalled || !logDeviceInstalled || !loaderInstalled || !fileCacheGranted || !launchStagingMapped || !sharedRegistered || lifecycleHandle != handle.ID(0x0000000100000001) ||
		serviceDeviceHandle != handle.ID(0x0000000100000002) || vfsChannelHandle != handle.ID(0x0000000100000003) || vfsServerHandle != handle.ID(0x0000000100000004) || vfsControlHandle != handle.ID(0x0000000100000005) || vfsControlServerHandle != handle.ID(0x0000000100000006) || execServerHandle != handle.ID(0x0000000100000007) || execClientHandle != handle.ID(0x0000000100000008) || blockClientHandle != handle.ID(0x0000000100000009) || blockServerHandle != handle.ID(0x000000010000000a) || virtioPortHandle != handle.ID(0x000000010000000b) || virtioIRQHandle != handle.ID(0x000000010000000c) || virtioDMAHandle != handle.ID(0x000000010000000d) || driverClientHandle != handle.ID(0x000000010000000e) || driverServerHandle != handle.ID(0x000000010000000f) || filesystemBlockClientHandle != handle.ID(0x0000000100000010) || filesystemBlockServerHandle != handle.ID(0x0000000100000011) || systemControlHandle != handle.ID(0x0000000100000012) || launchStagingHandle != handle.ID(0x0000000100000013) || execPageHandle != handle.ID(0x0000000100000014) || systemImageHandle != handle.ID(0x0000000100000015) || logDeviceHandle != handle.ID(0x0000000100000016) || loaderHandle != handle.ID(0x0000000100000017) || fileCacheHandle != handle.ID(0x0000000100000018) {
		panicSerial("PID 1 capability layout failed")
	}
	if validation.Enabled {
		if !memoryMapRejectsInvalid(rootInit, systemControlHandle, execPageHandle) {
			panicSerial("MemoryMap validation failed")
		}
		if !executableRejectsInvalid(rootInit, loaderHandle, systemControlHandle) {
			panicSerial("executable object validation failed")
		}
		console.WriteString("EXECUTABLE_OBJECT_OK\n")
		if !quotaBoundsStreamRegistry() {
			panicSerial("resource quota validation failed")
		}
		console.WriteString("QUOTA_OK\n")
		runKernelSelfTests(pageTable)
	}
	initBytes, initDiscovered := initramfs.Find(bootuefi.Initramfs(&bootInfo), initImagePath)
	if !initDiscovered {
		console.WriteString("PANIC: init image not found in initramfs\n")
		for {
			amd64.Halt()
		}
	}
	loadedInit, loadedImageInit := loader.LoadImage(&pageAllocator, rootInit, loader.Contiguous(initBytes), nil, loader.Origin{}, initProcess.Regions())
	if !loadedImageInit {
		console.WriteString("PANIC: init native image rejected\n")
		for {
			amd64.Halt()
		}
	}
	flags := amd64.DisableInterrupts()
	if pagesUsed := initPagesBefore - pageAllocator.Statistics().FreePages; pagesUsed != 0 && !initProcess.Charge(process.ResourcePages, uint32(pagesUsed)) {
		panicSerial("PID 1 exceeded its page quota")
	}
	initTaskID, initTaskAdded := amd64.AddUserTask(&pageAllocator, rootInit, loadedInit.Entry, loadedInit.StackTop)
	if !initTaskAdded || !processRegistry.BindTask(1, sched.TaskID(initTaskID)) {
		panicSerial("PID 1 task scheduling failed")
	}
	if validation.Enabled {
		startSelfTestTasks()
	}
	amd64.RestoreInterrupts(flags)
	if validation.Enabled {
		awaitValidationBoot()
	} else {
		console.WriteString("Kernel initialized\n")
	}
	kernelLogUint(logfmt.Info, "kernel initialized", "free-pages", pageAllocator.Statistics().FreePages)
	for {
		serviceKernel()
		if initExitObserved != 0 {
			console.WriteString("PANIC: init process exited: ")
			console.WriteHex(initExitCode)
			console.WriteString("\n")
			for {
				amd64.Halt()
			}
		}
	}
}

func drawFramebufferMilestone(buffer *kernelframebuffer.Buffer) {
	width, height := buffer.Width(), buffer.Height()
	background := buffer.Color(8, 18, 40)
	white := buffer.Color(235, 240, 248)
	red := buffer.Color(220, 55, 55)
	green := buffer.Color(55, 200, 110)
	blue := buffer.Color(60, 120, 230)
	buffer.Clear(background)
	band := height / 8
	if band < 16 {
		band = 16
	}
	buffer.FillRect(0, 0, width, band, white)
	marginX, marginY := width/8, height/3
	barWidth, barHeight := (width-2*marginX)/3, height/3
	buffer.FillRect(marginX, marginY, barWidth, barHeight, red)
	buffer.FillRect(marginX+barWidth, marginY, barWidth, barHeight, green)
	buffer.FillRect(marginX+2*barWidth, marginY, width-marginX-(marginX+2*barWidth), barHeight, blue)
}

// mark prints a validation marker. Normal builds print none.
func mark(marker string) {
	if validation.Enabled {
		port := kernelconsole.New()
		port.WriteString(marker)
		port.WriteString("\n")
	}
}

// serviceKernel runs one step of the kernel's own work inside the exclusion
// domain native calls already run in, then sleeps until the next tick, task
// exit, or lifecycle request. Preempted mid-step, it would expose torn
// registries and handle tables to whichever task the timer selects next;
// sleeping inside the domain means no wake can fall between the last check
// and the sleep.
func serviceKernel() {
	flags := amd64.DisableInterrupts()
	expireWaitDeadlines()
	flushLifecycle()
	deliverProcessExits()
	consumeInitControl()
	amd64.KernelSleep()
	amd64.RestoreInterrupts(flags)
}
