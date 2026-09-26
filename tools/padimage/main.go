// padimage writes a copy of a native image with one more read-only segment of
// SIZE pseudo-random bytes above its others. The program runs as before and
// never touches the segment; the storage and launch tests use the copy as an
// executable far larger than any program the build produces (ADR 0106).
package main

import (
	"fmt"
	"goos/kernel/loader"
	"goos/kernel/mm/vm"
	"os"
	"strconv"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: padimage INPUT OUTPUT SIZE")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fail(err)
	}
	size, err := strconv.ParseUint(os.Args[3], 0, 32)
	if err != nil || size == 0 {
		fail(fmt.Errorf("invalid size %q", os.Args[3]))
	}
	image, ok := loader.ParseNative(data, vm.PageSize, vm.UserCodeAddress, vm.UserImageAreaLimit)
	if !ok || image.SegmentCount == loader.MaxSegments {
		fail(fmt.Errorf("%s is not a native image with room for a segment", os.Args[1]))
	}
	source := loader.ImageSource{Entry: image.Entry, StackTop: image.StackTop, StackReserve: image.StackReserve}
	top := uint64(0)
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		source.Segments = append(source.Segments, loader.SegmentSource{
			VirtualAddress: segment.VirtualAddress, MemorySize: uint32(segment.MemorySize), Flags: segment.Flags,
			Data: data[segment.FileOffset : segment.FileOffset+segment.FileSize],
		})
		if end := segment.VirtualAddress + segment.MemorySize; end > top {
			top = end
		}
	}
	padding := make([]byte, size)
	state := uint32(0x9e3779b9)
	for index := range padding {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		padding[index] = byte(state)
	}
	base := (top + 1<<20 - 1) &^ (1<<20 - 1)
	source.Segments = append(source.Segments, loader.SegmentSource{VirtualAddress: base, MemorySize: uint32(size), Flags: loader.SegmentReadable, Data: padding})
	storage := make([]byte, len(data)+int(size)+int(loader.SegmentHeaderSize))
	padded, ok := loader.EncodeNative(storage, source)
	if !ok {
		fail(fmt.Errorf("encoding the padded image failed"))
	}
	if _, ok := loader.ParseNative(padded, vm.PageSize, vm.UserCodeAddress, vm.UserImageAreaLimit); !ok {
		fail(fmt.Errorf("the padded image does not validate"))
	}
	if err := os.WriteFile(os.Args[2], padded, 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
