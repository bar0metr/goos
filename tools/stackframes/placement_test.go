package main

import (
	"testing"

	"goos/kernel/mm/vm"
)

func TestKernelPlacement(t *testing.T) {
	if problems := checkKernelPlacement(vm.KernelLinkBase, vm.KernelImageMaxSize); len(problems) != 0 {
		t.Fatalf("an image of the largest size was refused: %v", problems)
	}
	if problems := checkKernelPlacement(vm.KernelLinkBase, vm.KernelImageMaxSize+vm.PageSize); len(problems) != 1 {
		t.Fatalf("an image one page too large gave %v", problems)
	}
	if problems := checkKernelPlacement(0x1000000, 0x1000); len(problems) != 1 {
		t.Fatalf("an image at another base gave %v", problems)
	}
}
