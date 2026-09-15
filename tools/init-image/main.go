package main

import (
	"fmt"
	"goos/kernel/loader"
	"goos/kernel/mm/vm"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: init-image OUTPUT")
		os.Exit(2)
	}
	program := []byte{
		0xb8, 0x02, 0, 0, 0,
		0x48, 0xbf, 0x00, 0x40, 0x00, 0x40, 0, 0, 0, 0,
		0xbe, 0x10, 0, 0, 0,
		0xcd, 0x80,
		0x85, 0xc0,
		0x75, 0x04,
		0xf3, 0x90,
		0xeb, 0xfc,
		0x0f, 0x0b,
	}
	data := []byte("INIT_PROCESS_OK\n")
	storage := make([]byte, 2048)
	image, ok := loader.EncodeNative(storage, loader.ImageSource{
		Entry:    vm.UserCodeAddress,
		StackTop: vm.UserStackAddress + vm.PageSize - 16,
		Segments: []loader.SegmentSource{
			{VirtualAddress: vm.UserCodeAddress, MemorySize: uint32(len(program)), Flags: loader.SegmentReadable | loader.SegmentExecutable, Data: program},
			{VirtualAddress: vm.UserStackAddress, MemorySize: uint32(vm.PageSize), Flags: loader.SegmentReadable | loader.SegmentWritable},
			{VirtualAddress: vm.UserDataAddress, MemorySize: uint32(len(data)), Flags: loader.SegmentReadable, Data: data},
		},
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "encode init image failed")
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[1], image, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
