package linux

import (
	"encoding/binary"
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/userspace/linuxsockapi"
)

func sockaddr(family uint16, port uint16, address [4]byte) []byte {
	wire := make([]byte, SockaddrInSize)
	binary.LittleEndian.PutUint16(wire, family)
	binary.BigEndian.PutUint16(wire[2:], port)
	copy(wire[4:], address[:])
	return wire
}

func TestSockaddrInputLengthMatchesLinux(t *testing.T) {
	for _, test := range []struct {
		length uint64
		copy   int
		errno  abi.Errno
	}{
		{0, 0, 0}, {2, 2, 0}, {15, 15, 0}, {16, 16, 0}, {17, 16, 0}, {128, 16, 0},
		{129, 0, abi.EINVAL}, {0xffffffff, 0, abi.EINVAL}, {1 << 32, 0, 0}, {0x80000000, 0, abi.EINVAL},
	} {
		size, errno := SockaddrInputLength(test.length)
		if size != test.copy || errno != test.errno {
			t.Errorf("length %d: %d %v, want %d %v", test.length, size, errno, test.copy, test.errno)
		}
	}
}

func TestDecodeSockaddrIn(t *testing.T) {
	loopback := [4]byte{127, 0, 0, 1}
	address, unspecified, errno := DecodeSockaddrIn(sockaddr(abi.FamilyInet, 8080, loopback), AddressConnect)
	if errno != 0 || unspecified || address.Port != 8080 || address.Address != loopback {
		t.Fatalf("decode %+v %v %v", address, unspecified, errno)
	}
	for _, test := range []struct {
		wire  []byte
		use   AddressUse
		errno abi.Errno
		unspc bool
	}{
		{sockaddr(abi.FamilyInet, 1, loopback)[:15], AddressBind, abi.EINVAL, false},
		{sockaddr(abi.FamilyInet6, 1, loopback), AddressBind, abi.EAFNOSUPPORT, false},
		{sockaddr(abi.FamilyUnspec, 1, [4]byte{}), AddressBind, 0, false},
		{sockaddr(abi.FamilyUnspec, 1, loopback), AddressBind, abi.EAFNOSUPPORT, false},
		{sockaddr(abi.FamilyUnspec, 53, loopback), AddressSend, 0, false},
		{sockaddr(abi.FamilyUnspec, 0, [4]byte{})[:2], AddressConnect, 0, true},
		{sockaddr(abi.FamilyInet, 0, [4]byte{})[:1], AddressConnect, abi.EINVAL, false},
		{sockaddr(abi.FamilyUnix, 1, loopback), AddressConnect, abi.EAFNOSUPPORT, false},
		{nil, AddressSend, abi.EINVAL, false},
	} {
		_, unspecified, errno := DecodeSockaddrIn(test.wire, test.use)
		if errno != test.errno || unspecified != test.unspc {
			t.Errorf("%x use %d: %v %v, want %v %v", test.wire, test.use, errno, unspecified, test.errno, test.unspc)
		}
	}
}

func TestBindPortErrno(t *testing.T) {
	for _, test := range []struct {
		port  uint16
		errno abi.Errno
	}{{0, 0}, {1, abi.EACCES}, {53, abi.EACCES}, {80, abi.EACCES}, {1023, abi.EACCES}, {1024, 0}, {8080, 0}, {65535, 0}} {
		if errno := BindPortErrno(test.port); errno != test.errno {
			t.Errorf("port %d: %v, want %v", test.port, errno, test.errno)
		}
	}
}

