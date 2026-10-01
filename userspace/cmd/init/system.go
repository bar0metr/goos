//go:build goos_userspace

package main

// The installed system (ADR 0175). PID 1 boots from the archive, waits for the
// volume, and checks what is installed on it before it trusts any of it: the
// installation record, the images of the services it will start from it, and
// the command index it will grant from. It then asks the VFS to publish the
// installed system at /system and /apps and runs in normal mode, with the
// session services read from the volume. Anything missing or refused puts it
// in recovery mode instead, which runs the archive's own shell and commands.

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/initramfs"
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/release"
	"goos/userspace/runtime/sys"
)

var (
	// boot is the mode PID 1 runs the system in, and why it is in recovery.
	boot supervisor.BootState
	// filesystemAttached is the outcome of the latest attach of the volume.
	filesystemAttached bool
	// systemReady says the installed system is published and programs may
	// be read from it. It is false from the moment its VFS or filesystem
	// service goes until the restarted one has published it again.
	systemReady bool
	// storageLost is the validation build's volume that is gone for good:
	// no filesystem service starts while it is set.
	storageLost bool
	// filesystemPending records that the VFS or the block service went, so
	// the volume is usable again only once the filesystem service that
	// follows them has been attached.
	filesystemPending bool
	// withdrawPending records a withdrawal of the installed system the VFS
	// refused while files were open through it.
	withdrawPending bool
	// bootstrapIndex is the archive's recovery index, read from the mapped
	// archive rather than through the VFS.
	bootstrapIndex     []byte
	bootstrapIndexRead bool
	// releaseData holds the installation record while it is checked.
	releaseData [release.MaxSize]byte
)

// The installed system's files, read through the volume's own mount before
// /system exists. Every one of them is below the system directory, which the
// VFS protects from the moment it mounts the volume.
const (
	installedRoot    = "/persist/system"
	installedRecord  = installedRoot + "/release"
	installedIndex   = installedRoot + "/commands"
	installedBin     = installedRoot + "/bin/"
	installedService = installedRoot + "/services/"
	// forceRecoveryMember is the archive member whose presence asks for
	// recovery mode, put there by the build's recovery option.
	forceRecoveryMember = "system/force-recovery"
)

// sessionServices are the services that run from the volume in normal mode.
var sessionServices = [...]string{"config", "log", "shell"}

// forcedRecovery reports whether the boot media asks for recovery mode.
func forcedRecovery() bool {
	_, _, found := initramfs.Extent(systemImage, []byte(forceRecoveryMember))
	return found
}

// chooseMode decides the boot mode once storage has had its chance to come
// up, and announces it.
func chooseMode() {
	switch {
	case forcedRecovery():
		boot.Recover(supervisor.StageForced, execproto.BootstrapRoot+"force-recovery", "requested by the boot media")
	case !filesystemAttached:
		boot.Recover(supervisor.StageStorage, "/persist", "no usable volume")
	default:
		if stage, path, reason := checkInstalledSystem(); stage != supervisor.StageNone {
			boot.Recover(stage, path, reason)
		} else if status := requestSystem(true); status != vfs.StatusOK {
			requestSystem(false)
			boot.Recover(supervisor.StagePublish, "/system", "the vfs refused to publish it")
		} else {
			boot.Normal()
			systemReady = true
		}
	}
	announceMode()
}

// announceMode writes the mode line to the console and the log.
func announceMode() {
	write(boot.Line())
	if boot.Mode() == supervisor.ModeRecovery {
		syslog.Warn("recovery mode", syslog.Str("stage", boot.Stage().String()), syslog.Str("path", bytesString(boot.Path())), syslog.Str("reason", bytesString(boot.Reason())))
		mark("BOOT_MODE_RECOVERY\n")
		return
	}
	syslog.Info("normal mode")
	mark("BOOT_MODE_NORMAL\n")
}

// checkInstalledSystem checks the installed system before anything in it is
// used. It returns StageNone when the system can be published, and otherwise
// the stage, path, and reason recovery reports. The command index it read is
// kept as the one installed commands are granted from.
func checkInstalledSystem() (supervisor.Stage, string, string) {
	commandIndexLoaded, commandIndexSize = false, 0
	session, reason := openVFSSession()
	if reason != "" {
		return supervisor.StageStorage, "/persist", "no filesystem session"
	}
	stage, path, reason := checkInstalled(session)
	sys.HandleClose(session)
	if stage == supervisor.StageNone {
		mark("SYSTEM_CHECK_OK\n")
	}
	return stage, path, reason
}

