package elf

import (
	"goos/kernel/loader"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
)

const (
	// DefaultStackReserve is the fixed initial Linux stack reservation. Pages
	// are supplied on demand; the stack installer populates the initial image.
	DefaultStackReserve = uint64(8 << 20)
	StackGuardSize      = uint64(64 << 10)
	// StaticPIEBase is where every position-independent image loads. The
	// address is fixed: GoOS randomizes no layout, so a program that prints
	// its addresses prints the same ones on each run. It leaves the lower
	// half of the image area to ET_EXEC images and the upper half to the
	// image and its heap.
	StaticPIEBase = uint64(1 << 30)
)

// Layout is the entry state reserved by MapImage. StackTop points just past
// the demand-zero stack reservation; no stack page is populated here.
type Layout struct {
	Entry    uint64
	StackTop uint64
}

// GoOSConfig is the address policy for Linux images. Executable loads use
// the ordinary low-address image area: an ET_EXEC image at the addresses it
// names, conventional ones such as 0x400000 included, and a static
// position-independent image at StaticPIEBase. Kernel-selected mappings and
// fixed GoOS user windows live above this limit and therefore cannot collide
// with a load segment.
func GoOSConfig() Config {
	return Config{PageSize: vm.PageSize, UserStart: vm.UserAddressStart, UserEnd: vm.UserImageAreaLimit, PIEBase: StaticPIEBase}
}

// MapImage records a validated Linux ELF image in a process region map. The
// executable origin must outlive the process. PT_LOAD pages remain absent:
// the common user-fault path reads Data bytes from Offset in the origin and
// supplies zero pages after them. The stack is writable, NX and demand-zero.
//
// The operation preflights every range and table slot before changing the map.
func MapImage(image *Image, source loader.Source, origin loader.Origin, regions *region.Map) (Layout, bool) {
	if image == nil || regions == nil || !origin.Valid() || !validMapping(image, source) {
		return Layout{}, false
	}
	stackBase := vm.UserStackTop - DefaultStackReserve
	guardBase := stackBase - StackGuardSize
	if regions.Count()+image.SegmentCount+2 > region.MaxRegions ||
		!regions.Free(guardBase, StackGuardSize) || !regions.Free(stackBase, DefaultStackReserve) {
		return Layout{}, false
	}
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		if !regions.Free(segment.MapStart, segment.MapEnd-segment.MapStart) {
			return Layout{}, false
		}
	}
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		flags, object := region.Pinned, origin.Key
		if origin.Object != 0 {
			flags, object = region.Pinned|region.FromObject, origin.Object
		}
		entry := region.Region{
			Base:       segment.MapStart,
			Length:     segment.MapEnd - segment.MapStart,
			Protection: segmentProtection(segment),
			Backing:    region.Image,
			Flags:      flags,
			Object:     object,
			Offset:     segment.FileMapOffset,
			Data:       segment.VirtualAddress - segment.MapStart + segment.FileSize,
		}
		if regions.Insert(entry) != region.OK {
			return Layout{}, false
		}
	}
	if regions.Insert(region.Region{Base: guardBase, Length: StackGuardSize, Backing: region.Guard, Flags: region.Pinned}) != region.OK ||
		regions.Insert(region.Region{Base: stackBase, Length: DefaultStackReserve, Protection: region.Read | region.Write, Backing: region.Anonymous, Flags: region.Pinned}) != region.OK {
		return Layout{}, false
	}
	return Layout{Entry: image.Entry, StackTop: vm.UserStackTop}, true
}

// validMapping rechecks the exported load plan at the mapping boundary. Parse
// is the normal producer, but callers cannot mutate an Image into a mapping
// that escapes the low image area, aliases pages, violates W^X, or reads past
// a source that changed after validation.
func validMapping(image *Image, source loader.Source) bool {
	if image.SegmentCount <= 0 || image.SegmentCount > MaxLoadSegments {
		return false
	}
	entryExecutable := false
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		if segment.MapStart%vm.PageSize != 0 || segment.MapEnd%vm.PageSize != 0 ||
			segment.MapStart < vm.UserAddressStart || segment.MapStart >= segment.MapEnd || segment.MapEnd > vm.UserImageAreaLimit ||
			segment.VirtualAddress < segment.MapStart || segment.VirtualAddress >= segment.MapEnd ||
			segment.MapStart != segment.VirtualAddress&^(vm.PageSize-1) || segment.FileSize > segment.MemorySize || segment.MemorySize == 0 ||
			segment.Flags&FlagReadable == 0 || segment.Flags&^validFlags != 0 ||
			segment.Writable() && segment.Executable() {
			return false
		}
		delta := segment.VirtualAddress - segment.MapStart
		memoryEnd, memoryOK := add(segment.VirtualAddress, segment.MemorySize)
		data, dataOK := add(delta, segment.FileSize)
		fileEnd, fileOK := add(segment.FileMapOffset, data)
		expectedMapEnd, mapEndOK := roundUp(memoryEnd, vm.PageSize)
		if segment.FileOffset < delta || segment.FileMapOffset != segment.FileOffset-delta ||
			!memoryOK || !dataOK || !fileOK || !mapEndOK || segment.MapEnd != expectedMapEnd || fileEnd > source.Len() {
			return false
		}
		for previous := 0; previous < index; previous++ {
			other := image.Segments[previous]
			if segment.MapStart < other.MapEnd && other.MapStart < segment.MapEnd {
				return false
			}
		}
		if segment.Executable() && image.Entry >= segment.VirtualAddress && image.Entry < memoryEnd {
			entryExecutable = true
		}
	}
	return entryExecutable
}

func segmentProtection(segment Segment) region.Protection {
	protection := region.Read
	if segment.Writable() {
		protection |= region.Write
	}
	if segment.Executable() {
		protection |= region.Execute
	}
	return protection
}
