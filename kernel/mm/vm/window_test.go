package vm_test

import (
	"testing"

	"goos/kernel/boot"
	"goos/kernel/mm/vm"
)

// The filesystem service maps the whole boot archive into the image window, so
// an archive the kernel accepts must fit there. The window once stayed at two
// MiB after the archive bound grew to four, and a 2.1 MiB archive booted with
// no /system and no prompt.
func TestImageWindowHoldsTheLargestBootArchive(t *testing.T) {
	window := vm.UserImageLimit - vm.UserImageAddress
	if window < boot.MaxInitramfs {
		t.Fatalf("image window is %d bytes, the kernel loads archives up to %d", window, boot.MaxInitramfs)
	}
	if vm.UserImageAddress%vm.LeafTableSpan != 0 || window%vm.LeafTableSpan != 0 {
		t.Fatalf("image window [%#x, %#x) is not made of whole leaf tables", vm.UserImageAddress, vm.UserImageLimit)
	}
	if vm.UserImageLimit > vm.UserAddressLimit {
		t.Fatalf("image window ends at %#x, past the user range", vm.UserImageLimit)
	}
}