func TestSockaddrOutput(t *testing.T) {
	var wire [SockaddrInSize]byte
	for index := range wire {
		wire[index] = 0xee
	}
	EncodeSockaddrIn(&wire, SocketAddress{Address: [4]byte{10, 0, 2, 2}, Port: 53})
	want := sockaddr(abi.FamilyInet, 53, [4]byte{10, 0, 2, 2})
	if string(wire[:]) != string(want) {
		t.Fatalf("encode %x, want %x (padding must be zero)", wire, want)
	}
	for _, test := range []struct {
		capacity uint32
		actual   int
		copy     int
		errno    abi.Errno
	}{
		{0, 16, 0, 0}, {4, 16, 4, 0}, {16, 16, 16, 0}, {64, 16, 16, 0}, {64, 0, 0, 0}, {0x80000000, 16, 0, abi.EINVAL},
	} {
		size, errno := SockaddrOutputLength(test.capacity, test.actual)
		if size != test.copy || errno != test.errno {
			t.Errorf("capacity %d: %d %v", test.capacity, size, errno)
		}
	}
}

func msghdr(name uint64, nameLength uint32, iov uint64, iovCount uint64, control uint64, controlLength uint64) *[MsghdrSize]byte {
	var wire [MsghdrSize]byte
	binary.LittleEndian.PutUint64(wire[0:], name)
	binary.LittleEndian.PutUint32(wire[8:], nameLength)
	binary.LittleEndian.PutUint64(wire[16:], iov)
	binary.LittleEndian.PutUint64(wire[24:], iovCount)
	binary.LittleEndian.PutUint64(wire[32:], control)
	binary.LittleEndian.PutUint64(wire[40:], controlLength)
	return &wire
}

func TestDecodeMsghdr(t *testing.T) {
	header, errno := DecodeMsghdr(msghdr(0x1000, 200, 0x2000, 3, 0, 0), true)
	if errno != 0 || header.NameLength != SockaddrStorageSize || header.IOVecCount != 3 || header.IOVec != 0x2000 {
		t.Fatalf("decode %+v %v", header, errno)
	}
	if header, _ := DecodeMsghdr(msghdr(0, 16, 0, 0, 0, 0), false); header.NameLength != 0 {
		t.Fatal("a name length without a name survived")
	}
	if _, errno := DecodeMsghdr(msghdr(0x1000, 0x80000000, 0, 0, 0, 0), false); errno != abi.EINVAL {
		t.Fatalf("negative name length: %v", errno)
	}
	if _, errno := DecodeMsghdr(msghdr(0, 0, 0x2000, MaxIOVec+1, 0, 0), false); errno != abi.EMSGSIZE {
		t.Fatalf("long vector: %v", errno)
	}
	if _, errno := DecodeMsghdr(msghdr(0, 0, 0, 0, 0x3000, 1<<31), true); errno != abi.ENOBUFS {
		t.Fatalf("huge control: %v", errno)
	}
	short, _ := DecodeMsghdr(msghdr(0, 0, 0, 0, 0x3000, CmsghdrSize-1), true)
	full, _ := DecodeMsghdr(msghdr(0, 0, 0, 0, 0x3000, CmsghdrSize), true)
	if SendControlErrno(short) != 0 || SendControlErrno(full) != abi.EOPNOTSUPP {
		t.Fatal("control messages")
	}
}

func TestDecodeSocketArguments(t *testing.T) {
	for _, test := range []struct {
		family, kind, protocol uint64
		errno                  abi.Errno
		datagram               bool
	}{
		{2, 1, 0, 0, false}, {2, 1, 6, 0, false}, {2, 2, 0, 0, true}, {2, 2, 17, 0, true},
		{2, 1, 17, abi.EPROTONOSUPPORT, false}, {2, 2, 6, abi.EPROTONOSUPPORT, false},
		{10, 1, 0, abi.EAFNOSUPPORT, false}, {1, 1, 0, abi.EAFNOSUPPORT, false},
		{16, 3, 0, abi.EAFNOSUPPORT, false}, {17, 3, 0, abi.EAFNOSUPPORT, false},
		{2, 3, 0, abi.ESOCKTNOSUPPORT, false}, {2, 5, 0, abi.ESOCKTNOSUPPORT, false},
		{2, 1 | 0x10, 0, abi.EINVAL, false}, {10, 12, 0, abi.EINVAL, false},
	} {
		kind, errno := DecodeSocket(test.family, test.kind, test.protocol)
		if errno != test.errno || errno == 0 && kind.Datagram != test.datagram {
			t.Errorf("socket(%d, %d, %d): %v %+v, want %v", test.family, test.kind, test.protocol, errno, kind, test.errno)
		}
	}
	kind, errno := DecodeSocket(2, 1|abi.SocketNonBlock|abi.SocketCloseExec, 0)
	if errno != 0 || !kind.NonBlock || !kind.CloseOnExec {
		t.Fatalf("flags %+v %v", kind, errno)
	}
	if _, errno := AcceptFlags(abi.SocketNonBlock | 1); errno != abi.EINVAL {
		t.Fatal("accept4 took an unknown flag")
	}
}

