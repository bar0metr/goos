package main

import (
	"debug/pe"
	"encoding/binary"
	"fmt"
	"sort"

	"goos/kernel/arch/amd64"
)

// The kernel is built for x86_64-unknown-windows-gnu, where LLVM allocates
// every frame of 4 KiB or more through a stack probe:
//
//	mov  $size, %eax
//	call ___chkstk_ms
//	sub  %rax, %rsp
//
// A real probe touches each page of the new frame from the top, so a one-page
// guard would catch any frame. TinyGo's UEFI runtime, however, defines
// ___chkstk_ms as a bare `ret` (src/device/uefi/asm_amd64.S), and it cannot be
// replaced without a duplicate symbol. The probe is therefore ignored and the
// frame is bounded like a userspace one: after the prologue's pushes, which
// write at the stack pointer, the frame allocation is the only move of the
// stack pointer that writes nothing, and the next write can be a call's
// return address just below it. Win64 has no red zone.
const (
	kernelMargin     = returnAddressSize
	kernelFrameLimit = int64(amd64.KernelStackGuardSize) - kernelMargin
)

// bootStackFrameLimit bounds the frames of the functions that run only on the
// boot task's stack, whose guard is wider than the other kernel stacks'.
const bootStackFrameLimit = int64(amd64.BootStackGuardSize) - kernelMargin

// bootStackFunctions may exceed the kernel limit, up to bootStackFrameLimit,
// because they run only on the boot task, whose stack has a guard of
// amd64.BootStackGuardSize (ADR 0091): goos_boot_continue is its entry on
// that stack, main.serviceKernel is the service loop it ends in, and
// main.runKernelSelfTests, in the validation build, is called from it.
// main.main runs before the switch, on the goroutine stack TinyGo allocates,
// which has no guard; it is bounded like any other kernel function and does
// little enough to stay far below that stack's 64 KiB.
var bootStackFunctions = map[string]bool{
	"goos_boot_continue":      true,
	"main.serviceKernel":      true,
	"main.runKernelSelfTests": true,
}

// kernelFrame is one frame allocation found in the kernel's code.
type kernelFrame struct {
	address uint64
	size    int64
	probed  bool
}

// scanFrames finds the frame allocations in code loaded at base: every
// `sub $imm32, %rsp` and every probe sequence calling probe. Smaller frames
// use `sub $imm8, %rsp`, which cannot exceed 127 bytes. The scan works on
// bytes rather than on a disassembly, so it may also report an immediate that
// merely looks like a frame; that can fail the build, never pass a frame.
func scanFrames(code []byte, base uint64, probe uint64) ([]kernelFrame, []string) {
	var frames []kernelFrame
	var problems []string
	for offset := 0; offset < len(code); offset++ {
		if offset+7 <= len(code) && code[offset] == 0x48 && code[offset+1] == 0x81 && code[offset+2] == 0xec {
			size := int64(int32(binary.LittleEndian.Uint32(code[offset+3:])))
			frames = append(frames, kernelFrame{address: base + uint64(offset), size: size})
		}
		if probe == 0 || code[offset] != 0xe8 || offset+5 > len(code) {
			continue
		}
		next := base + uint64(offset) + 5
		if next+uint64(int64(int32(binary.LittleEndian.Uint32(code[offset+1:])))) != probe {
			continue
		}
		if offset < 5 || code[offset-5] != 0xb8 || offset+8 > len(code) ||
			code[offset+5] != 0x48 || code[offset+6] != 0x29 || code[offset+7] != 0xc4 {
			problems = append(problems, fmt.Sprintf("call to ___chkstk_ms at %#x is not the mov/call/sub sequence of a frame allocation", base+uint64(offset)))
			continue
		}
		size := int64(binary.LittleEndian.Uint32(code[offset-4:]))
		frames = append(frames, kernelFrame{address: base + uint64(offset-5), size: size, probed: true})
	}
	return frames, problems
}

// symbol is a named address in the kernel's code.
type symbol struct {
	name    string
	address uint64
}

// owner returns the symbol with the highest address at or below address.
// symbols must be sorted by address.
func owner(symbols []symbol, address uint64) string {
	position := sort.Search(len(symbols), func(index int) bool { return symbols[index].address > address })
	if position == 0 {
		return fmt.Sprintf("%#x", address)
	}
	return symbols[position-1].name
}

func checkKernelFrames(frames []kernelFrame, symbols []symbol) report {
	result := report{limit: kernelFrameLimit, largest: -1}
	for _, frame := range frames {
		name := owner(symbols, frame.address)
		kind := "frame"
		if frame.probed {
			kind = "probed frame"
		}
		result.frames = append(result.frames, namedFrame{name: fmt.Sprintf("%s (%s at %#x)", name, kind, frame.address), size: frame.size})
		if bootStackFunctions[name] {
			if frame.size > bootStackFrameLimit {
				result.problems = append(result.problems, fmt.Sprintf("%s: %s of %d bytes at %#x exceeds %d bytes (the %d-byte boot stack guard less the %d-byte return address)",
					name, kind, frame.size, frame.address, bootStackFrameLimit, amd64.BootStackGuardSize, kernelMargin))
			}
			continue
		}
		if frame.size > result.largest {
			result.largest, result.function = frame.size, name
		}
		if frame.size > kernelFrameLimit {
			result.problems = append(result.problems, fmt.Sprintf(
				"%s: %s of %d bytes at %#x exceeds %d bytes (a %d-byte kernel stack guard less the %d-byte return address), and ___chkstk_ms does not probe",
				name, kind, frame.size, frame.address, kernelFrameLimit, amd64.KernelStackGuardSize, kernelMargin))
		}
	}
	return result
}

func checkKernel(path string) (report, error) {
	file, err := pe.Open(path)
	if err != nil {
		return report{}, err
	}
	defer file.Close()
	header, ok := file.OptionalHeader.(*pe.OptionalHeader64)
	if !ok || file.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
		return report{}, fmt.Errorf("not an x86-64 PE32+ image")
	}
	var symbols []symbol
	probe := uint64(0)
	for _, entry := range file.Symbols {
		if entry.SectionNumber <= 0 || int(entry.SectionNumber) > len(file.Sections) {
			continue
		}
		section := file.Sections[entry.SectionNumber-1]
		if section.Characteristics&pe.IMAGE_SCN_CNT_CODE == 0 {
			continue
		}
		address := header.ImageBase + uint64(section.VirtualAddress) + uint64(entry.Value)
		symbols = append(symbols, symbol{name: entry.Name, address: address})
		if entry.Name == "___chkstk_ms" {
			probe = address
		}
	}
	if len(symbols) == 0 {
		return report{}, fmt.Errorf("no symbols in code sections; the check needs the symbol table")
	}
	sort.Slice(symbols, func(left, right int) bool { return symbols[left].address < symbols[right].address })
	var frames []kernelFrame
	var problems []string
	for _, section := range file.Sections {
		if section.Characteristics&pe.IMAGE_SCN_CNT_CODE == 0 {
			continue
		}
		code, err := section.Data()
		if err != nil {
			return report{}, fmt.Errorf("section %s: %w", section.Name, err)
		}
		if uint32(len(code)) > section.VirtualSize {
			code = code[:section.VirtualSize]
		}
		found, trouble := scanFrames(code, header.ImageBase+uint64(section.VirtualAddress), probe)
		frames = append(frames, found...)
		problems = append(problems, trouble...)
	}
	result := checkKernelFrames(frames, symbols)
	result.problems = append(problems, result.problems...)
	return result, nil
}
