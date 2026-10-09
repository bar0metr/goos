//go:build goos_userspace

package arena

import (
	"goos/internal/timekeeping"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	_ "unsafe"
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
//
//go:linkname nativePutchar goos_runtime_putchar
func nativePutchar(c byte) {
	data, ok := startup.Read()
	if !ok {
		return
	}
	stderr, ok := data.Handle("stderr")
	if ok {
		buffer := [1]byte{c}
		sys.StreamWrite(stderr, buffer[:])
	}
}

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
