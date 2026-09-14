//go:build uefi && amd64

package amd64

import "goos/kernel/mm/vm"

func unmapGuardPage(pageTable uint64, address uint64) bool {
	if !vm.UnmapPage(pageTable, address) {
		return false
	}
	invalidatePage(address)
	return !vm.IsMapped(pageTable, address)
}
