package linux

import (
	"goos/internal/limits"
	"goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/lock"
	"goos/kernel/objects"
	"goos/kernel/sched"
)

type ContinuationPhase uint8

const (
	ContinuationReady ContinuationPhase = iota + 1
	ContinuationWaiting
)

type WakeReason uint8

const (
	WakeMessage WakeReason = iota + 1
	WakePeerDied
	WakeCancelled
	// WakeTimeout ends a wait whose deadline passed with no reply.
	WakeTimeout
	// WakeContext grants a queued task ownership of its family VFS context.
	WakeContext
	// WakeInterrupted ends a wait because a signal with a handler became
	// deliverable.
	WakeInterrupted
)

// Continuation is the bounded kernel-owned state of one blocking Linux
// syscall. It stores only validated scalars and copied paths; it never keeps
// an unchecked user pointer to dereference after the task sleeps.
type Continuation struct {
	Task        sched.TaskID
	Number      abi.Syscall
	Arguments   [6]uint64
	Stage       uint8
	Phase       ContinuationPhase
	Correlation uint64
	Partial     uint64
	Wake        WakeReason
	Issued      bool
	Sent        bool
	// Note is set once when the call begins and kept across every stage.
	Note       uint64
	path       [limits.PathMax]byte
	target     [limits.PathMax]byte
	pathSize   uint16
	targetSize uint16
}

func (state *Continuation) Path() []byte { return state.path[:state.pathSize] }

// ContinuationTable has one slot per scheduler slot. Full generation-safe task
// identity prevents a stale wake from attaching to a reused slot. Its lock
// makes each method one step, so a wake from another processor lands before
// or after the owner's own step, never inside it.
//
// A slot's record takes its pages from storage the first time the slot's
// task blocks in a Linux call, and keeps them for every later task of the
// slot (ADR 0111): over 8 KiB for each of sched.MaxTasks slots would be
// static memory most slots never touch.
type ContinuationTable struct {
	lock    lock.Spin
	entries [sched.MaxTasks]*Continuation
	storage objects.Storage[Continuation]
}

// SetStorage installs where records are allocated. The kernel installs page
// storage at boot; without it, records come from the Go heap on a host.
func (table *ContinuationTable) SetStorage(storage objects.Storage[Continuation]) {
	table.storage = storage
}

func (set *ContinuationTable) take() {
	set.lock.SetLevel(lock.LevelWait)
	set.lock.Lock()
}

var SyscallContinuations ContinuationTable

func (table *ContinuationTable) beginHeld(task sched.TaskID, request Request, path []byte) bool {
	slot, _, valid := task.Parts()
	if table == nil || !valid || len(path) > limits.PathMax || table.entries[slot] != nil && table.entries[slot].Task != 0 {
		return false
	}
	for _, character := range path {
		if character == 0 {
			return false
		}
	}
	if table.entries[slot] == nil {
		record, ok := objects.Or(table.storage).New()
		if !ok {
			return false
		}
		table.entries[slot] = record
	}
	// The record is filled in place. It is over 4 KiB, and a value that size
	// built on the way in would land on the kernel heap, which never frees.
	entry := table.entries[slot]
	*entry = Continuation{}
	entry.Task, entry.Number, entry.Arguments = task, request.Number, request.Arguments
	entry.Stage, entry.Phase, entry.pathSize = 1, ContinuationReady, uint16(len(path))
	copy(entry.path[:], path)
	return true
}

// Pending is a continuation without its copied path. The full record is
// over 4 KiB, which a syscall handler should not copy onto the kernel stack.
type Pending struct {
	Number      abi.Syscall
	Arguments   [6]uint64
	Stage       uint8
	Phase       ContinuationPhase
	Correlation uint64
	Partial     uint64
	Note        uint64
	Wake        WakeReason
	Issued      bool
	Sent        bool
}

func (pending Pending) AbandonFailsVFS() bool {
	return (pending.Number == abi.SysOpen || pending.Number == abi.SysOpenat) &&
		(pending.Stage == OpenStageCleanup || pending.Stage == OpenStageOpen && pending.Sent) ||
		pending.Number == abi.SysExecve && pending.Stage == ExecStageOpen && pending.Sent
}

