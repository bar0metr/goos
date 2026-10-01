package abi

// Syscall is a Linux x86_64 syscall number. It is intentionally distinct from
// the native kcall.Operation namespace.
type Syscall uint64

// Numbers are from Linux arch/x86/entry/syscalls/syscall_64.tbl. This bounded
// planning namespace includes measured release candidates and later fallbacks;
// membership is not a claim of implementation or a release commitment.
const (
	SysRead          Syscall = 0
	SysWrite         Syscall = 1
	SysOpen          Syscall = 2
	SysClose         Syscall = 3
	SysStat          Syscall = 4
	SysFstat         Syscall = 5
	SysLstat         Syscall = 6
	SysPoll          Syscall = 7
	SysLseek         Syscall = 8
	SysMmap          Syscall = 9
	SysMprotect      Syscall = 10
	SysMunmap        Syscall = 11
	SysBrk           Syscall = 12
	SysRtSigaction   Syscall = 13
	SysRtSigprocmask Syscall = 14
	SysRtSigreturn   Syscall = 15
	SysIoctl         Syscall = 16
	SysPread64       Syscall = 17
	SysPwrite64      Syscall = 18
	SysAccess        Syscall = 21
	SysReadv         Syscall = 19
	SysWritev        Syscall = 20
	SysPipe          Syscall = 22
	SysSchedYield    Syscall = 24
	SysMadvise       Syscall = 28
	SysDup           Syscall = 32
	SysDup2          Syscall = 33
	SysPause         Syscall = 34
	SysNanosleep     Syscall = 35
	SysGetpid        Syscall = 39
	SysClone         Syscall = 56
	SysFork          Syscall = 57
	SysVfork         Syscall = 58
	SysExecve        Syscall = 59
	SysExit          Syscall = 60
	SysWait4         Syscall = 61
	SysKill          Syscall = 62
	SysUname         Syscall = 63
	SysFcntl         Syscall = 72
	SysGetcwd        Syscall = 79
	SysChdir         Syscall = 80
	SysRename        Syscall = 82
	SysMkdir         Syscall = 83
	SysRmdir         Syscall = 84
	SysUnlink        Syscall = 87
	SysReadlink      Syscall = 89
	SysGetuid        Syscall = 102
	SysGetgid        Syscall = 104
	SysSetuid        Syscall = 105
	SysSetgid        Syscall = 106
	SysGeteuid       Syscall = 107
	SysGetegid       Syscall = 108
	SysGetppid       Syscall = 110
	SysGetgroups     Syscall = 115
	SysArchPrctl     Syscall = 158
	SysRtSigsuspend  Syscall = 130
	SysGettid        Syscall = 186
	SysTkill         Syscall = 200
	SysFutex         Syscall = 202
	SysGetdents64    Syscall = 217
	SysSetTidAddress Syscall = 218
	SysClockGettime  Syscall = 228
	SysExitGroup     Syscall = 231
	SysTgkill        Syscall = 234
	SysOpenat        Syscall = 257
	SysMkdirat       Syscall = 258
	SysNewfstatat    Syscall = 262
	SysUnlinkat      Syscall = 263
	SysRenameat      Syscall = 264
	SysReadlinkat    Syscall = 267
	SysFaccessat     Syscall = 269
	SysPpoll         Syscall = 271
	SysSetRobustList Syscall = 273
	SysDup3          Syscall = 292
	SysPipe2         Syscall = 293
	SysPrlimit64     Syscall = 302
	SysRenameat2     Syscall = 316
	SysGetrandom     Syscall = 318
	SysStatx         Syscall = 332
)

