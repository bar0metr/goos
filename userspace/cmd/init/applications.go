//go:build goos_userspace

package main

// Launching a command: the VFS session its manifest asks for, its standard
// streams, and the startup block that binds them (ADR 0080).

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/vfs"
	blockproto "goos/userspace/block"
	"goos/userspace/commands"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/netapi"
	"goos/userspace/partition"
	"goos/userspace/pathpage"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
)

// openVFSSession asks VFS for a new session and returns the client endpoint the
// child will be given. PID 1 keeps no copy of either end: the server end moves
// to VFS in the request and the client end is consumed into the child's
// manifest, so no two processes ever share one session queue.
//
// The second result is empty on success and otherwise names the step that
// failed, which the caller records.
func openVFSSession() (handle.ID, string) {
	var pair [2]uint64
	if !createChannel(&pair) {
		return 0, "APP_CHANNEL_CREATE_FAILED"
	}
	correlation++
	control, ok := vfs.AddSessionRequest(handle.ID(pair[1]), correlation)
	if !ok || !sendMessage(vfsControlHandle, control) {
		closePair(pair)
		return 0, "APP_VFS_REQUEST_FAILED"
	}
	controlReply, status := receive(vfsControlHandle, vfsSessionDeadline)
	vfsStatus, valid := vfs.DecodeAddSessionReply(controlReply)
	if status != ipc.StatusOK || !valid || vfsStatus != vfs.StatusOK {
		sys.HandleClose(handle.ID(pair[0]))
		if status != ipc.StatusOK {
			return 0, "APP_VFS_RECEIVE_STATUS"
		}
		if !valid {
			return 0, "APP_VFS_REPLY_INVALID"
		}
		return 0, "APP_VFS_RESOURCE_EXHAUSTED"
	}
	return handle.ID(pair[0]), ""
}

// diskCommandSeen records that an installed command has been launched.
var diskCommandSeen bool

// launch, when not nil, is the record of a job whose only stage may be given a
// launch channel (ADR 0130); a child launched through one gets none.
func launchApplication(path, command, cwd []byte, arguments, environment execproto.List, streams supervisor.Streams, launch *launcher) (uint64, handle.ID, supervisor.LaunchStatus) {
	// A command that cannot be found or read is reported before a session or
	// a stream is committed to it.
	executable, processName, located := openExecutable(path)
	// Grants are for the commands a trusted index describes, launched from
	// the origin that index belongs to: the archive's from the archive, the
	// installed system's from the protected volume. Any other program gets
	// its standard streams and nothing more, whatever it is called
	// (ADR 0097, ADR 0175).
	granted, grantable := trustedGrants(path, executable.Image == systemImageHandle)
	process, processHandle, launched := launchExecutable(executable, processName, located, grantable, granted, command, cwd, arguments, environment, streams, launch)
	closeExecutable(executable)
	// The validation markers stay as few as they were before the system moved
	// to the volume: one per program from outside it, and one the first time
	// an installed command runs, so the console does not fill with them.
	if launched == supervisor.LaunchOK && executable.Image != systemImageHandle {
		if !execproto.InSystem(path) {
			mark("FILE_EXEC_OK\n")
		}
		if grantable && !diskCommandSeen {
			diskCommandSeen = true
			mark("SYSTEM_COMMAND_FROM_DISK_OK\n")
		}
	}
	return process, processHandle, launched
}