func (table *ContinuationTable) pendingHeld(task sched.TaskID) (Pending, bool) {
	entry, ok := table.entry(task)
	if !ok {
		return Pending{}, false
	}
	return Pending{Number: entry.Number, Arguments: entry.Arguments, Stage: entry.Stage, Phase: entry.Phase,
		Correlation: entry.Correlation, Partial: entry.Partial, Note: entry.Note, Wake: entry.Wake, Issued: entry.Issued, Sent: entry.Sent}, true
}

func (table *ContinuationTable) annotateHeld(task sched.TaskID, target []byte, note uint64) bool {
	entry, ok := table.entry(task)
	if !ok || len(target) > limits.PathMax || entry.Stage != 1 || entry.Issued {
		return false
	}
	for _, character := range target {
		if character == 0 {
			return false
		}
	}
	entry.targetSize, entry.Note = uint16(copy(entry.target[:], target)), note
	return true
}

// PathSnapshot owns copied paths; callers must not share it with another
// concurrent or nested syscall. The kernel supplies dedicated per-CPU storage.
type PathSnapshot struct {
	path, target         [limits.PathMax]byte
	pathSize, targetSize uint16
}

func (snapshot *PathSnapshot) Path() []byte   { return snapshot.path[:snapshot.pathSize] }
func (snapshot *PathSnapshot) Target() []byte { return snapshot.target[:snapshot.targetSize] }

func (table *ContinuationTable) snapshotHeld(task sched.TaskID, snapshot *PathSnapshot) (Pending, bool) {
	entry, ok := table.entry(task)
	if !ok || snapshot == nil {
		return Pending{}, false
	}
	pending, _ := table.pendingHeld(task)
	snapshot.pathSize, snapshot.targetSize = entry.pathSize, entry.targetSize
	copy(snapshot.path[:], entry.path[:entry.pathSize])
	copy(snapshot.target[:], entry.target[:entry.targetSize])
	return pending, true
}

func (table *ContinuationTable) issueHeld(task sched.TaskID, stage uint8, correlation uint64) bool {
	entry, ok := table.entry(task)
	if !ok || stage == 0 || correlation == 0 || entry.Stage != stage || entry.Phase != ContinuationReady || entry.Issued {
		return false
	}
	entry.Correlation, entry.Wake, entry.Issued = correlation, 0, true
	return true
}

func (table *ContinuationTable) blockContextHeld(task sched.TaskID, stage uint8) bool {
	entry, ok := table.entry(task)
	if !ok || entry.Stage != stage || entry.Phase != ContinuationReady || entry.Issued || entry.Correlation != 0 {
		return false
	}
	entry.Phase = ContinuationWaiting
	return true
}

func (table *ContinuationTable) resumeContextHeld(task sched.TaskID) bool {
	entry, ok := table.entry(task)
	if !ok || entry.Phase != ContinuationWaiting || entry.Issued || entry.Correlation != 0 {
		return false
	}
	entry.Phase, entry.Wake = ContinuationReady, WakeContext
	return true
}

func (table *ContinuationTable) blockHeld(task sched.TaskID, stage uint8) bool {
	entry, ok := table.entry(task)
	if !ok || entry.Stage != stage || entry.Phase != ContinuationReady || !entry.Issued || entry.Correlation == 0 {
		return false
	}
	entry.Phase = ContinuationWaiting
	return true
}

func (table *ContinuationTable) blockFrameHeld(frame *abi.Frame, task sched.TaskID, stage uint8) bool {
	if frame == nil || !table.blockHeld(task, stage) {
		return false
	}
	frame.Action = abi.ActionBlock
	return true
}

func (table *ContinuationTable) resumeHeld(task sched.TaskID, correlation uint64, reason WakeReason) bool {
	entry, ok := table.entry(task)
	if !ok || correlation == 0 || reason < WakeMessage || reason > WakeTimeout && reason != WakeInterrupted ||
		entry.Phase != ContinuationWaiting || entry.Correlation != correlation {
		return false
	}
	entry.Phase, entry.Wake = ContinuationReady, reason
	return true
}

