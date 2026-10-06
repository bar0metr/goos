//go:build uefi && amd64

package vm

import (
	"goos/kernel/mm/pmm"
	"sync/atomic"
	"unsafe"
)

// User mappings anywhere in the user range (ADR 0104). Page tables below a
// process root are created on demand at every level; each function that may
// create one reports how many it created, so the caller charges them to the
// process.

//export goos_invalidate_page
func invalidatePage(address uint64)

// faultResolver supplies a demand-zero page for an absent user page, or
// reports that none belongs there. The kernel installs it once at boot; the
// copy routines below call it so that a system call may name memory its
// caller has not touched yet.
var faultResolver func(root uint64, address uint64, write bool) bool

// SetFaultResolver installs the demand-fault resolver.
func SetFaultResolver(resolver func(root uint64, address uint64, write bool) bool) {
	faultResolver = resolver
}

// shootdown makes every other processor drop its translations of a process
// root. Another task of the process may run in ring 3 on another processor
// while a call changes the process's mappings; a page an entry named goes
// back to the allocator only after the shootdown, so no stale translation
// reaches it once it has a new owner.
var shootdown func(root uint64)

// SetShootdown installs the TLB shootdown.
func SetShootdown(function func(root uint64)) { shootdown = function }

func shootdownRoot(root uint64) {
	if shootdown != nil {
		shootdown(root)
	}
}

// entryRetired marks a leaf UnmapUserRange has cleared but whose owned page
// it has not freed yet. The leaf is not present, so neither the processor
// nor any walker here uses it; it exists only between the two passes, which
// hold the address space for writing.
const entryRetired = uint64(1 << 10)

// userTable returns the table a user entry of parent names, creating it when
// the entry is empty and create is set. An entry the kernel placed — without
// the user bit, or a large leaf — is never descended into or replaced.
func userTable(allocator *pmm.Allocator, parent *table, index uint64, create bool, added *int) (*table, bool) {
	entry := atomic.LoadUint64(&parent[index])
	if entry&entryPresent == 0 {
		if !create || allocator == nil {
			return nil, false
		}
		address, child, ok := allocateTable(allocator)
		if !ok {
			return nil, false
		}
		// Faults of several tasks of a process run at once, each holding the
		// address space for reading. Two that need the same table race here;
		// the loser frees its table and descends into the winner's.
		if atomic.CompareAndSwapUint64(&parent[index], entry, address|entryPresent|entryWritable|entryUser) {
			*added++
			return child, true
		}
		freeTable(allocator, address)
		entry = atomic.LoadUint64(&parent[index])
		if entry&entryPresent == 0 {
			return nil, false
		}
	}
	if entry&entryUser == 0 || entry&entryLarge != 0 {
		return nil, false
	}
	// SAFETY: present user entries are created only by allocateTable, whose
	// pages are low pages the kernel identity map covers.
	return (*table)(unsafe.Pointer(uintptr(entry & addressMask))), true
}

// userSlot returns the leaf entry for virtual, creating the tables above it
// when create is set, and how many tables it created.
func userSlot(allocator *pmm.Allocator, rootAddress, virtual uint64, create bool) (*uint64, int, bool) {
	added := 0
	if rootAddress%PageSize != 0 || rootAddress >= IdentityMapLimit || !ValidateUserRange(virtual, 1) {
		return nil, 0, false
	}
	// SAFETY: rootAddress names a process PML4 created by this package, a low
	// page inside the identity map.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	pdpt, ok := userTable(allocator, root, (virtual>>39)&0x1ff, create, &added)
	if !ok {
		return nil, added, false
	}
	pd, ok := userTable(allocator, pdpt, (virtual>>30)&0x1ff, create, &added)
	if !ok {
		return nil, added, false
	}
	pt, ok := userTable(allocator, pd, (virtual>>21)&0x1ff, create, &added)
	if !ok {
		return nil, added, false
	}
	return &pt[(virtual>>12)&0x1ff], added, true
}

