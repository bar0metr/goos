//go:build goos_userspace

package main

// Shutdown: stopping services and flushing storage before a reset or power-off
// (ADR 0061, 0092).

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	execproto "goos/userspace/exec"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

// shutDown flushes storage and restarts or switches off the machine. A
// successful one is never answered; the shell hears only why it failed.
func shutDown(powerOff bool, correlationOfShell uint64) {
	name := "reboot"
	if powerOff {
		name = "poweroff"
	}
	syslog.Info("shutdown requested", syslog.Str("action", name))
	correlation++
	sync, encoded := vfs.SyncRequest(correlation)
	if !encoded || !sendMessage(vfsControlHandle, sync) {
		sendControlStatus(execproto.ControlSyncFailed, correlationOfShell)
		return
	}
	reply, status := receive(vfsControlHandle, vfsSessionDeadline)
	synced, valid := vfs.DecodeAddSessionReply(reply)
	if status != ipc.StatusOK || !valid || synced != vfs.StatusOK || reply.Correlation != correlation {
		syslog.Error("shutdown: storage sync failed", syslog.Str("action", name))
		sendControlStatus(execproto.ControlSyncFailed, correlationOfShell)
		return
	}
	if !powerOff {
		write("\nRebooting...\n")
		sys.SystemReset(handle.ID(systemControlHandle))
		syslog.Error("reboot: reset refused")
		sendControlStatus(execproto.ControlResetFailed, correlationOfShell)
		return
	}
	write("\nPowering off...\n")
	if sys.SystemPowerOff(handle.ID(systemControlHandle)) == kcall.StatusUnsupported {
		syslog.Warn("poweroff: no ACPI power-off on this machine")
		sendControlStatus(execproto.ControlUnsupported, correlationOfShell)
		return
	}
	syslog.Error("poweroff: power-off refused")
	sendControlStatus(execproto.ControlResetFailed, correlationOfShell)
}
