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

// UnmapPage removes one aligned leaf from a kernel-owned bootstrap table.
func UnmapPage(rootAddress uint64, address uint64) bool {
	if rootAddress%PageSize != 0 || address%PageSize != 0 || address >= IdentityMapLimit {
		return false
	}
	// SAFETY: rootAddress is the exclusively owned PML4 returned by
	// BuildIdentity and remains identity-mapped for the bootstrap lifetime.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	entry := leafEntry(root, address)
	if entry == nil || *entry&entryPresent == 0 {
		return false
	}
	*entry = 0
	return true
}

// MapKernelPage restores one supervisor RW+NX identity mapping. Guard-page
// teardown uses it before returning the physical page to the allocator.
func MapKernelPage(rootAddress uint64, address uint64) bool {
	if rootAddress%PageSize != 0 || address%PageSize != 0 || address >= IdentityMapLimit {
		return false
	}
	// SAFETY: rootAddress names the live kernel-owned PML4 and the shared
	// identity-map hierarchy remains resident for the kernel lifetime.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	entry := leafEntry(root, address)
	if entry == nil || *entry&entryPresent != 0 {
		return false
	}
	value, ok := pageEntry(address, Permissions{Writable: true})
	if !ok {
		return false
	}
	*entry = value
	return true
}

// IsMapped reports whether a bootstrap leaf is present.
func IsMapped(rootAddress uint64, address uint64) bool {
	if rootAddress%PageSize != 0 || address >= IdentityMapLimit {
		return false
	}
	// SAFETY: rootAddress follows the same BuildIdentity ownership invariant as
	// UnmapPage. This function reads but never mutates the page-table hierarchy.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	entry := leafEntry(root, address)
	return entry != nil && *entry&entryPresent != 0
}

// CreateUserAddressSpace creates a new PML4 that shares supervisor-only kernel
// mappings and has an empty, private bootstrap userspace region.
func CreateUserAddressSpace(allocator *pmm.Allocator, kernelRootAddress uint64) (uint64, bool) {
	if kernelRootAddress%PageSize != 0 {
		return 0, false
	}
	rootAddress, root, ok := allocateTable(allocator)
	if !ok {
		return 0, false
	}
	pdptAddress, pdpt, ok := allocateTable(allocator)
	if !ok {
		return 0, false
	}
	// SAFETY: kernelRootAddress is the live PML4 returned by BuildIdentity. Its
	// first entry names the identity-mapped kernel PDPT.
	kernelRoot := (*table)(unsafe.Pointer(uintptr(kernelRootAddress)))
	kernelPDPTEntry := kernelRoot[0]
	if kernelPDPTEntry&entryPresent == 0 {
		return 0, false
	}
	// SAFETY: the present non-leaf entry was created by BuildIdentity and points
	// to an identity-mapped page-table page.
	kernelPDPT := (*table)(unsafe.Pointer(uintptr(kernelPDPTEntry & addressMask)))
	for index := range pdpt {
		pdpt[index] = kernelPDPT[index]
	}
	root[0] = pdptAddress | entryPresent | entryWritable | entryUser

	pdAddress, pd, ok := allocateTable(allocator)
	if !ok {
		return 0, false
	}
	ptAddress, _, ok := allocateTable(allocator)
	if !ok {
		return 0, false
	}
	pdpt[(UserProbeAddress>>30)&0x1ff] = pdAddress | entryPresent | entryWritable | entryUser
	pd[(UserProbeAddress>>21)&0x1ff] = ptAddress | entryPresent | entryWritable | entryUser
	return rootAddress, true
}

// MapUserPage adds a supervisor-authorized user leaf to an existing process
// address space. Writable executable mappings are rejected by pageEntry.
func MapUserPage(rootAddress uint64, virtual uint64, physical uint64, permissions Permissions) bool {
	return mapUserPage(rootAddress, virtual, physical, permissions, false)
}

// MapOwnedUserPage maps private process storage. DestroyUserAddressSpace
// returns the backing physical page; shared pages must use MapUserPage.
func MapOwnedUserPage(rootAddress uint64, virtual uint64, physical uint64, permissions Permissions) bool {
	return mapUserPage(rootAddress, virtual, physical, permissions, true)
}

