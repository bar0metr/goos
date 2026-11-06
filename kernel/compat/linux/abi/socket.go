package abi

// The socket calls, 41 to 55, and accept4.
const (
	SysSocket      Syscall = 41
	SysConnect     Syscall = 42
	SysAccept      Syscall = 43
	SysSendto      Syscall = 44
	SysRecvfrom    Syscall = 45
	SysSendmsg     Syscall = 46
	SysRecvmsg     Syscall = 47
	SysShutdown    Syscall = 48
	SysBind        Syscall = 49
	SysListen      Syscall = 50
	SysGetsockname Syscall = 51
	SysGetpeername Syscall = 52
	SysSetsockopt  Syscall = 54
	SysGetsockopt  Syscall = 55
	SysAccept4     Syscall = 288
)

// ModeSocket is the st_mode file type of a socket.
const ModeSocket = uint32(0o140000)

// Address families, socket types, and protocols.
const (
	FamilyUnspec  = uint16(0)
	FamilyUnix    = uint16(1)
	FamilyInet    = uint16(2)
	FamilyInet6   = uint16(10)
	FamilyNetlink = uint16(16)
	FamilyPacket  = uint16(17)

	SocketStream    = uint64(1)
	SocketDatagram  = uint64(2)
	SocketRaw       = uint64(3)
	SocketSeqPacket = uint64(5)
	// SocketTypeMask selects the type; the flags above it are SOCK_NONBLOCK
	// and SOCK_CLOEXEC, which have the values of O_NONBLOCK and O_CLOEXEC.
	SocketTypeMask  = uint64(0xf)
	SocketNonBlock  = uint64(0o4000)
	SocketCloseExec = uint64(0o2000000)

	ProtocolIP  = uint64(0)
	ProtocolTCP = uint64(6)
	ProtocolUDP = uint64(17)
)

// Option levels and names of setsockopt and getsockopt.
const (
	LevelSocket = uint64(1)
	LevelIP     = uint64(0)
	LevelTCP    = uint64(6)
	LevelUDP    = uint64(17)

	OptionDebug        = uint64(1)
	OptionReuseAddr    = uint64(2)
	OptionType         = uint64(3)
	OptionError        = uint64(4)
	OptionDontRoute    = uint64(5)
	OptionBroadcast    = uint64(6)
	OptionSendBuffer   = uint64(7)
	OptionRecvBuffer   = uint64(8)
	OptionKeepAlive    = uint64(9)
	OptionOOBInline    = uint64(10)
	OptionLinger       = uint64(13)
	OptionReusePort    = uint64(15)
	OptionRecvLowat    = uint64(18)
	OptionSendLowat    = uint64(19)
	OptionRecvTimeout  = uint64(20)
	OptionSendTimeout  = uint64(21)
	OptionBindToDevice = uint64(25)
	OptionAcceptConn   = uint64(30)
	OptionProtocol     = uint64(38)
	OptionDomain       = uint64(39)

	OptionTCPNoDelay = uint64(1)
	OptionTCPMaxSeg  = uint64(2)

	OptionIPTTL = uint64(2)
)

// Message flags of the send and receive calls.
const (
	MessageOOB          = uint64(0x1)
	MessagePeek         = uint64(0x2)
	MessageDontRoute    = uint64(0x4)
	MessageControlTrunc = uint64(0x8)
	MessageTrunc        = uint64(0x20)
	MessageDontWait     = uint64(0x40)
	MessageEOR          = uint64(0x80)
	MessageWaitAll      = uint64(0x100)
	MessageNoSignal     = uint64(0x4000)
	MessageMore         = uint64(0x8000)
	MessageCmsgCloexec  = uint64(0x40000000)
)

// How of shutdown.
const (
	ShutdownRead  = uint64(0)
	ShutdownWrite = uint64(1)
	ShutdownBoth  = uint64(2)
)

// Socket ioctl requests besides FIONREAD, which is SIOCINQ. FIONBIO sets
// O_NONBLOCK.
const (
	IoctlFIONBIO    = uint64(0x5421)
	IoctlSIOCATMARK = uint64(0x8905)
)
