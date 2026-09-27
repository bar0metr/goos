//go:build goos_userspace

package main

import (
	"goos/internal/capability"
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	userwait "goos/userspace/wait"
)

const lifecycleHandle = uint64(0x0000000100000001)
const serviceDeviceHandle = handle.ID(0x0000000100000002)
const vfsChannelHandle = handle.ID(0x0000000100000003)
const vfsServerHandle = handle.ID(0x0000000100000004)

// vfsControlHandle is PID 1's end of the VFS control endpoint: the boot
// channel at first, a new one after every VFS restart (ADR 0099).
var vfsControlHandle = uint64(0x0000000100000005)

const vfsControlServerHandle = handle.ID(0x0000000100000006)

// execServerBoot is the server end of the exec channel created at boot. A
// restarted shell is given a fresh channel, so the end PID 1 serves is the
// variable execServer (ADR 0089).
const execServerBoot = uint64(0x0000000100000007)
const execClientHandle = handle.ID(0x0000000100000008)
const blockClientHandle = uint64(0x0000000100000009)
const blockServerHandle = handle.ID(0x000000010000000a)
const virtioPortHandle = handle.ID(0x000000010000000b)
const virtioIRQHandle = handle.ID(0x000000010000000c)
const virtioDMAHandle = handle.ID(0x000000010000000d)
const driverClientHandle = handle.ID(0x000000010000000e)
const driverServerHandle = handle.ID(0x000000010000000f)
const filesystemBlockClientHandle = handle.ID(0x0000000100000010)
const filesystemBlockServerHandle = handle.ID(0x0000000100000011)
const systemControlHandle = uint64(0x0000000100000012)
const execPageHandle = handle.ID(0x0000000100000014)
const systemImageHandle = handle.ID(0x0000000100000015)

// logDeviceHandle authorizes reading the kernel log ring and nothing else, so
// the log service and the shell can read the log without the console's input.
const logDeviceHandle = handle.ID(0x0000000100000016)

// loaderHandle authorizes creating executable objects, which PID 1 fills with
// programs it reads from a filesystem (ADR 0097).
const loaderHandle = handle.ID(0x0000000100000017)

// fileCacheHandle is the file cache, the memory the VFS and the filesystem
// service share for file data (ADR 0098). Each gets a copy at every launch.
const fileCacheHandle = handle.ID(0x0000000100000018)

// configControlHandle is the configuration service's control endpoint. PID 1
// holds the only client side of it, so it is the only process that can create a
// configuration session.
var configControlHandle handle.ID

var (
	// blockControl is the channel pair whose client side PID 1 keeps to tell
	// the block service about a replacement driver.
	blockControl [2]uint64
	// driverStarted marks the first launch, which uses the boot channel.
	driverStarted bool
	// driverProcessHandle is kept only by the validation build, to inject the
	// fault that proves a crashed driver is replaced.
	driverProcessHandle handle.ID
	// execServer is the end of the shell's exec channel PID 1 serves, or zero
	// while the shell is gone.
	execServer = execServerBoot
	// ttyControl is PID 1's end of the terminal service's control channel,
	// over which a restarted shell's session is handed to a running terminal.
	ttyControl handle.ID
	// ttyStarted and shellStarted mark the first launches, which use the
	// endpoints created at boot; every later launch renews them.
	ttyStarted, shellStarted bool
	// The validation build keeps the interactive services' process handles to
	// inject the faults that prove each restarts on its own.
	ttyProcessHandle, shellProcessHandle handle.ID
)

// execJob is one shell request in flight. The request is copied out of the
// shared page, so the shell cannot change it while it runs.
type execJob struct {
	active bool
	// orphaned marks a job whose shell has gone. Its stages are stopped with
	// the session and reaped as usual, but no one is left to answer.
	orphaned    bool
	slot        uint8
	correlation uint64
	data        [execproto.SlotSize]byte
	job         supervisor.Job
	// request is the job as the shell described it. While preparing, its
	// programs from files are being read into executable objects; the
	// pipeline starts once they are (ADR 0108).
	request   execproto.Request
	preparing bool
	cancelled bool
	nextStage uint8
	prepared  [execproto.MaxStages]prepared
	fill      pendingFill
}

// launchingJob is the job whose pipeline is starting, whose prepared
// programs the stages' launches take.
var launchingJob *execJob

