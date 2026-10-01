package linux

import (
	"encoding/binary"

	"goos/kernel/compat/linux/abi"
)

// The x86_64 rt_sigframe a handler starts on: the restorer address its
// return pops, then struct ucontext, then struct siginfo. GoOS writes no
// floating-point state, so uc_mcontext.fpstate is null.
const (
	signalFrameUContext = 8
	// UContextSize is struct ucontext as the kernel writes it: flags, link,
	// stack_t, the 256-byte sigcontext, and an 8-byte signal mask.
	UContextSize      = 304
	signalFrameInfo   = signalFrameUContext + UContextSize
	signalInfoSize    = 128
	SignalFrameSize   = signalFrameInfo + signalInfoSize
	ucontextStack     = 16
	ucontextMcontext  = 40
	ucontextSignalSet = 296
	// signalRedZone is the area below a stack pointer that leaf code may use
	// without moving it. A frame never overlaps it.
	signalRedZone = 128
	// ucontextFlags are UC_SIGCONTEXT_SS and UC_STRICT_RESTORE_SS, which
	// Linux sets on every frame.
	ucontextFlags    = 0x6
	stackDisabled    = 2
	userCodeSelector = 0x33
	userDataSelector = 0x2b
)

// Positions of the registers in sigcontext and mcontext_t gregs.
const (
	RegR8 = iota
	RegR9
	RegR10
	RegR11
	RegR12
	RegR13
	RegR14
	RegR15
	RegRDI
	RegRSI
	RegRBP
	RegRBX
	RegRDX
	RegRAX
	RegRCX
	RegRSP
	RegRIP
	RegRFlags
	contextRegisters
)

// UserContext is the user register state a signal interrupts and
// rt_sigreturn resumes.
type UserContext struct {
	Registers [contextRegisters]uint64
}

// SignalFrameAddress places a frame below the interrupted stack and its red
// zone, aligned as a function entry expects: 8 bytes past a 16-byte boundary.
// It reports false when the stack is too low to hold one.
func SignalFrameAddress(stack uint64, lowest uint64) (uint64, bool) {
	if stack < lowest+signalRedZone+SignalFrameSize+24 {
		return 0, false
	}
	return (stack-signalRedZone-SignalFrameSize)&^15 - 8, true
}

// EncodeSignalFrame writes the frame for signal at base. The handler's
// return pops restorer; context and oldMask are what rt_sigreturn restores.
func EncodeSignalFrame(frame *[SignalFrameSize]byte, signal uint64, info SignalInfo, context *UserContext, oldMask uint64, restorer uint64) {
	clear(frame[:])
	binary.LittleEndian.PutUint64(frame[0:8], restorer)
	uc := frame[signalFrameUContext:signalFrameInfo]
	binary.LittleEndian.PutUint64(uc[0:8], ucontextFlags)
	binary.LittleEndian.PutUint32(uc[ucontextStack+8:ucontextStack+12], stackDisabled)
	for index, value := range context.Registers[:] {
		binary.LittleEndian.PutUint64(uc[ucontextMcontext+index*8:], value)
	}
	selectors := uint64(userCodeSelector) | uint64(userDataSelector)<<48
	binary.LittleEndian.PutUint64(uc[ucontextMcontext+contextRegisters*8:], selectors)
	// err, trapno, oldmask, cr2, fpstate, and the reserved words follow.
	binary.LittleEndian.PutUint64(uc[ucontextMcontext+(contextRegisters+3)*8:], oldMask)
	binary.LittleEndian.PutUint64(uc[ucontextSignalSet:], oldMask)
	siginfo := frame[signalFrameInfo:]
	binary.LittleEndian.PutUint32(siginfo[0:4], uint32(signal))
	binary.LittleEndian.PutUint32(siginfo[8:12], uint32(info.Code))
	binary.LittleEndian.PutUint32(siginfo[16:20], uint32(info.PID))
	binary.LittleEndian.PutUint32(siginfo[20:24], info.UID)
	if signal == abi.SignalChild {
		binary.LittleEndian.PutUint32(siginfo[24:28], uint32(info.Status))
	}
}

// SignalFrameUContext and SignalFrameInfo are the addresses a handler gets in
// RDX and RSI for a frame at base.
func SignalFrameUContext(base uint64) uint64 { return base + signalFrameUContext }
func SignalFrameInfo(base uint64) uint64     { return base + signalFrameInfo }

// DecodeSignalContext reads the registers and signal mask rt_sigreturn
// restores from a copied struct ucontext. The caller validates them: the
// frame is user memory and may hold anything.
func DecodeSignalContext(uc *[UContextSize]byte) (UserContext, uint64) {
	var context UserContext
	for index := range context.Registers {
		context.Registers[index] = binary.LittleEndian.Uint64(uc[ucontextMcontext+index*8:])
	}
	return context, binary.LittleEndian.Uint64(uc[ucontextSignalSet:])
}