func TestMessageFlags(t *testing.T) {
	if SendFlags(abi.MessageOOB) != abi.EOPNOTSUPP || SendFlags(abi.MessageNoSignal|abi.MessageMore|abi.MessageDontWait) != 0 {
		t.Fatal("send flags")
	}
	if ReceiveFlags(abi.MessageOOB) != abi.EOPNOTSUPP || ReceiveFlags(0x2000) != abi.EAGAIN ||
		ReceiveFlags(abi.MessagePeek|abi.MessageWaitAll|abi.MessageTrunc|abi.MessageDontWait) != 0 {
		t.Fatal("receive flags")
	}
}

func TestSocketErrnoCoversEveryResult(t *testing.T) {
	calls := []SocketCall{SocketCallOpen, SocketCallBind, SocketCallListen, SocketCallAccept, SocketCallConnect,
		SocketCallRead, SocketCallWrite, SocketCallSend, SocketCallShutdown, SocketCallName, SocketCallOption}
	for result := linuxsockapi.ResultOK; result <= linuxsockapi.ResultStale; result++ {
		for _, call := range calls {
			errno := SocketErrno(result, call)
			if result == linuxsockapi.ResultOK && errno != 0 || result != linuxsockapi.ResultOK && (errno == 0 || errno == abi.EIO) {
				t.Errorf("result %d call %d: %v", result, call, errno)
			}
		}
	}
	for _, test := range []struct {
		result linuxsockapi.Result
		call   SocketCall
		errno  abi.Errno
	}{
		{linuxsockapi.ResultWouldBlock, SocketCallRead, abi.EAGAIN},
		{linuxsockapi.ResultInProgress, SocketCallConnect, abi.EINPROGRESS},
		{linuxsockapi.ResultNotConnected, SocketCallWrite, abi.EPIPE},
		{linuxsockapi.ResultNotConnected, SocketCallSend, abi.EDESTADDRREQ},
		{linuxsockapi.ResultNotConnected, SocketCallRead, abi.ENOTCONN},
		{linuxsockapi.ResultFamilyLimit, SocketCallOpen, abi.EMFILE},
		{linuxsockapi.ResultServiceLimit, SocketCallAccept, abi.ENFILE},
		{linuxsockapi.ResultDown, SocketCallConnect, abi.ENETDOWN},
		{linuxsockapi.ResultNoAddress, SocketCallBind, abi.EADDRNOTAVAIL},
		{linuxsockapi.ResultNoRoute, SocketCallConnect, abi.ENETUNREACH},
		{linuxsockapi.ResultHostUnreachable, SocketCallConnect, abi.EHOSTUNREACH},
		{linuxsockapi.ResultReconfigured, SocketCallRead, abi.ENETRESET},
		{linuxsockapi.ResultAddressInUse, SocketCallBind, abi.EADDRINUSE},
		{linuxsockapi.ResultRefused, SocketCallConnect, abi.ECONNREFUSED},
		{linuxsockapi.ResultTimeout, SocketCallConnect, abi.ETIMEDOUT},
		{linuxsockapi.ResultReset, SocketCallRead, abi.ECONNRESET},
		{linuxsockapi.ResultReset, SocketCallAccept, abi.ECONNABORTED},
		{linuxsockapi.ResultClosed, SocketCallWrite, abi.EPIPE},
		{linuxsockapi.ResultMessageSize, SocketCallSend, abi.EMSGSIZE},
		{linuxsockapi.ResultServiceLost, SocketCallOpen, abi.ENETDOWN},
		{linuxsockapi.ResultServiceLost, SocketCallRead, abi.ENETRESET},
		{linuxsockapi.ResultNoOption, SocketCallOption, abi.ENOPROTOOPT},
	} {
		if errno := SocketErrno(test.result, test.call); errno != test.errno {
			t.Errorf("result %d call %d: %v, want %v", test.result, test.call, errno, test.errno)
		}
	}
}

