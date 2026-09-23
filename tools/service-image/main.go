package main

import (
	"fmt"
	"goos/kernel/loader"
	"goos/kernel/mm/vm"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: service-image OUTPUT")
		os.Exit(2)
	}
	storage := make([]byte, 256)
	image, ok := loader.EncodeNative(storage, loader.ImageSource{
		Entry:    vm.UserCodeAddress,
		StackTop: vm.UserStackAddress + vm.PageSize - 16,
		Segments: []loader.SegmentSource{
			// call .: recurse until the stack runs into its unmapped guard
			// page, so every run of the probe ends in a page fault (ADR 0083).
			{VirtualAddress: vm.UserCodeAddress, MemorySize: 5, Flags: loader.SegmentReadable | loader.SegmentExecutable, Data: []byte{0xe8, 0xfb, 0xff, 0xff, 0xff}},
			{VirtualAddress: vm.UserStackAddress, MemorySize: uint32(vm.PageSize), Flags: loader.SegmentReadable | loader.SegmentWritable},
		},
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "encode service image failed")
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[1], image, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