const (
	ProtNone  = uint64(0)
	ProtRead  = uint64(1)
	ProtWrite = uint64(2)
	ProtExec  = uint64(4)

	MapPrivate   = uint64(0x02)
	MapAnonymous = uint64(0x20)

	MadvDontNeed = uint64(4)

	ArchSetGS = uint64(0x1001)
	ArchSetFS = uint64(0x1002)
	ArchGetFS = uint64(0x1003)
	ArchGetGS = uint64(0x1004)

	// AtFDCWD is the dirfd that names the working directory.
	AtFDCWD = int32(-100)

	OpenReadOnly  = uint32(0)
	OpenWriteOnly = uint32(1)
	OpenReadWrite = uint32(2)
	OpenAccess    = uint32(3)
	OpenCreate    = uint32(0o100)
	OpenExclusive = uint32(0o200)
	OpenNoCTTY    = uint32(0o400)
	OpenTruncate  = uint32(0o1000)
	OpenAppend    = uint32(0o2000)
	OpenLargeFile = uint32(0o100000)
	OpenDirectory = uint32(0o200000)
	OpenNoFollow  = uint32(0o400000)
	OpenCloseExec = uint32(0o2000000)

	// AtRemoveDir makes unlinkat remove a directory.
	AtRemoveDir = uint64(0x200)

	// Flags of newfstatat.
	AtSymlinkNoFollow = uint64(0x100)
	AtNoAutomount     = uint64(0x800)
	AtEmptyPath       = uint64(0x1000)

	// Commands of fcntl and the one descriptor flag.
	FcntlDupFD        = uint64(0)
	FcntlGetFD        = uint64(1)
	FcntlSetFD        = uint64(2)
	FcntlGetFL        = uint64(3)
	FcntlSetFL        = uint64(4)
	FcntlDupFDCloExec = uint64(1030)
	DescriptorCloExec = uint64(1)

	// Status flags F_SETFL can change on Linux besides O_APPEND. GoOS has
	// no behavior behind any of them.
	OpenNonBlock = uint32(0o4000)
	OpenAsync    = uint32(0o20000)
	OpenDirect   = uint32(0o40000)
	OpenNoATime  = uint32(0o1000000)

	// d_type values of a directory entry.
	DirentDirectory = uint8(4)
	DirentRegular   = uint8(8)

	// Clock identifiers of clock_gettime.
	ClockRealtime        = uint64(0)
	ClockMonotonic       = uint64(1)
	ClockMonotonicRaw    = uint64(4)
	ClockRealtimeCoarse  = uint64(5)
	ClockMonotonicCoarse = uint64(6)
	ClockBoottime        = uint64(7)

	// Resources of prlimit64 and the value that means no limit.
	ResourceCPU       = uint64(0)
	ResourceFileSize  = uint64(1)
	ResourceData      = uint64(2)
	ResourceStack     = uint64(3)
	ResourceCore      = uint64(4)
	ResourceOpenFiles = uint64(7)
	ResourceCount     = uint64(16)
	ResourceInfinity  = ^uint64(0)

	// Flags and field masks of statx.
	AtStatxSyncType   = uint64(0x6000)
	StatxBasicStats   = uint32(0x7ff)
	StatxBirthTime    = uint32(0x800)
	StatxReservedMask = uint64(0x80000000)

	// Flags of getrandom. None changes what GoOS returns.
	RandomNonBlock = uint64(1)
	RandomRandom   = uint64(2)
	RandomInsecure = uint64(4)

	// Operations of rt_sigprocmask, and the signals no mask or handler
	// may claim.
	SignalBlock     = uint64(0)
	SignalUnblock   = uint64(1)
	SignalSetMask   = uint64(2)
	SignalHangup    = uint64(1)
	SignalInterrupt = uint64(2)
	SignalQuit      = uint64(3)
	SignalAbort     = uint64(6)
	SignalKill      = uint64(9)
	SignalUser1     = uint64(10)
	SignalSegv      = uint64(11)
	SignalUser2     = uint64(12)
	SignalPipe      = uint64(13)
	SignalAlarm     = uint64(14)
	SignalTerm      = uint64(15)
	SignalChild     = uint64(17)
	SignalStop      = uint64(19)
	SignalCount     = uint64(64)

	// sigaction flags and the special handler values.
	SignalActionNoChildStop = uint64(1)
	SignalActionNoChildWait = uint64(2)
	SignalActionInfo        = uint64(4)
	SignalActionOnStack     = uint64(0x08000000)
	SignalActionRestart     = uint64(0x10000000)
	SignalActionRestorer    = uint64(0x04000000)
	SignalActionNoDefer     = uint64(0x40000000)
	SignalActionResetHand   = uint64(0x80000000)
	SignalHandlerDefault    = uint64(0)
	SignalHandlerIgnore     = uint64(1)

	// si_code values: sent by kill, sent by tkill or tgkill, and the two
	// ways a child ends.
	SignalCodeUser       = int32(0)
	SignalCodeThreadKill = int32(-6)
	SignalCodeChildExit  = int32(1)
	SignalCodeChildKill  = int32(2)

	// clone flags a process-creating call may carry. The low byte of the
	// flags is the signal the parent receives when the child ends.
	CloneVM         = uint64(0x100)
	CloneVfork      = uint64(0x4000)
	CloneSignalMask = uint64(0xff)

	// wait4 options. WaitAll and WaitNoThread change nothing for a
	// single-task process that creates children only with SIGCHLD.
	WaitNoHang   = uint64(1)
	WaitNoThread = uint64(0x20000000)
	WaitAll      = uint64(0x40000000)
	// RusageSize is the size of struct rusage on x86_64.
	RusageSize = 144

	// File type and permission bits of st_mode.
	ModeFIFO      = uint32(0o010000)
	ModeCharacter = uint32(0o020000)
	ModeDirectory = uint32(0o040000)
	ModeRegular   = uint32(0o100000)

	SeekSet = uint64(0)
	SeekCur = uint64(1)
	SeekEnd = uint64(2)

	// PathMax is Linux PATH_MAX: the longest path in bytes, terminator
	// included.
	PathMax = 4096

	// IOVecSize is the wire size of one struct iovec.
	IOVecSize = 16
)

// Known reports whether number belongs to the bounded planning namespace.
// Known does not mean implemented or release-scoped: an absent handler still
// returns ENOSYS.
func (number Syscall) Known() bool {
	switch number {
	case SysRead, SysWrite, SysOpen, SysClose, SysStat, SysFstat, SysLstat, SysPoll, SysLseek, SysMmap,
		SysMprotect, SysMunmap, SysBrk, SysRtSigaction, SysRtSigprocmask, SysRtSigreturn, SysRtSigsuspend, SysPause,
		SysIoctl, SysReadv, SysWritev, SysPipe, SysSchedYield, SysMadvise,
		SysDup, SysDup2, SysFcntl, SysDup3, SysPread64, SysPwrite64, SysAccess, SysSetuid, SysSetgid,
		SysGetgroups, SysFaccessat,
		SysNanosleep, SysGetpid, SysClone, SysFork, SysVfork, SysExecve, SysExit, SysWait4, SysKill, SysTkill, SysTgkill, SysUname, SysGetcwd, SysChdir,
		SysRename, SysMkdir, SysRmdir, SysUnlink, SysMkdirat, SysRenameat,
		SysRenameat2,
		SysReadlink, SysGetuid, SysGetgid, SysGeteuid, SysGetegid, SysGetppid,
		SysArchPrctl, SysGettid, SysFutex, SysGetdents64, SysSetTidAddress,
		SysClockGettime,
		SysExitGroup, SysOpenat, SysNewfstatat, SysUnlinkat, SysReadlinkat,
		SysPpoll, SysSetRobustList, SysPipe2, SysPrlimit64, SysGetrandom,
		SysStatx:
		return true
	default:
		return false
	}
}
