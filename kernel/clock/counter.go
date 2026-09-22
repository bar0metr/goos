package clock

const nanosPerSecond = 1_000_000_000

// maxCounterHz keeps the remainder term of Nanos within 64 bits.
const maxCounterHz = ^uint64(0) / nanosPerSecond

// Counter converts readings of a free-running cycle counter, such as the x86
// TSC, into nanoseconds. Its rate is measured against the PIT, whose input is
// fixed, so the conversion does not depend on the CPU model or the emulator.
type Counter struct {
	base   uint64
	origin uint64
	hz     uint64
}

// Calibrate derives the counter rate from readings start and end taken a
// whole number of PIT ticks apart, the first at tick startTick. It rejects a
// measurement that cannot describe a real counter.
func (counter *Counter) Calibrate(startTick, start, end, ticks uint64) bool {
	if ticks == 0 || end <= start || end-start > ^uint64(0)/PITInputHz {
		return false
	}
	hz := (end - start) * PITInputHz / (ticks * PITDivisor)
	if hz == 0 || hz > maxCounterHz {
		return false
	}
	*counter = Counter{base: start, origin: TicksToNanos(startTick), hz: hz}
	return true
}

func (counter *Counter) Calibrated() bool { return counter.hz != 0 }

func (counter *Counter) Hz() uint64 { return counter.hz }

// Nanos returns the nanoseconds since the timer started for a counter reading.
// Readings older than the calibration base report the base time.
func (counter *Counter) Nanos(now uint64) uint64 {
	if counter.hz == 0 {
		return 0
	}
	if now < counter.base {
		return counter.origin
	}
	delta := now - counter.base
	return counter.origin + delta/counter.hz*nanosPerSecond + delta%counter.hz*nanosPerSecond/counter.hz
}

// TicksToNanos converts PIT ticks at the programmed divisor into nanoseconds.
func TicksToNanos(ticks uint64) uint64 {
	cycles := ticks * PITDivisor
	return cycles/PITInputHz*nanosPerSecond + cycles%PITInputHz*nanosPerSecond/PITInputHz
}
