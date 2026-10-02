package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
)

func TestAnonymousMappingAcceptsOnlyBoundedPrivateAnonymousMemory(t *testing.T) {
	arguments := [6]uint64{0, 4097, abi.ProtRead | abi.ProtWrite, abi.MapPrivate | abi.MapAnonymous, ^uint64(0), 0}
	length, protection, errno := AnonymousMapping(arguments, 4096)
	if errno != 0 || length != 8192 || protection != abi.ProtRead|abi.ProtWrite {
		t.Fatalf("mapping = length=%d protection=%d errno=%d", length, protection, errno)
	}
	// The descriptor is ignored: index 4 has no invalid value.
	for index, value := range []uint64{1, 0, abi.ProtExec, abi.MapAnonymous, 4: 0, 5: 1} {
		if index == 4 {
			continue
		}
		invalid := arguments
		invalid[index] = value
		if _, _, errno := AnonymousMapping(invalid, 4096); errno != abi.EINVAL {
			t.Fatalf("argument %d accepted: %#v", index, invalid)
		}
	}
	shared := arguments
	shared[3] = 0x01 | abi.MapAnonymous
	if _, _, errno := AnonymousMapping(shared, 4096); errno != abi.EINVAL {
		t.Fatal("MAP_SHARED accepted")
	}
	// glibc's thread stacks and arenas: a 32-bit -1 descriptor, PROT_NONE,
	// and the MAP_STACK and MAP_NORESERVE hints.
	stack := [6]uint64{0, 8 << 20, abi.ProtNone, abi.MapPrivate | abi.MapAnonymous | abi.MapStack | abi.MapNoreserve, 0xffffffff, 0}
	if length, _, errno := AnonymousMapping(stack, 4096); errno != 0 || length != 8<<20 {
		t.Fatalf("thread stack mapping: length %d errno %d", length, errno)
	}
}

func TestMemoryProtectionRejectsExecutableAndWriteOnly(t *testing.T) {
	for _, accepted := range []uint64{abi.ProtNone, abi.ProtRead, abi.ProtRead | abi.ProtWrite} {
		if _, errno := MemoryProtection(accepted); errno != 0 {
			t.Fatalf("protection %d rejected", accepted)
		}
	}
	for _, rejected := range []uint64{abi.ProtWrite, abi.ProtExec, abi.ProtRead | abi.ProtExec, 8} {
		if _, errno := MemoryProtection(rejected); errno != abi.EINVAL {
			t.Fatalf("protection %d accepted", rejected)
		}
	}
}

func TestPageRangeRejectsUnalignedAndOverflow(t *testing.T) {
	if length, errno := PageRange(0x2000, 1, 4096); errno != 0 || length != 4096 {
		t.Fatalf("rounded = %d errno=%d", length, errno)
	}
	for _, test := range [][2]uint64{{0x2001, 1}, {0x2000, 0}, {0x2000, ^uint64(0)}, {^uint64(0) - 4095, 4096}} {
		if _, errno := PageRange(test[0], test[1], 4096); errno != abi.EINVAL {
			t.Fatalf("range %#x+%#x accepted", test[0], test[1])
		}
	}
}

func TestUserFSBaseStaysInCanonicalUserHalf(t *testing.T) {
	const limit = uint64(0x00007ffffffff000)
	if !UserFSBase(0, limit) || !UserFSBase(limit-1, limit) || UserFSBase(limit, limit) || UserFSBase(^uint64(0), limit) {
		t.Fatal("FS-base canonicality policy failed")
	}
}

func TestBrkBoundsFollowImageAndStayBounded(t *testing.T) {
	base, limit, ok := BrkBounds(0x401001, 4096, 0x10000000)
	if !ok || base != 0x402000 || limit-base != BrkReserve {
		t.Fatalf("brk bounds = %#x..%#x ok=%v", base, limit, ok)
	}
	if _, _, ok := BrkBounds(0x0ffff000, 4096, 0x10000000); ok {
		t.Fatal("undersized brk range accepted")
	}
}