func TestClassifyOption(t *testing.T) {
	for _, test := range []struct {
		level, name uint64
		datagram    bool
		set         bool
		place       OptionPlace
		errno       abi.Errno
	}{
		{abi.LevelSocket, abi.OptionType, false, false, OptionKernel, 0},
		{abi.LevelSocket, abi.OptionType, false, true, 0, abi.ENOPROTOOPT},
		{abi.LevelSocket, abi.OptionError, false, false, OptionPendingError, 0},
		{abi.LevelSocket, abi.OptionReuseAddr, false, true, OptionKernel, 0},
		{abi.LevelSocket, abi.OptionRecvBuffer, true, true, OptionService, 0},
		{abi.LevelSocket, abi.OptionReusePort, false, true, 0, abi.ENOPROTOOPT},
		{abi.LevelSocket, abi.OptionBindToDevice, false, true, 0, abi.ENOPROTOOPT},
		{abi.LevelTCP, abi.OptionTCPNoDelay, false, true, OptionService, 0},
		{abi.LevelTCP, abi.OptionTCPNoDelay, true, true, 0, abi.ENOPROTOOPT},
		{abi.LevelTCP, 99, false, false, 0, abi.ENOPROTOOPT},
		{abi.LevelIP, abi.OptionIPTTL, true, false, OptionService, 0},
		{abi.LevelIP, 32, true, true, 0, abi.ENOPROTOOPT},
		{abi.LevelUDP, 1, true, true, 0, abi.ENOPROTOOPT},
		{255, 1, false, false, 0, abi.EOPNOTSUPP},
	} {
		place, errno := ClassifyOption(test.level, test.name, test.datagram, test.set)
		if place != test.place || errno != test.errno {
			t.Errorf("level %d name %d: %v %v, want %v %v", test.level, test.name, place, errno, test.place, test.errno)
		}
	}
}

func TestDecodeTimeout(t *testing.T) {
	timeval := func(seconds, micros int64) []byte {
		wire := make([]byte, TimevalSize)
		binary.LittleEndian.PutUint64(wire, uint64(seconds))
		binary.LittleEndian.PutUint64(wire[8:], uint64(micros))
		return wire
	}
	if value, errno := DecodeTimeout(timeval(1, 500000)); errno != 0 || value != 1500000000 {
		t.Fatalf("1.5 s: %d %v", value, errno)
	}
	if value, _ := DecodeTimeout(timeval(0, 0)); value != 0 {
		t.Fatal("zero is no limit")
	}
	if value, _ := DecodeTimeout(timeval(-1, 0)); value != ImmediateTimeout {
		t.Fatal("negative is immediate")
	}
	if _, errno := DecodeTimeout(timeval(1, 1000000)); errno != abi.EDOM {
		t.Fatal("microseconds out of range")
	}
	if _, errno := DecodeTimeout(timeval(1, 0)[:15]); errno != abi.EINVAL {
		t.Fatal("short timeval")
	}
	var out [TimevalSize]byte
	EncodeTimeout(&out, 1500000000)
	if binary.LittleEndian.Uint64(out[:]) != 1 || binary.LittleEndian.Uint64(out[8:]) != 500000 {
		t.Fatalf("encode %x", out)
	}
	if size, errno := OptionOutputLength(2, 4); size != 2 || errno != 0 {
		t.Fatal("option output is cut to the caller's length")
	}
	if _, errno := OptionOutputLength(0x80000000, 4); errno != abi.EINVAL {
		t.Fatal("negative option length")
	}
	if _, errno := DecodeOptionInt([]byte{1, 0, 0}); errno != abi.EINVAL {
		t.Fatal("short int option")
	}
}

