package linux

import (
	"goos/kernel/compat/linux/abi"
	"goos/kernel/sched"
)

// AnyChild is the wait target that matches every child.
const AnyChild = int32(-1)

// WaitOptions are the wait4 options that change what it reports.
type WaitOptions struct {
	NoHang bool
	// Untraced and Continued also report a child that stopped or continued
	// since the parent last heard of it.
	Untraced, Continued bool
}

// WaitRequest decodes the pid and options of wait4. A positive pid names one
// child, -1 any child, 0 any child in the caller's process group, and a pid
// below -1 any child in group -pid. Every option bit other than WNOHANG,
// WUNTRACED, WCONTINUED, __WALL, and __WNOTHREAD is EINVAL, as on Linux.
func WaitRequest(pid uint64, options uint64) (target int32, decoded WaitOptions, errno abi.Errno) {
	signed := int64(int32(pid))
	if options&^(abi.WaitNoHang|abi.WaitUntraced|abi.WaitContinued|abi.WaitNoThread|abi.WaitAll) != 0 || signed == int64(-1<<31) {
		return 0, WaitOptions{}, abi.EINVAL
	}
	decoded = WaitOptions{NoHang: options&abi.WaitNoHang != 0, Untraced: options&abi.WaitUntraced != 0, Continued: options&abi.WaitContinued != 0}
	return int32(signed), decoded, 0
}

// waitMatches reports whether child is one the target of a wait4 by parent
// selects.
func waitMatches(parent ProcessRecord, child ProcessRecord, target int32) bool {
	switch {
	case target == AnyChild:
		return true
	case target > 0:
		return child.PID == target
	case target == 0:
		return child.PGID == parent.PGID
	}
	return child.PGID == -target
}

// waitableChildHeld returns the first child target selects that has
// something to report: an exit, or with the options a stop or a
// continuation. matched reports whether target selects any child at all.
func waitableChildHeld(parentPID int32, target int32, options WaitOptions) (ProcessRecord, bool) {
	guard.Check()
	parent, parentFound := findProcessRecordHeld(parentPID)
	if !parentFound {
		return ProcessRecord{}, false
	}
	matched := false
	for index := range processRecords {
		record := processRecords[index]
		if record.State == 0 || record.ParentPID != parentPID || !waitMatches(parent, record, target) {
			continue
		}
		if record.State == ProcessZombie ||
			options.Untraced && record.stopReport || options.Continued && record.continueReport {
			return record, true
		}
		matched = true
	}
	return ProcessRecord{}, matched
}

// takeChildReportHeld consumes the stop or continuation wait4 reports for
// child and returns the record with it.
func takeChildReportHeld(parentPID int32, childPID int32) (ProcessRecord, bool) {
	guard.Check()
	index := recordIndex(childPID)
	if index < 0 || processRecords[index].ParentPID != parentPID || processRecords[index].State != ProcessRunning {
		return ProcessRecord{}, false
	}
	record := &processRecords[index]
	reported := *record
	if !record.stopReport && !record.continueReport {
		return ProcessRecord{}, false
	}
	record.stopReport, record.continueReport = false, false
	return reported, true
}

// TakeChildReport consumes the stop or continuation of a running child.
func TakeChildReport(parentPID int32, childPID int32) (ProcessRecord, bool) {
	tablesLock.Lock()
	record, ok := takeChildReportHeld(parentPID, childPID)
	tablesLock.Unlock()
	return record, ok
}

func (state *ProcessState) holdChildWaiterHeld(task sched.TaskID) {
	guard.Check()
	state.childWaiter = task
}

func releaseChildWaiterHeld(parentPID int32) sched.TaskID {
	guard.Check()
	if parentPID < firstPID {
		return 0
	}
	for index := range processStates {
		state := &processStates[index]
		if state.Root != 0 && state.PID == parentPID {
			task := state.childWaiter
			state.childWaiter = 0
			return task
		}
	}
	return 0
}

// WaitableChild looks for a child of parentPID that target matches. It
// returns a zombie when one matches, and reports whether any child matches at
// all, running or not.
func WaitableChild(parentPID int32, target int32, options WaitOptions) (ProcessRecord, bool) {
	tablesLock.Lock()
	result0, result1 := waitableChildHeld(parentPID, target, options)
	tablesLock.Unlock()
	return result0, result1
}

// HoldChildWaiter records the task that wait4 blocked for a child of state.
func (state *ProcessState) HoldChildWaiter(task sched.TaskID) {
	tablesLock.Lock()
	state.holdChildWaiterHeld(task)
	tablesLock.Unlock()
}

// ReleaseChildWaiter returns the task blocked in wait4 by the process with
// PID parentPID, if any, and forgets it, so one child exit wakes it once.
func ReleaseChildWaiter(parentPID int32) sched.TaskID {
	tablesLock.Lock()
	result := releaseChildWaiterHeld(parentPID)
	tablesLock.Unlock()
	return result
}
