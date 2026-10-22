//go:build goos_userspace

package main

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/runtime/sys"
	"goos/userspace/utility"
)

// installedCall preserves the ordinary transaction policy and adds only a
// failed-record diagnostic in validation builds. In particular, a failed
// Close is recorded even though checkImage historically ignores its outcome.
func installedCall(session handle.ID, path, phase string, request vfs.Request) (vfs.Reply, bool) {
	if !validation.Enabled {
		return callSession(session, request)
	}
	start, clockStatus := sys.ClockRead()
	correlation++
	if correlation == 0 {
		correlation++
	}
	var failure utility.VFSFailure
	reply, received := filesystemRequests.CallWithFailure(session, request, 0, correlation, vfsSessionDeadline, &failure)
	if !received || reply.Status != vfs.StatusOK {
		end, endStatus := sys.ClockRead()
		elapsed := uint64(0)
		if clockStatus == kcall.StatusOK && endStatus == kcall.StatusOK && end >= start {
			elapsed = end - start
		}
		var line [512]byte
		write(bytesString(appendInstalledFailure(line[:0], path, phase, uint64(session), correlation, elapsed, received, reply.Status, failure)))
	}
	return reply, received
}

// readInstalledFile retains readSessionFile's read and size policy while
// naming the failed Open/Read in the installation check's diagnostics.
func readInstalledFile(session handle.ID, path string, target []byte) (int, bool) {
	opened, ok := installedCall(session, path, "open", vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte(path)})
	if !ok || opened.Status != vfs.StatusOK {
		return 0, false
	}
	size := 0
	for {
		want := uint32(vfs.MaxReplyData)
		if remaining := len(target) - size; remaining < int(want) {
			want = uint32(remaining) + 1
		}
		reply, ok := installedCall(session, path, "read", vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: want})
		if !ok || reply.Status != vfs.StatusOK || size+len(reply.Data) > len(target) {
			return 0, false
		}
		if len(reply.Data) == 0 {
			return size, true
		}
		size += copy(target[size:], reply.Data)
	}
}