var (
	manager              supervisor.Manager
	wire                 [ipc.WireMaxSize]byte
	supervisionTicks     uint64
	correlation          uint64
	arenaBase            arena.Mark
	supervisionComplete  bool
	processWaitAnnounced bool
	nextStagingSlot      uint8
	execJobs             [execproto.Slots]execJob
	// execPage is the page shared with the shell that carries its exec
	// requests, one slot per job (ADR 0064).
	execPage         []byte
	lifecyclePending [16]ipc.Message
	lifecycleHead    uint8
	lifecycleCount   uint8
	streamPair       [2]uint64
	streamDescriptor [3]uint64
	streamReadback   [16]byte
	// terminalOutput is the stream the shell and every command write to and
	// the terminal service reads (ADR 0089). PID 1 keeps both ends for as
	// long as it runs and grants copies: the service's reader survives the
	// service, so output written while it restarts waits in the stream, and
	// the writer is shared by every process that prints.
	terminalOutput [2]uint64
)

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("INIT_OUT_OF_MEMORY\n") })
	syslog.Init(serviceDeviceHandle, "init")
	if !mapSystemImage() {
		fatal("INIT_IMAGE_MAP_FAILED\n")
	}
	if validation.Enabled {
		// Each probed launch allocates its request and reply, and nothing the
		// probe keeps points into the arena, so what it used is given back
		// rather than taken from every later request (ADR 0097).
		probeBase := arena.Current()
		probed := probeLaunchAuthority()
		arena.Reset(probeBase)
		if !probed {
			fatal("LAUNCH_AUTHORITY_FAILED\n")
		}
		mark("LAUNCH_AUTHORITY_OK\n")
		if !probeStreamEndpoint() {
			fatal("STREAM_ENDPOINT_FAILED\n")
		}
		mark("STREAM_ENDPOINT_OK\n")
		if !probeSharedEndpoint() {
			fatal("STREAM_SHARED_ENDPOINT_FAILED\n")
		}
		mark("STREAM_SHARED_ENDPOINT_OK\n")
		if manager.Register(supervisor.Definition{Name: "fault-probe", Executable: "/system/fault-probe",
			Capabilities: []supervisor.Capability{{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write}}, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK ||
			manager.StartReady(0, bootstrapService) != 1 {
			fatal("INIT_POLICY_SETUP_FAILED\n")
		}
		mark("INIT_PROCESS_OK\n")
		mark("INIT_LIFECYCLE_IPC_OK\n")
	} else {
		startServices(0)
	}
	for {
		var interests [userwait.MaxObjects]userwait.Interest
		count := 1
		interests[0] = userwait.Interest{Handle: handle.ID(lifecycleHandle), Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		var jobs, fills [execproto.Slots]int
		execBit, firstJob, firstFill := 0, 1, 1
		if supervisionComplete {
			// While the shell is gone there is no exec channel to wait on; its
			// replacement brings a new one.
			if execServer != 0 {
				interests[count] = userwait.Interest{Handle: handle.ID(execServer), Events: kcall.WaitReadable | kcall.WaitPeerClosed}
				execBit = count
				count++
			}
			firstJob = count
			for index := range execJobs {
				if process, pending := execJobs[index].job.Next(); execJobs[index].active && !execJobs[index].preparing && pending {
					interests[count] = userwait.Interest{Handle: process, Events: kcall.WaitCompleted}
					jobs[count-firstJob] = index
					count++
				}
			}
			firstFill = count
			for index := range execJobs {
				if execJobs[index].active && execJobs[index].fill.session != 0 {
					interests[count] = userwait.Interest{Handle: execJobs[index].fill.session, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
					fills[count-firstFill] = index
					count++
				}
			}
		}
		readyMask := uint64(1)
		if lifecycleCount == 0 {
			// A scheduled restart is a deadline rather than an event. While one
			// is outstanding the wait is bounded, so the service comes back
			// even when nothing else happens; otherwise it blocks indefinitely.
			// A pending program read bounds it too.
			timeout := uint64(kcall.WaitAnyInfiniteTimeout)
			restarting := manager.RestartPending()
			if restarting {
				timeout = supervisor.RestartBackoff
			}
			if remaining, pending := fillTimeout(); pending && (!restarting || remaining < timeout) {
				timeout, restarting = remaining, false
			}
			result, status := userwait.Any(&interests, count, timeout)
			if status == kcall.StatusTimeout {
				expireFills()
				if restarting {
					supervisionTicks += supervisor.RestartBackoff
					manager.StartReady(supervisionTicks, launchService)
				}
				continue
			}
			if status != kcall.StatusOK {
				fatal("INIT_WAIT_FAILED\n")
			}
			readyMask = result.ReadyMask
		}
		for bit := firstJob; bit < firstFill; bit++ {
			if readyMask&(1<<bit) != 0 {
				advanceJob(jobs[bit-firstJob])
			}
		}
		for bit := firstFill; bit < count; bit++ {
			if readyMask&(1<<bit) != 0 {
				completeFill(&execJobs[fills[bit-firstFill]])
			}
		}
		if readyMask&1 == 0 {
			if execBit != 0 && readyMask&(1<<execBit) != 0 {
				pollExec()
			}
			continue
		}
		message, status := receiveLifecycle(0)
		if status != ipc.StatusOK {
			fatal("INIT_LIFECYCLE_FAILED\n")
		}
		event, valid := supervisor.DecodeLifecycle(message)
		if !valid || event.Operation != supervisor.LifecycleProcessExited {
			continue
		}
		supervisionTicks += supervisor.RestartBackoff
		checkerDone := event.Process == filesystemChecker
		if checkerDone {
			filesystemChecker = 0
			filesystemCheckComplete = true
		}
		shell, _ := manager.Snapshot("shell")
		exitStatus := manager.Exit(event.Process, event.Code, supervisionTicks)
		if filesystemCheckComplete && manager.Resume(filesystemName, supervisionTicks) == supervisor.StatusOK {
			filesystemCheckComplete = false
			manager.StartReady(supervisionTicks+supervisor.RestartBackoff, launchService)
			refreshMountedClients()
		}
		if exitStatus == supervisor.StatusNotFound {
			continue
		}
		if shell.State == supervisor.StateRunning && shell.PID == event.Process {
			endShellSession()
		}
		syslog.Warn("service exited", syslog.Uint("pid", event.Process), syslog.Uint("code", event.Code))
		manager.StartReady(supervisionTicks+supervisor.RestartBackoff, launchService)
		snapshot, found := manager.Snapshot("fault-probe")
		if validation.Enabled && found && snapshot.State == supervisor.StateFailed && !supervisionComplete {
			startServices(supervisionTicks)
			mark("USERSPACE_SUPERVISOR_OK\n")
		}
	}
}

func equalBytes(value []byte, expected string) bool {
	if len(value) != len(expected) {
		return false
	}
	for index := range value {
		if value[index] != expected[index] {
			return false
		}
	}
	return true
}

// driverName is the one service whose endpoints cannot survive its own restart.
const driverName = "virtio-blk"

const (
	// storageProbeDeadline covers the first answer from the storage stack,
	// which is still completing its own startup when PID 1 probes it.
	storageProbeDeadline = 5 * clock.TicksPerSecond
	// vfsSessionDeadline bounds one session handshake with the VFS service.
	vfsSessionDeadline = 2 * clock.TicksPerSecond
)

// launchReplyDeadline bounds the wait for a launch result. The kernel retains
// and retries these replies, so expiry means a real fault and must be reported
// instead of stalling PID 1 and every shell behind it.
const launchReplyDeadline = 5 * clock.TicksPerSecond

// The command index is read once, through a VFS session of PID 1's own, from
// the immutable system image. What a command is granted beyond its standard
// streams is decided from the manifest it was built with, not from a list of
// names kept here (ADR 0080). A command the index does not describe gets the
// standard streams only.
var (
	commandIndex       [commands.MaxIndex]byte
	commandIndexSize   int
	commandIndexLoaded bool
	commandIndexPath   = []byte(commands.IndexPath)
)

// createChannel and createStream fill pair with the two endpoints of a new
// channel or stream, the shape PID 1 keeps them in.
func createChannel(pair *[2]uint64) bool {
	first, second, status := sys.ChannelCreate()
	pair[0], pair[1] = uint64(first), uint64(second)
	return status == kcall.StatusOK
}

func createStream(pair *[2]uint64) bool {
	first, second, status := sys.StreamCreate()
	pair[0], pair[1] = uint64(first), uint64(second)
	return status == kcall.StatusOK
}

func closePair(pair [2]uint64) {
	if pair[0] != 0 {
		sys.HandleClose(handle.ID(pair[0]))
	}
	if pair[1] != 0 {
		sys.HandleClose(handle.ID(pair[1]))
	}
}

func bytesString(value []byte) string { return sys.String(value) }

// write prints to the console through the console capability PID 1 holds,
// like every other process that prints: there is no identity-authorized
// console path (ADR 0094). It is used before the terminal service exists and
// for the few lines that must not queue behind it (ADR 0089).
func write(message string) {
	if len(message) != 0 {
		sys.ConsoleWriteString(serviceDeviceHandle, message)
	}
}

func mark(message string) {
	if validation.Enabled {
		write(message)
	}
}

func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
	sys.Diagnostics(serviceDeviceHandle)
	for {
	}
}

func main() {}
