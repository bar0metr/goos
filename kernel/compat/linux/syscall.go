package linux

import (
	"goos/kernel/compat/linux/abi"
	"goos/kernel/lock"
)

const unsupportedHistorySize = 8

const (
	// MaxIOVec bounds the vector one readv or writev call may pass.
	MaxIOVec = 16
	// MaxVectorBytes bounds what one vector call transfers. A longer vector
	// is a short transfer, which the caller continues.
	MaxVectorBytes = 64 << 10
)

// Request is the scalar Linux register ABI after architectural entry checks.
// Pointer arguments remain integers until an operation-specific decoder copies
// them through validated user-memory helpers.
type Request struct {
	Number    abi.Syscall
	Arguments [6]uint64
}

type Result struct {
	Value  uint64
	Errno  abi.Errno
	Action uint64
	Entry  uint64
	Stack  uint64
	Root   uint64
	// Restart re-executes the call after any handler a signal runs, as
	// SA_RESTART asks: the return resumes at the SYSCALL instruction with
	// the call number in RAX.
	Restart bool
}

func (result Result) Register() uint64 {
	if result.Errno != 0 {
		return abi.Negative(result.Errno)
	}
	return result.Value
}

type Handler interface {
	// Read serves read and pread64, and Write serves write and pwrite64.
	Read(Request) Result
	Write(Request) Result
	Close(Request) Result
	Readv(Request) Result
	Writev(Request) Result
	Open(Request) Result
	Openat(Request) Result
	SetTidAddress(Request) Result
	// Filesystem serves the status, position, and path calls: the stat
	// family with statx, getdents64, lseek, getcwd, chdir, readlink, and
	// the calls that create, remove, and rename names.
	Filesystem(Request) Result
	// System serves the identity, time, limit, signal-state, random, umask, and
	// futex calls, which answer from kernel state without asking a service. It
	// also rejects a valid set_robust_list with ENOSYS.
	System(Request) Result
	// Descriptor serves dup, dup2, dup3, fcntl, and ioctl.
	Descriptor(Request) Result
	Brk(Request) Result
	Mmap(Request) Result
	Munmap(Request) Result
	Mprotect(Request) Result
	Madvise(Request) Result
	ArchPrctl(Request) Result
	Execve(Request) Result
	// Fork serves fork, vfork, and the process forms of clone.
	Fork(Request) Result
	Wait4(Request) Result
	// Pipe serves pipe and pipe2.
	Pipe(Request) Result
	SigReturn(Request) Result
	// SigSuspend serves rt_sigsuspend and pause.
	SigSuspend(Request) Result
	// Kill serves kill, tkill, and tgkill.
	Kill(Request) Result
	// Poll serves poll and ppoll.
	Poll(Request) Result
	Exit(Request, bool) Result
}

type UnsupportedDiagnostics struct {
	Count   uint64
	Numbers [unsupportedHistorySize]abi.Syscall
	Stored  uint8
	// Refused has one bit per call number below RefusedNumbers that was
	// ever refused, so a run shows every call a program missed however
	// many times it asked.
	Refused [RefusedNumbers / 64]uint64
}

// RefusedNumbers bounds the call numbers the refused set records. The
// x86_64 table ends below it.
const RefusedNumbers = 512

// WasRefused reports whether number was ever refused.
func (diagnostics UnsupportedDiagnostics) WasRefused(number abi.Syscall) bool {
	return number < RefusedNumbers && diagnostics.Refused[number/64]&(1<<(number%64)) != 0
}

var unsupportedDiagnostics UnsupportedDiagnostics

// diagnosticsLock guards the call counters, which calls of every class
// update, the fast ones without the Linux lock.
var diagnosticsLock = lock.New(lock.LevelLog)

// badPointers counts calls that ended in EFAULT: a program handed the kernel
// memory it could not use. The counter saturates.
var badPointers uint64

// lastBadPointer is the number of the most recent such call.
var lastBadPointer abi.Syscall

// BadPointers returns the count and the number of the latest call.
func BadPointers() (uint64, abi.Syscall) {
	diagnosticsLock.Lock()
	count, last := badPointers, lastBadPointer
	diagnosticsLock.Unlock()
	return count, last
}

func decode(frame *abi.Frame) (Request, bool) {
	if frame == nil {
		return Request{}, false
	}
	return Request{Number: abi.Syscall(frame.Number), Arguments: [6]uint64{frame.Arg1, frame.Arg2, frame.Arg3, frame.Arg4, frame.Arg5, frame.Arg6}}, true
}

// Dispatch is the bounded Linux syscall translation boundary. Implemented
// calls remain separate from the native kcall namespace; unsupported calls
// are recorded without emitting one log entry per invocation.
func Dispatch(frame *abi.Frame, handler Handler) Result {
	request, ok := decode(frame)
	if !ok {
		return Result{Errno: abi.EINVAL}
	}
	return route(request, handler)
}

// NoteBadPointer counts a call that ended in EFAULT. The kernel calls it
// once the result is final: a copy that only has to wait for a page of the
// program's own image is retried and is no bad pointer.
func NoteBadPointer(number abi.Syscall) {
	diagnosticsLock.Lock()
	saturate(&badPointers)
	lastBadPointer = number
	diagnosticsLock.Unlock()
}

