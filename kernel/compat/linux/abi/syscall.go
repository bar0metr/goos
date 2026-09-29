package abi

// Syscall is a Linux x86_64 syscall number. It is intentionally distinct from
// the native kcall.Operation namespace.
type Syscall uint64

// Numbers are from Linux arch/x86/entry/syscalls/syscall_64.tbl. This bounded
// planning namespace includes measured release candidates and later fallbacks;
// membership is not a claim of implementation or a 0.2.0 release commitment.
const (
	SysRead          Syscall = 0
	SysWrite         Syscall = 1
	SysClose         Syscall = 3
	SysFstat         Syscall = 5
	SysPoll          Syscall = 7
	SysLseek         Syscall = 8
	SysMmap          Syscall = 9
	SysMprotect      Syscall = 10
	SysMunmap        Syscall = 11
	SysBrk           Syscall = 12
	SysRtSigaction   Syscall = 13
	SysRtSigprocmask Syscall = 14
	SysIoctl         Syscall = 16
	SysReadv         Syscall = 19
	SysWritev        Syscall = 20
	SysPipe          Syscall = 22
	SysSchedYield    Syscall = 24
	SysMadvise       Syscall = 28
	SysNanosleep     Syscall = 35
	SysGetpid        Syscall = 39
	SysExit          Syscall = 60
	SysUname         Syscall = 63
	SysGetcwd        Syscall = 79
	SysChdir         Syscall = 80
	SysReadlink      Syscall = 89
	SysGetuid        Syscall = 102
	SysGetgid        Syscall = 104
	SysGeteuid       Syscall = 107
	SysGetegid       Syscall = 108
	SysArchPrctl     Syscall = 158
	SysGettid        Syscall = 186
	SysFutex         Syscall = 202
	SysGetdents64    Syscall = 217
	SysClockGettime  Syscall = 228
	SysExitGroup     Syscall = 231
	SysOpenat        Syscall = 257
	SysNewfstatat    Syscall = 262
	SysUnlinkat      Syscall = 263
	SysReadlinkat    Syscall = 267
	SysPpoll         Syscall = 271
	SysSetRobustList Syscall = 273
	SysPipe2         Syscall = 293
	SysPrlimit64     Syscall = 302
	SysGetrandom     Syscall = 318
	SysStatx         Syscall = 332
)

// Known reports whether number belongs to the bounded planning namespace.
// Known does not mean implemented or release-scoped: an absent handler still
// returns ENOSYS.
func (number Syscall) Known() bool {
	switch number {
	case SysRead, SysWrite, SysClose, SysFstat, SysPoll, SysLseek, SysMmap,
		SysMprotect, SysMunmap, SysBrk, SysRtSigaction, SysRtSigprocmask,
		SysIoctl, SysReadv, SysWritev, SysPipe, SysSchedYield, SysMadvise,
		SysNanosleep, SysGetpid, SysExit, SysUname, SysGetcwd, SysChdir,
		SysReadlink, SysGetuid, SysGetgid, SysGeteuid, SysGetegid,
		SysArchPrctl, SysGettid, SysFutex, SysGetdents64, SysClockGettime,
		SysExitGroup, SysOpenat, SysNewfstatat, SysUnlinkat, SysReadlinkat,
		SysPpoll, SysSetRobustList, SysPipe2, SysPrlimit64, SysGetrandom,
		SysStatx:
		return true
	default:
		return false
	}
}
