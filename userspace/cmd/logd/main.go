//go:build goos_userspace

// The log service is the persistent backend of the system log. The kernel ring
// is bounded and volatile: it holds the newest records and loses them at reset,
// which is exactly when a log is most wanted. This service drains the ring into
// rotated files on the persistent volume so that what happened before a reboot
// can be read after one.
//
// It holds no authority beyond the log capability that authorises reading the
// ring, console write for its own records, and a VFS session for its own
// files, and it never becomes a path that other services log through: they
// keep writing to the kernel ring, which stays the single ordering point.
//
// The service blocks on the ring with its cursor. A new record wakes it, it
// drains batches until the cursor reaches the ring's end, and it blocks
// again. The wait's timeout runs maintenance only: reading settings, retrying
// storage that failed, and reporting counters.
package main

import (
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/config"
	syslog "goos/userspace/log"
	"goos/userspace/logpersist"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
)

const (
	// logPath is on /state because a log is mutable system state rather than
	// user data or part of the immutable system image. Older segments are
	// logPath.1, logPath.2 and so on.
	logPath = "/state/system.log"
	// statusPath holds the counters for `log status`; statusScratch is
	// where the next one is written before it replaces the last.
	statusPath    = logPath + logpersist.StatusSuffix
	statusScratch = statusPath + ".new"
	// maintenanceTicks is how often the service rereads its settings and
	// retries storage that failed. Records never wait for it.
	maintenanceTicks = 5 * clock.TicksPerSecond
	// reportEvery is how many maintenance rounds pass between counter
	// reports when nothing went wrong: one an hour.
	reportEvery      = 720
	vfsReplyDeadline = uint64(200)
	vfsReplyAttempts = 5
)

var (
	consoleHandle handle.ID
	// logHandle authorizes reading the kernel ring. It is not the console:
	// the service can drain the log without being able to read the keyboard.
	logHandle    handle.ID
	vfsHandle    handle.ID
	configHandle handle.ID
	// controlHandle is the channel PID 1 hands a new filesystem session over
	// when the VFS was restarted and the old one died with it (ADR 0099).
	controlHandle handle.ID
	configWire    [ipc.WireMaxSize]byte
	// persisting is the setting that governs this service, reread at each
	// maintenance round.
	persisting  = true
	wire        [ipc.WireMaxSize]byte
	correlation uint64
	// reported bounds how often a persistent failure is announced, so a full
	// or broken volume cannot turn every drain into more log records.
	reported bool
	// vfsClosed is set once the filesystem session's peer is gone; the
	// service stops writing until PID 1 hands over a new one.
	vfsClosed bool
	// failing holds off draining until the next maintenance round after
	// storage failed, so a broken volume is retried, not spun on.
	failing bool

	persister logpersist.Persister
	ring      kernelRing
	files     vfsFiles
	// policyRefused reports a refused rotation setting once.
	policyRefused bool
	// lastReport holds the counters as last reported.
	lastReport logpersist.Stats
	// written holds the counters and policy the status file last got;
	// statusWritten says whether it got any.
	written       logpersist.Stats
	writtenPolicy logpersist.Policy
	statusWritten bool
	statusText    [logpersist.MaxStatus]byte
	rounds        int
	interests     [userwait.MaxObjects]userwait.CursorInterest
)

