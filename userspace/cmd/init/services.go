//go:build goos_userspace

package main

// Service declarations: which services run, with which capabilities, in which
// order, and the sessions PID 1 asks the configuration service for.

import (
	"goos/internal/capability"
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	configproto "goos/userspace/config"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
)

// startServices registers the system services and launches them in
// dependency order, reporting each on the console.
func startServices(ticks uint64) {
	sessions[0].active = true
	sessions[1].active = graphicalConsolePresent()
	for index := range sessions {
		if sessions[index].active && !mapSessionPage(&sessions[index], index == 0, execproto.AreaAddress, execproto.AreaSize) {
			fatal("INIT_EXEC_PAGE_FAILED\n")
		}
	}
	write("\nStarting services...\n")
	// The block service's control endpoint, over which a restarted driver's
	// replacement channel is delivered. PID 1 holds the only client side.
	if !createChannel(&blockControl) {
		fatal("INIT_BLOCK_CONTROL_FAILED\n")
	}
	// The channel between the VFS and the filesystem service. The service is
	// given one end at launch, the VFS the other once the service runs.
	var filesystemPair [2]uint64
	if !createChannel(&filesystemPair) {
		fatal("INIT_FILESYSTEM_CHANNEL_FAILED\n")
	}
	filesystemClient = handle.ID(filesystemPair[0])
	if manager.Register(supervisor.Definition{Name: "vfs", Executable: "/system/vfs",
		Capabilities: []supervisor.Capability{
			{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
			{Name: "session", Handle: vfsServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
			{Name: "control", Handle: vfsControlServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
			// PID 1 keeps its own copy, with Execute, to launch from the
			// image; the filesystem service may only read it (ADR 0093).
			{Name: "image", Handle: systemImageHandle, Type: handle.TypeDevice, Rights: capability.Read},
			{Name: "cache", Handle: fileCacheHandle, Type: handle.TypeMemory, Rights: capability.Read | capability.Write},
			// A VFS that fails is started again, and its clients are handed
			// new sessions (ADR 0099).
		}, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK ||
		manager.Register(supervisor.Definition{Name: "block", Executable: "/system/block",
			Capabilities: []supervisor.Capability{
				{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
				{Name: "block", Handle: blockServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
				{Name: "driver", Handle: driverClientHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
				{Name: "filesystem", Handle: filesystemBlockServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
				{Name: "control", Handle: handle.ID(blockControl[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
				// A block service that fails is started again; the driver
				// and the filesystem service start again after it
				// (ADR 0101).
			}, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK ||
		// The driver's device authority stays with PID 1 rather than being
		// consumed. A driver that crashes has to be replaced by another one
		// holding the same ports, line and DMA domain, and only PID 1 can grant
		// them. The dead process's copies are released by its teardown, which
		// also resets the device and masks its interrupt.
		// The driver serves the block service and nothing else, so it starts
		// after it, and again after it restarts.
		manager.Register(supervisor.Definition{Name: "virtio-blk", Executable: "/system/virtio-blk", Dependencies: []string{"block"},
			Capabilities: []supervisor.Capability{
				{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
				{Name: "ports", Handle: virtioPortHandle, Type: handle.TypeDevice, Rights: capability.Read | capability.Write},
				{Name: "irq", Handle: virtioIRQHandle, Type: handle.TypeIRQ, Rights: capability.Wait},
				{Name: "dma", Handle: virtioDMAHandle, Type: handle.TypeDevice, Rights: capability.Write},
				{Name: "service", Handle: driverServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
			}, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK ||
		// The FAT32 service parses the untrusted volume in a process of its
		// own, between the VFS and the block service (ADR 0096). It holds a
		// channel to each and nothing else: no namespace, no device.
		// It depends on the VFS as well as the block service and its driver:
		// it serves nothing without either, and after a restart of any of
		// them it has to start again once they run, so its mount can ask the
		// device for its size.
		manager.Register(supervisor.Definition{Name: filesystemName, Executable: "/system/fat32", Dependencies: []string{"block", "virtio-blk", "vfs"},
			Capabilities: []supervisor.Capability{
				{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
				{Name: "vfs", Handle: handle.ID(filesystemPair[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
				{Name: "block", Handle: filesystemBlockClientHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
				// File data moves through the cache it shares with the VFS,
				// not through messages (ADR 0098).
				{Name: "cache", Handle: fileCacheHandle, Type: handle.TypeMemory, Rights: capability.Read | capability.Write},
			}, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK {
		fatal("INIT_SERVICE_REGISTRATION_FAILED\n")
	}
	if manager.StartReady(ticks+supervisor.RestartBackoff, launchService) != 4 || validation.Enabled && !probeBlockService() {
		fatal("SHELL_LAUNCH_FAILED\n")
	}
	if validation.Enabled {
		if !probeLinuxLaunch() {
			fatal("LINUX_PID1_LAUNCH_FAILED\n")
		}
		mark("LINUX_PID1_LAUNCH_OK\n")
	}
	// The log service comes after the filesystem and before the shell: its
	// session has to be asked for rather than handed over at boot, and it
	// should be persisting records by the time a prompt appears.
	// Configuration comes first: the log service consults it, so it has to be
	// answering before anything that reads a setting starts.
	startConfigService(ticks)
	startLogService(ticks)
	for index := range sessions {
		if sessions[index].active {
			startSession(&sessions[index], ticks)
		}
	}
	// The fault goes in last, once the filesystem has mounted and the system is
	// up: the point is that a driver crashing under a working system is
	// recovered from, not that one crashing during boot is survivable.
	injectDriverFault()
	supervisionComplete = true
	arenaBase = arena.Current()
}

// startSession starts a console's terminal service and then its shell.
func startSession(session *terminalSession, ticks uint64) {
	terminal := startTerminalService(session, ticks)
	if session.execClient == 0 {
		// The first session's exec channel was made by the kernel at boot;
		// the others' are made here, the server end kept, as for a restart.
		var exec [2]uint64
		if !createChannel(&exec) {
			fatal("INIT_EXEC_CHANNEL_FAILED\n")
		}
		session.execClient, session.execServer = handle.ID(exec[0]), exec[1]
	}
	// The shell administers configuration, so its session is the whole
	// namespace and may write. Every other client gets less.
	configClient, _ := openConfigSession("", configproto.Read|configproto.Write)
	// Each shell gets a filesystem session of its own; the kernel made the
	// first one at boot.
	files := vfsChannelHandle
	if session != &sessions[0] {
		var reason string
		if files, reason = openVFSSession(); reason != "" {
			syslog.Error("shell filesystem session unavailable", syslog.Str("reason", reason))
			fatal("INIT_SHELL_VFS_FAILED\n")
		}
	}
	// The shell holds no console capability: what is typed reaches it as
	// lines from the terminal service, and what it prints goes back through
	// that service on a copy of the terminal's writing end.
	shellCapabilities := []supervisor.Capability{
		{Name: "stdout", Handle: handle.ID(session.output[1]), Type: handle.TypeStream, Rights: capability.Write | capability.Wait},
		{Name: "tty", Handle: terminal, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "log", Handle: logDeviceHandle, Type: handle.TypeDevice, Rights: capability.Read},
		{Name: "vfs", Handle: files, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "exec", Handle: session.execClient, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "execpage", Handle: session.pageHandle, Type: handle.TypeMemory, Rights: capability.Read | capability.Write},
	}
	if configClient != 0 {
		shellCapabilities = append(shellCapabilities, supervisor.Capability{Name: "config", Handle: configClient,
			Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	if manager.Register(supervisor.Definition{Name: session.shell, Executable: "/system/shell", Dependencies: []string{"vfs", session.tty},
		Capabilities: shellCapabilities, Restart: supervisor.RestartAlways}) != supervisor.StatusOK {
		fatal("INIT_SERVICE_REGISTRATION_FAILED\n")
	}
	if manager.StartReady(ticks+supervisor.RestartBackoff, launchService) != 1 {
		fatal("SHELL_LAUNCH_FAILED\n")
	}
}

// startTerminalService starts a session's terminal service and returns the
// client end of the session its shell reads lines over. The service is the
// only process given the right to read its console (ADR 0082), and the only
// one that reads what its shell and their commands print (ADR 0089).
//
// The terminal and the shell restart independently. The session between them
// is a channel, which dies with either, so whichever restarts is launched with
// a fresh one and the survivor is handed the other end: the shell over its
// exec channel, the terminal over the control channel created here.
func startTerminalService(session *terminalSession, ticks uint64) handle.ID {
	var pair, control [2]uint64
	if !createStream(&session.output) {
		fatal("INIT_TTY_OUTPUT_FAILED\n")
	}
	if !createChannel(&pair) || !createChannel(&control) {
		fatal("INIT_TTY_CHANNEL_FAILED\n")
	}
	session.ttyControl = handle.ID(control[0])
	capabilities := []supervisor.Capability{
		{Name: "console", Handle: session.console, Type: handle.TypeDevice, Rights: capability.Read | capability.Write},
		{Name: "output", Handle: handle.ID(session.output[0]), Type: handle.TypeStream, Rights: capability.Read | capability.Wait},
		{Name: "session", Handle: handle.ID(pair[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "control", Handle: handle.ID(control[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
	}
	if validation.Enabled && session.console != serialConsoleHandle {
		// The QEMU tests read only COM1. The validation build lets the
		// framebuffer's terminal copy what it writes there once someone
		// types at it, so a test that drives the keyboard sees the result.
		capabilities = append(capabilities, supervisor.Capability{Name: "mirror", Handle: serialConsoleHandle, Type: handle.TypeDevice, Rights: capability.Write})
	}
	// Always, not on failure: a terminal that exits, even cleanly, leaves a
	// console no one can type at. The restart limit still ends a crash loop.
	if manager.Register(supervisor.Definition{Name: session.tty, Executable: "/system/tty",
		Capabilities: capabilities, Restart: supervisor.RestartAlways}) != supervisor.StatusOK {
		fatal("INIT_SERVICE_REGISTRATION_FAILED\n")
	}
	if manager.StartReady(ticks+supervisor.RestartBackoff, launchService) != 1 {
		fatal("INIT_TTY_LAUNCH_FAILED\n")
	}
	return handle.ID(pair[0])
}

// startLogService registers and starts the persistent log service after the
// filesystem is up, because its session has to be asked for rather than handed
// over at boot: it is the only service whose VFS endpoint is created the same
// way an application's is.
//
// A failure here does not stop the system. The kernel ring is the system log
// and keeps working; only its persistence is lost, and that is worth a warning
// rather than a dead boot.
func startLogService(ticks uint64) {
	session, reason := openVFSSession()
	if reason != "" {
		syslog.Warn("log service has no filesystem session", syslog.Str("reason", reason))
		return
	}
	capabilities := []supervisor.Capability{
		// The log capability authorizes draining the kernel ring; console
		// write is the service's own logging. It gets no other authority,
		// and in particular cannot read what is typed.
		{Name: "log", Handle: logDeviceHandle, Type: handle.TypeDevice, Rights: capability.Read},
		{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
		{Name: "vfs", Handle: session, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
	}
	// A read-only session confined to the service's own namespace. It consults
	// the setting that governs it and can neither change it nor see anything
	// else. Without a session it keeps its default.
	if configSession, ok := openConfigSession("system.log.", configproto.Read); ok {
		capabilities = append(capabilities, supervisor.Capability{Name: "config", Handle: configSession,
			Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	// The channel a new filesystem session arrives on after a VFS restart,
	// so the service keeps its place in the log rather than restarting.
	var control [2]uint64
	if createChannel(&control) {
		logControl = handle.ID(control[0])
		capabilities = append(capabilities, supervisor.Capability{Name: "control", Handle: handle.ID(control[1]),
			Type: handle.TypeChannel, Rights: capability.Read | capability.Wait, Consume: true})
	}
	if manager.Register(supervisor.Definition{Name: "log", Executable: "/system/log", Dependencies: []string{"vfs"},
		Capabilities: capabilities, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK {
		syslog.Warn("log service registration failed")
		sys.HandleClose(session)
		return
	}
	if manager.StartReady(ticks+supervisor.RestartBackoff, launchService) != 1 {
		syslog.Warn("log service did not start")
	}
}

// startConfigService brings up the typed configuration store and returns the
// client endpoint the shell's session runs over, or zero if it could not start.
//
// PID 1 creates that session and is the only process that decides what it may
// do. The scope and rights the service applies to it are granted here, not
// asked for by the client, which is what makes the store's checks authority
// rather than convention.
//
// Like the log service, a failure warns rather than stopping the boot: nothing
// else reads configuration yet, and a system without it is still a system.
func startConfigService(ticks uint64) handle.ID {
	session, reason := openVFSSession()
	if reason != "" {
		syslog.Warn("config service has no filesystem session", syslog.Str("reason", reason))
		return 0
	}
	var pair [2]uint64
	if !createChannel(&pair) {
		syslog.Warn("config service channel unavailable")
		sys.HandleClose(session)
		return 0
	}
	if manager.Register(supervisor.Definition{Name: "config", Executable: "/system/config", Dependencies: []string{"vfs"},
		Capabilities: []supervisor.Capability{
			{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
			{Name: "vfs", Handle: session, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
			{Name: "control", Handle: handle.ID(pair[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		}, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK {
		syslog.Warn("config service registration failed")
		closePair(pair)
		sys.HandleClose(session)
		return 0
	}
	if manager.StartReady(ticks+supervisor.RestartBackoff, launchService) != 1 {
		syslog.Warn("config service did not start")
		sys.HandleClose(handle.ID(pair[0]))
		return 0
	}
	configControlHandle = handle.ID(pair[0])
	return configControlHandle
}

// openConfigSession asks the configuration service for a session confined to
// scope and limited to rights, and returns the client endpoint for the process
// that will use it. PID 1 chooses both: a client receives an endpoint that is
// already limited and cannot ask for more.
func openConfigSession(scope string, rights configproto.Rights) (handle.ID, bool) {
	if configControlHandle == 0 {
		return 0, false
	}
	confined, ok := configproto.MakeScope(scope)
	if !ok {
		return 0, false
	}
	var pair [2]uint64
	if !createChannel(&pair) {
		return 0, false
	}
	correlation++
	request, ok := configproto.AddSessionRequest(handle.ID(pair[1]), confined, rights, correlation)
	if !ok || !sendMessage(uint64(configControlHandle), request) {
		closePair(pair)
		return 0, false
	}
	reply, status := receive(uint64(configControlHandle), vfsSessionDeadline)
	configStatus, valid := configproto.DecodeAddSessionReply(reply)
	if status != ipc.StatusOK || !valid || configStatus != configproto.StatusOK {
		sys.HandleClose(handle.ID(pair[0]))
		return 0, false
	}
	return handle.ID(pair[0]), true
}