func checkInstalled(session handle.ID) (supervisor.Stage, string, string) {
	if stat, ok := callSession(session, vfs.Request{Operation: vfs.OperationStat, Path: []byte(installedRoot)}); !ok || stat.Status != vfs.StatusOK || stat.Kind != ramfs.KindDirectory {
		return supervisor.StageRecord, "/system", "no installed system on the volume"
	}
	size, status := readInstalled(session, installedRecord, releaseData[:])
	if status != "" {
		return supervisor.StageRecord, "/system/release", status
	}
	installed, problem := release.Parse(releaseData[:size])
	required := release.Current()
	if problem == release.OK {
		problem = release.Check(&installed, &required)
	}
	if problem == release.ProblemRelease {
		return supervisor.StageRecord, "/system/release", "release mismatch: installed " + bytesString(installed.ReleaseText()) + ", boot " + bytesString(required.ReleaseText())
	}
	if problem != release.OK {
		return supervisor.StageRecord, "/system/release", problem.String()
	}
	for _, name := range sessionServices {
		if reason := checkImage(session, installedService+name); reason != "" {
			return supervisor.StageServices, "/system/services/" + name, reason
		}
	}
	size, status = readInstalled(session, installedIndex, commandIndex[:])
	if status != "" {
		return supervisor.StageIndex, "/system/commands", status
	}
	if !commands.Valid(commandIndex[:size]) {
		return supervisor.StageIndex, "/system/commands", "malformed index"
	}
	for offset := 0; offset < size; {
		entry, next, _ := commands.Next(commandIndex[:size], offset)
		if reason := checkImage(session, installedBin+bytesString(entry.Name)); reason != "" {
			return supervisor.StageIndex, "/system/bin/" + bytesString(entry.Name), reason
		}
		offset = next
	}
	commandIndexSize, commandIndexLoaded = size, true
	mark("INIT_COMMAND_INDEX_OK\n")
	return supervisor.StageNone, "", ""
}

// readInstalled reads a whole installed file, or says why it could not.
func readInstalled(session handle.ID, path string, target []byte) (int, string) {
	stat, ok := callSession(session, vfs.Request{Operation: vfs.OperationStat, Path: []byte(path)})
	switch {
	case !ok:
		return 0, "unreadable"
	case stat.Status == vfs.StatusNotFound:
		return 0, "missing"
	case stat.Status != vfs.StatusOK || stat.Kind != ramfs.KindFile:
		return 0, "not a file"
	case stat.Size > uint64(len(target)):
		return 0, "too large"
	}
	size, read := readSessionFile(session, []byte(path), target)
	if !read {
		return 0, "unreadable"
	}
	return size, ""
}

// checkImage requires a non-empty regular file that starts as a native image.
// The loader validates the rest when the program is launched.
func checkImage(session handle.ID, path string) string {
	stat, ok := callSession(session, vfs.Request{Operation: vfs.OperationStat, Path: []byte(path)})
	switch {
	case !ok:
		return "unreadable"
	case stat.Status == vfs.StatusNotFound:
		return "missing"
	case stat.Status != vfs.StatusOK || stat.Kind != ramfs.KindFile || stat.Size == 0:
		return "not a file"
	}
	opened, ok := callSession(session, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte(path)})
	if !ok || opened.Status != vfs.StatusOK {
		return "unreadable"
	}
	header, ok := callSession(session, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 8})
	callSession(session, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	if !ok || header.Status != vfs.StatusOK || supervisor.ClassifyExecutable(header.Data) != supervisor.ExecutableNative {
		return "not a native image"
	}
	return ""
}

