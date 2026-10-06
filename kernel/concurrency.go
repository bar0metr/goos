//go:build uefi

package main

// Per-processor call state and concurrency diagnostics (ADR 0184).

import (
	"goos/kernel/deadline"
	"sync/atomic"

	"goos/internal/cpuinfo"
	"goos/internal/limits"
	"goos/internal/logfmt"
	"goos/internal/validation"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	kernelconsole "goos/kernel/console"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/klog"
	"goos/kernel/lock"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/kernel/stream"
	kernelwait "goos/kernel/wait"
)

// cpuScratch is the working memory of the call a processor runs. A call runs
// to completion on one processor with interrupts disabled, so nothing else
// touches its processor's scratch until it returns; what a call keeps across
// a block lives in its continuation instead. The kernel heap never frees, so
// these buffers are static rather than allocated per call.
type cpuScratch struct {
	cpuInfoWire    [cpuinfo.SummarySize]byte
	cpuEntryWire   [cpuinfo.CPUEntrySize]byte
	cpuCacheWire   [cpuinfo.CacheInfoSize]byte
	cpuRuntimeWire [kcall.CPURuntimeSize]byte
	affinityWire   [kcall.AffinityMaskSize]byte
	affinityTasks  [process.MaxTasksPerProcess]sched.TaskID
	// expiredDeadlines receives the deadlines housekeeping takes from the index.
	expiredDeadlines     [16]deadline.Entry
	executableWire       [8]byte
	executableSourceWire [32]byte
	executableReply      ipc.Message
	imageWire            [16]byte
	// linuxStackRandom holds the AT_RANDOM bytes of the launch in progress. The
	// launch clears it once the stack image has its copy.
	linuxStackRandom    [linuxabi.RandomSize]byte
	linuxExecPointer    [8]byte
	linuxStat           [linuxcompat.StatSize]byte
	linuxStatx          [linuxcompat.StatxSize]byte
	linuxDirent         [linuxcompat.MaxDirentSize]byte
	linuxTerminator     [1]byte
	linuxFutexWord      [4]byte
	linuxFutexTimeout   [linuxcompat.TimespecSize]byte
	linuxFSBaseResult   [8]byte
	linuxTimes          [32]byte
	linuxPipeWire       [8]byte
	linuxPollWire       [linuxcompat.LinuxMaxPollFD * linuxcompat.PollFDSize]byte
	linuxPollEntries    [linuxcompat.LinuxMaxPollFD]kernelwait.Entry
	linuxFamilyRoots    [linuxcompat.LinuxFamilyMaxProcesses]uint64
	linuxSignalFrame    [linuxcompat.SignalFrameSize]byte
	linuxSignalUContext [linuxcompat.UContextSize]byte
	linuxSignalFPU      [linuxcompat.FPUStateSize]byte
	linuxHandler        linuxCallHandler
	linuxIOBuffer       [stream.AtomicWrite]byte
	linuxIOVecWire      [linuxcompat.MaxIOVec * linuxabi.IOVecSize]byte
	linuxProbe          [1]byte
	linuxTimespec       [linuxcompat.TimespecSize]byte
	linuxUname          [linuxcompat.UnameSize]byte
	linuxRlimit         [linuxcompat.RlimitSize]byte
	// linuxSignalAction and linuxSignalPrevious hold the new and the old
	// disposition of one rt_sigaction call.
	linuxSignalAction   [linuxcompat.SignalActionSize]byte
	linuxSignalPrevious [linuxcompat.SignalActionSize]byte
	linuxSignalSet      [linuxcompat.SignalSetSize]byte
	linuxRandom         [linuxcompat.MaxRandomBytes]byte
	linuxCPUMask        [linuxCPUMaskSize]byte
	linuxCPUWire        [4]byte
	linuxTimeval        [linuxcompat.TimevalSize]byte
	linuxTimezone       [linuxcompat.TimezoneSize]byte
	linuxTime           [8]byte
	linuxSysinfo        [linuxcompat.SysinfoSize]byte
	linuxTIDWire        [4]byte
	// siblingTasks lists the other tasks of a process a call stops or ends.
	siblingTasks [sched.MaxTasks]sched.TaskID
	// linuxVFSMessage holds the request being sent, then the reply being read.
	// A decoded reply aliases it until the handler returns.
	linuxVFSMessage ipc.Message
	// linuxPath receives a path as the program wrote it; linuxResolved is its
	// canonical absolute form, the only one a request carries.
	linuxPath       [linuxabi.PathMax]byte
	linuxResolved   [limits.PathMax]byte
	linuxTarget     [limits.PathMax]byte
	linuxWaitStatus [4]byte
	linuxRusage     [linuxabi.RusageSize]byte
	logWire         [kcall.MaxLogRecord]byte
	logBoundsWire   [kcall.LogBoundsSize]byte
	logText         [kcall.LogText]byte
	logMessage      [logfmt.MaxMessage]byte
	logFields       [logfmt.MaxFields]byte
	logGrant        [logfmt.MaxFields]byte
	logCopy         klog.Copy
	kernelCallData  [kcall.MaxConsoleWrite]byte
	ipcWireData     [ipc.WireMaxSize]byte
	dispatchHandler nativeCallHandler
	// The wires a native call copies its arguments and results through.
	handleInfoWire        [kcall.HandleInfoSize]byte
	processInfoWire       [kcall.ProcessInfoSize]byte
	clockWire             [8]byte
	nanosWire             [8]byte
	timeWire              [kcall.ClockTimeSize]byte
	consoleByteWire       [1]byte
	systemInfoWire        [kcall.SystemInfoSize]byte
	consoleInfoWire       [kcall.ConsoleInfoSize]byte
	portWire              [4]byte
	dmaWire               [24]byte
	channelWire           [16]byte
	streamCreateWire      [16]byte
	streamDescriptorWire  [24]byte
	streamResultWire      [8]byte
	receiveDescriptorWire [24]byte
	exitWire              [8]byte
	waitAnyResultWire     [kcall.WaitAnyResultSize]byte
	waitMaskWire          [8]byte
	streamCallData        [kcall.MaxStreamIO]byte
	ipcWakeResults        [ipc.MaxWaiters]ipc.WaitResult
	processWakeResults    [process.MaxWaiters]process.WaitResult
	streamWakeResults     [ipc.MaxWaiters]ipc.WaitResult
	waitAnyCompletions    [sched.MaxTasks]kernelwait.Completion
	waitAnyEntries        [kernelwait.MaxObjects]kernelwait.Entry
	waitAnyWire           [kernelwait.MaxObjects * int(kcall.WaitAnyInterestSize)]byte
	memoryWire            [8]byte
	vmDescriptor          [kcall.VMDescriptorSize]byte
	vmRecord              [kcall.VMRecordSize]byte
	// vmPieces is the scratch the protection and discard checks inspect.
	vmPieces     region.Pieces
	startupChunk [512]byte
	// nativeCall is the lock set of the native call the processor runs,
	// peerWanted the process whose handle table that call asked for, and
	// streamsWanted whether it asked for the stream lock too.
	nativeCall    nativeCall
	linuxCall     heldLocks
	peerWanted    process.ID
	streamsWanted bool
	// call names the call the processor runs, for a panic report: 'n' and
	// the native operation, 'l' and the Linux number, or zero.
	callKind   byte
	callNumber uint64
	// linuxReleased collects descriptions the Linux tables let go of, for
	// release once the tables are no longer held.
	linuxReleased linuxcompat.Released
}

