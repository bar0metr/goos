package loader

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"goos/kernel/mm/vm"
)

func TestParseNativeRequiresStackGuardGap(t *testing.T) {
	const stackAddress = uint64(0x40020000)
	guardStart := stackAddress - StackGuardSize
	code := SegmentSource{VirtualAddress: 0x40002000, MemorySize: 16, Flags: SegmentReadable | SegmentExecutable, Data: []byte{0x90}}
	stack := SegmentSource{VirtualAddress: stackAddress, MemorySize: 2 * 4096, Flags: SegmentReadable | SegmentWritable}
	data := func(address uint64, size uint32) SegmentSource {
		return SegmentSource{VirtualAddress: address, MemorySize: size, Flags: SegmentReadable | SegmentWritable}
	}
	tests := []struct {
		name     string
		entry    uint64
		stackTop uint64
		segments []SegmentSource
		accepted bool
	}{
		{name: "unmapped gap below", stackTop: stackAddress + 0x2000, segments: []SegmentSource{code, stack}, accepted: true},
		{name: "heap directly above", stackTop: stackAddress + 0x2000, accepted: true,
			segments: []SegmentSource{code, stack, data(stackAddress+0x2000, 4096)}},
		{name: "segment ends where the gap begins", stackTop: stackAddress + 0x2000, accepted: true,
			segments: []SegmentSource{code, stack, data(guardStart-0x1000, 4096)}},
		{name: "segment tail one byte into the gap", stackTop: stackAddress + 0x2000,
			segments: []SegmentSource{code, stack, data(guardStart-0x1000, 4097)}},
		{name: "data at the bottom of the gap", stackTop: stackAddress + 0x2000,
			segments: []SegmentSource{code, stack, data(guardStart, 4096)}},
		{name: "data in the page below the stack", stackTop: stackAddress + 0x2000,
			segments: []SegmentSource{code, stack, data(stackAddress-0x1000, 8)}},
		// The layout ADR 0083 accepted: one free page, then data.
		{name: "one-page gap", stackTop: stackAddress + 0x2000,
			segments: []SegmentSource{code, stack, data(stackAddress-0x2000, 4096)}},
		{name: "code directly below", stackTop: 0x40004000, segments: []SegmentSource{code, data(0x40003000, 4096)}},
		{name: "gap below user start", entry: 0x40010000, stackTop: 0x40010000, segments: []SegmentSource{
			{VirtualAddress: 0x40010000, MemorySize: 16, Flags: SegmentReadable | SegmentExecutable, Data: []byte{0x90}},
			data(0x4000f000, 4096)}},
	}
	for _, test := range tests {
		entry := test.entry
		if entry == 0 {
			entry = 0x40002000
		}
		encoded, ok := EncodeNative(make([]byte, 512), ImageSource{Entry: entry, StackTop: test.stackTop, Segments: test.segments})
		if !ok {
			t.Fatalf("%s: encode failed", test.name)
		}
		if _, ok := ParseNative(encoded, testPageSize, testUserStart, testUserEnd); ok != test.accepted {
			t.Errorf("%s: accepted = %v, want %v", test.name, ok, test.accepted)
		}
	}
}

func TestStackGuardRange(t *testing.T) {
	image, ok := ParseNative(validNativeImage(t), testPageSize, testUserStart, testUserEnd)
	if !ok {
		t.Fatal("valid image rejected")
	}
	start, end, guarded := image.StackGuard()
	if !guarded || start != 0x40020000-StackGuardSize || end != 0x40020000 {
		t.Fatalf("guard = [%#x, %#x) %v", start, end, guarded)
	}
	if _, _, guarded := (Image{}).StackGuard(); guarded {
		t.Fatal("image without a stack reported a guard")
	}
}

func TestKernelMapsUserRange(t *testing.T) {
	tests := []struct {
		name       string
		start, end uint64
		mapped     bool
	}{
		{name: "probe page", start: vm.UserProbeAddress, end: vm.UserProbeAddress + 0x1000, mapped: true},
		{name: "startup page at the top of a gap", start: vm.UserCodeAddress - StackGuardSize, end: vm.UserCodeAddress, mapped: true},
		{name: "ends at the probe page", start: vm.UserProbeAddress - StackGuardSize, end: vm.UserProbeAddress},
		{name: "starts above the startup page", start: vm.UserCodeAddress, end: vm.UserCodeAddress + StackGuardSize},
		{name: "ends at the shared window", start: vm.UserSharedAddress - StackGuardSize, end: vm.UserSharedAddress},
		{name: "reaches into the shared window", start: vm.UserSharedAddress - StackGuardSize + 0x1000, end: vm.UserSharedAddress + 0x1000, mapped: true},
		{name: "DMA window", start: vm.UserDMAAddress, end: vm.UserDMAAddress + StackGuardSize, mapped: true},
		{name: "top of the system image window", start: vm.UserImageLimit - 0x1000, end: vm.UserImageLimit - 0x1000 + StackGuardSize, mapped: true},
		{name: "above the system image window", start: vm.UserImageLimit, end: vm.UserImageLimit + StackGuardSize},
	}
	for _, test := range tests {
		if got := KernelMapsUserRange(test.start, test.end); got != test.mapped {
			t.Errorf("%s: KernelMapsUserRange(%#x, %#x) = %v, want %v", test.name, test.start, test.end, got, test.mapped)
		}
	}
}

// The fixed self-test and fault-probe layout keeps its code, its data page and
// the self-test's shared-memory pages (0x40010000 to 0x40014fff) below the gap.
func TestFixedLayoutLeavesTheGapFree(t *testing.T) {
	start := vm.UserStackAddress - StackGuardSize
	if start < vm.UserDataAddress+vm.PageSize || start < 0x40015000 || KernelMapsUserRange(start, vm.UserStackAddress) {
		t.Fatalf("gap [%#x, %#x) overlaps the fixed layout", start, vm.UserStackAddress)
	}
}

// The userspace linker script leaves the gap itself; it must leave exactly the
// size the loader requires.
func TestLinkerScriptLeavesTheGuardGap(t *testing.T) {
	script, err := os.ReadFile("../../userspace/runtime/amd64.ld")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`\. = ALIGN\(0x1000\) \+ (0x[0-9a-fA-F]+);\s*\n\s*\.stack `).FindSubmatch(script)
	if match == nil {
		t.Fatal("amd64.ld: no gap before .stack")
	}
	gap, err := strconv.ParseUint(string(match[1]), 0, 64)
	if err != nil || gap != StackGuardSize {
		t.Fatalf("amd64.ld leaves %s below the stack, the loader requires %#x", match[1], StackGuardSize)
	}
}