//export userspaceMain
func userspaceMain() {
	arena.InitService()
	arena.OnExhausted(func() { fatal("LOG_OUT_OF_MEMORY\n") })
	data, ok := startup.Read()
	if !ok {
		fatal("LOG_STARTUP_INVALID\n")
	}
	consoleHandle, ok = data.Handle("console")
	syslog.Init(consoleHandle, "log")
	if !ok {
		fatal("LOG_CONSOLE_MISSING\n")
	}
	logHandle, ok = data.Handle("log")
	if !ok {
		fatal("LOG_RING_CAPABILITY_MISSING\n")
	}
	vfsHandle, ok = data.Handle("vfs")
	if !ok {
		fatal("LOG_VFS_MISSING\n")
	}
	// The configuration session is optional: without one the service keeps
	// its build's defaults, which are to persist with the default rotation.
	configHandle, _ = data.Handle("config")
	controlHandle, _ = data.Handle("control")
	_, restarted := data.Env(startup.RestartVariable)
	if !persister.Init(&ring, &files, logPath, logpersist.DefaultPolicy(validation.Enabled)) {
		fatal("LOG_POLICY_INVALID\n")
	}
	maintain()
	if !persister.Start(restarted) {
		fatal("LOG_RING_UNAVAILABLE\n")
	}
	mark("LOG_SERVICE_OK\n")
	policy := persister.Policy()
	syslog.Info("persisting", syslog.Str("path", logPath), syslog.Uint("segment-bytes", policy.SegmentBytes), syslog.Uint("segments", uint64(policy.Segments)))
	if restarted {
		// A service started again in the same boot resumes after the last
		// record its predecessor wrote, so a crash writes nothing twice
		// (ADR 0100).
		syslog.Info("resumed", syslog.Uint("cursor", persister.Cursor()))
		mark("LOG_RESUMED_OK\n")
	}
	next := now() + maintenanceTicks
	for {
		takeSession()
		if !before(now(), next) {
			maintain()
			failing = false
			next = now() + maintenanceTicks
		}
		waitLog := false
		switch {
		case !persisting:
			// The cursor follows the ring. Re-enabling persistence starts
			// from the present rather than writing out what happened while
			// it was off.
			persister.Skip()
		case failing || vfsClosed:
		default:
			switch persister.Drain() {
			case logpersist.CaughtUp:
				reported = false
				waitLog = true
			case logpersist.More:
				continue
			case logpersist.Failed:
				announce("log write failed")
				failing = true
			}
		}
		count := 0
		if waitLog {
			interests[count] = userwait.CursorInterest{Handle: logHandle, Events: kcall.WaitReadable, Cursor: persister.Cursor()}
			count++
		}
		if controlHandle != 0 {
			interests[count] = userwait.CursorInterest{Handle: controlHandle, Events: kcall.WaitReadable}
			count++
		}
		if !vfsClosed {
			interests[count] = userwait.CursorInterest{Handle: vfsHandle, Events: kcall.WaitPeerClosed}
			count++
		}
		timeout := uint64(1)
		if current := now(); before(current, next) {
			timeout = next - current
		}
		result, status := userwait.AnyCursor(&interests, count, timeout)
		if status == kcall.StatusOK && !vfsClosed && result.ReadyMask&(1<<(count-1)) != 0 {
			vfsClosed = true
			syslog.Warn("filesystem session closed")
		}
	}
}

func now() uint64 {
	ticks, _ := sys.ClockRead()
	return ticks
}

func before(left, right uint64) bool { return int64(left-right) < 0 }

// maintain rereads the settings and reports the counters when something
// went wrong since the last report, or once an hour.
func maintain() {
	refreshSettings()
	rounds++
	stats := persister.Stats
	saveStatus(stats)
	changed := stats.Lost != lastReport.Lost || stats.Failures != lastReport.Failures ||
		stats.RotationFailures != lastReport.RotationFailures || stats.Repaired != lastReport.Repaired
	if !changed && rounds < reportEvery {
		return
	}
	rounds = 0
	lastReport = stats
	// Two records, because the fields of one are bounded.
	syslog.Info("counters", syslog.Uint("appended", stats.Appended), syslog.Uint("overwritten", stats.Overwritten),
		syslog.Uint("persisted", stats.Persisted), syslog.Uint("lost", stats.Lost), syslog.Uint("failed", stats.Failures))
	syslog.Info("rotation counters", syslog.Uint("rotations", stats.Rotations), syslog.Uint("refused", stats.RotationFailures),
		syslog.Uint("repaired", stats.Repaired), syslog.Uint("cut-bytes", stats.RepairedBytes))
}

// saveStatus rewrites the status file when the counters or the policy
// changed since the last write. It writes nothing while persistence is off
// or storage is failing, and a failed write is retried next round without a
// record, as the log itself would carry it.
func saveStatus(stats logpersist.Stats) {
	policy := persister.Policy()
	if !persisting || failing || vfsClosed || statusWritten && stats == written && policy == writtenPolicy {
		return
	}
	text := logpersist.AppendStatus(statusText[:0], stats, policy)
	if writeStatus([]byte(statusPath), []byte(statusScratch), text) {
		written, writtenPolicy, statusWritten = stats, policy, true
	}
}