// cpuScratches holds each started processor's scratch. The bootstrap
// processor's is static, since boot logs before the page allocator exists;
// every other processor's comes from the allocator before it starts, so an
// image built for MaxCPUs processors stays small.
var (
	bootScratch  cpuScratch
	cpuScratches = [amd64.MaxCPUs]*cpuScratch{&bootScratch}
)

// thisCPU returns the calling processor's scratch.
func thisCPU() *cpuScratch { return cpuScratches[amd64.CPUIndex()] }

// prepareScratch gives processor index its scratch before it starts and
// reports whether it has one.
func prepareScratch(index int) bool {
	if index <= 0 || index >= len(cpuScratches) {
		return false
	}
	if cpuScratches[index] != nil {
		return true
	}
	scratch, ok := vm.NewObject[cpuScratch](&pageAllocator)
	cpuScratches[index] = scratch
	return ok
}

// waitStatuses holds, per task slot, the status a waker leaves for the task
// it resumes, which the task takes on its retried call. Wakers on other
// processors write it, so every access is atomic.
type waitStatuses [sched.MaxTasks]atomic.Uint64

func (statuses *waitStatuses) set(task uint64, status kcall.Status) {
	if task < uint64(len(statuses)) {
		statuses[task].Store(uint64(status))
	}
}

// take returns the stored status and leaves StatusOK in its place.
func (statuses *waitStatuses) take(task uint64) kcall.Status {
	if task >= uint64(len(statuses)) {
		return kcall.StatusOK
	}
	return kcall.Status(statuses[task].Swap(uint64(kcall.StatusOK)))
}

// The highest kernel peaks reported. Housekeeping alone touches them.
var kernelParallelReported, kernelThreadReported uint64

// reportKernelParallelPeak prints, in validation builds, each new most of
// processors that were inside system calls or fault resolution at once, and
// each new most of one process's tasks inside them at once, so a boot test
// can tell kernel code ran on several processors together.
func reportKernelParallelPeak() {
	reportPeak("KERNEL_PARALLEL_PEAK ", amd64.KernelParallelPeak(), &kernelParallelReported)
	reportPeak("KERNEL_THREAD_PEAK ", amd64.KernelThreadPeak(), &kernelThreadReported)
}

func reportPeak(marker string, peak uint64, reported *uint64) {
	if peak <= *reported {
		return
	}
	*reported = peak
	console := kernelconsole.New()
	console.WriteString(marker)
	console.WriteUint(peak)
	console.WriteString("\n")
}

// lockWatchdogSeconds is how long a processor spins for one lock before the
// watchdog reports it. No lock is held for more than a call's worth of work,
// so a spin this long is a deadlock or a lost unlock.
const lockWatchdogSeconds = 4

// reportStuckLock prints the stuck spin. A validation build stops there,
// since a test that deadlocks must fail where it deadlocked; a normal build
// keeps spinning, in case the holder is only slow.
func reportStuckLock(level lock.Level, holder uint32) {
	console := kernelconsole.New()
	console.WriteString("LOCK_STUCK level=")
	console.WriteString(level.String())
	console.WriteString(" cpu=")
	console.WriteUint(uint64(amd64.CPUIndex()))
	if holder != 0 {
		console.WriteString(" holder=")
		console.WriteUint(uint64(holder - 1))
	}
	console.WriteString("\n")
	if validation.Enabled {
		panicSerial("lock watchdog: a spin outlasted the watchdog")
	}
}
