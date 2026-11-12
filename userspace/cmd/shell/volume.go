//go:build goos_userspace

package main

import (
	"goos/kernel/vfs"
	execproto "goos/userspace/exec"
	shellcmd "goos/userspace/shell"
	"goos/userspace/volume"
)

const mountUsage = "usage: mount [-t TYPE] [-o ro|rw] SOURCE PATH\n"

// runVolumeCommand asks PID 1 to mount or unmount an additional volume
// (ADR 0222, ADR 0226): mount [-t TYPE] [-o ro|rw] SOURCE PATH, umount PATH.
// A mount is read-only unless -o rw asks for a writable one.
func runVolumeCommand(invocation shellcmd.Invocation) uint64 {
	arguments := invocation.Arguments[1:invocation.Count]
	var command execproto.VolumeCommand
	if argumentIs(invocation.Arguments[0], "umount") {
		command.Path = arguments[0]
	} else {
		command.Mount = true
		for len(arguments) > 2 {
			switch {
			case argumentIs(arguments[0], "-t") && command.Type == nil:
				command.Type = arguments[1]
			case argumentIs(arguments[0], "-o"):
				writable, ok := volume.ParseMode(arguments[1])
				if !ok {
					writeError("mount: ")
					writeError(volume.InvalidMode.String())
					writeError("\n")
					return shellcmd.StatusUsage
				}
				command.Writable = writable
			default:
				writeError(mountUsage)
				return shellcmd.StatusUsage
			}
			arguments = arguments[2:]
		}
		if len(arguments) != 2 {
			writeError(mountUsage)
			return shellcmd.StatusUsage
		}
		command.Source, command.Path = arguments[0], arguments[1]
	}
	correlation++
	request, ok := execproto.EncodeVolumeCommand(command, correlation)
	if !ok {
		writeError("mount: arguments too long\n")
		return shellcmd.StatusUsage
	}
	if !sendExec(request) {
		writeError("mount: PID 1 unreachable\n")
		return shellcmd.StatusFailure
	}
	reply, ok := awaitReply(correlation, -1)
	status, detail, valid := execproto.DecodeVolumeReply(reply)
	if !ok || !valid {
		writeError("mount: no answer from PID 1\n")
		return shellcmd.StatusFailure
	}
	if volume.Status(status) == volume.OK {
		mark("SHELL_VOLUME_OK\n")
		return 0
	}
	writeErrorBytes(invocation.Arguments[0])
	writeError(": ")
	writeError(volume.Status(status).String())
	if volume.Status(status) == volume.Refused {
		writeError(": ")
		writeError(shellcmd.VFSReason(vfs.Status(detail), false))
	}
	writeError("\n")
	return shellcmd.StatusFailure
}
