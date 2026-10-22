//go:build goos_userspace

package main

import (
	"goos/userspace/runtime/sys"
	vfsservice "goos/userspace/vfs"
)

// reportCopyFault is the validation build's handler for a read-path copy
// with no source or destination array. It writes one marker line and exits
// without running the copy: the buffer header is torn or the plan state is
// wrong, so failing only the request would hide the cause and keep serving
// from state that is no longer trustworthy. PID 1 restarts the service, as it
// does after a fault. Owner and workers may both call it; each prints its own
// line from its own stack.
func reportCopyFault(fault vfsservice.CopyFault) {
	var line [288]byte
	sys.ConsoleWrite(consoleHandle, fault.AppendMarker(line[:0]))
	sys.Exit(exitFailed)
}
