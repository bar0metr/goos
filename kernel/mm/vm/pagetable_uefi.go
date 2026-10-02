//go:build uefi && amd64

package vm

import (
	"goos/kernel/boot"
	"goos/kernel/mm/pmm"
	"unsafe"
)

const entriesPerTable = 512

type table [entriesPerTable]uint64

// kernelDirectMap records what BuildIdentity placed in the direct map. It is
// written once, before the kernel's page tables are loaded, and only read
// afterwards.
var kernelDirectMap directMap

// copyBytes and zeroBytes use string instructions. Both addresses must be
// kernel-mapped memory that the caller owns for the whole range.
//
//export goos_copy_bytes
func copyBytes(destination uintptr, source uintptr, length uint64)

//export goos_zero_bytes
func zeroBytes(destination uintptr, length uint64)

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
// sections replace those permissions, ensuring that no leaf is W+X. It then
// builds the direct map of every managed physical page at DirectMapBase.
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
	if !buildDirectMap(allocator, root) {
		return 0, false
	}
	// The same 4 KiB hierarchy supplies the higher-half kernel alias. Permission
	// changes and guard pages therefore stay identical in both views.
	root[kernelLowSlot] = root[0]
	return rootAddress, true
}

// buildDirectMap maps every segment the page allocator manages at
// DirectMapBase+physical with supervisor-only RW+NX two MiB leaves, in the
// one PML4 slot every address space shares. Its page tables are low pages.
func buildDirectMap(allocator *pmm.Allocator, root *table) bool {
	pdptAddress, pdpt, ok := allocateTable(allocator)
	if !ok {
		return false
	}
	root[directMapSlot] = pdptAddress | entryPresent | entryWritable
	for index := 0; ; index++ {
		start, end, found := allocator.Segment(index)
		if !found {
			return true
		}
		if !kernelDirectMap.add(start, end) {
			return false
		}
		for page := start &^ (LargePageSize - 1); page < end; page += LargePageSize {
			pd, ok := kernelTable(allocator, pdpt, (page>>30)&0x1ff)
			if !ok {
				return false
			}
			pd[(page>>21)&0x1ff] = largePageEntry(page)
		}
	}
}

// MapLoaderMemory identity-maps, supervisor-only RW+NX, the loader memory
// above IdentityMapLimit. The firmware places the TinyGo heap and stack at the
// top of low RAM, so on a machine with more than a GiB they lie above the
// kernel identity map, and the kernel keeps using them at their physical
// address. Loader memory that overlaps the fixed user windows, or lies beyond
// the first PML4 slot, is refused. Process roots do not inherit these mappings.
func MapLoaderMemory(allocator *pmm.Allocator, rootAddress uint64, regions []boot.MemoryRegion) bool {
	if allocator == nil || rootAddress%PageSize != 0 || rootAddress >= IdentityMapLimit {
		return false
	}
	// SAFETY: rootAddress is the kernel PML4 returned by BuildIdentity; its
	// page-table pages are low pages the kernel identity map covers.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	for _, region := range regions {
		if region.Kind != boot.MemoryLoader || region.End() <= IdentityMapLimit {
			continue
		}
		start := region.Start
		if start < IdentityMapLimit {
			start = IdentityMapLimit
		}
		if start < UserImageLimit || region.End() > 1<<39 {
			return false
		}
		for page := start; page < region.End(); page += PageSize {
			pdpt, ok := kernelTable(allocator, root, (page>>39)&0x1ff)
			if !ok {
				return false
			}
			pd, ok := kernelTable(allocator, pdpt, (page>>30)&0x1ff)
			if !ok {
				return false
			}
			pt, ok := kernelTable(allocator, pd, (page>>21)&0x1ff)
			if !ok {
				return false
			}
			pt[(page>>12)&0x1ff], _ = pageEntry(page, Permissions{Writable: true})
		}
	}
	return true
}

