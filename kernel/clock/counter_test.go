package clock

import "testing"

// readings returns counter values ticks PIT periods apart for a counter
// running at hz.
func readings(hz, ticks uint64) (uint64, uint64) {
	start := uint64(123456789)
	return start, start + hz*ticks*PITDivisor/PITInputHz
}

func TestCounterCalibratesAgainstPIT(t *testing.T) {
	for _, hz := range []uint64{24_000_000, 1_000_000_000, 3_600_000_000} {
		var counter Counter
		start, end := readings(hz, 10)
		if !counter.Calibrate(5, start, end, 10) {
			t.Fatalf("%d Hz: calibration rejected", hz)
		}
		if counter.Hz() < hz*999/1000 || counter.Hz() > hz*1001/1000 {
			t.Fatalf("%d Hz: measured %d Hz", hz, counter.Hz())
		}
		origin := TicksToNanos(5)
		if got := counter.Nanos(start); got != origin {
			t.Fatalf("%d Hz: base reads %d ns, want %d", hz, got, origin)
		}
		elapsed := counter.Nanos(start+counter.Hz()) - origin
		if elapsed != nanosPerSecond {
			t.Fatalf("%d Hz: one second of cycles reads %d ns", hz, elapsed)
		}
	}
}

func TestCounterRejectsImpossibleMeasurements(t *testing.T) {
	var counter Counter
	cases := []struct{ start, end, ticks uint64 }{
		{100, 200, 0},
		{200, 200, 10},
		{300, 200, 10},
		{0, ^uint64(0), 10},
		{0, 1, 1_000_000},
	}
	for _, c := range cases {
		if counter.Calibrate(0, c.start, c.end, c.ticks) || counter.Calibrated() {
			t.Fatalf("accepted start=%d end=%d ticks=%d", c.start, c.end, c.ticks)
		}
	}
	if counter.Nanos(1000) != 0 {
		t.Fatal("uncalibrated counter reported time")
	}
}

func TestCounterIsMonotonicAndDoesNotOverflow(t *testing.T) {
	var counter Counter
	start, end := readings(5_000_000_000, 100)
	if !counter.Calibrate(0, start, end, 100) {
		t.Fatal("calibration rejected")
	}
	if counter.Nanos(start-1) != 0 {
		t.Fatal("reading before the base went backwards")
	}
	previous := uint64(0)
	for _, delta := range []uint64{0, 1, 999, counter.Hz() - 1, counter.Hz(), 7 * counter.Hz() / 3} {
		got := counter.Nanos(start + delta)
		if got < previous {
			t.Fatalf("delta %d reads %d ns after %d ns", delta, got, previous)
		}
		previous = got
	}
	year := uint64(365 * 24 * 3600)
	if got := counter.Nanos(start + year*counter.Hz()); got != year*nanosPerSecond {
		t.Fatalf("one year reads %d ns", got)
	}
}

func TestTicksToNanosMatchesProgrammedRate(t *testing.T) {
	got := TicksToNanos(TicksPerSecond)
	if got < nanosPerSecond*999/1000 || got > nanosPerSecond*1001/1000 {
		t.Fatalf("%d ticks read %d ns", TicksPerSecond, got)
	}
}
