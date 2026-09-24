package main

import (
	"fmt"
	"goos/kernel/loader"
	"goos/kernel/mm/vm"
	"os"
)

// probeStackPages is the fault probe's stack: large enough that one 12 KiB
// frame fits and the next one does not.
const probeStackPages = 4

// probeProgram recurses through a function with a 12 KiB frame that stores to
// the lowest word of each new frame before calling itself again, the worst
// case for a frame larger than a page (ADR 0083, ADR 0088):
//
//	entry: call frame         ; the return address lands at top-24
//	       ud2
//	frame: sub  $0x3000, %rsp
//	       mov  %rax, (%rsp)  ; first access in the new frame
//	       call frame
//
// With the entry stack pointer 16 bytes below the top of a four-page stack, the
// first frame fits and the second one's store lands 8224 bytes below the stack,
// in the third page of the gap: past the single guard page ADR 0083 left, but
// inside the 64 KiB gap. Every run of the probe ends in that page fault.
var probeProgram = []byte{
	0xe8, 0x02, 0x00, 0x00, 0x00, // call frame
	0x0f, 0x0b, // ud2
	0x48, 0x81, 0xec, 0x00, 0x30, 0x00, 0x00, // frame: sub $0x3000, %rsp
	0x48, 0x89, 0x04, 0x24, // mov %rax, (%rsp)
	0xe8, 0xf0, 0xff, 0xff, 0xff, // call frame
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: service-image OUTPUT")
		os.Exit(2)
	}
	storage := make([]byte, 256)
	image, ok := loader.EncodeNative(storage, loader.ImageSource{
		Entry:    vm.UserCodeAddress,
		StackTop: vm.UserStackAddress + probeStackPages*vm.PageSize - 16,
		Segments: []loader.SegmentSource{
			{VirtualAddress: vm.UserCodeAddress, MemorySize: uint32(len(probeProgram)), Flags: loader.SegmentReadable | loader.SegmentExecutable, Data: probeProgram},
			{VirtualAddress: vm.UserStackAddress, MemorySize: uint32(probeStackPages * vm.PageSize), Flags: loader.SegmentReadable | loader.SegmentWritable},
		},
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "encode service image failed")
		os.Exit(1)
	}
	parsed, ok := loader.ParseNative(image, vm.PageSize, vm.IdentityMapLimit, vm.UserAddressLimit)
	if start, end, guarded := parsed.StackGuard(); !ok || !guarded || loader.KernelMapsUserRange(start, end) {
		fmt.Fprintln(os.Stderr, "service image is not a valid native image with a free stack guard")
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[1], image, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
