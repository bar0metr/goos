package abi

// Frame is the architecture-neutral snapshot of one Linux x86_64 SYSCALL.
// The assembly entry fills it on the caller's guarded kernel-entry stack.
// Keep the explicit fields and offsets synchronized with kernel/arch/amd64/io.S.
type Frame struct {
	Number    uint64
	Arg1      uint64
	Arg2      uint64
	Arg3      uint64
	Arg4      uint64
	Arg5      uint64
	Arg6      uint64
	UserIP    uint64
	UserFlags uint64
	UserStack uint64
	Root      uint64
	Return    uint64
	Action    uint64
}

const (
	FrameNumberOffset    = 0
	FrameArg1Offset      = 8
	FrameArg2Offset      = 16
	FrameArg3Offset      = 24
	FrameArg4Offset      = 32
	FrameArg5Offset      = 40
	FrameArg6Offset      = 48
	FrameUserIPOffset    = 56
	FrameUserFlagsOffset = 64
	FrameUserStackOffset = 72
	FrameRootOffset      = 80
	FrameReturnOffset    = 88
	FrameActionOffset    = 96
	FrameSize            = 104

	ActionReturn = uint64(0)
	ActionExit   = uint64(1)
	ActionBlock  = uint64(2)
	ActionExec   = uint64(3)
	// ActionRetry runs the call again after a scheduling point; the kernel
	// gave it up before it changed anything.
	ActionRetry = uint64(4)
)

type Errno uint64

// Negative returns the Linux x86_64 failure convention as an unsigned register
// value. Errno zero is not a failure and is rejected rather than encoded.
func Negative(errno Errno) uint64 {
	if errno == 0 {
		return 0
	}
	return ^(uint64(errno) - 1)
}