// kernelTable returns the supervisor page table that parent[index] names,
// allocating it when the entry is empty. A present leaf or user entry is
// refused: kernel mappings are never built under either.
func kernelTable(allocator *pmm.Allocator, parent *table, index uint64) (*table, bool) {
	entry := parent[index]
	if entry&entryPresent == 0 {
		address, child, ok := allocateTable(allocator)
		if !ok {
			return nil, false
		}
		parent[index] = address | entryPresent | entryWritable
		return child, true
	}
	if entry&(entryLarge|entryUser) != 0 {
		return nil, false
	}
	// SAFETY: present non-leaf kernel entries are created only by
	// allocateTable and name low page tables inside the identity map.
	return (*table)(unsafe.Pointer(uintptr(entry & addressMask))), true
}

// UnmapPage removes one aligned bootstrap leaf through its low or higher-half
// address.
func UnmapPage(rootAddress uint64, address uint64) bool {
	if rootAddress%PageSize != 0 || address%PageSize != 0 || !bootstrapAddress(address) {
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

// MapKernelDevicePage replaces a high bootstrap identity leaf with a
// supervisor-only RW+NX mapping of one device MMIO page.
func MapKernelDevicePage(rootAddress uint64, virtual uint64, physical uint64) bool {
	if rootAddress%PageSize != 0 || virtual%PageSize != 0 || virtual >= IdentityMapLimit || physical%PageSize != 0 {
		return false
	}
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	entry := leafEntry(root, virtual)
	if entry == nil {
		return false
	}
	value, ok := pageEntry(physical, Permissions{Writable: true})
	if !ok {
		return false
	}
	*entry = value
	return true
}

// IsMapped reports whether a bootstrap leaf is present.
func IsMapped(rootAddress uint64, address uint64) bool {
	if rootAddress%PageSize != 0 || !bootstrapAddress(address) {
		return false
	}
	// SAFETY: rootAddress follows the same BuildIdentity ownership invariant as
	// UnmapPage. This function reads but never mutates the page-table hierarchy.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	entry := leafEntry(root, address)
	return entry != nil && *entry&entryPresent != 0
}

// MappingPermissions reports the effective leaf permissions used by focused
// entry-mapping validation.
func MappingPermissions(rootAddress, address uint64) (Permissions, bool, bool) {
	if rootAddress%PageSize != 0 {
		return Permissions{}, false, false
	}
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	entry := leafEntry(root, address&^(PageSize-1))
	if entry == nil || *entry&entryPresent == 0 {
		return Permissions{}, false, false
	}
	return Permissions{Writable: *entry&entryWritable != 0, Executable: *entry&entryNX == 0, User: *entry&entryUser != 0}, *entry&entryUser != 0, true
}

// MapProcessKernelPage installs one supervisor-only higher-half mapping in a
// process root. Entry code uses executable read-only pages; stacks and entry
// metadata use writable NX pages.
func MapProcessKernelPage(allocator *pmm.Allocator, rootAddress, virtual, physical uint64, writable, executable bool) bool {
	if allocator == nil || rootAddress%PageSize != 0 || virtual%PageSize != 0 || physical%PageSize != 0 {
		return false
	}
	aliasPhysical, ok := KernelPhysical(virtual)
	if !ok || aliasPhysical != physical {
		return false
	}
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	pdpt, ok := kernelTable(allocator, root, (virtual>>39)&0x1ff)
	if !ok {
		return false
	}
	pd, ok := kernelTable(allocator, pdpt, (virtual>>30)&0x1ff)
	if !ok {
		return false
	}
	pt, ok := kernelTable(allocator, pd, (virtual>>21)&0x1ff)
	if !ok {
		return false
	}
	entry := &pt[(virtual>>12)&0x1ff]
	value, ok := pageEntry(physical, Permissions{Writable: writable, Executable: executable})
	if !ok {
		return false
	}
	if *entry&entryPresent != 0 {
		return *entry&^(entryAccessed|entryDirty) == value
	}
	*entry = value
	return true
}

// UnmapProcessKernelPage removes one higher-half mapping MapProcessKernelPage
// installed in a process root, leaving the page tables that held it. A
// thread's kernel-entry stack is mapped in the root its process shares; the
// mapping must go with the stack, or a later stack whose guard reuses the
// pages would find it mapped. The caller invalidates the address.
func UnmapProcessKernelPage(rootAddress, virtual uint64) bool {
	if rootAddress%PageSize != 0 || virtual%PageSize != 0 {
		return false
	}
	if _, ok := KernelPhysical(virtual); !ok {
		return false
	}
	// SAFETY: rootAddress is a live process PML4; kernel subtables in it are
	// low pages the identity map covers.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	entry := leafEntry(root, virtual)
	if entry == nil || *entry&entryPresent == 0 || *entry&entryUser != 0 {
		return false
	}
	*entry = 0
	return true
}

func bootstrapAddress(address uint64) bool {
	if address < IdentityMapLimit {
		return true
	}
	_, ok := KernelPhysical(address)
	return ok
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
	// SAFETY: kernelRootAddress is the live PML4 returned by BuildIdentity.
	kernelRoot := (*table)(unsafe.Pointer(uintptr(kernelRootAddress)))
	if kernelRoot[kernelLowSlot]&entryPresent == 0 || kernelRoot[directMapSlot]&entryPresent == 0 {
		return 0, false
	}
	// Entry code, descriptor metadata and the task's kernel stack are mapped
	// later as private supervisor leaves. The lower half starts empty.
	root[directMapSlot] = kernelRoot[directMapSlot]
	return rootAddress, true
}

// MapUserPage adds a supervisor-authorized user leaf to an existing process
// address space. Writable executable mappings are rejected by pageEntry.
func MapUserPage(rootAddress uint64, virtual uint64, physical uint64, permissions Permissions) bool {
	return mapUserPage(rootAddress, virtual, physical, permissions, false)
}

func mapUserPage(rootAddress uint64, virtual uint64, physical uint64, permissions Permissions, owned bool) bool {
	if rootAddress%PageSize != 0 || virtual%PageSize != 0 || physical%PageSize != 0 ||
		!ValidateUserRange(virtual, PageSize) || !permissions.User {
		return false
	}
	// SAFETY: rootAddress names a process PML4 created by this package and its
	// page-table pages remain owned and identity-mapped during bootstrap.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	entry := userLeafEntry(root, virtual)
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
// page-table page. The direct-map branch and shared-memory backing remain
// allocated. Supervisor entry leaves and stack leaves name kernel-owned pages,
// so teardown frees their private tables but not the mapped pages.
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
		if rootEntry&entryPresent == 0 || uint64(rootIndex) == directMapSlot {
			continue
		}
		userBranch := rootEntry&entryUser != 0
		if !userBranch && uint64(rootIndex) != kernelLowSlot {
			return false
		}
		pdptAddress := rootEntry & addressMask
		pdpt := (*table)(unsafe.Pointer(uintptr(pdptAddress)))
		for pdptIndex := range pdpt {
			pdptEntry := pdpt[pdptIndex]
			if pdptEntry&entryPresent == 0 {
				continue
			}
			if pdptEntry&entryLarge != 0 || (pdptEntry&entryUser != 0) != userBranch {
				return false
			}
			pdAddress := pdptEntry & addressMask
			pd := (*table)(unsafe.Pointer(uintptr(pdAddress)))
			for pdIndex := range pd {
				pdEntry := pd[pdIndex]
				if pdEntry&entryPresent == 0 {
					continue
				}
				if pdEntry&entryLarge != 0 || (pdEntry&entryUser != 0) != userBranch {
					return false
				}
				ptAddress := pdEntry & addressMask
				pt := (*table)(unsafe.Pointer(uintptr(ptAddress)))
				for leafIndex := range pt {
					leaf := pt[leafIndex]
					if userBranch && leaf&(entryPresent|entryOwned) == entryPresent|entryOwned {
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

// CopyIn copies a bounded range from a process address space through the
// kernel's view of physical memory (kernelAddress). Every page in the range
// must be explicitly user-accessible; supervisor mappings are never accepted
// through this path.
func CopyIn(rootAddress uint64, address uint64, destination []byte) bool {
	if !ValidateUserRange(address, uint64(len(destination))) || rootAddress%PageSize != 0 {
		return false
	}
	for copied := uint64(0); copied < uint64(len(destination)); {
		physical, _, ok := resolveOrFault(rootAddress, address+copied, false)
		if !ok {
			return false
		}
		pageRemaining := PageSize - physical&(PageSize-1)
		remaining := uint64(len(destination)) - copied
		if pageRemaining > remaining {
			pageRemaining = remaining
		}
		source, mapped := kernelDirectMap.kernelAddress(physical, pageRemaining)
		if !mapped {
			return false
		}
		// SAFETY: resolveUser validated every page-table level and returned a
		// present user leaf, and kernelAddress returned the kernel mapping of
		// exactly this part of that page.
		copyBytes(uintptr(unsafe.Pointer(&destination[copied])), uintptr(source), pageRemaining)
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
		physical, leaf, ok := resolveOrFault(rootAddress, address+copied, true)
		if !ok || leaf&entryWritable == 0 {
			return false
		}
		pageRemaining := PageSize - physical&(PageSize-1)
		remaining := uint64(len(source)) - copied
		if pageRemaining > remaining {
			pageRemaining = remaining
		}
		destination, mapped := kernelDirectMap.kernelAddress(physical, pageRemaining)
		if !mapped {
			return false
		}
		// SAFETY: resolveUser validated a present user leaf and the writable bit,
		// and kernelAddress returned the kernel mapping of exactly this part of
		// that page.
		copyBytes(uintptr(destination), uintptr(unsafe.Pointer(&source[copied])), pageRemaining)
		copied += pageRemaining
	}
	return true
}

// CanCopyOut validates an entire writable user range without modifying it.
func CanCopyOut(rootAddress uint64, address uint64, length uint64) bool {
	if !ValidateUserRange(address, length) || length == 0 || rootAddress%PageSize != 0 {
		return false
	}
	for checked := uint64(0); checked < length; {
		physical, leaf, ok := resolveOrFault(rootAddress, address+checked, true)
		if !ok || leaf&entryWritable == 0 {
			return false
		}
		pageRemaining := PageSize - physical&(PageSize-1)
		remaining := length - checked
		if pageRemaining > remaining {
			pageRemaining = remaining
		}
		if _, mapped := kernelDirectMap.kernelAddress(physical, pageRemaining); !mapped {
			return false
		}
		checked += pageRemaining
	}
	return true
}

// InitializePhysicalByte initializes an exclusively owned physical page before
// it is exposed through a process mapping.
func InitializePhysicalByte(address uint64, value byte) bool {
	if address%PageSize != 0 {
		return false
	}
	target, mapped := kernelDirectMap.kernelAddress(address, PageSize)
	if !mapped {
		return false
	}
	// SAFETY: the caller passes a page freshly allocated from PMM and owns all
	// of it; kernelAddress returned the kernel mapping of exactly that page.
	zeroBytes(uintptr(target), PageSize)
	page := (*[PageSize]byte)(unsafe.Pointer(uintptr(target)))
	page[0] = value
	return true
}

// InitializePhysical copies a bounded payload into a freshly allocated page.
func InitializePhysical(address uint64, data []byte) bool {
	if address%PageSize != 0 || len(data) > int(PageSize) {
		return false
	}
	target, mapped := kernelDirectMap.kernelAddress(address, PageSize)
	if !mapped {
		return false
	}
	// SAFETY: the page is exclusively owned by the caller and kernelAddress
	// returned its kernel mapping; the length check confines every write to
	// that page.
	if len(data) != 0 {
		copyBytes(uintptr(target), uintptr(unsafe.Pointer(&data[0])), uint64(len(data)))
	}
	zeroBytes(uintptr(target)+uintptr(len(data)), PageSize-uint64(len(data)))
	return true
}

// PhysicalPage returns the bytes of one kernel-owned physical page, for the
// kernel to fill and read memory no process maps, such as an executable
// object's (ADR 0097).
func PhysicalPage(address uint64) ([]byte, bool) {
	if address%PageSize != 0 {
		return nil, false
	}
	target, mapped := kernelDirectMap.kernelAddress(address, PageSize)
	if !mapped {
		return nil, false
	}
	// SAFETY: kernelAddress returned the kernel mapping of exactly this page,
	// in the identity map or the direct map, and the slice covers only it.
	// The caller owns the page for as long as it uses the slice.
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(target))), PageSize), true
}

// PhysicalWords is the page allocator's pmm.MetadataView: it returns words
// 64-bit words at physical, which must lie in [1 MiB, IdentityMapLimit).
func PhysicalWords(physical uint64, words uint64) []uint64 {
	if physical < identityMapStart || physical%PageSize != 0 || words == 0 ||
		physical >= IdentityMapLimit || words > (IdentityMapLimit-physical)/8 {
		return nil
	}
	// SAFETY: the range lies in [1 MiB, IdentityMapLimit). The allocator calls
	// this from Init, while the firmware's page tables still identity-map all
	// memory, for usable pages it keeps marked used for the kernel lifetime;
	// BuildIdentity maps the same range at the same addresses afterwards, so
	// the slice stays valid once the kernel's own tables are loaded.
	return unsafe.Slice((*uint64)(unsafe.Pointer(uintptr(physical))), words)
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

// ResolveUser returns the physical address behind a user-accessible mapping.
func ResolveUser(rootAddress uint64, virtual uint64) (uint64, bool) {
	physical, _, ok := resolveUser(rootAddress, virtual)
	return physical, ok
}

// FindUserMapping returns a user virtual address that maps the physical page,
// scanning the process's user leaves. It exists for diagnostics only.
func FindUserMapping(rootAddress uint64, physicalPage uint64) (uint64, bool) {
	if rootAddress%PageSize != 0 || rootAddress >= IdentityMapLimit {
		return 0, false
	}
	// SAFETY: rootAddress names a live process PML4 created by this package;
	// every level is checked for presence before it is dereferenced.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	for rootIndex, rootEntry := range root {
		if rootEntry&(entryPresent|entryUser) != entryPresent|entryUser {
			continue
		}
		// SAFETY: present user entries name owned page-table pages.
		pdpt := (*table)(unsafe.Pointer(uintptr(rootEntry & addressMask)))
		for pdptIndex, pdptEntry := range pdpt {
			if pdptEntry&(entryPresent|entryUser) != entryPresent|entryUser {
				continue
			}
			pd := (*table)(unsafe.Pointer(uintptr(pdptEntry & addressMask)))
			for pdIndex, pdEntry := range pd {
				if pdEntry&(entryPresent|entryUser) != entryPresent|entryUser {
					continue
				}
				pt := (*table)(unsafe.Pointer(uintptr(pdEntry & addressMask)))
				for leafIndex, leaf := range pt {
					if leaf&(entryPresent|entryUser) == entryPresent|entryUser && leaf&addressMask == physicalPage&^(PageSize-1) {
						return uint64(rootIndex)<<39 | uint64(pdptIndex)<<30 | uint64(pdIndex)<<21 | uint64(leafIndex)<<12, true
					}
				}
			}
		}
	}
	return 0, false
}

// UserLeaf returns the index-th present user leaf of a process in address
// order, for diagnostics only.
func UserLeaf(rootAddress uint64, index int) (uint64, uint64, bool) {
	if rootAddress%PageSize != 0 || rootAddress >= IdentityMapLimit {
		return 0, 0, false
	}
	// SAFETY: see FindUserMapping.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	seen := 0
	for rootIndex, rootEntry := range root {
		if rootEntry&(entryPresent|entryUser) != entryPresent|entryUser {
			continue
		}
		pdpt := (*table)(unsafe.Pointer(uintptr(rootEntry & addressMask)))
		for pdptIndex, pdptEntry := range pdpt {
			if pdptEntry&(entryPresent|entryUser) != entryPresent|entryUser {
				continue
			}
			pd := (*table)(unsafe.Pointer(uintptr(pdptEntry & addressMask)))
			for pdIndex, pdEntry := range pd {
				if pdEntry&(entryPresent|entryUser) != entryPresent|entryUser {
					continue
				}
				pt := (*table)(unsafe.Pointer(uintptr(pdEntry & addressMask)))
				for leafIndex, leaf := range pt {
					if leaf&(entryPresent|entryUser) != entryPresent|entryUser {
						continue
					}
					if seen == index {
						return uint64(rootIndex)<<39 | uint64(pdptIndex)<<30 | uint64(pdIndex)<<21 | uint64(leafIndex)<<12, leaf & addressMask, true
					}
					seen++
				}
			}
		}
	}
	return 0, 0, false
}

// resolveOrFault is resolveUser for the kernel's copies: an absent page is
// first offered to the demand-fault resolver, as the processor would when the
// process itself touched it.
func resolveOrFault(rootAddress uint64, virtual uint64, write bool) (uint64, uint64, bool) {
	physical, leaf, ok := resolveUser(rootAddress, virtual)
	if ok || faultResolver == nil || !ValidateUserRange(virtual, 1) || !faultResolver(rootAddress, virtual&^(PageSize-1), write) {
		return physical, leaf, ok
	}
	return resolveUser(rootAddress, virtual)
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

// allocateTable takes page-table pages from low memory, so every walker in
// this file can use a table's physical address as its kernel address.
func allocateTable(allocator *pmm.Allocator) (uint64, *table, bool) {
	address, ok := allocator.AllocateLow()
	if !ok || address >= IdentityMapLimit {
		return 0, nil, false
	}
	// SAFETY: allocator returned an exclusively owned, page-aligned physical
	// page below the current identity-map limit. It becomes page-table metadata.
	zeroBytes(uintptr(address), PageSize)
	return address, (*table)(unsafe.Pointer(uintptr(address))), true
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

// userLeafEntry is leafEntry for user mappings: every parent entry must be
// present and user-accessible, so a user leaf is never written into a table
// the kernel shares between address spaces.
func userLeafEntry(root *table, address uint64) *uint64 {
	if !ValidateUserRange(address, 1) {
		return nil
	}
	pml4Entry := root[(address>>39)&0x1ff]
	if pml4Entry&(entryPresent|entryUser) != entryPresent|entryUser {
		return nil
	}
	// SAFETY: present user entries were produced by allocateTable and name
	// page-table pages inside the identity map.
	pdpt := (*table)(unsafe.Pointer(uintptr(pml4Entry & addressMask)))
	pdptEntry := pdpt[(address>>30)&0x1ff]
	if pdptEntry&(entryPresent|entryUser) != entryPresent|entryUser {
		return nil
	}
	// SAFETY: same page-table ownership invariant as above.
	pd := (*table)(unsafe.Pointer(uintptr(pdptEntry & addressMask)))
	pdEntry := pd[(address>>21)&0x1ff]
	if pdEntry&(entryPresent|entryUser) != entryPresent|entryUser {
		return nil
	}
	// SAFETY: same page-table ownership invariant as above.
	pt := (*table)(unsafe.Pointer(uintptr(pdEntry & addressMask)))
	return &pt[(address>>12)&0x1ff]
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
	if pdptEntry&entryPresent == 0 || pdptEntry&entryLarge != 0 {
		return nil
	}
	// SAFETY: same page-table ownership invariant as above.
	pd := (*table)(unsafe.Pointer(uintptr(pdptEntry & addressMask)))
	pdEntry := pd[(address>>21)&0x1ff]
	// A two MiB direct-map leaf has no page table below it.
	if pdEntry&entryPresent == 0 || pdEntry&entryLarge != 0 {
		return nil
	}
	// SAFETY: same page-table ownership invariant as above.
	pt := (*table)(unsafe.Pointer(uintptr(pdEntry & addressMask)))
	return &pt[(address>>12)&0x1ff]
}