// MapUserPageAt maps one user page anywhere in the user range, creating the
// page tables it needs. It refuses a slot that is already mapped, and reports
// the tables it created even when it fails, since they stay with the address
// space.
func MapUserPageAt(allocator *pmm.Allocator, rootAddress, virtual, physical uint64, permissions Permissions, owned bool) (int, bool) {
	if virtual%PageSize != 0 || physical%PageSize != 0 || !permissions.User {
		return 0, false
	}
	entry, added, ok := userSlot(allocator, rootAddress, virtual, true)
	if !ok {
		return added, false
	}
	old := atomic.LoadUint64(entry)
	if old&entryPresent != 0 {
		return added, false
	}
	value, ok := pageEntry(physical, permissions)
	if !ok {
		return added, false
	}
	if owned {
		value |= entryOwned
	}
	// A fault that lost the race for the page finds it present and leaves.
	return added, atomic.CompareAndSwapUint64(entry, old, value)
}

// UserPageMapped reports whether a user leaf is present at virtual.
func UserPageMapped(rootAddress, virtual uint64) bool {
	entry, _, ok := userSlot(nil, rootAddress, virtual&^(PageSize-1), false)
	return ok && *entry&entryPresent != 0
}

// nextLeaf returns the first present user leaf at or above address and below
// end, skipping whole tables that are absent, and its virtual address.
func nextLeaf(root *table, address, end uint64) (*uint64, uint64) {
	return nextEntry(root, address, end, entryPresent)
}

// nextEntry is nextLeaf for the first leaf with any bit of mark set.
func nextEntry(root *table, address, end uint64, mark uint64) (*uint64, uint64) {
	for address < end {
		pml4Entry := root[(address>>39)&0x1ff]
		if pml4Entry&(entryPresent|entryUser) != entryPresent|entryUser {
			address = (address>>39 + 1) << 39
			continue
		}
		// SAFETY: present user entries name low page-table pages.
		pdpt := (*table)(unsafe.Pointer(uintptr(pml4Entry & addressMask)))
		pdptEntry := pdpt[(address>>30)&0x1ff]
		if pdptEntry&(entryPresent|entryUser) != entryPresent|entryUser || pdptEntry&entryLarge != 0 {
			address = (address>>30 + 1) << 30
			continue
		}
		// SAFETY: as above.
		pd := (*table)(unsafe.Pointer(uintptr(pdptEntry & addressMask)))
		pdEntry := pd[(address>>21)&0x1ff]
		if pdEntry&(entryPresent|entryUser) != entryPresent|entryUser || pdEntry&entryLarge != 0 {
			address = (address>>21 + 1) << 21
			continue
		}
		// SAFETY: as above.
		pt := (*table)(unsafe.Pointer(uintptr(pdEntry & addressMask)))
		for ; address < end; address += PageSize {
			leaf := &pt[(address>>12)&0x1ff]
			if *leaf&mark != 0 {
				return leaf, address
			}
			if (address+PageSize)&(LeafTableSpan-1) == 0 {
				address += PageSize
				break
			}
		}
	}
	return nil, end
}

// UnmapUserRange removes every user leaf in [base, base+length). Owned pages
// go back to the allocator; the count of them is returned so the caller can
// uncharge them. Shared, image, and device pages are only unmapped. The
// first pass clears the leaves, keeping owned pages' addresses in retired
// entries; after the shootdown the second pass frees them.
func UnmapUserRange(allocator *pmm.Allocator, rootAddress, base, length uint64) uint64 {
	if allocator == nil || rootAddress%PageSize != 0 || rootAddress >= IdentityMapLimit || base%PageSize != 0 || !ValidateUserRange(base, length) {
		return 0
	}
	// SAFETY: rootAddress names a process PML4 created by this package.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	cleared, retired := uint64(0), uint64(0)
	for address, end := base, base+length; ; {
		leaf, at := nextLeaf(root, address, end)
		if leaf == nil {
			break
		}
		value := *leaf
		*leaf = 0
		if value&entryOwned != 0 {
			*leaf = value&addressMask | entryRetired
			retired++
		}
		cleared++
		address = at + PageSize
	}
	if cleared == 0 {
		return 0
	}
	shootdownRoot(rootAddress)
	freed := uint64(0)
	for address, end := base, base+length; retired != 0; {
		leaf, at := nextEntry(root, address, end, entryRetired)
		if leaf == nil {
			break
		}
		if allocator.Free(*leaf & addressMask) {
			freed++
		}
		*leaf = 0
		retired--
		address = at + PageSize
	}
	return freed
}

