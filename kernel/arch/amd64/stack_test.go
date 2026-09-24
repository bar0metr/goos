package amd64

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// goos_reclaim_exited_task finds a dead task's stack allocation from its stack
// top; the size it subtracts must be the allocation the Go side makes.
func TestReclaimUsesTheStackAllocationSize(t *testing.T) {
	source, err := os.ReadFile("io.S")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`goos_reclaim_exited_task:[^:]*?addq \$64, %rax\s+subq \$(\d+), %rax`).FindSubmatch(source)
	if match == nil {
		t.Fatal("io.S: goos_reclaim_exited_task no longer recomputes the allocation start")
	}
	size, err := strconv.ParseUint(string(match[1]), 10, 64)
	if want := kernelStackPages*4096 + KernelStackGuardSize; err != nil || size != want {
		t.Fatalf("io.S subtracts %s, the allocation is %d bytes", match[1], want)
	}
	if KernelStackGuardSize%4096 != 0 {
		t.Fatal("the kernel stack guard is not whole pages")
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