func (table *ContinuationTable) advanceHeld(task sched.TaskID, stage uint8, next uint8, partial uint64) bool {
	entry, ok := table.entry(task)
	if !ok || entry.Stage != stage || next <= stage || entry.Phase != ContinuationReady || !entry.Issued || entry.Wake == 0 {
		return false
	}
	entry.Stage, entry.Partial = next, partial
	entry.Correlation, entry.Wake, entry.Issued, entry.Sent = 0, 0, false, false
	return true
}

func (table *ContinuationTable) rearmHeld(task sched.TaskID, partial uint64) bool {
	entry, ok := table.entry(task)
	if !ok || entry.Phase != ContinuationReady {
		return false
	}
	entry.Partial = partial
	entry.Correlation, entry.Wake, entry.Issued, entry.Sent = 0, 0, false, false
	return true
}

func (table *ContinuationTable) finishHeld(task sched.TaskID) bool {
	entry, ok := table.entry(task)
	if !ok || entry.Phase != ContinuationReady {
		return false
	}
	*entry = Continuation{}
	return true
}

func (table *ContinuationTable) cancelHeld(task sched.TaskID) bool {
	entry, ok := table.entry(task)
	if !ok {
		return false
	}
	*entry = Continuation{}
	return true
}

func (table *ContinuationTable) entry(task sched.TaskID) (*Continuation, bool) {
	if table == nil {
		return nil, false
	}
	slot, _, valid := task.Parts()
	if !valid || table.entries[slot] == nil || table.entries[slot].Task != task {
		return nil, false
	}
	return table.entries[slot], true
}

func (table *ContinuationTable) Begin(task sched.TaskID, request Request, path []byte) bool {
	table.take()
	result := table.beginHeld(task, request, path)
	table.lock.Unlock()
	return result
}

func (table *ContinuationTable) Pending(task sched.TaskID) (Pending, bool) {
	table.take()
	pending, active := table.pendingHeld(task)
	table.lock.Unlock()
	return pending, active
}

// Annotate stores the second path of a two-path call and a note the handler
// needs at every stage. Both belong to the call, not to a stage, so they
// are set once, before the first request is issued.
func (table *ContinuationTable) Annotate(task sched.TaskID, target []byte, note uint64) bool {
	table.take()
	result := table.annotateHeld(task, target, note)
	table.lock.Unlock()
	return result
}

// Snapshot copies the pending state and both paths under the same lock. The
// returned state and snapshot remain independent of cancellation and slot reuse.
func (table *ContinuationTable) Snapshot(task sched.TaskID, snapshot *PathSnapshot) (Pending, bool) {
	table.take()
	pending, ok := table.snapshotHeld(task, snapshot)
	table.lock.Unlock()
	return pending, ok
}

// Issue marks the current stage's externally visible request before it is
// sent. Retrying the syscall entry cannot issue that stage twice.
func (table *ContinuationTable) Issue(task sched.TaskID, stage uint8, correlation uint64) bool {
	table.take()
	result := table.issueHeld(task, stage, correlation)
	table.lock.Unlock()
	return result
}

// Send runs one externally visible send while the continuation is protected
// from cancellation. Its caller must already hold the channel lock: channel
// then wait is the kernel lock order. A successful send and its Sent bit are
// therefore one state transition: cancellation either removes the
// continuation first and send is not called, or observes Sent afterward.
func (table *ContinuationTable) Send(task sched.TaskID, stage uint8, correlation uint64, send func() bool) bool {
	table.take()
	entry, ok := table.entry(task)
	if ok && entry.Stage == stage && entry.Correlation == correlation && entry.Issued && !entry.Sent && send != nil && send() {
		entry.Sent = true
	} else {
		ok = false
	}
	table.lock.Unlock()
	return ok
}

// SendVFS sends under the continuation lock. The caller holds the channel
// lock, so cancellation observes either an unsent or a queued request.
func (table *ContinuationTable) SendVFS(task sched.TaskID, stage uint8, correlation uint64, channel *ipc.Channel, side uint8, message *ipc.Message, source, target *handle.Table, transfers []ipc.Transfer) (ipc.Status, bool) {
	table.take()
	entry, ok := table.entry(task)
	status := ipc.StatusNotFound
	if ok && entry.Stage == stage && entry.Correlation == correlation && entry.Issued && !entry.Sent && channel != nil {
		status = channel.SendFrom(side, message, source, target, transfers)
		ok = status == ipc.StatusOK
		if ok {
			entry.Sent = true
		}
	} else {
		ok = false
	}
	table.lock.Unlock()
	return status, ok
}

