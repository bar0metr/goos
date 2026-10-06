package linux

import (
	"goos/kernel/ipc"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/kernel/vfs"
)

// VFSPathInline is the longest path one VFS request carries in its message.
// The bridge sends a longer path in a memory object attached to the request,
// as native clients do.
const VFSPathInline = vfs.MaxProtocolData

// VFSSession is the shared sequencing state for one family's VFS authority.
// Each process retains its own granted channel handle. A session whose peer
// died stays failed; nothing reconnects it, so a restarted VFS cannot replace
// the authority PID 1 granted.
type VFSSession struct {
	correlation uint64
	failed      bool
}

// Next returns the correlation for a new request. Values rise from one and
// are never reused within a session, which lets a late reply to an abandoned
// request be told apart from the reply in flight.
func (session *VFSSession) Next() (uint64, bool) {
	if session == nil || session.failed || session.correlation == ^uint64(0) {
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

type VFSAccess uint8

const (
	VFSAccessInvalid VFSAccess = iota
	VFSAccessAcquired
	VFSAccessQueued
)

// LinuxVFSContext serializes one family's filesystem exchanges. Each process
// keeps its own channel handle and I/O buffer; the context shares correlation,
// failure, and queue state.
type LinuxVFSContext struct {
	FamilyID   uint64
	References uint8
	Session    VFSSession
	Owner      sched.TaskID
	waiters    [LinuxFamilyMaxProcesses - 1]sched.TaskID
	waitCount  uint8
}

var vfsContexts [process.MaxProcesses]LinuxVFSContext

func installVFSContext(familyID uint64) bool {
	if familyID == 0 {
		return false
	}
	for index := range vfsContexts {
		if vfsContexts[index].FamilyID == familyID {
			return false
		}
	}
	for index := range vfsContexts {
		if vfsContexts[index].FamilyID == 0 {
			vfsContexts[index] = LinuxVFSContext{FamilyID: familyID, References: 1}
			return true
		}
	}
	return false
}

func RetainVFSContext(familyID uint64) bool {
	guard.Check()
	context, found := VFSContext(familyID)
	if !found || context.References >= LinuxFamilyMaxProcesses {
		return false
	}
	context.References++
	return true
}

func ReleaseVFSContext(familyID uint64) (final bool, released bool) {
	guard.Check()
	context, found := VFSContext(familyID)
	if !found || context.References == 0 {
		return false, false
	}
	context.References--
	if context.References == 0 {
		*context = LinuxVFSContext{}
		return true, true
	}
	return false, true
}

func VFSContext(familyID uint64) (*LinuxVFSContext, bool) {
	guard.Check()
	if familyID != 0 {
		for index := range vfsContexts {
			if vfsContexts[index].FamilyID == familyID {
				return &vfsContexts[index], true
			}
		}
	}
	return nil, false
}

func AcquireVFS(familyID uint64, task sched.TaskID) VFSAccess {
	guard.Check()
	context, found := VFSContext(familyID)
	if !found || context.Session.Failed() {
		return VFSAccessInvalid
	}
	if context.Owner == 0 || context.Owner == task {
		context.Owner = task
		return VFSAccessAcquired
	}
	for index := 0; index < int(context.waitCount); index++ {
		if context.waiters[index] == task {
			return VFSAccessQueued
		}
	}
	if task == 0 || int(context.waitCount) == len(context.waiters) {
		return VFSAccessInvalid
	}
	context.waiters[context.waitCount] = task
	context.waitCount++
	return VFSAccessQueued
}

func ReleaseVFS(familyID uint64, task sched.TaskID) (sched.TaskID, bool) {
	guard.Check()
	context, found := VFSContext(familyID)
	if !found || task == 0 || context.Owner != task {
		return 0, false
	}
	context.Owner = 0
	if context.waitCount == 0 {
		return 0, true
	}
	next := context.waiters[0]
	context.waitCount--
	copy(context.waiters[:], context.waiters[1:int(context.waitCount)+1])
	context.waiters[context.waitCount] = 0
	context.Owner = next
	return next, true
}

// CancelVFSTask removes a killed or failed task from an owner or waiter slot.
// It returns the next owner when cancellation transfers the context.
func CancelVFSTask(task sched.TaskID) sched.TaskID {
	guard.Check()
	if task == 0 {
		return 0
	}
	for contextIndex := range vfsContexts {
		context := &vfsContexts[contextIndex]
		if context.FamilyID == 0 {
			continue
		}
		if context.Owner == task {
			next, _ := ReleaseVFS(context.FamilyID, task)
			return next
		}
		for index := 0; index < int(context.waitCount); index++ {
			if context.waiters[index] != task {
				continue
			}
			context.waitCount--
			copy(context.waiters[index:], context.waiters[index+1:int(context.waitCount)+1])
			context.waiters[context.waitCount] = 0
			return 0
		}
	}
	return 0
}

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