func mapUserPage(rootAddress uint64, virtual uint64, physical uint64, permissions Permissions, owned bool) bool {
	if rootAddress%PageSize != 0 || virtual%PageSize != 0 || physical%PageSize != 0 ||
		virtual < IdentityMapLimit || virtual >= UserAddressLimit || !permissions.User {
		return false
	}
	// SAFETY: rootAddress names a process PML4 created by this package and its
	// page-table pages remain owned and identity-mapped during bootstrap.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	entry := leafEntry(root, virtual)
	if entry == nil || *entry&entryPresent != 0 {
		return false
	}
	value, ok := pageEntry(physical, permissions)
	if !ok {
		return false
	}
	if owned {
		value |= entryOwned
	}
	*entry = value
	return true
}

// DestroyUserAddressSpace releases private user leaves and every process-owned
// page-table page. Kernel branches and shared-memory backing remain allocated.
func DestroyUserAddressSpace(allocator *pmm.Allocator, rootAddress uint64) bool {
	if allocator == nil || rootAddress%PageSize != 0 || rootAddress >= IdentityMapLimit {
		return false
	}
	// SAFETY: rootAddress names an inactive process PML4 created by this
	// package. The caller guarantees that no task can load it again.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	ok := true
	for rootIndex := range root {
		rootEntry := root[rootIndex]
		if rootEntry&(entryPresent|entryUser) != entryPresent|entryUser {
			continue
		}
		pdptAddress := rootEntry & addressMask
		pdpt := (*table)(unsafe.Pointer(uintptr(pdptAddress)))
		for pdptIndex := range pdpt {
			pdptEntry := pdpt[pdptIndex]
			if pdptEntry&(entryPresent|entryUser) != entryPresent|entryUser {
				continue
			}
			pdAddress := pdptEntry & addressMask
			pd := (*table)(unsafe.Pointer(uintptr(pdAddress)))
			for pdIndex := range pd {
				pdEntry := pd[pdIndex]
				if pdEntry&(entryPresent|entryUser) != entryPresent|entryUser {
					continue
				}
				ptAddress := pdEntry & addressMask
				pt := (*table)(unsafe.Pointer(uintptr(ptAddress)))
				for leafIndex := range pt {
					leaf := pt[leafIndex]
					if leaf&(entryPresent|entryUser|entryOwned) == entryPresent|entryUser|entryOwned {
						ok = allocator.Free(leaf&addressMask) && ok
					}
					pt[leafIndex] = 0
				}
				pd[pdIndex] = 0
				ok = allocator.Free(ptAddress) && ok
			}
			pdpt[pdptIndex] = 0
			ok = allocator.Free(pdAddress) && ok
		}
		root[rootIndex] = 0
		ok = allocator.Free(pdptAddress) && ok
	}
	return allocator.Free(rootAddress) && ok
}

// CanMapUserPage reports whether the existing bootstrap page-table hierarchy
// has an unused leaf for the requested user address. Loaders use this to reject
// unsupported branches and conflicts before allocating or exposing pages.
func CanMapUserPage(rootAddress uint64, virtual uint64) bool {
	if rootAddress%PageSize != 0 || virtual%PageSize != 0 || virtual < IdentityMapLimit || virtual >= UserAddressLimit {
		return false
	}
	// SAFETY: rootAddress names a process PML4 created by this package; the
	// function only inspects its owned, identity-mapped page-table hierarchy.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	entry := leafEntry(root, virtual)
	return entry != nil && *entry&entryPresent == 0
}

// CopyIn copies a bounded range from a process address space through the
// kernel's physical identity map. Every page in the range must be explicitly
// user-accessible; supervisor mappings are never accepted through this path.
func CopyIn(rootAddress uint64, address uint64, destination []byte) bool {
	if !ValidateUserRange(address, uint64(len(destination))) || rootAddress%PageSize != 0 {
		return false
	}
	for copied := uint64(0); copied < uint64(len(destination)); {
		physical, _, ok := resolveUser(rootAddress, address+copied)
		if !ok || physical >= IdentityMapLimit {
			return false
		}
		pageRemaining := PageSize - physical&(PageSize-1)
		remaining := uint64(len(destination)) - copied
		if pageRemaining > remaining {
			pageRemaining = remaining
		}
		// SAFETY: resolveUser validated every page-table level and returned a
		// present user leaf. Bootstrap user pages come from PMM below the active
		// identity-map limit, so this physical range is kernel-addressable.
		source := (*[PageSize]byte)(unsafe.Pointer(uintptr(physical &^ (PageSize - 1))))
		offset := physical & (PageSize - 1)
		for index := uint64(0); index < pageRemaining; index++ {
			destination[copied+index] = source[offset+index]
		}
		copied += pageRemaining
	}
	return true
}