// Abandon returns and clears one continuation atomically. VFS cancellation
// uses the returned Sent state to decide whether an issued open may already
// have mutated the namespace and therefore requires session teardown.
func (table *ContinuationTable) Abandon(task sched.TaskID) (Pending, bool) {
	table.take()
	pending, active := table.pendingHeld(task)
	if active {
		table.cancelHeld(task)
	}
	table.lock.Unlock()
	return pending, active
}

// CancelKnown clears a continuation whose VFS outcome the kernel knows and
// reports whether it kept one instead. A sent open or exec open stays, Sent
// state included, for the task's exit delivery: closing the family session
// needs the owner's handle table, which a remote canceller does not hold.
func (table *ContinuationTable) CancelKnown(task sched.TaskID) bool {
	table.take()
	pending, active := table.pendingHeld(task)
	kept := active && pending.AbandonFailsVFS()
	if active && !kept {
		table.cancelHeld(task)
	}
	table.lock.Unlock()
	return kept
}

// BlockContext parks a syscall before it issues a VFS request. The family
// context wakes it without a correlation because no request exists yet.
func (table *ContinuationTable) BlockContext(task sched.TaskID, stage uint8) bool {
	table.take()
	result := table.blockContextHeld(task, stage)
	table.lock.Unlock()
	return result
}

func (table *ContinuationTable) ResumeContext(task sched.TaskID) bool {
	table.take()
	result := table.resumeContextHeld(task)
	table.lock.Unlock()
	return result
}

func (table *ContinuationTable) Block(task sched.TaskID, stage uint8) bool {
	table.take()
	result := table.blockHeld(task, stage)
	table.lock.Unlock()
	return result
}

// BlockFrame couples persisted state to the architecture return contract. The
// assembly scheduler removes the task from the runnable set only after this
// transition succeeds.
func (table *ContinuationTable) BlockFrame(frame *abi.Frame, task sched.TaskID, stage uint8) bool {
	table.take()
	result := table.blockFrameHeld(frame, task, stage)
	table.lock.Unlock()
	return result
}

// Resume accepts only the exact in-flight correlation. A mismatched or late
// reply cannot advance the state machine or make a task runnable.
func (table *ContinuationTable) Resume(task sched.TaskID, correlation uint64, reason WakeReason) bool {
	table.take()
	result := table.resumeHeld(task, correlation, reason)
	table.lock.Unlock()
	return result
}

// Advance commits completion of one issued stage. The strictly increasing
// stage and one-shot Issued bit prevent duplicated effects after wake/retry.
func (table *ContinuationTable) Advance(task sched.TaskID, stage uint8, next uint8, partial uint64) bool {
	table.take()
	result := table.advanceHeld(task, stage, next, partial)
	table.lock.Unlock()
	return result
}

func (table *ContinuationTable) AdvanceOpenCleanup(task sched.TaskID, handle uint64, errno abi.Errno) bool {
	table.take()
	result := errno != 0 && table.advanceHeld(task, OpenStageOpen, OpenStageCleanup, handle)
	if result {
		entry, _ := table.entry(task)
		entry.Note = uint64(errno)
	}
	table.lock.Unlock()
	return result
}

// Rearm ends one wait of a call that waits more than once at the same
// stage, a socket call, and records its progress. The call keeps its
// arguments and note; the next wait is issued afresh.
func (table *ContinuationTable) Rearm(task sched.TaskID, partial uint64) bool {
	table.take()
	result := table.rearmHeld(task, partial)
	table.lock.Unlock()
	return result
}

func (table *ContinuationTable) Finish(task sched.TaskID) bool {
	table.take()
	result := table.finishHeld(task)
	table.lock.Unlock()
	return result
}

// Cancel is used by kill, fault, and process teardown. Peer death is first
// resumed as an exact correlated wake so the handler can return its error,
// then Finish clears the slot.
func (table *ContinuationTable) Cancel(task sched.TaskID) bool {
	table.take()
	result := table.cancelHeld(task)
	table.lock.Unlock()
	return result
}
