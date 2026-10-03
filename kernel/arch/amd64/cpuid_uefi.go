//go:build uefi && amd64

package amd64

import "goos/kernel/cpu/x86"

//export goos_cpuid
func cpuid(leaf, subleaf uint32, target *x86.Registers)

// CPUID executes one bounded leaf/subleaf query. The caller supplies no raw
// pointer; register results are returned by value.
func CPUID(leaf, subleaf uint32) x86.Registers {
	var registers x86.Registers
	cpuid(leaf, subleaf, &registers)
	return registers
}
