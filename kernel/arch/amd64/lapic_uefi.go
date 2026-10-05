//go:build uefi && amd64

package amd64

// Local APIC registers, as offsets in its MMIO page. Every processor sees its
// own local APIC at the same address.
const (
	lapicIDRegister   = 0x20
	lapicTPR          = 0x80
	lapicEOI          = 0xb0
	lapicSVR          = 0xf0
	lapicICRLow       = 0x300
	lapicICRHigh      = 0x310
	lapicLVTTimer     = 0x320
	lapicLVTLINT0     = 0x350
	lapicTimerInitial = 0x380
	lapicTimerCurrent = 0x390
	lapicTimerDivide  = 0x3e0

	lapicEnable        = 1 << 8
	lapicMasked        = 1 << 16
	lapicTimerPeriodic = 1 << 17
	// lapicDivideBy16 divides the timer's input clock by 16.
	lapicDivideBy16 = 0x3
	// icrPending is the delivery status: the last interrupt is still being
	// sent.
	icrPending = 1 << 12
	icrAssert  = 1 << 14
	icrInit    = 5 << 8
	icrStartup = 6 << 8
	icrSelf    = 1 << 18
	// icrPendingSpins bounds the wait for the previous interrupt to leave.
	icrPendingSpins = 1 << 20
)

func lapicRead(register uintptr) uint32 { return mmioRead32(lapicVirtual + register) }

func lapicWrite(register uintptr, value uint32) { mmioWrite32(lapicVirtual+register, value) }

// enableLocalAPIC software-enables the calling processor's local APIC with
// the spurious vector, accepts every interrupt priority, and masks LINT0,
// which only the bootstrap processor's firmware uses for the PIC.
func enableLocalAPIC() {
	lapicWrite(lapicSVR, lapicRead(lapicSVR)|lapicEnable|apicSpuriousVector)
	lapicWrite(lapicTPR, 0)
}

// LocalAPICID returns the calling processor's APIC ID.
func LocalAPICID() uint32 { return lapicRead(lapicIDRegister) >> 24 }

// sendInterrupt writes the interrupt command register. The caller has local
// interrupts disabled, so no handler on this processor writes it in between.
func sendInterrupt(apicID uint32, command uint32) bool {
	for spins := 0; lapicRead(lapicICRLow)&icrPending != 0; spins++ {
		if spins == icrPendingSpins {
			return false
		}
		cpuPause()
	}
	lapicWrite(lapicICRHigh, apicID<<24)
	lapicWrite(lapicICRLow, command)
	return true
}

// sendIPI sends vector to one processor.
func sendIPI(cpu int, vector uint8) {
	flags := DisableInterrupts()
	sendInterrupt(cpuAPIC[cpu], icrAssert|uint32(vector))
	RestoreInterrupts(flags)
}

// sendSelfIPI sends vector to the calling processor. It arrives once the
// processor enables interrupts.
func sendSelfIPI(vector uint8) {
	flags := DisableInterrupts()
	sendInterrupt(0, icrSelf|icrAssert|uint32(vector))
	RestoreInterrupts(flags)
}

// StopOtherProcessors parks every other processor after a panic.
func StopOtherProcessors() {
	if smpReady == 0 {
		return
	}
	for cpu := 0; cpu < MaxCPUs; cpu++ {
		if cpu != CPUIndex() && onlineMask()&(uint64(1)<<uint(cpu)) != 0 {
			sendIPI(cpu, stopVector)
		}
	}
}

// localTimerCount is the timer's initial count for one scheduler tick, in
// the divided APIC clock the bootstrap processor measured against the PIT.
var localTimerCount uint32

// StartLocalTimerCalibration starts the calling processor's APIC timer
// counting down, masked, from its maximum.
func StartLocalTimerCalibration() {
	lapicWrite(lapicTimerDivide, lapicDivideBy16)
	lapicWrite(lapicLVTTimer, lapicMasked|localTimerVector)
	lapicWrite(lapicTimerInitial, 0xffffffff)
}

// LocalTimerElapsed returns how far the calibration count has run.
func LocalTimerElapsed() uint32 { return 0xffffffff - lapicRead(lapicTimerCurrent) }

// SwitchToLocalTimer sets the tick length from a calibration of elapsed APIC
// counts over ticks PIT periods, stops the PIT, and starts the calling
// processor's periodic tick. It reports false, leaving the PIT running, when
// the measurement is unusable.
func SwitchToLocalTimer(elapsed uint32, ticks uint64) bool {
	if ticks == 0 || uint64(elapsed)/ticks == 0 || uint64(elapsed)/ticks > 0xffffffff {
		return false
	}
	localTimerCount = uint32(uint64(elapsed) / ticks)
	flags := DisableInterrupts()
	maskPIT()
	startLocalTimer()
	RestoreInterrupts(flags)
	return true
}

// startLocalTimer starts the calling processor's periodic scheduler tick.
// The local APIC timers of a machine share one clock, so the bootstrap
// processor's calibration serves every processor.
func startLocalTimer() {
	lapicWrite(lapicTimerDivide, lapicDivideBy16)
	lapicWrite(lapicLVTTimer, lapicTimerPeriodic|localTimerVector)
	lapicWrite(lapicTimerInitial, localTimerCount)
}

// maskPIT masks IRQ0 at the PIC, every other PIC line staying masked.
func maskPIT() {
	outb(0x21, 0xff)
}