// route sends a request to the handler method that implements its number.
func route(request Request, handler Handler) Result {
	if handler != nil {
		switch request.Number {
		case abi.SysRead, abi.SysPread64:
			return handler.Read(request)
		case abi.SysWrite, abi.SysPwrite64:
			return handler.Write(request)
		case abi.SysClose:
			return handler.Close(request)
		case abi.SysReadv:
			return handler.Readv(request)
		case abi.SysWritev:
			return handler.Writev(request)
		case abi.SysOpen:
			return handler.Open(request)
		case abi.SysOpenat:
			return handler.Openat(request)
		case abi.SysSetTidAddress:
			return handler.SetTidAddress(request)
		case abi.SysDup, abi.SysDup2, abi.SysDup3, abi.SysFcntl, abi.SysIoctl:
			return handler.Descriptor(request)
		case abi.SysGetpid, abi.SysGettid, abi.SysGetppid, abi.SysSetpgid, abi.SysGetpgrp, abi.SysSetsid, abi.SysGetpgid, abi.SysGetsid, abi.SysGetuid, abi.SysGeteuid, abi.SysGetgid, abi.SysGetegid,
			abi.SysSetuid, abi.SysSetgid, abi.SysGetgroups,
			abi.SysUname, abi.SysClockGettime, abi.SysGettimeofday, abi.SysTime, abi.SysSysinfo, abi.SysNanosleep, abi.SysPrlimit64,
			abi.SysRtSigaction, abi.SysRtSigprocmask, abi.SysGetrandom,
			abi.SysUmask, abi.SysSetRobustList, abi.SysFutex, abi.SysSchedYield, abi.SysSchedGetaffinity, abi.SysSchedSetaffinity, abi.SysGetcpu:
			return handler.System(request)
		case abi.SysAccess, abi.SysFaccessat, abi.SysGetdents64, abi.SysStat, abi.SysFstat, abi.SysLstat, abi.SysNewfstatat, abi.SysLseek, abi.SysGetcwd, abi.SysChdir, abi.SysRename, abi.SysMkdir, abi.SysRmdir, abi.SysUnlink,
			abi.SysMkdirat, abi.SysUnlinkat, abi.SysRenameat, abi.SysRenameat2, abi.SysStatx,
			abi.SysReadlink, abi.SysReadlinkat, abi.SysChmod, abi.SysFchmod, abi.SysFchmodat, abi.SysUtimensat,
			abi.SysFsync, abi.SysFdatasync:
			return handler.Filesystem(request)
		case abi.SysBrk:
			return handler.Brk(request)
		case abi.SysMmap:
			return handler.Mmap(request)
		case abi.SysMunmap:
			return handler.Munmap(request)
		case abi.SysMprotect:
			return handler.Mprotect(request)
		case abi.SysMadvise:
			return handler.Madvise(request)
		case abi.SysArchPrctl:
			return handler.ArchPrctl(request)
		case abi.SysExecve:
			return handler.Execve(request)
		case abi.SysFork, abi.SysVfork, abi.SysClone:
			return handler.Fork(request)
		case abi.SysWait4:
			return handler.Wait4(request)
		case abi.SysPipe, abi.SysPipe2:
			return handler.Pipe(request)
		case abi.SysRtSigreturn:
			return handler.SigReturn(request)
		case abi.SysRtSigsuspend, abi.SysPause:
			return handler.SigSuspend(request)
		case abi.SysKill, abi.SysTkill, abi.SysTgkill:
			return handler.Kill(request)
		case abi.SysPoll, abi.SysPpoll:
			return handler.Poll(request)
		case abi.SysExit:
			return handler.Exit(request, false)
		case abi.SysExitGroup:
			return handler.Exit(request, true)
		}
	}
	noteUnsupported(request.Number)
	return Result{Errno: abi.ENOSYS}
}

// Refuse records a call the handler cannot serve on this machine and
// returns ENOSYS, as route does for a number with no handler at all.
func Refuse(number abi.Syscall) Result {
	noteUnsupported(number)
	return Result{Errno: abi.ENOSYS}
}

func noteUnsupported(number abi.Syscall) {
	diagnosticsLock.Lock()
	if unsupportedDiagnostics.Count != ^uint64(0) {
		unsupportedDiagnostics.Count++
	}
	index := unsupportedDiagnostics.Count - 1
	if index >= unsupportedHistorySize {
		index %= unsupportedHistorySize
	}
	unsupportedDiagnostics.Numbers[index] = number
	if number < RefusedNumbers {
		unsupportedDiagnostics.Refused[number/64] |= 1 << (number % 64)
	}
	if unsupportedDiagnostics.Stored < unsupportedHistorySize {
		unsupportedDiagnostics.Stored++
	}
	diagnosticsLock.Unlock()
}

func Unsupported() UnsupportedDiagnostics {
	diagnosticsLock.Lock()
	diagnostics := unsupportedDiagnostics
	diagnosticsLock.Unlock()
	return diagnostics
}

// Recent returns the index-th retained number, oldest first.
func (diagnostics UnsupportedDiagnostics) Recent(index int) abi.Syscall {
	if index < 0 || index >= int(diagnostics.Stored) {
		return 0
	}
	oldest := uint64(0)
	if diagnostics.Count > unsupportedHistorySize {
		oldest = diagnostics.Count % unsupportedHistorySize
	}
	return diagnostics.Numbers[(oldest+uint64(index))%unsupportedHistorySize]
}

// Last returns the most recent unsupported number, or zero before the first.
func (diagnostics UnsupportedDiagnostics) Last() abi.Syscall {
	if diagnostics.Count == 0 {
		return 0
	}
	return diagnostics.Numbers[(diagnostics.Count-1)%unsupportedHistorySize]
}
