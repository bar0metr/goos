// Package clock names the bootstrap timer rate so tick-based deadlines keep
// their intended duration when the rate changes.
//
// The legacy PIT divides a fixed 1.193182 MHz input and is independent of CPU
// frequency, so the tick rate is a deliberate choice rather than a property of
// the machine. Every tick costs an interrupt, a context save, and a scheduler
// decision, so the rate trades responsiveness against that overhead. Deadlines
// name durations in terms of TicksPerSecond instead of raw tick counts.
package clock

const (
	// TicksPerSecond is the configured bootstrap timer rate.
	//
	// Measurement shows that 1 kHz makes persistent-storage commands three to
	// five times faster, because services wait for their peers with
	// tick-granularity polls. Raising the rate also exposes a lost wakeup in
	// process teardown: when a pipeline stage fails to launch, one surviving
	// stage is not woken by its peer's exit. The rate stays at 100 Hz until
	// that defect is fixed, so deadlines are expressed through this constant
	// and the change becomes a one-line edit.
	TicksPerSecond = 100
	// PITInputHz is the fixed oscillator feeding the legacy timer.
	PITInputHz = 1193182
	// PITDivisor is the reload value programmed by StartBootstrapTimer, rounded
	// to the nearest whole division. The assembly carries the same value as an
	// immediate, and a unit test keeps the two in step.
	PITDivisor = (PITInputHz + TicksPerSecond/2) / TicksPerSecond
)