// requestSystem asks the VFS to publish the installed system, or to withdraw
// it, and returns its answer.
func requestSystem(publish bool) vfs.Status {
	correlation++
	request, ok := vfs.WithdrawRequest(correlation)
	if publish {
		request, ok = vfs.PublishRequest(correlation)
	}
	if !ok || !sendMessage(vfsControlHandle, request) {
		return vfs.StatusBackendError
	}
	reply, status := receive(vfsControlHandle, vfsSessionDeadline)
	result, valid := vfs.DecodeAddSessionReply(reply)
	if status != ipc.StatusOK || !valid {
		return vfs.StatusBackendError
	}
	return result
}

// restoreSystem runs once a restarted filesystem service has been attached.
// The VFS protected the system directory again as it mounted the volume, or
// kept the protection across the restart; in normal mode the installed
// system is published again, and its programs may be read once more.
func restoreSystem() {
	if boot.Mode() != supervisor.ModeNormal {
		return
	}
	if !filesystemAttached {
		enterRecovery(supervisor.StageRuntime, "/persist", "volume not attached after a restart")
		return
	}
	if status := requestSystem(true); status != vfs.StatusOK {
		enterRecovery(supervisor.StageRuntime, "/system", "publish refused after a restart")
		return
	}
	// What was read from the volume before the restart is read again.
	if stage, path, reason := checkInstalledSystem(); stage != supervisor.StageNone {
		enterRecovery(stage, path, reason)
		return
	}
	filesystemPending = false
	systemResumed()
	mark("SYSTEM_REATTACHED_OK\n")
	syslog.Info("installed system attached again")
}

// driverRestarted runs once a restarted storage driver is installed. When the
// driver was all that went, the volume is usable again now.
func driverRestarted() {
	if boot.Mode() == supervisor.ModeNormal && !systemReady && !filesystemPending && filesystemAttached {
		systemResumed()
	}
}

// storageExited runs when a storage service has gone. Nothing is started
// from the volume until the services behind it are back.
func storageExited(name string) {
	if name == "vfs" || name == "block" {
		filesystemPending = true
	}
	if !systemReady {
		return
	}
	systemReady = false
	for _, service := range sessionServiceNames() {
		manager.Hold(service)
	}
}

// systemResumed lets the session services start from the volume again. A
// program read from a file fails when its source goes, so each storage fault
// can end them; recovering the storage gives each a fresh restart budget,
// and only a service that keeps failing with storage up uses its budget up.
func systemResumed() {
	systemReady = true
	for _, name := range sessionServiceNames() {
		if executable, found := manager.Executable(name); found {
			manager.Rebase(name, bytesString(executable), supervisionTicks)
		}
		manager.Release(name)
	}
}

// retryWithdraw withdraws the installed system once the files that kept it
// in use have closed.
func retryWithdraw() {
	if withdrawPending && requestSystem(false) == vfs.StatusOK {
		withdrawPending = false
		syslog.Info("installed system withdrawn")
	}
}

// sessionServiceNames returns the registered services that run from the
// volume in normal mode.
func sessionServiceNames() []string {
	names := []string{"config", "log"}
	for index := range sessions {
		if sessions[index].active {
			names = append(names, sessions[index].shell)
		}
	}
	return names
}

// diskService reports whether a service runs from the volume in normal mode.
func diskService(name string) bool {
	if name == "config" || name == "log" {
		return true
	}
	for index := range sessions {
		if sessions[index].active && name == sessions[index].shell {
			return true
		}
	}
	return false
}

// checkExhaustion moves a normal system whose storage or session services
// have used up their restarts into recovery.
func checkExhaustion() {
	if boot.Mode() != supervisor.ModeNormal {
		return
	}
	for _, name := range [...]string{"vfs", "block", driverName, filesystemName} {
		if snapshot, found := manager.Snapshot(name); found && snapshot.State == supervisor.StateFailed {
			enterRecovery(supervisor.StageRuntime, "/system", name+" failed")
			return
		}
	}
	for _, name := range sessionServiceNames() {
		if snapshot, found := manager.Snapshot(name); found && snapshot.State == supervisor.StateFailed {
			executable, _ := manager.Executable(name)
			enterRecovery(supervisor.StageLaunch, bytesString(executable), name+" failed")
			return
		}
	}
}

