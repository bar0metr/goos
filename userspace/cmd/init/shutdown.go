//go:build goos_userspace

package main

// Shutdown: stopping services and flushing storage before a reset or power-off
// (ADR 0061, 0092).

import (
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

const remoteShutdownDeadline = 3 * clock.TicksPerSecond

var pendingShutdown struct {
	grace    supervisor.ShutdownGrace
	powerOff bool
}

// shutDown syncs storage. A remote shell gets an acceptance reply and a
// bounded opportunity to close its SSH transport before the machine action.
func shutDown(powerOff bool, correlationOfShell uint64) {
	if pendingShutdown.grace.Session != 0 {
		sendControlStatus(execproto.ControlInvalid, correlationOfShell)
		return
	}
	name := "reboot"
	if powerOff {
		name = "poweroff"
	}
	syslog.Info("shutdown requested", syslog.Str("action", name))
	correlation++
	sync, encoded := vfs.SyncRequest(correlation)
	if !encoded {
		sendControlStatus(execproto.ControlSyncFailed, correlationOfShell)
		return
	}
	synced, status, _ := filesystemRequests.Control(handle.ID(vfsControlHandle), &sync, vfsSessionDeadline)
	if status != ipc.StatusOK || synced != vfs.StatusOK {
		syslog.Error("shutdown: storage sync failed", syslog.Str("action", name))
		sendControlStatus(execproto.ControlSyncFailed, correlationOfShell)
		return
	}
	if worker := remoteSession(replying); worker != nil {
		if powerOff {
			switch sys.PowerOffReady(handle.ID(systemControlHandle)) {
			case kcall.StatusOK:
			case kcall.StatusUnsupported:
				sendControlStatus(execproto.ControlUnsupported, correlationOfShell)
				return
			default:
				sendControlStatus(execproto.ControlResetFailed, correlationOfShell)
				return
			}
		}
		now, _ := sys.ClockRead()
		if !pendingShutdown.grace.Begin(worker.id, now, remoteShutdownDeadline) {
			sendControlStatus(execproto.ControlInvalid, correlationOfShell)
			return
		}
		pendingShutdown.powerOff = powerOff
		sendControlStatus(execproto.ControlAccepted, correlationOfShell)
		return
	}
	performShutdown(powerOff, correlationOfShell)
}

func performShutdown(powerOff bool, correlationOfShell uint64) {
	if !powerOff {
		write("\nRebooting...\n")
		sys.SystemReset(handle.ID(systemControlHandle))
		syslog.Error("reboot: reset refused")
		if correlationOfShell != 0 {
			sendControlStatus(execproto.ControlResetFailed, correlationOfShell)
		}
		return
	}
	write("\nPowering off...\n")
	if sys.SystemPowerOff(handle.ID(systemControlHandle)) == kcall.StatusUnsupported {
		syslog.Warn("poweroff: no ACPI power-off on this machine")
		if correlationOfShell != 0 {
			sendControlStatus(execproto.ControlUnsupported, correlationOfShell)
		}
		return
	}
	syslog.Error("poweroff: power-off refused")
	if correlationOfShell != 0 {
		sendControlStatus(execproto.ControlResetFailed, correlationOfShell)
	}
}

// finishPendingShutdown runs after the requesting worker has released its
// connection, or when the close deadline expires.
func finishPendingShutdown() {
	if pendingShutdown.grace.Session == 0 {
		return
	}
	now, _ := sys.ClockRead()
	present := false
	for index := range remoteWorkers {
		if remoteWorkers[index].id == pendingShutdown.grace.Session {
			present = true
			break
		}
	}
	if !pendingShutdown.grace.Ready(now, present) {
		return
	}
	powerOff := pendingShutdown.powerOff
	pendingShutdown.grace = supervisor.ShutdownGrace{}
	performShutdown(powerOff, 0)
}

func shutdownTimeout() (uint64, bool) {
	now, _ := sys.ClockRead()
	return pendingShutdown.grace.Remaining(now)
}
