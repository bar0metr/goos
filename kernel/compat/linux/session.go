package linux

import "goos/kernel/compat/linux/abi"

// Sessions and process groups as Linux job control sees them. Every Linux
// process has a SID and a PGID in its record. A process launched by PID 1
// leads a new session and group; fork copies both, exec keeps them, and a
// clone thread shares its process's record. No call reaches a process of
// another family, whatever its identities.

func setsidHeld(pid int32) (int32, abi.Errno) {
	guard.Check()
	index := recordIndex(pid)
	if index < 0 {
		return 0, abi.ESRCH
	}
	// A group leader cannot leave its group behind in another session.
	for other := range processRecords {
		if processRecords[other].State != 0 && processRecords[other].PGID == pid {
			return 0, abi.EPERM
		}
	}
	record := &processRecords[index]
	record.SID, record.PGID = pid, pid
	return pid, 0
}

// setpgidHeld moves target, the caller or one of its children, into group.
// A zero target is the caller and a zero group the target's own PID.
func setpgidHeld(caller int32, target int32, group int32) abi.Errno {
	guard.Check()
	if target < 0 || group < 0 {
		return abi.EINVAL
	}
	if target == 0 {
		target = caller
	}
	if group == 0 {
		group = target
	}
	callerRecord, callerFound := findProcessRecordHeld(caller)
	index := recordIndex(target)
	if !callerFound || index < 0 || processRecords[index].State != ProcessRunning ||
		processRecords[index].FamilyID != callerRecord.FamilyID {
		return abi.ESRCH
	}
	record := &processRecords[index]
	if target != caller {
		if record.ParentPID != caller {
			return abi.ESRCH
		}
		if record.Executed {
			return abi.EACCES
		}
	}
	if record.SID == target || record.SID != callerRecord.SID {
		return abi.EPERM
	}
	if group != target {
		found := false
		for other := range processRecords {
			candidate := &processRecords[other]
			if candidate.State != 0 && candidate.PGID == group && candidate.FamilyID == record.FamilyID {
				if candidate.SID != record.SID {
					return abi.EPERM
				}
				found = true
			}
		}
		if !found {
			return abi.EPERM
		}
	}
	record.PGID = group
	return 0
}

// identityOfHeld returns the group or session of target as getpgid and
// getsid report it. A process of another family does not exist for caller.
func identityOfHeld(caller int32, target int32, session bool) (int32, abi.Errno) {
	guard.Check()
	if target < 0 {
		return 0, abi.ESRCH
	}
	if target == 0 {
		target = caller
	}
	callerRecord, callerFound := findProcessRecordHeld(caller)
	record, found := findProcessRecordHeld(target)
	if !callerFound || !found || record.FamilyID != callerRecord.FamilyID {
		return 0, abi.ESRCH
	}
	if session {
		return record.SID, 0
	}
	return record.PGID, 0
}

// groupMembersHeld writes the PIDs of the running members of group in
// family to target and returns how many it wrote.
func groupMembersHeld(familyID uint64, group int32, target []int32) int {
	guard.Check()
	count := 0
	for index := range processRecords {
		record := &processRecords[index]
		if record.State == ProcessRunning && record.FamilyID == familyID && record.PGID == group && group > 0 && count < len(target) {
			target[count] = record.PID
			count++
		}
	}
	return count
}

// markStoppedHeld records that pid stopped on signal. It reports the parent
// to tell and whether the process was running before.
func markStoppedHeld(pid int32, signal uint64) (int32, bool) {
	guard.Check()
	index := recordIndex(pid)
	if index < 0 || processRecords[index].State != ProcessRunning || processRecords[index].Stopped {
		return 0, false
	}
	record := &processRecords[index]
	record.Stopped, record.StopSignal = true, uint8(signal)
	record.stopReport, record.continueReport = true, false
	return record.ParentPID, true
}

// markContinuedHeld records that a stopped pid runs again.
func markContinuedHeld(pid int32) (int32, bool) {
	guard.Check()
	index := recordIndex(pid)
	if index < 0 || processRecords[index].State != ProcessRunning || !processRecords[index].Stopped {
		return 0, false
	}
	record := &processRecords[index]
	record.Stopped, record.stopReport, record.continueReport = false, false, true
	return record.ParentPID, true
}

