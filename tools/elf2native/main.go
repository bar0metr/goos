package main

import (
	"debug/elf"
	"fmt"
	"goos/kernel/loader"
	"goos/kernel/mm/vm"
	"io"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: elf2native INPUT OUTPUT")
		os.Exit(2)
	}
	input, err := elf.Open(os.Args[1])
	if err != nil {
		fail(err)
	}
	defer input.Close()
	if input.Class != elf.ELFCLASS64 || input.Data != elf.ELFDATA2LSB || input.Machine != elf.EM_X86_64 || input.Type != elf.ET_EXEC {
		fail(fmt.Errorf("unsupported ELF format"))
	}
	symbols, err := input.Symbols()
	if err != nil {
		fail(err)
	}
	// The kernel provides the stack (ADR 0104); the program only says how
	// large a reservation it wants.
	var stackReserve uint64
	for _, symbol := range symbols {
		if symbol.Name == "_goos_stack_reserve" {
			stackReserve = symbol.Value
			break
		}
	}
	if stackReserve == 0 || stackReserve%vm.PageSize != 0 || stackReserve > loader.MaxStackReserve {
		fail(fmt.Errorf("missing or invalid _goos_stack_reserve"))
	}
	segments := make([]loader.SegmentSource, 0, loader.MaxSegments)
	for _, program := range input.Progs {
		if program.Type == elf.PT_INTERP || program.Type == elf.PT_DYNAMIC {
			fail(fmt.Errorf("dynamic ELF is not supported"))
		}
		if program.Type != elf.PT_LOAD || program.Memsz == 0 {
			continue
		}
		if len(segments) == loader.MaxSegments || program.Memsz > uint64(^uint32(0)) {
			fail(fmt.Errorf("unsupported load segments"))
		}
		payload, err := io.ReadAll(program.Open())
		if err != nil {
			fail(err)
		}
		// The loader zero-fills memory past the file bytes, so trailing zeros
		// such as the BSS need not be stored or copied.
		for len(payload) != 0 && payload[len(payload)-1] == 0 {
			payload = payload[:len(payload)-1]
		}
		flags := uint32(loader.SegmentReadable)
		if program.Flags&elf.PF_W != 0 {
			flags |= loader.SegmentWritable
		}
		if program.Flags&elf.PF_X != 0 {
			flags |= loader.SegmentExecutable
		}
		segments = append(segments, loader.SegmentSource{VirtualAddress: program.Vaddr, MemorySize: uint32(program.Memsz), Flags: flags, Data: payload})
	}
	storageSize := int(loader.NativeHeaderSize) + len(segments)*int(loader.SegmentHeaderSize)
	for _, segment := range segments {
		storageSize += len(segment.Data)
	}
	image, ok := loader.EncodeNative(make([]byte, storageSize), loader.ImageSource{Entry: input.Entry, StackReserve: stackReserve, Segments: segments})
	if !ok {
		fail(fmt.Errorf("native image encoding failed"))
	}
	if _, ok := loader.ParseNative(image, vm.PageSize, vm.UserAddressStart, vm.UserImageAreaLimit); !ok {
		fail(fmt.Errorf("ELF load layout is not a valid native image"))
	}
	if err := os.WriteFile(os.Args[2], image, 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
