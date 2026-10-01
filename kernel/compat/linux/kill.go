package linux

import "goos/kernel/compat/linux/abi"

// maxAncestry bounds the parent links a descendant check follows. A family
// holds at most LinuxFamilyMaxProcesses processes.
const maxAncestry = LinuxFamilyMaxProcesses

// SignalTarget checks that the process senderPID may signal targetPID: the
// target is the sender or one of its descendants. Linux identity grants
// nothing here. A process outside the sender's family is ESRCH, as if it did
// not exist, so a PID in another family reveals nothing. A process of the same
// family that is no descendant, such as the sender's parent, is EPERM. The
// returned record may be a zombie.
func SignalTarget(senderPID int32, targetPID int32) (ProcessRecord, abi.Errno) {
	sender, senderFound := FindProcessRecord(senderPID)
	target, targetFound := FindProcessRecord(targetPID)
	if !senderFound || !targetFound || target.FamilyID != sender.FamilyID {
		return ProcessRecord{}, abi.ESRCH
	}
	current := target
	for step := 0; step <= maxAncestry; step++ {
		if current.PID == senderPID {
			return target, 0
		}
		parent, found := FindProcessRecord(current.ParentPID)
		if !found {
			break
		}
		current = parent
	}
	return ProcessRecord{}, abi.EPERM
}
