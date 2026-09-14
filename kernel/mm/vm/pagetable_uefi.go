//go:build uefi && amd64

package vm

import (
	"goos/kernel/mm/pmm"
	"unsafe"
)

const entriesPerTable = 512

type table [entriesPerTable]uint64

// LoadedImage reads the fixed-address, non-relocatable PE image installed by
// the TinyGo UEFI target. The function validates every header before the image
// description is used to create executable mappings.
func LoadedImage() (Image, bool) {
	// SAFETY: the linker fixes the image at KernelImageBase and UEFI has loaded
	// at least the PE header page before entering main. Only this bounded header
	// view is used to discover and validate SizeOfImage.
	header := (*[PageSize]byte)(unsafe.Pointer(uintptr(KernelImageBase)))[:]
	if len(header) < 0x100 || header[0] != 'M' || header[1] != 'Z' {
		return Image{}, false
	}
	peOffset := uint64(header[0x3c]) | uint64(header[0x3d])<<8 | uint64(header[0x3e])<<16 | uint64(header[0x3f])<<24
	if peOffset+24+60 > uint64(len(header)) {
		return Image{}, false
	}
	optionalOffset := peOffset + 24
	imageSize := uint64(header[optionalOffset+56]) | uint64(header[optionalOffset+57])<<8 | uint64(header[optionalOffset+58])<<16 | uint64(header[optionalOffset+59])<<24
	if imageSize == 0 || imageSize > 16*1024*1024 {
		return Image{}, false
	}
	// SAFETY: SizeOfImage was read from a bounded header and capped at 16 MiB.
	// UEFI guarantees that the complete loaded image occupies this range.
	data := (*[16 * 1024 * 1024]byte)(unsafe.Pointer(uintptr(KernelImageBase)))[:imageSize:imageSize]
	return ParsePE(data, KernelImageBase)
}

// BuildIdentity creates supervisor-only 4 KiB mappings for the first GiB.
// Low memory remains unmapped. All ordinary pages are RW+NX; validated PE
// sections replace those permissions, ensuring that no leaf is W+X.
func BuildIdentity(allocator *pmm.Allocator, image Image) (uint64, bool) {
	rootAddress, root, ok := allocateTable(allocator)
	if !ok {
		return 0, false
	}
	pdptAddress, pdpt, ok := allocateTable(allocator)
	if !ok {
		return 0, false
	}
	pdAddress, pd, ok := allocateTable(allocator)
	if !ok {
		return 0, false
	}
	root[0] = pdptAddress | entryPresent | entryWritable
	pdpt[0] = pdAddress | entryPresent | entryWritable

	defaultPermissions := Permissions{Writable: true}
	for directoryIndex := uint64(0); directoryIndex < entriesPerTable; directoryIndex++ {
		ptAddress, pt, allocated := allocateTable(allocator)
		if !allocated {
			return 0, false
		}
		pd[directoryIndex] = ptAddress | entryPresent | entryWritable
		for tableIndex := uint64(0); tableIndex < entriesPerTable; tableIndex++ {
			physical := (directoryIndex*entriesPerTable + tableIndex) * PageSize
			if physical < 1024*1024 {
				continue
			}
			pt[tableIndex], _ = pageEntry(physical, defaultPermissions)
		}
	}

	if !protectRange(root, KernelImageBase, image.Size, Permissions{}) {
		return 0, false
	}
	for index := 0; index < image.SectionCount; index++ {
		section := image.Sections[index]
		if !protectRange(root, section.Address, section.Size, section.Permissions) {
			return 0, false
		}
	}
	return rootAddress, true
}

func allocateTable(allocator *pmm.Allocator) (uint64, *table, bool) {
	address, ok := allocator.Allocate()
	if !ok || address >= IdentityMapLimit {
		return 0, nil, false
	}
	// SAFETY: allocator returned an exclusively owned, page-aligned physical
	// page below the current identity-map limit. It becomes page-table metadata.
	value := (*table)(unsafe.Pointer(uintptr(address)))
	for index := range value {
		value[index] = 0
	}
	return address, value, true
}

func protectRange(root *table, address uint64, size uint64, permissions Permissions) bool {
	if size == 0 || !permissions.Valid() || address+size < address || address+size > IdentityMapLimit {
		return false
	}
	start := address &^ (PageSize - 1)
	end := (address + size + PageSize - 1) &^ (PageSize - 1)
	for page := start; page < end; page += PageSize {
		entry := leafEntry(root, page)
		if entry == nil {
			return false
		}
		value, ok := pageEntry(page, permissions)
		if !ok {
			return false
		}
		*entry = value
	}
	return true
}

func leafEntry(root *table, address uint64) *uint64 {
	pml4Entry := root[(address>>39)&0x1ff]
	if pml4Entry&entryPresent == 0 {
		return nil
	}
	// SAFETY: non-leaf entries were produced only by allocateTable and retain
	// exclusive, identity-mapped page-table addresses.
	pdpt := (*table)(unsafe.Pointer(uintptr(pml4Entry & addressMask)))
	pdptEntry := pdpt[(address>>30)&0x1ff]
	if pdptEntry&entryPresent == 0 {
		return nil
	}
	// SAFETY: same page-table ownership invariant as above.
	pd := (*table)(unsafe.Pointer(uintptr(pdptEntry & addressMask)))
	pdEntry := pd[(address>>21)&0x1ff]
	if pdEntry&entryPresent == 0 {
		return nil
	}
	// SAFETY: same page-table ownership invariant as above.
	pt := (*table)(unsafe.Pointer(uintptr(pdEntry & addressMask)))
	return &pt[(address>>12)&0x1ff]
}