func markExecutedHeld(pid int32) {
	guard.Check()
	if index := recordIndex(pid); index >= 0 {
		processRecords[index].Executed = true
	}
}

// Setsid makes pid the leader of a new session and group with no
// controlling terminal, as setsid does.
func Setsid(pid int32) (int32, abi.Errno) {
	tablesLock.Lock()
	sid, errno := setsidHeld(pid)
	tablesLock.Unlock()
	return sid, errno
}

// Setpgid serves setpgid for caller.
func Setpgid(caller int32, target int32, group int32) abi.Errno {
	tablesLock.Lock()
	errno := setpgidHeld(caller, target, group)
	tablesLock.Unlock()
	return errno
}

// ProcessGroupOf serves getpgid, and getpgrp with a zero target.
func ProcessGroupOf(caller int32, target int32) (int32, abi.Errno) {
	tablesLock.Lock()
	value, errno := identityOfHeld(caller, target, false)
	tablesLock.Unlock()
	return value, errno
}

// SessionOf serves getsid.
func SessionOf(caller int32, target int32) (int32, abi.Errno) {
	tablesLock.Lock()
	value, errno := identityOfHeld(caller, target, true)
	tablesLock.Unlock()
	return value, errno
}

// GroupMembers lists the running members of a group in one family.
func GroupMembers(familyID uint64, group int32, target []int32) int {
	tablesLock.Lock()
	count := groupMembersHeld(familyID, group, target)
	tablesLock.Unlock()
	return count
}

// MarkStopped records a stop and returns the parent to notify.
func MarkStopped(pid int32, signal uint64) (int32, bool) {
	tablesLock.Lock()
	parent, changed := markStoppedHeld(pid, signal)
	tablesLock.Unlock()
	return parent, changed
}

// MarkContinued records that a stopped process continues.
func MarkContinued(pid int32) (int32, bool) {
	tablesLock.Lock()
	parent, changed := markContinuedHeld(pid)
	tablesLock.Unlock()
	return parent, changed
}

// MarkExecuted records that pid replaced its image.
func MarkExecuted(pid int32) {
	tablesLock.Lock()
	markExecutedHeld(pid)
	tablesLock.Unlock()
}

// IsStopped reports whether pid is held by a stop signal.
func IsStopped(pid int32) bool {
	tablesLock.Lock()
	record, found := findProcessRecordHeld(pid)
	tablesLock.Unlock()
	return found && record.Stopped
}

// groupOrphanedHeld reports whether group is orphaned as POSIX defines it:
// no running member has a parent in another group of the same session. A
// launch root's parent is PID 1, which belongs to no session.
func groupOrphanedHeld(familyID uint64, group int32) bool {
	guard.Check()
	for index := range processRecords {
		member := &processRecords[index]
		if member.State != ProcessRunning || member.FamilyID != familyID || member.PGID != group {
			continue
		}
		parent, found := findProcessRecordHeld(member.ParentPID)
		if found && parent.State == ProcessRunning && parent.PGID != group && parent.SID == member.SID {
			return false
		}
	}
	return true
}

// GroupOrphaned reports whether a group of one family is orphaned. Linux
// discards a terminal stop sent to such a group and fails its background
// terminal reads with EIO: nothing in the session could continue it.
func GroupOrphaned(familyID uint64, group int32) bool {
	tablesLock.Lock()
	orphaned := groupOrphanedHeld(familyID, group)
	tablesLock.Unlock()
	return orphaned
}

// StoppedOrphans writes to target the stopped running processes of a family
// whose parent is gone, so PID 1 adopted them, and returns how many.
func StoppedOrphans(familyID uint64, target []int32) int {
	tablesLock.Lock()
	count := 0
	for index := range processRecords {
		record := &processRecords[index]
		if record.State == ProcessRunning && record.Stopped && record.ParentPID == 1 && record.FamilyID == familyID && count < len(target) {
			target[count] = record.PID
			count++
		}
	}
	tablesLock.Unlock()
	return count
}
