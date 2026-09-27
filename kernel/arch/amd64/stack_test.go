package amd64

import "testing"

func TestKernelStackLayout(t *testing.T) {
	if KernelStackGuardSize%4096 != 0 {
		t.Fatal("the kernel stack guard is not whole pages")
	}
	if kernelStackPages != 4 {
		t.Fatalf("kernel stack grew to %d pages", kernelStackPages)
	}
}

// The boot stack's guard must be whole pages and at least as wide as the
// other kernel stacks' guard, which bounds every function the boot task calls.
func TestBootStackLayout(t *testing.T) {
	if BootStackSize%4096 != 0 || BootStackGuardSize%4096 != 0 || BootStackGuardSize < KernelStackGuardSize {
		t.Fatalf("boot stack %d bytes with a %d-byte guard", BootStackSize, BootStackGuardSize)
	}
	if faultStackPages == 0 {
		t.Fatal("the double-fault stack has no pages")
	}
}
