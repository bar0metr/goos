//go:build uefi && amd64

package amd64

import "goos/kernel/mm/vm"

// unmapGuardPages removes the identity mapping of the stackGuardPages pages
// at address, the guard below a kernel stack.
func unmapGuardPages(pageTable uint64, address uint64) bool {
	for page := uint64(0); page < stackGuardPages; page++ {
		guard := address + page*vm.PageSize
		if !vm.UnmapPage(pageTable, guard) {
			return false
		}
		invalidatePage(guard)
		if vm.IsMapped(pageTable, guard) {
			return false
		}
	}
	return true
}
