//go:build goos_userspace

package utility

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/runtime/sys"
)

// VFSRequests is a service loop's request/reply state for owned VFS endpoints.
// The zero value is ready. It does not acquire, close or reconnect endpoints;
// its owner does so. A failed endpoint ID stays quarantined. Do not copy it.
// Callers serialize endpoint handoffs and asynchronous requests themselves.
type VFSRequests struct {
	client ownedVFS[nativeIPC, nativePaths]
}

// Call sends one exchange with a nonzero caller-owned correlation and an
// absolute time budget, including page preparation. Call takes ownership of
// attached immediately: it transfers the handle to VFS on successful send and
// closes it otherwise. The caller must not close it again. After failure the
// owner must retire any attached bulk mapping before reusing its bytes; paths
// are retired here. Success owns all reply slices. A valid error-status reply
// also completes the transaction. Control has different unsent-handle ownership.
func (v *VFSRequests) Call(channel handle.ID, request vfs.Request, attached handle.ID, correlation, budget uint64) (vfs.Reply, bool) {
	return v.client.callOn(channel, request, attached, correlation, budget)
}

// CallWithFailure is Call with a caller-owned failure snapshot. Validation
// callers log it after a failure; normal requests need no diagnostic storage.
func (v *VFSRequests) CallWithFailure(channel handle.ID, request vfs.Request, attached handle.ID, correlation, budget uint64, failure *VFSFailure) (vfs.Reply, bool) {
	return v.client.callObserved(channel, request, attached, correlation, budget, failure)
}

// Reject releases an unacceptable decoded reply's handles and quarantines
// its endpoint, including an asynchronously sent request. nil means the owner
// abandoned the request without receiving a reply. It does not revoke peer
// memory mappings or close the owner's channel handle.
func (v *VFSRequests) Reject(channel handle.ID, reply *ipc.Message) {
	v.client.rejectOn(channel, reply)
}

// DecodeIPC validates one kernel delivery window and closes handles from
// invalid IPC framing. For valid framing the receiver owns reply's handles,
// including handles in replies it later rejects at the protocol level.
func DecodeIPC(wire []byte, reply *ipc.Message) bool {
	return decodeIPC(wire, reply, (nativeIPC{}).close)
}

func (nativeIPC) now() (uint64, kcall.Status) { return sys.ClockRead() }

// Control exchanges a VFS control request for its status reply. sent reports
// attachment transfer even when a timeout or invalid reply follows. The owner
// closes unsent attachments; transferred ones belong to VFS. A failed control
// endpoint needs a genuine supervisor restart/handoff before reuse.
func (v *VFSRequests) Control(channel handle.ID, request *ipc.Message, budget uint64) (vfs.Status, kcall.Status, bool) {
	return v.client.controlOn(channel, request, budget)
}

// PollControl drains a control endpoint between exchanges, without waiting,
// and keeps the filesystem replacement notices it finds; everything else is
// discarded. It reports false for an endpoint quarantined by an earlier
// failure, which must not be waited on.
func (v *VFSRequests) PollControl(channel handle.ID) bool {
	return v.client.pollControlOn(channel)
}

// FilesystemLost returns, once, the attachment generation of the latest
// filesystem replacement notice received by Control or PollControl.
func (v *VFSRequests) FilesystemLost() (uint64, bool) {
	return v.client.takeLost()
}

// Complete validates an asynchronous request's reply under the same failure
// policy as Call. The owner retires the request's shared object on failure.
func (v *VFSRequests) Complete(channel handle.ID, request vfs.Request, correlation uint64, reply *ipc.Message, status kcall.Status) (vfs.Reply, bool) {
	return v.client.completeOn(channel, request, correlation, reply, status)
}