func TestSocketPollRevents(t *testing.T) {
	all := PollIn | PollOut | PollReadHangup
	for _, test := range []struct {
		ready    linuxsockapi.Ready
		live     bool
		shutRead bool
		want     uint16
	}{
		{0, true, false, 0},
		{linuxsockapi.ReadyIn, true, false, PollIn},
		{linuxsockapi.ReadyOut | linuxsockapi.ReadyHangup, true, false, PollOut | PollHangup},
		{linuxsockapi.ReadyReadHangup, true, false, PollIn | PollReadHangup},
		{linuxsockapi.ReadyError, true, false, PollError},
		{0, false, false, PollIn | PollOut | PollError | PollHangup},
		{0, true, true, PollIn | PollReadHangup},
	} {
		if got := SocketPollRevents(all, test.ready, test.live, test.shutRead); got != test.want {
			t.Errorf("ready %b live %v: %#x, want %#x", test.ready, test.live, got, test.want)
		}
	}
	if got := SocketPollRevents(0, linuxsockapi.ReadyIn|linuxsockapi.ReadyError|linuxsockapi.ReadyHangup, true, false); got != PollError|PollHangup {
		t.Fatalf("unrequested events: %#x", got)
	}
}

func FuzzSocketWire(f *testing.F) {
	f.Add(sockaddr(abi.FamilyInet, 80, [4]byte{1, 2, 3, 4}), uint64(16), uint8(1))
	f.Add([]byte{0, 0}, uint64(2), uint8(2))
	f.Add(make([]byte, MsghdrSize), uint64(MsghdrSize), uint8(3))
	f.Fuzz(func(t *testing.T, wire []byte, length uint64, use uint8) {
		size, errno := SockaddrInputLength(length)
		if errno == 0 && (size < 0 || size > SockaddrInSize) {
			t.Fatalf("copy %d", size)
		}
		if size > len(wire) {
			size = len(wire)
		}
		address, unspecified, errno := DecodeSockaddrIn(wire[:size], AddressUse(use%3+1))
		if errno == 0 && !unspecified && size < SockaddrInSize {
			t.Fatalf("decoded %d bytes into %+v", size, address)
		}
		if errno == 0 && !unspecified {
			var out [SockaddrInSize]byte
			EncodeSockaddrIn(&out, address)
			again, _, errno := DecodeSockaddrIn(out[:], AddressBind)
			if errno != 0 || again != address {
				t.Fatalf("round trip %+v %+v", address, again)
			}
		}
		var header [MsghdrSize]byte
		copy(header[:], wire)
		if decoded, errno := DecodeMsghdr(&header, use&1 == 0); errno == 0 {
			if decoded.IOVecCount > MaxIOVec || decoded.NameLength > SockaddrStorageSize || decoded.Name == 0 && decoded.NameLength != 0 {
				t.Fatalf("header %+v", decoded)
			}
		}
		if len(wire) >= TimevalSize {
			if value, errno := DecodeTimeout(wire); errno == 0 && value > 1<<62 {
				t.Fatalf("timeout %d", value)
			}
		}
		if out, errno := SockaddrOutputLength(uint32(length), SockaddrInSize); errno == 0 && (out < 0 || out > SockaddrInSize) {
			t.Fatalf("output %d", out)
		}
	})
}
