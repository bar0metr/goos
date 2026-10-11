//go:build goos_userspace

package remoteapi

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

func Now() uint64 { ticks, _ := sys.ClockRead(); return ticks }
func Receive(channel handle.ID, wait uint64, out *ipc.Message) kcall.Status {
	var wire [ipc.WireMaxSize]byte
	status := sys.IPCReceive(channel, wire[:])
	if status == kcall.StatusNotFound && wait != 0 {
		status = sys.IPCWaitReceive(channel, wire[:], wait)
	}
	if status != kcall.StatusOK {
		return status
	}
	n := ipc.WireHeaderSize + int(wire[24]) + int(wire[25])*256 + int(wire[26])*8
	if n > len(wire) || !ipc.DecodeInto(wire[:n], out) {
		return kcall.StatusCorruptData
	}
	return kcall.StatusOK
}
func Send(channel handle.ID, message *ipc.Message) bool {
	var wire [ipc.WireMaxSize]byte
	n, ok := ipc.EncodeFrom(message, wire[:])
	return ok && sys.IPCSend(channel, wire[:n]) == kcall.StatusOK
}

// SendWithin retries a bounded, handle-free fragment when the IPC queue is
// full. Channel WaitAny has no writable event, so a one-tick timer yields.
func SendWithin(channel handle.ID, message *ipc.Message, deadline uint64) bool {
	if message.HandleCount != 0 {
		return false
	}
	var wire [ipc.WireMaxSize]byte
	n, ok := ipc.EncodeFrom(message, wire[:])
	if !ok {
		return false
	}
	for Now() < deadline {
		status := sys.IPCSend(channel, wire[:n])
		if status == kcall.StatusOK {
			return true
		}
		if status != kcall.StatusResourceExhausted {
			return false
		}
		var interests [kcall.WaitAnyMaxObjects]sys.Interest
		if _, status = sys.WaitAny(&interests, 0, 1); status != kcall.StatusOK && status != kcall.StatusTimeout {
			return false
		}
	}
	return false
}

func CloseHandles(message *ipc.Message) {
	for i := 0; i < min(int(message.HandleCount), len(message.Handles)); i++ {
		if message.Handles[i] != 0 {
			sys.HandleClose(message.Handles[i])
		}
	}
}
