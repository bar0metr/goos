//go:build goos_userspace

// The ext4 filesystem service (ADR 0221, ADR 0222, ADR 0226). One process
// serves one additional volume: it parses the untrusted on-disk format,
// reaches its block slice through the block service, and answers the VFS over
// the filesystem protocol. It holds no namespace authority, no device, and no
// other volume's slice. The slice decides the mode: a writable slice gets a
// writable mount, which recovers the journal first; a read-only slice gets
// a read-only mount. A volume it cannot serve safely is refused, and the
// service keeps answering with the refusal rather than crashing in a restart
// loop.
package main

import (
	"goos/internal/timekeeping"
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/userspace/ext4"
	"goos/userspace/ext4vol"
	"goos/userspace/fsproto"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
)

// requestWait bounds one wait for the next request; waking without one is not
// an error.
const requestWait = 100000

// commitInterval is how long finished mutations may wait in memory before
// the service commits them on its own (ADR 0227).
const commitInterval = clock.TicksPerSecond

var (
	consoleHandle  handle.ID
	vfsHandle      handle.ID
	blockHandle    handle.ID
	wire           [ipc.WireMaxSize]byte
	requestMessage ipc.Message
	replyMessage   ipc.Message
	listedName     [ramfs.MaxName]byte
	requestedName  [ramfs.MaxName]byte
	// cache is the file cache this instance shares with the VFS. PID 1
	// creates it for this launch only (ADR 0222).
	cache  []byte
	volume ext4vol.Volume
	// device pools block clients so concurrent readers overlap block IPC
	// (ADR 0232).
	device *pooledDevice
)

// realtime stamps inode times with UTC Unix time; a clock that cannot be
// read stamps zero.
func realtime() ext4.Timestamp {
	now, status := sys.ClockNow(timekeeping.Realtime)
	if status != kcall.StatusOK || now.Seconds < 0 || !now.Valid() {
		return ext4.Timestamp{}
	}
	return ext4.Timestamp{Seconds: now.Seconds, Nanos: now.Nanoseconds}
}

//export userspaceMain
func userspaceMain() {
	initRuntime()
	arena.OnExhausted(func() { fatal("EXT4_OUT_OF_MEMORY\n") })
	data, ok := startup.Read()
	if !ok {
		fatal("EXT4_STARTUP_INVALID\n")
	}
	consoleHandle, ok = data.Handle("console")
	syslog.Init(consoleHandle, "ext4")
	if !ok {
		fatal("EXT4_CONSOLE_MISSING\n")
	}
	if vfsHandle, ok = data.Handle("vfs"); !ok {
		fatal("EXT4_VFS_MISSING\n")
	}
	if blockHandle, ok = data.Handle("block"); !ok {
		fatal("EXT4_BLOCK_MISSING\n")
	}
	cacheHandle, ok := data.Handle("cache")
	if !ok {
		fatal("EXT4_CACHE_MISSING\n")
	}
	base, size, status := sys.MemoryMapAnywhere(cacheHandle, true)
	if status != kcall.StatusOK || size != fsproto.CacheSize {
		fatal("EXT4_CACHE_UNMAPPED\n")
	}
	cache = sys.Window(base, size)
	device = newPooledDevice(maxReadWorkers)
	device.Capacity()
	mounted, mode := false, "read-only"
	if device.writable {
		mounted, mode = volume.MountWritable(device, realtime), "read-write"
	} else {
		mounted = volume.Mount(device)
	}
	if mounted {
		mark("EXT4_SERVICE_OK\n")
		status, _ := volume.Status()
		syslog.Info("volume mounted", syslog.Uint("capacity-mib", device.capacity>>20), syslog.Str("mode", mode),
			syslog.Uint("replayed", uint64(status.Recovery.Transactions)), syslog.Uint("orphans", uint64(status.Recovery.Orphans)))
		logCounters()
	} else {
		syslog.Error("volume refused", syslog.Str("reason", volume.Refusal()))
		mark("EXT4_VOLUME_REFUSED\n")
	}
	runService()
}

// exitIfFailed ends the instance after the device refused a write or a
// flush: the driver may have been replaced under the volume. A new
// instance replays the journal; this one could only answer errors.
func exitIfFailed() {
	if volume.Failed() {
		syslog.Error("volume failed", syslog.Str("action", "exit for recovery"))
		sys.Exit(exitVolumeFailed)
	}
}

// logCounters reports the volume's cache and listing-prefetch counters so a
// benchmark run can attribute readdir time to device reads (ADR 0231). The
// wire record caps fields at 96 bytes, so the counters travel in two lines.
func logCounters() {
	if status, ok := volume.Status(); ok {
		syslog.Warn("counters", syslog.Uint("cache-reads", status.CacheReads), syslog.Uint("cache-hits", status.CacheHits))
		syslog.Warn("prefetch", syslog.Uint("reads", status.PrefetchReads), syslog.Uint("fetches", status.PrefetchFetches))
	}
}

// ticks reads the monotonic clock. A failed read reports the largest
// time, so the service commits at once instead of waiting on a clock it
// cannot read.
func ticks() uint64 {
	now, status := sys.ClockRead()
	if status != kcall.StatusOK {
		return ^uint64(0)
	}
	return now
}

// answer executes one decoded request against the volume and returns its
// reply. It runs on the owner for mutations and on read workers for the
// read-only operations; the file cache slots are request-named, so concurrent
// readers of different slots do not collide.
func answer(request fsproto.Request, name *[ramfs.MaxName]byte) fsproto.Reply {
	if request.Operation == fsproto.OperationUnmount {
		// The counters come before the unmount clears the volume, so the
		// run log keeps the listing's device-read attribution (ADR 0231).
		logCounters()
	}
	return fsproto.Serve(&volume, request, cache, name)
}

// sendReply encodes and sends one reply over the VFS endpoint. It runs only
// on the owner.
func sendReply(reply fsproto.Reply, correlation uint64) {
	if !fsproto.EncodeReply(reply, correlation, &replyMessage, cache) {
		fatal("EXT4_REPLY_FAILED\n")
	}
	size, ok := ipc.EncodeFrom(&replyMessage, wire[:])
	if !ok {
		fatal("EXT4_REPLY_FAILED\n")
	}
	if sys.IPCSend(vfsHandle, wire[:size]) != kcall.StatusOK {
		syslog.Warn("reply undeliverable")
	}
}

func wireSize(data []byte) int {
	size := ipc.WireHeaderSize + (int(data[24]) | int(data[25])<<8) + int(data[26])*8
	if size > len(data) {
		return len(data)
	}
	return size
}

func mark(message string) {
	if validation.Enabled {
		sys.ConsoleWriteString(consoleHandle, message)
	}
}

// Exit codes. None is zero, so the supervisor counts each exit as a failure
// unless PID 1 planned the stop.
const (
	exitFailed    = 1
	exitVFSGone   = 2
	exitBlockGone = 3
	// exitVolumeFailed: a write or flush failed and the journal needs a
	// replay by the next writable mount.
	exitVolumeFailed = 4
)

func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		sys.ConsoleWriteString(consoleHandle, message)
	}
	sys.Diagnostics(consoleHandle)
	sys.Exit(exitFailed)
}

func main() {}