// enterRecovery leaves normal mode for good. The installed system is
// withdrawn, the configuration and log services stop, and each shell is
// started again from the archive. A service that is running is stopped
// first, so no session ever has two shells.
func enterRecovery(stage supervisor.Stage, path, reason string) {
	if !boot.Recover(stage, path, reason) {
		return
	}
	systemReady = false
	announceMode()
	if status := requestSystem(false); status != vfs.StatusOK {
		// Programs read from it are still running; it goes once they
		// have.
		withdrawPending = true
		syslog.Info("installed system in use; withdrawn once it is not", syslog.Uint("status", uint64(status)))
	}
	// Recovery needs the VFS, so a VFS that used up its restarts gets
	// another budget.
	if snapshot, found := manager.Snapshot("vfs"); found && snapshot.State == supervisor.StateFailed {
		manager.Rebase("vfs", execproto.BootstrapRoot+"vfs", supervisionTicks)
	}
	// Recovery runs no configuration or log service: neither is needed by
	// the shell, and each would hold files on the volume a check needs.
	for _, name := range [...]string{"config", "log"} {
		manager.Hold(name)
		if manager.PlanStop(name) == supervisor.StatusOK {
			process := &configProcessHandle
			if name == "log" {
				process = &logProcessHandle
			}
			if *process != 0 {
				sys.ProcessKill(*process)
				sys.HandleClose(*process)
				*process = 0
			}
		}
	}
	if configControlHandle != 0 {
		sys.HandleClose(configControlHandle)
		configControlHandle = 0
	}
	for index := range sessions {
		session := &sessions[index]
		if !session.active {
			continue
		}
		// The shell starts again from the archive, without configuration:
		// its restart finds no configuration service to ask.
		manager.Rebase(session.shell, supervisor.ServicePath("shell", supervisor.ModeRecovery), supervisionTicks)
		manager.Release(session.shell)
		if session.shellProcess != 0 {
			sys.ProcessKill(session.shellProcess)
			sys.HandleClose(session.shellProcess)
			session.shellProcess = 0
		}
	}
	// The main loop starts the shells: a stopped one when its exit arrives,
	// one that had failed at its next restart deadline. enterRecovery can
	// run inside a launch, which must not start others.
	mark("RECOVERY_SESSIONS_OK\n")
}

// trustedGrants returns what the index that describes the program at path
// grants it, and whether one describes it. A program only the installed
// index describes is granted only while the installed system is published
// in normal mode, and only when it was read from the volume; one of the
// archive's only when it was launched from the archive.
func trustedGrants(path []byte, fromArchive bool) (commands.Grant, bool) {
	switch supervisor.TrustedIndex(path, boot.Mode(), systemReady) {
	case supervisor.IndexBootstrap:
		command, _ := execproto.CommandFromBootstrapPath(path)
		if !fromArchive || !loadBootstrapIndex() {
			return 0, false
		}
		entry, found := commands.Lookup(bootstrapIndex, command)
		return entry.Grants, found
	case supervisor.IndexSystem:
		command, _ := execproto.CommandFromSystemPath(path)
		if fromArchive || !commandIndexLoaded {
			return 0, false
		}
		entry, found := commands.Lookup(commandIndex[:commandIndexSize], command)
		return entry.Grants, found
	}
	return 0, false
}

// loadBootstrapIndex finds the recovery index in the mapped archive. It is
// part of what the kernel loaded, so no service is needed to read it.
func loadBootstrapIndex() bool {
	if bootstrapIndexRead {
		return bootstrapIndex != nil
	}
	bootstrapIndexRead = true
	member, ok := execproto.ArchiveMember([]byte(commands.BootstrapIndexPath))
	if !ok {
		return false
	}
	offset, size, found := initramfs.Extent(systemImage, member)
	if !found || !commands.Valid(systemImage[offset:offset+size]) {
		syslog.Warn("recovery command index unusable")
		return false
	}
	bootstrapIndex = systemImage[offset : offset+size]
	return true
}

// injectStorageLoss kills the filesystem service and keeps it from starting
// again, in the validation build only, so the boot test sees a normal system
// lose its volume and move to recovery.
func injectStorageLoss() bool {
	if !validation.Enabled || filesystemProcessHandle == 0 {
		return false
	}
	storageLost = true
	if !injectFilesystemFault() {
		storageLost = false
		return false
	}
	mark("STORAGE_LOSS_INJECTED\n")
	return true
}