// CopyOut copies a bounded range into user memory. In addition to the user
// checks performed by CopyIn, every destination leaf must be writable.
func CopyOut(rootAddress uint64, address uint64, source []byte) bool {
	if !ValidateUserRange(address, uint64(len(source))) || rootAddress%PageSize != 0 {
		return false
	}
	for copied := uint64(0); copied < uint64(len(source)); {
		physical, leaf, ok := resolveUser(rootAddress, address+copied)
		if !ok || leaf&entryWritable == 0 || physical >= IdentityMapLimit {
			return false
		}
		pageRemaining := PageSize - physical&(PageSize-1)
		remaining := uint64(len(source)) - copied
		if pageRemaining > remaining {
			pageRemaining = remaining
		}
		// SAFETY: resolveUser validated a present user leaf and the writable bit.
		// The backing PMM page is below the active physical identity-map limit.
		target := (*[PageSize]byte)(unsafe.Pointer(uintptr(physical &^ (PageSize - 1))))
		offset := physical & (PageSize - 1)
		for index := uint64(0); index < pageRemaining; index++ {
			target[offset+index] = source[copied+index]
		}
		copied += pageRemaining
	}
	return true
}

// InitializePhysicalByte initializes an exclusively owned physical page before
// it is exposed through a process mapping.
func InitializePhysicalByte(address uint64, value byte) bool {
	if address%PageSize != 0 || address >= IdentityMapLimit {
		return false
	}
	// SAFETY: the caller passes a page freshly allocated from PMM while the
	// bootstrap identity map is active. The complete page is exclusively owned.
	target := (*[PageSize]byte)(unsafe.Pointer(uintptr(address)))
	for index := range target {
		target[index] = 0
	}
	target[0] = value
	return true
}

// InitializePhysical copies a bounded payload into a freshly allocated page.
func InitializePhysical(address uint64, data []byte) bool {
	if address%PageSize != 0 || address >= IdentityMapLimit || len(data) > int(PageSize) {
		return false
	}
	// SAFETY: the page is exclusively owned by the caller and identity-mapped;
	// the length check confines every write to that page.
	target := (*[PageSize]byte)(unsafe.Pointer(uintptr(address)))
	for index := range target {
		target[index] = 0
	}
	for index := range data {
		target[index] = data[index]
	}
	return true
}

// Resolve returns the physical leaf backing a virtual address.
func Resolve(rootAddress uint64, virtual uint64) (uint64, bool) {
	if rootAddress%PageSize != 0 {
		return 0, false
	}
	// SAFETY: rootAddress must name a PML4 created by this package; traversal
	// validates every present entry before dereferencing the next table.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	entry := leafEntry(root, virtual)
	if entry == nil || *entry&entryPresent == 0 {
		return 0, false
	}
	return *entry&addressMask | virtual&(PageSize-1), true
}

func resolveUser(rootAddress uint64, virtual uint64) (uint64, uint64, bool) {
	if rootAddress%PageSize != 0 || !ValidateUserRange(virtual, 1) {
		return 0, 0, false
	}
	// SAFETY: rootAddress names a process PML4 created by this package and its
	// page-table pages remain owned and identity-mapped during bootstrap.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	pml4Entry := root[(virtual>>39)&0x1ff]
	if pml4Entry&(entryPresent|entryUser) != entryPresent|entryUser {
		return 0, 0, false
	}
	// SAFETY: the present root entry was allocated by CreateUserAddressSpace.
	pdpt := (*table)(unsafe.Pointer(uintptr(pml4Entry & addressMask)))
	pdptEntry := pdpt[(virtual>>30)&0x1ff]
	if pdptEntry&(entryPresent|entryUser) != entryPresent|entryUser {
		return 0, 0, false
	}
	// SAFETY: the present user PDPT entry names an owned page-table page.
	pd := (*table)(unsafe.Pointer(uintptr(pdptEntry & addressMask)))
	pdEntry := pd[(virtual>>21)&0x1ff]
	if pdEntry&(entryPresent|entryUser) != entryPresent|entryUser {
		return 0, 0, false
	}
	// SAFETY: the present user directory entry names an owned page-table page.
	pt := (*table)(unsafe.Pointer(uintptr(pdEntry & addressMask)))
	leaf := pt[(virtual>>12)&0x1ff]
	if leaf&(entryPresent|entryUser) != entryPresent|entryUser {
		return 0, 0, false
	}
	return leaf&addressMask | virtual&(PageSize-1), leaf, true
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
