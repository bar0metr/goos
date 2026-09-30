package elf

import (
	"encoding/binary"
	"testing"

	"goos/kernel/loader"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
)

func mappedELF(t *testing.T) ([]byte, Image) {
	t.Helper()
	data := validELF(t)
	image, failure := Parse(data, GoOSConfig())
	if failure != OK {
		t.Fatalf("valid ELF rejected: %d", failure)
	}
	return data, image
}

func emptyRegions(t *testing.T) *region.Map {
	t.Helper()
	regions := new(region.Map)
	if !regions.Init(vm.UserAddressStart, vm.UserAddressLimit) {
		t.Fatal("region map init failed")
	}
	return regions
}

func TestMapImageUsesCommonLazySourceAndDemandZeroStack(t *testing.T) {
	data, image := mappedELF(t)
	regions := emptyRegions(t)
	layout, ok := MapImage(&image, loader.Contiguous(data), loader.Origin{Object: 41}, regions)
	if !ok || layout.Entry != image.Entry || layout.StackTop != vm.UserStackTop || regions.Count() != image.SegmentCount+2 {
		t.Fatalf("map failed: ok=%v layout=%+v regions=%d", ok, layout, regions.Count())
	}
	code, found := regions.Find(0x401000)
	if !found || code.Backing != region.Image || code.Flags != region.Pinned|region.FromObject || code.Object != 41 ||
		code.Offset != 0x1000 || code.Data != 0x200 || code.Protection != region.Read|region.Execute {
		t.Fatalf("code mapping: %+v", code)
	}
	bss, found := regions.Find(0x402200)
	if !found || bss.Offset != 0x2000 || bss.Data != 0x100 || bss.Length != vm.PageSize ||
		bss.Protection != region.Read|region.Write {
		t.Fatalf("BSS mapping: %+v", bss)
	}
	stackBase := vm.UserStackTop - DefaultStackReserve
	guard, guarded := regions.Find(stackBase - 1)
	stack, stacked := regions.Find(stackBase)
	if !guarded || guard.Backing != region.Guard || guard.Length != StackGuardSize ||
		!stacked || stack.Backing != region.Anonymous || stack.Length != DefaultStackReserve ||
		stack.Protection != region.Read|region.Write || stack.Protection&region.Execute != 0 {
		t.Fatalf("guard=%+v stack=%+v", guard, stack)
	}
}

func TestMapImagePreservesCongruentUnalignedFileWindow(t *testing.T) {
	data := validELF(t)
	header := programHeaderAt(data, validCode)
	binary.LittleEndian.PutUint64(header[8:], 0x1120)
	binary.LittleEndian.PutUint64(header[16:], 0x401120)
	binary.LittleEndian.PutUint64(header[48:], 0x10)
	binary.LittleEndian.PutUint64(data[24:], 0x401120)
	image, failure := Parse(data, GoOSConfig())
	if failure != OK {
		t.Fatalf("unaligned ELF rejected: %d", failure)
	}
	regions := emptyRegions(t)
	if _, ok := MapImage(&image, loader.Contiguous(data), loader.Origin{Key: 7}, regions); !ok {
		t.Fatal("unaligned image mapping failed")
	}
	mapping, found := regions.Find(0x401000)
	if !found || mapping.Base != 0x401000 || mapping.Offset != 0x1000 || mapping.Data != 0x320 || mapping.Object != 7 || mapping.Flags != region.Pinned {
		t.Fatalf("unaligned mapping: %+v", mapping)
	}
}

func TestMapImageAllowsConventionalLowAddressAndRejectsCollisions(t *testing.T) {
	data, image := mappedELF(t)
	regions := emptyRegions(t)
	if regions.Insert(region.Region{Base: 0x401000, Length: vm.PageSize, Protection: region.Read, Backing: region.Device, Flags: region.Pinned}) != region.OK {
		t.Fatal("collision setup failed")
	}
	before := regions.Count()
	if _, ok := MapImage(&image, loader.Contiguous(data), loader.Origin{Object: 1}, regions); ok || regions.Count() != before {
		t.Fatalf("collision was accepted or partially mapped: ok=%v count=%d", ok, regions.Count())
	}
	clean := emptyRegions(t)
	if _, ok := MapImage(&image, loader.Contiguous(data), loader.Origin{Object: 1}, clean); !ok {
		t.Fatal("ordinary 0x400000 ET_EXEC mapping rejected")
	}
}

func TestMapImageRevalidatesSourceOriginAndProtection(t *testing.T) {
	data, image := mappedELF(t)
	tests := []struct {
		name   string
		image  Image
		source loader.Source
		origin loader.Origin
	}{
		{name: "missing origin", image: image, source: loader.Contiguous(data)},
		{name: "ambiguous origin", image: image, source: loader.Contiguous(data), origin: loader.Origin{Key: 1, Object: 2}},
		{name: "truncated source", image: image, source: loader.Contiguous(data[:0x20ff]), origin: loader.Origin{Object: 1}},
	}
	wx := image
	wx.Segments[1].Flags |= FlagWritable
	tests = append(tests, struct {
		name   string
		image  Image
		source loader.Source
		origin loader.Origin
	}{name: "write execute", image: wx, source: loader.Contiguous(data), origin: loader.Origin{Object: 1}})
	for _, test := range tests {
		if _, ok := MapImage(&test.image, test.source, test.origin, emptyRegions(t)); ok {
			t.Errorf("accepted %s", test.name)
		}
	}
}
