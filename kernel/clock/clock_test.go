package clock

import "testing"

func TestDivisorMatchesConfiguredRate(t *testing.T) {
	if PITDivisor == 0 || PITDivisor > 0xffff {
		t.Fatalf("divisor %#x does not fit the 16-bit PIT reload register", PITDivisor)
	}
	rate := PITInputHz / PITDivisor
	if rate < TicksPerSecond*99/100 || rate > TicksPerSecond*101/100 {
		t.Fatalf("divisor %d yields %d Hz, want about %d Hz", PITDivisor, rate, TicksPerSecond)
	}
}