// ProtectUserRange gives every present user leaf in [base, base+length) new
// permissions, keeping the page and its ownership. W^X is enforced by
// pageEntry.
func ProtectUserRange(rootAddress, base, length uint64, permissions Permissions) bool {
	if rootAddress%PageSize != 0 || rootAddress >= IdentityMapLimit || base%PageSize != 0 || !ValidateUserRange(base, length) ||
		!permissions.User || !permissions.Valid() {
		return false
	}
	// SAFETY: rootAddress names a process PML4 created by this package.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	changed := false
	for address, end := base, base+length; ; {
		leaf, at := nextLeaf(root, address, end)
		if leaf == nil {
			break
		}
		value, _ := pageEntry(*leaf&addressMask, permissions)
		*leaf = value | *leaf&entryOwned
		changed = true
		address = at + PageSize
	}
	if changed {
		shootdownRoot(rootAddress)
	}
	return true
}

// UserPageAllows reports whether the present user leaf at virtual permits
// the access. A fault on such a page came from a translation another
// processor has since replaced, and only needs a retry.
func UserPageAllows(rootAddress, virtual uint64, write, fetch bool) bool {
	entry, _, ok := userSlot(nil, rootAddress, virtual&^(PageSize-1), false)
	if !ok || *entry&(entryPresent|entryUser) != entryPresent|entryUser {
		return false
	}
	return (!write || *entry&entryWritable != 0) && (!fetch || *entry&entryNX == 0)
}

// UserPageCount counts the present user leaves in [base, base+length) and how
// many of them are owned.
func UserPageCount(rootAddress, base, length uint64) (uint64, uint64) {
	if rootAddress%PageSize != 0 || rootAddress >= IdentityMapLimit || base%PageSize != 0 || !ValidateUserRange(base, length) {
		return 0, 0
	}
	// SAFETY: rootAddress names a process PML4 created by this package.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	present, owned := uint64(0), uint64(0)
	for address, end := base, base+length; ; {
		leaf, at := nextLeaf(root, address, end)
		if leaf == nil {
			return present, owned
		}
		present++
		if *leaf&entryOwned != 0 {
			owned++
		}
		address = at + PageSize
	}
}

// CopyUserRange gives target a private copy of every present user leaf of
// source in [base, base+length), with the same permission bits. Each copy is a
// fresh page target owns. A leaf source does not own is shared or device
// memory, which no copy may duplicate, so the call stops there and fails.
// Pages and tables already placed in target stay there for the caller to
// destroy with the address space.
func CopyUserRange(allocator *pmm.Allocator, sourceRoot, targetRoot, base, length uint64) bool {
	return CopyUserRangeLimited(allocator, sourceRoot, targetRoot, base, length, 0)
}

// CopyUserRangeLimited is CopyUserRange that fails once it has copied limit
// pages, when limit is not zero. Validation builds use it to fail a fork in
// the middle of its copy.
func CopyUserRangeLimited(allocator *pmm.Allocator, sourceRoot, targetRoot, base, length, limit uint64) bool {
	copied := uint64(0)
	if allocator == nil || sourceRoot == targetRoot || sourceRoot%PageSize != 0 || sourceRoot >= IdentityMapLimit ||
		base%PageSize != 0 || !ValidateUserRange(base, length) {
		return false
	}
	// SAFETY: sourceRoot names a process PML4 created by this package.
	root := (*table)(unsafe.Pointer(uintptr(sourceRoot)))
	for address, end := base, base+length; ; {
		leaf, at := nextLeaf(root, address, end)
		if leaf == nil {
			return true
		}
		value := *leaf
		if value&entryOwned == 0 || limit != 0 && copied == limit {
			return false
		}
		source, mapped := PhysicalPage(value & addressMask)
		physical, allocated := allocator.Allocate()
		if !mapped || !allocated {
			if allocated {
				allocator.Free(physical)
			}
			return false
		}
		slot, _, found := userSlot(allocator, targetRoot, at, true)
		if !found || *slot&entryPresent != 0 || !InitializePhysical(physical, source) {
			allocator.Free(physical)
			return false
		}
		*slot = physical | value&^(addressMask|entryAccessed|entryDirty)
		copied++
		address = at + PageSize
	}
}
