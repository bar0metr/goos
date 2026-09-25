//go:build goos_userspace

package main

// Launching a command: the VFS session its manifest asks for, its standard
// streams, and the startup block that binds them (ADR 0080).

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
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

func launchApplication(path, command, cwd, arguments, environment []byte, streams supervisor.Streams) (uint64, handle.ID, supervisor.LaunchStatus) {
	// A command the image does not hold is reported before a session or a
	// stream is committed to it.
	executable, processName, located := locateExecutable(path)
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
	useVFS := commandGrants(command)&commands.GrantVFS != 0
	if useVFS {
		session, reason := openVFSSession()
		if reason != "" {
			syslog.Error("APP_VFS_SESSION_FAILED")
			syslog.Error(reason)
			if reason == "APP_CHANNEL_CREATE_FAILED" {
				return 0, 0, supervisor.LaunchResourceExhausted
			}
			return 0, 0, supervisor.LaunchCapabilityInstallFailed
		}
		pair[0] = uint64(session)
	}
	slot, staging := stagingSlot()
	bindings := []startup.Handle{{Name: []byte("in")}, {Name: []byte("stdout")}, {Name: []byte("stderr")}}
	if useVFS {
		bindings = append(bindings, startup.Handle{Name: []byte("vfs")})
	}
	startupData, ok := startup.Encode(command, cwd, arguments, environment, bindings, staging)
	if !ok {
		syslog.Warn("APP_STARTUP_ENCODE_FAILED")
		sys.HandleClose(handle.ID(pair[0]))
		return 0, 0, supervisor.LaunchInvalidImage
	}
	grants := []supervisor.Capability{
		streamGrant("in", streams.Input, capability.Read),
		streamGrant("stdout", streams.Output, capability.Write),
		streamGrant("stderr", streams.Error, capability.Write),
	}
	if useVFS {
		grants = append(grants, supervisor.Capability{Name: "vfs", Handle: handle.ID(pair[0]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	event, launched := requestLaunch(executable, processName, grants, slot, len(startupData))
	if launched != supervisor.LaunchOK {
		if launched == supervisor.LaunchProcessFailed && event.Operation == 0 {
			syslog.Error("APP_LAUNCH_REQUEST_FAILED")
		}
		sys.HandleClose(handle.ID(pair[0]))
		return 0, 0, launched
	}
	return event.Process, event.ProcessHandle, supervisor.LaunchOK
}

func commandGrants(command []byte) commands.Grant {
	if !commandIndexLoaded {
		loadCommandIndex()
	}
	entry, found := commands.Lookup(commandIndex[:commandIndexSize], command)
	if !found {
		return 0
	}
	return entry.Grants
}

// loadCommandIndex reads the index, or leaves it unloaded so the next launch
// tries again: a VFS that is briefly unavailable must not cost the commands
// their grants for the rest of the boot.
func loadCommandIndex() {
	session, reason := openVFSSession()
	if reason != "" {
		syslog.Warn("INIT_COMMAND_INDEX_UNAVAILABLE")
		return
	}
	size, ok := readSessionFile(session, commandIndexPath, commandIndex[:])
	sys.HandleClose(session)
	if !ok {
		syslog.Warn("INIT_COMMAND_INDEX_UNREADABLE")
		return
	}
	commandIndexSize, commandIndexLoaded = size, true
	mark("INIT_COMMAND_INDEX_OK\n")
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
	correlation++
	message, ok := vfs.EncodeRequest(request, correlation)
	if !ok || !sendMessage(uint64(session), message) {
		return vfs.Reply{}, false
	}
	reply, status := receive(uint64(session), vfsSessionDeadline)
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