// refreshSettings asks the configuration service whether to persist and how
// to rotate. A session it cannot reach leaves the last known values in place:
// configuration governs the service, it does not decide whether the service
// works. A rotation setting out of bounds is refused with an error record.
func refreshSettings() {
	if configHandle == 0 {
		return
	}
	if value, ok := setting("system.log.persist", config.KindBool); ok && (value == 1) != persisting {
		persisting = value == 1
		syslog.Info("persistence changed", syslog.Uint("enabled", value))
	}
	policy := persister.Policy()
	if value, ok := setting("system.log.segment-bytes", config.KindUint); ok {
		policy.SegmentBytes = value
	}
	if value, ok := setting("system.log.segments", config.KindUint); ok {
		policy.Segments = int(min(value, logpersist.MaxSegments+1))
	}
	if policy == persister.Policy() {
		policyRefused = false
		return
	}
	if persister.SetPolicy(policy) {
		policyRefused = false
		syslog.Info("rotation changed", syslog.Uint("segment-bytes", policy.SegmentBytes), syslog.Uint("segments", uint64(policy.Segments)))
		return
	}
	if !policyRefused {
		policyRefused = true
		syslog.Error("rotation setting refused", syslog.Uint("segment-bytes", policy.SegmentBytes), syslog.Uint("segments", uint64(policy.Segments)))
	}
}

func setting(key string, kind config.Kind) (uint64, bool) {
	reply, ok := callConfig(config.Request{Operation: config.OperationGet, Key: []byte(key)})
	if !ok || reply.Status != config.StatusOK || reply.Value.Kind != kind {
		return 0, false
	}
	return reply.Value.Uint, true
}

// takeSession installs a filesystem session PID 1 has handed over, if there
// is one. The records produced while the VFS was down are still in the ring,
// and the cursor still names the first of them, so the next pass persists them
// without writing any twice.
func takeSession() {
	for controlHandle != 0 && sys.IPCReceive(controlHandle, wire[:]) == 0 {
		payloadSize := int(wire[24]) | int(wire[25])<<8
		message, valid := ipc.Decode(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8])
		if !valid {
			continue
		}
		if settings, ok := config.DecodeSessionNotice(&message); ok {
			// The configuration service was restarted and took the old
			// session with it (ADR 0100).
			if configHandle != 0 {
				sys.HandleClose(configHandle)
			}
			configHandle = settings
			mark("LOG_CONFIG_RENEWED_OK\n")
			continue
		}
		session, ok := vfs.DecodeSessionNotice(&message)
		if !ok {
			sys.HandleClose(message.Handles[0])
			continue
		}
		sys.HandleClose(vfsHandle)
		vfsHandle, reported, vfsClosed, failing = session, false, false, false
		syslog.Info("filesystem session renewed")
		mark("LOG_SESSION_RENEWED_OK\n")
	}
}

func callConfig(request config.Request) (config.Reply, bool) {
	correlation++
	message, ok := config.EncodeRequest(request, correlation)
	if !ok {
		return config.Reply{}, false
	}
	size, ok := ipc.Encode(message, configWire[:])
	if !ok || sys.IPCSend(configHandle, configWire[:size]) != 0 {
		return config.Reply{}, false
	}
	if sys.IPCWaitReceive(configHandle, configWire[:], vfsReplyDeadline) != 0 {
		return config.Reply{}, false
	}
	payloadSize := int(configWire[24]) | int(configWire[25])<<8
	received, valid := ipc.Decode(configWire[:ipc.WireHeaderSize+payloadSize+int(configWire[26])*8])
	if !valid || received.Correlation != correlation {
		return config.Reply{}, false
	}
	reply, valid := config.DecodeReply(received)
	return reply, valid && reply.Operation == request.Operation
}

// announce reports a persistent failure once. Logging from the log service
// produces records this service will itself try to persist, so repeating a
// failure every pass would turn a broken volume into a growing ring of
// complaints about it.
func announce(message string) {
	if reported {
		return
	}
	reported = true
	syslog.Error(message, syslog.Str("path", logPath))
}

func mark(message string) {
	if validation.Enabled {
		write(message)
	}
}

func write(message string) {
	sys.ConsoleWriteString(consoleHandle, message)
}

func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
	sys.Diagnostics(consoleHandle)
	for {
	}
}

func main() {}
