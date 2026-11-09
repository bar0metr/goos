//go:build goos_userspace

package main

import (
	"goos/kernel/vfs"
	execproto "goos/userspace/exec"
	shellcmd "goos/userspace/shell"
	"goos/userspace/volume"
)

// runVolumeCommand asks PID 1 to mount or unmount an additional volume
// (ADR 0222): mount [-t TYPE] SOURCE PATH, umount PATH.
func runVolumeCommand(invocation shellcmd.Invocation) uint64 {
	arguments := invocation.Arguments[1:invocation.Count]
	var command execproto.VolumeCommand
	if argumentIs(invocation.Arguments[0], "umount") {
		command.Path = arguments[0]
	} else {
		command.Mount = true
		if len(arguments) > 0 && argumentIs(arguments[0], "-t") {
			if len(arguments) != 4 {
				writeError("usage: mount [-t TYPE] SOURCE PATH\n")
				return shellcmd.StatusUsage
			}
			command.Type, arguments = arguments[1], arguments[2:]
		}
		if len(arguments) != 2 {
			writeError("usage: mount [-t TYPE] SOURCE PATH\n")
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
