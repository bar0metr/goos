//go:build uefi && amd64

package amd64

import "unsafe"

//export goos_has_rdrand
func hasRDRAND() bool

//export goos_rdrand64
func rdrand64(value unsafe.Pointer) bool

// randomProbed and randomPresent cache the CPUID answer, which cannot change
// while the system runs.
var randomProbed, randomPresent bool

// HasHardwareRandom reports whether the CPU has the RDRAND generator, the
// only entropy source the kernel trusts. Without it the kernel hands out no
// random bytes at all.
func HasHardwareRandom() bool {
	if !randomProbed {
		randomPresent, randomProbed = hasRDRAND(), true
	}
	return randomPresent
}

// FillRandom fills target from the hardware generator. It reports false when
// the CPU has none or the generator failed; target then holds nothing a
// caller may use.
func FillRandom(target []byte) bool {
	if !HasHardwareRandom() {
		return false
	}
	var value uint64
	for index := 0; index < len(target); index += 8 {
		if !rdrand64(unsafe.Pointer(&value)) {
			clear(target)
			return false
		}
		for offset := 0; offset < 8 && index+offset < len(target); offset++ {
			target[index+offset] = byte(value >> (8 * offset))
		}
	}
	return true
}
