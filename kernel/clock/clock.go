// Package clock names the bootstrap timer rate so tick-based deadlines keep
// their intended duration when the rate changes.
//
// The legacy PIT divides a fixed 1.193182 MHz input and is independent of CPU
// frequency, so the tick rate is a deliberate choice rather than a property of
// the machine. The rate is selected at build time with the goos_hz250 or
// goos_hz1000 tag (make HZ=250, make HZ=1000); without either tag it is 100 Hz.
package clock

const (
	// TicksPerSecond is the configured bootstrap timer rate.
	TicksPerSecond = rate
	// PITInputHz is the fixed oscillator feeding the legacy timer.
	PITInputHz = 1193182
	// PITDivisor is the reload value programmed by StartBootstrapTimer, rounded
	// to the nearest whole division.
	PITDivisor = (PITInputHz + TicksPerSecond/2) / TicksPerSecond
)
