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
			{VirtualAddress: vm.UserCodeAddress, MemorySize: 2, Flags: loader.SegmentReadable | loader.SegmentExecutable, Data: []byte{0x0f, 0x0b}},
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
