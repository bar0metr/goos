//go:build goos_userspace

package main

// The shell's VFS session.

import (
	"goos/kernel/ipc"
	"goos/kernel/vfs"
	"goos/userspace/pathpage"
	"goos/userspace/runtime/sys"
)

func callVFS(request vfs.Request) (vfs.Reply, bool) {
	correlation++
	if correlation == 0 {
		correlation++
	}
	// A path too long for a message goes in the path page (ADR 0110).
	pathpage.Hold()
	defer pathpage.Release()
	attached, ok := pathpage.Prepare(&request)
	if !ok {
		return vfs.Reply{}, false
	}
	message, ok := vfs.EncodeRequest(request, correlation)
	if ok && attached != 0 {
		message.HandleCount, message.Handles[0] = 1, attached
	}
	size := 0
	if ok {
		size, ok = ipc.Encode(message, wire[:])
	}
	if !ok || sys.IPCSend(vfsHandle, wire[:size]) != 0 {
		if attached != 0 {
			sys.HandleClose(attached)
		}
		return vfs.Reply{}, false
	}
	for {
		status := sys.IPCWaitReceive(vfsHandle, wire[:], 100000)
		if status == ipc.StatusTimeout {
			continue
		}
		if status != 0 {
			return vfs.Reply{}, false
		}
		payloadSize := int(wire[24]) | int(wire[25])<<8
		wireSize := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
		if wireSize < ipc.WireHeaderSize || wireSize > len(wire) {
			return vfs.Reply{}, false
		}
		received, valid := ipc.Decode(wire[:wireSize])
		if !valid || received.Correlation != correlation {
			return vfs.Reply{}, false
		}
		reply, valid := vfs.DecodeReply(received)
		if !valid || reply.Operation != request.Operation {
			return vfs.Reply{}, false
		}
		return reply, true
	}
}
