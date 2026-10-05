//go:build uefi && amd64

package amd64

import "goos/kernel/mm/vm"

// unmapGuardPages removes the higher-half aliases below a kernel stack. Other
// processors flush their stale translations of the guard before they next
// switch tasks.
func unmapGuardPages(pageTable uint64, address uint64) bool {
	KernelMappingsChanged()
	for page := uint64(0); page < stackGuardPages; page++ {
		guard := address + page*vm.PageSize
		alias, ok := vm.KernelAlias(guard)
		if !ok || !vm.UnmapPage(pageTable, alias) {
			return false
		}
		invalidatePage(guard)
		invalidatePage(alias)
		if vm.IsMapped(pageTable, alias) {
			return false
		}
	}
	return true
}
