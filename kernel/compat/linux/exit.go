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
