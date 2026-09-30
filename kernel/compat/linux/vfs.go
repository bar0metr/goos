package linux

import (
	"goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/vfs"
)

// VFSPathInline is the longest path one VFS request carries in its message.
// The bridge sends a longer path in a memory object attached to the request,
// as native clients do.
const VFSPathInline = vfs.MaxProtocolData

// VFSSession is the one filesystem authority a Linux process holds: the
// channel PID 1 granted at launch. Every file operation is a request on this
// channel. A session whose peer died stays failed; nothing reconnects it, so
// a restarted VFS cannot hand the process authority PID 1 never granted.
type VFSSession struct {
	Handle      handle.ID
	correlation uint64
	failed      bool
}

// Next returns the correlation for a new request. Values rise from one and
// are never reused within a session, which lets a late reply to an abandoned
// request be told apart from the reply in flight.
func (session *VFSSession) Next() (uint64, bool) {
	if session == nil || session.Handle == 0 || session.failed || session.correlation == ^uint64(0) {
		return 0, false
	}
	session.correlation++
	return session.correlation, true
}

func (session *VFSSession) Fail() {
	if session != nil {
		session.failed = true
	}
}

func (session *VFSSession) Failed() bool { return session == nil || session.failed }

type VFSReplyVerdict uint8

const (
	// VFSReplyAccepted is the reply to the request in flight.
	VFSReplyAccepted VFSReplyVerdict = iota + 1
	// VFSReplyStale answers an earlier request of this session that the
	// caller stopped waiting for. It is dropped and the wait continues.
	VFSReplyStale
	// VFSReplyMalformed is anything else: a message that is not a VFS reply,
	// one for a request never sent, or one naming another operation.
	VFSReplyMalformed
)

// ClassifyVFSReply checks one message from the session channel against the
// request in flight. The reply aliases message, so message must outlive it.
func ClassifyVFSReply(message *ipc.Message, correlation uint64, operation vfs.Operation) (vfs.Reply, VFSReplyVerdict) {
	reply, valid := vfs.DecodeReplyFrom(message)
	if !valid || correlation == 0 || message.HandleCount != 0 || message.Correlation > correlation {
		return vfs.Reply{}, VFSReplyMalformed
	}
	if message.Correlation < correlation {
		return vfs.Reply{}, VFSReplyStale
	}
	if reply.Operation != operation {
		return vfs.Reply{}, VFSReplyMalformed
	}
	return reply, VFSReplyAccepted
}

// OpenRights decodes the openat flags the bridge accepts: an access mode,
// O_CLOEXEC, and O_LARGEFILE, which is always in effect on x86_64. Creation,
// truncation, append, and directory opens are refused until their semantics
// exist. A resolved path that demands a directory is refused the same way.
func OpenRights(flags uint64) (vfs.Rights, bool, abi.Errno) {
	value := uint32(flags)
	if value&^(abi.OpenAccess|abi.OpenLargeFile|abi.OpenCloseExec) != 0 {
		return 0, false, abi.EOPNOTSUPP
	}
	closeOnExec := value&abi.OpenCloseExec != 0
	switch value & abi.OpenAccess {
	case abi.OpenReadOnly:
		return vfs.Read, closeOnExec, 0
	case abi.OpenWriteOnly:
		return vfs.Write, closeOnExec, 0
	case abi.OpenReadWrite:
		return vfs.Read | vfs.Write, closeOnExec, 0
	}
	return 0, false, abi.EINVAL
}

// VFSDiagnostics counts bridge traffic. The counters saturate.
type VFSDiagnostics struct {
	Requests uint64
	Failures uint64
	Stale    uint64
}

var vfsDiagnostics VFSDiagnostics

func NoteVFSRequest() { saturate(&vfsDiagnostics.Requests) }
func NoteVFSFailure() { saturate(&vfsDiagnostics.Failures) }
func NoteVFSStale()   { saturate(&vfsDiagnostics.Stale) }

func VFSStatistics() VFSDiagnostics { return vfsDiagnostics }

func saturate(counter *uint64) {
	if *counter != ^uint64(0) {
		*counter++
	}
}
