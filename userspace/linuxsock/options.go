package linuxsock

import "goos/userspace/linuxsockapi"

// Option levels and names, as Linux numbers them. The kernel answers the
// options it owns itself (SO_ERROR through GetPendingError, SO_TYPE,
// SO_DOMAIN, SO_ACCEPTCONN, SO_RCVTIMEO, SO_SNDTIMEO); the service answers
// these, and NoOption for any other.
const (
	LevelIP     = 0
	LevelSocket = 1
	LevelTCP    = 6

	OptionReuseAddr = 2 // SOL_SOCKET SO_REUSEADDR
	OptionSendBuf   = 7 // SOL_SOCKET SO_SNDBUF
	OptionRecvBuf   = 8 // SOL_SOCKET SO_RCVBUF
	OptionNoDelay   = 1 // IPPROTO_TCP TCP_NODELAY
	OptionMaxSeg    = 2 // IPPROTO_TCP TCP_MAXSEG
	OptionIPTTL     = 2 // IPPROTO_IP IP_TTL
)

// optionOf names the native option behind a Linux one, or zero for an
// option the service answers alone. known is false for an option it does
// not have.
func optionOf(stream bool, level, name uint32) (NativeOptionID, bool) {
	switch {
	case level == LevelSocket && name == OptionReuseAddr:
		return 0, true
	case level == LevelSocket && name == OptionSendBuf:
		return OptionSendBuffer, true
	case level == LevelSocket && name == OptionRecvBuf:
		return OptionReceiveBuffer, true
	case level == LevelTCP && stream && name == OptionNoDelay:
		return 0, true
	case level == LevelTCP && stream && name == OptionMaxSeg:
		return OptionMSS, true
	case level == LevelIP && name == OptionIPTTL:
		return OptionTTL, true
	}
	return 0, false
}

// stepOption answers an option:
//
//   - SO_REUSEADDR is kept by the socket and applied by its next bind;
//   - SO_RCVBUF and SO_SNDBUF report netd's effective buffer; a set is
//     accepted and answers that effective value, which it cannot change;
//   - TCP_NODELAY reports 1, as netd sends without Nagle's delay; setting 1
//     is accepted and setting 0 is NotSupported;
//   - TCP_MAXSEG reports the effective send MSS and cannot be set;
//   - IP_TTL reports the effective TTL; a set to any other value is
//     NotSupported.
func (s *Service) stepOption(o *op, sock *socket) {
	r := &o.request
	set := r.Opcode == linuxsockapi.OpSetOption
	native, known := optionOf(sock.stream, r.Level, r.Name)
	if !known {
		s.finish(o, linuxsockapi.ResultNoOption)
		return
	}
	switch {
	case r.Level == LevelSocket && r.Name == OptionReuseAddr:
		if set {
			sock.reuse = r.Value != 0
		}
		o.reply.Value = 0
		if sock.reuse {
			o.reply.Value = 1
		}
		s.finish(o, linuxsockapi.ResultOK)
		return
	case native == 0: // TCP_NODELAY
		if set && r.Value == 0 {
			s.finish(o, linuxsockapi.ResultNotSupported)
			return
		}
		o.reply.Value = 1
		s.finish(o, linuxsockapi.ResultOK)
		return
	case set && native == OptionMSS:
		s.finish(o, linuxsockapi.ResultNotSupported)
		return
	}
	if !s.usable(o, sock) {
		return
	}
	if sock.endpoint == 0 {
		s.finish(o, linuxsockapi.ResultInvalidArgument)
		return
	}
	s.native = NativeRequest{Op: NativeOption, Option: native, Endpoint: sock.endpoint}
	s.submit(o, sock.proto)
}

func (s *Service) doneOption(o *op, sock *socket, c *NativeCompletion) {
	if c.Result != NativeOK {
		s.finish(o, nativeResult(c.Result))
		return
	}
	r := &o.request
	if r.Opcode == linuxsockapi.OpSetOption && r.Level == LevelIP && uint64(c.Value) != r.Value {
		s.finish(o, linuxsockapi.ResultNotSupported)
		return
	}
	o.reply.Value = uint64(c.Value)
	s.finish(o, linuxsockapi.ResultOK)
}
