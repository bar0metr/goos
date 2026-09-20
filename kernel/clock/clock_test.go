package clock

import "testing"

// TestDivisorMatchesConfiguredRate keeps the reload value programmed in io.S
// and the constant every deadline is expressed in from drifting apart.
func TestDivisorMatchesConfiguredRate(t *testing.T) {
	if PITDivisor != 0x2e9c {
		t.Fatalf("divisor %#x does not match the immediate programmed in io.S", PITDivisor)
	}
	rate := PITInputHz / PITDivisor
	if rate < TicksPerSecond*95/100 || rate > TicksPerSecond*105/100 {
		t.Fatalf("divisor %d yields %d Hz, want about %d Hz", PITDivisor, rate, TicksPerSecond)
	}
}
