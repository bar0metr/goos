package linux

import (
	"goos/internal/limits"
	"goos/kernel/compat/linux/abi"
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
	// Note is set once when the call begins and kept across every stage.
	Note       uint64
	path       [limits.PathMax]byte
	target     [limits.PathMax]byte
	pathSize   uint16
	targetSize uint16
}

func (state *Continuation) Path() []byte { return state.path[:state.pathSize] }

// ContinuationTable has one slot per scheduler slot. Full generation-safe task
// identity prevents a stale wake from attaching to a reused slot.
type ContinuationTable struct {
	entries [sched.MaxTasks]Continuation
}

var SyscallContinuations ContinuationTable

func (table *ContinuationTable) Begin(task sched.TaskID, request Request, path []byte) bool {
	slot, _, valid := task.Parts()
	if table == nil || !valid || len(path) > limits.PathMax || table.entries[slot].Task != 0 {
		return false
	}
	for _, character := range path {
		if character == 0 {
			return false
		}
	}
	// The record is filled in place. It is over 4 KiB, and a value that size
	// built on the way in would land on the kernel heap, which never frees.
	entry := &table.entries[slot]
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
}

func (table *ContinuationTable) Pending(task sched.TaskID) (Pending, bool) {
	entry, ok := table.entry(task)
	if !ok {
		return Pending{}, false
	}
	return Pending{Number: entry.Number, Arguments: entry.Arguments, Stage: entry.Stage, Phase: entry.Phase,
		Correlation: entry.Correlation, Partial: entry.Partial, Note: entry.Note, Wake: entry.Wake, Issued: entry.Issued}, true
}

// Annotate stores the second path of a two-path call and a note the handler
// needs at every stage. Both belong to the call, not to a stage, so they
// are set once, before the first request is issued.
func (table *ContinuationTable) Annotate(task sched.TaskID, target []byte, note uint64) bool {
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

// Paths returns the copied paths of an active call. The slices alias the
// table and are valid until the call finishes.
func (table *ContinuationTable) Paths(task sched.TaskID) ([]byte, []byte) {
	entry, ok := table.entry(task)
	if !ok {
		return nil, nil
	}
	return entry.path[:entry.pathSize], entry.target[:entry.targetSize]
}

// Issue marks the current stage's externally visible request before it is
// sent. Retrying the syscall entry cannot issue that stage twice.
func (table *ContinuationTable) Issue(task sched.TaskID, stage uint8, correlation uint64) bool {
	entry, ok := table.entry(task)
	if !ok || stage == 0 || correlation == 0 || entry.Stage != stage || entry.Phase != ContinuationReady || entry.Issued {
		return false
	}
	entry.Correlation, entry.Wake, entry.Issued = correlation, 0, true
	return true
}

// BlockContext parks a syscall before it issues a VFS request. The family
// context wakes it without a correlation because no request exists yet.
func (table *ContinuationTable) BlockContext(task sched.TaskID, stage uint8) bool {
	entry, ok := table.entry(task)
	if !ok || entry.Stage != stage || entry.Phase != ContinuationReady || entry.Issued || entry.Correlation != 0 {
		return false
	}
	entry.Phase = ContinuationWaiting
	return true
}

func (table *ContinuationTable) ResumeContext(task sched.TaskID) bool {
	entry, ok := table.entry(task)
	if !ok || entry.Phase != ContinuationWaiting || entry.Issued || entry.Correlation != 0 {
		return false
	}
	entry.Phase, entry.Wake = ContinuationReady, WakeContext
	return true
}

func (table *ContinuationTable) Block(task sched.TaskID, stage uint8) bool {
	entry, ok := table.entry(task)
	if !ok || entry.Stage != stage || entry.Phase != ContinuationReady || !entry.Issued || entry.Correlation == 0 {
		return false
	}
	entry.Phase = ContinuationWaiting
	return true
}

// BlockFrame couples persisted state to the architecture return contract. The
// assembly scheduler removes the task from the runnable set only after this
// transition succeeds.
func (table *ContinuationTable) BlockFrame(frame *abi.Frame, task sched.TaskID, stage uint8) bool {
	if frame == nil || !table.Block(task, stage) {
		return false
	}
	frame.Action = abi.ActionBlock
	return true
}

// Resume accepts only the exact in-flight correlation. A mismatched or late
// reply cannot advance the state machine or make a task runnable.
func (table *ContinuationTable) Resume(task sched.TaskID, correlation uint64, reason WakeReason) bool {
	entry, ok := table.entry(task)
	if !ok || correlation == 0 || reason < WakeMessage || reason > WakeTimeout && reason != WakeInterrupted ||
		entry.Phase != ContinuationWaiting || entry.Correlation != correlation {
		return false
	}
	entry.Phase, entry.Wake = ContinuationReady, reason
	return true
}

// Advance commits completion of one issued stage. The strictly increasing
// stage and one-shot Issued bit prevent duplicated effects after wake/retry.
func (table *ContinuationTable) Advance(task sched.TaskID, stage uint8, next uint8, partial uint64) bool {
	entry, ok := table.entry(task)
	if !ok || entry.Stage != stage || next <= stage || entry.Phase != ContinuationReady || !entry.Issued || entry.Wake == 0 {
		return false
	}
	entry.Stage, entry.Partial = next, partial
	entry.Correlation, entry.Wake, entry.Issued = 0, 0, false
	return true
}

func (table *ContinuationTable) Finish(task sched.TaskID) bool {
	entry, ok := table.entry(task)
	if !ok || entry.Phase != ContinuationReady {
		return false
	}
	*entry = Continuation{}
	return true
}

// Cancel is used by kill, fault, and process teardown. Peer death is first
// resumed as an exact correlated wake so the handler can return its error,
// then Finish clears the slot.
func (table *ContinuationTable) Cancel(task sched.TaskID) bool {
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
	if !valid || table.entries[slot].Task != task {
		return nil, false
	}
	return &table.entries[slot], true
}
