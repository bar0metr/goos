package linux

import "goos/kernel/compat/linux/abi"

func signalTargetHeld(senderPID int32, targetPID int32) (ProcessRecord, abi.Errno) {
	guard.Check()
	sender, senderFound := findProcessRecordHeld(senderPID)
	target, targetFound := findProcessRecordHeld(targetPID)
	if !senderFound || !targetFound || target.FamilyID != sender.FamilyID {
		return ProcessRecord{}, abi.ESRCH
	}
	return target, 0
}

// SignalTarget checks that the process senderPID may signal targetPID: any
// process of the sender's family, its parent included, as a Linux process
// may signal any process of its own user. Linux identity grants nothing
// beyond the family. A process outside it is ESRCH, as if it did not exist,
// so a PID in another family reveals nothing. The returned record may be a
// zombie.
func SignalTarget(senderPID int32, targetPID int32) (ProcessRecord, abi.Errno) {
	tablesLock.Lock()
	result0, result1 := signalTargetHeld(senderPID, targetPID)
	tablesLock.Unlock()
	return result0, result1
}
