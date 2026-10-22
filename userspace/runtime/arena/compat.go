//go:build goos_userspace

package arena

import (
	"goos/internal/timekeeping"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"unsafe"
)

//go:linkname initializePackages goos_runtime_init_packages
func initializePackages()

// InitPackages runs the pinned standard-library initializers after heap setup.
// Only consumers built with the native runtime overlay call it.
func InitPackages() {
	Init()
	initializePackages()
}

// These hooks serve the opt-in native runtime overlay. They do not replace
// the kernel's UEFI runtime or call firmware from a native process.

//go:linkname panicWriteByte goos_panic_write_byte
func panicWriteByte(stream uint64, value byte)

//
//go:linkname nativePutchar goos_runtime_putchar
func nativePutchar(c byte) {
	if stream := panicStderr(); stream != 0 {
		panicWriteByte(stream, c)
	}
}

// panicStderr reads the validated startup mapping directly. Panic output
// must not allocate: an allocator panic would otherwise recurse through
// startup.Read's escaping header and exhaust the goroutine stack.
func panicStderr() uint64 {
	base := uintptr(vm.UserStartupAddress)
	if panicRead32(base) != 0x34545347 || panicRead16(base+4) != startup.Version {
		return 0
	}
	size := uint64(panicRead32(base + 8))
	if size < startup.HeaderSize || size > startup.MaxSize {
		return 0
	}
	arguments, environment := uint64(panicRead32(base+12)), uint64(panicRead32(base+16))
	handles := uint64(panicRead16(base + 6))
	if arguments > size || environment > size || handles > startup.MaxHandles {
		return 0
	}
	table := uint64(startup.HeaderSize) + (2+arguments+environment)*8
	strings := table + handles*16
	if strings > size || uint64(panicRead32(base+20)) != strings {
		return 0
	}
	for index := uint64(0); index < handles; index++ {
		entry := uintptr(table + index*16)
		offset, length := uint64(panicRead32(base+entry)), panicRead32(base+entry+4)
		if length != 6 || offset < strings || offset+6 > size {
			continue
		}
		if panicRead32(base+uintptr(offset)) == 0x65647473 &&
			panicRead16(base+uintptr(offset)+4) == 0x7272 {
			return panicRead64(base + entry + 8)
		}
	}
	return 0
}

func panicRead16(address uintptr) uint16 { return *(*uint16)(unsafe.Pointer(address)) }
func panicRead32(address uintptr) uint32 { return *(*uint32)(unsafe.Pointer(address)) }
func panicRead64(address uintptr) uint64 { return *(*uint64)(unsafe.Pointer(address)) }

//go:linkname nativeExit goos_runtime_exit
func nativeExit(code int) { sys.Exit(uint64(code)) }

//go:linkname nativeTicks goos_runtime_ticks
func nativeTicks() int64 {
	value, result := sys.ClockNow(timekeeping.Monotonic)
	if result != kcall.StatusOK || value.Seconds < 0 || value.Seconds > (1<<63-1)/1000000000 || value.Nanoseconds >= 1000000000 {
		sys.Exit(1)
	}
	seconds := value.Seconds * 1000000000
	if seconds > (1<<63-1)-int64(value.Nanoseconds) {
		sys.Exit(1)
	}
	return seconds + int64(value.Nanoseconds)
}

//go:linkname nativeRandom goos_runtime_random
func nativeRandom(dst []byte) {
	if sys.RandomRead(dst) != kcall.StatusOK {
		sys.Exit(1)
	}
}

//go:linkname nativeNow goos_runtime_now
func nativeNow() (sec int64, nsec int32, mono int64) {
	value, result := sys.ClockNow(timekeeping.Realtime)
	if result != kcall.StatusOK || value.Seconds <= 0 || value.Nanoseconds >= 1000000000 {
		sys.Exit(1)
	}
	return value.Seconds, int32(value.Nanoseconds), nativeTicks()
}
