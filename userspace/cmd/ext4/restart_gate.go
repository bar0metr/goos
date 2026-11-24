//go:build goos_userspace && goos_goroutines && goos_validation && goos_ext4_restart_gate

package main

import (
	"goos/kernel/clock"
	"goos/kernel/kcall"
	"goos/userspace/fsproto"
	userwait "goos/userspace/wait"
)

// The probe reserves the cold page at 1 MiB for the fault barrier.
func restartGateRead(request fsproto.Request) bool {
	return request.Operation == fsproto.OperationRead && request.Offset == 1024*1024 && request.Size == fsproto.PageSize
}

// holdRestartRead retains an admitted worker until its backing endpoint dies.
// The host injects the fault after this marker. The VFS persistent lane queues
// the other three reader processes, so this gate proves one active ext4 read.
// A timeout fails the gate; no worker fabricates a successful read.
func holdRestartRead() bool {
	mark("EXT4_RESTART_READER_ACTIVE\n")
	var interests [userwait.MaxObjects]userwait.Interest
	interests[0] = userwait.Interest{Handle: blockHandle, Events: kcall.WaitPeerClosed}
	interests[1] = userwait.Interest{Handle: vfsHandle, Events: kcall.WaitPeerClosed}
	_, status := userwait.Any(&interests, 2, 4*clock.TicksPerSecond)
	if status != kcall.StatusOK {
		mark("EXT4_RESTART_BARRIER_TIMEOUT\n")
	} else {
		mark("EXT4_RESTART_ACTIVE_READ_REVOKED\n")
	}
	return true
}
