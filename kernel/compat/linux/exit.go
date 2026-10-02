package linux

// Termination is how a Linux process ended.
type Termination struct {
	Status   uint8
	Signal   int32
	CPUTicks uint64
}

// abnormalExit marks a kernel task exit code that is not an exit status. The
// low bits then carry the CPU exception vector of a fault, or a kernel reason:
// 9 for a kill and 5 for a task the kernel had to end.
const abnormalExit = uint64(1) << 63

// signalExit marks an abnormal exit whose low seven bits are the Linux signal
// that ended the process by its default action.
const signalExit = uint64(1) << 62

// AbnormalExit reports a task exit code that is no exit status: a fault, a
// kill, or a signal's default action.
func AbnormalExit(code uint64) bool { return code&abnormalExit != 0 }

// SignalExitCode is the task exit code of a process a signal ended.
func SignalExitCode(signal uint64) uint64 { return abnormalExit | signalExit | signal&0x7f }

// Linux signal numbers a termination can carry.
const (
	signalIllegal     = 4
	signalTrap        = 5
	signalBus         = 7
	signalFloatingPt  = 8
	signalKill        = 9
	signalSegmentFail = 11
)

// TerminationFromCode decodes a kernel task exit code. A normal exit keeps the
// low eight bits as its status. A fault becomes the signal Linux sends for
// that exception, and a kill or kernel termination becomes SIGKILL.
func TerminationFromCode(code uint64, cpuTicks uint64) Termination {
	if code&abnormalExit == 0 {
		return Termination{Status: uint8(code), CPUTicks: cpuTicks}
	}
	if code&signalExit != 0 {
		return Termination{Signal: int32(code & 0x7f), CPUTicks: cpuTicks}
	}
	signal := int32(signalKill)
	switch code &^ abnormalExit {
	case 0, 16, 19:
		signal = signalFloatingPt
	case 1, 3:
		signal = signalTrap
	case 6:
		signal = signalIllegal
	case 4, 10, 13, 14:
		signal = signalSegmentFail
	case 11, 12, 17:
		signal = signalBus
	}
	return Termination{Signal: signal, CPUTicks: cpuTicks}
}
