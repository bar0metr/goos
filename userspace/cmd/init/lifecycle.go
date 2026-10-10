//go:build goos_userspace

package main

// Receiving lifecycle events and replies from the kernel and services.

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

// drainRemoteLifecycle preserves notices while synchronous remote cleanup
// runs. The buffer covers the kernel task bound; no service exit is discarded.
func drainRemoteLifecycle() {
	for lifecycleCount < uint8(len(lifecyclePending)) {
		message, status := receive(lifecycleHandle, 0)
		if status == ipc.StatusNotFound || status == ipc.StatusTimeout {
			return
		}
		if status != ipc.StatusOK {
			fatal("INIT_REMOTE_LIFECYCLE_FAILED\n")
		}
		tail := (int(lifecycleHead) + int(lifecycleCount)) % len(lifecyclePending)
		lifecyclePending[tail] = message
		lifecycleCount++
	}
}

func receiveLifecycle(timeout uint64) (ipc.Message, ipc.Status) {
	if lifecycleCount != 0 {
		message := lifecyclePending[lifecycleHead]
		lifecyclePending[lifecycleHead] = ipc.Message{}
		lifecycleHead = (lifecycleHead + 1) % uint8(len(lifecyclePending))
		lifecycleCount--
		return message, ipc.StatusOK
	}
	return receive(lifecycleHandle, timeout)
}

func receiveLaunchResult(expectedCorrelation uint64) (supervisor.LifecycleEvent, ipc.Status) {
	for {
		message, status := receive(lifecycleHandle, launchReplyDeadline)
		if status != ipc.StatusOK {
			if status == ipc.StatusTimeout {
				syslog.Warn("INIT_LAUNCH_REPLY_TIMEOUT")
			}
			return supervisor.LifecycleEvent{}, status
		}
		event, valid := supervisor.DecodeLifecycle(message)
		if valid && event.Operation == supervisor.LifecycleLaunchResult && message.Correlation == expectedCorrelation {
			return event, ipc.StatusOK
		}
		if !valid || event.Operation != supervisor.LifecycleProcessExited || lifecycleCount == uint8(len(lifecyclePending)) {
			return supervisor.LifecycleEvent{}, ipc.StatusInvalidArgument
		}
		tail := (int(lifecycleHead) + int(lifecycleCount)) % len(lifecyclePending)
		lifecyclePending[tail] = message
		lifecycleCount++
	}
}

func receive(channel uint64, timeout uint64) (ipc.Message, ipc.Status) {
	for {
		status := sys.IPCWaitReceive(handle.ID(channel), wire[:], timeout)
		if status == ipc.StatusTimeout {
			return ipc.Message{}, ipc.StatusTimeout
		}
		if status != ipc.StatusOK {
			return ipc.Message{}, status
		}
		break
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	size := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
	if size < ipc.WireHeaderSize || size > len(wire) {
		return ipc.Message{}, ipc.StatusInvalidArgument
	}
	message, ok := ipc.Decode(wire[:size])
	if !ok {
		return ipc.Message{}, ipc.StatusInvalidArgument
	}
	return message, ipc.StatusOK
}

func sendMessage(channel uint64, message ipc.Message) bool {
	size, ok := ipc.Encode(message, wire[:])
	return ok && sys.IPCSend(handle.ID(channel), wire[:size]) == 0
}
