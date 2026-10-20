//go:build goos_userspace

package main

import (
	"goos/kernel/clock"
	"goos/kernel/vfs"
	"goos/userspace/utility"
)

// Only startup and PID 1's decoded SessionNotice replace vfsHandle. A failed
// endpoint remains quarantined even if selected again; no reconnect is tried.
var filesystemRequests utility.VFSRequests

func callVFS(request vfs.Request) (vfs.Reply, bool) {
	correlation++
	if correlation == 0 {
		correlation++
	}
	return filesystemRequests.Call(vfsHandle, request, 0, correlation, 8*clock.TicksPerSecond)
}
