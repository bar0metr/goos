package config

import "testing"

func TestParseUintRefusesOverflow(t *testing.T) {
	for text, want := range map[string]uint64{"0": 0, "64": 64, "18446744073709551615": ^uint64(0)} {
		if got, ok := ParseUint([]byte(text)); !ok || got != want {
			t.Fatalf("%s: got %d %v", text, got, ok)
		}
	}
	// Each of these would wrap in unchecked uint64 arithmetic.
	for _, text := range []string{"18446744073709551616", "18446744073709551624", "99999999999999999999",
		"", "123456789012345678901", "-1", "1x"} {
		if got, ok := ParseUint([]byte(text)); ok {
			t.Fatalf("%q accepted as %d", text, got)
		}
	}
}
