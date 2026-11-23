//go:build goos_userspace && !goos_goroutines

package main

import (
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/userspace/fsproto"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
	userwait "goos/userspace/wait"
)

// runService answers one request at a time on one thread. The goroutine build
// replaces this with an owner and a bounded read-worker pool (ADR 0232).
func runService() {
	commitDue := uint64(0)
	for {
		wait := uint64(requestWait)
		if volume.Pending() {
			now := ticks()
			if commitDue == 0 {
				commitDue = now + commitInterval
			}
			if now >= commitDue {
				commitDue = 0
				volume.Flush()
				exitIfFailed()
				continue
			}
			wait = commitDue - now
		} else {
			commitDue = 0
		}
		var interests [userwait.MaxObjects]userwait.Interest
		interests[0] = userwait.Interest{Handle: vfsHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		interests[1] = userwait.Interest{Handle: blockHandle, Events: kcall.WaitPeerClosed}
		ready, waited := userwait.Any(&interests, 2, wait)
		if waited == kcall.StatusTimeout {
			continue
		}
		if waited != kcall.StatusOK {
			fatal("EXT4_WAIT_FAILED\n")
		}
		if ready.ReadyMask&2 != 0 {
			// The block service restarted, or PID 1 unmounted the volume
			// and closed the slot: either way this instance is done.
			logCounters()
			sys.Exit(exitBlockGone)
		}
		status := sys.IPCReceive(vfsHandle, wire[:])
		if status == kcall.StatusNotFound {
			continue
		}
		if status == kcall.StatusPeerDied {
			// The block endpoint still works: commit what the volume holds
			// before PID 1 replaces this instance.
			volume.Flush()
			syslog.Warn("vfs endpoint closed")
			logCounters()
			sys.Exit(exitVFSGone)
		}
		if status != kcall.StatusOK {
			fatal("EXT4_RECEIVE_FAILED\n")
		}
		if !ipc.DecodeInto(wire[:wireSize(wire[:])], &requestMessage) {
			continue
		}
		serve()
		exitIfFailed()
	}
}

// serve decodes the received request and answers it.
func serve() {
	request, valid := fsproto.DecodeRequest(&requestMessage, cache, &requestedName)
	var reply fsproto.Reply
	if !valid {
		operation := fsproto.Operation(requestMessage.Payload[0])
		if requestMessage.Correlation == 0 || operation < fsproto.OperationMount || operation > fsproto.OperationUnmount {
			return
		}
		reply = fsproto.Reply{Operation: operation, Status: ramfs.StatusInvalidArgument}
	} else {
		reply = answer(request, &listedName)
	}
	sendReply(reply, requestMessage.Correlation)
}