func launchExecutable(executable supervisor.Executable, processName string, located supervisor.LaunchStatus, grantable bool, granted commands.Grant, command, cwd []byte, arguments, environment execproto.List, streams supervisor.Streams, launch *launcher) (uint64, handle.ID, supervisor.LaunchStatus) {
	if located != supervisor.LaunchOK {
		// The kernel used to record this when it did the lookup; the record
		// moved with the lookup, so a failed command still leaves a trace.
		if located == supervisor.LaunchNotFound {
			syslog.Info("launch failed", syslog.Str("command", bytesString(command)), syslog.Uint("status", uint64(located)))
		} else {
			syslog.Warn("launch failed", syslog.Str("command", bytesString(command)), syslog.Uint("status", uint64(located)))
		}
		return 0, 0, located
	}
	var pair [2]uint64
	linuxProcess := executable.Format == supervisor.ExecutableLinuxX8664
	grantsRequested := commands.Grant(0)
	if grantable && !linuxProcess {
		grantsRequested = granted
	}
	useVFS := linuxProcess || grantable && grantsRequested&commands.GrantVFS != 0
	useBlockCheck := grantable && grantsRequested&commands.GrantBlockCheck != 0
	useLaunch := grantable && launch != nil && grantsRequested&commands.GrantLaunch != 0 && streams.Input.Type == handle.TypeStream
	// A command asks for one network role; one asking for both gets the
	// narrower, so no session can both ping and reload. Without a network
	// service the command runs without a session and reports it.
	var netSession handle.ID
	switch {
	case grantable && grantsRequested&commands.GrantNetworkPing != 0:
		netSession, _ = openNetSession(netapi.RightsPingStatus)
	case grantable && grantsRequested&commands.GrantNetworkAdmin != 0:
		netSession, _ = openNetSession(netapi.RightsAdminStatus)
	}
	useNetwork := netSession != 0
	// A resolver session is separate from the network one: resolving names
	// grants no echo, and pinging grants no name resolution.
	var resolveSession handle.ID
	switch {
	case grantable && grantsRequested&commands.GrantResolve != 0:
		resolveSession, _ = openResolverSession(netapi.RightsResolveStatus)
	case grantable && grantsRequested&commands.GrantResolveAdmin != 0:
		resolveSession, _ = openResolverSession(netapi.RightsAdminStatus)
	}
	useResolver := resolveSession != 0
	// A UDP session is its own grant: neither ping nor name resolution
	// carries the authority to send datagrams.
	var udpSession [3]handle.ID
	if grantable && grantsRequested&commands.GrantUDP != 0 {
		udpSession, _ = openUDPSession()
	}
	useUDP := udpSession[0] != 0
	var tcpSession [3]handle.ID
	if grantable && grantsRequested&commands.GrantTCP != 0 {
		tcpSession, _ = openTCPSession()
	}
	useTCP := tcpSession[0] != 0
	if useBlockCheck && boot.Mode() != supervisor.ModeRecovery {
		// The running system starts its programs from the volume, so the
		// volume is never taken from it for a check (ADR 0175).
		syslog.Warn("volume check refused in normal mode")
		closeOpen(netSession)
		closeOpen(resolveSession)
		closeUDP(udpSession)
		closeUDP(tcpSession)
		return 0, 0, supervisor.LaunchVolumeBusy
	}
	var checkBlock handle.ID
	if useBlockCheck {
		var reason string
		checkBlock, reason = openCheckBlock()
		if reason != "" {
			syslog.Error(reason)
			closeOpen(netSession)
			closeOpen(resolveSession)
			closeUDP(udpSession)
			closeUDP(tcpSession)
			return 0, 0, supervisor.LaunchCapabilityInstallFailed
		}
	}
	if useVFS {
		session, reason := openVFSSession()
		if reason != "" {
			syslog.Error("APP_VFS_SESSION_FAILED")
			syslog.Error(reason)
			closeOpen(netSession)
			closeOpen(resolveSession)
			closeUDP(udpSession)
			closeUDP(tcpSession)
			if reason == "APP_CHANNEL_CREATE_FAILED" {
				return 0, 0, supervisor.LaunchResourceExhausted
			}
			return 0, 0, supervisor.LaunchCapabilityInstallFailed
		}
		pair[0] = uint64(session)
	}
	// A launcher's input is granted as a copy, so PID 1 keeps the reader
	// to give its children.
	var launchPair [2]uint64
	if useLaunch {
		if !createChannel(&launchPair) {
			closeOpen(netSession)
			closeOpen(resolveSession)
			closeUDP(udpSession)
			closeUDP(tcpSession)
			sys.HandleClose(handle.ID(pair[0]))
			sys.HandleClose(checkBlock)
			if useBlockCheck {
				manager.CancelStop(filesystemName)
			}
			return 0, 0, supervisor.LaunchResourceExhausted
		}
		streams.Input.Shared = true
	}
	slot, staging := stagingSlot()
	handles := 3
	if useVFS {
		handles++
	}
	if useBlockCheck {
		handles++
	}
	if useLaunch {
		handles++
	}
	if useNetwork {
		handles++
	}
	if useResolver {
		handles++
	}
	if useUDP {
		handles += len(udpSession)
	}
	if useTCP {
		handles += len(tcpSession)
	}
	// A Linux program searches the Linux fixture directory, never the
	// native one: its execve refuses native images.
	addLinuxPath := linuxProcess && execproto.LinuxPathMissing(environment)
	entries := environment.Count()
	if addLinuxPath {
		entries++
	}
	writer := startup.NewWriter(staging, arguments.Count(), entries, handles)
	writer.Command(command)
	writer.CWD(cwd)
	for offset, index := 0, 0; index < arguments.Count(); index++ {
		var argument []byte
		argument, offset = arguments.Next(offset)
		writer.Argument(argument)
	}
	for offset, index := 0, 0; index < environment.Count(); index++ {
		var variable []byte
		variable, offset = environment.Next(offset)
		if linuxProcess {
			variable = execproto.LinuxEnvironmentEntry(variable)
		}
		writer.Entry(variable)
	}
	if addLinuxPath {
		writer.Entry(execproto.LinuxPathVariable())
	}
	writer.Handle([]byte("in"), 0)
	writer.Handle([]byte("stdout"), 0)
	writer.Handle([]byte("stderr"), 0)
	if useVFS {
		writer.Handle([]byte("vfs"), 0)
	}
	if useBlockCheck {
		writer.Handle([]byte("block"), 0)
	}
	if useLaunch {
		writer.Handle([]byte("launch"), 0)
	}
	if useNetwork {
		writer.Handle([]byte("net"), 0)
	}
	if useResolver {
		writer.Handle([]byte("resolve"), 0)
	}
	if useUDP {
		writer.Handle([]byte("udp"), 0)
		writer.Handle([]byte("udp-tx"), 0)
		writer.Handle([]byte("udp-rx"), 0)
	}
	if useTCP {
		writer.Handle([]byte("tcp"), 0)
		writer.Handle([]byte("tcp-tx"), 0)
		writer.Handle([]byte("tcp-rx"), 0)
	}
	// The request was validated when it arrived, so what does not fit is its
	// size: its strings need more room in the startup object's tables.
	startupData, ok := writer.Finish()
	if !ok {
		syslog.Warn("APP_STARTUP_ENCODE_FAILED")
		closeOpen(netSession)
		closeOpen(resolveSession)
		closeUDP(udpSession)
		closeUDP(tcpSession)
		closePair(launchPair)
		sys.HandleClose(handle.ID(pair[0]))
		sys.HandleClose(checkBlock)
		if useBlockCheck {
			manager.CancelStop(filesystemName)
		}
		return 0, 0, supervisor.LaunchArgumentsTooLong
	}
	grants := []supervisor.Capability{
		streamGrant("in", streams.Input, capability.Read),
		streamGrant("stdout", streams.Output, capability.Write),
		streamGrant("stderr", streams.Error, capability.Write),
	}
	if useVFS {
		grants = append(grants, supervisor.Capability{Name: "vfs", Handle: handle.ID(pair[0]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	if useBlockCheck {
		grants = append(grants, supervisor.Capability{Name: "block", Handle: checkBlock, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	if useLaunch {
		grants = append(grants, supervisor.Capability{Name: "launch", Handle: handle.ID(launchPair[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	if useNetwork {
		grants = append(grants, supervisor.Capability{Name: "net", Handle: netSession, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	if useResolver {
		grants = append(grants, supervisor.Capability{Name: "resolve", Handle: resolveSession, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	if useUDP {
		grants = append(grants,
			supervisor.Capability{Name: "udp", Handle: udpSession[0], Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
			supervisor.Capability{Name: "udp-tx", Handle: udpSession[1], Type: handle.TypeMemory, Rights: capability.Read | capability.Write, Consume: true},
			supervisor.Capability{Name: "udp-rx", Handle: udpSession[2], Type: handle.TypeMemory, Rights: capability.Read, Consume: true})
	}
	if useTCP {
		grants = append(grants,
			supervisor.Capability{Name: "tcp", Handle: tcpSession[0], Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
			supervisor.Capability{Name: "tcp-tx", Handle: tcpSession[1], Type: handle.TypeMemory, Rights: capability.Read | capability.Write, Consume: true},
			supervisor.Capability{Name: "tcp-rx", Handle: tcpSession[2], Type: handle.TypeMemory, Rights: capability.Read, Consume: true})
	}
	profile := supervisor.ProfileNormal
	if linuxProcess {
		profile = supervisor.ProfileLarge
	}
	if !linuxProcess && grantable && equalBytes(command, "quota") {
		profile = supervisor.ProfileTiny
	}
	if !linuxProcess && grantable && (equalBytes(command, "fsck-fat") || equalBytes(command, "vmprobe")) {
		profile = supervisor.ProfileLarge
	}
	event, launched := requestLaunch(executable, processName, grants, profile, slot, uint32(len(startupData)))
	if launched != supervisor.LaunchOK {
		if launched == supervisor.LaunchProcessFailed && event.Operation == 0 {
			syslog.Error("APP_LAUNCH_REQUEST_FAILED")
		}
		closePair(launchPair)
		closeOpen(netSession)
		closeOpen(resolveSession)
		closeUDP(udpSession)
		closeUDP(tcpSession)
		sys.HandleClose(handle.ID(pair[0]))
		sys.HandleClose(checkBlock)
		if useBlockCheck {
			manager.CancelStop(filesystemName)
		}
		return 0, 0, launched
	}
	if useBlockCheck {
		filesystemChecker = event.Process
	}
	if useLaunch {
		launch.server, launch.input = handle.ID(launchPair[0]), streams.Input.ID
	}
	return event.Process, event.ProcessHandle, supervisor.LaunchOK
}

// closeOpen closes a handle a failed launch did not hand over.
// closeUDP closes whatever of a UDP session's handles was opened.
func closeUDP(session [3]handle.ID) {
	for _, id := range session {
		closeOpen(id)
	}
}

func closeOpen(id handle.ID) {
	if id != 0 {
		sys.HandleClose(id)
	}
}

// openCheckBlock detaches the mounted filesystem and replaces its block
// endpoint with a private read-only checker endpoint. Closing it lets the
// supervised FAT32 service restart and mount the volume again.
func openCheckBlock() (handle.ID, string) {
	if manager.PlanStop(filesystemName) != supervisor.StatusOK {
		return 0, "APP_FSCK_FILESYSTEM_NOT_RUNNING"
	}
	correlation++
	request, ok := vfs.UnmountRequest(correlation)
	if !ok || !sendMessage(vfsControlHandle, request) {
		manager.CancelStop(filesystemName)
		return 0, "APP_FSCK_UNMOUNT_SEND_FAILED"
	}
	reply, received := receive(vfsControlHandle, vfsSessionDeadline)
	status, valid := vfs.DecodeAddSessionReply(reply)
	if received != ipc.StatusOK || !valid || status != vfs.StatusOK {
		manager.CancelStop(filesystemName)
		return 0, "APP_FSCK_UNMOUNT_REFUSED"
	}
	var pair [2]uint64
	if !createChannel(&pair) {
		manager.CancelStop(filesystemName)
		return 0, "APP_FSCK_CHANNEL_CREATE_FAILED"
	}
	correlation++
	control, ok := blockproto.SetFilesystemRequest(handle.ID(pair[1]), partition.Selector{Kind: partition.SelectAuto}, correlation)
	if !ok || !sendMessage(uint64(blockControl[0]), control) {
		manager.CancelStop(filesystemName)
		closePair(pair)
		return 0, "APP_FSCK_BLOCK_SEND_FAILED"
	}
	blockReply, receiveStatus := receive(uint64(blockControl[0]), vfsSessionDeadline)
	blockStatus, valid := blockproto.DecodeSetDriverReply(blockReply)
	if receiveStatus != ipc.StatusOK || !valid || blockStatus != blockproto.StatusOK {
		manager.CancelStop(filesystemName)
		sys.HandleClose(handle.ID(pair[0]))
		return 0, "APP_FSCK_BLOCK_REFUSED"
	}
	return handle.ID(pair[0]), ""
}

// readSessionFile reads the whole of path into target and refuses a file that
// does not fit, rather than using a prefix of it.
func readSessionFile(session handle.ID, path []byte, target []byte) (int, bool) {
	opened, ok := callSession(session, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	if !ok || opened.Status != vfs.StatusOK {
		return 0, false
	}
	size := 0
	for {
		want := uint32(vfs.MaxReplyData)
		if remaining := len(target) - size; remaining < int(want) {
			// Ask for one byte past a full buffer, so an oversized file is
			// detected instead of silently truncated.
			want = uint32(remaining) + 1
		}
		reply, ok := callSession(session, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: want})
		if !ok || reply.Status != vfs.StatusOK || size+len(reply.Data) > len(target) {
			return 0, false
		}
		if len(reply.Data) == 0 {
			return size, true
		}
		size += copy(target[size:], reply.Data)
	}
}

func callSession(session handle.ID, request vfs.Request) (vfs.Reply, bool) {
	return callSessionAttached(session, request, 0, vfsSessionDeadline)
}

// callSessionAttached is callSession with a handle moved to the VFS with the
// request and a deadline of the caller's choosing.
func callSessionAttached(session handle.ID, request vfs.Request, attached handle.ID, deadline uint64) (vfs.Reply, bool) {
	// A program path too long for a message goes in the path page (ADR 0110).
	if attached == 0 {
		var ok bool
		if attached, ok = pathpage.Prepare(&request); !ok {
			return vfs.Reply{}, false
		}
	}
	correlation++
	message, ok := vfs.EncodeRequest(request, correlation)
	if !ok {
		if attached != 0 {
			sys.HandleClose(attached)
		}
		return vfs.Reply{}, false
	}
	if attached != 0 {
		message.HandleCount, message.Handles[0] = 1, attached
	}
	if !sendMessage(uint64(session), message) {
		if attached != 0 {
			sys.HandleClose(attached)
		}
		return vfs.Reply{}, false
	}
	reply, status := receive(uint64(session), deadline)
	if status != ipc.StatusOK || reply.Correlation != correlation {
		return vfs.Reply{}, false
	}
	decoded, ok := vfs.DecodeReply(reply)
	return decoded, ok && decoded.Operation == request.Operation
}

// streamGrant grants one standard-stream endpoint. A pipeline's own streams are
// consumed by a successful launch; the terminal's writing end is shared, so the
// child gets a counted copy and PID 1 keeps its own (ADR 0089).
func streamGrant(name string, endpoint supervisor.Endpoint, rights capability.Rights) supervisor.Capability {
	return supervisor.Capability{Name: name, Handle: endpoint.ID, Type: endpoint.Type, Rights: streamRights(endpoint.Type, rights), Consume: endpoint.Type == handle.TypeStream && !endpoint.Shared}
}

func streamRights(typeID handle.Type, rights capability.Rights) capability.Rights {
	if typeID == handle.TypeStream {
		return rights | capability.Wait
	}
	return rights
}
