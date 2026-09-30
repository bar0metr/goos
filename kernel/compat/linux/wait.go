package linux

import (
	"goos/kernel/compat/linux/abi"
	"goos/kernel/sched"
)

// AnyChild is the wait target that matches every child.
const AnyChild = int32(-1)

// WaitRequest decodes the pid and options of wait4. A positive pid names one
// child and -1 names any child. Process-group targets, 0 and below -1, are
// EINVAL until GoOS has process groups. WUNTRACED and WCONTINUED report
// stopped and continued children, which never exist here, so they are EINVAL
// as well. Every other option bit is EINVAL, as on Linux.
func WaitRequest(pid uint64, options uint64) (target int32, noHang bool, errno abi.Errno) {
	signed := int64(int32(pid))
	if options&^(abi.WaitNoHang|abi.WaitNoThread|abi.WaitAll) != 0 || signed == 0 || signed < -1 {
		return 0, false, abi.EINVAL
	}
	return int32(signed), options&abi.WaitNoHang != 0, 0
}

// WaitableChild looks for a child of parentPID that target matches. It
// returns a zombie when one matches, and reports whether any child matches at
// all, running or not.
func WaitableChild(parentPID int32, target int32) (ProcessRecord, bool) {
	matched := false
	for index := range processRecords {
		record := processRecords[index]
		if record.State == 0 || record.ParentPID != parentPID || parentPID < firstPID || target != AnyChild && record.PID != target {
			continue
		}
		if record.State == ProcessZombie {
			return record, true
		}
		matched = true
	}
	return ProcessRecord{}, matched
}

// HoldChildWaiter records the task that wait4 blocked for a child of state.
func (state *ProcessState) HoldChildWaiter(task sched.TaskID) {
	state.childWaiter = task
}

// ReleaseChildWaiter returns the task blocked in wait4 by the process with
// PID parentPID, if any, and forgets it, so one child exit wakes it once.
func ReleaseChildWaiter(parentPID int32) sched.TaskID {
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
